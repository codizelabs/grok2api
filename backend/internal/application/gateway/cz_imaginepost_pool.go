package gateway

// CodizeLabs fork patch P3 (ADR-0002 § 1): the account leasing that
// internal/cz/imaginepost needs from the selector, in a new file so upstream
// pulls never conflict. A post's calls lease exactly its pinned account; there
// is no fallback to the pool (sec-rowan B3).

import (
	"context"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// CZImagineLease is one leased web account for a P3 call.
type CZImagineLease struct {
	Credential account.Credential
	lease      *accountLease
}

// Release returns the account's concurrency slot.
func (l *CZImagineLease) Release() {
	if l != nil && l.lease != nil {
		l.lease.Release()
		l.lease = nil
	}
}

// CZAcquireImaginePinned leases exactly accountID. Any refusal (cooling,
// disabled, out of quota, saturated, outside the key's scope) is returned as
// is; another account is never tried.
func (s *Service) CZAcquireImaginePinned(ctx context.Context, accountID uint64, upstreamModel, quotaMode string, scope clientkeydomain.AccountScope) (*CZImagineLease, error) {
	if accountID == 0 {
		return nil, pinnedUnavailableError(0, "")
	}
	route, err := s.models.GetByProviderUpstream(ctx, account.ProviderWeb, upstreamModel)
	if err != nil {
		return nil, err
	}
	lease, err := s.selector.AcquirePinnedForKey(ctx, account.ProviderWeb, accountID, route.ID, route.UpstreamModel, quotaMode, true, scope)
	if err != nil {
		return nil, err
	}
	return s.czFinishImagineLease(ctx, lease, accountID)
}

// CZAcquireImagineNew chooses an account for a new post, inside the key's
// account scope, through the ordinary selector, skipping excluded accounts.
func (s *Service) CZAcquireImagineNew(ctx context.Context, upstreamModel, quotaMode string, scope clientkeydomain.AccountScope, excluded map[uint64]bool) (*CZImagineLease, error) {
	route, err := s.models.GetByProviderUpstream(ctx, account.ProviderWeb, upstreamModel)
	if err != nil {
		return nil, err
	}
	lease, err := s.selector.AcquireForKey(ctx, account.ProviderWeb, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, scope)
	if err != nil {
		return nil, err
	}
	return s.czFinishImagineLease(ctx, lease, 0)
}

func (s *Service) czFinishImagineLease(ctx context.Context, lease *accountLease, wantID uint64) (*CZImagineLease, error) {
	if wantID != 0 && lease.Credential.ID != wantID {
		lease.Release()
		return nil, pinnedUnavailableError(wantID, "")
	}
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		lease.Release()
		return nil, err
	}
	if credential.ID != lease.Credential.ID {
		lease.Release()
		return nil, pinnedUnavailableError(lease.Credential.ID, "")
	}
	return &CZImagineLease{Credential: credential, lease: lease}, nil
}

// CZReportImagineStatus feeds one P3 answer into the account's health: a 401
// marks the SSO session rejected, a 429 cools the account. Nothing else, so a
// P3 refusal never rotates or penalises other accounts.
func (s *Service) CZReportImagineStatus(ctx context.Context, credential account.Credential, status int) {
	switch status {
	case http.StatusUnauthorized:
		s.markSSOCredentialRejected(ctx, credential, "web SSO credential rejected")
	case http.StatusTooManyRequests:
		s.selector.MarkFailure(ctx, credential, status, 0)
	}
}
