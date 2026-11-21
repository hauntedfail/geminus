package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
	"github.com/hauntedfail/geminus/internal/version"
)

// geminus replaces its own program with the latest release when asked to
// (geminus update, /update). Releases come from one server; see package
// updates. Nothing is installed without being asked for: at the start
// geminus only looks whether a newer release exists, and says so.

const (
	// updateLimit bounds a whole update, download included.
	updateLimit = 5 * time.Minute
	// noticeLimit bounds the look at the start. It runs beside everything
	// else, and a server that does not answer is not mentioned.
	noticeLimit = 2 * time.Second
)

// update installs the latest release if it is newer than this program. It
// returns the line that says what happened and whether the program on disk
// is now another one.
func (a *App) update() (said string, installed bool, err error) {
	if a.Updates == nil {
		return "", false, errors.New("this geminus has no update server to ask")
	}
	ctx, cancel := context.WithTimeout(a.ctx, updateLimit)
	defer cancel()
	m, err := a.Updates.Latest(ctx)
	if err != nil {
		return "", false, err
	}
	own := version.Version
	switch {
	case own == m.Version:
		return "already the latest (" + own + ")", false, nil
	case !updates.Newer(own, m.Version):
		return fmt.Sprintf("no release is newer than this build (%s); the latest is %s", own, m.Version), false, nil
	}
	target := updates.Target(runtime.GOOS, runtime.GOARCH)
	if _, listed := m.Files[target]; !listed {
		return "", false, fmt.Errorf("release %s has no program for %s", m.Version, target)
	}
	if a.Exe == "" {
		return "", false, errors.New("geminus cannot tell where its own program is")
	}
	err = updates.Replace(a.Exe, runtime.GOOS, func(w io.Writer) error { return a.Updates.Fetch(ctx, m, target, w) })
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("geminus %s → %s", own, m.Version), true, nil
}

// updateCommand is `geminus update`.
func (a *App) updateCommand(errw io.Writer) int {
	said, _, err := a.update()
	if err != nil {
		fmt.Fprintln(errw, "geminus: update:", sanitize(err.Error()))
		return 1
	}
	a.Out.line(said)
	return 0
}

// slashUpdate is /update: the same, and on a terminal the new program then
// takes over the conversation that is open.
func (a *App) slashUpdate() error {
	said, installed, err := a.update()
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	a.Out.line(a.Out.accent("◆ ") + said)
	if !installed {
		return nil
	}
	a.newer, a.newerFound = "", nil
	args, ok := a.reopenArgs()
	if a.live == nil || a.Restart == nil || !ok {
		// Lines already read from a pipe would be lost to a new process.
		a.Out.line(a.Out.dim("The new program runs the next time geminus starts."))
		return nil
	}
	a.restarting, a.restartWith = true, args
	return nil
}

// isUpdateLine reports whether a typed line is the /update command.
func isUpdateLine(line string) bool {
	cmd, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	return cmd == "/update"
}

// goOnReading lets the keyboard reader, which stopped after an /update
// line, read on: the line has been dealt with and this program stays.
func (a *App) goOnReading(line string) {
	if !isUpdateLine(line) {
		return
	}
	select {
	case a.readOn <- struct{}{}:
	default:
	}
}

// reopenArgs is the command line that brings a new geminus to where this
// one is: the open conversation, or the persona, harness and directory
// chosen for the one the next prompt would start. ok is false when a value
// would be read as an option there; then geminus does not start itself
// again.
func (a *App) reopenArgs() (args []string, ok bool) {
	if a.cur == nil {
		return nil, true
	}
	args = []string{"persona", a.cur.Name}
	switch {
	case a.session != "":
		args = append(args, "--resume", a.session)
	default:
		if a.harness != nil {
			args = append(args, "--harness", a.harness.ID)
		}
		if a.cwd != "" {
			args = append(args, "--cwd", a.cwd)
		}
	}
	for i := 1; i < len(args); i += 2 {
		if strings.HasPrefix(args[i], "-") {
			return nil, false
		}
	}
	return args, true
}

// relaunch hands the terminal to the new program. The box is already gone.
func (a *App) relaunch(errw io.Writer) int {
	code, err := a.Restart(a.Exe, a.restartWith)
	if err != nil {
		fmt.Fprintln(errw, "geminus: the new program is in place but could not be started:", sanitize(err.Error()))
		fmt.Fprintln(errw, "geminus: start geminus again to use it.")
		return 1
	}
	return code
}

// lookForRelease asks, beside everything else, whether a release newer than
// this program exists. The answer is picked up by newerRelease.
func (a *App) lookForRelease() {
	if a.Updates == nil {
		return
	}
	found := make(chan string, 1)
	a.newerFound = found
	// Read here, not in the goroutine: both belong to the caller.
	client, own, parent := a.Updates, version.Version, a.ctx
	go func() {
		ctx, cancel := context.WithTimeout(parent, noticeLimit)
		defer cancel()
		if m, err := client.Latest(ctx); err == nil && updates.Newer(own, m.Version) {
			found <- m.Version
		}
	}()
}

// newerRelease is the version of a release newer than this program, or "".
func (a *App) newerRelease() string {
	select {
	case v := <-a.newerFound:
		a.newer = v
	default:
	}
	return a.newer
}

// releaseCommand is `geminus release VERSION DIR`, used by
// scripts/release.sh: it publishes the programs of a build, the files
// DIR/geminus-<os>-<arch>[.exe].
func (a *App) releaseCommand(v, dir string, errw io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(errw, "geminus: release:", sanitize(err.Error()))
		return 1
	}
	if a.Updates == nil {
		return fail(errors.New("this geminus has no update server to publish to"))
	}
	paths, err := filepath.Glob(filepath.Join(dir, "geminus-*"))
	if err != nil {
		return fail(err)
	}
	files := map[string]string{}
	for _, path := range paths {
		target := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "geminus-"), ".exe")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && updates.ValidTarget(target) {
			files[target] = path
		}
	}
	if len(files) == 0 {
		return fail(fmt.Errorf("no geminus-<os>-<arch> programs in %s", dir))
	}
	ctx, cancel := context.WithTimeout(a.ctx, time.Duration(len(files)+1)*updateLimit)
	defer cancel()
	m, err := a.Updates.Publish(ctx, v, files)
	if err != nil {
		return fail(err)
	}
	a.Out.line(fmt.Sprintf("published %s: %d programs at %s", m.Version, len(m.Files), sanitize(a.Updates.Base)))
	return 0
}
