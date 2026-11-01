package web

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// testdata/statsig_local_v8_vectors.json was produced by an independent
// JavaScript reference run in V8 (node), so these tests check the Go
// emulation of Number.prototype.toFixed, toString(16), Math.round and the
// whole fingerprint and header against the real JavaScript engine.
type statsigV8Vectors struct {
	ToFixed [][2]float64            `json:"toFixed"`
	Hex     [][2]json.RawMessage    `json:"hex"`
	Seed    string                  `json:"seed"`
	Curves  [][]statsigCurveSegment `json:"curves"`
	FPs     [][2]string             `json:"fps"`
	FP0     string                  `json:"fp0"`
	IDs     [][5]json.RawMessage    `json:"ids"`
}

func loadStatsigV8Vectors(t *testing.T) statsigV8Vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/statsig_local_v8_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors statsigV8Vectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

func TestStatsigLocalToFixedMatchesV8(t *testing.T) {
	for _, pair := range loadStatsigV8Vectors(t).ToFixed {
		if got := jsToFixed(pair[0], 2); got != pair[1] && !(got == 0 && pair[1] == 0) {
			t.Errorf("jsToFixed(%v) = %v, V8 %v", pair[0], got, pair[1])
		}
	}
}

func TestStatsigLocalToString16MatchesV8(t *testing.T) {
	for _, pair := range loadStatsigV8Vectors(t).Hex {
		var value float64
		var want string
		if err := json.Unmarshal(pair[0], &value); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(pair[1], &want); err != nil {
			t.Fatal(err)
		}
		if got := jsNumberToString16(value); got != want {
			t.Errorf("jsNumberToString16(%v) = %q, V8 %q", value, got, want)
		}
	}
	if got := jsNumberToString16(math.Copysign(0, -1)); got != "0" {
		t.Errorf("jsNumberToString16(-0) = %q, want 0", got)
	}
}

func TestStatsigLocalFingerprintMatchesV8(t *testing.T) {
	vectors := loadStatsigV8Vectors(t)
	seed, err := base64.StdEncoding.DecodeString(vectors.Seed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := statsigFingerprint(seed, vectors.Curves)
	if err != nil || got != vectors.FP0 {
		t.Fatalf("fingerprint = %q, %v; V8 %q", got, err, vectors.FP0)
	}
	for i, pair := range vectors.FPs {
		s, err := base64.StdEncoding.DecodeString(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		got, err := statsigFingerprint(s, vectors.Curves)
		if err != nil || got != pair[1] {
			t.Errorf("vector %d: fingerprint = %q, %v; V8 %q", i, got, err, pair[1])
		}
	}
}

func TestStatsigLocalIDMatchesV8(t *testing.T) {
	vectors := loadStatsigV8Vectors(t)
	seed, err := base64.StdEncoding.DecodeString(vectors.Seed)
	if err != nil {
		t.Fatal(err)
	}
	material := statsigLocalMaterial{seed: seed, fingerprint: vectors.FP0}
	for _, row := range vectors.IDs {
		var method, path, want string
		var unix int64
		var key int
		for i, target := range []any{&method, &path, &unix, &key, &want} {
			if err := json.Unmarshal(row[i], target); err != nil {
				t.Fatal(err)
			}
		}
		got := localStatsigID(material, method, path, time.Unix(unix, 0), byte(key))
		if got != want {
			t.Errorf("%s %s @%d k=%d: id = %q, V8 %q", method, path, unix, key, got, want)
		}
		if !validStatsigID(got) {
			t.Errorf("id %q fails validStatsigID", got)
		}
	}
}
