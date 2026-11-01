package imaginepost

// PROVISIONAL WIRE SHAPES (U1, U2): the request bodies P3 sends and the
// stream it reads. Every send-side key here is read from grok.com's public
// client code (research/grok-imagine-posts-api-2026-11.md rows 5-9 in
// AIFriend-gen), never seen on the wire. Spike S-P (ADR-0002 § 7) records the
// real shapes; correct this file and fakegrok/wire_provisional.go together.
// The read-side parsers below (responses list, asset, segment, quota) follow
// observed key names and are not provisional, except where marked.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Video spec P3 sends: the only values the contract allows.
const (
	videoDurationSeconds = 15
	videoResolution      = "720p"
)

func marshal(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// buildCreateBody: POST /rest/app-chat/conversations/new for an imagine edit
// from uploaded references. PROVISIONAL (U1); matches grok2api's own
// buildImageEditPayload plus kind and responseMetadata from the client code.
func buildCreateBody(prompt string, inputAssets []string, aspectRatio string) []byte {
	imageToImage := map[string]any{"prompt": prompt, "inputAssets": inputAssets}
	if aspectRatio != "" {
		imageToImage["aspectRatio"] = aspectRatio
	}
	return marshal(map[string]any{
		"modelName": "imagine-image-edit", "message": prompt,
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"kind":          "CONVERSATION_KIND_IMAGINE",
		"mediaGenInput": map[string]any{"imageToImage": imageToImage},
		"responseMetadata": map[string]any{"modelConfigOverride": map[string]any{
			"modelMap": map[string]any{"imageEditModel": "imagine"},
		}},
	})
}

// buildEditBody: add-response for an edit inside the post. PROVISIONAL (U1).
func buildEditBody(prompt, parentResponseID string, inputAssets []string, aspectRatio string) []byte {
	imageToImage := map[string]any{"prompt": prompt, "inputAssets": inputAssets}
	if aspectRatio != "" {
		imageToImage["aspectRatio"] = aspectRatio
	}
	return marshal(map[string]any{
		"modelName": "imagine-image-edit", "message": prompt,
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"parentResponseId":                  parentResponseID,
		"skipCancelCurrentInflightRequests": true,
		"mediaGenInput":                     map[string]any{"imageToImage": imageToImage},
	})
}

// buildVideoBody: add-response for a video inside the post from one image
// item. PROVISIONAL (U2): whether duration and resolution also belong in
// message is unknown; P3 puts them in imageToVideo only.
func buildVideoBody(prompt, parentResponseID, inputAsset, aspectRatio, mode string) []byte {
	if mode == "" {
		mode = "normal"
	}
	imageToVideo := map[string]any{
		"prompt": prompt, "inputAssets": []string{inputAsset},
		"duration": videoDurationSeconds, "resolutionName": videoResolution, "mode": mode,
		"skipAudio": false, "useFirstFrame": true,
	}
	if aspectRatio != "" {
		imageToVideo["aspectRatio"] = aspectRatio
	}
	return marshal(map[string]any{
		"modelName": "imagine-video-gen", "message": prompt + " --mode=" + mode,
		"sendFinalMetadata":                 true,
		"parentResponseId":                  parentResponseID,
		"skipCancelCurrentInflightRequests": true,
		"mediaGenInput":                     map[string]any{"imageToVideo": imageToVideo},
	})
}

func listResponsesPath(conversationID string) string {
	return "/rest/app-chat/conversations/" + conversationID + "/responses?" + listKindJSON
}

func addResponsePath(conversationID string) string {
	return "/rest/app-chat/conversations/" + conversationID + "/responses"
}

// streamResult is what P3 keeps from a send's NDJSON stream. It never keeps
// message text, prompts or URLs.
type streamResult struct {
	ConversationIDs []string // every conversation id the stream named, in order
	HumanResponseID string
	ResponseID      string
	AssetIDs        []string
	Moderated       bool
	Failed          bool
	ProviderText    string
	Final           bool
}

const maxStreamBytes = 32 << 20

var errStreamDropped = errors.New("stream ended before the final response")

// parseSendStream reads one send's stream. PROVISIONAL (U1, U2): it accepts
// lines with or without the "result" wrapper and takes ids from the
// conversation chunk, the model response and the streaming image or video
// frames.
func parseSendStream(source io.Reader) (streamResult, error) {
	var out streamResult
	reader := bufio.NewReaderSize(io.LimitReader(source, maxStreamBytes), 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			out.consume(trimmed)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, err
		}
	}
	if out.Failed || out.Moderated {
		return out, nil
	}
	if !out.Final {
		return out, errStreamDropped
	}
	return out, nil
}

func (out *streamResult) addConversation(id string) {
	if id != "" && (len(out.ConversationIDs) == 0 || out.ConversationIDs[len(out.ConversationIDs)-1] != id) {
		out.ConversationIDs = append(out.ConversationIDs, id)
	}
}

func (out *streamResult) addAsset(id string) {
	if id == "" || !validID.MatchString(id) {
		return
	}
	for _, existing := range out.AssetIDs {
		if existing == id {
			return
		}
	}
	out.AssetIDs = append(out.AssetIDs, id)
}

func (out *streamResult) consume(line []byte) {
	var root map[string]any
	if json.Unmarshal(line, &root) != nil {
		return
	}
	if errValue, ok := root["error"]; ok && errValue != nil {
		out.Failed = true
		out.ProviderText = providerText(errValue)
		return
	}
	result, _ := root["result"].(map[string]any)
	if result == nil {
		result = root
	}
	if conversation, _ := result["conversation"].(map[string]any); conversation != nil {
		out.addConversation(stringOf(conversation["conversationId"]))
	}
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return
	}
	for _, key := range []string{"streamError", "error"} {
		if value, ok := response[key]; ok && value != nil {
			out.Failed = true
			out.ProviderText = providerText(value)
			return
		}
	}
	out.addConversation(stringOf(response["conversationId"]))
	if stringOf(response["sender"]) == "human" && out.HumanResponseID == "" {
		out.HumanResponseID = stringOf(response["responseId"])
	}
	for _, key := range []string{"streamingImageGenerationResponse", "streamingVideoGenerationResponse"} {
		frame, _ := response[key].(map[string]any)
		if frame == nil {
			continue
		}
		if moderated, _ := frame["moderated"].(bool); moderated {
			out.Moderated = true
		}
		for _, idKey := range []string{"imageId", "assetId", "videoId", "videoPostId"} {
			out.addAsset(stringOf(frame[idKey]))
		}
	}
	if model, _ := response["modelResponse"].(map[string]any); model != nil {
		out.Final = true
		out.ResponseID = stringOf(model["responseId"])
		out.addConversation(stringOf(model["conversationId"]))
		for _, id := range stringsOf(model["fileAttachments"]) {
			out.addAsset(id)
		}
	}
}

func stringOf(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func stringsOf(value any) []string {
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text := stringOf(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// providerText caps and strips provider free text (sec-rowan B10, S2).
func providerText(value any) string {
	text := ""
	switch typed := value.(type) {
	case string:
		text = typed
	case map[string]any:
		text = stringOf(typed["message"])
	}
	return capText(text, 300)
}

func capText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > limit {
		text = string(runes[:limit])
	}
	return text
}

// PostItem is one response of a post, with ids and facts only.
type PostItem struct {
	ResponseID       string      `json:"response_id"`
	ParentResponseID string      `json:"parent_response_id,omitempty"`
	Sender           string      `json:"sender"`
	Model            string      `json:"model,omitempty"`
	CreateTime       string      `json:"create_time,omitempty"`
	Assets           []AssetFact `json:"assets"`
}

// AssetFact is what P3 returns about one asset of an item.
type AssetFact struct {
	AssetID  string `json:"asset_id"`
	MIMEType string `json:"mime_type,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// PostSnapshot is one fresh read of a post (GET …/responses).
type PostSnapshot struct {
	PostID         string     `json:"post_id"`
	AccountLabel   string     `json:"account_label"`
	HeadResponseID string     `json:"head_response_id"`
	Items          []PostItem `json:"items"`
	InflightKnown  bool       `json:"inflight_known"`
	InflightCount  int        `json:"inflight_count"`
	ReadAt         time.Time  `json:"read_at"`
}

func (p PostSnapshot) hasResponse(id string) bool {
	for _, item := range p.Items {
		if item.ResponseID == id {
			return true
		}
	}
	return false
}

// responseHolding returns the response that holds assetID, preferring a
// model response over the human turn that attached it.
func (p PostSnapshot) responseHolding(assetID string) (PostItem, bool) {
	var found PostItem
	ok := false
	for _, item := range p.Items {
		for _, asset := range item.Assets {
			if asset.AssetID == assetID && (!ok || (found.Sender == "human" && item.Sender != "human")) {
				found, ok = item, true
			}
		}
	}
	return found, ok
}

func (p PostSnapshot) asset(assetID string) (AssetFact, bool) {
	for _, item := range p.Items {
		for _, asset := range item.Assets {
			if asset.AssetID == assetID {
				return asset, true
			}
		}
	}
	return AssetFact{}, false
}

// parseResponses reads GET …/responses. The head is the newest response by
// createTime, list order breaking ties (PROVISIONAL: the page uses the
// container's lastResponseId, which the list doesn't name).
func parseResponses(data []byte) (PostSnapshot, error) {
	var raw struct {
		Responses []struct {
			ResponseID       string          `json:"responseId"`
			ParentResponseID string          `json:"parentResponseId"`
			Sender           string          `json:"sender"`
			Model            string          `json:"model"`
			CreateTime       string          `json:"createTime"`
			FileAttachments  []string        `json:"fileAttachments"`
			AssetMetadata    []assetRecordIn `json:"fileAttachmentAssetMetadata"`
		} `json:"responses"`
		InflightResponses *[]json.RawMessage `json:"inflightResponses"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return PostSnapshot{}, err
	}
	snapshot := PostSnapshot{Items: make([]PostItem, 0, len(raw.Responses)), ReadAt: time.Now().UTC()}
	if raw.InflightResponses != nil {
		snapshot.InflightKnown = true
		snapshot.InflightCount = len(*raw.InflightResponses)
	}
	type ordered struct {
		index int
		at    time.Time
		id    string
	}
	var order []ordered
	for index, response := range raw.Responses {
		if !validID.MatchString(response.ResponseID) {
			continue
		}
		item := PostItem{ResponseID: response.ResponseID, Sender: "model", Model: capText(response.Model, 64), CreateTime: response.CreateTime}
		if response.Sender == "human" {
			item.Sender = "human"
		}
		if validID.MatchString(response.ParentResponseID) {
			item.ParentResponseID = response.ParentResponseID
		}
		seen := map[string]bool{}
		for _, record := range response.AssetMetadata {
			if validID.MatchString(record.AssetID) && !seen[record.AssetID] {
				seen[record.AssetID] = true
				item.Assets = append(item.Assets, AssetFact{AssetID: record.AssetID, MIMEType: capText(record.MIMEType, 64), Width: record.Width, Height: record.Height})
			}
		}
		for _, id := range response.FileAttachments {
			if validID.MatchString(id) && !seen[id] {
				seen[id] = true
				item.Assets = append(item.Assets, AssetFact{AssetID: id})
			}
		}
		if item.Assets == nil {
			item.Assets = []AssetFact{}
		}
		at, _ := time.Parse(time.RFC3339Nano, response.CreateTime)
		order = append(order, ordered{index: index, at: at, id: item.ResponseID})
		snapshot.Items = append(snapshot.Items, item)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].at.Before(order[j].at) })
	if len(order) > 0 {
		snapshot.HeadResponseID = order[len(order)-1].id
	}
	return snapshot, nil
}

// assetRecordIn is the observed GET /rest/assets/{id} record (subset).
type assetRecordIn struct {
	AssetID               string   `json:"assetId"`
	MIMEType              string   `json:"mimeType"`
	Key                   string   `json:"key"`
	ResponseID            string   `json:"responseId"`
	CurrentConversationID string   `json:"currentConversationId"`
	SourceConversationID  string   `json:"sourceConversationId"`
	OwnerUserID           string   `json:"ownerUserId"`
	SharedWithUserIDs     []string `json:"sharedWithUserIds"`
	IsPublic              bool     `json:"isPublic"`
	Width                 int      `json:"width"`
	Height                int      `json:"height"`
}

func (a assetRecordIn) conversation() string {
	if a.CurrentConversationID != "" {
		return a.CurrentConversationID
	}
	return a.SourceConversationID
}

// RLE is a COCO run-length mask exactly as grok.com gives it: counts is a
// compressed string or an integer list.
type RLE struct {
	Size   [2]int          `json:"size"`
	Counts json.RawMessage `json:"counts"`
}

// Segment is one object of an item's segment map.
type Segment struct {
	Name  string     `json:"name"`
	Box   [4]float64 `json:"box_xyxy"`
	Score float64    `json:"score"`
	Mask  RLE        `json:"mask_rle"`
}

// Segments is an item's cached segment map.
type Segments struct {
	PostID  string    `json:"post_id"`
	AssetID string    `json:"asset_id"`
	Version string    `json:"version,omitempty"`
	Objects []Segment `json:"objects"`
}

const maxSegmentObjects = 512

var errSegmentsMalformed = errors.New("segment map malformed")

// parseSegments reads POST /rest/media/segment. maskUrl is never read or
// fetched (sec-rowan S1).
func parseSegments(data []byte) (cached bool, segments Segments, err error) {
	var raw struct {
		Cached bool `json:"cached"`
		Map    *struct {
			Objects []struct {
				Name    string     `json:"name"`
				BoxXyxy [4]float64 `json:"boxXyxy"`
				Score   float64    `json:"score"`
				MaskRLE *RLE       `json:"maskRle"`
			} `json:"objects"`
			Version string `json:"version"`
		} `json:"map"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, Segments{}, errSegmentsMalformed
	}
	if !raw.Cached || raw.Map == nil {
		return false, Segments{}, nil
	}
	if len(raw.Map.Objects) > maxSegmentObjects {
		return true, Segments{}, errSegmentsMalformed
	}
	segments.Version = capText(raw.Map.Version, 64)
	segments.Objects = make([]Segment, 0, len(raw.Map.Objects))
	for _, object := range raw.Map.Objects {
		if object.MaskRLE == nil || len(object.MaskRLE.Counts) == 0 {
			return true, Segments{}, errSegmentsMalformed
		}
		counts := bytes.TrimSpace(object.MaskRLE.Counts)
		if counts[0] != '"' && counts[0] != '[' {
			return true, Segments{}, errSegmentsMalformed
		}
		segments.Objects = append(segments.Objects, Segment{
			Name: capText(object.Name, 64), Box: object.BoxXyxy, Score: object.Score,
			Mask: RLE{Size: object.MaskRLE.Size, Counts: append(json.RawMessage(nil), counts...)},
		})
	}
	return true, segments, nil
}

// Quota is an account's free quota read.
type Quota struct {
	AccountLabel    string         `json:"account_label"`
	Available       map[string]int `json:"available"`
	MaxVideoSeconds int            `json:"max_video_seconds"`
	Video720p       bool           `json:"video_720p"`
}

// parseQuota reads POST /rest/media/imagine/quota_info. The categories are
// observed; maxVideoDurationSeconds at the top level is PROVISIONAL (the
// client model has the field; where the answer carries it is unconfirmed).
func parseQuota(data []byte) (Quota, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Quota{}, err
	}
	quota := Quota{Available: map[string]int{}}
	for _, name := range []string{"image", "imagePro", "imageEdit", "video", "video720p"} {
		var category struct {
			Available *int `json:"available"`
		}
		if value, ok := raw[name]; ok && json.Unmarshal(value, &category) == nil && category.Available != nil {
			quota.Available[name] = *category.Available
			if name == "video720p" {
				quota.Video720p = true
			}
		}
	}
	if value, ok := raw["maxVideoDurationSeconds"]; ok {
		_ = json.Unmarshal(value, &quota.MaxVideoSeconds)
	}
	return quota, nil
}
