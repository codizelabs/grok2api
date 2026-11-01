package imaginepost

// T5 (WP08a): the first real in-post edit (1 Nov 2026) succeeded on grok.com
// but P3 didn't recognise the reply and answered outcome_unknown. After an
// in-post send whose reply isn't recognised, P3 makes one free re-read of
// the post: a new human item followed by a model item with assets is a
// verified success; a model item with no assets is provider_refused; anything
// else stays outcome_unknown. An opt-in debug capture records the reply's key
// structure only.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
)

func (h *harness) listReads(postID string) int {
	return h.fake.Count(http.MethodGet, "^/rest/app-chat/conversations/"+postID+"/responses$")
}

func (h *harness) jobState(jobID string) string {
	h.t.Helper()
	var state string
	if err := h.db.Raw("SELECT state FROM cz_imagine_send_key WHERE client_key_id = ? AND job_id = ?", keyID, jobID).Scan(&state).Error; err != nil {
		h.t.Fatalf("job state: %v", err)
	}
	return state
}

func TestUnrecognisedReplyVerifiedByOneFreeReRead(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	readsBefore, sendsBefore := h.listReads(post.PostID), h.fake.Sends()
	h.fake.SetFault(fakegrok.FaultUnrecognisedReply)
	result, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-e", Prompt: "eyes closed", SourceAssetID: post.AssetIDs[0]})
	if err != nil {
		t.Fatalf("unrecognised reply of a successful send: %v, want a verified success", err)
	}
	if h.fake.Sends() != sendsBefore+1 {
		t.Fatalf("sends = %d, want exactly one more (never a resend)", h.fake.Sends()-sendsBefore)
	}
	if reads := h.listReads(post.PostID) - readsBefore; reads != 2 {
		t.Fatalf("list reads during the edit = %d, want 2 (the pre-send read and one re-read)", reads)
	}
	after, err := h.service.ListItems(h.ctx, h.caller, post.PostID, h.label())
	if err != nil {
		t.Fatal(err)
	}
	var model PostItem
	for _, item := range after.Items {
		if item.ResponseID == after.HeadResponseID {
			model = item
		}
	}
	if model.Sender == "human" || len(model.Assets) != 1 {
		t.Fatalf("post head after the edit = %+v, want a model item with one asset", model)
	}
	if !result.ContextVerified || result.ResponseID != model.ResponseID || len(result.AssetIDs) != 1 || result.AssetIDs[0] != model.Assets[0].AssetID ||
		result.Kind != "image" || result.PostID != post.PostID || result.SourceAssetID != post.AssetIDs[0] {
		t.Fatalf("result = %+v, want the re-read model item %+v, context_verified", result, model)
	}
	if state := h.jobState("job-e"); state != string(Sent) {
		t.Fatalf("job state = %q, want sent", state)
	}
}

func TestUnrecognisedVideoReplyVerifiedByReRead(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	h.fake.SetFault(fakegrok.FaultUnrecognisedReply)
	result, err := h.service.VideoInPost(h.ctx, h.caller, post.PostID, h.label(), VideoInput{JobID: "job-v", Prompt: "turn", SourceAssetID: post.AssetIDs[0], DurationS: 15, Resolution: "720p"})
	if err != nil {
		t.Fatalf("unrecognised video reply: %v", err)
	}
	if result.Kind != "video" || len(result.AssetIDs) != 1 || !result.ContextVerified || result.DurationS != 15 {
		t.Fatalf("video result = %+v", result)
	}
}

func TestUnrecognisedReplyWithNoAssetIsProviderRefused(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	h.fake.SetFault(fakegrok.FaultRefusedUnrecognised)
	_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-r", Prompt: "eyes closed", SourceAssetID: post.AssetIDs[0]})
	var failure *Error
	if errCode(err) != CodeProviderRefused || errSend(err) != Sent || !asError(err, &failure) || failure.Status != http.StatusBadGateway {
		t.Fatalf("refused send: %v, want provider_refused, sent, 502", err)
	}
	if failure.Result != nil {
		t.Fatalf("refused send carries a result %+v, want none (no image)", failure.Result)
	}
	if failure.Message() == "" || failure.Message() == (&Error{Code: "unknown-code"}).Message() {
		t.Fatalf("provider_refused has no message of its own")
	}
	if state := h.jobState("job-r"); state != string(Sent) {
		t.Fatalf("job state = %q, want sent", state)
	}
}

func TestUnrecognisedReplyWithNoModelItemStaysOutcomeUnknown(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	readsBefore := h.listReads(post.PostID)
	h.fake.SetFault(fakegrok.FaultUnrecognisedNoResult)
	_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-u", Prompt: "eyes closed", SourceAssetID: post.AssetIDs[0]})
	if errCode(err) != CodeOutcomeUnknown || errSend(err) != Unknown {
		t.Fatalf("no model item: %v, want outcome_unknown", err)
	}
	if reads := h.listReads(post.PostID) - readsBefore; reads != 2 {
		t.Fatalf("list reads = %d, want 2 (one re-read only)", reads)
	}
}

// A dropped stream whose post shows nothing new after the send stays unknown.
func TestDroppedStreamWithNothingNewStaysOutcomeUnknown(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	h.fake.SetFault(fakegrok.FaultDropAfterStart)
	_, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-d", Prompt: "x", SourceAssetID: post.AssetIDs[0]})
	if errCode(err) != CodeOutcomeUnknown || errSend(err) != Unknown {
		t.Fatalf("dropped stream: %v, want outcome_unknown", err)
	}
}

// Debug capture: off by default, writes nothing.
func TestReplyShapeCaptureOffByDefault(t *testing.T) {
	h := newDefaultHarness(t)
	post := h.createPost("job-c")
	h.fake.SetFault(fakegrok.FaultUnrecognisedReply)
	if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-e", Prompt: "x", SourceAssetID: post.AssetIDs[0]}); err != nil {
		t.Fatal(err)
	}
	if h.service.cfg().DebugReplyShapeFile != "" {
		t.Fatalf("capture file configured by default: %q", h.service.cfg().DebugReplyShapeFile)
	}
}

// Debug capture on: one JSON line per send with the reply's key structure,
// never a value, prompt, URL or id.
func TestReplyShapeCaptureRecordsKeysOnly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "shapes", "reply-shapes.jsonl")
	h := newHarness(t, validStatsig, Config{DebugReplyShapeFile: file})
	post := h.createPost("job-c")
	h.fake.SetFault(fakegrok.FaultUnrecognisedReply)
	result, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-e", Prompt: "secret prompt words", SourceAssetID: post.AssetIDs[0]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-e2", Prompt: "secret prompt words", SourceAssetID: post.AssetIDs[0]}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("capture file: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("capture file mode = %v, want owner-only", mode)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, banned := range []string{"secret prompt", "https", "assets.grok.com", post.PostID, post.AssetIDs[0], result.AssetIDs[0], result.ResponseID, "imagine_progress", "imagine_done", "acct-a"} {
		if strings.Contains(text, banned) {
			t.Fatalf("capture holds %q, want key structure only:\n%s", banned, text)
		}
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 3 {
		t.Fatalf("capture has %d lines, want 3 (the create and both edits):\n%s", len(lines), text)
	}
	var unrecognised, recognised map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &unrecognised); err != nil {
		t.Fatalf("capture line is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &recognised); err != nil {
		t.Fatalf("capture line is not JSON: %v", err)
	}
	if unrecognised["op"] != "edit" || unrecognised["recognised"] != false || unrecognised["http_status"] != float64(200) {
		t.Fatalf("unrecognised capture = %v", unrecognised)
	}
	if recognised["recognised"] != true {
		t.Fatalf("recognised capture = %v", recognised)
	}
	for _, key := range []string{"result", "event", "type", "progress"} {
		if !strings.Contains(lines[1], "\""+key+"\"") {
			t.Fatalf("unrecognised capture lacks key %q: %s", key, lines[1])
		}
	}
	for _, key := range []string{"modelResponse", "fileAttachments", "streamingImageGenerationResponse"} {
		if !strings.Contains(lines[2], "\""+key+"\"") {
			t.Fatalf("recognised capture lacks key %q: %s", key, lines[2])
		}
	}
}

// Keys that look like ids (a map keyed by asset or conversation id) are
// replaced, and values never appear.
func TestReplyShapeHidesIDLikeKeysAndValues(t *testing.T) {
	line := []byte("{\"result\":{\"byId\":{\"0f9c2a7e-1b2c-4d5e-8f90-123456789abc\":{\"url\":\"https://x/y\"}},\"deadbeefcafe1234\":1,\"list\":[{\"n\":2},{\"n\":3}],\"flag\":true,\"none\":null}}")
	shape, ok := replyLineShape(line)
	if !ok {
		t.Fatal("a JSON line was not shaped")
	}
	encoded, err := json.Marshal(shape)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, banned := range []string{"0f9c2a7e", "deadbeef", "https", "true", "\"2\""} {
		if strings.Contains(text, banned) {
			t.Fatalf("shape holds %q: %s", banned, text)
		}
	}
	for _, kept := range []string{"result", "byId", "url", "list", "flag", "none"} {
		if !strings.Contains(text, "\""+kept+"\"") {
			t.Fatalf("shape lacks key %q: %s", kept, text)
		}
	}
	if _, ok := replyLineShape([]byte("not json")); ok {
		t.Fatal("a non-JSON line was shaped")
	}
}
