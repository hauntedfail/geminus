package updates

import (
	"crypto/ed25519"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// File is one program of a release.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest says what a release consists of. It is signed where the release
// is built, with a key only that computer holds, so nothing between it and
// geminus, and no other member of the group, can change which bytes a
// version means.
type Manifest struct {
	Version string `json:"version"`
	// TS is when the release was signed, in Unix seconds.
	TS    int64           `json:"ts"`
	Files map[string]File `json:"files"`
	// Sig is the Ed25519 signature over manifestText, in hex.
	Sig string `json:"sig"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// releaseHeader is the first line of what a release signature covers; it
// keeps the key from signing anything that is not a release.
const releaseHeader = "geminus-release"

// manifestText is what a manifest signature covers: the header, the version
// and the time, then one line per file, "<target> <sha256> <size>", sorted
// by target.
func manifestText(version string, ts int64, files map[string]File) []byte {
	targets := make([]string, 0, len(files))
	for target := range files {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	lines := []string{releaseHeader, version, strconv.FormatInt(ts, 10)}
	for _, target := range targets {
		f := files[target]
		lines = append(lines, target+" "+f.SHA256+" "+strconv.FormatInt(f.Size, 10))
	}
	return []byte(strings.Join(lines, "\n"))
}

// Seal signs a release. key must be a whole Ed25519 private key.
func Seal(key ed25519.PrivateKey, version string, files map[string]File, ts int64) Manifest {
	return Manifest{Version: version, TS: ts, Files: files,
		Sig: hex.EncodeToString(ed25519.Sign(key, manifestText(version, ts, files)))}
}

// Verify reports whether m is a well-formed release signed with the private
// half of key.
func (m Manifest) Verify(key ed25519.PublicKey) bool {
	if len(key) != ed25519.PublicKeySize || !IsRelease(m.Version) || len(m.Files) == 0 {
		return false
	}
	for target, f := range m.Files {
		if !ValidTarget(target) || !hashPattern.MatchString(f.SHA256) || f.Size <= 0 || f.Size > MaxProgram {
			return false
		}
	}
	sig, err := hex.DecodeString(m.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(key, manifestText(m.Version, m.TS, m.Files), sig)
}
