package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// ---------- fixtures ----------

func czP5Seed(n int) []byte {
	seed := make([]byte, n)
	for i := range seed {
		seed[i] = byte(i*7 + 3)
	}
	return seed
}

type czP5Segment struct {
	Color  []float64 `json:"color"`
	Deg    float64   `json:"deg"`
	Bezier []float64 `json:"bezier"`
}

func czP5Curves() [][]czP5Segment {
	sets := make([][]czP5Segment, 0, 4)
	for s := 0; s < 4; s++ {
		set := make([]czP5Segment, 0, 4)
		for i := 0; i < 4; i++ {
			set = append(set, czP5Segment{
				Color:  []float64{float64(10 * s), float64(20 + i), 200, float64(255 - 10*i), 40, float64(5 * s)},
				Deg:    float64(30*s + 15*i),
				Bezier: []float64{0.25, 0.1, 0.25, 1},
			})
		}
		sets = append(sets, set)
	}
	return sets
}

func czP5CurvesJSON(t *testing.T) string {
	t.Helper()
	encoded, err := json.Marshal(czP5Curves())
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func czP5Meta(seed []byte) string {
	return `<meta name="grok-site-verification" content="` + base64.StdEncoding.EncodeToString(seed) + `">`
}

// czP5Page builds a grok.com-like page with the seed meta and the plain
// "curves":[...] JSON inside a script.
func czP5Page(t *testing.T, seed []byte) []byte {
	t.Helper()
	return []byte(`<!doctype html><html><head>` + czP5Meta(seed) +
		`</head><body><script>window.__anim={"curves":` + czP5CurvesJSON(t) + `,"other":1}</script></body></html>`)
}

// czP5EscapedPage carries the curves once-escaped inside a script string
// (\"curves\":[...]), the way a Next.js flight payload does.
func czP5EscapedPage(t *testing.T, seed []byte) []byte {
	t.Helper()
	inner := `{"curves":` + czP5CurvesJSON(t) + `,"other":1}`
	escaped := strings.ReplaceAll(inner, `"`, `\"`)
	return []byte(`<!doctype html><html><head>` + czP5Meta(seed) +
		`</head><body><script>self.__next_f.push([1,"` + escaped + `"])</script></body></html>`)
}

var czP5HexPattern = regexp.MustCompile(`^[0-9a-f]+$`)

func czP5AssertReason(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want statsig_unavailable with reason %q", want)
	}
	if !errors.Is(err, provider.ErrStatsigUnavailable) {
		t.Fatalf("err = %v (%T), want it to wrap provider.ErrStatsigUnavailable", err, err)
	}
	if got := provider.StatsigUnavailableReason(err); got != want {
		t.Fatalf("StatsigUnavailableReason = %q, want %q", got, want)
	}
	if !strings.HasPrefix(err.Error(), "statsig_unavailable: ") {
		t.Fatalf("err.Error() = %q, want prefix %q", err.Error(), "statsig_unavailable: ")
	}
}

// czP5Expected builds the A2 layout independently of the implementation.
func czP5Expected(seed []byte, fingerprint, method, path string, now time.Time, k byte) string {
	counter := uint32(now.Unix() - 1682924400)
	out := make([]byte, 70)
	out[0] = k
	for i := 0; i < 48; i++ {
		out[1+i] = seed[i] ^ k
	}
	var le [4]byte
	binary.LittleEndian.PutUint32(le[:], counter)
	for i := 0; i < 4; i++ {
		out[49+i] = le[i] ^ k
	}
	sum := sha256.Sum256([]byte(method + "!" + path + "!" + strconv.FormatUint(uint64(counter), 10) + "obfiowerehiring" + fingerprint))
	for i := 0; i < 16; i++ {
		out[53+i] = sum[i] ^ k
	}
	out[69] = 0x03 ^ k
	return base64.RawStdEncoding.EncodeToString(out)
}

// ---------- A3: page parsing ----------

func TestCZP5ParsePageReadsSeedAndFingerprint(t *testing.T) {
	seed := czP5Seed(48)
	material, err := parseStatsigLocalPage(czP5Page(t, seed))
	if err != nil {
		t.Fatalf("parse valid page: %v", err)
	}
	if !bytes.Equal(material.seed, seed) {
		t.Fatalf("seed = %x, want %x", material.seed, seed)
	}
	if material.fingerprint == "" || !czP5HexPattern.MatchString(material.fingerprint) {
		t.Fatalf("fingerprint = %q, want non-empty lowercase hex", material.fingerprint)
	}
}

func TestCZP5ParsePageUsesFirst48SeedBytes(t *testing.T) {
	seed := czP5Seed(60)
	material, err := parseStatsigLocalPage(czP5Page(t, seed))
	if err != nil {
		t.Fatalf("parse page with a 60-byte seed: %v", err)
	}
	if !bytes.Equal(material.seed, seed[:48]) {
		t.Fatalf("seed = %x (len %d), want the first 48 bytes %x", material.seed, len(material.seed), seed[:48])
	}
}

func TestCZP5ParsePageAcceptsEscapedCurves(t *testing.T) {
	seed := czP5Seed(48)
	plain, err := parseStatsigLocalPage(czP5Page(t, seed))
	if err != nil {
		t.Fatalf("parse plain page: %v", err)
	}
	escaped, err := parseStatsigLocalPage(czP5EscapedPage(t, seed))
	if err != nil {
		t.Fatalf("parse page with once-escaped curves: %v", err)
	}
	if !bytes.Equal(escaped.seed, seed) {
		t.Fatalf("escaped page seed = %x, want %x", escaped.seed, seed)
	}
	if escaped.fingerprint != plain.fingerprint {
		t.Fatalf("escaped fingerprint %q != plain fingerprint %q for the same curves", escaped.fingerprint, plain.fingerprint)
	}
}

func TestCZP5ParsePageIsDeterministic(t *testing.T) {
	page := czP5Page(t, czP5Seed(48))
	first, err := parseStatsigLocalPage(page)
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseStatsigLocalPage(page)
	if err != nil {
		t.Fatal(err)
	}
	if first.fingerprint != second.fingerprint || !bytes.Equal(first.seed, second.seed) {
		t.Fatalf("parsing the same page twice differed: %q vs %q", first.fingerprint, second.fingerprint)
	}
}

func TestCZP5ParsePageFailures(t *testing.T) {
	curves := czP5CurvesJSON(t)
	script := func(curvesJSON string) string {
		return `<script>window.__anim={"curves":` + curvesJSON + `}</script>`
	}
	goodMeta := czP5Meta(czP5Seed(48))
	tests := []struct {
		name   string
		page   string
		reason string
	}{
		{"missing meta", `<html><head></head><body>` + script(curves) + `</body></html>`, provider.StatsigReasonMetaMissing},
		{"bad base64 meta", `<html><head><meta name="grok-site-verification" content="!!!not*base64!!!"></head><body>` + script(curves) + `</body></html>`, provider.StatsigReasonMetaInvalid},
		{"short seed", `<html><head>` + czP5Meta(czP5Seed(40)) + `</head><body>` + script(curves) + `</body></html>`, provider.StatsigReasonMetaInvalid},
		{"no curves", `<html><head>` + goodMeta + `</head><body><script>window.__anim={}</script></body></html>`, provider.StatsigReasonCurvesMissing},
		{"color has 5 numbers", `<html><head>` + goodMeta + `</head><body>` + script(`[[{"color":[1,2,3,4,5],"deg":10,"bezier":[0.1,0.2,0.3,0.4]}]]`) + `</body></html>`, provider.StatsigReasonCurvesInvalid},
		{"bezier has 3 numbers", `<html><head>` + goodMeta + `</head><body>` + script(`[[{"color":[1,2,3,4,5,6],"deg":10,"bezier":[0.1,0.2,0.3]}]]`) + `</body></html>`, provider.StatsigReasonCurvesInvalid},
		{"deg missing", `<html><head>` + goodMeta + `</head><body>` + script(`[[{"color":[1,2,3,4,5,6],"bezier":[0.1,0.2,0.3,0.4]}]]`) + `</body></html>`, provider.StatsigReasonCurvesInvalid},
		{"color is strings", `<html><head>` + goodMeta + `</head><body>` + script(`[[{"color":["a","b","c","d","e","f"],"deg":10,"bezier":[0.1,0.2,0.3,0.4]}]]`) + `</body></html>`, provider.StatsigReasonCurvesInvalid},
		{"curves is flat numbers", `<html><head>` + goodMeta + `</head><body>` + script(`[1,2,3]`) + `</body></html>`, provider.StatsigReasonCurvesInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseStatsigLocalPage([]byte(test.page))
			czP5AssertReason(t, err, test.reason)
		})
	}
}

// ---------- A1/A2: value layout ----------

func TestCZP5LocalStatsigIDMatchesLayout(t *testing.T) {
	seed := czP5Seed(48)
	material := statsigLocalMaterial{seed: seed, fingerprint: "0a1b2c3d4e5f"}
	now := time.Date(2026, 11, 1, 12, 34, 56, 0, time.UTC)
	const method, path = http.MethodPost, "/rest/app-chat/conversations/new"
	for _, k := range []byte{0x00, 0x5a, 0xff} {
		got := localStatsigID(material, method, path, now, k)
		want := czP5Expected(seed, material.fingerprint, method, path, now, k)
		if got != want {
			t.Fatalf("xor %#x: localStatsigID = %q, want %q", k, got, want)
		}
		if !validStatsigID(got) {
			t.Fatalf("xor %#x: value %q does not pass validStatsigID", k, got)
		}
		raw, err := base64.RawStdEncoding.DecodeString(got)
		if err != nil || len(raw) != 70 {
			t.Fatalf("xor %#x: decoded len = %d err = %v, want 70 bytes", k, len(raw), err)
		}
		if raw[0] != k || raw[69]^k != 0x03 {
			t.Fatalf("xor %#x: out[0]=%#x out[69]^k=%#x, want %#x and 0x03", k, raw[0], raw[69]^k, k)
		}
	}
}

func TestCZP5LocalStatsigIDIsDeterministicAndInputSensitive(t *testing.T) {
	material := statsigLocalMaterial{seed: czP5Seed(48), fingerprint: "abc123"}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	base := localStatsigID(material, http.MethodPost, "/rest/a", now, 7)
	if again := localStatsigID(material, http.MethodPost, "/rest/a", now, 7); again != base {
		t.Fatalf("same inputs gave %q then %q", base, again)
	}
	variants := map[string]string{
		"method":      localStatsigID(material, http.MethodGet, "/rest/a", now, 7),
		"path":        localStatsigID(material, http.MethodPost, "/rest/b", now, 7),
		"time":        localStatsigID(material, http.MethodPost, "/rest/a", now.Add(time.Second), 7),
		"xor":         localStatsigID(material, http.MethodPost, "/rest/a", now, 8),
		"fingerprint": localStatsigID(statsigLocalMaterial{seed: material.seed, fingerprint: "abc124"}, http.MethodPost, "/rest/a", now, 7),
	}
	for name, value := range variants {
		if value == base {
			t.Errorf("changing %s did not change the value %q", name, value)
		}
	}
}

// ---------- A5: SignLocal cache, stale, invalidate, clear ----------

type czP5Fetcher struct {
	mu       sync.Mutex
	calls    map[string]int
	material statsigLocalMaterial
	err      error
}

func (f *czP5Fetcher) fetch(_ context.Context, baseURL, _ string, _ *infraegress.Lease) (statsigLocalMaterial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[baseURL]++
	if f.err != nil {
		return statsigLocalMaterial{}, f.err
	}
	return f.material, nil
}

func (f *czP5Fetcher) count(baseURL string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[baseURL]
}

func (f *czP5Fetcher) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

type czP5Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *czP5Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *czP5Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

const czP5Base = "https://grok.com"
const czP5Path = "/rest/app-chat/conversations/new"

func czP5Signer(t *testing.T) (*statsigSigner, *czP5Fetcher, *czP5Clock) {
	t.Helper()
	material, err := parseStatsigLocalPage(czP5Page(t, czP5Seed(48)))
	if err != nil {
		t.Fatalf("fixture page did not parse: %v", err)
	}
	fetcher := &czP5Fetcher{material: material}
	clock := &czP5Clock{now: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)}
	signer := newStatsigSigner()
	signer.fetchLocal = fetcher.fetch
	signer.now = clock.Now
	signer.randomByte = func() byte { return 0x42 }
	signer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("local mode made a network request to %s", request.URL)
		return nil, errors.New("no network in tests")
	})}
	return signer, fetcher, clock
}

func czP5SignLocal(t *testing.T, signer *statsigSigner, baseURL string) (string, string, error) {
	t.Helper()
	return signer.SignLocal(context.Background(), baseURL, "fake-sso", nil, http.MethodPost, baseURL+czP5Path)
}

func TestCZP5SignLocalProducesExpectedValue(t *testing.T) {
	signer, fetcher, clock := czP5Signer(t)
	value, _, err := czP5SignLocal(t, signer, czP5Base)
	if err != nil {
		t.Fatalf("SignLocal: %v", err)
	}
	want := localStatsigID(fetcher.material, http.MethodPost, czP5Path, clock.Now(), 0x42)
	if value != want {
		t.Fatalf("SignLocal value = %q, want localStatsigID(...) = %q", value, want)
	}
	if !validStatsigID(value) {
		t.Fatalf("SignLocal value %q does not pass validStatsigID", value)
	}
}

func TestCZP5SignLocalCachesMaterialFor30Minutes(t *testing.T) {
	signer, fetcher, clock := czP5Signer(t)
	for i := 0; i < 3; i++ {
		if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := fetcher.count(czP5Base); n != 1 {
		t.Fatalf("page fetches after 3 calls = %d, want 1 (material cached)", n)
	}
	clock.Advance(29 * time.Minute)
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.count(czP5Base); n != 1 {
		t.Fatalf("page fetches at 29 min = %d, want 1", n)
	}
	clock.Advance(2 * time.Minute)
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.count(czP5Base); n != 2 {
		t.Fatalf("page fetches at 31 min = %d, want 2 (30-minute TTL expired)", n)
	}
}

func TestCZP5SignLocalCachesPerBaseURL(t *testing.T) {
	signer, fetcher, _ := czP5Signer(t)
	other := "https://grok.example.test"
	for _, base := range []string{czP5Base, other, czP5Base, other} {
		if _, _, err := czP5SignLocal(t, signer, base); err != nil {
			t.Fatalf("%s: %v", base, err)
		}
	}
	if fetcher.count(czP5Base) != 1 || fetcher.count(other) != 1 {
		t.Fatalf("fetches = %d (%s), %d (%s), want 1 each", fetcher.count(czP5Base), czP5Base, fetcher.count(other), other)
	}
}

func TestCZP5SignLocalFallsBackToStaleMaterial(t *testing.T) {
	signer, fetcher, clock := czP5Signer(t)
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	fetcher.fail(provider.NewStatsigUnavailable(provider.StatsigReasonPageUnreachable))
	clock.Advance(31 * time.Minute)
	value, source, err := czP5SignLocal(t, signer, czP5Base)
	if err != nil {
		t.Fatalf("refresh failed with earlier material available: err = %v, want stale fallback", err)
	}
	if source != "stale" {
		t.Fatalf("source = %q, want %q", source, "stale")
	}
	if fetcher.count(czP5Base) != 2 {
		t.Fatalf("fetches = %d, want 2 (a refresh was attempted)", fetcher.count(czP5Base))
	}
	want := localStatsigID(fetcher.material, http.MethodPost, czP5Path, clock.Now(), 0x42)
	if value != want {
		t.Fatalf("stale value = %q, want %q (earlier material, current time)", value, want)
	}
}

func TestCZP5SignLocalFirstFetchFailureIsStatsigUnavailable(t *testing.T) {
	signer, fetcher, _ := czP5Signer(t)
	fetcher.fail(provider.NewStatsigUnavailable(provider.StatsigReasonPageUnreachable))
	value, _, err := czP5SignLocal(t, signer, czP5Base)
	if value != "" {
		t.Fatalf("value = %q on failure, want empty", value)
	}
	czP5AssertReason(t, err, provider.StatsigReasonPageUnreachable)
}

func TestCZP5InvalidateLocalForcesRefetch(t *testing.T) {
	signer, fetcher, _ := czP5Signer(t)
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	signer.InvalidateLocal(czP5Base)
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.count(czP5Base); n != 2 {
		t.Fatalf("fetches after InvalidateLocal = %d, want 2", n)
	}
}

func TestCZP5ClearDropsLocalMaterial(t *testing.T) {
	signer, fetcher, clock := czP5Signer(t)
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	signer.Clear()
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.count(czP5Base); n != 2 {
		t.Fatalf("fetches after Clear = %d, want 2", n)
	}
	// Clear drops the material entirely: no stale fallback afterwards.
	signer.Clear()
	fetcher.fail(provider.NewStatsigUnavailable(provider.StatsigReasonPageUnreachable))
	clock.Advance(time.Minute)
	if value, source, err := czP5SignLocal(t, signer, czP5Base); err == nil {
		t.Fatalf("after Clear and a failed fetch: value=%q source=%q err=nil, want statsig_unavailable", value, source)
	}
}

func TestCZP5InvalidateSignedStatsigLocalMode(t *testing.T) {
	signer, fetcher, _ := czP5Signer(t)
	adapter := &Adapter{cfg: Config{BaseURL: czP5Base, StatsigMode: "local"}, statsig: signer}
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	if !adapter.invalidateSignedStatsig(http.MethodPost, czP5Base+czP5Path) {
		t.Fatal("invalidateSignedStatsig returned false for local mode, want true")
	}
	if _, _, err := czP5SignLocal(t, signer, czP5Base); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.count(czP5Base); n != 2 {
		t.Fatalf("fetches after invalidateSignedStatsig = %d, want 2 (material refetched)", n)
	}
}

// ---------- A1/A4: adapter wiring ----------

func TestCZP5LocalModeSignsRequestsWithoutSignerOrManualValue(t *testing.T) {
	for _, call := range p1Calls {
		t.Run(call.name, func(t *testing.T) {
			h := newP1Harness(t, Config{StatsigMode: "local"})
			page := czP5Page(t, czP5Seed(48))
			var fetches int
			var mu sync.Mutex
			h.adapter.statsig.fetchLocal = func(context.Context, string, string, *infraegress.Lease) (statsigLocalMaterial, error) {
				mu.Lock()
				fetches++
				mu.Unlock()
				return parseStatsigLocalPage(page)
			}
			_ = call.run(h, t) // the stub's {} may not parse; only the wire matters
			if n := h.restCalls.Load(); n == 0 {
				t.Fatal("local mode: no request reached /rest/, want at least one")
			}
			h.mu.Lock()
			ids := append([]string(nil), h.statsigIDs...)
			h.mu.Unlock()
			for i, id := range ids {
				if !validStatsigID(id) {
					t.Fatalf("request %d x-statsig-id = %q, want a valid 70-byte base64 value", i, id)
				}
			}
			if seen := h.signer.seen(); len(seen) != 0 {
				t.Fatalf("local mode contacted the signer client %q, want 0", seen)
			}
			mu.Lock()
			defer mu.Unlock()
			if fetches == 0 {
				t.Fatal("local mode never loaded page material")
			}
		})
	}
}

func TestCZP5LocalModePageFailureSendsNothing(t *testing.T) {
	cases := []struct {
		name   string
		page   []byte
		reason string
	}{
		{"no meta", []byte(`<html><script>window.a={"curves":[[{"color":[1,2,3,4,5,6],"deg":1,"bezier":[0.1,0.2,0.3,0.4]}]]}</script></html>`), provider.StatsigReasonMetaMissing},
		{"no curves", []byte(`<html><head>` + czP5Meta(czP5Seed(48)) + `</head></html>`), provider.StatsigReasonCurvesMissing},
	}
	for _, c := range cases {
		for _, call := range p1Calls {
			t.Run(c.name+"/"+call.name, func(t *testing.T) {
				h := newP1Harness(t, Config{StatsigMode: "local"})
				h.adapter.statsig.fetchLocal = func(context.Context, string, string, *infraegress.Lease) (statsigLocalMaterial, error) {
					return parseStatsigLocalPage(c.page)
				}
				err := call.run(h, t)
				h.assertNothingSent(t, err)
				czP5AssertReason(t, err, c.reason)
			})
		}
	}
}

func TestCZP5ApplySignedStatsigReasons(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		reason string
	}{
		{"manual empty", Config{BaseURL: czP5Base, StatsigMode: "manual", StatsigManualValue: ""}, provider.StatsigReasonManualMissing},
		{"manual invalid", Config{BaseURL: czP5Base, StatsigMode: "manual", StatsigManualValue: "invalid"}, provider.StatsigReasonManualInvalid},
		{"url empty", Config{BaseURL: czP5Base, StatsigMode: "url", StatsigSignerURL: ""}, provider.StatsigReasonSignerURLEmpty},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			signer := newStatsigSigner()
			signer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				t.Errorf("unexpected network request to %s", request.URL)
				return nil, errors.New("no network in tests")
			})}
			signer.fetchLocal = func(context.Context, string, string, *infraegress.Lease) (statsigLocalMaterial, error) {
				t.Error("non-local mode loaded local page material")
				return statsigLocalMaterial{}, errors.New("unexpected")
			}
			adapter := &Adapter{cfg: test.cfg, statsig: signer}
			request, err := http.NewRequest(http.MethodPost, czP5Base+czP5Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("x-statsig-id", "leftover")
			applyErr := adapter.applySignedStatsig(context.Background(), request, "fake-sso", nil)
			czP5AssertReason(t, applyErr, test.reason)
			if value := request.Header.Get("x-statsig-id"); value != "" {
				t.Fatalf("x-statsig-id = %q after a signing failure, want none", value)
			}
		})
	}
}

func TestCZP5ApplySignedStatsigLocalSetsHeader(t *testing.T) {
	signer, _, _ := czP5Signer(t)
	adapter := &Adapter{cfg: Config{BaseURL: czP5Base, StatsigMode: "local"}, statsig: signer}
	request, err := http.NewRequest(http.MethodPost, czP5Base+czP5Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.applySignedStatsig(context.Background(), request, "fake-sso", nil); err != nil {
		t.Fatalf("applySignedStatsig local: %v", err)
	}
	if value := request.Header.Get("x-statsig-id"); !validStatsigID(value) {
		t.Fatalf("x-statsig-id = %q, want a valid value", value)
	}
}
