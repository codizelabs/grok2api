package web

import "testing"

// A photo-to-photo edit must be created as an Imagine conversation, as grok.com's own Imagine page does
// (kind CONVERSATION_KIND_IMAGINE and the imagine edit model). Without kind, grok.com files it as an ordinary
// chat and it shows in the account's chat history (owner report, 2 Nov 2026).
func TestImageEditPayloadIsImagineConversation(t *testing.T) {
	payload := buildImageEditPayload("p", []string{"a"}, "")
	if got := payload["kind"]; got != "CONVERSATION_KIND_IMAGINE" {
		t.Fatalf("kind = %v, want CONVERSATION_KIND_IMAGINE", got)
	}
	meta, _ := payload["responseMetadata"].(map[string]any)
	override, _ := meta["modelConfigOverride"].(map[string]any)
	modelMap, _ := override["modelMap"].(map[string]any)
	if got := modelMap["imageEditModel"]; got != "imagine" {
		t.Fatalf("responseMetadata.modelConfigOverride.modelMap.imageEditModel = %v, want imagine", got)
	}
}
