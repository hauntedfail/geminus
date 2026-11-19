//go:build unix

package session

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// FIFO は開くだけで書き手を待つ。読み取りが止まらずにエラーになること。
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

// Lstat の後でファイルがすり替わっても、開いた後の確認で弾く。
func TestOpenRegularChecksTheOpenedFile(t *testing.T) {
	dir := t.TempDir()
	fifo := dir + "/fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// Lstat の確認を飛ばして、開く段階だけを試す。
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

// 追記で開く側も、Lstat の後でファイルが FIFO にすり替わったら待たずに失敗する。
// 読み手のいない FIFO を書き込みで開くと、O_NONBLOCK が無ければ読み手を待って止まる。
func TestAppendOpenDoesNotBlockOnAFIFO(t *testing.T) {
	fifo := t.TempDir() + "/fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		// Lstat の確認を飛ばして、開く段階だけを試す。
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
