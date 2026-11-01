// Package fakegrok is a fake grok.com for P3's Go tests (WP08a-T1). It speaks
// the request and answer shapes of research/grok-imagine-posts-api-2026-11.md
// (AIFriend-gen) over httptest, never contacts the network, and records every
// call by method, path and account so tests can prove zero sends.
//
// Accounts are told apart by the fake sso cookie value the test gives each
// one. Shapes that only a real send could confirm live in wire_provisional.go.
package fakegrok

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Call is one request the fake received.
type Call struct {
	Method  string
	Path    string // URL path only, no query
	Query   string
	Account string // the fake sso value, "" when no cookie was sent
	Statsig string // x-statsig-id header value
	Body    []byte
}

// Quota is one account's answer to quota_info.
type Quota struct {
	MaxVideoSeconds int  // 0 omits maxVideoDurationSeconds
	Video720p       bool // false makes video720p unavailable
}

// Segment is one object of a segment map.
type Segment struct {
	Name   string
	Box    [4]float64
	Score  float64
	Size   [2]int
	Counts any // a COCO RLE string, or []int
}

type asset struct {
	ID             string
	Owner          string
	ConversationID string
	ResponseID     string
	MIME           string
	Width, Height  int
	Key            string
	Uploaded       bool
}

type response struct {
	ResponseID       string
	ParentResponseID string
	Sender           string
	Message          string
	AssetIDs         []string
	Model            string
	CreateTime       time.Time
}

type fakeConversation struct {
	ID        string
	Owner     string
	Responses []response
	Inflight  []string
}

// Fault makes the next matching send misbehave.
type Fault string

const (
	// FaultNone answers normally.
	FaultNone Fault = ""
	// FaultPeakHour answers the next send with 429 and peak-hour fields.
	FaultPeakHour Fault = "peak_hour"
	// FaultOtherConversation ends the next send's stream in another conversation.
	FaultOtherConversation Fault = "other_conversation"
	// FaultDropAfterStart drops the next send's stream after its first line.
	// For an in-post send the post then holds only the new human item: Grok
	// never finished the model item.
	FaultDropAfterStart Fault = "drop_after_start"
	// FaultModerated ends the next send's stream with a streamError.
	FaultModerated Fault = "moderated"
	// FaultUnrecognisedReply makes the next in-post send succeed (the post
	// gains the human item and a model item with the result asset) while its
	// reply is in a shape P3's parser doesn't read (unrecognisedReplyLines),
	// as on the first real in-post edit (1 Nov 2026).
	FaultUnrecognisedReply Fault = "unrecognised_reply"
	// FaultRefusedUnrecognised is FaultUnrecognisedReply where Grok refused:
	// the post gains the human item and a model item with no asset.
	FaultRefusedUnrecognised Fault = "refused_unrecognised"
	// FaultUnrecognisedNoResult answers the next in-post send in the
	// unrecognised shape and adds only the human item to the post (no model
	// item ever appears).
	FaultUnrecognisedNoResult Fault = "unrecognised_no_result"
)

// Server is the fake grok.com.
type Server struct {
	*httptest.Server

	mu            sync.Mutex
	calls         []Call
	countFile     string
	seq           int
	conversations map[string]*fakeConversation
	assets        map[string]*asset
	quotas        map[string]Quota
	segments      map[string][]Segment
	nextFault     Fault
	omitInflight  map[string]bool
	userIDs       map[string]string
	streamError   string
}

var (
	conversationResponsesPath = regexp.MustCompile(`^/rest/app-chat/conversations/([A-Za-z0-9-]+)/responses$`)
	assetPath                 = regexp.MustCompile(`^/rest/assets/([A-Za-z0-9-]+)$`)
)

// New starts a fake grok.com. countFile, when not empty, receives one
// fsynced line per call ("METHOD PATH").
func New(countFile string) *Server {
	s := &Server{
		countFile:     countFile,
		conversations: map[string]*fakeConversation{},
		assets:        map[string]*asset{},
		quotas:        map[string]Quota{},
		segments:      map[string][]Segment{},
		omitInflight:  map[string]bool{},
		userIDs:       map[string]string{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// SetQuota sets one account's quota_info answer.
func (s *Server) SetQuota(account string, quota Quota) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotas[account] = quota
}

// SetUserID sets the ownerUserId the fake reports for an account's assets.
func (s *Server) SetUserID(account, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userIDs[account] = userID
}

// SetSegments sets the cached segment map of an asset.
func (s *Server) SetSegments(assetID string, objects []Segment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segments[assetID] = objects
}

// SetFault makes the next send misbehave once.
func (s *Server) SetFault(fault Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextFault = fault
}

// SetStreamErrorText sets the text of FaultModerated's streamError.
func (s *Server) SetStreamErrorText(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamError = text
}

// OmitInflight makes GET .../responses for a conversation omit inflightResponses.
func (s *Server) OmitInflight(conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.omitInflight[conversationID] = true
}

// SetInflight lists in-flight response ids for a conversation.
func (s *Server) SetInflight(conversationID string, ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conversation := s.conversations[conversationID]; conversation != nil {
		conversation.Inflight = append([]string(nil), ids...)
	}
}

// SeedPost creates a conversation owned by account with one uploaded root
// image and one model result, and returns its ids.
func (s *Server) SeedPost(account, conversationID string) (rootAsset, resultAsset, headResponse string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rootAsset = s.newID("asset")
	resultAsset = s.newID("asset")
	human := s.newID("resp")
	model := s.newID("resp")
	s.assets[rootAsset] = &asset{ID: rootAsset, Owner: account, ConversationID: conversationID, ResponseID: human, MIME: "image/png", Width: 64, Height: 96, Key: "users/u/" + rootAsset + "/content", Uploaded: true}
	s.assets[resultAsset] = &asset{ID: resultAsset, Owner: account, ConversationID: conversationID, ResponseID: model, MIME: "image/jpeg", Width: 64, Height: 96, Key: "users/u/generated/" + resultAsset + "/image.jpg"}
	now := time.Now().UTC().Add(-time.Hour)
	s.conversations[conversationID] = &fakeConversation{ID: conversationID, Owner: account, Inflight: []string{}, Responses: []response{
		{ResponseID: human, Sender: "human", Message: "seed", AssetIDs: []string{rootAsset}, CreateTime: now},
		{ResponseID: model, ParentResponseID: human, Sender: "assistant", Message: "seed", AssetIDs: []string{resultAsset}, Model: "imagine-image-edit", CreateTime: now.Add(time.Minute)},
	}}
	return rootAsset, resultAsset, model
}

// Calls returns every call so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Count returns how many calls matched method and path (a regexp on the path).
func (s *Server) Count(method, pathPattern string) int {
	matcher := regexp.MustCompile(pathPattern)
	n := 0
	for _, call := range s.Calls() {
		if call.Method == method && matcher.MatchString(call.Path) {
			n++
		}
	}
	return n
}

// CountByAccount returns how many calls the given account made.
func (s *Server) CountByAccount(account string) int {
	n := 0
	for _, call := range s.Calls() {
		if call.Account == account {
			n++
		}
	}
	return n
}

// Sends counts the generation sends (conversations/new and add-response POSTs).
func (s *Server) Sends() int {
	return s.Count(http.MethodPost, `^/rest/app-chat/conversations/(new|[A-Za-z0-9-]+/responses)$`)
}

func (s *Server) newID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%04d-0000-4000-8000-%012d", prefix[:4], s.seq, s.seq)
}

func (s *Server) record(request *http.Request, body []byte) Call {
	call := Call{
		Method: request.Method, Path: request.URL.Path, Query: request.URL.RawQuery,
		Account: ssoFromCookie(request.Header.Get("Cookie")), Statsig: request.Header.Get("x-statsig-id"), Body: body,
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	countFile := s.countFile
	s.mu.Unlock()
	if countFile != "" {
		if file, err := os.OpenFile(countFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = fmt.Fprintf(file, "%s %s\n", call.Method, call.Path)
			_ = file.Sync()
			_ = file.Close()
		}
	}
	return call
}

func ssoFromCookie(header string) string {
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name == "sso" {
			return value
		}
	}
	return ""
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (s *Server) serve(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(request.Body, 64<<20))
	call := s.record(request, body)
	if call.Account == "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 16, "message": "unauthenticated"}})
		return
	}
	path := request.URL.Path
	switch {
	case request.Method == http.MethodPost && path == "/http/upload-file-v2/direct":
		s.serveUpload(writer, request, call, body)
	case request.Method == http.MethodPost && path == "/rest/app-chat/conversations/new":
		s.serveCreate(writer, call, body)
	case conversationResponsesPath.MatchString(path) && request.Method == http.MethodPost:
		s.serveAddResponse(writer, call, conversationResponsesPath.FindStringSubmatch(path)[1], body)
	case conversationResponsesPath.MatchString(path) && request.Method == http.MethodGet:
		s.serveListResponses(writer, call, conversationResponsesPath.FindStringSubmatch(path)[1])
	case assetPath.MatchString(path) && request.Method == http.MethodGet:
		s.serveAsset(writer, call, assetPath.FindStringSubmatch(path)[1])
	case request.Method == http.MethodPost && path == "/rest/media/segment":
		s.serveSegment(writer, call, body)
	case request.Method == http.MethodPost && path == "/rest/media/imagine/quota_info":
		s.serveQuota(writer, call)
	case request.Method == http.MethodPost && (path == "/rest/media/post/like" || path == "/rest/media/post/unlike"):
		writeJSON(writer, http.StatusOK, map[string]any{})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/users/"):
		s.serveMedia(writer, call, path)
	default:
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 5, "message": "not found"}})
	}
}

func (s *Server) serveUpload(writer http.ResponseWriter, request *http.Request, call Call, body []byte) {
	_, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "bad multipart"})
		return
	}
	reader := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	mimeType := ""
	for {
		part, partErr := reader.NextPart()
		if partErr != nil {
			break
		}
		if part.FormName() == "makePublic" {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "makePublic is not expected"})
			return
		}
		if part.FormName() == "file" {
			mimeType = part.Header.Get("Content-Type")
		}
	}
	if mimeType == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	s.mu.Lock()
	id := s.newID("file")
	s.assets[id] = &asset{ID: id, Owner: call.Account, MIME: mimeType, Width: 64, Height: 96, Key: "users/u/" + id + "/content", Uploaded: true}
	s.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"fileMetadata": map[string]any{"fileMetadataId": id}})
}

func peakHour(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"code": 8, "message": "Too many requests during peak hours", "details": []any{map[string]any{"peakHours": true, "retryAfterSeconds": 60}},
	}})
}

func (s *Server) takeFault() Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault := s.nextFault
	s.nextFault = FaultNone
	return fault
}

func writeStream(writer http.ResponseWriter, lines [][]byte, dropAfter int) {
	writer.Header().Set("Content-Type", "application/x-ndjson")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	for index, line := range lines {
		if dropAfter > 0 && index >= dropAfter {
			if hijacker, ok := writer.(http.Hijacker); ok {
				if flusher != nil {
					flusher.Flush()
				}
				conn, buffered, err := hijacker.Hijack()
				if err == nil {
					_ = buffered.Flush()
					_ = conn.Close()
				}
			}
			return
		}
		_, _ = writer.Write(line)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func inputAssetsOf(body map[string]any) []string {
	input, _ := body["mediaGenInput"].(map[string]any)
	var out []string
	for _, key := range []string{"imageToImage", "imageToVideo", "referenceToVideo"} {
		section, _ := input[key].(map[string]any)
		values, _ := section["inputAssets"].([]any)
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
	}
	return out
}

func (s *Server) serveCreate(writer http.ResponseWriter, call Call, raw []byte) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	if err := missingKeys(body, "create"); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	fault := s.takeFault()
	if fault == FaultPeakHour {
		peakHour(writer)
		return
	}
	s.mu.Lock()
	conversationID := s.newID("conv")
	human := s.newID("resp")
	model := s.newID("resp")
	result := s.newID("asset")
	inputs := inputAssetsOf(body)
	for _, id := range inputs {
		if value := s.assets[id]; value != nil {
			value.ConversationID = conversationID
			value.ResponseID = human
		}
	}
	s.assets[result] = &asset{ID: result, Owner: call.Account, ConversationID: conversationID, ResponseID: model, MIME: "image/jpeg", Width: 64, Height: 96, Key: "users/u/generated/" + result + "/image.jpg"}
	now := time.Now().UTC()
	message, _ := body["message"].(string)
	s.conversations[conversationID] = &fakeConversation{ID: conversationID, Owner: call.Account, Inflight: []string{}, Responses: []response{
		{ResponseID: human, Sender: "human", Message: message, AssetIDs: inputs, CreateTime: now},
		{ResponseID: model, ParentResponseID: human, Sender: "assistant", Message: message, AssetIDs: []string{result}, Model: "imagine-image-edit", CreateTime: now.Add(time.Second)},
	}}
	s.mu.Unlock()
	lines := createStreamLines(conversationID, human, model, result, "https://assets.grok.com/users/u/generated/"+result+"/image.jpg", "title "+message)
	dropAfter := 0
	if fault == FaultDropAfterStart {
		dropAfter = 1
	}
	writeStream(writer, lines, dropAfter)
}

func (s *Server) serveAddResponse(writer http.ResponseWriter, call Call, conversationID string, raw []byte) {
	s.mu.Lock()
	conversation := s.conversations[conversationID]
	s.mu.Unlock()
	if conversation == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 5, "message": "conversation not found"}})
		return
	}
	if conversation.Owner != call.Account {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 7, "message": "permission denied"}})
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	modelName, _ := body["modelName"].(string)
	op := "edit"
	if modelName == "imagine-video-gen" {
		op = "video"
	}
	if err := missingKeys(body, op); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	parent, _ := body["parentResponseId"].(string)
	s.mu.Lock()
	found := false
	for _, value := range conversation.Responses {
		if value.ResponseID == parent {
			found = true
		}
	}
	s.mu.Unlock()
	if !found {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 3, "message": "unknown parent"}})
		return
	}
	fault := s.takeFault()
	if fault == FaultPeakHour {
		peakHour(writer)
		return
	}
	s.mu.Lock()
	human := s.newID("resp")
	model := s.newID("resp")
	result := s.newID("asset")
	resultConversation := conversationID
	if fault == FaultOtherConversation {
		resultConversation = s.newID("conv")
		s.conversations[resultConversation] = &fakeConversation{ID: resultConversation, Owner: call.Account, Inflight: []string{}}
	}
	mimeType, key := "image/jpeg", "users/u/generated/"+result+"/image.jpg"
	if op == "video" {
		mimeType, key = "video/mp4", "users/u/generated/"+result+"/generated_video.mp4"
	}
	// A refused send makes no asset; a dropped or unfinished one adds only
	// the human item (Grok never finished the model item).
	withAsset := fault != FaultRefusedUnrecognised
	withModel := fault != FaultDropAfterStart && fault != FaultUnrecognisedNoResult
	resultAssets := []string{}
	if withAsset && withModel {
		s.assets[result] = &asset{ID: result, Owner: call.Account, ConversationID: resultConversation, ResponseID: model, MIME: mimeType, Width: 720, Height: 1280, Key: key}
		resultAssets = []string{result}
	}
	now := time.Now().UTC()
	message, _ := body["message"].(string)
	target := s.conversations[resultConversation]
	target.Responses = append(target.Responses,
		response{ResponseID: human, ParentResponseID: parent, Sender: "human", Message: message, AssetIDs: inputAssetsOf(body), CreateTime: now})
	if withModel {
		target.Responses = append(target.Responses,
			response{ResponseID: model, ParentResponseID: human, Sender: "assistant", Message: message, AssetIDs: resultAssets, Model: modelName, CreateTime: now.Add(time.Second)})
	}
	streamText := s.streamError
	s.mu.Unlock()
	url := "https://assets.grok.com/" + key
	var lines [][]byte
	switch {
	case fault == FaultUnrecognisedReply || fault == FaultRefusedUnrecognised || fault == FaultUnrecognisedNoResult:
		lines = unrecognisedReplyLines()
	case op == "video":
		lines = addResponseVideoLines(resultConversation, human, model, result, url)
	default:
		lines = addResponseImageLines(resultConversation, human, model, result, url)
	}
	dropAfter := 0
	switch fault {
	case FaultDropAfterStart:
		dropAfter = 1
	case FaultModerated:
		lines = append(lines[:1:1], streamErrorLine(streamText))
	}
	writeStream(writer, lines, dropAfter)
}

func (s *Server) serveListResponses(writer http.ResponseWriter, call Call, conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conversation := s.conversations[conversationID]
	if conversation == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 5, "message": "conversation not found"}})
		return
	}
	if conversation.Owner != call.Account {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 7, "message": "permission denied"}})
		return
	}
	responses := make([]map[string]any, 0, len(conversation.Responses))
	for _, value := range conversation.Responses {
		entry := map[string]any{
			"responseId": value.ResponseID, "sender": value.Sender, "message": value.Message,
			"createTime": value.CreateTime.Format(time.RFC3339Nano), "model": value.Model,
			"mediaGenInput":               map[string]any{"imageToImage": map[string]any{"prompt": value.Message, "inputAssets": value.AssetIDs}},
			"fileAttachments":             value.AssetIDs,
			"fileAttachmentAssetMetadata": s.assetRecords(value.AssetIDs),
		}
		if value.ParentResponseID != "" {
			entry["parentResponseId"] = value.ParentResponseID
		}
		if value.Sender != "human" {
			urls := make([]string, 0, len(value.AssetIDs))
			for _, id := range value.AssetIDs {
				if record := s.assets[id]; record != nil {
					urls = append(urls, "https://assets.grok.com/"+record.Key)
				}
			}
			entry["generatedImageUrls"] = urls
		}
		responses = append(responses, entry)
	}
	answer := map[string]any{"responses": responses, "conversationId": conversation.ID, "workspaceId": ""}
	if !s.omitInflight[conversationID] {
		inflight := make([]map[string]any, 0, len(conversation.Inflight))
		for _, id := range conversation.Inflight {
			inflight = append(inflight, map[string]any{"responseId": id})
		}
		answer["inflightResponses"] = inflight
	}
	writeJSON(writer, http.StatusOK, answer)
}

func (s *Server) assetRecords(ids []string) []map[string]any {
	out := []map[string]any{}
	for _, id := range ids {
		if record := s.assets[id]; record != nil {
			out = append(out, s.assetRecord(record))
		}
	}
	return out
}

func (s *Server) assetRecord(record *asset) map[string]any {
	return map[string]any{
		"assetId": record.ID, "mimeType": record.MIME, "key": record.Key, "responseId": record.ResponseID,
		"sourceConversationId": record.ConversationID, "currentConversationId": record.ConversationID,
		"rootAssetSourceConversationId": record.ConversationID, "isLatest": true, "isModelGenerated": !record.Uploaded,
		"ownerUserId": s.userIDs[record.Owner], "sharedWithUserIds": []string{}, "isPublic": false,
		"width": record.Width, "height": record.Height, "rRated": false,
	}
}

func (s *Server) serveAsset(writer http.ResponseWriter, call Call, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.assets[id]
	if record == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 5, "message": "asset not found"}})
		return
	}
	if record.Owner != call.Account {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 7, "message": "permission denied"}})
		return
	}
	writeJSON(writer, http.StatusOK, s.assetRecord(record))
}

func (s *Server) serveSegment(writer http.ResponseWriter, call Call, raw []byte) {
	var body struct {
		AssetID    string `json:"assetId"`
		CachedOnly *bool  `json:"cachedOnly"`
		MaskFormat string `json:"maskFormat"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.CachedOnly == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "bad segment request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.assets[body.AssetID]
	if record == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 5, "message": "asset not found"}})
		return
	}
	if record.Owner != call.Account {
		writeJSON(writer, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 7, "message": "permission denied"}})
		return
	}
	objects, cached := s.segments[body.AssetID]
	if !cached {
		writeJSON(writer, http.StatusOK, map[string]any{"cached": false, "map": nil})
		return
	}
	list := make([]map[string]any, 0, len(objects))
	for _, object := range objects {
		list = append(list, map[string]any{
			"name": object.Name, "boxXyxy": object.Box, "score": object.Score, "maskUrl": "",
			"maskRle": map[string]any{"size": object.Size, "counts": object.Counts},
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"cached": true, "map": map[string]any{"objects": list, "version": "fake-1", "textProvenance": "model"}})
}

func (s *Server) serveQuota(writer http.ResponseWriter, call Call) {
	s.mu.Lock()
	quota, ok := s.quotas[call.Account]
	s.mu.Unlock()
	if !ok {
		quota = Quota{MaxVideoSeconds: 15, Video720p: true}
	}
	category := func(available int) map[string]any {
		return map[string]any{"available": available, "windowSizeSeconds": 86400}
	}
	answer := map[string]any{"image": category(50), "imagePro": category(10), "imageEdit": category(50), "video": category(20)}
	if quota.Video720p {
		answer["video720p"] = category(10)
	}
	if quota.MaxVideoSeconds > 0 {
		answer["maxVideoDurationSeconds"] = quota.MaxVideoSeconds
	}
	writeJSON(writer, http.StatusOK, answer)
}

func (s *Server) serveMedia(writer http.ResponseWriter, call Call, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.assets {
		if "/"+record.Key == path {
			if record.Owner != call.Account {
				writeJSON(writer, http.StatusForbidden, map[string]any{"error": "permission denied"})
				return
			}
			writer.Header().Set("Content-Type", record.MIME)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("FAKE-MEDIA-" + record.ID))
			return
		}
	}
	writeJSON(writer, http.StatusNotFound, map[string]any{"error": "not found"})
}

// ReadCountFile returns the "METHOD PATH" lines of a count file, sorted.
func ReadCountFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	sort.Strings(lines)
	return lines, scanner.Err()
}
