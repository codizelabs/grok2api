package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// F3 (pool half): a P3 call leases only the pinned account; cooling or
// disabled is an error, and the healthy second account is never leased.
func TestCZImaginePinnedLeaseNeverFallsBack(t *testing.T) {
	h := newCZPinHarness(t)
	scope := h.key.AccountScope()

	lease, err := h.service.CZAcquireImaginePinned(h.ctx, h.pinned.ID, "grok-imagine-video", "", scope)
	if err != nil || lease.Credential.ID != h.pinned.ID {
		t.Fatalf("available pinned account: lease=%v err=%v, want the pinned account", lease, err)
	}
	lease.Release()

	h.selector.MarkFailure(h.ctx, h.pinned, 429, time.Hour)
	lease, err = h.service.CZAcquireImaginePinned(h.ctx, h.pinned.ID, "grok-imagine-video", "", scope)
	var unavailable *SelectionUnavailableError
	if lease != nil || !errors.As(err, &unavailable) {
		t.Fatalf("cooling pinned account: lease=%v err=%v, want a SelectionUnavailableError and no lease", lease, err)
	}

	h2 := newCZPinHarness(t)
	disabled := false
	if _, err := h2.accountRepo.UpdateMany(h.ctx, account.ProviderWeb, []uint64{h2.pinned.ID}, repository.AccountUpdates{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	lease, err = h2.service.CZAcquireImaginePinned(h2.ctx, h2.pinned.ID, "grok-imagine-video", "", scope)
	if lease != nil || !errors.As(err, &unavailable) || unavailable.Reason != SelectionPinnedUnavailable {
		t.Fatalf("disabled pinned account: lease=%v err=%v, want pinned unavailable", lease, err)
	}
	if lease, err := h.service.CZAcquireImaginePinned(h.ctx, 0, "grok-imagine-video", "", scope); lease != nil || err == nil {
		t.Fatalf("account 0: lease=%v err=%v, want refused", lease, err)
	}
	// The healthy account was never touched: its slot (MaxConcurrent 1) is free.
	other, err := h.service.CZAcquireImaginePinned(h.ctx, h.healthy.ID, "grok-imagine-video", "", scope)
	if err != nil || other.Credential.ID != h.healthy.ID {
		t.Fatalf("healthy account lease=%v err=%v", other, err)
	}
	other.Release()
}

func TestCZImagineNewLeaseStaysInScope(t *testing.T) {
	h := newCZPinHarness(t)
	lease, err := h.service.CZAcquireImagineNew(h.ctx, "grok-imagine-video", "", h.key.AccountScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Credential.ID != h.pinned.ID && lease.Credential.ID != h.healthy.ID {
		t.Fatalf("new lease on unknown account %d", lease.Credential.ID)
	}
}

// ADR-0002 § 1 (pool half): a new post's candidates are every usable account
// in scope, sorted by id, and a cooling account is left out. Nothing is leased.
func TestCZImagineNewCandidatesSkipCooling(t *testing.T) {
	h := newCZPinHarness(t)
	scope := h.key.AccountScope()
	route := CZImagineRoute{UpstreamModel: "grok-imagine-video"}
	values, err := h.service.CZImagineNewCandidates(h.ctx, scope, route)
	if err != nil || len(values) != 2 || values[0].AccountID >= values[1].AccountID || len(values[0].Remaining) != 1 {
		t.Fatalf("candidates = %+v err = %v, want both accounts sorted by id, one remaining per route", values, err)
	}
	h.selector.MarkFailure(h.ctx, h.pinned, 429, time.Hour)
	values, err = h.service.CZImagineNewCandidates(h.ctx, scope, route)
	if err != nil || len(values) != 1 || values[0].AccountID != h.healthy.ID {
		t.Fatalf("with the pinned account cooling: candidates = %+v err = %v, want only the healthy one", values, err)
	}
	// Listing leased nothing: the healthy account's single slot is free.
	lease, err := h.service.CZAcquireImaginePinned(h.ctx, h.healthy.ID, "grok-imagine-video", "", scope)
	if err != nil {
		t.Fatalf("healthy account after listing: %v", err)
	}
	lease.Release()
	if _, err := h.service.CZImagineNewCandidates(h.ctx, scope); err == nil {
		t.Fatal("no routes: want an error")
	}
}
