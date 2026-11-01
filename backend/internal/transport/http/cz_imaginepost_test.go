package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// P3's routes exist under /cz/v1/imagine and sit behind client-key auth and
// the readiness gate; without a service they never reach a handler.
func TestCZImaginePostRoutesAreGuarded(t *testing.T) {
	deps := testDependencies()
	router := New(deps)
	for _, path := range []string{"/cz/v1/imagine/posts", "/cz/v1/imagine/posts/conv-1/items"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a key: status %d, want 401", path, recorder.Code)
		}
	}
	deps.TrafficReady = func() bool { return false }
	router = New(deps)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/cz/v1/imagine/posts", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("before startup recovery: status %d, want 503", recorder.Code)
	}
}
