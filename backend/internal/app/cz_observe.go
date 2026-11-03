package app

import (
	"log/slog"
	"os"
	"strings"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/cz/observe"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// ObserveTokenEnv names fork patch P6's own read-only token. It is separate
// from the admin login and from client keys, and it opens only
// /cz/v1/observe/*.
const ObserveTokenEnv = "GROK2API_OBSERVE_TKN"

// newCZObserve builds fork patch P6. It is feature-only: with no token (or a
// short one) grok2api starts as before, logs one warning and P6's reads
// answer 503. It never fails startup.
func newCZObserve(cfg config.Config, accounts *accountapp.Service, usage *dashboardapp.Service, requests *auditapp.Service, proxies *egressapp.Service, logger *slog.Logger) (*observe.Service, string) {
	token := strings.TrimSpace(os.Getenv(ObserveTokenEnv))
	if len(token) < observe.MinTokenLength {
		if token != "" {
			logger.Warn("cz_observe_unavailable", "reason", "observe token shorter than the minimum; /cz/v1/observe answers 503", "min_length", observe.MinTokenLength)
		} else {
			logger.Warn("cz_observe_unavailable", "reason", "observe token not set; /cz/v1/observe answers 503", "env", ObserveTokenEnv)
		}
		return nil, ""
	}
	return observe.New(observe.Options{Accounts: accounts, Usage: usage, Requests: requests, Proxies: proxies, LabelSecret: cfg.Secrets.CredentialEncryptionKey}), token
}
