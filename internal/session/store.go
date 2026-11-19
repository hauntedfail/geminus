package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// EnvHome names the environment variable that overrides the state directory.
const EnvHome = "GEMINUS_HOME"

const (
	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600

	sessionsDirName = "sessions"
	metaFileName    = "session.json"
	transcriptName  = "transcript.jsonl"
	rawFileName     = "raw.log"

	// Number of retries if IDs generated in the same second collide.
	maxIDAttempts = 16
)

// ResolveHome chooses the state directory: override (--home) takes precedence over GEMINUS_HOME,
// which takes precedence over ~/.geminus.
func ResolveHome(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if env := os.Getenv(EnvHome); env != "" {
		return filepath.Abs(env)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(userHome, ".geminus"), nil
}

// Store reads and writes sessions under one GEMINUS_HOME.
type Store struct {
	home string
	now  func() time.Time
}

// Open returns a Store rooted at home. It creates no directory until the first Create, so commands
// such as ls and doctor have no side effects.
func Open(home string) (*Store, error) {
	if home == "" {
		return nil, errors.New("session: home directory is empty")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("session: resolve home %q: %w", home, err)
	}
	return &Store{home: abs, now: time.Now}, nil
}

// Home returns the absolute path to the state directory.
func (s *Store) Home() string { return s.home }

func (s *Store) sessionsDir() string { return filepath.Join(s.home, sessionsDirName) }

// Paths holds the paths to a session's files.
type Paths struct {
	Dir        string
	Meta       string
	Transcript string
	Raw        string
}

// Paths returns the paths for session id without checking whether they exist.
func (s *Store) Paths(id string) (Paths, error) {
	if err := checkID(id); err != nil {
		return Paths{}, err
	}
	dir := filepath.Join(s.sessionsDir(), id)
	return Paths{
		Dir:        dir,
		Meta:       filepath.Join(dir, metaFileName),
		Transcript: filepath.Join(dir, transcriptName),
		Raw:        filepath.Join(dir, rawFileName),
	}, nil
}

// CreateParams holds the initial values for a new session.
type CreateParams struct {
	Title string
	Agent string
	// Set AgentSession if known. For keys derived from the ID (hermes), use Update after Create.
	AgentSession *AgentSession
	Cwd          string
}

// Create creates a session directory and its session.json.
func (s *Store) Create(p CreateParams) (Meta, error) {
	if err := s.ensureSessionsDir(); err != nil {
		return Meta{}, err
	}
	now := s.timestamp()
	id, err := s.makeSessionDir(now)
	if err != nil {
		return Meta{}, err
	}

	meta := Meta{
		Version:      SchemaVersion,
		ID:           id,
		Title:        p.Title,
		Agent:        p.Agent,
		AgentSession: p.AgentSession,
		Cwd:          p.Cwd,
		CreatedAt:    now,
		UpdatedAt:    now,
		Status:       StatusCreated,
	}.clone()
	if err := s.writeMeta(meta); err != nil {
		// Do not leave an empty session behind.
		if rmErr := os.RemoveAll(filepath.Join(s.sessionsDir(), id)); rmErr != nil {
			return Meta{}, errors.Join(err, rmErr)
		}
		return Meta{}, err
	}
	return meta, nil
}

// Load reads session.json for session id. It returns ErrNotFound for a missing session, ErrCorrupt
// for malformed content, or ErrNotRegular if session.json is a symlink or other nonregular file.
func (s *Store) Load(id string) (Meta, error) {
	paths, err := s.existingPaths(id)
	if err != nil {
		return Meta{}, err
	}
	data, err := readRegular(paths.Meta)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Meta{}, fmt.Errorf("session %s: %w: %s is missing", id, ErrCorrupt, metaFileName)
	case err != nil:
		return Meta{}, fmt.Errorf("session %s: read %s: %w", id, metaFileName, err)
	}

	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("session %s: %w: %v", id, ErrCorrupt, err)
	}
	switch {
	case meta.Version > SchemaVersion:
		return Meta{}, fmt.Errorf("session %s: %w: version %d (this build reads up to %d)", id, ErrUnsupportedVersion, meta.Version, SchemaVersion)
	case meta.Version < 1:
		return Meta{}, fmt.Errorf("session %s: %w: missing version", id, ErrCorrupt)
	case meta.ID != id:
		return Meta{}, fmt.Errorf("session %s: %w: %s holds id %q", id, ErrCorrupt, metaFileName, meta.ID)
	}
	return meta, nil
}

// Save writes meta to session.json atomically and returns a copy with an updated updated_at. It
// leaves meta unchanged and returns ErrNotFound rather than recreating a deleted session.
func (s *Store) Save(meta Meta) (Meta, error) {
	if _, err := s.existingPaths(meta.ID); err != nil {
		return Meta{}, err
	}

	next := meta.clone()
	next.Version = SchemaVersion
	next.UpdatedAt = s.timestamp()
	if err := s.writeMeta(next); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Another process deleted the session between the check and the write.
			return Meta{}, fmt.Errorf("session %s: %w", meta.ID, ErrNotFound)
		}
		return Meta{}, err
	}
	return next, nil
}

// Update reads the session metadata and saves the value returned by change. It uses the original
// ID even if change modifies it.
func (s *Store) Update(id string, change func(Meta) Meta) (Meta, error) {
	current, err := s.Load(id)
	if err != nil {
		return Meta{}, err
	}
	next := change(current.clone())
	next.ID = id
	return s.Save(next)
}

// List returns all sessions by descending updated_at, breaking ties by descending ID.
//
// List returns readable sessions even when others fail, combining the failures into one error.
// Callers can use the slice even when the error is non-nil.
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.sessionsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	var metas []Meta
	var broken []error
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		meta, err := s.Load(entry.Name())
		if err != nil {
			broken = append(broken, err)
			continue
		}
		metas = append(metas, meta)
	}
	sort.SliceStable(metas, func(i, j int) bool {
		if !metas[i].UpdatedAt.Equal(metas[j].UpdatedAt) {
			return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
		}
		return metas[i].ID > metas[j].ID
	})
	return metas, errors.Join(broken...)
}

// Delete removes the entire session directory, even if session.json is corrupt. It returns
// ErrNotFound if the session does not exist.
func (s *Store) Delete(id string) error {
	paths, err := s.Paths(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(paths.Dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("session %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("session %s: %w", id, err)
	}
	if err := os.RemoveAll(paths.Dir); err != nil {
		return fmt.Errorf("session %s: delete: %w", id, err)
	}
	return nil
}

// timestamp removes the monotonic clock reading from the current time so it compares equal after a
// JSON round trip.
func (s *Store) timestamp() time.Time {
	return s.now().Round(0)
}

func (s *Store) ensureSessionsDir() error {
	if err := os.MkdirAll(filepath.Dir(s.home), dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.home), err)
	}
	for _, dir := range []string{s.home, s.sessionsDir()} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// ensurePrivateDir creates dir with mode 0700 if it does not exist. Leave existing permissions
// unchanged because the user may have supplied this location with --home.
func ensurePrivateDir(dir string) error {
	err := os.Mkdir(dir, dirPerm)
	if err == nil {
		// Use mode 0700 even if the umask removed some bits.
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		return fmt.Errorf("stat %s: %w", dir, statErr)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	return nil
}

// makeSessionDir chooses an unused ID and creates its directory. Because os.Mkdir rejects existing
// paths, directory creation also reserves the ID in one operation.
func (s *Store) makeSessionDir(now time.Time) (string, error) {
	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		id, err := NewID(now)
		if err != nil {
			return "", err
		}
		dir := filepath.Join(s.sessionsDir(), id)
		err = os.Mkdir(dir, dirPerm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create session directory: %w", err)
		}
		if err := os.Chmod(dir, dirPerm); err != nil {
			return "", fmt.Errorf("chmod %s: %w", dir, err)
		}
		return id, nil
	}
	return "", fmt.Errorf("create session directory: no free id after %d attempts", maxIDAttempts)
}

func (s *Store) writeMeta(meta Meta) error {
	paths, err := s.Paths(meta.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("session %s: encode: %w", meta.ID, err)
	}
	if err := writeFileAtomic(paths.Meta, append(data, '\n')); err != nil {
		return fmt.Errorf("session %s: write %s: %w", meta.ID, metaFileName, err)
	}
	return nil
}

// writeFileAtomic writes to a temporary file in the same directory, then renames it into place.
// Readers see either the old contents or the new contents.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			err = errors.Join(err, tmp.Close(), removeIfExists(tmpPath))
		}
	}()

	if err = tmp.Chmod(filePerm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return replaceFile(tmpPath, path)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// replaceFile replaces the file by renaming. On Windows a concurrent reader may cause a sharing
// violation, so retry after a short delay.
func replaceFile(from, to string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 5
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return err
}
