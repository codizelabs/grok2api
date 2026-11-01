package imaginepost

// Security tests for P3 (contract test plan group F, sec-rowan B5 and B6).
// Everything runs against the in-process fake grok.com from harness_test.go;
// nothing here reaches the network.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
)

// secAsError returns err as a P3 *Error or fails the test.
func secAsError(t *testing.T, name string, err error) *Error {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("%s: err = %v, want a *imaginepost.Error", name, err)
	}
	return failure
}

// secExpect asserts err is a P3 *Error with the given code and send state.
func secExpect(t *testing.T, name string, err error, code string, send SendState) *Error {
	t.Helper()
	failure := secAsError(t, name, err)
	if failure.Code != code {
		t.Errorf("%s: code = %q, want %q", name, failure.Code, code)
	}
	if failure.Send != send {
		t.Errorf("%s: send_state = %q, want %q", name, failure.Send, send)
	}
	return failure
}

// secSeedOwnedPost seeds a post on the fake for accountA and pins it to the
// harness's client key, returning the post's asset ids and accountA's label.
func secSeedOwnedPost(h *harness, conversationID string) (rootAsset, resultAsset, label string) {
	h.t.Helper()
	rootAsset, resultAsset, _ = h.fake.SeedPost(ssoA, conversationID)
	if err := h.service.Store().PutOwner(h.ctx, PostOwner{ConversationID: conversationID, AccountID: accountA, ClientKeyID: keyID}); err != nil {
		h.t.Fatalf("put owner: %v", err)
	}
	return rootAsset, resultAsset, h.service.labels.AccountLabel(accountA)
}

func secCallRows(h *harness) []CallRecord {
	h.t.Helper()
	rows, err := h.service.Store().Calls(h.ctx)
	if err != nil {
		h.t.Fatalf("store calls: %v", err)
	}
	return rows
}

func secRowText(row CallRecord) string { return fmt.Sprintf("%+v", row) }

type secCallCase struct {
	name   string
	policy Policy
	call   Call
}

// --- F1: the allow-list ------------------------------------------------------

func TestSecAllowListDeniesEverythingOffTheList(t *testing.T) {
	const conv = "conv-1234"
	post := http.MethodPost
	get := http.MethodGet
	denied := []secCallCase{
		{"post create", Policy{}, Call{Method: post, Path: "/rest/media/post/create", Body: []byte(`{}`)}},
		{"asset clone", Policy{}, Call{Method: post, Path: "/rest/assets/asset-1/clone", Body: []byte(`{}`)}},
		{"media conversation create", Policy{}, Call{Method: post, Path: "/rest/media/conversation/create", Body: []byte(`{}`)}},
		{"stop inflight", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/" + conv + "/stop-inflight-responses", Body: []byte(`{}`)}},
		{"reconnect GET", Policy{}, Call{Method: get, Path: "/rest/app-chat/conversations/" + conv + "/reconnect-response-v2/resp-1"}},
		{"reconnect POST", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/" + conv + "/reconnect-response-v2/resp-1", Body: []byte(`{}`)}},
		{"video upscale", Policy{}, Call{Method: post, Path: "/rest/media/video/upscale", Body: []byte(`{}`)}},
		{"segment cachedOnly false", Policy{}, Call{Method: post, Path: "/rest/media/segment", Body: []byte(`{"assetId":"asset-1","cachedOnly":false,"maskFormat":"rle"}`)}},
		{"segment cachedOnly missing", Policy{}, Call{Method: post, Path: "/rest/media/segment", Body: []byte(`{"assetId":"asset-1","maskFormat":"rle"}`)}},
		{"segment cachedOnly string", Policy{}, Call{Method: post, Path: "/rest/media/segment", Body: []byte(`{"assetId":"asset-1","cachedOnly":"true","maskFormat":"rle"}`)}},
		{"segment extra key", Policy{}, Call{Method: post, Path: "/rest/media/segment", Body: []byte(`{"assetId":"asset-1","cachedOnly":true,"maskFormat":"rle","force":true}`)}},
		{"segment no body", Policy{}, Call{Method: post, Path: "/rest/media/segment"}},
		{"websocket bot", Policy{}, Call{Method: get, Path: "wss://grok.com/ws/bot/"}},
		{"cdn asset", Policy{}, Call{Method: get, Path: "https://cdn.grok.com/_next/x.js"}},
		{"imagine post page GET", Policy{}, Call{Method: get, Path: "/imagine/post/" + conv}},
		{"imagine post page POST", Policy{}, Call{Method: post, Path: "/imagine/post/" + conv, Body: []byte(`[]`)}},
		{"like disabled", Policy{}, Call{Method: post, Path: "/rest/media/post/like", Body: []byte(`{"id":"asset-1"}`)}},
		{"unlike disabled", Policy{}, Call{Method: post, Path: "/rest/media/post/unlike", Body: []byte(`{"id":"asset-1"}`)}},
		{"path traversal", Policy{}, Call{Method: post, Path: "/rest/assets/../media/post/create", Body: []byte(`{}`)}},
		{"encoded traversal", Policy{}, Call{Method: get, Path: "/rest/assets/%2e%2e"}},
		{"double slash", Policy{}, Call{Method: get, Path: "//rest/assets/asset-1"}},
		{"DELETE", Policy{}, Call{Method: http.MethodDelete, Path: "/rest/assets/asset-1"}},
		{"lower-case method", Policy{}, Call{Method: "post", Path: "/rest/app-chat/conversations/new", Body: []byte(`{"message":"x"}`)}},
		{"absolute grok.com URL", Policy{}, Call{Method: get, Path: "https://grok.com/rest/assets/asset-1"}},
		{"absolute grok.com send", Policy{}, Call{Method: post, Path: "https://grok.com/rest/app-chat/conversations/new", Body: []byte(`{"message":"x"}`)}},
		{"makePublic top level", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new", Body: []byte(`{"message":"x","makePublic":false}`)}},
		{"makePublic nested", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new", Body: []byte(`{"message":"x","a":{"b":[1,{"c":{"makePublic":true}}]}}`)}},
		{"makePublic nested add-response", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/" + conv + "/responses", Body: []byte(`{"mediaGenInput":{"imageToImage":{"opts":[{"makePublic":true}]}}}`)}},
		{"send body array", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new", Body: []byte(`[]`)}},
		{"send body not JSON", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new", Body: []byte(`message=x`)}},
		{"send body null", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new", Body: []byte(`null`)}},
		{"send with query", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new?makePublic=true", Body: []byte(`{"message":"x"}`)}},
		{"GET with body", Policy{}, Call{Method: get, Path: "/rest/assets/asset-1", Body: []byte(`{}`)}},
		{"GET responses with body", Policy{}, Call{Method: get, Path: "/rest/app-chat/conversations/" + conv + "/responses", Body: []byte(`{}`)}},
		{"quota with body", Policy{}, Call{Method: post, Path: "/rest/media/imagine/quota_info", Body: []byte(`{"x":1}`)}},
		{"upload with body", Policy{}, Call{Method: post, Path: "/http/upload-file-v2/direct", Body: []byte(`{"makePublic":true}`)}},
		{"assets over http", Policy{}, Call{Method: get, Path: "http://assets.grok.com/users/u/a/content"}},
		{"assets lookalike host", Policy{}, Call{Method: get, Path: "https://assets.grok.com.example.com/users/u/a/content"}},
		{"assets traversal", Policy{}, Call{Method: get, Path: "https://assets.grok.com/users/../admin"}},
		{"assets POST", Policy{}, Call{Method: post, Path: "https://assets.grok.com/users/u/a/content"}},
		{"assets with userinfo", Policy{}, Call{Method: get, Path: "https://x@assets.grok.com/users/u/a/content"}},
		{"assets outside users", Policy{}, Call{Method: get, Path: "https://assets.grok.com/other/a"}},
	}
	for _, tc := range denied {
		t.Run("deny "+tc.name, func(t *testing.T) {
			secExpect(t, tc.name, CheckCall(tc.policy, tc.call), CodeCallNotAllowed, NotSent)
		})
	}

	allowed := []secCallCase{
		{"conversations new", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/new", Body: []byte(`{"message":"x","mediaGenInput":{}}`)}},
		{"add response", Policy{}, Call{Method: post, Path: "/rest/app-chat/conversations/" + conv + "/responses", Body: []byte(`{"message":"x"}`)}},
		{"list responses", Policy{}, Call{Method: get, Path: "/rest/app-chat/conversations/" + conv + "/responses"}},
		{"asset record", Policy{}, Call{Method: get, Path: "/rest/assets/asset-1"}},
		{"segment cachedOnly true", Policy{}, Call{Method: post, Path: "/rest/media/segment", Body: []byte(`{"assetId":"asset-1","cachedOnly":true,"maskFormat":"rle"}`)}},
		{"quota", Policy{}, Call{Method: post, Path: "/rest/media/imagine/quota_info", Body: []byte(`{}`)}},
		{"upload", Policy{}, Call{Method: post, Path: "/http/upload-file-v2/direct"}},
		{"item media", Policy{}, Call{Method: get, Path: "https://assets.grok.com/users/u/generated/asset-1/image.jpg"}},
		{"like allowed", Policy{AllowLike: true}, Call{Method: post, Path: "/rest/media/post/like", Body: []byte(`{"id":"asset-1"}`)}},
		{"unlike allowed", Policy{AllowLike: true}, Call{Method: post, Path: "/rest/media/post/unlike", Body: []byte(`{"id":"asset-1"}`)}},
	}
	for _, tc := range allowed {
		t.Run("allow "+tc.name, func(t *testing.T) {
			if err := CheckCall(tc.policy, tc.call); err != nil {
				t.Errorf("%s: CheckCall = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestSecPreSendRefusalsMakeZeroCalls(t *testing.T) {
	h := newDefaultHarness(t)
	const conv = "conv-sec-pre-0001"
	if err := h.service.Store().PutOwner(h.ctx, PostOwner{ConversationID: conv, AccountID: accountA, ClientKeyID: keyID}); err != nil {
		t.Fatal(err)
	}
	labelA := h.service.labels.AccountLabel(accountA)
	labelB := h.service.labels.AccountLabel(accountB)
	other := Caller{ClientKeyID: 99}
	edit := EditInput{JobID: "job-pre-edit", Prompt: "edit it", SourceAssetID: "asset-1"}
	video := VideoInput{JobID: "job-pre-video", Prompt: "move", SourceAssetID: "asset-1", DurationS: 15, Resolution: "720p"}

	cases := []struct {
		name string
		code string
		run  func() error
	}{
		{"create without job id", CodeJobIDRequired, func() error {
			_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{Prompt: "p", Image: pngImage()})
			return err
		}},
		{"create empty prompt", CodeInvalidRequest, func() error {
			_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-pre-1", Prompt: "  ", Image: pngImage()})
			return err
		}},
		{"create text only", CodeCapabilityUnsupported, func() error {
			_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-pre-2", Prompt: "p"})
			return err
		}},
		{"create non-image upload", CodeInvalidRequest, func() error {
			_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-pre-3", Prompt: "p", Image: &Image{MIMEType: "text/html", Data: []byte("<x>")}})
			return err
		}},
		{"edit without job id", CodeJobIDRequired, func() error {
			input := edit
			input.JobID = ""
			_, err := h.service.EditInPost(h.ctx, h.caller, conv, labelA, input)
			return err
		}},
		{"edit traversal asset", CodeInvalidRequest, func() error {
			input := edit
			input.SourceAssetID = "../x"
			_, err := h.service.EditInPost(h.ctx, h.caller, conv, labelA, input)
			return err
		}},
		{"edit unknown post", CodePostNotFound, func() error {
			_, err := h.service.EditInPost(h.ctx, h.caller, "conv-unknown-0001", labelA, edit)
			return err
		}},
		{"edit traversal post id", CodePostNotFound, func() error {
			_, err := h.service.EditInPost(h.ctx, h.caller, "../media/post/create", labelA, edit)
			return err
		}},
		{"edit missing label", CodeAccountLabelRequired, func() error {
			_, err := h.service.EditInPost(h.ctx, h.caller, conv, "", edit)
			return err
		}},
		{"edit wrong label", CodeAccountLabelMismatch, func() error {
			_, err := h.service.EditInPost(h.ctx, h.caller, conv, labelB, edit)
			return err
		}},
		{"edit other key", CodePostNotFound, func() error {
			_, err := h.service.EditInPost(h.ctx, other, conv, labelA, edit)
			return err
		}},
		{"video 6s", CodeSpecUnsupported, func() error {
			input := video
			input.DurationS = 6
			_, err := h.service.VideoInPost(h.ctx, h.caller, conv, labelA, input)
			return err
		}},
		{"video 1080p", CodeSpecUnsupported, func() error {
			input := video
			input.Resolution = "1080p"
			_, err := h.service.VideoInPost(h.ctx, h.caller, conv, labelA, input)
			return err
		}},
		{"video wrong label", CodeAccountLabelMismatch, func() error {
			_, err := h.service.VideoInPost(h.ctx, h.caller, conv, labelB, video)
			return err
		}},
		{"list missing label", CodeAccountLabelRequired, func() error {
			_, err := h.service.ListItems(h.ctx, h.caller, conv, " ")
			return err
		}},
		{"list other key", CodePostNotFound, func() error {
			_, err := h.service.ListItems(h.ctx, other, conv, labelA)
			return err
		}},
		{"read item traversal asset", CodeItemNotInPost, func() error {
			_, err := h.service.ReadItem(h.ctx, h.caller, conv, labelA, "../../etc")
			return err
		}},
		{"read item wrong label", CodeAccountLabelMismatch, func() error {
			_, err := h.service.ReadItem(h.ctx, h.caller, conv, labelB, "asset-1")
			return err
		}},
		{"segments wrong label", CodeAccountLabelMismatch, func() error {
			_, err := h.service.Segments(h.ctx, h.caller, conv, labelB, "asset-1")
			return err
		}},
		{"media wrong label", CodeAccountLabelMismatch, func() error {
			body, _, err := h.service.FetchMedia(h.ctx, h.caller, conv, labelB, "asset-1")
			if body != nil {
				_ = body.Close()
			}
			return err
		}},
		{"like disabled", CodeLikeDisabled, func() error {
			return h.service.Like(h.ctx, h.caller, conv, labelA, "asset-1", false)
		}},
		{"unlike disabled", CodeLikeDisabled, func() error {
			return h.service.Like(h.ctx, h.caller, conv, labelA, "asset-1", true)
		}},
		{"quota unknown label", CodeAccountLabelMismatch, func() error {
			_, err := h.service.QuotaByLabel(h.ctx, h.caller, labelB)
			return err
		}},
	}
	for _, tc := range cases {
		beforeTransport, beforeFake := len(h.transport.Calls()), len(h.fake.Calls())
		secExpect(t, tc.name, tc.run(), tc.code, NotSent)
		if got := len(h.transport.Calls()) - beforeTransport; got != 0 {
			t.Errorf("%s: %d transport calls, want 0 (%v)", tc.name, got, h.transport.Calls()[beforeTransport:])
		}
		if got := len(h.fake.Calls()) - beforeFake; got != 0 {
			t.Errorf("%s: %d fake grok.com calls, want 0", tc.name, got)
		}
	}
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Errorf("fake grok.com saw %d calls in total, want 0", len(calls))
	}
}

// --- F2: Statsig fails closed ------------------------------------------------

func TestSecStatsigFailsClosed(t *testing.T) {
	for _, statsig := range []string{"", "invalid"} {
		t.Run(fmt.Sprintf("statsig=%q", statsig), func(t *testing.T) {
			h := newHarness(t, statsig, Config{})
			const conv = "conv-sec-statsig-0001"
			_, resultAsset, label := secSeedOwnedPost(h, conv)
			if len(h.fake.Calls()) != 0 {
				t.Fatal("seeding made fake calls")
			}
			ops := []struct {
				name string
				run  func() error
			}{
				{"CreatePost", func() error {
					_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-st-create", Prompt: "p", Image: pngImage()})
					return err
				}},
				{"EditInPost", func() error {
					_, err := h.service.EditInPost(h.ctx, h.caller, conv, label, EditInput{JobID: "job-st-edit", Prompt: "p", SourceAssetID: resultAsset})
					return err
				}},
				{"VideoInPost", func() error {
					_, err := h.service.VideoInPost(h.ctx, h.caller, conv, label, VideoInput{JobID: "job-st-video", Prompt: "p", SourceAssetID: resultAsset, DurationS: 15, Resolution: "720p"})
					return err
				}},
				{"ListItems", func() error {
					_, err := h.service.ListItems(h.ctx, h.caller, conv, label)
					return err
				}},
				{"ReadItem", func() error {
					_, err := h.service.ReadItem(h.ctx, h.caller, conv, label, resultAsset)
					return err
				}},
				{"Segments", func() error {
					_, err := h.service.Segments(h.ctx, h.caller, conv, label, resultAsset)
					return err
				}},
			}
			for _, op := range ops {
				before := len(h.fake.Calls())
				secExpect(t, op.name, op.run(), CodeStatsigUnavailable, NotSent)
				if calls := h.fake.Calls()[before:]; len(calls) != 0 {
					t.Errorf("%s: fake grok.com saw %d calls without a valid Statsig, want 0 (first: %s %s)", op.name, len(calls), calls[0].Method, calls[0].Path)
				}
			}
			if calls := h.fake.Calls(); len(calls) != 0 {
				t.Errorf("fake grok.com saw %d calls in total without a valid Statsig, want 0", len(calls))
			}
		})
	}
}

// --- F3: pinned account, no fallback -----------------------------------------

func TestSecPinnedAccountNeverFallsBack(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-pin-create")
	if post.AccountLabel != h.service.labels.AccountLabel(accountA) {
		t.Fatalf("post pinned to %q, want accountA's label", post.AccountLabel)
	}
	assetID := post.AssetIDs[0]
	// A fallback through AcquireNew would now land on accountB.
	h.pool.mu.Lock()
	h.pool.newAccount = accountB
	h.pool.mu.Unlock()
	h.pool.setUnavailable(accountA, refusedErr{status: http.StatusTooManyRequests})
	before := len(h.fake.Calls())

	ops := []struct {
		name string
		run  func() error
	}{
		{"EditInPost", func() error {
			_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-pin-edit", Prompt: "edit", SourceAssetID: assetID})
			return err
		}},
		{"VideoInPost", func() error {
			_, err := h.service.VideoInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, VideoInput{JobID: "job-pin-video", Prompt: "move", SourceAssetID: assetID, DurationS: 15, Resolution: "720p"})
			return err
		}},
		{"Segments", func() error {
			_, err := h.service.Segments(h.ctx, h.caller, post.PostID, post.AccountLabel, assetID)
			return err
		}},
		{"ListItems", func() error {
			_, err := h.service.ListItems(h.ctx, h.caller, post.PostID, post.AccountLabel)
			return err
		}},
		{"ReadItem", func() error {
			_, err := h.service.ReadItem(h.ctx, h.caller, post.PostID, post.AccountLabel, assetID)
			return err
		}},
	}
	for _, op := range ops {
		failure := secExpect(t, op.name, op.run(), CodePinnedAccountUnavailable, NotSent)
		if failure.Status != http.StatusTooManyRequests {
			t.Errorf("%s: status = %d, want 429 for a 429 refusal", op.name, failure.Status)
		}
		if failure.RetryAfter <= 0 {
			t.Errorf("%s: RetryAfter = %v, want > 0", op.name, failure.RetryAfter)
		}
	}
	for _, id := range h.pool.acquiredAccounts() {
		if id == accountB {
			t.Errorf("a lease was taken on accountB (acquired: %v); a pinned post must never move", h.pool.acquiredAccounts())
		}
	}
	if n := h.fake.CountByAccount(ssoB); n != 0 {
		t.Errorf("accountB made %d grok.com calls, want 0", n)
	}
	if after := len(h.fake.Calls()); after != before {
		t.Errorf("%d grok.com calls after the pinned account was refused, want 0", after-before)
	}
}

func TestSecPinnedPostLabelAndKeyChecks(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-label-create")
	assetID := post.AssetIDs[0]
	labelB := h.service.labels.AccountLabel(accountB)
	before := len(h.fake.Calls())
	acquiredBefore := len(h.pool.acquiredAccounts())

	cases := []struct {
		name   string
		caller Caller
		label  string
		code   string
	}{
		{"missing label", h.caller, "", CodeAccountLabelRequired},
		{"wrong label", h.caller, labelB, CodeAccountLabelMismatch},
		{"other client key", Caller{ClientKeyID: 99}, post.AccountLabel, CodePostNotFound},
	}
	for i, tc := range cases {
		_, err := h.service.EditInPost(h.ctx, tc.caller, post.PostID, tc.label, EditInput{JobID: fmt.Sprintf("job-label-edit-%d", i), Prompt: "edit", SourceAssetID: assetID})
		secExpect(t, tc.name+" edit", err, tc.code, NotSent)
		_, err = h.service.VideoInPost(h.ctx, tc.caller, post.PostID, tc.label, VideoInput{JobID: fmt.Sprintf("job-label-video-%d", i), Prompt: "move", SourceAssetID: assetID, DurationS: 15, Resolution: "720p"})
		secExpect(t, tc.name+" video", err, tc.code, NotSent)
		_, err = h.service.Segments(h.ctx, tc.caller, post.PostID, tc.label, assetID)
		secExpect(t, tc.name+" segments", err, tc.code, NotSent)
		_, err = h.service.ListItems(h.ctx, tc.caller, post.PostID, tc.label)
		secExpect(t, tc.name+" list", err, tc.code, NotSent)
	}
	if after := len(h.fake.Calls()); after != before {
		t.Errorf("label/key refusals made %d grok.com calls, want 0", after-before)
	}
	if acquired := h.pool.acquiredAccounts(); len(acquired) != acquiredBefore {
		t.Errorf("label/key refusals leased accounts %v, want none", acquired[acquiredBefore:])
	}
}

// --- F5: one send per job_id -------------------------------------------------

func TestSecEditJobIDOneSendEvenAfterRestart(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-f5-create")
	input := EditInput{JobID: "job-f5-edit", Prompt: "edit once", SourceAssetID: post.AssetIDs[0]}
	sendsBefore := h.fake.Sends()

	if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, input); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, input)
	failure := secExpect(t, "second edit", err, CodeJobIDReused, NotSent)
	if failure.Status != http.StatusConflict {
		t.Errorf("second edit status = %d, want 409", failure.Status)
	}
	if got := h.fake.Sends() - sendsBefore; got != 1 {
		t.Errorf("two edits with one job_id made %d sends, want exactly 1", got)
	}

	// The create's job id is used too, whatever the op.
	reuse := input
	reuse.JobID = "job-f5-create"
	_, err = h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, reuse)
	secExpect(t, "edit with the create's job id", err, CodeJobIDReused, NotSent)
	if got := h.fake.Sends() - sendsBefore; got != 1 {
		t.Errorf("after reusing the create's job id: %d sends, want 1", got)
	}

	restarted, err := New(h.ctx, Options{DB: h.db, Pool: h.pool, Transport: h.transport, LabelSecret: "test-label-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = restarted.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, input)
	failure = secExpect(t, "edit after restart", err, CodeJobIDReused, NotSent)
	if failure.Status != http.StatusConflict {
		t.Errorf("edit after restart status = %d, want 409", failure.Status)
	}
	if got := h.fake.Sends() - sendsBefore; got != 1 {
		t.Errorf("after restart: %d sends in total, want 1", got)
	}

	// A fresh job still sends after the restart: the guard is per job, not a blanket refusal.
	fresh := input
	fresh.JobID = "job-f5-edit-2"
	if _, err := restarted.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, fresh); err != nil {
		t.Fatalf("fresh job after restart: %v", err)
	}
	if got := h.fake.Sends() - sendsBefore; got != 2 {
		t.Errorf("after a fresh job: %d sends, want 2", got)
	}
}

func TestSecCreateJobIDOneSendEvenAfterRestart(t *testing.T) {
	h := newDefaultHarness(t)
	input := CreateInput{JobID: "job-f5c", Prompt: "anchor prompt", Image: pngImage()}
	if _, err := h.service.CreatePost(h.ctx, h.caller, input); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if got := h.fake.Sends(); got != 1 {
		t.Fatalf("first create made %d sends, want 1", got)
	}
	callsBefore := len(h.fake.Calls())
	_, err := h.service.CreatePost(h.ctx, h.caller, input)
	failure := secExpect(t, "second create", err, CodeJobIDReused, NotSent)
	if failure.Status != http.StatusConflict {
		t.Errorf("second create status = %d, want 409", failure.Status)
	}
	if got := h.fake.Sends(); got != 1 {
		t.Errorf("two creates with one job_id made %d sends, want 1", got)
	}
	if got := h.fake.Count(http.MethodPost, `^/http/upload-file-v2/direct$`); got != 1 {
		t.Errorf("two creates with one job_id made %d uploads, want 1", got)
	}

	restarted, err := New(h.ctx, Options{DB: h.db, Pool: h.pool, Transport: h.transport, LabelSecret: "test-label-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = restarted.CreatePost(h.ctx, h.caller, input)
	secExpect(t, "create after restart", err, CodeJobIDReused, NotSent)
	if got := h.fake.Sends(); got != 1 {
		t.Errorf("after restart: %d sends, want 1", got)
	}
	if got := len(h.fake.Calls()) - callsBefore; got != 0 {
		t.Errorf("refused creates made %d grok.com calls, want 0", got)
	}
}

// --- F6: audit rows ----------------------------------------------------------

func TestSecAuditRowsHoldCodesNotContent(t *testing.T) {
	h := newDefaultHarness(t)
	const ownerID = "fake-owner-user-f6-55aa"
	const prompt = "audit-secret-prompt-f6"
	h.fake.SetUserID(ssoA, ownerID)
	post := h.createPost("job-f6-create")
	h.fake.SetFault(fakegrok.FaultPeakHour)
	_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-f6-edit", Prompt: prompt, SourceAssetID: post.AssetIDs[0]})
	failure := secExpect(t, "peak-hour edit", err, CodeRateLimited, NotSent)
	if failure.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("edit upstream status = %d, want 429", failure.UpstreamStatus)
	}

	rows := secCallRows(h)
	var limited []CallRecord
	for _, row := range rows {
		if row.Code == CodeRateLimited {
			limited = append(limited, row)
		}
	}
	if len(limited) != 1 {
		t.Fatalf("audit has %d rate_limited rows, want 1 (rows: %+v)", len(limited), rows)
	}
	row := limited[0]
	if row.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("rate_limited row upstream status = %d, want 429", row.UpstreamStatus)
	}
	if row.BodySHA256 == "" {
		t.Error("rate_limited row has no body hash")
	}
	if row.AccountID != accountA {
		t.Errorf("rate_limited row account = %d, want %d", row.AccountID, accountA)
	}
	hex8 := regexp.MustCompile(`^[0-9a-f]{8}$`)
	if !hex8.MatchString(row.PostRef) {
		t.Errorf("rate_limited row PostRef = %q, want 8 hex chars", row.PostRef)
	}
	for _, r := range rows {
		if r.PostRef != "" && !hex8.MatchString(r.PostRef) {
			t.Errorf("row %d PostRef = %q, want 8 hex chars", r.ID, r.PostRef)
		}
		text := secRowText(r)
		for _, forbidden := range []string{prompt, "anchor prompt", post.PostID, "https://", ownerID, "Too many requests"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("audit row %d contains %q: %s", r.ID, forbidden, text)
			}
		}
	}
}

// --- F7: like disabled -------------------------------------------------------

func TestSecLikeDisabledByDefault(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-f7-create")
	before := len(h.transport.Calls())
	for _, unlike := range []bool{false, true} {
		err := h.service.Like(h.ctx, h.caller, post.PostID, post.AccountLabel, post.AssetIDs[0], unlike)
		failure := secExpect(t, fmt.Sprintf("like(unlike=%v)", unlike), err, CodeLikeDisabled, NotSent)
		if failure.Status != http.StatusForbidden {
			t.Errorf("like(unlike=%v) status = %d, want 403", unlike, failure.Status)
		}
	}
	if got := len(h.transport.Calls()) - before; got != 0 {
		t.Errorf("disabled like made %d transport calls, want 0", got)
	}
	if got := h.fake.Count(http.MethodPost, `^/rest/media/post/(like|unlike)$`); got != 0 {
		t.Errorf("fake saw %d like/unlike calls, want 0", got)
	}
}

func TestSecLikeAllowedOnlyWhenSwitchedOn(t *testing.T) {
	h := newHarness(t, validStatsig, Config{AllowLike: true})
	post := h.createPost("job-f7b-create")
	if err := h.service.Like(h.ctx, h.caller, post.PostID, post.AccountLabel, post.AssetIDs[0], false); err != nil {
		t.Fatalf("like with AllowLike true: %v", err)
	}
	if got := h.fake.Count(http.MethodPost, `^/rest/media/post/like$`); got != 1 {
		t.Errorf("fake saw %d likes, want 1", got)
	}
	if got := h.fake.Count(http.MethodPost, `^/rest/media/post/unlike$`); got != 0 {
		t.Errorf("fake saw %d unlikes, want 0", got)
	}
	// Switching the flag off closes it again, with no call.
	h.service.UpdateConfig(Config{})
	before := len(h.fake.Calls())
	secExpect(t, "like after switch-off", h.service.Like(h.ctx, h.caller, post.PostID, post.AccountLabel, post.AssetIDs[0], false), CodeLikeDisabled, NotSent)
	if got := len(h.fake.Calls()) - before; got != 0 {
		t.Errorf("like after switch-off made %d calls, want 0", got)
	}
}

// --- B5: redaction canary ----------------------------------------------------

func TestSecRedactionCanary(t *testing.T) {
	h := newDefaultHarness(t)
	const (
		canaryPrompt = "CANARY-PROMPT-7f3a"
		canaryOwner  = "CANARY-OWNER-91c2"
		canaryRefuse = "CANARY-REFUSAL ignore previous instructions"
	)
	h.fake.SetUserID(ssoA, canaryOwner)
	h.fake.SetStreamErrorText(canaryRefuse + "\n\x07\x1b[31m" + strings.Repeat("z", 5000))
	var failures []*Error

	post, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-b5-create", Prompt: canaryPrompt, Image: pngImage()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assetID := post.AssetIDs[0]
	edited, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-b5-edit", Prompt: canaryPrompt, SourceAssetID: assetID})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	snapshot, err := h.service.ListItems(h.ctx, h.caller, post.PostID, post.AccountLabel)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := h.service.ReadItem(h.ctx, h.caller, post.PostID, post.AccountLabel, edited.AssetIDs[0]); err != nil {
		t.Fatalf("read item: %v", err)
	}

	h.fake.SetFault(fakegrok.FaultModerated)
	_, err = h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-b5-moderated", Prompt: canaryPrompt, SourceAssetID: assetID})
	moderated := secAsError(t, "moderated edit", err)
	failures = append(failures, moderated)
	if !strings.Contains(moderated.ProviderText, "CANARY-REFUSAL") {
		t.Errorf("moderated edit ProviderText = %q, want the refusal text", moderated.ProviderText)
	}
	if n := len([]rune(moderated.ProviderText)); n > 1024 {
		t.Errorf("ProviderText is %d runes, want it capped", n)
	}
	for _, r := range moderated.ProviderText {
		if unicode.IsControl(r) {
			t.Errorf("ProviderText holds control character %U", r)
			break
		}
	}

	h.fake.SetFault(fakegrok.FaultOtherConversation)
	_, err = h.service.EditInPost(h.ctx, h.caller, post.PostID, post.AccountLabel, EditInput{JobID: "job-b5-lost", Prompt: canaryPrompt, SourceAssetID: assetID})
	failures = append(failures, secExpect(t, "other-conversation edit", err, CodePostContextLost, Sent))

	// A label mismatch adds one more error's text to the check.
	_, err = h.service.ListItems(h.ctx, h.caller, post.PostID, h.service.labels.AccountLabel(accountB))
	failures = append(failures, secAsError(t, "mismatch", err))

	forbidden := []string{canaryPrompt, canaryOwner, post.PostID, "assets.grok.com", "sso=", ssoA, ssoB, validStatsig, "CANARY-REFUSAL"}
	logs := h.logs.String()
	if logs == "" {
		t.Error("no log output captured; the redaction check would be vacuous")
	}
	for _, value := range forbidden {
		if strings.Contains(logs, value) {
			t.Errorf("logs contain %q", value)
		}
	}
	for _, failure := range failures {
		for _, value := range forbidden {
			if strings.Contains(failure.Error(), value) {
				t.Errorf("%s Error() contains %q", failure.Code, value)
			}
			if strings.Contains(failure.Message(), value) {
				t.Errorf("%s Message() contains %q", failure.Code, value)
			}
		}
		if failure != moderated && strings.Contains(failure.ProviderText, "CANARY") {
			t.Errorf("%s ProviderText carries canary text", failure.Code)
		}
	}
	rows := secCallRows(h)
	if len(rows) == 0 {
		t.Error("no audit rows; the redaction check would be vacuous")
	}
	for _, row := range rows {
		text := secRowText(row)
		for _, value := range forbidden {
			if strings.Contains(text, value) {
				t.Errorf("audit row %d contains %q", row.ID, value)
			}
		}
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{canaryPrompt, `"message"`, "mediaGenInput", canaryOwner, "assets.grok.com"} {
		if strings.Contains(string(encoded), value) {
			t.Errorf("ListItems snapshot contains %q: %s", value, encoded)
		}
	}
	if len(snapshot.Items) == 0 {
		t.Error("ListItems snapshot has no items; the snapshot check would be vacuous")
	}
}

// --- B6: segments are cachedOnly ---------------------------------------------

func secSegmentCalls(h *harness) []fakegrok.Call {
	var out []fakegrok.Call
	for _, call := range h.fake.Calls() {
		if call.Method == http.MethodPost && call.Path == "/rest/media/segment" {
			out = append(out, call)
		}
	}
	return out
}

func TestSecSegmentsAlwaysCachedOnly(t *testing.T) {
	h := newDefaultHarness(t)
	const conv = "conv-sec-seg-0001"
	rootAsset, resultAsset, label := secSeedOwnedPost(h, conv)
	h.fake.SetSegments(resultAsset, []fakegrok.Segment{{Name: "person", Box: [4]float64{1, 2, 30, 40}, Score: 0.9, Size: [2]int{96, 64}, Counts: "PXa1b2"}})

	segments, err := h.service.Segments(h.ctx, h.caller, conv, label, resultAsset)
	if err != nil {
		t.Fatalf("cached segments: %v", err)
	}
	if len(segments.Objects) != 1 {
		t.Errorf("got %d segment objects, want 1", len(segments.Objects))
	}
	calls := secSegmentCalls(h)
	if len(calls) != 1 {
		t.Fatalf("fake saw %d segment calls, want 1", len(calls))
	}
	if calls[0].Account != ssoA {
		t.Errorf("segment call ran on %q, want the pinned account", calls[0].Account)
	}

	_, err = h.service.Segments(h.ctx, h.caller, conv, label, rootAsset)
	secExpect(t, "uncached segments", err, CodeSegmentsNotCached, NotSent)
	calls = secSegmentCalls(h)
	if len(calls) != 2 {
		t.Fatalf("after the uncached read the fake saw %d segment calls, want 2 (one per Segments call, no retry)", len(calls))
	}
	for i, call := range calls {
		var body map[string]any
		if err := json.Unmarshal(call.Body, &body); err != nil {
			t.Fatalf("segment call %d body is not JSON: %v", i, err)
		}
		if cached, ok := body["cachedOnly"].(bool); !ok || !cached {
			t.Errorf("segment call %d sent cachedOnly = %#v, want boolean true (body %s)", i, body["cachedOnly"], call.Body)
		}
	}
}
