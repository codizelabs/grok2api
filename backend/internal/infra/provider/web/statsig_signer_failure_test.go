package web

import (
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// A valid signer URL whose signer fails (here: an empty, unparsable response)
// must not let the request go out unsigned: nothing reaches /rest/ and the
// call returns an error wrapping provider.ErrStatsigUnavailable.
func TestP1URLModeFailingSignerSendsNothing(t *testing.T) {
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "url", StatsigSignerURL: "http://signer.internal/sign"})
			err := call.run(h, t)
			if n := h.restCalls.Load(); n != 0 {
				h.mu.Lock()
				ids := append([]string(nil), h.statsigIDs...)
				h.mu.Unlock()
				t.Errorf("%d request(s) reached /rest/ after the signer failed (x-statsig-id values %q), want 0", n, ids)
			}
			if len(h.signer.seen()) == 0 {
				t.Errorf("signer was never asked, want the configured signer URL to be tried")
			}
			if err == nil {
				t.Fatal("call returned nil error, want a statsig_unavailable error")
			}
			if !errors.Is(err, provider.ErrStatsigUnavailable) {
				t.Fatalf("err = %q, want it to wrap provider.ErrStatsigUnavailable", err.Error())
			}
		})
	}
}
