package imaginepost

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
)

func (h *harness) label() string { return h.service.labels.AccountLabel(accountA) }

func lastBody(t *testing.T, fake *fakegrok.Server, method, path string) map[string]any {
	t.Helper()
	calls := fake.Calls()
	for index := len(calls) - 1; index >= 0; index-- {
		if calls[index].Method == method && calls[index].Path == path {
			var body map[string]any
			if err := json.Unmarshal(calls[index].Body, &body); err != nil {
				t.Fatalf("body of %s %s: %v", method, path, err)
			}
			return body
		}
	}
	t.Fatalf("no %s %s call", method, path)
	return nil
}

func TestCreatePostPinsTheCreatingAccount(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SetUserID(ssoA, "user-a")
	result := h.createPost("job-create-1")
	if result.PostID == "" || len(result.AssetIDs) != 1 || result.ResponseID == "" || !result.ContextVerified {
		t.Fatalf("result = %+v", result)
	}
	if result.AccountLabel != h.label() || strings.Contains(result.AccountLabel, "11") {
		t.Fatalf("account label = %q, want the opaque label of account A", result.AccountLabel)
	}
	owner, err := h.service.Store().Owner(h.ctx, result.PostID)
	if err != nil || owner.AccountID != accountA || owner.ClientKeyID != keyID || owner.OwnerUserHash == "" {
		t.Fatalf("owner = %+v err=%v", owner, err)
	}
	if h.fake.Sends() != 1 || h.fake.CountByAccount(ssoB) != 0 {
		t.Fatalf("sends = %d, account B calls = %d", h.fake.Sends(), h.fake.CountByAccount(ssoB))
	}
	body := lastBody(t, h.fake, http.MethodPost, "/rest/app-chat/conversations/new")
	if body["modelName"] != "imagine-image-edit" || body["kind"] != "CONVERSATION_KIND_IMAGINE" {
		t.Fatalf("create body = %v", body)
	}
}

func TestCreatePostNeedsAnImage(t *testing.T) {
	h := newDefaultHarness(t)
	_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-text", Prompt: "text only"})
	if errCode(err) != CodeCapabilityUnsupported || len(h.transport.Calls()) != 0 {
		t.Fatalf("err = %v, calls = %v", err, h.transport.Calls())
	}
	_, err = h.service.CreatePost(h.ctx, h.caller, CreateInput{Prompt: "p", Image: pngImage()})
	if errCode(err) != CodeJobIDRequired {
		t.Fatalf("missing job id: %v", err)
	}
}

func TestEditInPostOnHeadParent(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	before, err := h.service.ListItems(h.ctx, h.caller, post.PostID, h.label())
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-e", Prompt: "eyes closed", SourceAssetID: post.AssetIDs[0]})
	if err != nil {
		t.Fatal(err)
	}
	if result.PostID != post.PostID || result.ParentResponseID != before.HeadResponseID || result.SourceAssetID != post.AssetIDs[0] || !result.ContextVerified || result.Kind != "image" {
		t.Fatalf("edit result = %+v (head %s)", result, before.HeadResponseID)
	}
	body := lastBody(t, h.fake, http.MethodPost, "/rest/app-chat/conversations/"+post.PostID+"/responses")
	if body["parentResponseId"] != before.HeadResponseID || body["modelName"] != "imagine-image-edit" {
		t.Fatalf("edit body = %v", body)
	}
	inputs := body["mediaGenInput"].(map[string]any)["imageToImage"].(map[string]any)["inputAssets"].([]any)
	if len(inputs) != 1 || inputs[0] != post.AssetIDs[0] {
		t.Fatalf("input assets = %v", inputs)
	}
	if h.fake.Sends() != 2 || h.fake.CountByAccount(ssoB) != 0 {
		t.Fatalf("sends = %d", h.fake.Sends())
	}
	after, err := h.service.ListItems(h.ctx, h.caller, post.PostID, h.label())
	if err != nil || after.HeadResponseID != result.ResponseID {
		t.Fatalf("head after edit = %q, want %q (err %v)", after.HeadResponseID, result.ResponseID, err)
	}
}

func TestEditInPostParentModes(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	first, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-1", Prompt: "one", SourceAssetID: post.AssetIDs[0]})
	if err != nil {
		t.Fatal(err)
	}
	// parent: source → the response that made the source item, not the head.
	second, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-2", Prompt: "two", SourceAssetID: post.AssetIDs[0], Parent: "source"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ParentResponseID != post.ResponseID || second.ParentResponseID == first.ResponseID {
		t.Fatalf("source parent = %q, want the create's response %q", second.ParentResponseID, post.ResponseID)
	}
	// An explicit response id in the post.
	third, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-3", Prompt: "three", SourceAssetID: first.AssetIDs[0], Parent: first.ResponseID})
	if err != nil || third.ParentResponseID != first.ResponseID {
		t.Fatalf("explicit parent = %+v err=%v", third, err)
	}
	sends := h.fake.Sends()
	for _, input := range []EditInput{
		{JobID: "job-4", Prompt: "x", SourceAssetID: post.AssetIDs[0], Parent: "resp-0000-0000-4000-8000-999999999999"},
		{JobID: "job-5", Prompt: "x", SourceAssetID: "asset-not-in-post"},
	} {
		if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), input); errCode(err) != CodeItemNotInPost || errSend(err) != NotSent {
			t.Fatalf("%+v: err = %v, want item_not_in_post", input, err)
		}
	}
	if h.fake.Sends() != sends {
		t.Fatalf("refused edits sent %d times", h.fake.Sends()-sends)
	}
}

func TestEditInPostRefusesInFlightOrUnknownInFlight(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	h.fake.SetInflight(post.PostID, "resp-running")
	if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-a", Prompt: "x", SourceAssetID: post.AssetIDs[0]}); errCode(err) != CodePostInFlight {
		t.Fatalf("in flight: %v", err)
	}
	h.fake.SetInflight(post.PostID)
	h.fake.OmitInflight(post.PostID)
	if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-b", Prompt: "x", SourceAssetID: post.AssetIDs[0]}); errCode(err) != CodePostInFlight {
		t.Fatalf("missing inflightResponses: %v", err)
	}
	if h.fake.Sends() != 1 {
		t.Fatalf("sends = %d, want only the create", h.fake.Sends())
	}
}

func TestVideoInPostSpec(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	result, err := h.service.VideoInPost(h.ctx, h.caller, post.PostID, h.label(), VideoInput{JobID: "job-v", Prompt: "turn", SourceAssetID: post.AssetIDs[0], DurationS: 15, Resolution: "720p"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != "video" || result.DurationS != 15 || result.Resolution != "720p" || len(result.AssetIDs) == 0 || !result.ContextVerified {
		t.Fatalf("video result = %+v", result)
	}
	body := lastBody(t, h.fake, http.MethodPost, "/rest/app-chat/conversations/"+post.PostID+"/responses")
	video := body["mediaGenInput"].(map[string]any)["imageToVideo"].(map[string]any)
	if body["modelName"] != "imagine-video-gen" || video["duration"] != float64(15) || video["resolutionName"] != "720p" {
		t.Fatalf("video body = %v", body)
	}
	calls := len(h.transport.Calls())
	for _, input := range []VideoInput{
		{JobID: "job-6s", Prompt: "x", SourceAssetID: post.AssetIDs[0], DurationS: 6, Resolution: "720p"},
		{JobID: "job-480", Prompt: "x", SourceAssetID: post.AssetIDs[0], DurationS: 15, Resolution: "480p"},
	} {
		if _, err := h.service.VideoInPost(h.ctx, h.caller, post.PostID, h.label(), input); errCode(err) != CodeSpecUnsupported {
			t.Fatalf("%+v: %v", input, err)
		}
	}
	if len(h.transport.Calls()) != calls {
		t.Fatal("a refused spec reached the transport")
	}
}

func TestVideoInPostRefusesA6SecondAccount(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	h.fake.SetQuota(ssoA, fakegrok.Quota{MaxVideoSeconds: 6, Video720p: true})
	sends := h.fake.Sends()
	_, err := h.service.VideoInPost(h.ctx, h.caller, post.PostID, h.label(), VideoInput{JobID: "job-v", Prompt: "turn", SourceAssetID: post.AssetIDs[0], DurationS: 15, Resolution: "720p"})
	if errCode(err) != CodeSpecUnconfirmable || errSend(err) != NotSent || h.fake.Sends() != sends {
		t.Fatalf("6 s account: err = %v, sends %d → %d", err, sends, h.fake.Sends())
	}
	h.fake.SetQuota(ssoA, fakegrok.Quota{MaxVideoSeconds: 15})
	if _, err := h.service.VideoInPost(h.ctx, h.caller, post.PostID, h.label(), VideoInput{JobID: "job-v2", Prompt: "turn", SourceAssetID: post.AssetIDs[0], DurationS: 15, Resolution: "720p"}); errCode(err) != CodeSpecUnconfirmable {
		t.Fatalf("no 720p: %v", err)
	}
}

func TestListItemsKeepsIdsAndFactsOnly(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	snapshot, err := h.service.ListItems(h.ctx, h.caller, post.PostID, h.label())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PostID != post.PostID || snapshot.AccountLabel != h.label() || len(snapshot.Items) != 2 || !snapshot.InflightKnown || snapshot.HeadResponseID != post.ResponseID {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	data, _ := json.Marshal(snapshot)
	for _, banned := range []string{"anchor prompt", "message", "mediaGenInput", "https://"} {
		if strings.Contains(string(data), banned) {
			t.Fatalf("snapshot carries %q: %s", banned, data)
		}
	}
	if h.fake.Count(http.MethodGet, `/responses$`) != 1 {
		t.Fatal("want one free read")
	}
}

func TestSegmentsReturnMasksAsGiven(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	asset := post.AssetIDs[0]
	if _, err := h.service.Segments(h.ctx, h.caller, post.PostID, h.label(), asset); errCode(err) != CodeSegmentsNotCached || errSend(err) != NotSent {
		t.Fatalf("not cached: %v", err)
	}
	h.fake.SetSegments(asset, []fakegrok.Segment{
		{Name: "woman", Box: [4]float64{1, 2, 30, 40}, Score: 0.97, Size: [2]int{96, 64}, Counts: "PXa04b"},
		{Name: "small earring", Box: [4]float64{5, 6, 7, 8}, Score: 0.5, Size: [2]int{96, 64}, Counts: []int{10, 5, 6129}},
	})
	segments, err := h.service.Segments(h.ctx, h.caller, post.PostID, h.label(), asset)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments.Objects) != 2 || segments.Objects[0].Name != "woman" || string(segments.Objects[0].Mask.Counts) != `"PXa04b"` ||
		string(segments.Objects[1].Mask.Counts) != `[10,5,6129]` || segments.Objects[1].Mask.Size != [2]int{96, 64} || segments.Objects[0].Box != [4]float64{1, 2, 30, 40} {
		t.Fatalf("segments = %+v", segments)
	}
	body := lastBody(t, h.fake, http.MethodPost, "/rest/media/segment")
	if body["cachedOnly"] != true || body["maskFormat"] != "rle" || len(body) != 3 {
		t.Fatalf("segment body = %v", body)
	}
}

func TestSendFailureStates(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	edit := func(job string) error {
		_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: job, Prompt: "x", SourceAssetID: post.AssetIDs[0]})
		return err
	}
	h.fake.SetFault(fakegrok.FaultPeakHour)
	if err := edit("job-429"); errCode(err) != CodeRateLimited || errSend(err) != NotSent {
		t.Fatalf("peak hour: %v", err)
	}
	if len(h.pool.reports) != 1 || h.pool.reports[0] != http.StatusTooManyRequests {
		t.Fatalf("reports = %v, want one 429", h.pool.reports)
	}
	h.fake.SetFault(fakegrok.FaultOtherConversation)
	err := edit("job-other")
	var failure *Error
	if errCode(err) != CodePostContextLost || errSend(err) != Sent {
		t.Fatalf("other conversation: %v", err)
	}
	if !asError(err, &failure) || failure.Result == nil || failure.Result.ContextVerified || len(failure.Result.AssetIDs) == 0 {
		t.Fatalf("context lost keeps the item: %+v", failure)
	}
	h.fake.SetFault(fakegrok.FaultDropAfterStart)
	if err := edit("job-drop"); errCode(err) != CodeOutcomeUnknown || errSend(err) != Unknown {
		t.Fatalf("dropped stream: %v", err)
	}
	h.fake.SetStreamErrorText("blocked\x07 by policy")
	h.fake.SetFault(fakegrok.FaultModerated)
	err = edit("job-mod")
	if errCode(err) != CodeGenerationFailed || errSend(err) != Sent || !asError(err, &failure) || failure.ProviderText != "blocked  by policy" {
		t.Fatalf("moderated: %v (%+v)", err, failure)
	}
}

func asError(err error, target **Error) bool {
	value, ok := err.(*Error)
	if ok {
		*target = value
	}
	return ok
}

func TestPostGoneUpstreamIsPostNotFound(t *testing.T) {
	h := newDefaultHarness(t)
	if err := h.service.Store().PutOwner(h.ctx, PostOwner{ConversationID: "conv-gone", AccountID: accountA, ClientKeyID: keyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ListItems(h.ctx, h.caller, "conv-gone", h.label()); errCode(err) != CodePostNotFound {
		t.Fatalf("gone post: %v", err)
	}
	if _, err := h.service.ListItems(h.ctx, h.caller, "conv-never-pinned", h.label()); errCode(err) != CodePostNotFound {
		t.Fatalf("unknown post: %v", err)
	}
}

func TestReadItemAndMedia(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SetUserID(ssoA, "user-a")
	post := h.createPost("job-c")
	facts, err := h.service.ReadItem(h.ctx, h.caller, post.PostID, h.label(), post.AssetIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if facts.MIMEType != "image/jpeg" || facts.Width != 64 || facts.Height != 96 || !facts.InPost || facts.OwnedByPinned == nil || !*facts.OwnedByPinned {
		t.Fatalf("facts = %+v", facts)
	}
	data, _ := json.Marshal(facts)
	if strings.Contains(string(data), "users/") || strings.Contains(string(data), "user-a") {
		t.Fatalf("facts leak the key or owner: %s", data)
	}
	body, contentType, err := h.service.FetchMedia(h.ctx, h.caller, post.PostID, h.label(), post.AssetIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	media, _ := io.ReadAll(body)
	_ = body.Close()
	if contentType != "image/jpeg" || string(media) != "FAKE-MEDIA-"+post.AssetIDs[0] {
		t.Fatalf("media = %q %q", contentType, media)
	}
	if _, err := h.service.ReadItem(h.ctx, h.caller, post.PostID, h.label(), "asset-unknown"); errCode(err) != CodeItemNotInPost {
		t.Fatalf("unknown asset: %v", err)
	}
}

func TestOneSendPerAccountAtATime(t *testing.T) {
	h := newDefaultHarness(t)
	release, err := h.service.claimAccount(accountA)
	if err != nil {
		t.Fatal(err)
	}
	_, sendErr := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-busy", Prompt: "p", Image: pngImage()})
	if errCode(sendErr) != CodeAccountBusy || h.fake.Sends() != 0 {
		t.Fatalf("busy account: %v", sendErr)
	}
	release()
	h.createPost("job-after")
}

// A create whose stream drops after it named the conversation is
// outcome_unknown, and the post is still pinned to its account so a free
// read can settle it later.
func TestCreateDroppedAfterConversationStillPins(t *testing.T) {
	h := newDefaultHarness(t)
	h.fake.SetFault(fakegrok.FaultDropAfterStart)
	_, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: "job-drop", Prompt: "p", Image: pngImage()})
	var failure *Error
	if errCode(err) != CodeOutcomeUnknown || errSend(err) != Unknown || !asError(err, &failure) || failure.Result == nil || failure.Result.PostID == "" {
		t.Fatalf("dropped create: %v (%+v)", err, failure)
	}
	owner, ownerErr := h.service.Store().Owner(h.ctx, failure.Result.PostID)
	if ownerErr != nil || owner.AccountID != accountA {
		t.Fatalf("owner = %+v err = %v, want pinned to account A", owner, ownerErr)
	}
	if _, err := h.service.ListItems(h.ctx, h.caller, failure.Result.PostID, failure.Result.AccountLabel); err != nil {
		t.Fatalf("free read after an unknown create: %v", err)
	}
}
