package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	got := String()
	for _, want := range []string{"geminus", Version, runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("String() = %q, want a single line", got)
	}
}

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version is empty")
	}
}
