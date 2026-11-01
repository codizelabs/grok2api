package config

import "testing"

func czP5BaseConfig() Config {
	base := defaultConfig()
	base.Secrets.JWTSecret = "12345678901234567890123456789012"
	base.Secrets.CredentialEncryptionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	return base
}

func TestCZP5ResolveStatsigMode(t *testing.T) {
	tests := []struct {
		mode, signerURL, want string
	}{
		{"url", "", "local"},
		{"url", "   ", "local"},
		{"url", "https://x", "url"},
		{"manual", "", "manual"},
		{"local", "", "local"},
		{"local", "https://x", "local"},
		{"", "", ""},
	}
	for _, test := range tests {
		if got := ResolveStatsigMode(test.mode, test.signerURL); got != test.want {
			t.Errorf("ResolveStatsigMode(%q, %q) = %q, want %q", test.mode, test.signerURL, got, test.want)
		}
	}
}

func TestCZP5DefaultStatsigModeIsLocal(t *testing.T) {
	if got := defaultConfig().Provider.Web.StatsigMode; got != StatsigModeLocal {
		t.Fatalf("default StatsigMode = %q, want %q", got, StatsigModeLocal)
	}
	if StatsigModeLocal != "local" {
		t.Fatalf("StatsigModeLocal = %q, want %q", StatsigModeLocal, "local")
	}
}

func TestCZP5ValidateAcceptsLocalMode(t *testing.T) {
	cfg := czP5BaseConfig()
	cfg.Provider.Web.StatsigMode = StatsigModeLocal
	cfg.Provider.Web.StatsigSignerURL = ""
	cfg.Provider.Web.StatsigManualValue = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("local mode rejected: %v", err)
	}
}

func TestCZP5ValidateRejectsEmptyMode(t *testing.T) {
	cfg := czP5BaseConfig()
	cfg.Provider.Web.StatsigMode = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty Statsig mode accepted, want an error")
	}
}
