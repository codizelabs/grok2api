package web

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// czP5bUndecryptable has a token that isn't base64 at all, so any attempt to
// decrypt it fails.
var czP5bUndecryptable = account.Credential{ID: 93, EncryptedAccessToken: "not-a-ciphertext"}

// P5b review fix: in browser mode SignImagine has nothing to sign, so it
// returns before it decrypts a token or takes an egress lease, as manual mode
// does. The url-mode control below proves that a mode which does decrypt
// fails on the same credential.
func TestCZP5bSignImagineBrowserModeReturnsEarly(t *testing.T) {
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	for _, path := range []string{"/rest/app-chat/conversations/new", "/rest/app-chat/conversations/abc123/responses"} {
		if err := h.adapter.SignImagine(context.Background(), czP5bUndecryptable, http.MethodPost, path); err != nil {
			t.Fatalf("SignImagine(browser, %s) = %v, want nil with no token decrypt", path, err)
		}
	}
	h.assertNoGrokCalls(t)
	if seen := h.signer.seen(); len(seen) != 0 {
		t.Fatalf("SignImagine(browser) contacted a signer %q, want 0", seen)
	}
	if n := h.pageFetches.Load(); n != 0 {
		t.Fatalf("SignImagine(browser) loaded grok.com's page %d time(s), want 0", n)
	}
	if err := h.adapter.SignImagine(context.Background(), czP5bUndecryptable, http.MethodGet, "/rest/assets/a"); !errors.Is(err, ErrImagineCallRefused) {
		t.Fatalf("SignImagine(browser, GET) = %v, want refused", err)
	}

	// Control: url mode decrypts the token before it signs.
	control := newCZP5bHarness(t, Config{StatsigMode: "url", StatsigSignerURL: "http://signer.internal/sign"})
	if err := control.adapter.SignImagine(context.Background(), czP5bUndecryptable, http.MethodPost, "/rest/app-chat/conversations/new"); err == nil {
		t.Fatal("SignImagine(url) with an undecryptable token = nil, want an error (the control for the early return)")
	}
}

// An endpoint listed as needing a signature still fails closed in browser
// mode, with the browser-required reason and zero calls.
func TestCZP5bSignImagineBrowserModeRequiredEndpointFailsClosed(t *testing.T) {
	czP5bMarkRequired(t, "POST /rest/app-chat/conversations/{id}/responses")
	h := newCZP5bHarness(t, Config{StatsigMode: "browser"})
	czP5bAssertBrowserRequired(t, h.adapter.SignImagine(context.Background(), czP5bUndecryptable, http.MethodPost, "/rest/app-chat/conversations/abc123/responses"))
	if err := h.adapter.SignImagine(context.Background(), czP5bUndecryptable, http.MethodPost, "/rest/app-chat/conversations/new"); err != nil {
		t.Fatalf("unmarked create: %v, want nil", err)
	}
	h.assertNoGrokCalls(t)
}
