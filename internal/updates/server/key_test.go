package server

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAKeyIsCreatedOnceAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "geminus", "release.key")
	created, err := CreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != ed25519.PrivateKeySize {
		t.Fatalf("the key has %d bytes", len(created))
	}
	loaded, err := LoadKey(path)
	if err != nil || !loaded.Equal(created) {
		t.Fatalf("LoadKey = %v, the same key: %v", err, loaded.Equal(created))
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("the key file is %v", info.Mode().Perm())
		}
		if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0o700 {
			t.Errorf("the key's directory is %v", info.Mode().Perm())
		}
	}
	// A key that exists is never replaced: every geminus trusts it.
	if _, err := CreateKey(path); err == nil {
		t.Error("CreateKey wrote over a key")
	}
	if again, _ := LoadKey(path); !again.Equal(created) {
		t.Error("the key changed")
	}
	// Two keys are not alike.
	other, err := CreateKey(filepath.Join(t.TempDir(), "release.key"))
	if err != nil || other.Equal(created) {
		t.Errorf("a second key: %v, the same as the first: %v", err, other.Equal(created))
	}
}

func TestAKeyFileOthersCanReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not say who can read a file there")
	}
	path := filepath.Join(t.TempDir(), "release.key")
	if _, err := CreateKey(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(path); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Errorf("mode %v: err = %v", mode, err)
		}
	}
}

func TestWhatIsNotAKeyIsNotLoadedAndNeverPrinted(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKey(filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), "-keygen") {
		t.Errorf("a missing key: %v", err)
	}
	for name, content := range map[string]string{
		"empty":   "",
		"short":   "abcdef0123",
		"not hex": strings.Repeat("zz", 32),
		"long":    strings.Repeat("ab", 33),
	} {
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKey(path)
		if err == nil {
			t.Errorf("%s: loaded", name)
			continue
		}
		if content != "" && strings.Contains(err.Error(), content) {
			t.Errorf("%s: the error shows the file's content: %v", name, err)
		}
	}
}
