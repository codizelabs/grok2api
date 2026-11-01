package imaginepost

// The re-read guards (review of WP08a-T5): after an unrecognised in-post
// reply, the one free re-read counts as a verified result only when exactly
// one new human item, on the parent P3 sent, made after the send, has one
// model answer and nothing is in flight. Each case below breaks one of those
// and must stay outcome_unknown, with one send and one re-read.

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
)

func TestReReadGuardsKeepOutcomeUnknown(t *testing.T) {
	cases := []struct {
		name  string
		twist fakegrok.Twist
	}{
		{"two new human items", fakegrok.TwistTwoHumans},
		{"human item on another parent", fakegrok.TwistOtherParent},
		{"human item with no parent", fakegrok.TwistNoParent},
		{"a response still in flight", fakegrok.TwistInflight},
		{"human item older than the send", fakegrok.TwistOldItem},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newDefaultHarness(t)
			post := h.createPost("job-c")
			readsBefore, sendsBefore := h.listReads(post.PostID), h.fake.Sends()
			h.fake.SetFault(fakegrok.FaultUnrecognisedReply)
			h.fake.SetTwist(c.twist)
			result, err := h.service.EditInPost(h.ctx, h.caller, post.PostID, h.label(), EditInput{JobID: "job-g", Prompt: "eyes closed", SourceAssetID: post.AssetIDs[0]})
			if errCode(err) != CodeOutcomeUnknown || errSend(err) != Unknown || result != nil {
				t.Fatalf("%s: result %+v err %v, want outcome_unknown", c.name, result, err)
			}
			if sends := h.fake.Sends() - sendsBefore; sends != 1 {
				t.Fatalf("sends = %d, want 1", sends)
			}
			if reads := h.listReads(post.PostID) - readsBefore; reads != 2 {
				t.Fatalf("list reads = %d, want 2: the pre-send read and one re-read", reads)
			}
		})
	}
}
