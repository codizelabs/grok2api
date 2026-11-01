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
