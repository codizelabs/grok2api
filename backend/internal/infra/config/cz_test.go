package config

import (
	"bytes"
	"testing"

	"gopkg.in/yaml.v3"
)

// F7 (config half): like stays off unless cz.imaginePost.allowLike is set.
func TestCZImaginePostAllowLikeDefaultsOff(t *testing.T) {
	if defaultConfig().CZ.ImaginePost.AllowLike {
		t.Fatal("cz.imaginePost.allowLike defaults to true, want false")
	}
	cfg := defaultConfig()
	decoder := yaml.NewDecoder(bytes.NewReader([]byte("cz:\n  imaginePost:\n    allowLike: true\n")))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("strict decode of the cz section: %v", err)
	}
	if !cfg.CZ.ImaginePost.AllowLike {
		t.Fatal("allowLike: true was not read")
	}
}
