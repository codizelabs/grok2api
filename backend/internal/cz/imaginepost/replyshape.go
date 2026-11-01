package imaginepost

// Opt-in debug capture of a send's reply shape (WP08a-T5). The first real
// in-post edit (1 Nov 2026) succeeded on grok.com while P3 didn't recognise
// the reply. With cz.imaginePost.debugReplyShapeFile set, every send appends
// one JSON line to that file: the reply's JSON key structure, line by line,
// with every value replaced by its type (string, number, bool, null). It
// never records a value, prompt, URL or id; a key that looks like an id
// becomes "<key>". Off by default.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxShapeDepth    = 12
	maxShapeKinds    = 200 // distinct line shapes kept per reply
	maxShapeSequence = 500 // entries of the in-order event sequence
	maxArrayShapes   = 3   // distinct element shapes kept per array
)

var (
	shapeKeyPattern = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]{0,63}$")
	shapeHexRun     = regexp.MustCompile("[0-9a-fA-F]{8,}")
)

// shapeKey keeps a key only when it reads like a field name.
func shapeKey(key string) string {
	if !shapeKeyPattern.MatchString(key) {
		return "<key>"
	}
	digits := 0
	for _, r := range key {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits > 4 || (digits > 0 && shapeHexRun.MatchString(key)) {
		return "<key>"
	}
	return key
}

func shapeOf(value any, depth int) any {
	if depth >= maxShapeDepth {
		return "<deep>"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			name := shapeKey(key)
			if _, seen := out[name]; seen {
				continue
			}
			out[name] = shapeOf(inner, depth+1)
		}
		return out
	case []any:
		out := []any{}
		seen := map[string]bool{}
		for _, element := range typed {
			shape := shapeOf(element, depth+1)
			encoded, _ := json.Marshal(shape)
			if seen[string(encoded)] {
				continue
			}
			seen[string(encoded)] = true
			out = append(out, shape)
			if len(out) == maxArrayShapes {
				break
			}
		}
		return out
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return "other"
}

// replyLineShape is the key structure of one reply line, or false when the
// line isn't JSON.
func replyLineShape(line []byte) (any, bool) {
	var value any
	if json.Unmarshal(line, &value) != nil {
		return nil, false
	}
	return shapeOf(value, 0), true
}

type shapeStep struct {
	Shape int "json:\"shape\""
	Count int "json:\"count\""
}

// replyCapture collects one reply's line shapes in order.
type replyCapture struct {
	lines     int
	unparsed  int
	truncated bool
	shapes    []any
	index     map[string]int
	sequence  []shapeStep
}

func newReplyCapture() *replyCapture { return &replyCapture{index: map[string]int{}} }

func (c *replyCapture) observe(line []byte) {
	c.lines++
	shape, ok := replyLineShape(line)
	if !ok {
		c.unparsed++
		return
	}
	encoded, _ := json.Marshal(shape)
	position, known := c.index[string(encoded)]
	if !known {
		if len(c.shapes) >= maxShapeKinds {
			c.truncated = true
			return
		}
		position = len(c.shapes)
		c.index[string(encoded)] = position
		c.shapes = append(c.shapes, shape)
	}
	if last := len(c.sequence) - 1; last >= 0 && c.sequence[last].Shape == position {
		c.sequence[last].Count++
		return
	}
	if len(c.sequence) >= maxShapeSequence {
		c.truncated = true
		return
	}
	c.sequence = append(c.sequence, shapeStep{Shape: position, Count: 1})
}

// replyCaptureRecord is one line of the capture file.
type replyCaptureRecord struct {
	At          string      "json:\"at\""
	Op          string      "json:\"op\""
	HTTPStatus  int         "json:\"http_status\""
	ContentType string      "json:\"content_type\""
	Recognised  bool        "json:\"recognised\""
	Lines       int         "json:\"lines\""
	Unparsed    int         "json:\"unparsed_lines\""
	Truncated   bool        "json:\"truncated\""
	Shapes      []any       "json:\"shapes\""
	Sequence    []shapeStep "json:\"sequence\""
}

var replyCaptureMu sync.Mutex

// writeReplyCapture appends one record to path (created 0600, its folder
// 0700). A failure only logs a warning: the capture never affects a send.
func (s *Service) writeReplyCapture(path, op string, status int, contentType string, recognised bool, capture *replyCapture) {
	mediaType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	if !shapeKeyPattern.MatchString(strings.NewReplacer("/", "_", "-", "_", "+", "_", ".", "_").Replace(mediaType)) {
		mediaType = "<other>"
	}
	record := replyCaptureRecord{
		At: time.Now().UTC().Format(time.RFC3339), Op: op, HTTPStatus: status, ContentType: mediaType, Recognised: recognised,
		Lines: capture.lines, Unparsed: capture.unparsed, Truncated: capture.truncated, Shapes: capture.shapes, Sequence: capture.sequence,
	}
	if record.Shapes == nil {
		record.Shapes = []any{}
	}
	if record.Sequence == nil {
		record.Sequence = []shapeStep{}
	}
	data, err := json.Marshal(record)
	if err != nil {
		s.logger.Warn("cz_imagine_reply_capture_failed", "op", op)
		return
	}
	replyCaptureMu.Lock()
	defer replyCaptureMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.logger.Warn("cz_imagine_reply_capture_failed", "op", op)
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.logger.Warn("cz_imagine_reply_capture_failed", "op", op)
		return
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		s.logger.Warn("cz_imagine_reply_capture_failed", "op", op)
	}
}
