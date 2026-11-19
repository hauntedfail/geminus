// Package persona answers two questions: which agents can be talked to (the
// Honcho workspace's peers), and how to reach each one (the routes in
// geminus's own config file).
package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// EnvHome overrides geminus's state directory (default ~/.geminus).
	EnvHome = "GEMINUS_HOME"
	// EnvHermesHome overrides where the Hermes profile is read from
	// (default ~/.hermes).
	EnvHermesHome = "HERMES_HOME"

	configName   = "config.json"
	listPageSize = 50
	maxListPages = 20
	listTimeout  = 15 * time.Second
)

// Route says how to reach a persona's agent.
type Route struct {
	// SSH is an ssh host alias. Empty means the agent runs on this computer.
	SSH string `json:"ssh,omitempty"`
	// Runtime is "hermes" or "claude".
	Runtime string `json:"runtime"`
	// CWD is the working directory for sessions on the agent's machine.
	// Required for ssh routes; defaults to the current directory otherwise.
	CWD string `json:"cwd,omitempty"`
	// Command replaces the default agent command line (see AgentCommand).
	Command string `json:"command,omitempty"`
}

// Describe returns a short human-readable form, such as "hermes via ssh btcvps".
func (r Route) Describe() string {
	if r.SSH == "" {
		return r.Runtime + " (local)"
	}
	return r.Runtime + " via ssh " + r.SSH
}

// Persona is one agent that can be talked to.
type Persona struct {
	Name string
	// Local means the persona is this computer's own agent.
	Local bool
	// Route is nil when geminus has not been told how to reach the persona.
	Route *Route
}

// Tag returns the locality label shown next to the persona's name.
func (p Persona) Tag() string {
	if p.Local {
		return "THIS COMPUTER"
	}
	return "REMOTE"
}

// Directory is the result of Load.
type Directory struct {
	// Workspace is the Honcho workspace the personas were read from.
	Workspace string
	Personas  []Persona
	// Warning is set when Honcho could not be read and Personas holds only
	// what the config file and the local profile name.
	Warning string
	// ConfigPath is where routes are configured.
	ConfigPath string
}

// Find returns the persona with the given name.
func (d Directory) Find(name string) (Persona, bool) {
	for _, p := range d.Personas {
		if p.Name == name {
			return p, true
		}
	}
	return Persona{}, false
}

type config struct {
	Personas map[string]Route `json:"personas"`
	// Honcho overrides the connection read from the Hermes profile.
	Honcho *struct {
		BaseURL   string `json:"baseUrl"`
		Workspace string `json:"workspace"`
		// APIKeyEnv names an environment variable holding the API key.
		APIKeyEnv string `json:"apiKeyEnv"`
	} `json:"honcho"`
}

// honchoProfile is the part of Hermes's honcho.json that geminus reads.
type honchoProfile struct {
	BaseURL   string `json:"baseUrl"`
	Workspace string `json:"workspace"`
	APIKey    string `json:"apiKey"`
	// AIPeer is this computer's agent; PeerName is the human.
	AIPeer   string `json:"aiPeer"`
	PeerName string `json:"peerName"`
}

// membership is the part of geminise.json that geminus reads: which peers
// are agents and which are humans.
type membership struct {
	Members map[string]string `json:"members"`
}

func homeDir(env, fallback string) (string, error) {
	if dir := os.Getenv(env); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, fallback), nil
}

// readJSON decodes path into v. A missing file is not an error; found
// reports whether the file existed.
func readJSON(path string, v any) (found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// Load reads the personas of the Honcho workspace and attaches the routes
// from the config file. If Honcho cannot be read, it still returns the
// personas it knows locally and explains why in Directory.Warning.
func Load(ctx context.Context) (Directory, error) {
	home, err := homeDir(EnvHome, ".geminus")
	if err != nil {
		return Directory{}, err
	}
	hermesHome, err := homeDir(EnvHermesHome, ".hermes")
	if err != nil {
		return Directory{}, err
	}
	dir := Directory{ConfigPath: filepath.Join(home, configName)}

	var cfg config
	if _, err := readJSON(dir.ConfigPath, &cfg); err != nil {
		return dir, err
	}
	for name, route := range cfg.Personas {
		if err := route.validate(); err != nil {
			return dir, fmt.Errorf("%s: persona %q: %w", dir.ConfigPath, name, err)
		}
	}
	var profile honchoProfile
	if _, err := readJSON(filepath.Join(hermesHome, "honcho.json"), &profile); err != nil {
		return dir, err
	}
	var members membership
	if _, err := readJSON(filepath.Join(hermesHome, "geminise.json"), &members); err != nil {
		return dir, err
	}
	if cfg.Honcho != nil {
		if cfg.Honcho.BaseURL != "" {
			profile.BaseURL = cfg.Honcho.BaseURL
		}
		if cfg.Honcho.Workspace != "" {
			profile.Workspace = cfg.Honcho.Workspace
		}
		if cfg.Honcho.APIKeyEnv != "" {
			profile.APIKey = os.Getenv(cfg.Honcho.APIKeyEnv)
		}
	}
	dir.Workspace = profile.Workspace

	names, listErr := listPeers(ctx, profile)
	if listErr != nil {
		dir.Warning = "Honcho directory unavailable: " + listErr.Error()
		names = nil
		for name := range cfg.Personas {
			names = append(names, name)
		}
		if profile.AIPeer != "" {
			names = append(names, profile.AIPeer)
		}
	}

	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] || !isAgent(name, profile, members) {
			continue
		}
		seen[name] = true
		p := Persona{Name: name, Local: name == profile.AIPeer}
		if route, ok := cfg.Personas[name]; ok {
			p.Route = &route
		} else if p.Local {
			// This computer's own agent needs no configuration.
			p.Route = &Route{Runtime: "hermes"}
		}
		dir.Personas = append(dir.Personas, p)
	}
	sort.Slice(dir.Personas, func(i, j int) bool { return dir.Personas[i].Name < dir.Personas[j].Name })
	return dir, nil
}

// isAgent filters out the human and, when a geminise membership exists,
// every peer that it does not list as an agent (Honcho workspaces collect
// stray peers).
func isAgent(name string, profile honchoProfile, members membership) bool {
	if name == "" || name == profile.PeerName {
		return false
	}
	if len(members.Members) == 0 {
		return true
	}
	return members.Members[name] == "agent"
}

// listPeers returns the IDs of every peer in the Honcho workspace.
func listPeers(ctx context.Context, profile honchoProfile) ([]string, error) {
	switch {
	case profile.BaseURL == "" || profile.Workspace == "":
		return nil, errors.New("no Honcho connection is configured (expected baseUrl and workspace in the Hermes profile's honcho.json)")
	case profile.APIKey == "":
		return nil, errors.New("no Honcho API key is configured")
	}
	base, err := url.Parse(profile.BaseURL)
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("invalid Honcho base URL %q", profile.BaseURL)
	}
	endpoint := base.JoinPath("v3", "workspaces", profile.Workspace, "peers", "list")

	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	var names []string
	for page := 1; page <= maxListPages; page++ {
		endpoint.RawQuery = fmt.Sprintf("page=%d&size=%d", page, listPageSize)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader("{}"))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+profile.APIKey)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			// The error text carries the URL but never the API key.
			return nil, err
		}
		var body struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
			Pages int `json:"pages"`
		}
		decodeErr := json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Honcho answered HTTP %d", res.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("unreadable Honcho response: %w", decodeErr)
		}
		for _, item := range body.Items {
			names = append(names, item.ID)
		}
		if page >= body.Pages {
			break
		}
	}
	return names, nil
}
