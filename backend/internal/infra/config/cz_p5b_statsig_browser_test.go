package config

import "testing"

func TestCZP5bStatsigModeConstants(t *testing.T) {
	if StatsigModeBrowser != "browser" {
		t.Fatalf("StatsigModeBrowser = %q, want %q", StatsigModeBrowser, "browser")
	}
	if StatsigModeLocalStored != "local-computed" {
		t.Fatalf("StatsigModeLocalStored = %q, want %q", StatsigModeLocalStored, "local-computed")
	}
}

func TestCZP5bDefaultStatsigModeIsBrowser(t *testing.T) {
	if got := defaultConfig().Provider.Web.StatsigMode; got != "browser" {
		t.Fatalf("default StatsigMode = %q, want %q", got, "browser")
	}
}

func TestCZP5bValidateAcceptsBrowserModeWithoutSignerOrManualValue(t *testing.T) {
	cfg := defaultConfig()
	cfg.Secrets.JWTSecret = "12345678901234567890123456789012"
	cfg.Secrets.CredentialEncryptionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	cfg.Provider.Web.StatsigMode = "browser"
	cfg.Provider.Web.StatsigSignerURL = ""
	cfg.Provider.Web.StatsigManualValue = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("browser mode rejected: %v", err)
	}
}

func TestCZP5bResolveStatsigModeOnSave(t *testing.T) {
	tests := []struct {
		mode, signerURL, want string
	}{
		{"url", "", "browser"},
		{"url", "   ", "browser"},
		{"url", "\t\n", "browser"},
		{"url", "https://signer.example.com/sign", "url"},
		{"local", "", "local"},
		{"local", "https://signer.example.com/sign", "local"},
		{"manual", "", "manual"},
		{"browser", "", "browser"},
		{"browser", "https://signer.example.com/sign", "browser"},
		{"", "", ""},
	}
	for _, test := range tests {
		if got := ResolveStatsigMode(test.mode, test.signerURL); got != test.want {
			t.Errorf("ResolveStatsigMode(%q, %q) = %q, want %q", test.mode, test.signerURL, got, test.want)
		}
	}
}

func TestCZP5bResolveStoredStatsigModeOnLoad(t *testing.T) {
	tests := []struct {
		stored, signerURL, want string
	}{
		{"local", "", "browser"}, // legacy save from the old local default
		{"local", "https://signer.example.com/sign", "browser"},
		{"local-computed", "", "local"},
		{"url", "", "browser"},
		{"url", "  ", "browser"},
		{"url", "https://signer.example.com/sign", "url"},
		{"manual", "", "manual"},
		{"browser", "", "browser"},
	}
	for _, test := range tests {
		if got := ResolveStoredStatsigMode(test.stored, test.signerURL); got != test.want {
			t.Errorf("ResolveStoredStatsigMode(%q, %q) = %q, want %q", test.stored, test.signerURL, got, test.want)
		}
	}
}

func TestCZP5bStoredStatsigMode(t *testing.T) {
	tests := []struct{ mode, want string }{
		{"local", "local-computed"},
		{"browser", "browser"},
		{"url", "url"},
		{"manual", "manual"},
		{"", ""},
	}
	for _, test := range tests {
		if got := StoredStatsigMode(test.mode); got != test.want {
			t.Errorf("StoredStatsigMode(%q) = %q, want %q", test.mode, got, test.want)
		}
	}
}

// Saving a mode and loading it back gives the mode the save resolved to: an
// explicit local choice survives a restart instead of turning into browser.
func TestCZP5bStoredStatsigModeRoundTrips(t *testing.T) {
	for _, mode := range []string{"browser", "local", "manual", "url"} {
		for _, signer := range []string{"", "https://signer.example.com/sign"} {
			saved := ResolveStatsigMode(mode, signer)
			loaded := ResolveStoredStatsigMode(StoredStatsigMode(saved), signer)
			if loaded != saved {
				t.Errorf("mode %q signer %q: saved %q, stored %q, loaded %q; want loaded == saved",
					mode, signer, saved, StoredStatsigMode(saved), loaded)
			}
		}
	}
}
