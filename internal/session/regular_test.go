package session

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sessionFiles は読み取りで開く3つのファイル。
var sessionFiles = []string{metaFileName, transcriptName, rawFileName}

// readers は、セッションのファイルを読む公開 API を名前付きで並べる。
// どれも、通常ファイルでないものを見つけたら ErrNotRegular を返すこと。
func readers(store *Store, id string) map[string]func() error {
	return map[string]func() error{
		"Load": func() error {
			_, err := store.Load(id)
			return err
		},
		"ReadEvents": func() error {
			_, err := store.ReadEvents(id)
			return err
		},
		"OpenRaw": func() error {
			f, err := store.OpenRaw(id)
			if err == nil {
				f.Close()
			}
			return err
		},
		"OpenLog": func() error {
			log, err := store.OpenLog(id, LogOptions{})
			if err == nil {
				log.Close()
			}
			return err
		},
	}
}

// readerOf は file を読む API の名前を返す。
func readerOf(file string) []string {
	switch file {
	case metaFileName:
		return []string{"Load"}
	case transcriptName:
		return []string{"ReadEvents", "OpenLog"}
	default:
		return []string{"OpenRaw", "OpenLog"}
	}
}

// replaceWith は セッションの file を消し、plant に同じ名前で別のものを置かせる。
func replaceWith(t *testing.T, store *Store, id, file string, plant func(path string) error) {
	t.Helper()
	paths, err := store.Paths(id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.Dir, file)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := plant(path); err != nil {
		t.Fatalf("plant %s: %v", file, err)
	}
}

// expectNotRegular は read が待たされずに ErrNotRegular を返すことを確かめる。
func expectNotRegular(t *testing.T, name string, read func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- read() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("%s: err = %v, want ErrNotRegular", name, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a file that is not a regular file", name)
	}
}

func seedSessionFiles(t *testing.T, store *Store) Meta {
	t.Helper()
	meta := mustCreate(t, store, "x")
	log := openTestLog(t, store, meta.ID, LogOptions{Raw: true})
	if _, err := log.Write([]byte("output")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return meta
}

// session.json / transcript.jsonl / raw.log がシンボリックリンクにすり替わっていたら、
// リンク先を読まずにエラーにする。リンク先が正しい形式のファイルでも読まない。
func TestReadsRejectSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	for _, file := range sessionFiles {
		t.Run(file, func(t *testing.T) {
			store, _ := newTestStore(t)
			meta := seedSessionFiles(t, store)
			paths, _ := store.Paths(meta.ID)

			// リンク先は、辿れば問題なく読める内容にしておく。
			original, err := os.ReadFile(filepath.Join(paths.Dir, file))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(target, original, 0o600); err != nil {
				t.Fatal(err)
			}
			replaceWith(t, store, meta.ID, file, func(path string) error { return os.Symlink(target, path) })

			all := readers(store, meta.ID)
			for _, name := range readerOf(file) {
				expectNotRegular(t, name, all[name])
			}
		})
	}
}

// ディレクトリは通常ファイルではない。OS を問わず確かめられる形。
func TestReadsRejectDirectories(t *testing.T) {
	for _, file := range sessionFiles {
		t.Run(file, func(t *testing.T) {
			store, _ := newTestStore(t)
			meta := seedSessionFiles(t, store)
			replaceWith(t, store, meta.ID, file, func(path string) error { return os.Mkdir(path, 0o700) })

			all := readers(store, meta.ID)
			for _, name := range readerOf(file) {
				expectNotRegular(t, name, all[name])
			}
		})
	}
}

// セッションのディレクトリ自体がシンボリックリンクなら、その先のセッションを読まない。
func TestReadsRejectSymlinkedSessionDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	store, _ := newTestStore(t)
	meta := seedSessionFiles(t, store)
	paths, _ := store.Paths(meta.ID)

	// 別の場所へ移し、元の場所からリンクする。
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(paths.Dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, paths.Dir); err != nil {
		t.Fatal(err)
	}

	for name, read := range readers(store, meta.ID) {
		expectNotRegular(t, name, read)
	}
	if _, err := store.Save(meta); !errors.Is(err, ErrNotRegular) {
		t.Errorf("Save: err = %v, want ErrNotRegular", err)
	}
	// rm はリンクだけを消し、リンク先には触れない。
	if err := store.Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, metaFileName)); err != nil {
		t.Errorf("Delete followed the symlink: %v", err)
	}
}

// List は読めないセッションを報告し、残りは返す。
func TestListReportsSymlinkedMeta(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	store, _ := newTestStore(t)
	good := mustCreate(t, store, "good")
	bad := mustCreate(t, store, "bad")
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"id":"`+bad.ID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	replaceWith(t, store, bad.ID, metaFileName, func(path string) error { return os.Symlink(target, path) })

	metas, err := store.List()
	if !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), bad.ID) {
		t.Errorf("List error = %v, want ErrNotRegular naming %s", err, bad.ID)
	}
	if len(metas) != 1 || metas[0].ID != good.ID {
		t.Errorf("List = %+v, want only %s", metas, good.ID)
	}
}
