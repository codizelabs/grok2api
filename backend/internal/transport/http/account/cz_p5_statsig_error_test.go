package account

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

func TestCZP5WriteServiceErrorMapsStatsigUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, err := range map[string]error{
		"direct":  provider.NewStatsigUnavailable(provider.StatsigReasonCurvesMissing),
		"wrapped": fmt.Errorf("sync quota: %w", provider.NewStatsigUnavailable(provider.StatsigReasonCurvesMissing)),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			new(Handler).writeServiceError(ctx, "fallback", err, http.StatusInternalServerError, "failed")
			body := recorder.Body.String()
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body = %s", recorder.Code, body)
			}
			if !strings.Contains(body, `"code":"statsig_unavailable"`) {
				t.Fatalf("body = %s, want code statsig_unavailable", body)
			}
			if !strings.Contains(body, provider.StatsigReasonCurvesMissing) {
				t.Fatalf("body = %s, want the reason %q", body, provider.StatsigReasonCurvesMissing)
			}
		})
	}
}
