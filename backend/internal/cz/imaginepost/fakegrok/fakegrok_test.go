package fakegrok

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func do(t *testing.T, s *Server, account, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, s.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if account != "" {
		request.Header.Set("Cookie", "sso="+account+"; sso-rw="+account)
	}
	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(response.Body)
	return response, out.Bytes()
}

func TestFakeRecordsCallsAndOwnership(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "calls.txt")
	s := New(countFile)
	defer s.Close()
	_, _, head := s.SeedPost("acct-a", "conv-seeded")

	response, body := do(t, s, "acct-a", http.MethodGet, "/rest/app-chat/conversations/conv-seeded/responses?conversationKind=CONVERSATION_KIND_IMAGINE", nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), head) || !strings.Contains(string(body), "inflightResponses") {
		t.Fatalf("list = %d %s", response.StatusCode, body)
	}
	if response, _ := do(t, s, "acct-b", http.MethodGet, "/rest/app-chat/conversations/conv-seeded/responses", nil); response.StatusCode != http.StatusForbidden {
		t.Fatalf("another account's conversation = %d, want 403", response.StatusCode)
	}
	if response, _ := do(t, s, "acct-a", http.MethodGet, "/rest/app-chat/conversations/conv-missing/responses", nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing conversation = %d, want 404", response.StatusCode)
	}
	edit := map[string]any{"modelName": "imagine-image-edit", "message": "m", "parentResponseId": head, "mediaGenInput": map[string]any{"imageToImage": map[string]any{"prompt": "m"}}}
	response, body = do(t, s, "acct-a", http.MethodPost, "/rest/app-chat/conversations/conv-seeded/responses", edit)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "modelResponse") {
		t.Fatalf("add-response = %d %s", response.StatusCode, body)
	}
	s.SetFault(FaultPeakHour)
	if response, _ := do(t, s, "acct-a", http.MethodPost, "/rest/app-chat/conversations/conv-seeded/responses", edit); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("peak hour = %d, want 429", response.StatusCode)
	}
	if got := s.Sends(); got != 2 {
		t.Fatalf("sends = %d, want 2", got)
	}
	lines, err := ReadCountFile(countFile)
	if err != nil || len(lines) != len(s.Calls()) {
		t.Fatalf("count file lines = %d (err %v), calls = %d", len(lines), err, len(s.Calls()))
	}
}

func TestFakeQuotaAndSegments(t *testing.T) {
	s := New("")
	defer s.Close()
	s.SetQuota("acct-basic", Quota{MaxVideoSeconds: 6})
	_, body := do(t, s, "acct-basic", http.MethodPost, "/rest/media/imagine/quota_info", map[string]any{})
	if !strings.Contains(string(body), `"maxVideoDurationSeconds":6`) || strings.Contains(string(body), "video720p") {
		t.Fatalf("6 s quota = %s", body)
	}
	_, asset, _ := s.SeedPost("acct-a", "conv-seg")
	s.SetSegments(asset, []Segment{
		{Name: "woman", Box: [4]float64{1, 2, 3, 4}, Score: 0.9, Size: [2]int{96, 64}, Counts: "PXa04"},
		{Name: "earring", Box: [4]float64{5, 6, 7, 8}, Score: 0.5, Size: [2]int{96, 64}, Counts: []int{10, 5, 6129}},
	})
	_, body = do(t, s, "acct-a", http.MethodPost, "/rest/media/segment", map[string]any{"assetId": asset, "cachedOnly": true, "maskFormat": "rle"})
	if !strings.Contains(string(body), `"counts":"PXa04"`) || !strings.Contains(string(body), `"counts":[10,5,6129]`) {
		t.Fatalf("segments = %s", body)
	}
}
