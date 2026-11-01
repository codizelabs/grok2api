package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// czSigner answers every signing request with value, counting them.
type czSigner struct {
	mu    sync.Mutex
	value string
	calls int
}

func (s *czSigner) RoundTrip(request *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"x-statsig-id":"` + s.value + `"}`))),
		Request:    request,
	}, nil
}

func newCZURLTransport(t *testing.T, transport http.RoundTripper) (*Adapter, *fakegrok.Server, account.Credential) {
	t.Helper()
	adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "url", StatsigSignerURL: "http://signer.internal/sign"})
	adapter.statsig.client = &http.Client{Transport: transport}
	adapter.statsig.validateEndpoint = func(context.Context, string) error { return nil }
	adapter.statsig.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
		return `<meta name="grok-site-verification" content="cz-fixed-meta">`, nil
	}
	return adapter, fake, credential
}

// url mode: SignImagine asks the signer for exactly the send's request
// before anything of the send goes out, and the send then reuses that
// signature. A failing signer fails closed with zero grok.com calls.
func TestCZSignImagineURLMode(t *testing.T) {
	const sendPath = "/rest/app-chat/conversations/new"

	failing := &p1Recorder{} // answers 200 with no signature
	adapter, fake, credential := newCZURLTransport(t, failing)
	if err := adapter.SignImagine(context.Background(), credential, http.MethodPost, sendPath); !errors.Is(err, provider.ErrStatsigUnavailable) {
		t.Fatalf("failing signer: err = %v, want ErrStatsigUnavailable", err)
	}
	if len(failing.seen()) == 0 {
		t.Fatal("the signer was never asked")
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("%d grok.com calls while signing, want 0", n)
	}

	signer := &czSigner{value: czValidStatsig}
	good, goodFake, goodCredential := newCZURLTransport(t, signer)
	if err := good.SignImagine(context.Background(), goodCredential, http.MethodPost, sendPath); err != nil {
		t.Fatalf("working signer: %v", err)
	}
	if signer.calls != 1 || len(goodFake.Calls()) != 0 {
		t.Fatalf("signer calls = %d, grok.com calls = %d; want 1 and 0", signer.calls, len(goodFake.Calls()))
	}
	response, err := good.DoImagine(context.Background(), goodCredential, http.MethodPost, sendPath, []byte(`{}`))
	if err == nil {
		_ = response.Body.Close()
	}
	calls := goodFake.Calls()
	if signer.calls != 1 || len(calls) != 1 || calls[0].Statsig != czValidStatsig {
		t.Fatalf("the send after signing: signer calls = %d, grok.com calls = %+v; want the cached signature reused", signer.calls, calls)
	}
	if err := good.SignImagine(context.Background(), goodCredential, http.MethodGet, "/rest/assets/a"); !errors.Is(err, ErrImagineCallRefused) {
		t.Fatalf("signing a GET: err = %v, want refused (only sends are signed ahead)", err)
	}
}

// Manual mode: SignImagine only checks the value, with no call of any kind.
func TestCZSignImagineManualMode(t *testing.T) {
	adapter, fake, credential := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: czValidStatsig})
	if err := adapter.SignImagine(context.Background(), credential, http.MethodPost, "/rest/app-chat/conversations/new"); err != nil {
		t.Fatalf("valid manual value: %v", err)
	}
	bad, _, _ := newCZTransport(t, Config{StatsigMode: "manual", StatsigManualValue: ""})
	if err := bad.SignImagine(context.Background(), credential, http.MethodPost, "/rest/app-chat/conversations/new"); !errors.Is(err, provider.ErrStatsigUnavailable) {
		t.Fatalf("empty manual value: err = %v, want ErrStatsigUnavailable", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("%d grok.com calls, want 0", n)
	}
}
