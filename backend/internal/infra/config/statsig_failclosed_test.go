package config

import (
	"strings"
	"testing"
)

func p1BaseConfig() Config {
	base := defaultConfig()
	base.Secrets.JWTSecret = "12345678901234567890123456789012"
	base.Secrets.CredentialEncryptionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	return base
}

func TestP1DefaultConfigHasNoBuiltInSignerURL(t *testing.T) {
	got := defaultConfig().Provider.Web.StatsigSignerURL
	if strings.Contains(strings.ToLower(got), "wodf") {
		t.Errorf("default StatsigSignerURL %q points at the third-party wodf signer", got)
	}
	if got != "" {
		t.Fatalf("default StatsigSignerURL = %q, want empty (no built-in third-party signer)", got)
	}
}

func TestP1ValidateURLModeAcceptsEmptySignerURL(t *testing.T) {
	cfg := p1BaseConfig()
	cfg.Provider.Web.StatsigMode = StatsigModeURL
	cfg.Provider.Web.StatsigSignerURL = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("url mode with empty (unconfigured) signer URL rejected: %v", err)
	}
}

func TestP1ValidateURLModeRejectsInvalidSignerURL(t *testing.T) {
	cfg := p1BaseConfig()
	cfg.Provider.Web.StatsigMode = StatsigModeURL
	cfg.Provider.Web.StatsigSignerURL = "http://signer.example.com:8788/sign"
	if err := cfg.Validate(); err == nil {
		t.Fatal("public plaintext Statsig signer URL was accepted")
	}
}
