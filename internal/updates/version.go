// Package updates is how a geminus finds, checks and installs a newer
// release of itself, and what it shares with the server that hands releases
// out (cmd/geminus-latest-channel): versions, the signed manifest of a
// release, and signed requests. See plans/geminus/updates-design.md.
package updates

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is the X.Y.Z at the front of a version string. Exact is false when
// something follows it ("0.4.0-dev", a build from the repository).
type Version struct {
	Major, Minor, Patch int
	Exact               bool
}

// maxDigits bounds each number so it always fits an int.
const maxDigits = 9

// ParseVersion reads "X.Y.Z" with an optional suffix after "-" or "+".
func ParseVersion(s string) (Version, bool) {
	core, exact := s, true
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core, exact = s[:i], false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || len(p) > maxDigits || len(p) > 1 && p[0] == '0' || strings.Trim(p, "0123456789") != "" {
			return Version{}, false
		}
		n[i], _ = strconv.Atoi(p)
	}
	return Version{Major: n[0], Minor: n[1], Patch: n[2], Exact: exact}, true
}

// Less reports whether v is older than o. With the same numbers, a build
// with a suffix is older than the release without one; two builds with
// suffixes are not ordered, since only releases are ever compared against.
func (v Version) Less(o Version) bool {
	switch {
	case v.Major != o.Major:
		return v.Major < o.Major
	case v.Minor != o.Minor:
		return v.Minor < o.Minor
	case v.Patch != o.Patch:
		return v.Patch < o.Patch
	}
	return !v.Exact && o.Exact
}

// IsRelease reports whether s is a version the server hands out: X.Y.Z and
// nothing else.
func IsRelease(s string) bool {
	v, ok := ParseVersion(s)
	return ok && v.Exact
}

// Older reports whether version a is older than b; false if either is not a
// version.
func Older(a, b string) bool {
	va, okA := ParseVersion(a)
	vb, okB := ParseVersion(b)
	return okA && okB && va.Less(vb)
}

// Newer reports whether release is a version this program, at version
// current, should move to. A program that cannot say which version it is
// takes any release.
func Newer(current, release string) bool {
	r, ok := ParseVersion(release)
	if !ok {
		return false
	}
	c, ok := ParseVersion(current)
	return !ok || c.Less(r)
}

var targetPattern = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)

// Target names the program for one operating system and processor.
func Target(goos, goarch string) string { return goos + "-" + goarch }

// ValidTarget reports whether s has the form of a target.
func ValidTarget(s string) bool { return targetPattern.MatchString(s) }
