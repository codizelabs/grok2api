package observe

import (
	"context"
	"encoding/json"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func router(service *Service, token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Mount(r.Group("/cz/v1/observe"), service, token)
	return r
}

func do(r http.Handler, method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{"enabled":false}`))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

var readPaths = []string{"/cz/v1/observe/", "/cz/v1/observe/overview", "/cz/v1/observe/accounts", "/cz/v1/observe/summary",
	"/cz/v1/observe/usage", "/cz/v1/observe/usage?period=7d", "/cz/v1/observe/requests?limit=100", "/cz/v1/observe/proxies"}

// O1: no credential, token, e-mail, name, user id, client IP, header, body,
// proxy URL, exit IP or free-text error appears in any P6 response.
func TestNoCredentialOrPromptInAnyResponse(t *testing.T) {
	r := router(newCanaryService(), testToken)
	for _, path := range readPaths {
		rec := do(r, http.MethodGet, path, "Bearer "+testToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", path, rec.Code, rec.Body.String())
		}
		body := strings.ToLower(rec.Body.String())
		for _, canary := range []string{"canary", "@example.com", "sk-live", "bearer", "10.0.0.1", "prompt"} {
			if strings.Contains(body, canary) {
				t.Fatalf("%s leaks %q: %s", path, canary, rec.Body.String())
			}
		}
	}
}

// O2: P6 is read-only. Every write method answers 405 on every path, with or
// without the token, before auth and before the configured check.
func TestWriteMethodsAre405(t *testing.T) {
	for _, configured := range []bool{true, false} {
		svc, token := newCanaryService(), testToken
		if !configured {
			svc, token = nil, ""
		}
		r := router(svc, token)
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
			for _, path := range []string{"/cz/v1/observe/accounts", "/cz/v1/observe/accounts/1", "/cz/v1/observe/anything", "/cz/v1/observe/"} {
				for _, auth := range []string{"", "Bearer " + testToken} {
					rec := do(r, method, path, auth)
					if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
						t.Fatalf("configured=%v %s %s auth=%v: status %d allow %q", configured, method, path, auth != "", rec.Code, rec.Header().Get("Allow"))
					}
				}
			}
		}
	}
}

// O3: its own token, compared in constant time; anything else is 401.
func TestTokenIsRequiredAndScoped(t *testing.T) {
	r := router(newCanaryService(), testToken)
	for _, auth := range []string{"", "Bearer", "Bearer ", "Bearer wrong", "Basic " + testToken, testToken, "Bearer " + testToken + "x", "Bearer " + strings.ToUpper(testToken),
		"Bearer eyJhbGciOiJIUzI1NiJ9.admin.jwt"} {
		for _, path := range readPaths {
			rec := do(r, http.MethodGet, path, auth)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "observe_unauthorized") {
				t.Fatalf("%s with %q: status %d", path, auth, rec.Code)
			}
		}
	}
	if rec := do(r, http.MethodGet, "/cz/v1/observe/accounts", "Bearer "+testToken); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("with the token: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// O4: no token, a short token or no service: every read is 503, never a crash.
func TestUnconfiguredIs503(t *testing.T) {
	for _, c := range []struct {
		svc   *Service
		token string
	}{{nil, testToken}, {newCanaryService(), ""}, {newCanaryService(), "short-token"}} {
		r := router(c.svc, c.token)
		for _, path := range readPaths {
			rec := do(r, http.MethodGet, path, "Bearer "+c.token)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "observe_not_configured") {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
			}
		}
	}
}

func get[T any](t *testing.T, r http.Handler, path string) T {
	t.Helper()
	rec := do(r, http.MethodGet, path, "Bearer "+testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// O5: each account's state, reason and until; masked identity; opaque label;
// the free-text error as a code; the quota windows.
func TestAccountStatesAndMasks(t *testing.T) {
	r := router(newCanaryService(), testToken)
	list := get[AccountList](t, r, "/cz/v1/observe/accounts")
	if len(list.Accounts) != 5 || list.Total != 5 || list.Truncated {
		t.Fatalf("accounts: %+v", list)
	}
	want := []struct{ state, reason string }{{"active", ""}, {"cooling", "cooldown"}, {"disabled", "config"}, {"failed", "auth"}, {"cooling", "quota"}}
	seen := map[string]bool{}
	for i, a := range list.Accounts {
		if a.State != want[i].state || a.Reason != want[i].reason {
			t.Fatalf("account %d: %s/%s, want %+v", i, a.State, a.Reason, want[i])
		}
		if !strings.HasPrefix(a.Label, "acct_") || len(a.Label) != 21 || seen[a.Label] {
			t.Fatalf("label %q", a.Label)
		}
		seen[a.Label] = true
		if a.IdentityMasked != "CA…RY" || a.LastErrorCode != "rate_limited" || a.Provider != "grok_web" || a.Tier != "super" {
			t.Fatalf("account %d: %+v", i, a)
		}
		if len(a.QuotaWindows) != 1 || a.QuotaWindows[0].Mode != "video" || a.QuotaWindows[0].Total != 10 {
			t.Fatalf("windows: %+v", a.QuotaWindows)
		}
	}
	if list.Accounts[1].Until == nil || !list.Accounts[1].Until.Equal(t0.Add(30*60e9)) || list.Accounts[4].Until == nil {
		t.Fatalf("until: %+v %+v", list.Accounts[1].Until, list.Accounts[4].Until)
	}
	// the same label P3 shows for the same account
	if list.Accounts[0].Label != New(Options{LabelSecret: "label-secret"}).accountLabel(1) {
		t.Fatal("label is not P3's keyed label")
	}
}

func TestUsageRequestsProxiesAndSummary(t *testing.T) {
	a, u, q, p := canaryWorld()
	r := router(New(Options{Accounts: a, Usage: u, Requests: q, Proxies: p, LabelSecret: "s", Now: func() time.Time { return t0 }}), testToken)
	usage := get[UsageView](t, r, "/cz/v1/observe/usage")
	if usage.Requests != 40 || usage.SuccessRate == nil || *usage.SuccessRate != 0.95 || usage.AvgFirstTokenMS == nil || *usage.AvgFirstTokenMS != 1000 ||
		len(usage.Series) != 1 || len(usage.Providers) != 1 || u.periods[0] != "24h|UTC" {
		t.Fatalf("usage: %+v %v", usage, u.periods)
	}
	if rec := do(r, http.MethodGet, "/cz/v1/observe/usage?period=30d", "Bearer "+testToken); rec.Code != http.StatusBadRequest {
		t.Fatalf("30d: %d", rec.Code)
	}
	reqs := get[RequestList](t, r, "/cz/v1/observe/requests?limit=2")
	if len(reqs.Requests) != 2 || !reqs.Requests[0].OK || reqs.Requests[1].OK || reqs.Requests[1].ErrorCode != "error" ||
		reqs.Requests[0].Model != "grok-imagine-video" || !strings.HasPrefix(reqs.Requests[0].AccountLabel, "acct_") || reqs.Requests[0].Operation != "video" {
		t.Fatalf("requests: %+v", reqs)
	}
	for _, bad := range []string{"0", "101", "x"} {
		if rec := do(r, http.MethodGet, "/cz/v1/observe/requests?limit="+bad, "Bearer "+testToken); rec.Code != http.StatusBadRequest {
			t.Fatalf("limit %s: %d", bad, rec.Code)
		}
	}
	px := get[ProxyList](t, r, "/cz/v1/observe/proxies")
	if px.Total != 2 || px.Healthy != 1 || px.Unhealthy != 1 || px.Cooling != 1 || px.Proxies[0].State != "healthy" || px.Proxies[1].State != "cooling" ||
		px.Proxies[0].LastErrorCode != "network" || !strings.HasPrefix(px.Proxies[0].Label, "proxy_") {
		t.Fatalf("proxies: %+v", px)
	}
	sum := get[Summary](t, r, "/cz/v1/observe/summary")
	if sum.Total != 3 || sum.Available != 1 || sum.Providers["grok_web"].Total != 3 || len(sum.Providers) != 3 {
		t.Fatalf("summary: %+v", sum)
	}
	ov := get[Overview](t, r, "/cz/v1/observe/overview?limit=1")
	if ov.Summary == nil || ov.Accounts == nil || ov.Usage == nil || ov.Requests == nil || len(ov.Requests.Requests) != 1 || ov.Proxies == nil || len(ov.Errors) != 0 {
		t.Fatalf("overview: %+v", ov)
	}
}

// A failing source is named in the overview; the rest still answers.
func TestOverviewNamesAFailingSource(t *testing.T) {
	a, u, q, p := canaryWorld()
	a.err = io.ErrUnexpectedEOF
	svc := New(Options{Accounts: a, Usage: u, Requests: q, Proxies: p, LabelSecret: "s"})
	ov := svc.Overview(context.Background(), 5)
	if ov.Accounts != nil || ov.Summary != nil || ov.Usage == nil || strings.Join(ov.Errors, ",") != "summary,accounts" {
		t.Fatalf("overview: %+v", ov)
	}
	r := router(New(Options{LabelSecret: "s"}), testToken)
	if rec := do(r, http.MethodGet, "/cz/v1/observe/accounts", "Bearer "+testToken); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no source: %d", rec.Code)
	}
}

// A used-up weekly window (or every window used up) is cooling for quota,
// as grok2api's own summary counts it "waiting for reset".
func TestSpentWindowsAreCooling(t *testing.T) {
	svc := New(Options{LabelSecret: "s", Now: func() time.Time { return t0 }})
	mk := func(windows ...accountdomain.QuotaWindow) accountapp.View {
		return accountapp.View{Credential: accountdomain.Credential{ID: 1, Enabled: true, AuthStatus: accountdomain.AuthStatusActive}, QuotaWindows: windows}
	}
	for _, c := range []struct {
		v    accountapp.View
		want string
	}{
		{mk(accountdomain.QuotaWindow{Mode: "weekly", Remaining: 0, Total: 10000, ResetAt: at(48 * time.Hour)}), "cooling"},
		{mk(accountdomain.QuotaWindow{Mode: "weekly", Remaining: 6700, Total: 10000}), "active"},
		{mk(accountdomain.QuotaWindow{Mode: "video", Remaining: 0, Total: 12}, accountdomain.QuotaWindow{Mode: "image_pro", Remaining: 3, Total: 50}), "active"},
		{mk(accountdomain.QuotaWindow{Mode: "video", Remaining: 0, Total: 12}, accountdomain.QuotaWindow{Mode: "image_pro", Remaining: 0, Total: 50}), "cooling"},
	} {
		a := svc.account(c.v, t0)
		if a.State != c.want {
			t.Fatalf("%+v: state %s, want %s", c.v.QuotaWindows, a.State, c.want)
		}
	}
	a := svc.account(mk(accountdomain.QuotaWindow{Mode: "weekly", Remaining: 0, Total: 10000, ResetAt: at(48 * time.Hour)}), t0)
	if a.Reason != "quota" || a.Until == nil || !a.Until.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("weekly spent: %+v", a)
	}
}

func TestMaskAndErrorCode(t *testing.T) {
	for in, want := range map[string]string{"": "…", "ab": "…", "abcdef": "…", "alice@example.com": "al…om", "  zara.grok.01  ": "za…01", "名字名字名字名字": "名字…名字"} {
		if got := MaskIdentity(in); got != want {
			t.Fatalf("MaskIdentity(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"": "", "429 too many": "rate_limited", "token expired": "auth", "HTTP 403 Forbidden": "forbidden",
		"statsig unavailable": "statsig", "dial tcp: i/o timeout": "timeout", "proxy connect failed": "network", "weird": "error", "quota exhausted": "quota"} {
		if got := ErrorCode(in); got != want {
			t.Fatalf("ErrorCode(%q) = %q, want %q", in, got, want)
		}
	}
}
