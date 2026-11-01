package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestCZP5LoadPersistedURLModeWithoutSignerLoadsBrowser(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.StatsigMode = config.StatsigModeURL
	value.ProviderWeb.StatsigSignerURL = ""
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatalf("LoadPersisted(url mode, no signer URL): %v", err)
	}
	if got := loaded.Provider.Web.StatsigMode; got != config.StatsigModeBrowser {
		t.Fatalf("loaded StatsigMode = %q, want %q", got, config.StatsigModeBrowser)
	}
}

func TestCZP5LoadPersistedURLModeWithSignerStaysURL(t *testing.T) {
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.StatsigMode = config.StatsigModeURL
	value.ProviderWeb.StatsigSignerURL = "https://signer.example.com/sign"
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatalf("LoadPersisted(url mode with signer): %v", err)
	}
	if got := loaded.Provider.Web.StatsigMode; got != config.StatsigModeURL {
		t.Fatalf("loaded StatsigMode = %q, want %q", got, config.StatsigModeURL)
	}
}

func TestCZP5UpdateURLModeWithoutSignerSavesBrowser(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.ProviderWeb.StatsigMode = config.StatsigModeURL
	input.ProviderWeb.StatsigSignerURL = ""

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatalf("Update(url mode, no signer URL): %v", err)
	}
	if got := repository.value.ProviderWeb.StatsigMode; got != config.StatsigModeBrowser {
		t.Fatalf("persisted StatsigMode = %q, want %q", got, config.StatsigModeBrowser)
	}
	if got := snapshot.Config.ProviderWeb.StatsigMode; got != config.StatsigModeBrowser {
		t.Fatalf("snapshot StatsigMode = %q, want %q", got, config.StatsigModeBrowser)
	}
	if got := applied.Provider.Web.StatsigMode; got != config.StatsigModeBrowser {
		t.Fatalf("applied StatsigMode = %q, want %q", got, config.StatsigModeBrowser)
	}
}

func TestCZP5MergeEditableURLModeWithoutSignerIsBrowser(t *testing.T) {
	cfg := testConfig(t)
	input := toEditable(cfg)
	input.ProviderWeb.StatsigMode = config.StatsigModeURL
	input.ProviderWeb.StatsigSignerURL = ""
	next, err := mergeEditable(cfg, input)
	if err != nil {
		t.Fatalf("mergeEditable: %v", err)
	}
	if got := next.Provider.Web.StatsigMode; got != config.StatsigModeBrowser {
		t.Fatalf("merged StatsigMode = %q, want %q", got, config.StatsigModeBrowser)
	}
}

func TestCZP5UpdateLocalModeRoundTrips(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, nil)
	input := service.Get().Config
	input.ProviderWeb.StatsigMode = config.StatsigModeLocal
	input.ProviderWeb.StatsigSignerURL = ""
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err != nil {
		t.Fatalf("Update(local): %v", err)
	}
	if got := repository.value.ProviderWeb.StatsigMode; got != config.StatsigModeLocalStored {
		t.Fatalf("persisted StatsigMode = %q, want %q", got, config.StatsigModeLocalStored)
	}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatalf("LoadPersisted(local): %v", err)
	}
	if got := loaded.Provider.Web.StatsigMode; got != config.StatsigModeLocal {
		t.Fatalf("reloaded StatsigMode = %q, want %q", got, config.StatsigModeLocal)
	}
}
