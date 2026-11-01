package imaginepost

// Import (WP08a-T6): pin a post that already exists on grok.com, made outside
// P3, to the account it lives on. The pin is written only after one free
// GET .../responses of the post succeeds on that account, so a post is never
// pinned to an account that can't read it. Nothing is sent or uploaded.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
)

// defaultMaxImportReads bounds the free reads one import makes.
const defaultMaxImportReads = 4

// ImportInput names an existing grok.com post. AccountLabel, when given,
// limits the import to that one account.
type ImportInput struct {
	ConversationID string
	AccountLabel   string
}

// ImportResult is a post's pin after an import. Imported is false when the
// post was already pinned for this key (nothing was read).
type ImportResult struct {
	PostID         string `json:"post_id"`
	AccountLabel   string `json:"account_label"`
	Imported       bool   `json:"imported"`
	HeadResponseID string `json:"head_response_id,omitempty"`
	ItemCount      int    `json:"item_count"`
}

// ImportPost pins an existing post for the caller's key. An existing pin is
// returned unchanged (idempotent). Otherwise the key's eligible accounts,
// pinned-only ones included, are tried in account-id order, one free read
// each, at most MaxImportReads reads; the first account whose read succeeds
// gets the pin. A 404 on every account read is post_not_found.
func (s *Service) ImportPost(ctx context.Context, caller Caller, input ImportInput) (*ImportResult, error) {
	conversationID := input.ConversationID
	label := strings.TrimSpace(input.AccountLabel)
	if !validID.MatchString(conversationID) {
		return nil, s.logged("import", "", 0, notSent(CodePostNotFound, http.StatusNotFound))
	}
	if existing, done, refusal := s.existingPin(ctx, caller, conversationID, label); done {
		if refusal != nil {
			return nil, s.logged("import", conversationID, 0, refusal)
		}
		return existing, nil
	}

	accounts, refusal := s.importAccounts(ctx, caller, label)
	if refusal != nil {
		return nil, s.logged("import", conversationID, 0, refusal)
	}
	reads, lastFailure := 0, (*Error)(nil)
	for _, accountID := range accounts {
		if reads >= s.cfg().MaxImportReads {
			break
		}
		lease, acquireErr := s.pool.AcquirePinned(ctx, accountID, PurposeRead, caller.Scope)
		if acquireErr != nil {
			if lastFailure == nil {
				lastFailure = mapAcquireErr(acquireErr, CodePinnedAccountUnavailable)
			}
			continue
		}
		credential := lease.Credential()
		if credential.ID != accountID {
			// The pool broke its contract: never pin to another account.
			lease.Release()
			continue
		}
		reads++
		snapshot, readErr := s.readPost(ctx, credential, conversationID)
		lease.Release()
		if readErr == nil {
			s.record(ctx, "import", conversationID, credential, http.MethodGet, listResponsesPath(conversationID), nil)
			return s.pinImported(ctx, caller, conversationID, accountID, snapshot)
		}
		switch {
		case readErr.Code == CodeStatsigUnavailable || readErr.Code == CodeCallNotAllowed:
			return nil, s.logged("import", conversationID, accountID, readErr)
		case readErr.Code == CodePostNotFound || readErr.UpstreamStatus == http.StatusForbidden:
			// Not on this account (grok.com answers another account's
			// conversation with 404 or 403, provisional): try the next.
		default:
			// The post may be on this account: a later retry can find it.
			lastFailure = readErr
		}
	}
	// Every read answered 404 (a pool refusal on another account doesn't
	// change that): the post isn't on any account this key can reach.
	onlyNotFound := lastFailure == nil || lastFailure.Code == CodePinnedAccountUnavailable
	if reads != 0 && onlyNotFound {
		return nil, s.logged("import", conversationID, 0, notSent(CodePostNotFound, http.StatusNotFound))
	}
	if lastFailure != nil {
		return nil, s.logged("import", conversationID, 0, lastFailure)
	}
	return nil, s.logged("import", conversationID, 0, notSent(CodeNoAccount, http.StatusServiceUnavailable))
}

// existingPin answers an import of a post that is already pinned: done is
// true when the pin exists (or the store failed) and the import must not read.
func (s *Service) existingPin(ctx context.Context, caller Caller, conversationID, label string) (*ImportResult, bool, *Error) {
	owner, err := s.store.Owner(ctx, conversationID)
	if errors.Is(err, errNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, notSent(CodeInternal, http.StatusInternalServerError)
	}
	if owner.ClientKeyID != caller.ClientKeyID {
		// Another key owns the post: answer as if it were missing.
		return nil, true, notSent(CodePostNotFound, http.StatusNotFound)
	}
	accountLabel := s.labels.AccountLabel(owner.AccountID)
	if label != "" && label != accountLabel {
		return nil, true, notSent(CodeAccountLabelMismatch, http.StatusConflict)
	}
	return &ImportResult{PostID: conversationID, AccountLabel: accountLabel}, true, nil
}

// importAccounts lists the accounts an import may read, in account-id order:
// every eligible account in the key scope (pinned-only ones too, since they
// serve the posts that live on them), or only the labelled one.
func (s *Service) importAccounts(ctx context.Context, caller Caller, label string) ([]uint64, *Error) {
	candidates, err := s.pool.NewPostCandidates(ctx, caller.Scope)
	if err != nil {
		return nil, mapAcquireErr(err, CodeNoAccount)
	}
	seen := map[uint64]bool{}
	accounts := make([]uint64, 0, len(candidates))
	for _, candidate := range candidates {
		id := candidate.AccountID
		if id == 0 || seen[id] || (label != "" && s.labels.AccountLabel(id) != label) {
			continue
		}
		seen[id] = true
		accounts = append(accounts, id)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i] < accounts[j] })
	if label != "" && len(accounts) == 0 {
		return nil, notSent(CodePinnedAccountUnavailable, http.StatusServiceUnavailable)
	}
	return accounts, nil
}

// pinImported writes the pin once. If another request pinned the post first,
// its pin stands: the same key gets it back, another key gets post_not_found.
func (s *Service) pinImported(ctx context.Context, caller Caller, conversationID string, accountID uint64, snapshot PostSnapshot) (*ImportResult, error) {
	if err := s.store.PutOwner(context.WithoutCancel(ctx), PostOwner{ConversationID: conversationID, AccountID: accountID, ClientKeyID: caller.ClientKeyID}); err != nil {
		return nil, s.logged("import", conversationID, accountID, notSent(CodeInternal, http.StatusInternalServerError))
	}
	owner, err := s.store.Owner(context.WithoutCancel(ctx), conversationID)
	if err != nil {
		return nil, s.logged("import", conversationID, accountID, notSent(CodeInternal, http.StatusInternalServerError))
	}
	if owner.ClientKeyID != caller.ClientKeyID {
		return nil, s.logged("import", conversationID, accountID, notSent(CodePostNotFound, http.StatusNotFound))
	}
	result := &ImportResult{PostID: conversationID, AccountLabel: s.labels.AccountLabel(owner.AccountID)}
	if owner.AccountID == accountID {
		result.Imported = true
		result.HeadResponseID = snapshot.HeadResponseID
		result.ItemCount = len(snapshot.Items)
	}
	s.logOK("import", conversationID, owner.AccountID)
	return result, nil
}
