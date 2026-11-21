package updates

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// On Windows the running program is moved aside before the new one takes
// its name. If that second step fails, the program must be back.
func TestTheProgramIsPutBackWhenTheNewOneCannotTakeItsName(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "geminus.exe")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the disk said no")
	rename = func(from, to string) error {
		if from == exe+newSuffix {
			return broken
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { rename = os.Rename })
	err := Replace(exe, "windows", func(w io.Writer) error { _, err := io.WriteString(w, "new"); return err })
	if !errors.Is(err, broken) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Errorf("program = %q", got)
	}
	left, _ := os.ReadDir(filepath.Dir(exe))
	if len(left) != 1 {
		t.Errorf("%d files beside the program", len(left))
	}
}
