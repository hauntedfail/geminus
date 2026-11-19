package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hauntedfail/geminus/internal/session"
)

const (
	// lsLimit is the maximum number of sessions ls displays without --all.
	lsLimit = 20
	// timeLayout formats local times in ls and show.
	timeLayout = "2006-01-02 15:04:05"
	// statusStale labels a session still marked running after its relay process has exited.
	statusStale = "stale"
)

// sessionView adds current state to session.json metadata for ls and show JSON output.
type sessionView struct {
	session.Meta
	// Active reports whether the relay process is still alive.
	Active bool `json:"active"`
	// Dir holds the session files' directory. Only show sets it.
	Dir string `json:"dir,omitempty"`
}

func viewOf(meta session.Meta) sessionView {
	return sessionView{Meta: meta, Active: meta.Active()}
}

// displayStatus returns the human-readable status, showing running only while the process
// is alive.
func displayStatus(meta session.Meta) string {
	if meta.Status == session.StatusRunning && !meta.Active() {
		return statusStale
	}
	return string(meta.Status)
}

func runLs(inv Invocation) int {
	const name = "ls"
	if err := noArgs(inv.Args); err != nil {
		return report(inv.Env, name, err)
	}
	store, err := openStore(inv)
	if err != nil {
		return fail(inv.Env, name, err)
	}

	// Display readable sessions even if others could not be read.
	metas, listErr := store.List()
	hidden := 0
	if !inv.Options.Ls.All && len(metas) > lsLimit {
		hidden = len(metas) - lsLimit
		metas = metas[:lsLimit]
	}

	if inv.Options.JSON {
		views := make([]sessionView, 0, len(metas))
		for _, meta := range metas {
			views = append(views, viewOf(meta))
		}
		err = writeJSON(inv.Env.Stdout, views)
	} else {
		err = writeSessionTable(inv.Env.Stdout, metas)
	}
	if err != nil {
		return fail(inv.Env, name, err)
	}
	if hidden > 0 {
		fmt.Fprintf(inv.Env.Stderr, "%s %s: %d older session(s) not shown; use --all\n", programName, name, hidden)
	}
	if listErr != nil {
		return fail(inv.Env, name, fmt.Errorf("some sessions could not be read:\n%w", listErr))
	}
	return ExitOK
}

func writeSessionTable(w io.Writer, metas []session.Meta) error {
	if len(metas) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGENT\tSTATUS\tEXIT\tUPDATED\tTITLE")
	for _, meta := range metas {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			meta.ID, printable(meta.Agent), displayStatus(meta), exitText(meta), meta.UpdatedAt.Local().Format(timeLayout), orDash(meta.Title))
	}
	return tw.Flush()
}

func exitText(meta session.Meta) string {
	if meta.ExitCode == nil {
		return "-"
	}
	return strconv.Itoa(*meta.ExitCode)
}

// orDash returns terminal-safe text, or "-" if s is empty.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return printable(s)
}

func runShow(inv Invocation) int {
	const name = "show"
	id, err := oneArg(inv.Args, "session id")
	if err != nil {
		return report(inv.Env, name, err)
	}
	store, err := openStore(inv)
	if err != nil {
		return fail(inv.Env, name, err)
	}
	meta, err := store.Load(id)
	if err != nil {
		return fail(inv.Env, name, err)
	}
	paths, err := store.Paths(id)
	if err != nil {
		return fail(inv.Env, name, err)
	}

	if inv.Options.JSON {
		view := viewOf(meta)
		view.Dir = paths.Dir
		err = writeJSON(inv.Env.Stdout, view)
	} else {
		err = writeSessionDetail(inv.Env.Stdout, meta, paths.Dir)
	}
	if err != nil {
		return fail(inv.Env, name, err)
	}
	return ExitOK
}

func writeSessionDetail(w io.Writer, meta session.Meta, dir string) error {
	status := displayStatus(meta)
	switch {
	case meta.Active():
		status += fmt.Sprintf(" (pid %d)", meta.PID)
	case meta.ExitCode != nil:
		status += fmt.Sprintf(" (exit %d)", *meta.ExitCode)
	}
	agentSession := "-"
	if meta.AgentSession != nil {
		agentSession = printable(meta.AgentSession.Kind + " " + meta.AgentSession.Value)
	}

	rows := [][2]string{
		{"id", meta.ID},
		{"title", orDash(meta.Title)},
		{"agent", orDash(meta.Agent)},
		{"agent session", agentSession},
		{"status", status},
		{"cwd", orDash(meta.Cwd)},
		{"created", formatTime(meta.CreatedAt)},
		{"updated", formatTime(meta.UpdatedAt)},
		{"turns", strconv.Itoa(meta.Turns)},
		{"raw bytes", strconv.FormatInt(meta.RawBytes, 10)},
		{"dir", dir},
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintf(tw, "%s:\t%s\n", row[0], row[1])
	}
	return tw.Flush()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(timeLayout)
}
