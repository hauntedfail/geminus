// Package profile reads who and where this computer is from files that
// already exist on it. geminus writes neither of them and has no files of
// its own.
package profile

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnvHermesHome overrides where the Hermes profile is read from (default
// ~/.hermes).
const EnvHermesHome = "HERMES_HOME"

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

// Load reads the Hermes profile's honcho.json and geminise.json.
func Load() (Profile, error) {
	home := os.Getenv(EnvHermesHome)
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return Profile{}, fmt.Errorf("resolve home directory: %w", err)
		}
		home = filepath.Join(user, ".hermes")
	}
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
