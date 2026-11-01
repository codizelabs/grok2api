package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func czP5bLoadStored(t *testing.T, stored, signer string) string {
	t.Helper()
	cfg := testConfig(t)
	value := toDomainConfig(cfg)
	value.ProviderWeb.StatsigMode = stored
	value.ProviderWeb.StatsigSignerURL = signer
	repository := &runtimeSettingsRepositoryStub{value: value, found: true}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatalf("LoadPersisted(stored %q, signer %q): %v", stored, signer, err)
	}
	return loaded.Provider.Web.StatsigMode
}

func TestCZP5bLoadPersistedLegacyLocalLoadsBrowser(t *testing.T) {
	if got := czP5bLoadStored(t, "local", ""); got != "browser" {
		t.Fatalf("stored %q loaded StatsigMode = %q, want %q", "local", got, "browser")
	}
}

func TestCZP5bLoadPersistedLocalComputedLoadsLocal(t *testing.T) {
	if got := czP5bLoadStored(t, "local-computed", ""); got != "local" {
		t.Fatalf("stored %q loaded StatsigMode = %q, want %q", "local-computed", got, "local")
	}
}

func TestCZP5bLoadPersistedURLWithoutSignerLoadsBrowser(t *testing.T) {
	if got := czP5bLoadStored(t, "url", ""); got != "browser" {
		t.Fatalf("stored url with blank signer loaded StatsigMode = %q, want %q", got, "browser")
	}
}

type czP5bUpdateResult struct {
	persisted, snapshot, applied, reloaded string
}

func czP5bUpdate(t *testing.T, mode, signer string) czP5bUpdateResult {
	t.Helper()
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })
	input := service.Get().Config
	input.ProviderWeb.StatsigMode = mode
	input.ProviderWeb.StatsigSignerURL = signer
	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatalf("Update(mode %q, signer %q): %v", mode, signer, err)
	}
	loaded, _, _, err := LoadPersisted(context.Background(), cfg, repository)
	if err != nil {
		t.Fatalf("LoadPersisted after Update(%q): %v", mode, err)
	}
	return czP5bUpdateResult{
		persisted: repository.value.ProviderWeb.StatsigMode,
		snapshot:  snapshot.Config.ProviderWeb.StatsigMode,
		applied:   applied.Provider.Web.StatsigMode,
		reloaded:  loaded.Provider.Web.StatsigMode,
	}
}

func TestCZP5bUpdateExplicitLocalPersistsLocalComputed(t *testing.T) {
	got := czP5bUpdate(t, "local", "")
	want := czP5bUpdateResult{persisted: "local-computed", snapshot: "local", applied: "local", reloaded: "local"}
	if got != want {
		t.Fatalf("Update(local) = %+v, want %+v", got, want)
	}
}

func TestCZP5bUpdateBrowserRoundTrips(t *testing.T) {
	got := czP5bUpdate(t, "browser", "")
	want := czP5bUpdateResult{persisted: "browser", snapshot: "browser", applied: "browser", reloaded: "browser"}
	if got != want {
		t.Fatalf("Update(browser) = %+v, want %+v", got, want)
	}
}

func TestCZP5bUpdateURLWithoutSignerSavesBrowser(t *testing.T) {
	got := czP5bUpdate(t, "url", "")
	want := czP5bUpdateResult{persisted: "browser", snapshot: "browser", applied: "browser", reloaded: "browser"}
	if got != want {
		t.Fatalf("Update(url, blank signer) = %+v, want %+v", got, want)
	}
}
