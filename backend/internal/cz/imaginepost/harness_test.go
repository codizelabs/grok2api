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
	"sort"
	"strings"
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
	accountC uint64 = 33
	ssoA            = "acct-a"
	ssoB            = "acct-b"
	ssoC            = "acct-c"
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
	// candidates is NewPostCandidates' answer; nil means every credential,
	// sorted by id, with unknown (-1) remaining counts.
	candidates     []Candidate
	candidateCalls int
}

// NewPostCandidates returns the scripted candidates (see fakePool.candidates).
func (p *fakePool) NewPostCandidates(_ context.Context, _ clientkeydomain.AccountScope) ([]Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.candidateCalls++
	if p.candidates != nil {
		return append([]Candidate(nil), p.candidates...), nil
	}
	ids := make([]uint64, 0, len(p.credentials))
	for id := range p.credentials {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{AccountID: id, ImageEditRemaining: -1, Video720pRemaining: -1})
	}
	return out, nil
}

// setCandidates scripts NewPostCandidates; no arguments means none.
func (p *fakePool) setCandidates(candidates ...Candidate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.candidates = append([]Candidate{}, candidates...)
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

// AcquireNew offers newAccount first, then the other account, skipping
// excluded ones; newAccount 0 refuses.
func (p *fakePool) AcquireNew(_ context.Context, _ Purpose, _ clientkeydomain.AccountScope, excluded map[uint64]bool) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.newAccount == 0 {
		return nil, refusedErr{status: http.StatusServiceUnavailable}
	}
	order := []uint64{p.newAccount, accountA, accountB}
	for _, id := range order {
		credential, ok := p.credentials[id]
		if !ok || excluded[id] {
			continue
		}
		p.acquired = append(p.acquired, id)
		return poolLease{credential: credential}, nil
	}
	return nil, refusedErr{status: http.StatusServiceUnavailable}
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
	// signErr, when set, fails every SignImagine without reaching inner.
	signErr error
	// signs records every SignImagine as "METHOD path".
	signs []string
	// failNextAssetRead, when set, fails the next GET /rest/assets/ once.
	failNextAssetRead error
	// failListReadsFor, when set, fails every GET .../responses made on
	// that account id with failListReadsErr.
	failListReadsFor uint64
	failListReadsErr error
}

func (r *recordingTransport) SignImagine(ctx context.Context, credential account.Credential, method, path string) error {
	r.mu.Lock()
	r.signs = append(r.signs, method+" "+path)
	failure := r.signErr
	r.mu.Unlock()
	if failure != nil {
		return failure
	}
	return r.inner.SignImagine(ctx, credential, method, path)
}

func (r *recordingTransport) setSignErr(err error) {
	r.mu.Lock()
	r.signErr = err
	r.mu.Unlock()
}

// Signs lists every SignImagine call as "METHOD path".
func (r *recordingTransport) Signs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.signs...)
}

func (r *recordingTransport) failNextAssetReadWith(err error) {
	r.mu.Lock()
	r.failNextAssetRead = err
	r.mu.Unlock()
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
	r.mu.Lock()
	listFailure := r.failListReadsErr
	listFailFor := r.failListReadsFor
	r.mu.Unlock()
	if listFailure != nil && credential.ID == listFailFor && method == http.MethodGet && strings.Contains(path, "/responses?") {
		return nil, listFailure
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/rest/assets/") {
		r.mu.Lock()
		failure := r.failNextAssetRead
		r.failNextAssetRead = nil
		r.mu.Unlock()
		if failure != nil {
			return nil, failure
		}
	}
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
	switch credential.ID {
	case accountB:
		sso = ssoB
	case accountC:
		sso = ssoC
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
	encrypt   func(string) string
}

// addAccountC adds a third pool account (id 33, sso "acct-c"). It is opt-in
// so the two-account tests keep their meaning.
func (h *harness) addAccountC() {
	h.t.Helper()
	credential := account.Credential{ID: accountC, Provider: account.ProviderWeb, WebTier: account.WebTierAuto, EncryptedAccessToken: h.encrypt(ssoC)}
	h.pool.mu.Lock()
	defer h.pool.mu.Unlock()
	h.pool.credentials[accountC] = credential
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
	return newHarnessWithWeb(t, web.Config{StatsigMode: "manual", StatsigManualValue: statsig}, config)
}

// newHarnessWithWeb is newHarness with the web adapter's Statsig settings
// given (BaseURL is always the fake's).
func newHarnessWithWeb(t *testing.T, webConfig web.Config, config Config) *harness {
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
	webConfig.BaseURL = fake.URL
	adapter := web.NewAdapter(webConfig, infraegress.NewManager(noEgress{}, cipher), cipher, nil, nil)
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
	return &harness{t: t, ctx: ctx, fake: fake, pool: pool, transport: transport, service: service, logs: logs, db: db, caller: Caller{ClientKeyID: keyID}, encrypt: encrypt}
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
