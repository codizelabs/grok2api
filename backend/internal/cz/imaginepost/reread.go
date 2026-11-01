package imaginepost

// One free re-read after an unrecognised reply (WP08a-T5). On 1 Nov 2026
// the first real in-post edit succeeded on grok.com, but P3 didn't recognise
// Grok's reply and answered outcome_unknown; a free re-read by hand showed
// the new request and Grok's reply with the image 14 s later. P3 now makes
// that one re-read itself.

import (
	"context"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// reReadClockSkew is how far grok.com's createTime may run behind P3's clock
// for an item P3 still counts as made after the send.
const reReadClockSkew = 2 * time.Minute

type reReadOutcome int

const (
	reReadUnknown reReadOutcome = iota
	reReadSucceeded
	reReadRefused
)

// verifyByReRead makes ONE free read of the post after a send whose reply
// wasn't recognised. It succeeds only when exactly one new human item (not in
// the pre-send read, made after the send, on the parent P3 sent) appears with
// exactly one model item answering it and nothing in flight: with assets that
// is the result; with none, Grok refused (provider_refused). Anything else is
// unknown, and the caller keeps outcome_unknown.
func (s *Service) verifyByReRead(ctx context.Context, credential account.Credential, op, conversationID string, before PostSnapshot, parent string, sentAt time.Time) (streamResult, reReadOutcome) {
	after, err := s.readPost(context.WithoutCancel(ctx), credential, conversationID)
	if err != nil || !after.InflightKnown || after.InflightCount > 0 {
		return streamResult{}, reReadUnknown
	}
	known := make(map[string]bool, len(before.Items))
	for _, item := range before.Items {
		known[item.ResponseID] = true
	}
	var humans, models []PostItem
	for _, item := range after.Items {
		if known[item.ResponseID] {
			continue
		}
		if item.Sender == "human" {
			humans = append(humans, item)
		} else {
			models = append(models, item)
		}
	}
	if len(humans) != 1 {
		return streamResult{}, reReadUnknown
	}
	human := humans[0]
	if human.ParentResponseID != "" && human.ParentResponseID != parent {
		return streamResult{}, reReadUnknown
	}
	if at, parseErr := time.Parse(time.RFC3339Nano, human.CreateTime); parseErr == nil && at.Before(sentAt.Add(-reReadClockSkew)) {
		return streamResult{}, reReadUnknown
	}
	var answer []PostItem
	for _, item := range models {
		if item.ParentResponseID == human.ResponseID || (item.ParentResponseID == "" && len(models) == 1) {
			answer = append(answer, item)
		}
	}
	if len(answer) != 1 {
		return streamResult{}, reReadUnknown
	}
	model := answer[0]
	if len(model.Assets) == 0 {
		return streamResult{HumanResponseID: human.ResponseID, ResponseID: model.ResponseID, Final: true}, reReadRefused
	}
	result := streamResult{ConversationIDs: []string{conversationID}, HumanResponseID: human.ResponseID, ResponseID: model.ResponseID, Final: true}
	for _, asset := range model.Assets {
		result.addAsset(asset.AssetID)
	}
	return result, reReadSucceeded
}

// settleUnrecognised runs the re-read for an in-post send whose reply P3
// didn't recognise (sendErr marked replyUnrecognised, or a recognised final
// with no asset). It returns the stream to continue with, or the error to
// answer.
func (s *Service) settleUnrecognised(ctx context.Context, credential account.Credential, op, conversationID string, before PostSnapshot, parent string, sentAt time.Time, partial streamResult, sendErr *Error) (streamResult, *Error) {
	fallback := sendErr
	if fallback == nil {
		fallback = fail(CodeGenerationFailed, http.StatusBadGateway, Sent)
	}
	verified, outcome := s.verifyByReRead(ctx, credential, op, conversationID, before, parent, sentAt)
	path := listResponsesPath(conversationID)
	switch outcome {
	case reReadSucceeded:
		// Keep every conversation the partial stream named, so the
		// post-context check still sees a foreign one.
		verified.ConversationIDs = append(append([]string{}, partial.ConversationIDs...), verified.ConversationIDs...)
		s.record(ctx, "reread", conversationID, credential, http.MethodGet, path, nil)
		s.logger.Info("cz_imagine_reply_unrecognised", "op", op, "post", s.labels.Ref(conversationID), "account", s.labels.AccountLabel(credential.ID), "outcome", "verified_by_reread")
		return verified, nil
	case reReadRefused:
		refused := &Error{Code: CodeProviderRefused, Status: http.StatusBadGateway, Send: Sent}
		s.record(ctx, "reread", conversationID, credential, http.MethodGet, path, refused)
		s.logger.Info("cz_imagine_reply_unrecognised", "op", op, "post", s.labels.Ref(conversationID), "account", s.labels.AccountLabel(credential.ID), "outcome", CodeProviderRefused)
		return partial, refused
	}
	s.record(ctx, "reread", conversationID, credential, http.MethodGet, path, fallback)
	return partial, fallback
}

// replyNeedsReRead says whether an in-post send's reply went unrecognised:
// a 200 stream P3 could not read, or a recognised final with no asset.
func (s *Service) replyNeedsReRead(stream streamResult, sendErr *Error) bool {
	if sendErr != nil {
		return sendErr.replyUnrecognised
	}
	return len(stream.AssetIDs) == 0
}
