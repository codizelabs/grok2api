package imaginehttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// countingTransport fails the test's assertions if anything reaches it.
type countingTransport struct{ calls int }

func (c *countingTransport) DoImagine(context.Context, account.Credential, string, string, []byte) (*http.Response, error) {
	c.calls++
	return nil, errors.New("no network in handler tests")
}
func (c *countingTransport) UploadImagine(context.Context, account.Credential, string, string, []byte) (string, error) {
	c.calls++
	return "", errors.New("no network in handler tests")
}
func (c *countingTransport) SignImagine(context.Context, account.Credential, string, string) error {
	c.calls++
	return errors.New("no network in handler tests")
}
func (c *countingTransport) FetchImagineAsset(context.Context, account.Credential, string) (io.ReadCloser, string, error) {
	c.calls++
	return nil, "", errors.New("no network in handler tests")
}

type refusingPool struct{}

func (refusingPool) AcquirePinned(context.Context, uint64, imaginepost.Purpose, clientkeydomain.AccountScope) (imaginepost.Lease, error) {
	return nil, errors.New("refused")
}
func (refusingPool) NewPostCandidates(context.Context, clientkeydomain.AccountScope) ([]imaginepost.Candidate, error) {
	return nil, errors.New("refused")
}
func (refusingPool) Report(context.Context, account.Credential, int) {}

func newRouter(t *testing.T, withKey bool) (*gin.Engine, *countingTransport, *imaginepost.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(glebarezsqlite.Open(filepath.Join(t.TempDir(), "h.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	transport := &countingTransport{}
	service, err := imaginepost.New(context.Background(), imaginepost.Options{DB: db, Pool: refusingPool{}, Transport: transport, LabelSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	group := router.Group("/cz/v1/imagine")
	if withKey {
		group.Use(func(c *gin.Context) { c.Set(middleware.ClientKey, clientkeydomain.Key{ID: 5}); c.Next() })
	}
	NewHandler(service).Register(group)
	return router, transport, service
}

func call(router *gin.Engine, method, path, body string, header map[string]string) (int, map[string]any) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func errorOf(body map[string]any) map[string]any {
	value, _ := body["error"].(map[string]any)
	return value
}

func TestRoutesNeedAClientKey(t *testing.T) {
	router, transport, _ := newRouter(t, false)
	status, body := call(router, http.MethodGet, "/cz/v1/imagine/posts/conv-1/items", "", nil)
	if status != http.StatusUnauthorized || errorOf(body)["send_state"] != "not_sent" || transport.calls != 0 {
		t.Fatalf("status %d body %v calls %d", status, body, transport.calls)
	}
}

func TestErrorShapeAndPreSendRefusals(t *testing.T) {
	router, transport, service := newRouter(t, true)
	if err := service.Store().PutOwner(context.Background(), imaginepost.PostOwner{ConversationID: "conv-1", AccountID: 3, ClientKeyID: 5}); err != nil {
		t.Fatal(err)
	}
	label := imaginepost.NewLabeler("s").AccountLabel(3)
	cases := []struct {
		name, method, path, body string
		header                   map[string]string
		status                   int
		code                     string
	}{
		{"missing label", http.MethodGet, "/cz/v1/imagine/posts/conv-1/items", "", nil, http.StatusBadRequest, imaginepost.CodeAccountLabelRequired},
		{"wrong label", http.MethodGet, "/cz/v1/imagine/posts/conv-1/items", "", map[string]string{AccountLabelHeader: "acct_wrong"}, http.StatusConflict, imaginepost.CodeAccountLabelMismatch},
		{"unknown post", http.MethodGet, "/cz/v1/imagine/posts/conv-2/items", "", map[string]string{AccountLabelHeader: label}, http.StatusNotFound, imaginepost.CodePostNotFound},
		{"pinned refused", http.MethodPost, "/cz/v1/imagine/posts/conv-1/items/asset-1/segments", `{"cachedOnly":false}`, map[string]string{AccountLabelHeader: label}, http.StatusServiceUnavailable, imaginepost.CodePinnedAccountUnavailable},
		{"like off", http.MethodPost, "/cz/v1/imagine/posts/conv-1/items/asset-1/like", "", map[string]string{AccountLabelHeader: label}, http.StatusForbidden, imaginepost.CodeLikeDisabled},
		{"video spec", http.MethodPost, "/cz/v1/imagine/posts/conv-1/videos", `{"job_id":"j1","prompt":"p","source_asset_id":"asset-1","duration_s":6,"resolution":"720p"}`, map[string]string{AccountLabelHeader: label}, http.StatusUnprocessableEntity, imaginepost.CodeSpecUnsupported},
		{"no job id", http.MethodPost, "/cz/v1/imagine/posts/conv-1/edits", `{"prompt":"p","source_asset_id":"asset-1"}`, map[string]string{AccountLabelHeader: label}, http.StatusBadRequest, imaginepost.CodeJobIDRequired},
		{"bad json", http.MethodPost, "/cz/v1/imagine/posts", `{`, nil, http.StatusBadRequest, imaginepost.CodeInvalidRequest},
		{"bad base64", http.MethodPost, "/cz/v1/imagine/posts", `{"job_id":"j","prompt":"p","image":{"mime_type":"image/png","data_base64":"%%"}}`, nil, http.StatusBadRequest, imaginepost.CodeInvalidRequest},
	}
	for _, c := range cases {
		status, body := call(router, c.method, c.path, c.body, c.header)
		failure := errorOf(body)
		if status != c.status || failure["code"] != c.code || failure["send_state"] != "not_sent" || failure["message"] == "" {
			t.Errorf("%s: status %d body %v, want %d %s", c.name, status, body, c.status, c.code)
		}
		if c.code == imaginepost.CodePinnedAccountUnavailable && failure["layer_code"] != imaginepost.LayerCodePostAccountUnavailable {
			t.Errorf("%s: layer_code = %v", c.name, failure["layer_code"])
		}
	}
	if transport.calls != 0 {
		t.Fatalf("%d transport calls, want 0", transport.calls)
	}
}
