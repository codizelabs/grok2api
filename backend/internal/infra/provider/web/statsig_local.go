package web

// Local x-statsig-id (statsigMode "local", P5).
//
// grok.com's web client signs each REST call with an x-statsig-id header it
// computes in the browser. This file computes the same value in-process, so
// Grok Web calls need neither a third-party signer nor a pasted value. The
// only network call it makes is the GET of grok.com's own page that
// fetchStatsigPage already performs (same egress lease, same headers); it
// contacts no other host.
//
// The value is 70 bytes, base64 without padding:
//
//	out[0]      = k, one random byte
//	out[1..48]  = seed[i] XOR k        (seed: the page's grok-site-verification meta, base64-decoded)
//	out[49..69] = tail[i] XOR k        (tail: uint32le(t) ++ sha256(input)[0:16] ++ 0x03)
//	t           = unix seconds - 1682924400
//	input       = METHOD "!" PATH "!" t "obfiowerehiring" FINGERPRINT
//
// FINGERPRINT is derived from the seed and the animation "curves" the same
// page carries: the browser plays one curve as a CSS animation and reads its
// computed colour and rotation matrix back; statsigFingerprint reproduces that
// reading. The research note research/grok-statsig-local-2026-11.md in
// AIFriend-gen records how this was worked out and what is verified.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	statsigLocalEpoch       = 1682924400
	statsigLocalSalt        = "obfiowerehiring"
	statsigLocalMark        = 0x03
	statsigLocalSeedLen     = 48
	statsigLocalIDLen       = 70
	statsigLocalAnimationMS = 4096.0
	statsigLocalMaterialTTL = 30 * time.Minute
	statsigCurvesScanLimit  = 256 << 10
)

// statsigCurveSegment is one entry of a curve set in the page's "curves" array.
type statsigCurveSegment struct {
	Color  []float64 `json:"color"`
	Deg    *float64  `json:"deg"`
	Bezier []float64 `json:"bezier"`
}

// statsigLocalMaterial is what one page fetch yields: the 48-byte seed and the
// animation fingerprint derived from it. Every request's value is computed
// from it with a fresh time and random byte.
type statsigLocalMaterial struct {
	seed        []byte
	fingerprint string
}

type statsigLocalEntry struct {
	material  statsigLocalMaterial
	fetchedAt time.Time
}

type statsigLocalResult struct {
	material statsigLocalMaterial
	source   string
}

func statsigUnavailable(reason string) error { return provider.NewStatsigUnavailable(reason) }

// parseStatsigLocalPage reads the seed and curves from a grok.com page body.
func parseStatsigLocalPage(body []byte) (statsigLocalMaterial, error) {
	meta, err := extractStatsigMetaContent(body)
	if err != nil {
		return statsigLocalMaterial{}, statsigUnavailable(provider.StatsigReasonMetaMissing)
	}
	seed, err := decodeStatsigSeed(meta)
	if err != nil {
		return statsigLocalMaterial{}, err
	}
	curves, err := extractStatsigCurves(body)
	if err != nil {
		return statsigLocalMaterial{}, err
	}
	fingerprint, err := statsigFingerprint(seed, curves)
	if err != nil {
		return statsigLocalMaterial{}, err
	}
	return statsigLocalMaterial{seed: seed, fingerprint: fingerprint}, nil
}

// decodeStatsigSeed decodes the meta value; at least 48 bytes are required.
func decodeStatsigSeed(meta string) ([]byte, error) {
	meta = strings.TrimSpace(meta)
	var decoded []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err = encoding.DecodeString(meta); err == nil {
			break
		}
	}
	if err != nil || len(decoded) < statsigLocalSeedLen {
		return nil, statsigUnavailable(provider.StatsigReasonMetaInvalid)
	}
	return decoded[:statsigLocalSeedLen], nil
}

// extractStatsigCurves finds the "curves" array in the page. The page carries
// it inside the framework's streamed payload, where quotes are escaped once
// (\"curves\":[...]); a plain "curves":[ is accepted too.
func extractStatsigCurves(body []byte) ([][]statsigCurveSegment, error) {
	plain := bytes.Index(body, []byte(`"curves":[`))
	escaped := bytes.Index(body, []byte(`\"curves\":[`))
	var start int
	var isEscaped bool
	switch {
	case plain < 0 && escaped < 0:
		return nil, statsigUnavailable(provider.StatsigReasonCurvesMissing)
	case plain < 0 || (escaped >= 0 && escaped < plain):
		start, isEscaped = escaped+len(`\"curves\":`), true
	default:
		start = plain + len(`"curves":`)
	}
	window := body[start:]
	if len(window) > statsigCurvesScanLimit {
		window = window[:statsigCurvesScanLimit]
	}
	text := string(window)
	if isEscaped {
		text = strings.ReplaceAll(text, `\\`, `\`)
		text = strings.ReplaceAll(text, `\"`, `"`)
	}
	end := balancedJSONArrayEnd(text)
	if end < 0 {
		return nil, statsigUnavailable(provider.StatsigReasonCurvesInvalid)
	}
	var curves [][]statsigCurveSegment
	if err := json.Unmarshal([]byte(text[:end]), &curves); err != nil {
		return nil, statsigUnavailable(provider.StatsigReasonCurvesInvalid)
	}
	if len(curves) == 0 {
		return nil, statsigUnavailable(provider.StatsigReasonCurvesInvalid)
	}
	for _, set := range curves {
		if len(set) == 0 {
			return nil, statsigUnavailable(provider.StatsigReasonCurvesInvalid)
		}
		for _, segment := range set {
			if len(segment.Color) != 6 || len(segment.Bezier) != 4 || segment.Deg == nil {
				return nil, statsigUnavailable(provider.StatsigReasonCurvesInvalid)
			}
		}
	}
	return curves, nil
}

// balancedJSONArrayEnd returns the index just past the array that opens at
// text[0], or -1. Brackets inside JSON strings are ignored.
func balancedJSONArrayEnd(text string) int {
	if text == "" || text[0] != '[' {
		return -1
	}
	depth := 0
	inString := false
	escapedChar := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escapedChar:
				escapedChar = false
			case c == '\\':
				escapedChar = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// statsigFingerprint reproduces the browser's animation reading. Seed byte 5
// picks the curve set and the segment in it; bytes 22, 23 and 24 pick the
// moment (0 to 3380 ms of a 4096 ms animation) at which colour and rotation
// are read. Each value is rounded to two decimals, written in base 16 as
// JavaScript does, and joined with '.' and '-' removed.
func statsigFingerprint(seed []byte, curves [][]statsigCurveSegment) (string, error) {
	if len(seed) < statsigLocalSeedLen || len(curves) == 0 {
		return "", statsigUnavailable(provider.StatsigReasonCurvesInvalid)
	}
	set := curves[int(seed[5])%len(curves)]
	if len(set) == 0 {
		return "", statsigUnavailable(provider.StatsigReasonCurvesInvalid)
	}
	segment := set[int(seed[5])%len(set)]
	if len(segment.Color) != 6 || len(segment.Bezier) != 4 || segment.Deg == nil {
		return "", statsigUnavailable(provider.StatsigReasonCurvesInvalid)
	}
	scale := func(value, low, high float64) float64 {
		return jsToFixed(value*((high-low)/255)+low, 2)
	}
	x1 := scale(segment.Bezier[0], 0, 1)
	y1 := scale(segment.Bezier[1], -1, 1)
	x2 := scale(segment.Bezier[2], 0, 1)
	y2 := scale(segment.Bezier[3], -1, 1)
	seekMS := jsRound(float64(int(seed[24]%16)*int(seed[22]%16)*int(seed[23]%16))/10) * 10
	progress := cubicBezierProgress(x1, y1, x2, y2, seekMS/statsigLocalAnimationMS)

	channel := func(from, to float64) float64 {
		return math.Max(0, math.Min(255, jsRound(from+(to-from)*progress)))
	}
	endAngle := math.Floor(*segment.Deg*((360-60)/255.0) + 60)
	angle := endAngle * progress * math.Pi / 180
	cos, sin := math.Cos(angle), math.Sin(angle)
	values := []float64{
		channel(segment.Color[0], segment.Color[3]),
		channel(segment.Color[1], segment.Color[4]),
		channel(segment.Color[2], segment.Color[5]),
		cos, sin, -sin, cos, 0, 0,
	}
	var out strings.Builder
	for _, value := range values {
		out.WriteString(jsNumberToString16(jsToFixed(value, 2)))
	}
	return strings.NewReplacer(".", "", "-", "").Replace(out.String()), nil
}

// localStatsigID builds one x-statsig-id. now and xorKey are parameters so
// tests can fix them; callers pass the clock and a random byte.
func localStatsigID(material statsigLocalMaterial, method, path string, now time.Time, xorKey byte) string {
	counter := uint32(now.Unix() - statsigLocalEpoch)
	input := method + "!" + path + "!" + strconv.FormatUint(uint64(counter), 10) + statsigLocalSalt + material.fingerprint
	sum := sha256.Sum256([]byte(input))

	out := make([]byte, statsigLocalIDLen)
	out[0] = xorKey
	for i := 0; i < statsigLocalSeedLen; i++ {
		out[1+i] = material.seed[i] ^ xorKey
	}
	tail := make([]byte, 0, 21)
	tail = binary.LittleEndian.AppendUint32(tail, counter)
	tail = append(tail, sum[:16]...)
	tail = append(tail, statsigLocalMark)
	for i, b := range tail {
		out[1+statsigLocalSeedLen+i] = b ^ xorKey
	}
	return base64.RawStdEncoding.EncodeToString(out)
}

// jsRound is JavaScript's Math.round: halves round towards +Infinity.
func jsRound(value float64) float64 { return math.Floor(value + 0.5) }

// jsToFixed is Number(value.toFixed(digits)): the decimal nearest the exact
// binary value, a tie taking the larger magnitude, read back as a double.
func jsToFixed(value float64, digits int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1e21 {
		return value
	}
	negative := math.Signbit(value)
	exact := new(big.Rat).SetFloat64(math.Abs(value))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	exact.Mul(exact, new(big.Rat).SetInt(scale))
	exact.Add(exact, big.NewRat(1, 2))
	rounded := new(big.Int).Quo(exact.Num(), exact.Denom())
	result, _ := new(big.Rat).SetFrac(rounded, scale).Float64()
	if negative {
		return -result
	}
	return result
}

// jsNumberToString16 is JavaScript's Number.prototype.toString(16) as V8
// computes it: fraction digits stop once the input double's precision is
// reached, rounding half to even.
func jsNumberToString16(value float64) string {
	const radix = 16
	const digits = "0123456789abcdef"
	if value == 0 {
		return "0"
	}
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 0) {
		if value < 0 {
			return "-Infinity"
		}
		return "Infinity"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	integer := math.Floor(value)
	fraction := value - integer
	delta := 0.5 * (math.Nextafter(value, math.Inf(1)) - value)
	delta = math.Max(math.Nextafter(0, 1), delta)

	var fractionDigits []byte
	if fraction >= delta {
		for {
			fraction *= radix
			delta *= radix
			digit := int(fraction)
			fractionDigits = append(fractionDigits, digits[digit])
			fraction -= float64(digit)
			if fraction > 0.5 || (fraction == 0.5 && digit&1 == 1) {
				if fraction+delta > 1 {
					// Round up, carrying into earlier digits as needed.
					for {
						if len(fractionDigits) == 0 {
							integer++
							break
						}
						last := fractionDigits[len(fractionDigits)-1]
						fractionDigits = fractionDigits[:len(fractionDigits)-1]
						d := strings.IndexByte(digits, last)
						if d+1 < radix {
							fractionDigits = append(fractionDigits, digits[d+1])
							break
						}
					}
					break
				}
			}
			if fraction < delta {
				break
			}
		}
	}

	var integerDigits []byte
	for float64Exponent(integer/radix) > 0 {
		integer /= radix
		integerDigits = append(integerDigits, '0')
	}
	for {
		remainder := math.Mod(integer, radix)
		integerDigits = append(integerDigits, digits[int(remainder)])
		integer = (integer - remainder) / radix
		if integer <= 0 {
			break
		}
	}
	var out strings.Builder
	if negative {
		out.WriteByte('-')
	}
	for i := len(integerDigits) - 1; i >= 0; i-- {
		out.WriteByte(integerDigits[i])
	}
	if len(fractionDigits) > 0 {
		out.WriteByte('.')
		out.Write(fractionDigits)
	}
	return out.String()
}

// float64Exponent is V8's Double::Exponent: e in value = f * 2^e with f the
// 53-bit significand.
func float64Exponent(value float64) int {
	bits := math.Float64bits(value)
	biased := int((bits >> 52) & 0x7FF)
	if biased == 0 {
		return -1074
	}
	return biased - 0x3FF - 52
}

// cubicBezierProgress is the CSS cubic-bezier(x1, y1, x2, y2) easing at x.
func cubicBezierProgress(x1, y1, x2, y2, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	cx := 3 * x1
	bx := 3*(x2-x1) - cx
	ax := 1 - cx - bx
	cy := 3 * y1
	by := 3*(y2-y1) - cy
	ay := 1 - cy - by
	sampleX := func(t float64) float64 { return ((ax*t+bx)*t + cx) * t }
	sampleY := func(t float64) float64 { return ((ay*t+by)*t + cy) * t }
	slopeX := func(t float64) float64 { return (3*ax*t+2*bx)*t + cx }

	const epsilon = 1e-7
	t := x
	for i := 0; i < 8; i++ {
		errX := sampleX(t) - x
		if math.Abs(errX) < epsilon {
			return sampleY(t)
		}
		slope := slopeX(t)
		if math.Abs(slope) < epsilon {
			break
		}
		t -= errX / slope
	}
	low, high := 0.0, 1.0
	t = x
	for i := 0; i < 64 && low < high; i++ {
		value := sampleX(t)
		if math.Abs(value-x) < epsilon {
			break
		}
		if x > value {
			low = t
		} else {
			high = t
		}
		t = (low + high) / 2
	}
	return sampleY(t)
}

func statsigTargetPath(target string) (string, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path, nil
}

func randomStatsigByte() byte {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return byte(time.Now().UnixNano())
	}
	return b[0]
}

// fetchStatsigLocalMaterial GETs grok.com's page through the request's own
// egress lease (fetchStatsigPage) and reads seed and curves from it. Its
// errors carry a fixed readable reason, never a token or upstream text.
func fetchStatsigLocalMaterial(ctx context.Context, baseURL, token string, lease *infraegress.Lease) (statsigLocalMaterial, error) {
	body, err := fetchStatsigPage(ctx, baseURL, token, lease)
	if err != nil {
		return statsigLocalMaterial{}, statsigPageFetchError(err)
	}
	return parseStatsigLocalPage(body)
}

func statsigPageFetchError(err error) error {
	switch {
	case errors.Is(err, provider.ErrStatsigUnavailable):
		return err
	case errors.Is(err, errStatsigMetaMissing):
		return statsigUnavailable(provider.StatsigReasonMetaMissing)
	case errors.Is(err, errStatsigPageStatus):
		return statsigUnavailable(provider.StatsigReasonPageStatus)
	case errors.Is(err, errStatsigPageTooLarge):
		return statsigUnavailable(provider.StatsigReasonPageTooLarge)
	default:
		return statsigUnavailable(provider.StatsigReasonPageUnreachable)
	}
}

// SignLocal computes x-statsig-id for method+target in-process. The page
// material is cached per base URL for statsigLocalMaterialTTL; a failed
// refresh falls back to the previous material ("stale").
func (s *statsigSigner) SignLocal(ctx context.Context, baseURL, token string, lease *infraegress.Lease, method, target string) (string, string, error) {
	path, err := statsigTargetPath(target)
	if err != nil {
		return "", "", statsigUnavailable(provider.StatsigReasonTargetInvalid)
	}
	result, err := s.localMaterial(ctx, baseURL, token, lease)
	if err != nil {
		return "", "", err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	randomByte := s.randomByte
	if randomByte == nil {
		randomByte = randomStatsigByte
	}
	return localStatsigID(result.material, method, path, s.now().UTC(), randomByte()), result.source, nil
}

// WarmLocal loads the page material once so the first request need not.
func (s *statsigSigner) WarmLocal(ctx context.Context, baseURL, token string, lease *infraegress.Lease) (int, error) {
	result, err := s.localMaterial(ctx, baseURL, token, lease)
	if err != nil {
		return 0, err
	}
	if result.source == "refresh" {
		return 1, nil
	}
	return 0, nil
}

func (s *statsigSigner) localMaterial(ctx context.Context, baseURL, token string, lease *infraegress.Lease) (statsigLocalResult, error) {
	key := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if material, ok := s.freshLocal(key, s.now().UTC()); ok {
		return statsigLocalResult{material: material, source: "cache"}, nil
	}
	value, err, _ := s.refreshes.Do("local\x00"+key, func() (any, error) {
		if material, ok := s.freshLocal(key, s.now().UTC()); ok {
			return statsigLocalResult{material: material, source: "cache"}, nil
		}
		fetch := s.fetchLocal
		if fetch == nil {
			fetch = fetchStatsigLocalMaterial
		}
		material, fetchErr := fetch(ctx, baseURL, token, lease)
		if fetchErr != nil {
			if stale, ok := s.staleLocal(key); ok {
				return statsigLocalResult{material: stale, source: "stale"}, nil
			}
			return nil, fetchErr
		}
		s.storeLocal(key, material, s.now().UTC())
		return statsigLocalResult{material: material, source: "refresh"}, nil
	})
	if err != nil {
		return statsigLocalResult{}, err
	}
	return value.(statsigLocalResult), nil
}

func (s *statsigSigner) freshLocal(key string, now time.Time) (statsigLocalMaterial, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.local[key]
	if !ok || entry.material.fingerprint == "" || !now.Before(entry.fetchedAt.Add(statsigLocalMaterialTTL)) {
		return statsigLocalMaterial{}, false
	}
	return entry.material, true
}

func (s *statsigSigner) staleLocal(key string) (statsigLocalMaterial, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.local[key]
	return entry.material, ok && len(entry.material.seed) == statsigLocalSeedLen && entry.material.fingerprint != ""
}

func (s *statsigSigner) storeLocal(key string, material statsigLocalMaterial, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.local == nil {
		s.local = make(map[string]statsigLocalEntry)
	}
	s.local[key] = statsigLocalEntry{material: material, fetchedAt: now}
}

// InvalidateLocal drops the cached page material after an anti-bot
// rejection, so the next request reads the page again.
func (s *statsigSigner) InvalidateLocal(baseURL string) {
	key := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	s.mu.Lock()
	delete(s.local, key)
	s.mu.Unlock()
}
