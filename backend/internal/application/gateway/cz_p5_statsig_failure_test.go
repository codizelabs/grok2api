package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestCZP5TransportFailureCarriesStatsigReason(t *testing.T) {
	reasons := []string{
		provider.StatsigReasonMetaMissing,
		provider.StatsigReasonCurvesInvalid,
		provider.StatsigReasonManualMissing,
		provider.StatsigReasonSignerURLEmpty,
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			for name, err := range map[string]error{
				"direct":  provider.NewStatsigUnavailable(reason),
				"wrapped": fmt.Errorf("sync quota: %w", provider.NewStatsigUnavailable(reason)),
			} {
				failure := newTransportUpstreamFailure(err, 7, "web")
				if failure.HTTPStatus != http.StatusServiceUnavailable {
					t.Fatalf("%s: HTTPStatus = %d, want 503", name, failure.HTTPStatus)
				}
				if failure.Code != "statsig_unavailable" {
					t.Fatalf("%s: Code = %q, want statsig_unavailable", name, failure.Code)
				}
				if !strings.Contains(failure.PublicMessage, reason) {
					t.Fatalf("%s: PublicMessage = %q, want it to contain %q", name, failure.PublicMessage, reason)
				}
				if failure.AccountScoped || failure.CredentialRejected {
					t.Fatalf("%s: statsig failure marked account-scoped: %#v", name, failure)
				}
			}
		})
	}
}
