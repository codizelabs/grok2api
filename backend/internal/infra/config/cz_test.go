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

// ADR-0002 § 1: no account is pinned-only by default; the list is read from
// cz.imaginePost.pinnedOnlyAccountIds.
func TestCZImaginePostPinnedOnlyAccountIDs(t *testing.T) {
	if ids := defaultConfig().CZ.ImaginePost.PinnedOnlyAccountIDs; len(ids) != 0 {
		t.Fatalf("pinnedOnlyAccountIds defaults to %v, want empty", ids)
	}
	cfg := defaultConfig()
	decoder := yaml.NewDecoder(bytes.NewReader([]byte("cz:\n  imaginePost:\n    pinnedOnlyAccountIds: [7, 9]\n")))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("strict decode of the cz section: %v", err)
	}
	if ids := cfg.CZ.ImaginePost.PinnedOnlyAccountIDs; len(ids) != 2 || ids[0] != 7 || ids[1] != 9 {
		t.Fatalf("pinnedOnlyAccountIds = %v, want [7 9]", ids)
	}
}

// WP08a-T5: the reply-shape debug capture is off unless
// cz.imaginePost.debugReplyShapeFile names a file.
func TestCZImaginePostDebugReplyShapeFileDefaultsOff(t *testing.T) {
	if file := defaultConfig().CZ.ImaginePost.DebugReplyShapeFile; file != "" {
		t.Fatalf("debugReplyShapeFile defaults to %q, want empty (off)", file)
	}
	cfg := defaultConfig()
	decoder := yaml.NewDecoder(bytes.NewReader([]byte("cz:\n  imaginePost:\n    debugReplyShapeFile: /data/cz-reply-shapes.jsonl\n")))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("strict decode of the cz section: %v", err)
	}
	if file := cfg.CZ.ImaginePost.DebugReplyShapeFile; file != "/data/cz-reply-shapes.jsonl" {
		t.Fatalf("debugReplyShapeFile = %q, want /data/cz-reply-shapes.jsonl", file)
	}
}
