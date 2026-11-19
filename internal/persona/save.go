package persona

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// edit rewrites one top-level key of the config file and leaves every other
// key, including ones this version does not know, as it was.
func edit(key string, change func(current json.RawMessage) (any, error)) error {
	home, err := homeDir(EnvHome, ".geminus")
	if err != nil {
		return err
	}
	path := filepath.Join(home, configName)
	doc := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	value, err := change(doc[key])
	if err != nil {
		return err
	}
	if doc[key], err = json.Marshal(value); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	// Write beside the file and rename, so a crash cannot leave half a config.
	tmp, err := os.CreateTemp(home, "."+configName+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SaveDefaults stores the defaults in the config file.
func SaveDefaults(d Defaults) error {
	return edit("defaults", func(json.RawMessage) (any, error) { return d, nil })
}

// SaveNode stores where a persona runs, keeping the other personas.
func SaveNode(name string, node Node) error {
	if err := node.validate(); err != nil {
		return err
	}
	return edit("personas", func(current json.RawMessage) (any, error) {
		nodes := map[string]Node{}
		if len(current) > 0 {
			if err := json.Unmarshal(current, &nodes); err != nil {
				return nil, fmt.Errorf("personas: %w", err)
			}
		}
		nodes[name] = node
		return nodes, nil
	})
}
