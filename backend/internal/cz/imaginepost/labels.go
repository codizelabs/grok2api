package imaginepost

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Labeler derives the opaque names P3 shows outside grok2api: account labels
// (never a credential, name or id) and 8-hex keyed prefixes for logging
// confidential ids (sec-rowan B5).
type Labeler struct {
	key []byte
}

// NewLabeler keys every label with a secret that never leaves grok2api.
func NewLabeler(secret string) *Labeler {
	mac := hmac.New(sha256.New, []byte("cz-imaginepost-labels-v1"))
	mac.Write([]byte(secret))
	return &Labeler{key: mac.Sum(nil)}
}

func (l *Labeler) sum(kind, value string) string {
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// AccountLabel is the stable opaque label of an account.
func (l *Labeler) AccountLabel(accountID uint64) string {
	if accountID == 0 {
		return ""
	}
	return "acct_" + l.sum("account", strconv.FormatUint(accountID, 10))[:16]
}

// Ref is the 8-hex log prefix of a confidential id.
func (l *Labeler) Ref(id string) string {
	if id == "" {
		return ""
	}
	return l.sum("ref", id)[:8]
}

// UserHash hashes a grok.com ownerUserId for a boolean comparison (N3).
func (l *Labeler) UserHash(userID string) string {
	if userID == "" {
		return ""
	}
	return l.sum("user", userID)
}
