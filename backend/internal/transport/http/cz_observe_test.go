package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P6's routes exist under /cz/v1/observe on grok2api's own router (no new
// listener). This test uses only what the base already has, so on the base
// (no P6) it fails: the paths fall through to the frontend, not 503 or 405.
func TestCZObserveRoutesAreMountedReadOnlyAndUnconfiguredByDefault(t *testing.T) {
	router := New(testDependencies())
	for _, path := range []string{"/cz/v1/observe/accounts", "/cz/v1/observe/usage", "/cz/v1/observe/requests", "/cz/v1/observe/proxies", "/cz/v1/observe/summary", "/cz/v1/observe/overview"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "observe_not_configured") {
			t.Fatalf("GET %s without a token configured: status %d %s, want 503 observe_not_configured", path, recorder.Code, recorder.Body.String())
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader("{}")))
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: status %d, want 405", method, path, recorder.Code)
			}
		}
	}
}

// Security review 2026-11-05 G1: an unknown path under /cz is a backend 404, never the web app's index.html.
func TestCZUnknownPathIsBackend404(t *testing.T) {
	for _, p := range []string{"/cz", "/cz/v1/observe/nope", "/cz/v1/observe/accounts/../x"} {
		if !isBackendPath(p) {
			t.Fatalf("%s should be a backend path (404, not the web app)", p)
		}
	}
	if isBackendPath("/czar") {
		t.Fatal("/czar is not under /cz")
	}
}
