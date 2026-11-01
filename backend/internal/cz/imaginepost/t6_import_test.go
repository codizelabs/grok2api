package imaginepost

// T6 (WP08a): POST /cz/v1/imagine/posts/import pins an existing grok.com
// post to an account only after one free GET .../responses of it succeeds on
// that account. It tries the key's eligible accounts in a fixed order, stops
// at the first success, makes at most MaxImportReads reads, never sends or
// uploads, and is idempotent.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

const importConv = "conv-import-0001"

func (h *harness) ownerRow(conversationID string) (PostOwner, bool) {
	h.t.Helper()
	owner, err := h.service.Store().Owner(h.ctx, conversationID)
	if errors.Is(err, errNotFound) {
		return PostOwner{}, false
	}
	if err != nil {
		h.t.Fatalf("owner: %v", err)
	}
	return owner, true
}

func (h *harness) expectNoSendsOrUploads() {
	h.t.Helper()
	if sends := h.fake.Sends(); sends != 0 {
		h.t.Fatalf("%d sends, want 0", sends)
	}
	for _, call := range h.transport.Calls() {
		if call == "UPLOAD" {
			h.t.Fatal("an import uploaded a file")
		}
	}
	for _, call := range h.fake.Calls() {
		if call.Method != http.MethodGet {
			h.t.Fatalf("an import made %s %s; only free GETs are allowed", call.Method, call.Path)
		}
	}
}

func TestImportPinsToTheFirstAccountWhoseFreeReadSucceeds(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	result, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	labelB := h.service.labels.AccountLabel(accountB)
	if result.PostID != importConv || result.AccountLabel != labelB || !result.Imported || result.ItemCount != 2 || result.HeadResponseID == "" {
		t.Fatalf("result %+v", result)
	}
	if reads := h.listReads(importConv); reads != 2 {
		t.Fatalf("%d reads, want 2 (A answers 404, B succeeds)", reads)
	}
	owner, ok := h.ownerRow(importConv)
	if !ok || owner.AccountID != accountB || owner.ClientKeyID != keyID {
		t.Fatalf("owner %+v (found %v), want account B for key %d", owner, ok, keyID)
	}
	h.expectNoSendsOrUploads()
	// The imported post now works like any other, on its account only.
	if _, err := h.service.ListItems(h.ctx, h.caller, importConv, labelB); err != nil {
		t.Fatalf("list after import: %v", err)
	}
}

func TestImportWithAccountLabelReadsOnlyThatAccount(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	result, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv, AccountLabel: h.service.labels.AccountLabel(accountB)})
	if err != nil || result.AccountLabel != h.service.labels.AccountLabel(accountB) {
		t.Fatalf("import: %+v %v", result, err)
	}
	if n := h.fake.CountByAccount(ssoA); n != 0 {
		t.Fatalf("account A got %d calls, want 0", n)
	}
	if reads := h.listReads(importConv); reads != 1 {
		t.Fatalf("%d reads, want 1", reads)
	}
}

func TestImportWithLabelOfTheWrongAccountPinsNothing(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv, AccountLabel: h.service.labels.AccountLabel(accountA)})
	if errCode(err) != CodePostNotFound || errSend(err) != NotSent {
		t.Fatalf("err %v, want post_not_found", err)
	}
	if n := h.fake.CountByAccount(ssoB); n != 0 {
		t.Fatalf("account B got %d calls, want 0", n)
	}
	if _, ok := h.ownerRow(importConv); ok {
		t.Fatal("a pin was written")
	}
}

func TestImportWithUnknownLabelMakesNoCall(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv, AccountLabel: "acct_0000000000000000"})
	if errCode(err) != CodePinnedAccountUnavailable || errSend(err) != NotSent {
		t.Fatalf("err %v, want upstream_pinned_account_unavailable", err)
	}
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("%d calls, want 0", len(calls))
	}
}

func TestImportIsIdempotent(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	first, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if err != nil {
		t.Fatal(err)
	}
	callsBefore := len(h.fake.Calls())
	second, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Imported || second.PostID != first.PostID || second.AccountLabel != first.AccountLabel {
		t.Fatalf("second %+v, first %+v", second, first)
	}
	if n := len(h.fake.Calls()); n != callsBefore {
		t.Fatalf("second import made %d calls, want 0", n-callsBefore)
	}
	// The same label is accepted; another one is a mismatch.
	if _, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv, AccountLabel: first.AccountLabel}); err != nil {
		t.Fatalf("import with its label: %v", err)
	}
	_, err = h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv, AccountLabel: h.service.labels.AccountLabel(accountA)})
	if errCode(err) != CodeAccountLabelMismatch {
		t.Fatalf("err %v, want account_label_mismatch", err)
	}
}

func TestImportOfAPostAnotherKeyPinnedIsNotFound(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-other-key")
	callsBefore := len(h.fake.Calls())
	_, err := h.service.ImportPost(h.ctx, Caller{ClientKeyID: keyID + 1}, ImportInput{ConversationID: post.PostID})
	if errCode(err) != CodePostNotFound {
		t.Fatalf("err %v, want post_not_found", err)
	}
	if n := len(h.fake.Calls()); n != callsBefore {
		t.Fatalf("%d calls, want 0", n-callsBefore)
	}
	owner, _ := h.ownerRow(post.PostID)
	if owner.ClientKeyID != keyID {
		t.Fatalf("pin changed hands: %+v", owner)
	}
}

func TestImportOfAMissingPostIsNotFoundAndPinsNothing(t *testing.T) {
	h := newDefaultHarness(t)
	_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if errCode(err) != CodePostNotFound || errSend(err) != NotSent {
		t.Fatalf("err %v, want post_not_found", err)
	}
	if reads := h.listReads(importConv); reads != 2 {
		t.Fatalf("%d reads, want one per account (2)", reads)
	}
	if _, ok := h.ownerRow(importConv); ok {
		t.Fatal("a pin was written")
	}
	h.expectNoSendsOrUploads()
}

func TestImportStopsAfterMaxImportReads(t *testing.T) {
	h := newHarness(t, validStatsig, Config{MaxImportReads: 2})
	h.addAccountC()
	h.fake.SeedPost(ssoC, importConv)
	_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if errCode(err) != CodePostNotFound {
		t.Fatalf("err %v, want post_not_found after 2 reads", err)
	}
	if reads := h.listReads(importConv); reads != 2 {
		t.Fatalf("%d reads, want 2", reads)
	}
	if n := h.fake.CountByAccount(ssoC); n != 0 {
		t.Fatalf("account C got %d calls past the cap", n)
	}
}

func TestImportSkipsAccountsThePoolRefuses(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	h.pool.setUnavailable(accountA, refusedErr{status: http.StatusTooManyRequests})
	result, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if err != nil || result.AccountLabel != h.service.labels.AccountLabel(accountB) {
		t.Fatalf("import: %+v %v", result, err)
	}
	if n := h.fake.CountByAccount(ssoA); n != 0 {
		t.Fatalf("refused account A got %d calls", n)
	}
}

func TestImportReachesPinnedOnlyAccounts(t *testing.T) {
	h := newHarness(t, validStatsig, Config{PinnedOnlyAccountIDs: []uint64{accountB}})
	h.fake.SeedPost(ssoB, importConv)
	result, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if err != nil || result.AccountLabel != h.service.labels.AccountLabel(accountB) {
		t.Fatalf("import: %+v %v", result, err)
	}
}

func TestImportRefusesBadIDsWithoutACall(t *testing.T) {
	h := newDefaultHarness(t)
	for _, id := range []string{"", "../etc", "conv 1", "conv-1/responses"} {
		_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: id})
		if errCode(err) != CodePostNotFound {
			t.Errorf("%q: err %v, want post_not_found", id, err)
		}
	}
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("%d calls, want 0", len(calls))
	}
}

func TestImportFailsClosedWithoutStatsig(t *testing.T) {
	h := newHarness(t, "invalid", Config{})
	h.fake.SeedPost(ssoB, importConv)
	_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if errCode(err) != CodeStatsigUnavailable || errSend(err) != NotSent {
		t.Fatalf("err %v, want statsig_unavailable", err)
	}
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("%d calls, want 0", len(calls))
	}
	if _, ok := h.ownerRow(importConv); ok {
		t.Fatal("a pin was written")
	}
}

func TestImportLogsNoConversationID(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	if _, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv}); err != nil {
		t.Fatal(err)
	}
	if logs := h.logs.String(); logs == "" || strings.Contains(logs, importConv) {
		t.Fatalf("logs empty or carry the conversation id:\n%s", logs)
	}
}

func TestImportAnswersARetryableFailureNotNotFound(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SeedPost(ssoB, importConv)
	h.transport.mu.Lock()
	h.transport.failListReadsFor = accountB
	h.transport.failListReadsErr = &web.ImagineHTTPError{Status: http.StatusTooManyRequests}
	h.transport.mu.Unlock()
	_, err := h.service.ImportPost(h.ctx, h.caller, ImportInput{ConversationID: importConv})
	if errCode(err) != CodeRateLimited || errSend(err) != NotSent {
		t.Fatalf("err %v, want rate_limited: the post may be on account B", err)
	}
	if _, ok := h.ownerRow(importConv); ok {
		t.Fatal("a pin was written")
	}
}
