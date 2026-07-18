// Package sessionid generates and validates gateway public session IDs.
//
// Public IDs are non-token-shaped values of the form session-<32 lowercase hex>
// (40 characters total) and are safe to expose to Emby clients.
package sessionid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

const (
	prefix      = "session-"
	randomBytes = 16
	// Length is the exact character length of a valid public session ID.
	Length = len(prefix) + randomBytes*2 // 40
)

// Pattern is the exact case-sensitive public session ID regular expression.
const Pattern = `^session-[0-9a-f]{32}$`

var validPattern = regexp.MustCompile(Pattern)

// New returns a crypto-random public session ID: "session-" + 16 random bytes
// encoded as lowercase hex (exactly 40 characters).
func New() (string, error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}

// Valid reports whether s is an exact case-sensitive public session ID.
func Valid(s string) bool {
	return validPattern.MatchString(s)
}
