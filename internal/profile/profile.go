// Package profile reads who and where this computer is from files that
// already exist on it. geminus writes neither of them and has no files of
// its own.
package profile

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvHermesHome overrides where the Hermes profile is read from (default
// ~/.hermes; on Windows %LOCALAPPDATA%\hermes, then %USERPROFILE%\.hermes).
const EnvHermesHome = "HERMES_HOME"

// HumanKeyFile is the optional Ed25519 seed file kept only on the owner's
// signing computer(s), inside HERMES_HOME (mode 0600, 64 hex characters).
const HumanKeyFile = "geminus_human.key"

// Where Windows keeps the profile when HERMES_HOME is not set.
const (
	envLocalAppData = "LOCALAPPDATA"
	envUserProfile  = "USERPROFILE"
)

// minSecretBytes is the shortest signing secret geminish accepts.
const minSecretBytes = 32

// Profile is what geminus needs to know about this computer and its owner.
type Profile struct {
	// BaseURL, Workspace and APIKey are the Honcho connection.
	BaseURL, Workspace, APIKey string
	// Human is the owner's peer; prompts are written under it.
	Human string
	// Local is this computer's own agent.
	Local string
	// Members maps every peer of the group to "agent" or "human".
	Members map[string]string
	// Secret signs legacy prompts and verifies what the bridges write.
	Secret []byte
	// HumanKey is the optional Ed25519 private key held only on the owner's computer(s).
	HumanKey ed25519.PrivateKey
	// HumanPubKey is the owner's configured Ed25519 public key (from geminise.json human_pubkeys,
	// or derived from HumanKey when present).
	HumanPubKey ed25519.PublicKey
	// RequireEd25519 is true when geminise.json sets geminus.human_auth = "ed25519_only".
	RequireEd25519 bool
}

// IsAgent reports whether name is an agent of the group.
func (p Profile) IsAgent(name string) bool { return p.Members[name] == "agent" }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// hermesHome is the directory of this computer's Hermes profile.
func hermesHome() (string, error) { return hermesHomeFor(runtime.GOOS, os.Getenv) }

// hermesHomeFor resolves the Hermes home for the given operating system from
// the given environment. HERMES_HOME is taken as given everywhere, without
// looking at the disk, as Hermes and the geminish bridge take it. Without it
// the home is ~/.hermes, except on Windows.
func hermesHomeFor(goos string, getenv func(string) string) (string, error) {
	if home := getenv(EnvHermesHome); home != "" {
		return home, nil
	}
	if goos == "windows" {
		return windowsHermesHome(getenv)
	}
	user := getenv("HOME")
	if user == "" {
		var err error
		if user, err = os.UserHomeDir(); err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
	}
	return filepath.Join(user, ".hermes"), nil
}

// windowsHermesHome returns the first of %LOCALAPPDATA%\hermes and
// %USERPROFILE%\.hermes that holds a honcho.json. Hermes on Windows lives in
// the first, and a shell that sets no HERMES_HOME must still find it. An
// unset variable is skipped, so no candidate is ever a relative path.
func windowsHermesHome(getenv func(string) string) (string, error) {
	var tried []string
	for _, c := range [][2]string{{getenv(envLocalAppData), "hermes"}, {getenv(envUserProfile), ".hermes"}} {
		if c[0] == "" {
			continue
		}
		dir := filepath.Join(c[0], c[1])
		// A file the user may not reach is still this computer's profile:
		// stop there and let Load say why, rather than read another one.
		if _, err := os.Stat(filepath.Join(dir, "honcho.json")); err == nil || errors.Is(err, fs.ErrPermission) {
			return dir, nil
		}
		tried = append(tried, dir)
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("no Hermes profile found: none of %s, %s and %s is set", EnvHermesHome, envLocalAppData, envUserProfile)
	}
	return "", fmt.Errorf("no Hermes profile found: no honcho.json in %s", strings.Join(tried, " or "))
}

// Load reads the Hermes profile's honcho.json and geminise.json.
func Load() (Profile, error) {
	home, err := hermesHome()
	if err != nil {
		return Profile{}, err
	}
	return loadFrom(home)
}

// loadFrom reads both files from the one directory home.
func loadFrom(home string) (Profile, error) {
	var honcho struct {
		BaseURL   string `json:"baseUrl"`
		Workspace string `json:"workspace"`
		APIKey    string `json:"apiKey"`
		AIPeer    string `json:"aiPeer"`
		PeerName  string `json:"peerName"`
	}
	if err := readJSON(filepath.Join(home, "honcho.json"), &honcho); err != nil {
		return Profile{}, fmt.Errorf("the Honcho connection of this computer's Hermes profile is not readable: %w", err)
	}
	var group struct {
		From         string            `json:"from"`
		Members      map[string]string `json:"members"`
		Secret       string            `json:"secret"`
		HumanPubKeys map[string]string `json:"human_pubkeys"`
		Geminus      json.RawMessage   `json:"geminus"`
	}
	if err := readJSON(filepath.Join(home, "geminise.json"), &group); err != nil {
		return Profile{}, fmt.Errorf("the group settings (members and signing secret) are not readable: %w", err)
	}
	secret, err := hex.DecodeString(group.Secret)
	if err != nil || len(secret) < minSecretBytes {
		return Profile{}, errors.New("geminise.json holds no usable signing secret")
	}
	p := Profile{
		BaseURL: honcho.BaseURL, Workspace: honcho.Workspace, APIKey: honcho.APIKey,
		Human: honcho.PeerName, Local: honcho.AIPeer, Members: group.Members, Secret: secret,
	}
	if p.Local == "" {
		p.Local = group.From
	}
	switch {
	case p.BaseURL == "" || p.Workspace == "" || p.APIKey == "":
		return Profile{}, errors.New("honcho.json needs baseUrl, workspace and apiKey")
	case p.Members[p.Human] != "human":
		return Profile{}, fmt.Errorf("the owner's peer %q is not listed as a human in geminise.json", p.Human)
	}
	if rawPub, ok := group.HumanPubKeys[p.Human]; ok && strings.TrimSpace(rawPub) != "" {
		pub, err := hex.DecodeString(strings.TrimSpace(rawPub))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return Profile{}, fmt.Errorf("geminise.json human_pubkeys[%q] must be 64 hex characters", p.Human)
		}
		p.HumanPubKey = ed25519.PublicKey(pub)
	}
	if len(group.Geminus) > 0 {
		var gOpts struct {
			HumanAuth string `json:"human_auth"`
		}
		if json.Unmarshal(group.Geminus, &gOpts) == nil && strings.EqualFold(strings.TrimSpace(gOpts.HumanAuth), "ed25519_only") {
			p.RequireEd25519 = true
		}
	}
	keyPath := filepath.Join(home, HumanKeyFile)
	if info, err := os.Stat(keyPath); err == nil {
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return Profile{}, fmt.Errorf("%s holds a private key and must be mode 0600 (is %04o)", keyPath, info.Mode().Perm())
		}
		rawKey, err := os.ReadFile(keyPath)
		if err != nil {
			return Profile{}, fmt.Errorf("%s: %w", keyPath, err)
		}
		seed, err := hex.DecodeString(strings.TrimSpace(string(rawKey)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return Profile{}, fmt.Errorf("%s must contain a 32-byte Ed25519 seed in hex (64 hex chars)", keyPath)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		derived := priv.Public().(ed25519.PublicKey)
		if len(p.HumanPubKey) > 0 && !bytes.Equal(p.HumanPubKey, derived) {
			return Profile{}, fmt.Errorf("%s does not match human_pubkeys[%q] in geminise.json", keyPath, p.Human)
		}
		p.HumanKey = priv
		if len(p.HumanPubKey) == 0 {
			p.HumanPubKey = derived
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Profile{}, fmt.Errorf("%s: %w", keyPath, err)
	}
	return p, nil
}

// GenerateHumanKey creates a new Ed25519 human signing key in the default
// Hermes profile directory. It never overwrites an existing key.
func GenerateHumanKey() (keyPath, pubHex string, err error) {
	home, err := hermesHome()
	if err != nil {
		return "", "", err
	}
	return GenerateHumanKeyIn(home)
}

// GenerateHumanKeyIn creates a new Ed25519 human signing key at <home>/geminus_human.key
// (mode 0600). Refuses to overwrite an existing key file.
func GenerateHumanKeyIn(home string) (keyPath, pubHex string, err error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", "", err
	}
	keyPath = filepath.Join(home, HumanKeyFile)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("%s already exists; refusing to overwrite an existing human key", keyPath)
		}
		return "", "", err
	}
	_, werr := f.WriteString(hex.EncodeToString(priv.Seed()) + "\n")
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(keyPath)
		return "", "", werr
	}
	if cerr != nil {
		_ = os.Remove(keyPath)
		return "", "", cerr
	}
	return keyPath, hex.EncodeToString(pub), nil
}
