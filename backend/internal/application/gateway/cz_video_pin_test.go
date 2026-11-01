package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// czRecordingVideoAdapter records which account every video create ran on.
type czRecordingVideoAdapter struct {
	mu       sync.Mutex
	accounts []uint64
}

func (a *czRecordingVideoAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *czRecordingVideoAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *czRecordingVideoAdapter) GenerateVideo(_ context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	a.mu.Lock()
	a.accounts = append(a.accounts, request.Credential.ID)
	a.mu.Unlock()
	return provider.VideoResult{AssetID: "video_asset_00002", ContentType: "video/mp4"}, nil
}

func (a *czRecordingVideoAdapter) calls() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.accounts...)
}

// czPinHarness holds two web accounts: the job's pinned account and a
// healthy one the old fallback would have moved the job to.
type czPinHarness struct {
	ctx         context.Context
	service     *Service
	selector    *Selector
	mediaRepo   *relational.MediaJobRepository
	accountRepo *relational.AccountRepository
	adapter     *czRecordingVideoAdapter
	key         clientkey.Key
	pinned      account.Credential
	healthy     account.Credential
	route       model.Route
}

func newCZPinHarness(t *testing.T) *czPinHarness {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "cz-pin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "cz-pin", Prefix: "cz-pin", SecretHash: strings.Repeat("c", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	upsert := func(name string, priority int) account.Credential {
		credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: priority, MaxConcurrent: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return credential
	}
	pinned := upsert("cz-pinned", 100)
	healthy := upsert("cz-healthy", 200)
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{pinned.ID, healthy.ID} {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, id, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &czRecordingVideoAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	service.ConfigureMedia(mediaRepo, 1)
	service.UpdateVideoMaxAttempts(3)
	return &czPinHarness{ctx: ctx, service: service, selector: selector, mediaRepo: mediaRepo, accountRepo: accountRepo,
		adapter: adapter, key: key, pinned: pinned, healthy: healthy, route: route}
}

func (h *czPinHarness) runPinnedJob(t *testing.T, id string) media.Job {
	t.Helper()
	now := time.Now().UTC()
	job := media.Job{
		ID: id, RequestID: "request-" + id, ClientKeyID: h.key.ID, ClientKeyName: h.key.Name,
		Provider: string(account.ProviderWeb), Model: h.route.PublicID, ModelRouteID: h.route.ID, UpstreamModel: h.route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 15, Quality: "720p",
		Status: media.StatusQueued, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
		AccountID: h.pinned.ID, AccountName: h.pinned.Name,
	}
	if err := h.mediaRepo.CreateMediaJob(h.ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := h.service.claimVideoJob(h.ctx, id)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	h.service.runVideoJob(h.ctx, claimed, h.route)
	stored, err := h.mediaRepo.GetMediaJob(h.ctx, id, h.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (h *czPinHarness) assertNeverMoved(t *testing.T, stored media.Job) {
	t.Helper()
	if calls := h.adapter.calls(); len(calls) != 0 {
		t.Fatalf("video creates ran on accounts %v; a pinned job must make zero calls on any other account", calls)
	}
	if stored.Status != media.StatusFailed || stored.ErrorCode != "upstream_pinned_account_unavailable" {
		t.Fatalf("job status = %q code = %q, want failed / upstream_pinned_account_unavailable", stored.Status, stored.ErrorCode)
	}
	if stored.AccountID != h.pinned.ID {
		t.Fatalf("job account = %d, want the pinned account %d", stored.AccountID, h.pinned.ID)
	}
}

// F4 (generation-v1.1-posts.tests.md § 5): attempt 0's AcquirePinned fails
// on a disabled pinned account; the job ends upstream_pinned_account_unavailable
// and never moves to the healthy account (application/gateway/video.go fix).
func TestCZVideoPinnedAccountDisabledNeverMoves(t *testing.T) {
	h := newCZPinHarness(t)
	disabled := false
	if updated, err := h.accountRepo.UpdateMany(h.ctx, account.ProviderWeb, []uint64{h.pinned.ID}, repository.AccountUpdates{Enabled: &disabled}); err != nil || updated != 1 {
		t.Fatalf("disable pinned: updated=%d err=%v", updated, err)
	}
	h.assertNeverMoved(t, h.runPinnedJob(t, "video_cz_pin_disabled"))
}

// F4, cooling variant: a pinned account in cool-down is never swapped.
func TestCZVideoPinnedAccountCoolingNeverMoves(t *testing.T) {
	h := newCZPinHarness(t)
	h.selector.MarkFailure(h.ctx, h.pinned, 429, time.Hour)
	h.assertNeverMoved(t, h.runPinnedJob(t, "video_cz_pin_cooling"))
}

// The healthy path is unchanged: a pinned account that is available runs the job.
func TestCZVideoPinnedAccountAvailableRunsOnIt(t *testing.T) {
	h := newCZPinHarness(t)
	stored := h.runPinnedJob(t, "video_cz_pin_ok")
	calls := h.adapter.calls()
	if len(calls) != 1 || calls[0] != h.pinned.ID {
		t.Fatalf("video creates ran on %v, want exactly once on the pinned account %d", calls, h.pinned.ID)
	}
	if stored.Status != media.StatusCompleted {
		t.Fatalf("job status = %q (%s/%s), want completed", stored.Status, stored.ErrorCode, stored.ErrorMessage)
	}
}
