package imaginepost

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

// seedPins pins count posts to accountID, so the ranking sees them open.
func (h *harness) seedPins(accountID uint64, count int) {
	h.t.Helper()
	for index := 0; index < count; index++ {
		owner := PostOwner{ConversationID: fmt.Sprintf("conv-seed-%d-%d", accountID, index), AccountID: accountID, ClientKeyID: keyID}
		if err := h.service.Store().PutOwner(h.ctx, owner); err != nil {
			h.t.Fatalf("seed pin: %v", err)
		}
	}
}

// createdOn makes a post and returns the account it was pinned to.
func (h *harness) createdOn(jobID string) uint64 {
	h.t.Helper()
	result := h.createPost(jobID)
	owner, err := h.service.Store().Owner(h.ctx, result.PostID)
	if err != nil {
		h.t.Fatalf("owner of %s: %v", jobID, err)
	}
	return owner.AccountID
}

// ADR-0002 § 1: a new post goes to the account with the fewest open pinned
// posts, then the most remaining quota, then the lowest account id, whatever
// order the pool lists them in.
func TestNewPostRanking(t *testing.T) {
	cases := []struct {
		name       string
		pins       map[uint64]int
		candidates []Candidate
		want       uint64
	}{
		{
			name: "fewest open posts beats more quota",
			pins: map[uint64]int{accountA: 2, accountB: 1, accountC: 0},
			candidates: []Candidate{
				{AccountID: accountA, ImageEditRemaining: 90, Video720pRemaining: 90},
				{AccountID: accountB, ImageEditRemaining: 90, Video720pRemaining: 90},
				{AccountID: accountC, ImageEditRemaining: 1, Video720pRemaining: 1},
			},
			want: accountC,
		},
		{
			name: "equal posts: most remaining quota",
			pins: map[uint64]int{accountA: 1, accountB: 1, accountC: 1},
			candidates: []Candidate{
				{AccountID: accountA, ImageEditRemaining: 10, Video720pRemaining: 10},
				{AccountID: accountB, ImageEditRemaining: 50, Video720pRemaining: 10},
				{AccountID: accountC, ImageEditRemaining: 20, Video720pRemaining: 10},
			},
			want: accountB,
		},
		{
			name: "equal posts and quota: lowest account id",
			pins: map[uint64]int{},
			candidates: []Candidate{
				{AccountID: accountC, ImageEditRemaining: 5, Video720pRemaining: 5},
				{AccountID: accountB, ImageEditRemaining: 5, Video720pRemaining: 5},
				{AccountID: accountA, ImageEditRemaining: 5, Video720pRemaining: 5},
			},
			want: accountA,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDefaultHarness(t)
			h.addAccountC()
			for id, count := range tc.pins {
				h.seedPins(id, count)
			}
			h.pool.setCandidates(tc.candidates...)
			if got := h.createdOn("job-rank"); got != tc.want {
				t.Fatalf("new post pinned to %d, want %d", got, tc.want)
			}
		})
	}
}

// A busy account (a send in flight) and a cooling one are skipped for the
// next candidate; with every account busy the create is account_busy and
// nothing is sent.
func TestNewPostSkipsBusyAndCoolingAccounts(t *testing.T) {
	h := newDefaultHarness(t)
	h.addAccountC()
	release, busyErr := h.service.claimAccount(accountA)
	if busyErr != nil {
		t.Fatal(busyErr)
	}
	defer release()
	h.pool.setUnavailable(accountB, refusedErr{status: http.StatusTooManyRequests})
	if got := h.createdOn("job-skip"); got != accountC {
		t.Fatalf("new post pinned to %d, want account C (A busy, B cooling)", got)
	}
	if n := h.fake.CountByAccount(ssoA) + h.fake.CountByAccount(ssoB); n != 0 {
		t.Fatalf("the busy and cooling accounts made %d grok.com calls, want 0", n)
	}

	all := newDefaultHarness(t)
	all.addAccountC()
	for _, id := range []uint64{accountA, accountB, accountC} {
		releaseOne, err := all.service.claimAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseOne()
	}
	_, err := all.service.CreatePost(all.ctx, all.caller, CreateInput{JobID: "job-all-busy", Prompt: "p", Image: pngImage()})
	if errCode(err) != CodeAccountBusy || errSend(err) != NotSent || len(all.fake.Calls()) != 0 {
		t.Fatalf("every account busy: err = %v, calls = %d", err, len(all.fake.Calls()))
	}
}

// With no eligible account (pinned-only, cooling, none listed) the create is
// the documented no_account capacity error, and grok.com sees nothing.
func TestNewPostNoEligibleAccountIsCapacityError(t *testing.T) {
	h := newHarness(t, validStatsig, Config{PinnedOnlyAccountIDs: []uint64{accountA}})
	h.addAccountC()
	h.pool.setUnavailable(accountB, refusedErr{status: http.StatusServiceUnavailable})
	h.pool.setUnavailable(accountC, refusedErr{status: http.StatusServiceUnavailable})
	_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-none", Prompt: "p", Image: pngImage()})
	var failure *Error
	if errCode(err) != CodeNoAccount || errSend(err) != NotSent || !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("no eligible account: %v", err)
	}
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("%d grok.com calls with no eligible account, want 0", len(calls))
	}

	empty := newDefaultHarness(t)
	empty.pool.setCandidates()
	_, err = empty.service.CreatePost(empty.ctx, empty.caller, CreateInput{JobID: "job-empty", Prompt: "p", Image: pngImage()})
	if errCode(err) != CodeNoAccount || len(empty.fake.Calls()) != 0 {
		t.Fatalf("no candidates: err = %v, calls = %d", err, len(empty.fake.Calls()))
	}
}

// A pinned-only account serves the posts already pinned to it and is never
// chosen for a new post, even when it ranks first.
func TestPinnedOnlyAccountServesOnlyItsPosts(t *testing.T) {
	h := newHarness(t, validStatsig, Config{PinnedOnlyAccountIDs: []uint64{accountA}})
	const conv = "conv-pinned-only-0001"
	_, resultAsset, label := secSeedOwnedPost(h, conv)
	if _, err := h.service.EditInPost(h.ctx, h.caller, conv, label, EditInput{JobID: "job-po-edit", Prompt: "edit", SourceAssetID: resultAsset}); err != nil {
		t.Fatalf("edit on the pinned-only account's own post: %v", err)
	}
	h.seedPins(accountB, 3) // A would rank first on open posts
	if got := h.createdOn("job-po-create"); got != accountB {
		t.Fatalf("new post pinned to %d, want account B (A is pinned-only)", got)
	}
	if n := h.fake.Count(http.MethodPost, `^/rest/app-chat/conversations/new$`); n != 1 {
		t.Fatalf("creates = %d, want 1", n)
	}
	for _, call := range h.fake.Calls() {
		if call.Method == http.MethodPost && call.Path == pathQuota && call.Account == ssoA {
			t.Fatal("the pinned-only account was considered for the new post (quota read)")
		}
	}
}

// url Statsig mode with a failing signer: no reference upload goes out, for
// a create or an in-post edit with an extra reference.
func TestURLModeFailingSignerUploadsNothing(t *testing.T) {
	signer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "signer down", http.StatusInternalServerError)
	}))
	defer signer.Close()
	h := newHarnessWithWeb(t, web.Config{StatsigMode: "url", StatsigSignerURL: signer.URL + "/sign"}, Config{})
	const conv = "conv-url-signer-0001"
	_, resultAsset, label := secSeedOwnedPost(h, conv)

	_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-url-create", Prompt: "p", Image: pngImage()})
	secExpect(t, "CreatePost", err, CodeStatsigUnavailable, NotSent)
	_, err = h.service.EditInPost(h.ctx, h.caller, conv, label, EditInput{JobID: "job-url-edit", Prompt: "p", SourceAssetID: resultAsset, Extra: []Image{*pngImage()}})
	secExpect(t, "EditInPost", err, CodeStatsigUnavailable, NotSent)

	if n := h.fake.Count(http.MethodPost, `^/http/upload`); n != 0 {
		t.Fatalf("fake grok.com saw %d uploads without a signature, want 0", n)
	}
	for _, call := range h.transport.Calls() {
		if call == "UPLOAD" {
			t.Fatal("an upload reached the transport without a signature")
		}
	}
	if h.fake.Sends() != 0 {
		t.Fatalf("sends = %d, want 0", h.fake.Sends())
	}
}

// The send's signature comes before its reference upload: with the free
// reads signed but the send's signature failing, nothing is uploaded or sent.
func TestSendSignatureComesBeforeAnyUpload(t *testing.T) {
	h := newDefaultHarness(t)
	const conv = "conv-sign-first-0001"
	_, resultAsset, label := secSeedOwnedPost(h, conv)
	h.transport.setSignErr(fmt.Errorf("%w: signer failed", provider.ErrStatsigUnavailable))

	_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-sign-create", Prompt: "p", Image: pngImage()})
	secExpect(t, "CreatePost", err, CodeStatsigUnavailable, NotSent)
	_, err = h.service.EditInPost(h.ctx, h.caller, conv, label, EditInput{JobID: "job-sign-edit", Prompt: "p", SourceAssetID: resultAsset, Extra: []Image{*pngImage()}})
	secExpect(t, "EditInPost", err, CodeStatsigUnavailable, NotSent)

	if n := h.fake.Count(http.MethodPost, `^/http/upload`); n != 0 {
		t.Fatalf("%d uploads went out before the send's signature, want 0", n)
	}
	if h.fake.Sends() != 0 {
		t.Fatalf("sends = %d, want 0", h.fake.Sends())
	}
	if signs := h.transport.Signs(); len(signs) != 2 || signs[0] != "POST "+pathCreate || signs[1] != "POST "+addResponsePath(conv) {
		t.Fatalf("signs = %v, want the create then the add-response send", signs)
	}
}

// The account_mismatch tripwire works even when the read right after create
// failed: the first successful read of the post's item fills the creator.
func TestOwnerHashFilledOnFirstSuccessfulRead(t *testing.T) {
	t.Run("by a read", func(t *testing.T) {
		h := newDefaultHarness(t)
		h.fake.SetUserID(ssoA, "user-a")
		h.transport.failNextAssetReadWith(errors.New("read dropped"))
		post := h.createPost("job-fill-create")
		if owner, _ := h.service.Store().Owner(h.ctx, post.PostID); owner.OwnerUserHash != "" {
			t.Fatal("the failed read still stored an owner hash; the test needs it empty")
		}
		if _, err := h.service.ReadItem(h.ctx, h.caller, post.PostID, post.AccountLabel, post.AssetIDs[0]); err != nil {
			t.Fatalf("read item: %v", err)
		}
		if owner, _ := h.service.Store().Owner(h.ctx, post.PostID); owner.OwnerUserHash == "" {
			t.Fatal("owner hash still empty after a successful read")
		}
		h.fake.SetUserID(ssoA, "user-other")
		_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-fill-edit", Prompt: "x", SourceAssetID: post.AssetIDs[0]})
		if errCode(err) != CodeAccountMismatch || errSend(err) != Sent {
			t.Fatalf("edit after rotation: %v, want account_mismatch", err)
		}
	})
	t.Run("by the next send", func(t *testing.T) {
		h := newDefaultHarness(t)
		h.fake.SetUserID(ssoA, "user-a")
		h.transport.failNextAssetReadWith(errors.New("read dropped"))
		post := h.createPost("job-fill-create")
		if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-fill-edit", Prompt: "x", SourceAssetID: post.AssetIDs[0]}); err != nil {
			t.Fatalf("edit: %v", err)
		}
		owner, _ := h.service.Store().Owner(h.ctx, post.PostID)
		if owner.OwnerUserHash == "" || owner.OwnerUserHash != h.service.labels.UserHash("user-a") {
			t.Fatal("owner hash not filled with the creator's by the next send")
		}
	})
}
