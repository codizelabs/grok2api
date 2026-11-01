package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// P5b: statsigMode "browser" sends what grok.com's browser sends: no
// x-statsig-id at all and a fresh UUIDv4 x-xai-request-id per request.

var czP5bUUIDv4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type czP5bRequest struct {
	method string
	path   string
	header http.Header
}

// czP5bHarness is a fake grok.com that records every request (any path,
// all headers), plus counters on every way the adapter could reach grok.com's
// page or a signer.
type czP5bHarness struct {
	server      *httptest.Server
	adapter     *Adapter
	credential  account.Credential
	signer      *p1Recorder
	pageFetches atomic.Int64
	mu          sync.Mutex
	requests    []czP5bRequest
}

func newCZP5bHarness(t *testing.T, cfg Config) *czP5bHarness {
	t.Helper()
	h := &czP5bHarness{signer: &p1Recorder{}}
	h.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, czP5bRequest{method: request.Method, path: request.URL.Path, header: request.Header.Clone()})
		h.mu.Unlock()
		if strings.HasPrefix(request.URL.Path, "/rest/") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(h.server.Close)

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("fake-sso")
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseURL = h.server.URL
	h.adapter = NewAdapter(cfg, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	if h.adapter.statsig != nil {
		h.adapter.statsig.client = &http.Client{Transport: h.signer}
		h.adapter.statsig.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
			h.pageFetches.Add(1)
			return `<meta name="grok-site-verification" content="p5b-fixed-meta">`, nil
		}
		h.adapter.statsig.fetchLocal = func(context.Context, string, string, *infraegress.Lease) (statsigLocalMaterial, error) {
			h.pageFetches.Add(1)
			return statsigLocalMaterial{}, errors.New("p5b: page material must not be loaded")
		}
	}
	h.credential = account.Credential{ID: 93, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted}
	return h
}

func (h *czP5bHarness) seen() []czP5bRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]czP5bRequest(nil), h.requests...)
}

func (h *czP5bHarness) quotaGroup() error {
	_, err := h.adapter.SyncQuotaGroup(context.Background(), h.credential, account.QuotaGroupWebImagine)
	return err
}

func (h *czP5bHarness) syncQuota() error {
	_, err := h.adapter.SyncQuota(context.Background(), h.credential)
	return err
}

func (h *czP5bHarness) postJSON(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	lease, err := h.adapter.egress.AcquireCredential(ctx, domainegress.ScopeWeb, h.credential)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	defer lease.Release()
	response, err := h.adapter.postJSON(ctx, h.adapter.config(), lease, "fake-sso",
		h.server.URL+"/rest/app-chat/conversations/new", map[string]any{"message": "p5b"}, 5*time.Second)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return err
}

func (h *czP5bHarness) imagine(method, path, body string) error {
	var payload []byte
	if body != "" {
		payload = []byte(body)
	}
	response, err := h.adapter.DoImagine(context.Background(), h.credential, method, path, payload)
	if response != nil && response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	return err
}

// assertBrowserWire checks every recorded request went to /rest/, carried no
// x-statsig-id header (not even an empty one), and a UUIDv4
// x-xai-request-id; that the ids are all different; and that neither
// grok.com's page nor a signer was asked.
func (h *czP5bHarness) assertBrowserWire(t *testing.T) []czP5bRequest {
	t.Helper()
	requests := h.seen()
	if len(requests) == 0 {
		t.Fatal("browser mode: no request reached the fake grok.com, want at least one")
	}
	ids := map[string]string{}
	for i, request := range requests {
		where := request.method + " " + request.path
		if !strings.HasPrefix(request.path, "/rest/") {
			t.Errorf("request %d %s: browser mode fetched a non-/rest/ path (grok.com's page?), want only /rest/ calls", i, where)
			continue
		}
		if values, present := request.header[http.CanonicalHeaderKey("x-statsig-id")]; present {
			t.Errorf("request %d %s carried x-statsig-id %q, want no such header in browser mode", i, where, values)
		}
		id := request.header.Get("x-xai-request-id")
		if !czP5bUUIDv4.MatchString(id) {
			t.Errorf("request %d %s x-xai-request-id = %q, want a lowercase UUIDv4", i, where, id)
			continue
		}
		if previous, dup := ids[id]; dup {
			t.Errorf("request %d %s reused x-xai-request-id %q from %s, want a fresh id per request", i, where, id, previous)
		}
		ids[id] = where
	}
	if seen := h.signer.seen(); len(seen) != 0 {
		t.Errorf("browser mode contacted a signer %q, want 0", seen)
	}
	if n := h.pageFetches.Load(); n != 0 {
		t.Errorf("browser mode loaded grok.com's page %d time(s), want 0", n)
	}
	return requests
}

func (h *czP5bHarness) assertSent(t *testing.T, method, path string) {
	t.Helper()
	for _, request := range h.seen() {
		if request.method == method && request.path == path {
			return
		}
	}
	t.Fatalf("no %s %s reached the fake grok.com; saw %v", method, path, h.seen())
}

func (h *czP5bHarness) assertNoGrokCalls(t *testing.T) {
	t.Helper()
	if requests := h.seen(); len(requests) != 0 {
		t.Fatalf("%d request(s) reached the fake grok.com (%v), want 0", len(requests), requests)
	}
}

func czP5bAssertBrowserRequired(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want statsig_unavailable (browser required)")
	}
	if !errors.Is(err, provider.ErrStatsigUnavailable) {
		t.Fatalf("err = %v, want it to wrap provider.ErrStatsigUnavailable", err)
	}
	if got := provider.StatsigUnavailableReason(err); got != provider.StatsigReasonBrowserRequired {
		t.Fatalf("StatsigUnavailableReason = %q, want %q", got, provider.StatsigReasonBrowserRequired)
	}
}

func czP5bMarkRequired(t *testing.T, keys ...string) {
	t.Helper()
	previous := statsigRequiredEndpoints
	next := make(map[string]bool, len(previous)+len(keys))
	for key, value := range previous {
		next[key] = value
	}
	for _, key := range keys {
		next[key] = true
	}
	statsigRequiredEndpoints = next
	t.Cleanup(func() { statsigRequiredEndpoints = previous })
}

// ---------- defaults ----------

func TestCZP5bNewAdapterDefaultsToBrowserMode(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: ""})
	if got := h.adapter.config().StatsigMode; got != "browser" {
		t.Fatalf("adapter StatsigMode = %q, want %q for an empty mode", got, "browser")
	}
}

func TestCZP5bStatsigConfigUsableAcceptsBrowser(t *testing.T) {
	if err := statsigConfigUsable(Config{StatsigMode: "browser"}); err != nil {
		t.Fatalf("statsigConfigUsable(browser) = %v, want nil", err)
	}
}

func TestCZP5bStatsigRequiredEndpointsEmptyByDefault(t *testing.T) {
	if n := len(statsigRequiredEndpoints); n != 0 {
		t.Fatalf("statsigRequiredEndpoints has %d entries %v, want none by default", n, statsigRequiredEndpoints)
	}
}

// ---------- browser mode on the wire ----------

func TestCZP5bBrowserModeQuotaGroupSendsBrowserHeaders(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	_ = h.quotaGroup() // the stub's {} may not parse as a quota; only the wire matters
	h.assertSent(t, http.MethodPost, "/rest/media/imagine/quota_info")
	h.assertBrowserWire(t)
}

func TestCZP5bBrowserModeFullQuotaSyncSendsBrowserHeaders(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	_ = h.syncQuota()
	h.assertSent(t, http.MethodPost, "/rest/media/imagine/quota_info")
	h.assertBrowserWire(t)
}

func TestCZP5bBrowserModePostJSONSendsBrowserHeaders(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	_ = h.postJSON(t)
	h.assertSent(t, http.MethodPost, "/rest/app-chat/conversations/new")
	h.assertBrowserWire(t)
}

func TestCZP5bBrowserModeDoImagineSendsBrowserHeaders(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	if err := h.imagine(http.MethodGet, "/rest/assets/asset-p5b", ""); err != nil {
		t.Fatalf("DoImagine GET /rest/assets: %v", err)
	}
	if err := h.imagine(http.MethodPost, "/rest/media/post/list", `{}`); err != nil {
		t.Fatalf("DoImagine POST /rest/media/post/list: %v", err)
	}
	requests := h.assertBrowserWire(t)
	if len(requests) != 2 {
		t.Fatalf("fake saw %d requests, want 2", len(requests))
	}
	first, second := requests[0].header.Get("x-xai-request-id"), requests[1].header.Get("x-xai-request-id")
	if first == second {
		t.Fatalf("two calls shared x-xai-request-id %q, want two different ids", first)
	}
}

// The same call twice still gets two ids: the id is per request, not cached.
func TestCZP5bBrowserModeRepeatedCallGetsFreshRequestID(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	for i := 0; i < 3; i++ {
		_ = h.quotaGroup()
	}
	if requests := h.assertBrowserWire(t); len(requests) != 3 {
		t.Fatalf("fake saw %d requests, want 3", len(requests))
	}
}

// ---------- other modes still fail closed ----------

func TestCZP5bURLModeFailingSignerDoImagineSendsNothing(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "url", StatsigSignerURL: "http://signer.internal/sign"})
	err := h.imagine(http.MethodGet, "/rest/assets/asset-p5b", "")
	if !errors.Is(err, provider.ErrStatsigUnavailable) {
		t.Fatalf("err = %v, want provider.ErrStatsigUnavailable", err)
	}
	h.assertNoGrokCalls(t)
}

func TestCZP5bURLModeFailingSignerQuotaGroupSendsNothing(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "url", StatsigSignerURL: "http://signer.internal/sign"})
	err := h.quotaGroup()
	if !errors.Is(err, provider.ErrStatsigUnavailable) {
		t.Fatalf("err = %v, want provider.ErrStatsigUnavailable", err)
	}
	h.assertNoGrokCalls(t)
}

func TestCZP5bManualModeStillSendsManualValue(t *testing.T) {
	value := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	h := newCZP5bHarness(t, Config{StatsigMode: "manual", StatsigManualValue: value})
	if err := h.imagine(http.MethodGet, "/rest/assets/asset-p5b", ""); err != nil {
		t.Fatalf("DoImagine manual: %v", err)
	}
	requests := h.seen()
	if len(requests) != 1 {
		t.Fatalf("fake saw %d requests, want 1", len(requests))
	}
	if got := requests[0].header.Get("x-statsig-id"); got != value {
		t.Fatalf("manual mode x-statsig-id = %q, want the manual value %q", got, value)
	}
}

// ---------- required-path table ----------

func TestCZP5bBrowserRequiredQuotaPathFailsClosed(t *testing.T) {
	czP5bMarkRequired(t, "POST /rest/media/imagine/quota_info")
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	czP5bAssertBrowserRequired(t, h.quotaGroup())
	h.assertNoGrokCalls(t)
	if seen := h.signer.seen(); len(seen) != 0 {
		t.Fatalf("signer contacted %q, want 0", seen)
	}
	if n := h.pageFetches.Load(); n != 0 {
		t.Fatalf("grok.com page loaded %d time(s), want 0", n)
	}
}

func TestCZP5bBrowserRequiredImaginePathFailsClosedOthersSend(t *testing.T) {
	czP5bMarkRequired(t, "POST /rest/app-chat/conversations/{id}/responses")
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})

	czP5bAssertBrowserRequired(t, h.imagine(http.MethodPost, "/rest/app-chat/conversations/abc123/responses", `{"message":"m"}`))
	h.assertNoGrokCalls(t)

	// Same endpoint with another method, and an unmarked endpoint, still go out.
	if err := h.imagine(http.MethodGet, "/rest/app-chat/conversations/abc123/responses?conversationKind=CONVERSATION_KIND_IMAGINE", ""); err != nil {
		t.Fatalf("unmarked GET responses: %v", err)
	}
	if err := h.imagine(http.MethodPost, "/rest/media/post/list", `{}`); err != nil {
		t.Fatalf("unmarked POST /rest/media/post/list: %v", err)
	}
	requests := h.assertBrowserWire(t)
	if len(requests) != 2 {
		t.Fatalf("fake saw %d requests, want 2 (the unmarked calls only)", len(requests))
	}
	for _, request := range requests {
		if request.method == http.MethodPost && strings.HasSuffix(request.path, "/responses") {
			t.Fatalf("marked POST %s reached the fake grok.com, want it refused", request.path)
		}
	}
}

// ---------- warm-up ----------

func TestCZP5bWarmStatsigBrowserModeIsNoop(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	// A zero credential cannot be decrypted: browser mode must return before
	// it tries.
	warmed, err := h.adapter.WarmStatsig(context.Background(), account.Credential{})
	if err != nil || warmed != 0 {
		t.Fatalf("WarmStatsig(browser) = (%d, %v), want (0, nil)", warmed, err)
	}
	h.assertNoGrokCalls(t)
	if seen := h.signer.seen(); len(seen) != 0 {
		t.Fatalf("WarmStatsig(browser) contacted a signer %q, want 0", seen)
	}
	if n := h.pageFetches.Load(); n != 0 {
		t.Fatalf("WarmStatsig(browser) loaded grok.com's page %d time(s), want 0", n)
	}
}
