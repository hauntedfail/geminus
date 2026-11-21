package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The key that signs releases is a file on this computer: the seed of an
// Ed25519 key, in hex, readable by its owner only. It is made here and is
// never copied anywhere; only its public half leaves, inside every geminus.

// CreateKey makes a new key and writes it to path. It fails if a file is
// there already.
func CreateKey(path string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(hex.EncodeToString(seed) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, errors.Join(err, os.Remove(path))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// LoadKey reads the key CreateKey wrote. An error never shows the file's
// content.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("there is no release key at %s; make one with -keygen", path)
	}
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("the release key %s can be read by others (%04o); it must be 0600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a release key (64 hex digits)", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
