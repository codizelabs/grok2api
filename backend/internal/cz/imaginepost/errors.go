package imaginepost

import (
	"fmt"
	"net/http"
	"time"
)

// SendState says whether grok.com may have received a send. It follows
// generation-v1's send_state values.
type SendState string

const (
	// NotSent means nothing that could cost a generation left grok2api.
	NotSent SendState = "not_sent"
	// Sent means the send reached grok.com and was answered.
	Sent SendState = "sent"
	// Unknown means the send may have reached grok.com; never retry it.
	Unknown SendState = "unknown"
)

// Error codes. Messages are written by P3, never copied from grok.com.
const (
	CodeCallNotAllowed           = "call_not_allowed"
	CodeStatsigUnavailable       = "statsig_unavailable"
	CodePinnedAccountUnavailable = "upstream_pinned_account_unavailable"
	CodeAccountLabelRequired     = "account_label_required"
	CodeAccountLabelMismatch     = "account_label_mismatch"
	CodeAccountBusy              = "account_busy"
	CodeNoAccount                = "account_unavailable"
	CodePostNotFound             = "post_not_found"
	CodeItemNotInPost            = "item_not_in_post"
	CodePostInFlight             = "post_in_flight"
	CodeJobIDRequired            = "job_id_required"
	CodeJobIDReused              = "job_id_reused"
	CodeInvalidRequest           = "invalid_request"
	CodeSpecUnsupported          = "spec_unsupported"
	CodeSpecUnconfirmable        = "spec_unconfirmable"
	CodeLikeDisabled             = "like_disabled"
	CodeCapabilityUnsupported    = "capability_unsupported"
	CodeSegmentsNotCached        = "segments_not_cached"
	CodeUpstreamRejected         = "upstream_rejected"
	CodeRateLimited              = "rate_limited"
	CodeGenerationFailed         = "generation_failed"
	CodePostContextLost          = "post_context_lost"
	CodeAccountMismatch          = "account_mismatch"
	CodeOutcomeUnknown           = "outcome_unknown"
	CodeProviderRefused          = "provider_refused"
	CodeUpstreamUnreadable       = "upstream_unreadable"
	CodeInternal                 = "internal_error"
)

// LayerCodePostAccountUnavailable is the code the generation layer shows for
// CodePinnedAccountUnavailable on a post job (contract delta § 4 rule 3).
const LayerCodePostAccountUnavailable = "post_account_unavailable"

// Error is every P3 failure. It never carries upstream text, prompts, ids or
// URLs: Message is fixed per code.
type Error struct {
	Code       string
	Status     int
	Send       SendState
	RetryAfter time.Duration
	// UpstreamStatus is grok.com's HTTP status, when it answered.
	UpstreamStatus int
	// BodySHA256 is a prefix hash of a rejected upstream body, never the body.
	BodySHA256 string
	// ProviderText is capped, control-stripped provider free text (a
	// moderation reason), returned only in its own field (sec-rowan B10).
	ProviderText string
	// Result carries what a sent-but-failed call produced (post_context_lost).
	Result *SendResult
	// replyUnrecognised marks an outcome_unknown whose send was answered with
	// a 200 stream P3 could not read, so an in-post send re-reads the post.
	replyUnrecognised bool
}

func (e *Error) Error() string {
	if e == nil {
		return "imaginepost: <nil>"
	}
	return fmt.Sprintf("imaginepost: %s (%s)", e.Code, e.Send)
}

// Message is the fixed, P3-written text for a code.
func (e *Error) Message() string {
	if e == nil {
		return ""
	}
	if text, ok := messages[e.Code]; ok {
		return text
	}
	return "the imagine post call failed"
}

var messages = map[string]string{
	CodeCallNotAllowed:           "the grok.com call is not on P3's allow-list; nothing was sent",
	CodeStatsigUnavailable:       "no valid Statsig signature is available; nothing was sent",
	CodePinnedAccountUnavailable: "the post's pinned account can't take the call; no other account is tried",
	CodeAccountLabelRequired:     "the post's account_label is required",
	CodeAccountLabelMismatch:     "the account_label does not match the post's pinned account",
	CodeAccountBusy:              "the pinned account already has a send in flight",
	CodeNoAccount:                "no eligible account can create a post",
	CodePostNotFound:             "the post is not known to this key, or grok.com no longer has it",
	CodeItemNotInPost:            "the item is not in this post",
	CodePostInFlight:             "the post has a response in flight, or its in-flight state is unknown",
	CodeJobIDRequired:            "job_id is required on every send",
	CodeJobIDReused:              "this job_id was already used; P3 never sends twice for one job",
	CodeInvalidRequest:           "the request is invalid",
	CodeSpecUnsupported:          "only 15 s at 720p is supported",
	CodeSpecUnconfirmable:        "the pinned account's free quota read does not confirm 15 s at 720p",
	CodeLikeDisabled:             "like and unlike are disabled (cz.imaginePost.allowLike)",
	CodeCapabilityUnsupported:    "the operation is not supported by P3 yet",
	CodeSegmentsNotCached:        "grok.com has no cached segments for this item; P3 only reads cachedOnly",
	CodeUpstreamRejected:         "grok.com refused the call",
	CodeRateLimited:              "grok.com rate-limited the call",
	CodeGenerationFailed:         "grok.com ended the generation without a result",
	CodePostContextLost:          "the result came back outside the post's conversation",
	CodeAccountMismatch:          "the result belongs to another account than the post's pinned one",
	CodeOutcomeUnknown:           "the send may have reached grok.com; do not retry it",
	CodeProviderRefused:          "grok.com answered the send without an image (a refusal); rebuild the prompt before a new send",
	CodeUpstreamUnreadable:       "grok.com's answer could not be read",
	CodeInternal:                 "an internal error stopped the call",
}

func fail(code string, status int, send SendState) *Error {
	return &Error{Code: code, Status: status, Send: send}
}

func notSent(code string, status int) *Error { return fail(code, status, NotSent) }

func errCallNotAllowed() *Error { return notSent(CodeCallNotAllowed, http.StatusForbidden) }
