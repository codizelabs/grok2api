package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

var czValidStatsig = base64.RawStdEncoding.EncodeToString(make([]byte, 70))

func newCZTransport(t *testing.T, cfg Config) (*Adapter, *fakegrok.Server, account.Credential) {
	t.Helper()
	fake := fakegrok.New("")
	t.Cleanup(fake.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("acct-a")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseURL = fake.URL
	adapter := NewAdapter(cfg, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	return adapter, fake, account.Credential{ID: 7, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted}
}

type czCall struct {
	name, method, path string
	body               string
}

func czAllowedRestCalls(conversation, asset, head string) []czCall {
	return []czCall{
		{"list responses", http.MethodGet, "/rest/app-chat/conversations/" + conversation + "/responses?conversationKind=CONVERSATION_KIND_IMAGINE", ""},
		{"read asset", http.MethodGet, "/rest/assets/" + asset, ""},
		{"segments", http.MethodPost, "/rest/media/segment", `{"assetId":"` + asset + `","cachedOnly":true,"maskFormat":"rle"}`},
		{"quota", http.MethodPost, "/rest/media/imagine/quota_info", `{}`},
		{"create", http.MethodPost, "/rest/app-chat/conversations/new", `{"modelName":"imagine-image-edit","message":"m","kind":"CONVERSATION_KIND_IMAGINE","mediaGenInput":{"imageToImage":{"prompt":"m","inputAssets":[]}}}`},
		{"add response", http.MethodPost, "/rest/app-chat/conversations/" + conversation + "/responses", `{"modelName":"imagine-image-edit","message":"m","parentResponseId":"` + head + `","mediaGenInput":{"imageToImage":{"prompt":"m","inputAssets":["` + asset + `"]}}}`},
	}
}

// F2: every allowed /rest/ call, GETs included, carries x-statsig-id.
func TestCZImagineTransportSignsEveryRestCall(t *testing.T) {
	adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: czValidStatsig})
	_, asset, head := fake.SeedPost("acct-a", "conv-sign")
	for _, call := range czAllowedRestCalls("conv-sign", asset, head) {
		var body []byte
		if call.body != "" {
			body = []byte(call.body)
		}
		response, err := adapter.DoImagine(context.Background(), credential, call.method, call.path, body)
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	calls := fake.Calls()
	if len(calls) != len(czAllowedRestCalls("", "", "")) {
		t.Fatalf("fake saw %d calls, want %d", len(calls), len(czAllowedRestCalls("", "", "")))
	}
	for _, call := range calls {
		if call.Statsig != czValidStatsig {
			t.Errorf("%s %s carried x-statsig-id %q, want the signed value", call.Method, call.Path, call.Statsig)
		}
	}
}

// F2: manual mode with an empty or invalid value makes zero calls and
// returns statsig_unavailable, for reads and sends alike.
func TestCZImagineTransportStatsigFailsClosed(t *testing.T) {
	for _, value := range []string{"", "invalid"} {
		adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: value})
		_, asset, head := fake.SeedPost("acct-a", "conv-closed")
		for _, call := range czAllowedRestCalls("conv-closed", asset, head) {
			response, err := adapter.DoImagine(context.Background(), credential, call.method, call.path, []byte(call.body))
			if response != nil {
				_ = response.Body.Close()
			}
			if !errors.Is(err, provider.ErrStatsigUnavailable) {
				t.Errorf("manual %q, %s: err = %v, want ErrStatsigUnavailable", value, call.name, err)
			}
		}
		if _, err := adapter.UploadImagine(context.Background(), credential, "a.png", "image/png", []byte("png")); !errors.Is(err, provider.ErrStatsigUnavailable) {
			t.Errorf("manual %q, upload: err = %v, want ErrStatsigUnavailable", value, err)
		}
		if n := len(fake.Calls()); n != 0 {
			t.Fatalf("manual %q: %d calls reached the fake, want 0", value, n)
		}
	}
}

// The transport refuses anything outside /rest/ with zero calls.
func TestCZImagineTransportRefusesOtherPaths(t *testing.T) {
	adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: czValidStatsig})
	for _, call := range []czCall{
		{"bot websocket", http.MethodGet, "/ws/bot/", ""},
		{"absolute url", http.MethodGet, "https://cdn.grok.com/x.js", ""},
		{"traversal", http.MethodGet, "/rest/../imagine", ""},
		{"double slash", http.MethodGet, "//cdn.grok.com/rest/x", ""},
		{"delete", http.MethodDelete, "/rest/assets/a", ""},
		{"server action", http.MethodPost, "/imagine/post/abc", "[]"},
		{"get with body", http.MethodGet, "/rest/assets/a", "{}"},
	} {
		_, err := adapter.DoImagine(context.Background(), credential, call.method, call.path, []byte(call.body))
		if !errors.Is(err, ErrImagineCallRefused) {
			t.Errorf("%s: err = %v, want ErrImagineCallRefused", call.name, err)
		}
	}
	if _, _, err := adapter.FetchImagineAsset(context.Background(), credential, "../etc/passwd"); !errors.Is(err, ErrImagineCallRefused) {
		t.Errorf("asset traversal: err = %v, want ErrImagineCallRefused", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("%d calls reached the fake, want 0", n)
	}
}

// A non-2xx answer comes back as ImagineHTTPError with a hash, never the body.
func TestCZImagineTransportRejectionKeepsNoBody(t *testing.T) {
	adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: czValidStatsig})
	_, err := adapter.DoImagine(context.Background(), credential, http.MethodGet, "/rest/assets/asset-missing", nil)
	var httpErr *ImagineHTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound || httpErr.BodyPrefixSHA256 == "" {
		t.Fatalf("err = %#v, want ImagineHTTPError 404 with a body hash", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("error text %q carries the upstream body", err.Error())
	}
	if fake.Count(http.MethodGet, `^/rest/assets/`) != 1 {
		t.Fatal("want exactly one call, no replay")
	}
}

func TestCZImagineTransportUploadAndMedia(t *testing.T) {
	adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: czValidStatsig})
	id, err := adapter.UploadImagine(context.Background(), credential, "anchor.png", "image/png", []byte("\x89PNG fake"))
	if err != nil || id == "" {
		t.Fatalf("upload id=%q err=%v", id, err)
	}
	if _, err := adapter.UploadImagine(context.Background(), credential, "x.txt", "text/plain", []byte("x")); !errors.Is(err, ErrImagineCallRefused) {
		t.Fatalf("non-image upload err = %v, want refused", err)
	}
	previous := czImagineAssetsBase
	czImagineAssetsBase = fake.URL
	t.Cleanup(func() { czImagineAssetsBase = previous })
	_, asset, _ := fake.SeedPost("acct-a", "conv-media")
	body, contentType, err := adapter.FetchImagineAsset(context.Background(), credential, "users/u/generated/"+asset+"/image.jpg")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	_ = body.Close()
	if contentType != "image/jpeg" || string(data) != "FAKE-MEDIA-"+asset {
		t.Fatalf("media = %q %q", contentType, data)
	}
}
