package imaginepost

// Test harness: the real web.Adapter transport against the fake grok.com
// (fakegrok), a scripted two-account pool, an sqlite store and a captured
// log. Nothing here can reach the network: the adapter's BaseURL is the
// fake, Statsig is manual, and item media is routed to the fake.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/cz/imaginepost/fakegrok"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Account ids and fake sso values of the harness's two accounts.
const (
	accountA uint64 = 11
	accountB uint64 = 22
	ssoA            = "acct-a"
	ssoB            = "acct-b"
	keyID    uint64 = 5
)

var validStatsig = base64.RawStdEncoding.EncodeToString(make([]byte, 70))

type noEgress struct{}

func (noEgress) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return nil, nil
}
func (noEgress) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("not found")
}
func (noEgress) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}
func (noEgress) UpdateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}
func (noEgress) DeleteEgressNode(context.Context, uint64) error { return errors.New("unsupported") }

// fakePool leases scripted accounts and records every acquire.
type fakePool struct {
	mu          sync.Mutex
	credentials map[uint64]account.Credential
	unavailable map[uint64]error // AcquirePinned refusal per account
	newAccount  uint64           // AcquireNew's pick; 0 refuses
	acquired    []uint64         // every account a lease was granted on
	reports     []int
}

type poolLease struct {
	credential account.Credential
}

func (l poolLease) Credential() account.Credential { return l.credential }
func (l poolLease) Release()                       {}

type refusedErr struct{ status int }

func (r refusedErr) Error() string                     { return "refused" }
func (r refusedErr) HTTPStatus() int                   { return r.status }
func (r refusedErr) RetryAfterDuration() time.Duration { return 30 * time.Second }

func (p *fakePool) AcquirePinned(_ context.Context, accountID uint64, _ Purpose, _ clientkeydomain.AccountScope) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.unavailable[accountID]; err != nil {
		return nil, err
	}
	credential, ok := p.credentials[accountID]
	if !ok {
		return nil, refusedErr{status: http.StatusServiceUnavailable}
	}
	p.acquired = append(p.acquired, accountID)
	return poolLease{credential: credential}, nil
}

func (p *fakePool) AcquireNew(_ context.Context, _ Purpose, _ clientkeydomain.AccountScope) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	credential, ok := p.credentials[p.newAccount]
	if !ok {
		return nil, refusedErr{status: http.StatusServiceUnavailable}
	}
	p.acquired = append(p.acquired, p.newAccount)
	return poolLease{credential: credential}, nil
}

func (p *fakePool) Report(_ context.Context, _ account.Credential, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, status)
}

func (p *fakePool) setUnavailable(accountID uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unavailable[accountID] = err
}

func (p *fakePool) acquiredAccounts() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint64(nil), p.acquired...)
}

// recordingTransport counts every transport call and sends media to the fake.
type recordingTransport struct {
	inner *web.Adapter
	fake  *fakegrok.Server
	mu    sync.Mutex
	calls []string // "METHOD path" per DoImagine, "UPLOAD", "MEDIA key"
}

func (r *recordingTransport) note(entry string) {
	r.mu.Lock()
	r.calls = append(r.calls, entry)
	r.mu.Unlock()
}

func (r *recordingTransport) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recordingTransport) DoImagine(ctx context.Context, credential account.Credential, method, path string, body []byte) (*http.Response, error) {
	r.note(method + " " + path)
	return r.inner.DoImagine(ctx, credential, method, path, body)
}

func (r *recordingTransport) UploadImagine(ctx context.Context, credential account.Credential, filename, mimeType string, data []byte) (string, error) {
	r.note("UPLOAD")
	return r.inner.UploadImagine(ctx, credential, filename, mimeType, data)
}

// FetchImagineAsset goes to the fake, never to assets.grok.com.
func (r *recordingTransport) FetchImagineAsset(ctx context.Context, credential account.Credential, key string) (io.ReadCloser, string, error) {
	r.note("MEDIA " + key)
	sso := ssoA
	if credential.ID == accountB {
		sso = ssoB
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.fake.URL+"/"+key, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Cookie", "sso="+sso)
	response, err := r.fake.Client().Do(request)
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, "", &web.ImagineHTTPError{Status: response.StatusCode}
	}
	return response.Body, response.Header.Get("Content-Type"), nil
}

type harness struct {
	t         *testing.T
	ctx       context.Context
	fake      *fakegrok.Server
	pool      *fakePool
	transport *recordingTransport
	service   *Service
	logs      *bytes.Buffer
	db        *gorm.DB
	caller    Caller
}

type syncBuffer struct {
	mu  sync.Mutex
	buf *bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func newHarness(t *testing.T, statsig string, config Config) *harness {
	t.Helper()
	ctx := context.Background()
	fake := fakegrok.New(filepath.Join(t.TempDir(), "fake-calls.txt"))
	t.Cleanup(fake.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypt := func(value string) string {
		encrypted, err := cipher.Encrypt(value)
		if err != nil {
			t.Fatal(err)
		}
		return encrypted
	}
	adapter := web.NewAdapter(web.Config{BaseURL: fake.URL, StatsigMode: "manual", StatsigManualValue: statsig}, infraegress.NewManager(noEgress{}, cipher), cipher, nil, nil)
	pool := &fakePool{
		credentials: map[uint64]account.Credential{
			accountA: {ID: accountA, Provider: account.ProviderWeb, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypt(ssoA)},
			accountB: {ID: accountB, Provider: account.ProviderWeb, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypt(ssoB)},
		},
		unavailable: map[uint64]error{},
		newAccount:  accountA,
	}
	db, err := gorm.Open(glebarezsqlite.Open(filepath.Join(t.TempDir(), "p3.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	transport := &recordingTransport{inner: adapter, fake: fake}
	service, err := New(ctx, Options{
		DB: db, Pool: pool, Transport: transport, LabelSecret: "test-label-secret",
		Logger: slog.New(slog.NewTextHandler(&syncBuffer{buf: logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, ctx: ctx, fake: fake, pool: pool, transport: transport, service: service, logs: logs, db: db, caller: Caller{ClientKeyID: keyID}}
}

func newDefaultHarness(t *testing.T) *harness { return newHarness(t, validStatsig, Config{}) }

func pngImage() *Image {
	return &Image{MIMEType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nfake-anchor")}
}

// createPost makes a post through P3 on the pool's AcquireNew account.
func (h *harness) createPost(jobID string) *SendResult {
	h.t.Helper()
	result, err := h.service.CreatePost(h.ctx, h.caller, CreateInput{JobID: jobID, Prompt: "anchor prompt", Image: pngImage()})
	if err != nil {
		h.t.Fatalf("create post: %v", err)
	}
	return result
}

func errCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	if err == nil {
		return ""
	}
	return "non-P3 error: " + err.Error()
}

func errSend(err error) SendState {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Send
	}
	return ""
}
