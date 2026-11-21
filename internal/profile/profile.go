// Package profile reads who and where this computer is from files that
// already exist on it. geminus writes neither of them and has no files of
// its own.
package profile

import (
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
	// Secret signs prompts and verifies what the bridges write.
	Secret []byte
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
		From    string            `json:"from"`
		Members map[string]string `json:"members"`
		Secret  string            `json:"secret"`
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
	return p, nil
}
