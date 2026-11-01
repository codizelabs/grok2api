package imaginepost

// The method + path allow-list (ADR-0002 § 1, sec-rowan B2). Every grok.com
// call P3 makes passes CheckCall first; anything not listed here returns
// call_not_allowed before the transport is touched, so it makes zero
// network calls. Denied on purpose: post/create, assets/{id}/clone,
// media/conversation/create, stop-inflight-responses, reconnect-response-v2,
// video/upscale and image upscale, segment with cachedOnly other than true,
// wss://grok.com/ws/bot/*, anything on cdn.grok.com, and like/unlike unless
// cz.imaginePost.allowLike is true.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
)

// Call is one grok.com call P3 wants to make. Path is a grok.com path
// ("/rest/…", "/http/upload-file-v2/direct", optionally with a query) or,
// for item media, an absolute https://assets.grok.com/users/… URL.
type Call struct {
	Method string
	Path   string
	Body   []byte
}

// Policy holds the switches that open allow-list entries.
type Policy struct {
	AllowLike bool
}

const (
	pathCreate   = "/rest/app-chat/conversations/new"
	pathUpload   = "/http/upload-file-v2/direct"
	pathSegment  = "/rest/media/segment"
	pathQuota    = "/rest/media/imagine/quota_info"
	pathLike     = "/rest/media/post/like"
	pathUnlike   = "/rest/media/post/unlike"
	assetsHost   = "assets.grok.com"
	idPattern    = `[A-Za-z0-9-]{1,128}`
	listKindJSON = "conversationKind=CONVERSATION_KIND_IMAGINE"
)

var (
	responsesPath  = regexp.MustCompile(`^/rest/app-chat/conversations/` + idPattern + `/responses$`)
	assetRecord    = regexp.MustCompile(`^/rest/assets/` + idPattern + `$`)
	assetMediaPath = regexp.MustCompile(`^/users/[A-Za-z0-9._/-]{1,400}$`)
	validID        = regexp.MustCompile(`^` + idPattern + `$`)
)

// CheckCall returns nil only for a call on the allow-list.
func CheckCall(policy Policy, call Call) error {
	parsed, err := url.Parse(call.Path)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return errCallNotAllowed()
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		if call.Method == http.MethodGet && parsed.Scheme == "https" && parsed.Host == assetsHost &&
			parsed.RawQuery == "" && assetMediaPath.MatchString(parsed.Path) && !bytes.Contains([]byte(parsed.Path), []byte("..")) && len(call.Body) == 0 {
			return nil
		}
		return errCallNotAllowed()
	}
	path, query := parsed.Path, parsed.RawQuery
	if bytes.Contains([]byte(call.Path), []byte("..")) || bytes.Contains([]byte(call.Path), []byte("//")) {
		return errCallNotAllowed()
	}
	switch call.Method {
	case http.MethodGet:
		if len(call.Body) != 0 {
			return errCallNotAllowed()
		}
		switch {
		case responsesPath.MatchString(path) && (query == "" || query == listKindJSON):
			return nil
		case assetRecord.MatchString(path) && query == "":
			return nil
		}
	case http.MethodPost:
		if query != "" {
			return errCallNotAllowed()
		}
		switch {
		case path == pathCreate || responsesPath.MatchString(path):
			return checkSendBody(call.Body)
		case path == pathUpload:
			if len(call.Body) != 0 {
				return errCallNotAllowed()
			}
			return nil
		case path == pathSegment:
			return checkSegmentBody(call.Body)
		case path == pathQuota:
			if len(call.Body) == 0 || string(call.Body) == "{}" {
				return nil
			}
		case path == pathLike || path == pathUnlike:
			if policy.AllowLike {
				return nil
			}
		}
	}
	return errCallNotAllowed()
}

// checkSendBody admits only a JSON object that never asks for publishing.
func checkSendBody(body []byte) error {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil || value == nil {
		return errCallNotAllowed()
	}
	if containsKey(value, "makePublic") {
		return errCallNotAllowed()
	}
	return nil
}

func containsKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if name == key || containsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsKey(child, key) {
				return true
			}
		}
	}
	return false
}

// checkSegmentBody admits exactly {assetId, cachedOnly: true, maskFormat: "rle"}
// (sec-rowan B6: cachedOnly false is a send until U4 is answered).
func checkSegmentBody(body []byte) error {
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil || len(value) != 3 {
		return errCallNotAllowed()
	}
	var assetID, format string
	var cachedOnly bool
	if json.Unmarshal(value["assetId"], &assetID) != nil || !validID.MatchString(assetID) ||
		json.Unmarshal(value["maskFormat"], &format) != nil || format != "rle" ||
		json.Unmarshal(value["cachedOnly"], &cachedOnly) != nil || !cachedOnly {
		return errCallNotAllowed()
	}
	return nil
}
