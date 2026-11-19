//go:build unix

package session

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// Opening a FIFO normally waits for a writer. Reading should fail rather than block.
func TestReadsRejectFIFOs(t *testing.T) {
	for _, file := range sessionFiles {
		t.Run(file, func(t *testing.T) {
			store, _ := newTestStore(t)
			meta := seedSessionFiles(t, store)
			replaceWith(t, store, meta.ID, file, func(path string) error { return syscall.Mkfifo(path, 0o600) })

			all := readers(store, meta.ID)
			for _, name := range readerOf(file) {
				expectNotRegular(t, name, all[name])
			}
		})
	}
}

// The post-open check rejects a file replaced after Lstat.
func TestOpenRegularChecksTheOpenedFile(t *testing.T) {
	dir := t.TempDir()
	fifo := dir + "/fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Skip the Lstat check to exercise the open step in isolation.
	f, err := openNoFollow(fifo)
	if err == nil {
		defer f.Close()
		if err := requireRegular(f); err == nil {
			t.Error("a FIFO passed the check on the opened file")
		}
	}

	link := dir + "/link"
	if err := os.Symlink(os.DevNull, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openNoFollow(link); err == nil {
		f.Close()
		t.Error("openNoFollow followed a symlink")
	}
}

// Opening for append must also fail without blocking if a FIFO replaces the file after Lstat.
// Without O_NONBLOCK, opening a FIFO for writing would wait for a reader.
func TestAppendOpenDoesNotBlockOnAFIFO(t *testing.T) {
	fifo := t.TempDir() + "/fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		// Skip the Lstat check to exercise the open step in isolation.
		f, err := openAppendChecked(fifo)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was opened for appending")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO for appending blocked")
	}
	if info, err := os.Lstat(fifo); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the FIFO was changed: %v, %v", info, err)
	}
}
