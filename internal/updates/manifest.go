package updates

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hauntedfail/geminus/internal/wire"
)

// File is one program of a release.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest says what a release consists of. It is signed where the release
// is built, so neither the server nor anything between it and geminus can
// change which bytes a version means.
type Manifest struct {
	Version string `json:"version"`
	// TS is when the release was signed, in Unix seconds.
	TS    int64           `json:"ts"`
	Files map[string]File `json:"files"`
	Sig   string          `json:"sig"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// releaseLabel binds a manifest signature to its version.
func releaseLabel(version string) string { return "geminus-release:" + version }

// manifestText is what a manifest signature covers: one line per file,
// "<target> <sha256> <size>", sorted by target.
func manifestText(files map[string]File) string {
	targets := make([]string, 0, len(files))
	for target := range files {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	lines := make([]string, len(targets))
	for i, target := range targets {
		f := files[target]
		lines[i] = target + " " + f.SHA256 + " " + strconv.FormatInt(f.Size, 10)
	}
	return strings.Join(lines, "\n")
}

// Seal signs a release.
func Seal(secret []byte, version string, files map[string]File, ts int64) Manifest {
	return Manifest{Version: version, TS: ts, Files: files,
		Sig: wire.Sign(secret, manifestText(files), releaseLabel(version), ts)}
}

// Verify reports whether m is a well-formed release signed with secret.
func (m Manifest) Verify(secret []byte) bool {
	if !IsRelease(m.Version) || len(m.Files) == 0 {
		return false
	}
	for target, f := range m.Files {
		if !ValidTarget(target) || !hashPattern.MatchString(f.SHA256) || f.Size <= 0 || f.Size > MaxProgram {
			return false
		}
	}
	return wire.Verify(secret, manifestText(m.Files), releaseLabel(m.Version), m.TS, m.Sig)
}
