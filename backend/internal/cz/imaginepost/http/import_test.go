package imaginehttp

import (
	"context"
	"io"
	"net/http"
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

// readOnlyTransport answers every GET .../responses with an empty post and
// counts every call; anything else fails.
type readOnlyTransport struct {
	countingTransport
	reads int
}

func (r *readOnlyTransport) DoImagine(_ context.Context, _ account.Credential, method, path string, _ []byte) (*http.Response, error) {
	if method != http.MethodGet || !strings.Contains(path, "/responses?") {
		r.calls++
		return nil, io.ErrUnexpectedEOF
	}
	r.reads++
	body := io.NopCloser(strings.NewReader(`{"responses":[],"inflightResponses":[]}`))
	return &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{}}, nil
}

type oneAccountPool struct{}

func (oneAccountPool) AcquirePinned(_ context.Context, accountID uint64, _ imaginepost.Purpose, _ clientkeydomain.AccountScope) (imaginepost.Lease, error) {
	return lease{credential: account.Credential{ID: accountID}}, nil
}
func (oneAccountPool) NewPostCandidates(context.Context, clientkeydomain.AccountScope) ([]imaginepost.Candidate, error) {
	return []imaginepost.Candidate{{AccountID: 3, ImageEditRemaining: -1, Video720pRemaining: -1}}, nil
}
func (oneAccountPool) Report(context.Context, account.Credential, int) {}

type lease struct{ credential account.Credential }

func (l lease) Credential() account.Credential { return l.credential }
func (l lease) Release()                       {}

func TestImportRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(glebarezsqlite.Open(filepath.Join(t.TempDir(), "h.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	transport := &readOnlyTransport{}
	service, err := imaginepost.New(context.Background(), imaginepost.Options{DB: db, Pool: oneAccountPool{}, Transport: transport, LabelSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	group := router.Group("/cz/v1/imagine")
	group.Use(func(c *gin.Context) { c.Set(middleware.ClientKey, clientkeydomain.Key{ID: 5}); c.Next() })
	NewHandler(service).Register(group)
	label := imaginepost.NewLabeler("s").AccountLabel(3)

	status, body := call(router, http.MethodPost, "/cz/v1/imagine/posts/import", `{"conversation_id":"conv-import-1"}`, nil)
	if status != http.StatusCreated || body["post_id"] != "conv-import-1" || body["account_label"] != label || body["imported"] != true {
		t.Fatalf("first import: %d %v", status, body)
	}
	status, body = call(router, http.MethodPost, "/cz/v1/imagine/posts/import", `{"conversation_id":"conv-import-1","account_label":"`+label+`"}`, nil)
	if status != http.StatusOK || body["imported"] != false || body["account_label"] != label {
		t.Fatalf("second import: %d %v", status, body)
	}
	if transport.reads != 1 || transport.calls != 0 {
		t.Fatalf("reads %d other calls %d, want 1 and 0", transport.reads, transport.calls)
	}
	// The post routes still match after the static import route.
	status, _ = call(router, http.MethodGet, "/cz/v1/imagine/posts/conv-import-1/items", "", map[string]string{AccountLabelHeader: label})
	if status != http.StatusOK {
		t.Fatalf("items after import: %d", status)
	}
}

func TestImportRouteRefusals(t *testing.T) {
	router, transport, _ := newRouter(t, true)
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"bad json", `{`, http.StatusBadRequest, imaginepost.CodeInvalidRequest},
		{"no id", `{}`, http.StatusNotFound, imaginepost.CodePostNotFound},
		{"path id", `{"conversation_id":"../x"}`, http.StatusNotFound, imaginepost.CodePostNotFound},
		{"no account", `{"conversation_id":"conv-9"}`, http.StatusServiceUnavailable, imaginepost.CodeNoAccount},
	}
	for _, c := range cases {
		status, body := call(router, http.MethodPost, "/cz/v1/imagine/posts/import", c.body, nil)
		failure := errorOf(body)
		if status != c.status || failure["code"] != c.code || failure["send_state"] != "not_sent" {
			t.Errorf("%s: %d %v, want %d %s", c.name, status, body, c.status, c.code)
		}
	}
	if transport.calls != 0 {
		t.Fatalf("%d transport calls, want 0", transport.calls)
	}
	unauth, _, _ := newRouter(t, false)
	if status, _ := call(unauth, http.MethodPost, "/cz/v1/imagine/posts/import", `{"conversation_id":"conv-9"}`, nil); status != http.StatusUnauthorized {
		t.Fatalf("no key: %d", status)
	}
}
