package web

// CodizeLabs fork patch P3 (ADR-0002 § 1): the one transport file in package
// web used by internal/cz/imaginepost. It exists here only because the
// Statsig signer and the header builders are unexported. It adds no replay,
// signs every /rest/ call (GETs included) and fails closed when it can't.
// The method + path allow-list is enforced by imaginepost before any call;
// this file refuses anything outside /rest/, the direct upload and
// assets.grok.com/users/ as a second line.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// ErrImagineCallRefused is returned, with zero network calls, for a call the
// transport will never make.
var ErrImagineCallRefused = errors.New("cz imagine: call refused before send")

// ImagineHTTPError is a non-2xx grok.com answer. It never holds the body,
// only its length and a SHA-256 prefix (sec-rowan B5).
type ImagineHTTPError struct {
	Status           int
	BodyBytes        int
	BodyPrefixSHA256 string
}

func (e *ImagineHTTPError) Error() string {
	return fmt.Sprintf("grok imagine answered %d", e.Status)
}

// czImagineAssetsBase is the media host; package tests point it at a fake.
var czImagineAssetsBase = "https://assets.grok.com"

var czImagineAssetKey = regexp.MustCompile(`^users/[A-Za-z0-9._/-]{1,400}$`)

func czImaginePathAllowed(method, path string) bool {
	if method != http.MethodGet && method != http.MethodPost {
		return false
	}
	if !strings.HasPrefix(path, "/rest/") || strings.Contains(path, "//") || strings.Contains(path, "..") ||
		strings.Contains(path, "#") || strings.ContainsAny(path, "\r\n\t \\") {
		return false
	}
	return true
}

// DoImagine makes one signed grok.com /rest/ call for imaginepost. The
// response body releases the egress lease when closed. A non-2xx answer is
// returned as *ImagineHTTPError with the body already drained and dropped.
func (a *Adapter) DoImagine(ctx context.Context, credential account.Credential, method, path string, body []byte) (*http.Response, error) {
	if !czImaginePathAllowed(method, path) {
		return nil, ErrImagineCallRefused
	}
	if method == http.MethodGet && len(body) != 0 {
		return nil, ErrImagineCallRefused
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	// Check the signer before taking an egress lease so a missing value makes
	// zero calls of any kind.
	if err := statsigConfigUsable(cfg); err != nil {
		return nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return nil, err
	}
	var reader io.Reader = http.NoBody
	if method == http.MethodPost {
		if body == nil {
			body = []byte(`{}`)
		}
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, cfg.BaseURL+path, reader)
	if err != nil {
		lease.Release()
		return nil, err
	}
	request.Header = buildHeaders(token, lease, "application/json")
	if method == http.MethodGet {
		request.Header.Del("Content-Type")
	}
	applyAppHeaders(request.Header, cfg.BaseURL, cfg.BaseURL+"/imagine")
	if err := a.applySignedStatsig(ctx, request, token, lease); err != nil {
		lease.Release()
		return nil, err
	}
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		lease.Release()
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, a.czImagineRejection(response, lease, method, request.URL.String())
	}
	original := response.Body
	response.Body = &readerCloser{Reader: original, closer: closerFunc(func() error {
		err := original.Close()
		lease.Release()
		return err
	})}
	return response, nil
}

// closerFunc runs its close once; later calls return nil.
func closerFunc(close func() error) io.Closer {
	var once sync.Once
	return czOnceCloser(func() error {
		var err error
		once.Do(func() { err = close() })
		return err
	})
}

type czOnceCloser func() error

func (f czOnceCloser) Close() error { return f() }

func (a *Adapter) czImagineRejection(response *http.Response, lease interface{ InvalidateClearance(); Release() }, method, target string) error {
	defer lease.Release()
	captured, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
	_ = response.Body.Close()
	truncated := len(captured) > webMediaDiagnosticBodyLimit
	if truncated {
		captured = captured[:webMediaDiagnosticBodyLimit]
	}
	upstreamErr := newWebMediaUpstreamError(response.StatusCode, captured, truncated)
	if response.StatusCode == http.StatusForbidden {
		if isClearanceRefreshableMediaError(upstreamErr) {
			lease.InvalidateClearance()
		}
		// A code-7 Statsig rejection only refreshes the signature for the next
		// call; P3 never replays (sec-rowan N1).
		_ = a.invalidateSignedStatsig(method, target)
	}
	a.logWebMediaUpstreamRejection("cz_imagine", response, upstreamErr)
	return &ImagineHTTPError{Status: response.StatusCode, BodyBytes: upstreamErr.bodyBytes, BodyPrefixSHA256: upstreamErr.bodyPrefixSHA256}
}

// UploadImagine uploads one reference image through /http/upload-file-v2/direct
// (never makePublic) and returns its file metadata id.
func (a *Adapter) UploadImagine(ctx context.Context, credential account.Credential, filename, mimeType string, data []byte) (string, error) {
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") || len(data) == 0 {
		return "", ErrImagineCallRefused
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return "", err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	uploaded, err := a.uploadFileV2Direct(ctx, cfg, lease, token, provider.ImageInput{Filename: filename, MIMEType: mimeType, Data: data},
		cfg.BaseURL+"/imagine", imagineSelfUploadSource, "cz_imagine_upload")
	if err != nil {
		var upstreamErr *webMediaUpstreamError
		if errors.As(err, &upstreamErr) {
			return "", &ImagineHTTPError{Status: upstreamErr.status, BodyBytes: upstreamErr.bodyBytes, BodyPrefixSHA256: upstreamErr.bodyPrefixSHA256}
		}
		return "", err
	}
	if uploaded.MetadataID == "" {
		return "", fmt.Errorf("cz imagine: upload returned no file metadata id")
	}
	return uploaded.MetadataID, nil
}

// FetchImagineAsset streams one item's media from assets.grok.com through the
// owning account's session. key is the asset record's relative "key".
func (a *Adapter) FetchImagineAsset(ctx context.Context, credential account.Credential, key string) (io.ReadCloser, string, error) {
	if !czImagineAssetKey.MatchString(key) || strings.Contains(key, "..") || strings.Contains(key, "//") {
		return nil, "", ErrImagineCallRefused
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, "", err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, czImagineAssetsBase+"/"+key, nil)
	if err != nil {
		lease.Release()
		return nil, "", err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		lease.Release()
		return nil, "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", &ImagineHTTPError{Status: response.StatusCode}
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if !strings.HasPrefix(contentType, "image/") && !strings.HasPrefix(contentType, "video/") {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", fmt.Errorf("cz imagine: media content type not image or video")
	}
	body := response.Body
	return &readerCloser{Reader: body, closer: closerFunc(func() error {
		err := body.Close()
		lease.Release()
		return err
	})}, contentType, nil
}
