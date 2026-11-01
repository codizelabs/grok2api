package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/signerurl"
	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

const (
	statsigCacheTTL        = time.Hour
	statsigCacheMaxEntries = 4096
	statsigMetaBodyLimit   = 4 << 20
	statsigResponseLimit   = 4 << 10
)

var (
	errStatsigMetaMissing  = errors.New("Grok index 缺少 grok-site-verification")
	errStatsigPageStatus   = errors.New("grok.com page returned an error status")
	errStatsigPageTooLarge = errors.New("grok.com page is larger than the safety limit")
)

type statsigCacheEntry struct {
	value     string
	expiresAt time.Time
}

type statsigSignResult struct {
	value  string
	source string
}

type statsigWarmTarget struct {
	method string
	target string
}

type statsigSigner struct {
	client           *http.Client
	fetchMeta        func(context.Context, string, string, *infraegress.Lease) (string, error)
	validateEndpoint func(context.Context, string) error
	now              func() time.Time
	// fetchLocal and randomByte serve statsigMode "local" (statsig_local.go);
	// tests replace them to fix the page and the random byte.
	fetchLocal func(context.Context, string, string, *infraegress.Lease) (statsigLocalMaterial, error)
	randomByte func() byte
	mu         sync.Mutex
	entries    map[string]statsigCacheEntry
	local      map[string]statsigLocalEntry
	refreshes  singleflight.Group
}

func newStatsigSigner() *statsigSigner {
	return &statsigSigner{
		client: &http.Client{
			Timeout:       12 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
		fetchMeta:        fetchStatsigMetaContent,
		validateEndpoint: validateStatsigSignerEndpoint,
		now:              time.Now,
		fetchLocal:       fetchStatsigLocalMaterial,
		randomByte:       randomStatsigByte,
		entries:          make(map[string]statsigCacheEntry),
		local:            make(map[string]statsigLocalEntry),
	}
}

func (s *statsigSigner) Sign(ctx context.Context, baseURL, signerURL, token string, lease *infraegress.Lease, method, target string) (string, string, error) {
	key, path, err := statsigSignatureKey(baseURL, signerURL, method, target)
	if err != nil {
		return "", "", err
	}
	if value, ok := s.cached(key, s.now().UTC()); ok {
		return value, "cache", nil
	}
	value, err, _ := s.refreshes.Do(key, func() (any, error) {
		now := s.now().UTC()
		if cached, ok := s.cached(key, now); ok {
			return statsigSignResult{value: cached, source: "cache"}, nil
		}
		fresh, refreshErr := s.freshSignature(ctx, baseURL, signerURL, token, lease, method, path)
		if refreshErr != nil {
			if stale, ok := s.stale(key); ok {
				return statsigSignResult{value: stale, source: "stale"}, nil
			}
			return statsigSignResult{}, refreshErr
		}
		s.store(key, fresh, now.Add(statsigCacheTTL), now)
		return statsigSignResult{value: fresh, source: "refresh"}, nil
	})
	if err != nil {
		return "", "", err
	}
	result := value.(statsigSignResult)
	return result.value, result.source, nil
}

// Warm 使用一次 metaContent 请求预热多个常用签名键，避免按账号或按路径重复抓取首页。
func (s *statsigSigner) Warm(ctx context.Context, baseURL, signerURL, token string, lease *infraegress.Lease, targets []statsigWarmTarget) (int, error) {
	now := s.now().UTC()
	type pendingTarget struct {
		key    string
		method string
		path   string
	}
	pending := make([]pendingTarget, 0, len(targets))
	for _, target := range targets {
		key, path, err := statsigSignatureKey(baseURL, signerURL, target.method, target.target)
		if err != nil {
			return 0, err
		}
		if _, ok := s.cached(key, now); ok {
			continue
		}
		pending = append(pending, pendingTarget{key: key, method: target.method, path: path})
	}
	if len(pending) == 0 {
		return 0, nil
	}
	meta, err := s.fetchMeta(ctx, baseURL, token, lease)
	if err != nil {
		return 0, err
	}
	warmed := 0
	for _, target := range pending {
		value, signErr := s.requestSignature(ctx, signerURL, target.method, target.path, meta)
		if signErr != nil {
			return warmed, signErr
		}
		s.store(target.key, value, now.Add(statsigCacheTTL), now)
		warmed++
	}
	return warmed, nil
}

func (s *statsigSigner) freshSignature(ctx context.Context, baseURL, signerURL, token string, lease *infraegress.Lease, method, path string) (string, error) {
	meta, err := s.fetchMeta(ctx, baseURL, token, lease)
	if err != nil {
		return "", err
	}
	signature, err := s.requestSignature(ctx, signerURL, method, path, meta)
	if err == nil {
		return signature, nil
	}

	meta, refreshErr := s.fetchMeta(ctx, baseURL, token, lease)
	if refreshErr != nil {
		return "", fmt.Errorf("刷新 Statsig metaContent: %w", refreshErr)
	}
	signature, retryErr := s.requestSignature(ctx, signerURL, method, path, meta)
	if retryErr != nil {
		return "", fmt.Errorf("Statsig 签名失败: %w", retryErr)
	}
	return signature, nil
}

func (s *statsigSigner) Invalidate(baseURL, signerURL, method, target string) {
	key, _, err := statsigSignatureKey(baseURL, signerURL, method, target)
	if err != nil {
		return
	}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

func (s *statsigSigner) Clear() {
	s.mu.Lock()
	clear(s.entries)
	clear(s.local)
	s.mu.Unlock()
}

func (s *statsigSigner) cached(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || entry.value == "" || !now.Before(entry.expiresAt) {
		return "", false
	}
	return entry.value, true
}

func (s *statsigSigner) stale(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	return entry.value, ok && validStatsigID(entry.value)
}

func (s *statsigSigner) store(key, value string, expiresAt, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for existingKey, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, existingKey)
		}
	}
	if len(s.entries) >= statsigCacheMaxEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for existingKey, entry := range s.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
				oldestKey, oldestExpiry = existingKey, entry.expiresAt
			}
		}
		delete(s.entries, oldestKey)
	}
	s.entries[key] = statsigCacheEntry{value: value, expiresAt: expiresAt}
}

func statsigSignatureKey(baseURL, signerURL, method, target string) (string, string, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", "", fmt.Errorf("解析 Statsig 目标地址: %w", err)
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	return strings.TrimRight(baseURL, "/") + "\x00" + strings.TrimSpace(signerURL) + "\x00" + method + "\x00" + path, path, nil
}

func (s *statsigSigner) requestSignature(ctx context.Context, endpoint, method, path, metaContent string) (string, error) {
	if err := s.validateEndpoint(ctx, endpoint); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{
		"method": strings.ToUpper(strings.TrimSpace(method)),
		"path":   path,
		"environment": map[string]string{
			"metaContent": metaContent,
		},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, statsigResponseLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > statsigResponseLimit {
		return "", fmt.Errorf("签名响应超过安全上限")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("签名服务返回 %d", response.StatusCode)
	}
	var value struct {
		StatsigID string `json:"x-statsig-id"`
	}
	if json.Unmarshal(body, &value) != nil || !validStatsigID(value.StatsigID) {
		return "", fmt.Errorf("签名服务响应无效")
	}
	return value.StatsigID, nil
}

func validateStatsigSignerEndpoint(ctx context.Context, endpoint string) error {
	_ = ctx
	return signerurl.Validate(endpoint)
}

func fetchStatsigMetaContent(ctx context.Context, baseURL, token string, lease *infraegress.Lease) (string, error) {
	if lease == nil {
		return "", fmt.Errorf("Statsig 获取缺少出口租约")
	}
	return fetchStatsigMetaContentWithDo(ctx, baseURL, token, lease, lease.Do)
}

func fetchStatsigMetaContentWithDo(ctx context.Context, baseURL, token string, lease *infraegress.Lease, do func(*http.Request) (*http.Response, error)) (string, error) {
	body, err := fetchStatsigPageWithDo(ctx, baseURL, token, lease, do)
	if err != nil {
		return "", err
	}
	return extractStatsigMetaContent(body)
}

// fetchStatsigPage GETs the grok.com page that carries the
// grok-site-verification meta and returns its body.
func fetchStatsigPage(ctx context.Context, baseURL, token string, lease *infraegress.Lease) ([]byte, error) {
	if lease == nil {
		return nil, fmt.Errorf("Statsig 获取缺少出口租约")
	}
	return fetchStatsigPageWithDo(ctx, baseURL, token, lease, lease.Do)
}

func fetchStatsigPageWithDo(ctx context.Context, baseURL, token string, lease *infraegress.Lease, do func(*http.Request) (*http.Response, error)) ([]byte, error) {
	if do == nil {
		return nil, fmt.Errorf("Statsig 获取缺少出口租约")
	}
	index, err := fetchStatsigMetaResponse(ctx, baseURL, token, lease, "/index", do)
	if err != nil {
		return nil, err
	}
	if index.statusCode >= 200 && index.statusCode < 300 {
		// A successful index response without the verification meta remains a hard
		// protocol failure. Falling back here would hide a changed upstream page.
		if _, extractErr := extractStatsigMetaContent(index.body); extractErr != nil {
			return nil, extractErr
		}
		return index.body, nil
	}
	if index.statusCode != http.StatusNotFound {
		return nil, statsigMetaStatusError("/index", index.statusCode)
	}
	if _, extractErr := extractStatsigMetaContent(index.body); extractErr == nil {
		return index.body, nil
	} else if !errors.Is(extractErr, errStatsigMetaMissing) {
		return nil, extractErr
	}

	// /index currently returns a branded 404. Only that exact status with a
	// missing meta is allowed to fall back to the canonical root page.
	root, err := fetchStatsigMetaResponse(ctx, baseURL, token, lease, "/", do)
	if err != nil {
		return nil, err
	}
	if root.statusCode < 200 || root.statusCode >= 300 {
		return nil, statsigMetaStatusError("/", root.statusCode)
	}
	if _, extractErr := extractStatsigMetaContent(root.body); extractErr != nil {
		return nil, extractErr
	}
	return root.body, nil
}

type statsigMetaResponse struct {
	statusCode int
	body       []byte
}

func fetchStatsigMetaResponse(ctx context.Context, baseURL, token string, lease *infraegress.Lease, path string, do func(*http.Request) (*http.Response, error)) (statsigMetaResponse, error) {
	requestCtx, cancel := context.WithTimeout(infraegress.WithPhysicalCallStage(ctx, "statsig_meta"), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return statsigMetaResponse{}, err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Sec-Fetch-Dest", "document")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	if lease != nil {
		request.Header.Set("User-Agent", lease.UserAgent)
		request.Header.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	}
	response, err := do(request)
	if err != nil {
		return statsigMetaResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, statsigMetaBodyLimit+1))
	if err != nil {
		return statsigMetaResponse{}, err
	}
	if len(body) > statsigMetaBodyLimit {
		return statsigMetaResponse{}, statsigMetaOversizeError(path)
	}
	return statsigMetaResponse{statusCode: response.StatusCode, body: body}, nil
}

func statsigMetaStatusError(path string, statusCode int) error {
	if path == "/" {
		return fmt.Errorf("%w: Grok 首页返回 %d", errStatsigPageStatus, statusCode)
	}
	return fmt.Errorf("%w: Grok index 返回 %d", errStatsigPageStatus, statusCode)
}

func statsigMetaOversizeError(path string) error {
	if path == "/" {
		return fmt.Errorf("%w: Grok 首页超过安全上限", errStatsigPageTooLarge)
	}
	return fmt.Errorf("%w: Grok index 超过安全上限", errStatsigPageTooLarge)
}

func extractStatsigMetaContent(body []byte) (string, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return "", errStatsigMetaMissing
			}
			return "", tokenizer.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttrs := tokenizer.TagName()
			if !strings.EqualFold(string(name), "meta") || !hasAttrs {
				continue
			}
			metaName := ""
			content := ""
			for {
				key, value, more := tokenizer.TagAttr()
				switch strings.ToLower(string(key)) {
				case "name":
					metaName = normalizeStatsigMetaName(string(value))
				case "content":
					content = strings.TrimSpace(string(value))
				}
				if !more {
					break
				}
			}
			if metaName == "grok-site-verification" && content != "" {
				return content, nil
			}
		}
	}
}

func normalizeStatsigMetaName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.NewReplacer("‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-").Replace(value)
}

func validStatsigID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 70
}

// applySignedStatsig sets x-statsig-id on request. It fails closed: when no
// valid value can be had from the configured source it returns an error that
// wraps ErrStatsigUnavailable with a readable reason, and the caller must not
// send the request. "local" computes the value in-process from grok.com's own
// page; "url" asks a configured signer (there is no built-in signer URL);
// "manual" sends a pasted value.
func (a *Adapter) applySignedStatsig(ctx context.Context, request *http.Request, token string, lease *infraegress.Lease) error {
	if request == nil {
		return nil
	}
	cfg := a.config()
	request.Header.Del("x-statsig-id")
	if err := statsigConfigUsable(cfg); err != nil {
		return err
	}
	if cfg.StatsigMode == "manual" {
		request.Header.Set("x-statsig-id", strings.TrimSpace(cfg.StatsigManualValue))
		return nil
	}
	value, source, err := a.signStatsig(ctx, cfg, token, lease, request.Method, request.URL.String())
	if err == nil {
		request.Header.Set("x-statsig-id", value)
		if source == "refresh" {
			a.log().Info("web_statsig_refreshed", "mode", cfg.StatsigMode, "method", request.Method, "path", request.URL.EscapedPath())
		} else if source == "stale" {
			a.log().Warn("web_statsig_refresh_failed_using_stale", "mode", cfg.StatsigMode, "method", request.Method, "path", request.URL.EscapedPath())
		}
		return nil
	}
	a.log().Warn("web_statsig_fetch_failed", "mode", cfg.StatsigMode, "method", request.Method, "path", request.URL.EscapedPath(), "reason", provider.StatsigUnavailableReason(err))
	return err
}

// signStatsig produces a value for the "local" and "url" modes. Its error
// always wraps ErrStatsigUnavailable with a readable reason; a url-mode
// signer's own error text is not passed on (it is upstream-controlled).
func (a *Adapter) signStatsig(ctx context.Context, cfg Config, token string, lease *infraegress.Lease, method, target string) (string, string, error) {
	if a.statsig == nil {
		return "", "", provider.NewStatsigUnavailable(provider.StatsigReasonNotInitialised)
	}
	if cfg.StatsigMode == "local" {
		return a.statsig.SignLocal(ctx, cfg.BaseURL, token, lease, method, target)
	}
	value, source, err := a.statsig.Sign(ctx, cfg.BaseURL, cfg.StatsigSignerURL, token, lease, method, target)
	if err != nil {
		if errors.Is(err, provider.ErrStatsigUnavailable) {
			return "", "", err
		}
		if errors.Is(err, errStatsigMetaMissing) || errors.Is(err, errStatsigPageStatus) || errors.Is(err, errStatsigPageTooLarge) {
			return "", "", statsigPageFetchError(err)
		}
		return "", "", provider.NewStatsigUnavailable(provider.StatsigReasonSignerFailed)
	}
	return value, source, nil
}

// statsigConfigUsable reports ErrStatsigUnavailable, with its reason, when the
// configured Statsig source cannot produce a valid value, before any network
// call. "local" needs no configuration.
func statsigConfigUsable(cfg Config) error {
	switch cfg.StatsigMode {
	case "local":
		return nil
	case "manual":
		value := strings.TrimSpace(cfg.StatsigManualValue)
		if value == "" {
			return provider.NewStatsigUnavailable(provider.StatsigReasonManualMissing)
		}
		if !validStatsigID(value) {
			return provider.NewStatsigUnavailable(provider.StatsigReasonManualInvalid)
		}
		return nil
	case "url":
		if strings.TrimSpace(cfg.StatsigSignerURL) == "" {
			return provider.NewStatsigUnavailable(provider.StatsigReasonSignerURLEmpty)
		}
		if err := signerurl.Validate(cfg.StatsigSignerURL); err != nil {
			return provider.NewStatsigUnavailable(provider.StatsigReasonSignerURLBad)
		}
		return nil
	default:
		return provider.NewStatsigUnavailable(provider.StatsigReasonUnknownMode)
	}
}

// WarmStatsig 只使用一个 Web 账号和一个出口租约预热共享签名，不会逐账号访问上游。
func (a *Adapter) WarmStatsig(ctx context.Context, credential account.Credential) (int, error) {
	cfg := a.config()
	if err := statsigConfigUsable(cfg); err != nil {
		return 0, err
	}
	if cfg.StatsigMode == "manual" {
		return 0, nil
	}
	if a.statsig == nil {
		return 0, provider.NewStatsigUnavailable(provider.StatsigReasonNotInitialised)
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return 0, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	if cfg.StatsigMode == "local" {
		return a.statsig.WarmLocal(ctx, cfg.BaseURL, token, lease)
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return a.statsig.Warm(ctx, cfg.BaseURL, cfg.StatsigSignerURL, token, lease, []statsigWarmTarget{
		{method: http.MethodPost, target: baseURL + "/rest/app-chat/conversations/new"},
		{method: http.MethodPost, target: baseURL + "/rest/rate-limits"},
		{method: http.MethodPost, target: baseURL + "/rest/media/post/create"},
	})
}

// invalidateSignedStatsig forgets what produced a rejected value: the cached
// signature (url) or the page material (local). Manual values are kept.
func (a *Adapter) invalidateSignedStatsig(method, target string) bool {
	cfg := a.config()
	if a.statsig == nil {
		return false
	}
	switch cfg.StatsigMode {
	case "url":
		a.statsig.Invalidate(cfg.BaseURL, cfg.StatsigSignerURL, method, target)
	case "local":
		a.statsig.InvalidateLocal(cfg.BaseURL)
	default:
		return false
	}
	if parsed, err := url.Parse(target); err == nil {
		a.log().Info("web_statsig_invalidated", "mode", cfg.StatsigMode, "method", method, "path", parsed.EscapedPath())
	}
	return true
}
