package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// A missing or short GROK2API_OBSERVE_TKN never stops grok2api: P6 is off,
// one warning is logged, and the token's value is never logged.
func TestCZObserveIsFeatureOnly(t *testing.T) {
	for _, c := range []struct {
		value string
		on    bool
	}{{"", false}, {"short-but-secret-VALUE", false}, {"  ", false}, {"observe-token-long-enough-0123456789abcdef", true}} {
		t.Setenv(ObserveTokenEnv, c.value)
		var logs bytes.Buffer
		svc, token := newCZObserve(config.Config{}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(&logs, nil)))
		if (svc != nil) != c.on || (token != "") != c.on {
			t.Fatalf("token %q: service %v token set %v", c.value, svc != nil, token != "")
		}
		if !c.on && !strings.Contains(logs.String(), "cz_observe_unavailable") {
			t.Fatalf("token %q: no warning logged", c.value)
		}
		if strings.TrimSpace(c.value) != "" && strings.Contains(logs.String(), strings.TrimSpace(c.value)) {
			t.Fatal("the token's value was logged")
		}
	}
}
