package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// p1StatsigWebAdapter is a Web provider whose every request fails before it is
// sent because no valid x-statsig-id can be had.
type p1StatsigWebAdapter struct {
	mu       sync.Mutex
	attempts []uint64
}

func (a *p1StatsigWebAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *p1StatsigWebAdapter) Definition() provider.Definition {
	return testConversationDefinition(account.ProviderWeb)
}
func (a *p1StatsigWebAdapter) record(id uint64) error {
	a.mu.Lock()
	a.attempts = append(a.attempts, id)
	a.mu.Unlock()
	return fmt.Errorf("%w: signer failed", provider.ErrStatsigUnavailable)
}
func (a *p1StatsigWebAdapter) ForwardResponse(_ context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	return nil, a.record(request.Credential.ID)
}
func (a *p1StatsigWebAdapter) GenerateImage(_ context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	return nil, a.record(request.Credential.ID)
}
func (a *p1StatsigWebAdapter) EditImage(_ context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	return nil, a.record(request.Credential.ID)
}
func (a *p1StatsigWebAdapter) Attempts() []uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]uint64(nil), a.attempts...)
}

type p1StatsigHarness struct {
	service     *Service
	adapter     *p1StatsigWebAdapter
	accountRepo *relational.AccountRepository
	key         clientkey.Key
	credentials []account.Credential
}

func newP1StatsigHarness(t *testing.T) *p1StatsigHarness {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "statsig-no-rotate.db"))
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
	responseRepo := relational.NewResponseRepository(database)
	keyRepo := relational.NewClientKeyRepository(database)
	h := &p1StatsigHarness{adapter: &p1StatsigWebAdapter{}, accountRepo: accountRepo}
	for index, name := range []string{"web-first", "web-second"} {
		credential, _, createErr := accountRepo.UpsertByIdentity(ctx, account.Credential{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierSuper,
			Name: name, SourceKey: name, EncryptedAccessToken: name + "-token", ExpiresAt: time.Now().Add(time.Hour),
			Enabled: true, AuthStatus: account.AuthStatusActive, Priority: 200 - index, MaxConcurrent: 1,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		h.credentials = append(h.credentials, credential)
	}
	if err := modelRepo.UpsertRoutes(ctx, []modeldomain.Route{
		{PublicID: "p1-chat", Provider: account.ProviderWeb, UpstreamModel: "p1-chat", Capability: modeldomain.CapabilityChat, Enabled: true},
		{PublicID: "p1-responses", Provider: account.ProviderWeb, UpstreamModel: "p1-responses", Capability: modeldomain.CapabilityResponses, Enabled: true},
		{PublicID: "p1-image", Provider: account.ProviderWeb, UpstreamModel: "p1-image", Capability: modeldomain.CapabilityImage, Enabled: true},
		{PublicID: "p1-image-edit", Provider: account.ProviderWeb, UpstreamModel: "p1-image-edit", Capability: modeldomain.CapabilityImageEdit, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range h.credentials {
		if err := modelRepo.ReplaceAccountCapabilities(ctx, credential.ID, []string{"p1-chat", "p1-responses", "p1-image", "p1-image-edit"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	h.key, err = keyRepo.Create(ctx, clientkey.Key{
		Name: "statsig-no-rotate", Prefix: "statsig-nr", SecretHash: strings.Repeat("d", 64),
		EncryptedSecret: "encrypted", Enabled: true, RPMLimit: 120, MaxConcurrent: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(h.adapter)
	sticky := memory.NewStickyStore()
	accountService := accountapp.NewService(accountRepo, auditRepo, memory.NewDeviceSessionStore(), sticky, registry, testCipher(t), nil)
	selector := NewSelector(accountRepo, memory.NewConcurrencyLimiter(), sticky, registry, time.Hour, time.Second, time.Minute)
	h.service = NewService(modelRepo, auditRepo, accountService, clientkeyapp.NewService(keyRepo, nil, nil, 60, 4, nil), registry, selector, responseRepo, 3)
	return h
}

func (h *p1StatsigHarness) assertOneAttemptNoPenalty(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("call returned nil error, want statsig_unavailable")
	}
	if !errors.Is(err, provider.ErrStatsigUnavailable) && !strings.Contains(err.Error(), "statsig_unavailable") {
		t.Fatalf("err = %v, want statsig_unavailable", err)
	}
	var failure *UpstreamFailure
	if errors.As(err, &failure) && failure.Code != "statsig_unavailable" {
		t.Fatalf("upstream failure code = %q, want statsig_unavailable", failure.Code)
	}
	attempts := h.adapter.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("attempts = %#v, want exactly one (no rotation on statsig_unavailable)", attempts)
	}
	if attempts[0] != h.credentials[0].ID {
		t.Fatalf("attempt went to account %d, want the higher-priority first account %d (second account unused)", attempts[0], h.credentials[0].ID)
	}
	for _, credential := range h.credentials {
		stored, getErr := h.accountRepo.Get(context.Background(), credential.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.FailureCount != 0 || stored.CooldownUntil != nil || stored.AuthStatus != account.AuthStatusActive {
			t.Fatalf("account %s penalised on statsig_unavailable: failures=%d cooldown=%v auth=%q, want none",
				credential.Name, stored.FailureCount, stored.CooldownUntil, stored.AuthStatus)
		}
	}
}

func TestP1StatsigUnavailableDoesNotRotateOrPenalise(t *testing.T) {
	cases := []struct {
		name string
		run  func(*p1StatsigHarness) error
	}{
		{"responses", func(h *p1StatsigHarness) error {
			result, err := h.service.CreateResponse(context.Background(), Input{
				RequestID: "req-p1-responses", ClientKey: h.key, PublicModel: "p1-responses",
				Body: []byte(`{"model":"p1-responses","input":"hello"}`),
			})
			if result != nil && result.Body != nil {
				_ = result.Body.Close()
			}
			return err
		}},
		{"chat", func(h *p1StatsigHarness) error {
			result, err := h.service.CreateChatCompletion(context.Background(), Input{
				RequestID: "req-p1-chat", ClientKey: h.key, PublicModel: "p1-chat",
				Body: []byte(`{"model":"p1-chat","messages":[{"role":"user","content":"hello"}]}`),
			})
			if result != nil && result.Body != nil {
				_ = result.Body.Close()
			}
			return err
		}},
		{"image", func(h *p1StatsigHarness) error {
			result, err := h.service.GenerateImage(context.Background(), ImageGenerationInput{
				RequestID: "req-p1-image", ClientKey: h.key, PublicModel: "p1-image",
				Prompt: "test", Count: 1, ResponseFormat: "url",
			})
			if result != nil && result.Body != nil {
				_ = result.Body.Close()
			}
			return err
		}},
		{"image_edit", func(h *p1StatsigHarness) error {
			result, err := h.service.EditImage(context.Background(), ImageEditInput{
				RequestID: "req-p1-image-edit", ClientKey: h.key, PublicModel: "p1-image-edit",
				Prompt: "edit", ImageURLs: []string{"data:image/png;base64,a"}, Count: 1, ResponseFormat: "url",
			})
			if result != nil && result.Body != nil {
				_ = result.Body.Close()
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newP1StatsigHarness(t)
			h.assertOneAttemptNoPenalty(t, tc.run(h))
		})
	}
}
