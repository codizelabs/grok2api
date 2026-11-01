package fakegrok

// PROVISIONAL WIRE SHAPES (U1, U2).
//
// Every grok.com shape in this file comes from grok.com's public client code
// (research/grok-imagine-posts-api-2026-11.md in AIFriend-gen, rows 5-9 and
// 13), never from a recorded send. Spike S-P (ADR-0002 § 7) records the real
// add-response body and stream; when it does, correct this file and
// ../wire_provisional.go together, and nothing else.
//
// What is modelled here:
//   - the NDJSON stream lines of conversations/new and of add-response
//     (POST /rest/app-chat/conversations/{id}/responses);
//   - where the new conversation, response and asset ids appear in it;
//   - the video progress frames (streamingVideoGenerationResponse).
//
// Observed (not provisional): GET .../responses keys, GET /rest/assets/{id}
// keys, POST /rest/media/segment request and answer keys, quota_info
// categories.

import (
	"encoding/json"
	"fmt"
)

// streamLine encodes one NDJSON line.
func streamLine(value any) []byte {
	data, _ := json.Marshal(value)
	return append(data, '\n')
}

// createStreamLines is the stream of a successful conversations/new imagine
// edit: the new conversation, the human turn, image progress, then the model
// response that carries the result asset. PROVISIONAL (U1).
func createStreamLines(conversationID, humanResponseID, modelResponseID, assetID, assetURL, title string) [][]byte {
	return [][]byte{
		streamLine(map[string]any{"result": map[string]any{"conversation": map[string]any{
			"conversationId": conversationID, "title": title, "kind": "CONVERSATION_KIND_IMAGINE",
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"responseId": humanResponseID, "sender": "human",
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"streamingImageGenerationResponse": map[string]any{"progress": 50, "imageId": assetID},
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"streamingImageGenerationResponse": map[string]any{"progress": 100, "imageId": assetID, "imageUrl": assetURL},
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"modelResponse": map[string]any{
				"responseId": modelResponseID, "parentResponseId": humanResponseID, "sender": "assistant",
				"generatedImageUrls": []string{assetURL}, "fileAttachments": []string{assetID},
			},
		}}}),
		streamLine(map[string]any{"result": map[string]any{"title": map[string]any{"newTitle": title}}}),
	}
}

// addResponseImageLines is the stream of an edit inside a post.
// PROVISIONAL (U1): whether the stream repeats the conversation id is
// unknown, so the fake puts it on the model response only; the service also
// checks the new asset's conversation with a free read.
func addResponseImageLines(conversationID, humanResponseID, modelResponseID, assetID, assetURL string) [][]byte {
	return [][]byte{
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"responseId": humanResponseID, "sender": "human",
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"streamingImageGenerationResponse": map[string]any{"progress": 100, "imageId": assetID, "imageUrl": assetURL},
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"modelResponse": map[string]any{
				"responseId": modelResponseID, "parentResponseId": humanResponseID, "sender": "assistant",
				"conversationId": conversationID, "fileAttachments": []string{assetID},
			},
		}}}),
	}
}

// addResponseVideoLines is the stream of a video inside a post.
// PROVISIONAL (U2): the frame names follow grok2api's own reader
// (infra/provider/web/video.go:378-391).
func addResponseVideoLines(conversationID, humanResponseID, modelResponseID, videoID, videoURL string) [][]byte {
	return [][]byte{
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"responseId": humanResponseID, "sender": "human",
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"streamingVideoGenerationResponse": map[string]any{"progress": 40, "videoId": videoID},
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"streamingVideoGenerationResponse": map[string]any{"progress": 100, "videoId": videoID, "videoPostId": videoID, "videoUrl": videoURL},
		}}}),
		streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
			"modelResponse": map[string]any{
				"responseId": modelResponseID, "parentResponseId": humanResponseID, "sender": "assistant",
				"conversationId": conversationID, "fileAttachments": []string{videoID},
			},
		}}}),
	}
}

// streamErrorLine is a refusal or moderation stop inside a stream.
// PROVISIONAL: research row "streamError".
func streamErrorLine(text string) []byte {
	return streamLine(map[string]any{"result": map[string]any{"response": map[string]any{
		"streamError": map[string]any{"code": "moderated", "message": text},
	}}})
}

// requiredSendKeys are the request keys the fake insists on, per operation.
// PROVISIONAL (U1, U2): from the client code, never seen on the wire.
var requiredSendKeys = map[string][]string{
	"create": {"modelName", "message", "mediaGenInput", "kind"},
	"edit":   {"modelName", "message", "mediaGenInput", "parentResponseId"},
	"video":  {"modelName", "message", "mediaGenInput", "parentResponseId"},
}

func missingKeys(body map[string]any, op string) error {
	for _, key := range requiredSendKeys[op] {
		if _, ok := body[key]; !ok {
			return fmt.Errorf("missing %s", key)
		}
	}
	return nil
}

// unrecognisedReplyLines stands in for the real add-response reply that P3's
// parser didn't recognise on 1 Nov 2026 (the first real in-post edit:
// grok.com made the image, P3 answered outcome_unknown). These lines are
// INVENTED, not recorded: they only have to carry nothing the parser reads
// (no conversation, response, modelResponse or streaming frame), so the
// service falls back to its one free re-read. Replace them with the shape the
// debug capture (Config.DebugReplyShapeFile) records on the next real send.
func unrecognisedReplyLines() [][]byte {
	return [][]byte{
		streamLine(map[string]any{"result": map[string]any{"event": map[string]any{"type": "imagine_progress", "progress": 50}}}),
		streamLine(map[string]any{"result": map[string]any{"event": map[string]any{"type": "imagine_progress", "progress": 100}}}),
		streamLine(map[string]any{"result": map[string]any{"event": map[string]any{"type": "imagine_done"}}}),
	}
}
