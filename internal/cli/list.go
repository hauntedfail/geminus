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
	// lsLimit は --all が無いときに ls が出す件数。
	lsLimit = 20
	// timeLayout は一覧と show で使う時刻の書式（ローカル時刻）。
	timeLayout = "2006-01-02 15:04:05"
	// statusStale は、running のまま中継していたプロセスがいなくなったセッションの表示。
	statusStale = "stale"
)

// sessionView は ls / show の JSON 出力。session.json の内容に、今の状態を足したもの。
type sessionView struct {
	session.Meta
	// Active は、中継しているプロセスが今も生きているかを表す。
	Active bool `json:"active"`
	// Dir はセッションのファイルがあるディレクトリ。show だけが入れる。
	Dir string `json:"dir,omitempty"`
}

func viewOf(meta session.Meta) sessionView {
	return sessionView{Meta: meta, Active: meta.Active()}
}

// displayStatus は人が読む状態を返す。running は、プロセスが生きている場合だけそう表示する。
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

	// 読めないセッションがあっても、読めた分は出す。
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

// orDash は s を端末に出せる形にして返す。空なら "-"。
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
