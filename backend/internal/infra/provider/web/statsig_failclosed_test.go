package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// p1Recorder captures every request the signer client would send and never
// lets one leave the process.
type p1Recorder struct {
	mu   sync.Mutex
	urls []string
}

func (r *p1Recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, request.URL.String())
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
		Request:    request,
	}, nil
}

func (r *p1Recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

type p1Harness struct {
	server     *httptest.Server
	adapter    *Adapter
	credential account.Credential
	restCalls  atomic.Int64
	mu         sync.Mutex
	statsigIDs []string
	signer     *p1Recorder
}

func newP1Harness(t *testing.T, cfg Config) *p1Harness {
	t.Helper()
	h := &p1Harness{signer: &p1Recorder{}}
	h.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/rest/") {
			h.restCalls.Add(1)
			h.mu.Lock()
			h.statsigIDs = append(h.statsigIDs, request.Header.Get("x-statsig-id"))
			h.mu.Unlock()
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
			return `<meta name="grok-site-verification" content="p1-fixed-meta">`, nil
		}
	}
	h.credential = account.Credential{ID: 91, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted}
	return h
}

func (h *p1Harness) callSyncQuota(t *testing.T) error {
	t.Helper()
	_, err := h.adapter.SyncQuota(context.Background(), h.credential)
	return err
}

func (h *p1Harness) callPostJSON(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	lease, err := h.adapter.egress.AcquireCredential(ctx, domainegress.ScopeWeb, h.credential)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	defer lease.Release()
	response, err := h.adapter.postJSON(ctx, h.adapter.config(), lease, "fake-sso",
		h.server.URL+"/rest/app-chat/conversations/new", map[string]any{"message": "p1"}, 5*time.Second)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return err
}

func (h *p1Harness) assertNothingSent(t *testing.T, err error) {
	t.Helper()
	if n := h.restCalls.Load(); n != 0 {
		h.mu.Lock()
		ids := append([]string(nil), h.statsigIDs...)
		h.mu.Unlock()
		t.Errorf("%d request(s) reached /rest/ without a usable Statsig signature (x-statsig-id values %q), want 0", n, ids)
	}
	if seen := h.signer.seen(); len(seen) != 0 {
		t.Errorf("signer client attempted %d request(s) %q, want 0 (no fallback signer)", len(seen), seen)
	}
	if err == nil {
		t.Fatal("call returned nil error, want a statsig_unavailable error")
	}
	if !strings.Contains(err.Error(), "statsig_unavailable") {
		t.Fatalf("err = %q, want it to contain %q", err.Error(), "statsig_unavailable")
	}
}

// testValidStatsigID is a well-formed x-statsig-id (70 bytes, base64) for
// fixtures: a missing or malformed manual value now fails closed (P1).
var testValidStatsigID = base64.RawStdEncoding.EncodeToString(make([]byte, 70))

type p1Call struct {
	name string
	run  func(*p1Harness, *testing.T) error
}

var p1Calls = []p1Call{
	{"SyncQuota", (*p1Harness).callSyncQuota},
	{"postJSON", (*p1Harness).callPostJSON},
}

func TestP1NewAdapterKeepsEmptySignerURL(t *testing.T) {
	h := newP1Harness(t, Config{StatsigMode: "url", StatsigSignerURL: ""})
	if got := h.adapter.config().StatsigSignerURL; got != "" {
		t.Fatalf("adapter StatsigSignerURL = %q, want empty (no built-in fallback signer)", got)
	}
}

func TestP1URLModeEmptySignerSendsNothing(t *testing.T) {
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "url", StatsigSignerURL: ""})
			h.assertNothingSent(t, call.run(h, t))
		})
	}
}

func TestP1URLModeInvalidSignerSendsNothing(t *testing.T) {
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "url", StatsigSignerURL: "http://grok.wodf.de/sign"})
			h.assertNothingSent(t, call.run(h, t))
		})
	}
}

func TestP1ManualModeMissingValueSendsNothing(t *testing.T) {
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "manual", StatsigManualValue: ""})
			h.assertNothingSent(t, call.run(h, t))
		})
	}
}

func TestP1ManualModeInvalidValueSendsNothing(t *testing.T) {
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "manual", StatsigManualValue: "invalid"})
			h.assertNothingSent(t, call.run(h, t))
		})
	}
}

func TestP1ManualModeValidValueSendsSignedRequest(t *testing.T) {
	value := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "manual", StatsigManualValue: value})
			// The stub's empty JSON may not parse as a quota; only what went on the wire matters here.
			_ = call.run(h, t)
			if n := h.restCalls.Load(); n == 0 {
				t.Fatal("valid manual Statsig value: no request reached /rest/, want at least one")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			for i, got := range h.statsigIDs {
				if got != value {
					t.Fatalf("request %d x-statsig-id = %q, want the manual value %q", i, got, value)
				}
			}
			if seen := h.signer.seen(); len(seen) != 0 {
				t.Fatalf("manual mode contacted the signer %q, want 0", seen)
			}
		})
	}
}
