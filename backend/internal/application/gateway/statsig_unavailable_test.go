package gateway

import (
	"context"
	"fmt"
	"net/http"
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
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestP1TransportFailureMapsStatsigUnavailable(t *testing.T) {
	failure := newTransportUpstreamFailure(fmt.Errorf("%w: manual value missing or invalid", provider.ErrStatsigUnavailable), 7, "web")
	if failure.Code != "statsig_unavailable" || failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("failure = %#v, want code statsig_unavailable and 503", failure)
	}
	if failure.AccountScoped || failure.CredentialRejected {
		t.Fatalf("pre-send statsig failure must not be account-scoped: %#v", failure)
	}
}

type p1StatsigVideoAdapter struct {
	mu    sync.Mutex
	calls int
}

func (a *p1StatsigVideoAdapter) Provider() account.Provider { return account.ProviderWeb }

func (a *p1StatsigVideoAdapter) Definition() provider.Definition {
	definition := testConversationDefinition(account.ProviderWeb)
	definition.Media.VideoGeneration = true
	return definition
}

func (a *p1StatsigVideoAdapter) GenerateVideo(context.Context, provider.VideoRequest) (provider.VideoResult, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, 0, fmt.Errorf("%w: signer URL not configured or invalid", provider.ErrStatsigUnavailable))
}

func TestP1VideoJobFailsStatsigUnavailableWithoutFailover(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-statsig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accountRepo := relational.NewAccountRepository(database)
	modelRepo := relational.NewModelRepository(database)
	auditRepo := relational.NewAuditRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	key, err := keyRepo.Create(ctx, clientkey.Key{
		Name: "video-statsig", Prefix: "video-statsig", SecretHash: strings.Repeat("c", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	var credentials []account.Credential
	for _, name := range []string{"first", "second"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 100, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		credentials = append(credentials, credential)
	}
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderWeb, []string{"grok-imagine-video"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"grok-imagine-video"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	route, err := modelRepo.GetByProviderUpstream(ctx, account.ProviderWeb, "grok-imagine-video")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &p1StatsigVideoAdapter{}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	service := NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, nil, 3)
	service.ConfigureMedia(mediaRepo, 1)
	service.UpdateVideoMaxAttempts(10)

	now := time.Now().UTC()
	job := media.Job{
		ID: "video_statsig_unavailable", RequestID: "request-video-statsig", ClientKeyID: key.ID, ClientKeyName: key.Name,
		AccountID: credentials[0].ID, AccountName: credentials[0].Name, Provider: string(account.ProviderWeb),
		Model: route.PublicID, ModelRouteID: route.ID, UpstreamModel: route.UpstreamModel,
		Operation: provider.VideoOperationGenerate, Prompt: "test", Seconds: 5, Quality: "720p",
		Status: media.StatusInProgress, InputJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	service.runVideoJob(ctx, job, route)

	adapter.mu.Lock()
	calls := adapter.calls
	adapter.mu.Unlock()
	if calls != 1 {
		t.Fatalf("GenerateVideo calls = %d, want 1 (a pre-send statsig failure must not fail over)", calls)
	}
	stored, err := mediaRepo.GetMediaJob(ctx, job.ID, job.ClientKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != media.StatusFailed || stored.ErrorCode != "statsig_unavailable" {
		t.Fatalf("job status=%q code=%q, want failed/statsig_unavailable", stored.Status, stored.ErrorCode)
	}
}
