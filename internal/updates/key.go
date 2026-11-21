package updates

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
)

// releaseKey is the public half of the key releases are signed with, in hex.
// The private half is on the computer that builds releases (sibyl) and
// nowhere else, so nothing a member of the group holds can sign a release.
//
// A release build links the key of its builder in here (-X), so a released
// program trusts the key that signed it.
var releaseKey = "1780a76724aaa94c9ca5295c56e6f6f879c867f37fd88497dc3997b95dcc10f8"

// ReleaseKey is the key a release must be signed with, or nil if this
// program was built without one.
func ReleaseKey() ed25519.PublicKey {
	key, err := ParsePublicKey(releaseKey)
	if err != nil {
		return nil
	}
	return key
}

// PublicKeyText is how a public key is written down: lower-case hex.
func PublicKeyText(key ed25519.PublicKey) string { return hex.EncodeToString(key) }

// ParsePublicKey reads what PublicKeyText wrote.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("a release key is 64 hex digits")
	}
	return ed25519.PublicKey(raw), nil
}
