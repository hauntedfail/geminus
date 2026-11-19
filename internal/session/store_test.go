package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock advances by step on each call so update order is deterministic.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock := &fakeClock{now: time.Date(2026, 11, 19, 10, 57, 0, 0, time.Local), step: time.Second}
	store.now = clock.Now
	return store, clock
}

func mustCreate(t *testing.T, store *Store, title string) Meta {
	t.Helper()
	meta, err := store.Create(CreateParams{Title: title, Agent: "claude", Cwd: "/work"})
	if err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	return meta
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows has no POSIX permission bits.
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: mode = %o, want %o", path, got, want)
	}
}

func assertMetaEqual(t *testing.T, got, want Meta) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("meta mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestOpenRejectsEmptyHome(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded, want error")
	}
}

func TestCreate(t *testing.T) {
	store, _ := newTestStore(t)
	key := &AgentSession{Kind: "uuid", Value: "3f2c9a52-6f0e-4c53-9a43-0d6c2b1f7e11"}
	meta, err := store.Create(CreateParams{Title: "geminus design", Agent: "claude", AgentSession: key, Cwd: "/work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !regexp.MustCompile(`^20261119-105700-[0-9a-f]{4}$`).MatchString(meta.ID) {
		t.Errorf("ID = %q, want 20261119-105700-<4hex>", meta.ID)
	}
	if meta.Version != SchemaVersion {
		t.Errorf("Version = %d, want %d", meta.Version, SchemaVersion)
	}
	if meta.Status != StatusCreated {
		t.Errorf("Status = %q, want %q", meta.Status, StatusCreated)
	}
	if meta.Title != "geminus design" || meta.Agent != "claude" || meta.Cwd != "/work" {
		t.Errorf("unexpected fields: %+v", meta)
	}
	if meta.AgentSession == nil || *meta.AgentSession != *key {
		t.Errorf("AgentSession = %+v, want %+v", meta.AgentSession, key)
	}
	if meta.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil before exit", *meta.ExitCode)
	}
	if meta.CreatedAt.IsZero() || !meta.CreatedAt.Equal(meta.UpdatedAt) {
		t.Errorf("CreatedAt = %v, UpdatedAt = %v, want equal and non-zero", meta.CreatedAt, meta.UpdatedAt)
	}

	paths, err := store.Paths(meta.ID)
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if want := filepath.Join(store.Home(), "sessions", meta.ID); paths.Dir != want {
		t.Errorf("Dir = %q, want %q", paths.Dir, want)
	}
	assertPerm(t, store.Home(), 0o700)
	assertPerm(t, filepath.Join(store.Home(), "sessions"), 0o700)
	assertPerm(t, paths.Dir, 0o700)
	assertPerm(t, paths.Meta, 0o600)

	// Changes to the caller's values must not affect the stored metadata.
	key.Value = "changed"
	if meta.AgentSession.Value == "changed" {
		t.Error("Create kept a reference to the caller's AgentSession")
	}
}

func TestCreateTightensLooseUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions only")
	}
	// Session directories and files must be private even if the existing home directory has loose
	// permissions.
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	meta := mustCreate(t, store, "x")
	paths, err := store.Paths(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPerm(t, paths.Dir, 0o700)
	assertPerm(t, paths.Meta, 0o600)
}

func TestCreateGeneratesUniqueIDs(t *testing.T) {
	store, clock := newTestStore(t)
	clock.step = 0 // IDs must not collide even within the same second.
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		meta := mustCreate(t, store, "x")
		if seen[meta.ID] {
			t.Fatalf("duplicate id %s", meta.ID)
		}
		seen[meta.ID] = true
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "round trip")

	code := 7
	meta.Title = "renamed"
	meta.AgentSession = &AgentSession{Kind: "name", Value: meta.ID}
	meta.Status = StatusExited
	meta.ExitCode = &code
	meta.PID = 4242
	meta.Turns = 3
	meta.RawBytes = 18422

	saved, err := store.Save(meta)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !saved.UpdatedAt.After(meta.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want after %v", saved.UpdatedAt, meta.UpdatedAt)
	}
	if !saved.CreatedAt.Equal(meta.CreatedAt) {
		t.Errorf("CreatedAt changed: %v -> %v", meta.CreatedAt, saved.CreatedAt)
	}

	loaded, err := store.Load(meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertMetaEqual(t, loaded, saved)
	if loaded.ExitCode == nil || *loaded.ExitCode != 7 {
		t.Errorf("ExitCode = %v, want 7", loaded.ExitCode)
	}
	if !loaded.UpdatedAt.Equal(saved.UpdatedAt) || !loaded.CreatedAt.Equal(saved.CreatedAt) {
		t.Errorf("timestamps differ after reload")
	}
}

func TestSessionJSONShape(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "shape")
	code := 0
	meta.Status = StatusExited
	meta.ExitCode = &code
	if _, err := store.Save(meta); err != nil {
		t.Fatal(err)
	}
	paths, _ := store.Paths(meta.ID)
	data, err := os.ReadFile(paths.Meta)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("session.json is not a JSON object: %v", err)
	}
	// Check the field names from plan.md section 3.2. Include exit_code even when zero.
	for _, name := range []string{"version", "id", "title", "agent", "cwd", "created_at", "updated_at", "status", "exit_code", "turns", "raw_bytes"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("session.json lacks %q: %s", name, data)
		}
	}
	if string(fields["exit_code"]) != "0" {
		t.Errorf("exit_code = %s, want 0", fields["exit_code"])
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("session.json does not end with a newline")
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "tmp")
	for i := 0; i < 5; i++ {
		var err error
		if meta, err = store.Save(meta); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := store.Paths(meta.ID)
	entries, err := os.ReadDir(paths.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "session.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("session dir = %v, want only session.json", names)
	}
}

// Readers must always see a complete session.json, even during a write.
func TestSaveIsAtomicForReaders(t *testing.T) {
	store, clock := newTestStore(t)
	clock.step = time.Millisecond
	meta := mustCreate(t, store, strings.Repeat("long title ", 200))
	id := meta.ID // The reader goroutine does not share meta.

	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readErr)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := store.Load(id); err != nil {
				readErr <- err
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		meta.Turns = i
		var err error
		if meta, err = store.Save(meta); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}
	close(stop)
	if err := <-readErr; err != nil {
		t.Fatalf("reader saw a partial write: %v", err)
	}
}

func TestSaveErrors(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")

	t.Run("invalid id", func(t *testing.T) {
		bad := meta
		bad.ID = "../escape"
		if _, err := store.Save(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("err = %v, want ErrInvalidID", err)
		}
	})
	t.Run("deleted session is not resurrected", func(t *testing.T) {
		if err := store.Delete(meta.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(meta); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestUpdate(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")

	updated, err := store.Update(meta.ID, func(m Meta) Meta {
		m.Status = StatusRunning
		m.PID = os.Getpid()
		return m
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != StatusRunning || updated.PID != os.Getpid() {
		t.Errorf("updated = %+v", updated)
	}
	loaded, err := store.Load(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertMetaEqual(t, loaded, updated)

	t.Run("change cannot move the session to another id", func(t *testing.T) {
		got, err := store.Update(meta.ID, func(m Meta) Meta {
			m.ID = "20200101-000000-0000"
			return m
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != meta.ID {
			t.Errorf("ID = %q, want %q", got.ID, meta.ID)
		}
	})
	t.Run("missing session", func(t *testing.T) {
		_, err := store.Update("20200101-000000-0000", func(m Meta) Meta { return m })
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestListOrdersByUpdatedAtDescending(t *testing.T) {
	store, _ := newTestStore(t)

	empty, err := store.List()
	if err != nil || len(empty) != 0 {
		t.Fatalf("List on empty home = %v, %v; want empty, nil", empty, err)
	}

	first := mustCreate(t, store, "first")
	second := mustCreate(t, store, "second")
	third := mustCreate(t, store, "third")

	titles := func(metas []Meta) string {
		names := make([]string, 0, len(metas))
		for _, m := range metas {
			names = append(names, m.Title)
		}
		return strings.Join(names, ",")
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := titles(list); got != "third,second,first" {
		t.Errorf("order = %s, want third,second,first", got)
	}

	// Updating the oldest session moves it to the front; the order is not by creation time.
	if _, err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	list, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := titles(list); got != "first,third,second" {
		t.Errorf("order after update = %s, want first,third,second", got)
	}
	_ = second
	_ = third
}

func TestListBreaksTiesByID(t *testing.T) {
	store, clock := newTestStore(t)
	clock.step = 0
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, mustCreate(t, store, "x").ID)
	}
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID < list[i].ID {
			t.Fatalf("ids not descending on equal updated_at: %s before %s", list[i-1].ID, list[i].ID)
		}
	}
	if len(list) != len(ids) {
		t.Errorf("len = %d, want %d", len(list), len(ids))
	}
}

func TestListReportsBrokenSessionsAndKeepsTheRest(t *testing.T) {
	store, _ := newTestStore(t)
	good := mustCreate(t, store, "good")
	broken := mustCreate(t, store, "broken")
	paths, _ := store.Paths(broken.ID)
	if err := os.WriteFile(paths.Meta, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ignore entries whose names do not look like session IDs.
	sessions := filepath.Join(store.Home(), "sessions")
	if err := os.WriteFile(filepath.Join(sessions, ".DS_Store"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sessions, "not-a-session"), 0o700); err != nil {
		t.Fatal(err)
	}

	list, err := store.List()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), broken.ID) {
		t.Errorf("err = %v, want it to name %s", err, broken.ID)
	}
	if len(list) != 1 || list[0].ID != good.ID {
		t.Errorf("list = %+v, want only %s", list, good.ID)
	}
}

func TestDelete(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	paths, _ := store.Paths(meta.ID)
	if err := os.WriteFile(paths.Raw, []byte("raw"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(paths.Dir); !os.IsNotExist(err) {
		t.Errorf("session dir still exists (stat err = %v)", err)
	}
	if err := store.Delete(meta.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete err = %v, want ErrNotFound", err)
	}
	if _, err := store.Load(meta.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after Delete err = %v, want ErrNotFound", err)
	}
}

// A user-supplied id must not allow access outside the session store.
func TestInvalidIDsAreRejected(t *testing.T) {
	store, _ := newTestStore(t)
	mustCreate(t, store, "x")
	outside := filepath.Join(store.Home(), "keep.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	ids := []string{"", ".", "..", "../keep.txt", "../..", "/etc", "20261119-105700-a1b2/..", "20261119-105700-A1B2", "20261119-105700-a1b", "x20261119-105700-a1b2", "20261119-105700-a1b2\n"}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if err := store.Delete(id); !errors.Is(err, ErrInvalidID) {
				t.Errorf("Delete err = %v, want ErrInvalidID", err)
			}
			if _, err := store.Load(id); !errors.Is(err, ErrInvalidID) {
				t.Errorf("Load err = %v, want ErrInvalidID", err)
			}
			if _, err := store.Paths(id); !errors.Is(err, ErrInvalidID) {
				t.Errorf("Paths err = %v, want ErrInvalidID", err)
			}
			if _, err := store.OpenLog(id, LogOptions{}); !errors.Is(err, ErrInvalidID) {
				t.Errorf("OpenLog err = %v, want ErrInvalidID", err)
			}
		})
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside sessions/ was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Home(), "sessions")); err != nil {
		t.Errorf("sessions dir was touched: %v", err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	paths, _ := store.Paths(meta.ID)
	valid, err := os.ReadFile(paths.Meta)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		content string
		remove  bool
		want    error
	}{
		{name: "garbage", content: "\x00\x01\x02 not json", want: ErrCorrupt},
		{name: "empty file", content: "", want: ErrCorrupt},
		{name: "truncated", content: string(valid[:len(valid)/2]), want: ErrCorrupt},
		{name: "json array", content: "[]", want: ErrCorrupt},
		{name: "json null", content: "null", want: ErrCorrupt},
		{name: "wrong field type", content: `{"version":1,"id":"` + meta.ID + `","turns":"three"}`, want: ErrCorrupt},
		{name: "bad timestamp", content: `{"version":1,"id":"` + meta.ID + `","created_at":"yesterday"}`, want: ErrCorrupt},
		{name: "id does not match directory", content: `{"version":1,"id":"20200101-000000-0000"}`, want: ErrCorrupt},
		{name: "missing version", content: `{"id":"` + meta.ID + `"}`, want: ErrCorrupt},
		{name: "newer schema", content: `{"version":99,"id":"` + meta.ID + `"}`, want: ErrUnsupportedVersion},
		{name: "session.json missing", remove: true, want: ErrCorrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.remove {
				if err := os.Remove(paths.Meta); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(paths.Meta, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(meta.ID) // Also check that Load does not panic.
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), meta.ID) {
				t.Errorf("err = %v, want it to name the session", err)
			}
		})
	}
}

func TestResolveHome(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)

	t.Run("default", func(t *testing.T) {
		t.Setenv(EnvHome, "")
		got, err := ResolveHome("")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(userHome, ".geminus"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv(EnvHome, filepath.Join(userHome, "env-home"))
		got, err := ResolveHome("")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(userHome, "env-home"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("override wins over environment", func(t *testing.T) {
		t.Setenv(EnvHome, filepath.Join(userHome, "env-home"))
		got, err := ResolveHome(filepath.Join(userHome, "flag-home"))
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(userHome, "flag-home"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("relative path becomes absolute", func(t *testing.T) {
		got, err := ResolveHome("rel-home")
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(got) || filepath.Base(got) != "rel-home" {
			t.Errorf("got %q, want an absolute path ending in rel-home", got)
		}
	})
}

func TestValidID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"20261119-105700-a1b2", true},
		{"20261119-105700-0000", true},
		{"20261119-105700-A1B2", false},
		{"20261119-105700-a1b", false},
		{"20261119-105700-a1b2c", false},
		{"2026111-105700-a1b2", false},
		{"20261119_105700_a1b2", false},
		{"", false},
		{"..", false},
	}
	for _, tt := range tests {
		if got := ValidID(tt.id); got != tt.want {
			t.Errorf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestNewIDUsesLocalTimestamp(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	id, err := NewID(at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "20260102-030405-") || !ValidID(id) {
		t.Errorf("id = %q", id)
	}
}

func TestCreateFailsWhenHomeIsAFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(CreateParams{Agent: "claude"}); err == nil {
		t.Fatal("Create succeeded with a regular file as home")
	}
	if list, err := store.List(); err == nil {
		t.Errorf("List = %v, nil; want an error for a home that is not a directory", list)
	}
}

// An unwritable location must produce an error without leaving a partial session or temporary
// files.
func TestWriteFailuresLeaveNoDebris(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	store, _ := newTestStore(t)
	meta := mustCreate(t, store, "x")
	paths, _ := store.Paths(meta.ID)
	sessions := filepath.Join(store.Home(), "sessions")

	readOnly := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
	}

	t.Run("save", func(t *testing.T) {
		readOnly(t, paths.Dir)
		if _, err := store.Save(meta); err == nil {
			t.Fatal("Save succeeded in a read-only session directory")
		}
		entries, err := os.ReadDir(paths.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("session dir has %d entries after a failed Save, want 1", len(entries))
		}
		if _, err := store.OpenLog(meta.ID, LogOptions{Raw: true}); err == nil {
			t.Error("OpenLog succeeded in a read-only session directory")
		}
	})
	t.Run("create", func(t *testing.T) {
		readOnly(t, sessions)
		if _, err := store.Create(CreateParams{Agent: "claude"}); err == nil {
			t.Fatal("Create succeeded in a read-only sessions directory")
		}
		entries, err := os.ReadDir(sessions)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("sessions dir has %d entries after a failed Create, want 1", len(entries))
		}
	})
}
