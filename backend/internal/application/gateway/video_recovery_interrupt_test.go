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
)

// p2CountingVideoAdapter counts every upstream video generation; each call
// would be a paid, duplicate send to the provider.
type p2CountingVideoAdapter struct {
	mu    sync.Mutex
	calls int
	// generate, when set, runs after the call is counted and its error is
	// returned instead of a result (for example to simulate a shutdown).
	generate func(context.Context) error
}

func (a *p2CountingVideoAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *p2CountingVideoAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *p2CountingVideoAdapter) GenerateVideo(ctx context.Context, _ provider.VideoRequest) (provider.VideoResult, error) {
	a.mu.Lock()
	a.calls++
	generate := a.generate
	a.mu.Unlock()
	if generate != nil {
		if err := generate(ctx); err != nil {
			return provider.VideoResult{}, err
		}
	}
	return provider.VideoResult{AssetID: "video_asset_00001", ContentType: "video/mp4"}, nil
}

func (a *p2CountingVideoAdapter) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type p2Harness struct {
	ctx       context.Context
	service   *Service
	mediaRepo *relational.MediaJobRepository
	adapter   *p2CountingVideoAdapter
	key       clientkey.Key
	account   account.Credential
	route     model.Route
}

// newP2Harness builds the store and the gateway. beforeBoot runs before the
// Service is constructed, so jobs it creates belong to a "previous process".
func newP2Harness(t *testing.T, beforeBoot func(h *p2Harness)) *p2Harness {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-recovery.db"))
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
		Name: "video-recovery", Prefix: "video-recovery", SecretHash: strings.Repeat("b", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := accountRepo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
		Name: "web", SourceKey: "web", EncryptedAccessToken: "web-token", ExpiresAt: time.Now().Add(time.Hour),
		Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}

	h := &p2Harness{ctx: ctx, mediaRepo: mediaRepo, adapter: &p2CountingVideoAdapter{}, key: key, account: credential, route: route}
	if beforeBoot != nil {
		beforeBoot(h)
		// Keep every "before boot" timestamp strictly earlier than construction.
		time.Sleep(5 * time.Millisecond)
	}

	registry := provider.NewRegistry(h.adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	service.ConfigureMedia(mediaRepo, 1)
	service.UpdateVideoMaxAttempts(1)
	h.service = service
	return h
}

func (h *p2Harness) job(id string, status media.Status, createdAt time.Time, leaseUntil *time.Time) media.Job {
	job := media.Job{
		ID: id, RequestID: "request-" + id, ClientKeyID: h.key.ID, ClientKeyName: h.key.Name,
		Provider: string(account.ProviderWeb),
		Model:    h.route.PublicID, ModelRouteID: h.route.ID, UpstreamModel: h.route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
		Status: status, InputJSON: `{}`, LeaseUntil: leaseUntil, CreatedAt: createdAt.UTC(), UpdatedAt: createdAt.UTC(),
	}
	if status == media.StatusInProgress {
		job.AccountID = h.account.ID
		job.AccountName = h.account.Name
		job.ClaimToken = "previous-process-claim"
	}
	return job
}

func (h *p2Harness) create(t *testing.T, job media.Job) {
	t.Helper()
	if err := h.mediaRepo.CreateMediaJob(h.ctx, job); err != nil {
		t.Fatal(err)
	}
}

func (h *p2Harness) get(t *testing.T, id string) media.Job {
	t.Helper()
	stored, err := h.mediaRepo.GetMediaJob(h.ctx, id, h.key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func p2IsTerminal(status media.Status) bool {
	return status == media.StatusCompleted || status == media.StatusFailed
}

// recoverAndRun calls RecoverVideoJobs, runs the workers, and polls until the
// job is terminal or the window closes; then stops the workers and re-reads it.
func (h *p2Harness) recoverAndRun(t *testing.T, id string, window time.Duration) media.Job {
	t.Helper()
	if err := h.service.RecoverVideoJobs(h.ctx); err != nil {
		t.Fatalf("RecoverVideoJobs: %v", err)
	}
	workerCtx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.service.RunVideoWorkers(workerCtx)
	}()
	deadline := time.Now().Add(window)
	for {
		stored := h.get(t, id)
		if p2IsTerminal(stored.Status) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Grace period so a wrong, late upstream call is still counted.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunVideoWorkers did not stop after cancel")
	}
	return h.get(t, id)
}

func p2AssertInterrupted(t *testing.T, h *p2Harness, stored media.Job) {
	t.Helper()
	if calls := h.adapter.Calls(); calls != 0 {
		t.Errorf("GenerateVideo calls = %d, want 0: a job from a previous process must never be re-sent", calls)
	}
	if stored.Status != media.StatusFailed || stored.ErrorCode != "interrupted" {
		t.Errorf("job status = %q, error code = %q, want %q / %q", stored.Status, stored.ErrorCode, media.StatusFailed, "interrupted")
	}
}

func TestP2QueuedJobFromPreviousProcessFailsInterruptedWithoutResend(t *testing.T) {
	h := newP2Harness(t, func(h *p2Harness) {
		h.create(t, h.job("video_p2_prev_queued", media.StatusQueued, time.Now().Add(-time.Hour), nil))
	})
	stored := h.recoverAndRun(t, "video_p2_prev_queued", 3*time.Second)
	p2AssertInterrupted(t, h, stored)
}

func TestP2ExpiredInProgressJobFromPreviousProcessFailsInterruptedWithoutResend(t *testing.T) {
	expired := time.Now().Add(-10 * time.Minute).UTC()
	h := newP2Harness(t, func(h *p2Harness) {
		h.create(t, h.job("video_p2_prev_expired", media.StatusInProgress, time.Now().Add(-time.Hour), &expired))
	})
	stored := h.recoverAndRun(t, "video_p2_prev_expired", 3*time.Second)
	p2AssertInterrupted(t, h, stored)
}

func TestP2InProgressJobWithoutLeaseFromPreviousProcessFailsInterruptedWithoutResend(t *testing.T) {
	h := newP2Harness(t, func(h *p2Harness) {
		h.create(t, h.job("video_p2_prev_nolease", media.StatusInProgress, time.Now().Add(-time.Hour), nil))
	})
	stored := h.recoverAndRun(t, "video_p2_prev_nolease", 3*time.Second)
	p2AssertInterrupted(t, h, stored)
}

func TestP2QueuedJobFromThisProcessStillRunsOnce(t *testing.T) {
	h := newP2Harness(t, nil)
	// Created after boot (for example it missed the in-memory queue): the
	// periodic recovery must still run it rather than fail it.
	h.create(t, h.job("video_p2_fresh_queued", media.StatusQueued, time.Now(), nil))
	stored := h.recoverAndRun(t, "video_p2_fresh_queued", 5*time.Second)
	if calls := h.adapter.Calls(); calls != 1 {
		t.Errorf("GenerateVideo calls = %d, want exactly 1", calls)
	}
	if stored.Status != media.StatusCompleted || stored.ResultAssetID != "video_asset_00001" {
		t.Errorf("job status = %q, asset = %q, error = %q/%q, want completed with video_asset_00001",
			stored.Status, stored.ResultAssetID, stored.ErrorCode, stored.ErrorMessage)
	}
}

func TestP2InProgressJobWithLiveLeaseIsLeftAlone(t *testing.T) {
	live := time.Now().Add(10 * time.Minute).UTC()
	h := newP2Harness(t, func(h *p2Harness) {
		h.create(t, h.job("video_p2_live_lease", media.StatusInProgress, time.Now().Add(-time.Hour), &live))
	})
	stored := h.recoverAndRun(t, "video_p2_live_lease", 500*time.Millisecond)
	if calls := h.adapter.Calls(); calls != 0 {
		t.Errorf("GenerateVideo calls = %d, want 0 for a job whose lease is still live", calls)
	}
	if stored.Status != media.StatusInProgress || stored.ErrorCode != "" {
		t.Errorf("job status = %q, error code = %q, want in_progress untouched", stored.Status, stored.ErrorCode)
	}
}
