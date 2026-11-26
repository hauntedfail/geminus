package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	// releaseLimit bounds the wait for the server to build a release.
	releaseLimit = 20 * time.Minute
	// noticeLimit bounds each background look for a release. It runs beside
	// everything else, and a server that does not answer is not mentioned.
	noticeLimit = 2 * time.Second
	// updateCheckEvery is how often a running session checks for a newer
	// release on the update server.
	updateCheckEvery = 30 * time.Second
	// updatedFromEnv carries the previous version across /update relaunch so
	// the new process can display the version transition inside its alt-screen
	// viewport. Older binaries ignore it cleanly.
	updatedFromEnv = "GEMINUS_UPDATED_FROM"
)

// consumeUpdatedFrom returns and clears the version passed from the previous
// geminus process after an in-session /update relaunch.
func consumeUpdatedFrom() string {
	v := strings.TrimSpace(os.Getenv(updatedFromEnv))
	if v == "" {
		return ""
	}
	_ = os.Unsetenv(updatedFromEnv)
	return sanitize(v)
}

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

// parseBoolToggle parses common boolean toggle values.
func parseBoolToggle(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "enable", "enabled", "yes", "1":
		return true, nil
	case "off", "false", "disable", "disabled", "no", "0":
		return false, nil
	default:
		return false, fmt.Errorf("expected on or off, got %q", oneLine(s))
	}
}

// autoUpdateEnabled reports whether automatic updates are active for this run.
// Honcho signed defaults take precedence; otherwise geminise.json's
// geminus.auto_update applies; default is off.
func (a *App) autoUpdateEnabled() bool {
	if a.defaults.AutoUpdate != nil {
		return *a.defaults.AutoUpdate
	}
	if a.Profile.AutoUpdate != nil {
		return *a.Profile.AutoUpdate
	}
	return false
}

// autoUpdateSource describes where the current auto_update setting came from.
func (a *App) autoUpdateSource() string {
	if a.defaults.AutoUpdate != nil {
		return "Honcho"
	}
	if a.Profile.AutoUpdate != nil {
		return "geminise.json"
	}
	return "default"
}

// slashUpdateCmd handles `/update` (manual update) and `/update auto [on|off]`.
func (a *App) slashUpdateCmd(arg string) error {
	if arg == "" {
		return a.slashUpdate()
	}
	sub, rest, _ := strings.Cut(arg, " ")
	sub, rest = strings.ToLower(strings.TrimSpace(sub)), strings.TrimSpace(rest)
	if sub != "auto" && sub != "auto_update" {
		return errors.New("usage: /update [auto [on|off]]")
	}
	if rest == "" {
		state := "off"
		if a.autoUpdateEnabled() {
			state = "on"
		}
		a.Out.line(fmt.Sprintf("auto_update: %s (%s)", state, a.autoUpdateSource()))
		return nil
	}
	d := a.cloneDefaults()
	var said string
	if strings.EqualFold(rest, "none") || strings.EqualFold(rest, "default") {
		d.AutoUpdate = nil
		said = "auto_update: default"
	} else {
		on, err := parseBoolToggle(rest)
		if err != nil {
			return errors.New("usage: /update auto on|off")
		}
		d.AutoUpdate = &on
		if on {
			said = "auto_update: on"
		} else {
			said = "auto_update: off"
		}
	}
	if err := a.saveDefaults(d); err != nil {
		return err
	}
	a.Out.line(said + " (saved in Honcho)")
	if a.autoUpdateEnabled() {
		a.checkAndAutoUpdateNow()
	}
	return nil
}

// checkAndAutoUpdateNow checks immediately for a newer release when auto_update
// is enabled and triggers an automatic update if one is available.
func (a *App) checkAndAutoUpdateNow() {
	if !a.autoUpdateEnabled() || a.restarting || a.Updates == nil {
		return
	}
	if a.newerRelease() == "" {
		ctx, cancel := context.WithTimeout(a.ctx, noticeLimit)
		m, err := a.Updates.Latest(ctx)
		cancel()
		if err == nil && updates.Newer(version.Version, m.Version) {
			a.newer = m.Version
		}
	}
	a.maybeAutoUpdate()
}

// maybeAutoUpdate installs and relaunches automatically when auto_update is
// enabled and a newer release has been caught. If the user is actively typing a
// draft in the prompt box, it defers until the draft is sent or cleared.
func (a *App) maybeAutoUpdate() {
	if !a.autoUpdateEnabled() || a.restarting {
		return
	}
	v := a.newerRelease()
	if v == "" || v == a.autoUpdateFailed {
		return
	}
	if a.live != nil && a.live.hasDraft() {
		return
	}
	if len(a.lines) > 0 {
		return
	}
	if err := a.slashUpdate(); err != nil {
		a.autoUpdateFailed = v
		a.Out.line(a.Out.fail("✗ auto-" + err.Error()))
	}
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
	a.restarting, a.restartWith, a.restartNotice = true, args, a.Out.accent("◆ ")+said
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
	_ = os.Setenv(updatedFromEnv, version.Version)
	defer os.Unsetenv(updatedFromEnv)
	code, err := a.Restart(a.Exe, a.restartWith)
	if err != nil {
		fmt.Fprintln(errw, "geminus: the new program is in place but could not be started:", sanitize(err.Error()))
		fmt.Fprintln(errw, "geminus: start geminus again to use it.")
		return 1
	}
	return code
}

// lookForRelease asks, beside everything else, whether a release newer than
// this program exists, and continues checking periodically while the session
// stays open. Each newly discovered version is delivered on newerFound.
func (a *App) lookForRelease() {
	if a.Updates == nil {
		return
	}
	found := make(chan string, 1)
	a.newerFound = found
	// Read here, not in the goroutine: all belong to the caller.
	client, own, parent := a.Updates, version.Version, a.ctx
	interval := a.UpdateCheck
	if interval <= 0 {
		interval = updateCheckEvery
	}
	deliver := func(v string) {
		// Replace any unread older version with the latest one.
		select {
		case <-found:
		default:
		}
		select {
		case found <- v:
		default:
		}
	}
	go func() {
		lastNotified := ""
		check := func() {
			ctx, cancel := context.WithTimeout(parent, noticeLimit)
			defer cancel()
			if m, err := client.Latest(ctx); err == nil && updates.Newer(own, m.Version) && m.Version != lastNotified {
				lastNotified = m.Version
				deliver(m.Version)
			}
		}
		check()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case <-ticker.C:
				check()
			}
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

// releaseCommand is `geminus release VERSION [--download DIR]`, used by
// scripts/release.sh: it asks the server to build the tag vVERSION, sign it
// and publish it, waits for that, and optionally downloads the verified
// binaries and SHA256SUMS into outDir.
func (a *App) releaseCommand(v, outDir string, errw io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(errw, "geminus: release:", sanitize(err.Error()))
		return 1
	}
	if a.Updates == nil {
		return fail(errors.New("this geminus has no update server to ask"))
	}
	if !updates.IsRelease(v) {
		return fail(fmt.Errorf("%q is not a release version: use X.Y.Z", v))
	}
	ctx, cancel := context.WithTimeout(a.ctx, releaseLimit)
	defer cancel()
	a.Out.line(fmt.Sprintf("building %s at %s …", v, sanitize(a.Updates.Base)))
	m, err := a.Updates.Release(ctx, v)
	if err != nil {
		return fail(err)
	}
	a.Out.line(fmt.Sprintf("released %s: %d programs", m.Version, len(m.Files)))
	if outDir != "" {
		n, err := a.downloadReleaseAssets(ctx, m, outDir)
		if err != nil {
			return fail(err)
		}
		a.Out.line(fmt.Sprintf("downloaded %d programs and SHA256SUMS to %s", n, sanitize(outDir)))
	}
	return 0
}

func releaseAssetName(target string) string {
	name := "geminus-" + target
	if strings.HasPrefix(target, "windows-") {
		name += ".exe"
	}
	return name
}

func (a *App) downloadReleaseAssets(ctx context.Context, m updates.Manifest, dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("create download directory: %w", err)
	}
	targets := make([]string, 0, len(m.Files))
	for target := range m.Files {
		if !updates.ValidTarget(target) {
			return 0, fmt.Errorf("manifest names invalid target %q", target)
		}
		targets = append(targets, target)
	}
	sort.Strings(targets)

	var sums strings.Builder
	for _, target := range targets {
		name := releaseAssetName(target)
		dst := filepath.Join(dir, name)
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return 0, fmt.Errorf("create %s: %w", name, err)
		}
		fetchErr := a.Updates.Fetch(ctx, m, target, f)
		closeErr := f.Close()
		if fetchErr != nil {
			_ = os.Remove(dst)
			return 0, fetchErr
		}
		if closeErr != nil {
			_ = os.Remove(dst)
			return 0, fmt.Errorf("close %s: %w", name, closeErr)
		}
		fmt.Fprintf(&sums, "%s  %s\n", m.Files[target].SHA256, name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		return 0, fmt.Errorf("write SHA256SUMS: %w", err)
	}
	return len(targets), nil
}
