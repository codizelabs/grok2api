package gateway

// CodizeLabs fork patch P3 (ADR-0002 § 1): the account leasing that
// internal/cz/imaginepost needs from the selector, in a new file so upstream
// pulls never conflict. A post's calls lease exactly its pinned account; there
// is no fallback to the pool (sec-rowan B3).

import (
	"context"
	"net/http"
	"sort"
	"time"

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

// CZImagineRoute is one model route a new post's account must serve.
type CZImagineRoute struct {
	UpstreamModel string
	QuotaMode     string
}

// CZImagineCandidate is one account a new post may be pinned to, with the
// quota grok2api already holds for it: one Remaining per route, -1 unknown.
type CZImagineCandidate struct {
	AccountID uint64
	Remaining []int
}

// CZImagineNewCandidates lists the web accounts a new post may be pinned to
// (ADR-0002 § 1): enabled, signed in, inside the key's scope, serving every
// route's model, not cooling and not out of quota on any route, sorted by
// account id. It reads only grok2api's own state: nothing is leased and
// grok.com is never called. The caller ranks them and leases one with
// CZAcquireImaginePinned, which checks the account again.
func (s *Service) CZImagineNewCandidates(ctx context.Context, scope clientkeydomain.AccountScope, routes ...CZImagineRoute) ([]CZImagineCandidate, error) {
	if len(routes) == 0 {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts}
	}
	accountScope, scopeValid := clientkeydomain.NormalizeAccountScope(scope)
	if !scopeValid || !accountScope.AllowsProvider(account.ProviderWeb) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	now := time.Now().UTC()
	consumed := s.selector.quotaConsumptionSnapshot(account.ProviderWeb)
	overrides := s.selector.routingHealthSnapshot(account.ProviderWeb, now)
	eligible := map[uint64][]int{}
	for index, route := range routes {
		resolved, err := s.models.GetByProviderUpstream(ctx, account.ProviderWeb, route.UpstreamModel)
		if err != nil {
			return nil, err
		}
		values, err := s.selector.loadCandidates(ctx, account.ProviderWeb, resolved.ID, resolved.UpstreamModel, route.QuotaMode, now, false)
		if err != nil {
			return nil, err
		}
		seen := map[uint64]bool{}
		for _, candidate := range values {
			value := applyHealthSnapshot(candidate.Credential, overrides)
			if !czImagineCandidateUsable(s.selector, route, accountScope, candidate, value, consumed, now) {
				continue
			}
			if index == 0 {
				eligible[value.ID] = make([]int, len(routes))
			} else if _, ok := eligible[value.ID]; !ok {
				continue
			}
			remaining := -1
			if window := candidate.QuotaWindow; window != nil {
				remaining = window.Remaining - consumed[accountQuotaConsumptionKey{accountID: value.ID, mode: window.Mode}]
			}
			eligible[value.ID][index] = remaining
			seen[value.ID] = true
		}
		for id := range eligible {
			if !seen[id] {
				delete(eligible, id)
			}
		}
	}
	result := make([]CZImagineCandidate, 0, len(eligible))
	for id, remaining := range eligible {
		result = append(result, CZImagineCandidate{AccountID: id, Remaining: remaining})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].AccountID < result[right].AccountID })
	return result, nil
}

// czImagineCandidateUsable applies the same account checks as an inference
// AcquirePinned, without leasing.
func czImagineCandidateUsable(selector *Selector, route CZImagineRoute, scope clientkeydomain.AccountScope, candidate account.RoutingCandidate, value account.Credential, consumed map[accountQuotaConsumptionKey]int, now time.Time) bool {
	switch {
	case value.ID == 0 || !value.Enabled || value.AuthStatus != account.AuthStatusActive:
		return false
	case !accountScopeAllowsCandidate(account.ProviderWeb, scope, candidate):
		return false
	case !selector.candidateSupportsModel(account.ProviderWeb, route.UpstreamModel, route.QuotaMode, candidate):
		return false
	case candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil):
		return false
	case candidateEgressLeaseCooling(candidate, value, now):
		return false
	case value.CooldownUntil != nil && now.Before(*value.CooldownUntil):
		return false
	case candidate.QuotaRecovery != nil && candidate.QuotaRecovery.Status != account.QuotaRecoveryStatusActive:
		return false
	case candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining):
		return false
	case quotaWindowExhausted(candidate, consumed):
		return false
	}
	return true
}
