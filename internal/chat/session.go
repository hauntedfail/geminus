package chat

// The connection and its sessions, and how they are shown.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hauntedfail/geminus/internal/persona"
	"github.com/hauntedfail/geminus/internal/version"
)

// who is the persona as shown in the prompt and status line. The local
// persona always carries its tag: talking to this computer's own agent
// through geminus is possible, but it should never be mistaken for a remote.
func who(p persona.Persona) string {
	return oneLine(p.Name) + " [" + p.Tag() + "]"
}

// short abbreviates a session ID for display. /resume accepts any unique
// prefix, so the short form is enough to name a session.
func short(id string) string {
	const keep = 14
	id = oneLine(id)
	if len(id) > keep+2 {
		return id[:keep] + "…"
	}
	return id
}

// clip shortens s to at most n characters.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// when trims an ISO timestamp to minutes.
func when(stamp string) string {
	stamp = oneLine(stamp)
	if len(stamp) >= 16 && stamp[10] == 'T' {
		return stamp[:10] + " " + stamp[11:16]
	}
	return stamp
}

func (a *App) status() string {
	if a.cur == nil {
		return "○ disconnected  no persona chosen (/persona)"
	}
	if a.agent == nil {
		s := "○ disconnected  " + who(*a.cur)
		if a.har != nil {
			s += "  " + a.har.Name
		}
		if a.last != "" {
			s += fmt.Sprintf("  (last session %s; /resume reconnects)", short(a.last))
		}
		return s
	}
	route := "on this computer"
	if a.node.SSH != "" {
		route = "via ssh " + a.node.SSH
	}
	agent := a.har.Name
	if reported := oneLine(a.info.Name + " " + a.info.Version); reported != "" {
		agent += " (" + reported + ")"
	}
	return fmt.Sprintf("● connected  %s  %s  %s  session %s  cwd %s", who(*a.cur), agent, route, short(a.session), oneLine(a.cwd))
}

func (a *App) prompt() string {
	if a.agent == nil || a.cur == nil {
		return a.Out.dim("geminus (disconnected)") + " ❯ "
	}
	return a.Out.bold(who(*a.cur)) + a.Out.dim(" · "+a.har.ID) + " ❯ "
}

func (a *App) disconnect() {
	if a.agent != nil {
		_ = a.agent.Close()
	}
	a.agent, a.session = nil, ""
}

// dropped records that the connection ended on its own.
func (a *App) dropped(err error) {
	a.Out.line(a.Out.fail(fmt.Sprintf("✗ connection to %s lost: %v", a.cur.Name, err)))
	a.disconnect()
	a.Out.line(a.status())
}

// connect opens a connection to the selected harness and a session on it.
func (a *App) connect(resume string, latest bool) error {
	if a.cur == nil || a.har == nil {
		return errors.New("pick a persona first: /persona")
	}
	cwd, err := a.node.SessionCWD(a.probe)
	if err != nil {
		return err
	}
	a.disconnect()
	a.Out.line(a.Out.dim(fmt.Sprintf("connecting to %s · %s (%s)…", who(*a.cur), a.har.Name, a.node.Where())))

	agent, err := a.Dial(a.node, *a.har)
	if err != nil {
		return fmt.Errorf("cannot start %s on %s: %w", a.har.Name, a.cur.Name, err)
	}
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	info, err := agent.Initialize(ctx, version.Version)
	if err != nil {
		_ = agent.Close()
		return fmt.Errorf("cannot start %s on %s (%s): %w", a.har.Name, a.cur.Name, a.node.Where(), err)
	}
	a.agent, a.info, a.cwd = agent, info, cwd

	mode := openNew
	switch {
	case resume != "" || latest:
		mode = openExisting
	case a.dir.Defaults.Session == persona.SessionContinue:
		mode = openLatestOrNew
	}
	if err := a.open(resume, mode); err != nil {
		a.disconnect()
		return err
	}
	return nil
}

type openMode int

const (
	// openNew starts a session.
	openNew openMode = iota
	// openExisting reopens the named session, or the latest; having none
	// is an error.
	openExisting
	// openLatestOrNew reopens the latest session if there is one.
	openLatestOrNew
)

// open starts or reopens a session on the current connection.
func (a *App) open(want string, mode openMode) error {
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	id := ""
	if mode != openNew && a.info.CanLoad {
		found, err := a.resolve(ctx, want)
		switch {
		case err == nil:
			id = found
		case mode == openExisting:
			return err
		}
	} else if mode == openExisting {
		return fmt.Errorf("%s cannot reopen sessions", a.har.Name)
	}
	if id == "" {
		created, err := a.agent.NewSession(ctx, a.cwd)
		if err != nil {
			return fmt.Errorf("%s could not start a session: %s", a.har.Name, reason(err))
		}
		a.session, a.last = created, created
		a.Out.line(a.status())
		return nil
	}
	replay := newReplay(a.Out)
	if err := a.agent.LoadSession(ctx, a.cwd, id, replay.handler()); err != nil {
		return fmt.Errorf("%s could not reopen session %s: %w", a.har.Name, short(id), err)
	}
	replay.end()
	a.session, a.last = id, id
	a.Out.line(a.status())
	return nil
}

// resolve turns what the person typed (nothing, a list number, an ID or the
// start of one) into a session ID the agent knows.
func (a *App) resolve(ctx context.Context, want string) (string, error) {
	if !a.info.CanList {
		if want == "" {
			return "", fmt.Errorf("%s cannot list sessions; give a session ID", a.har.Name)
		}
		return want, nil
	}
	sessions, err := a.agent.ListSessions(ctx, a.cwd)
	if err != nil {
		return "", fmt.Errorf("%s could not list sessions: %s", a.har.Name, reason(err))
	}
	if len(sessions) == 0 {
		return "", fmt.Errorf("%s has no %s sessions in %s yet", a.cur.Name, a.har.Name, a.cwd)
	}
	if want == "" {
		return sessions[0].ID, nil
	}
	var matches []string
	for _, s := range sessions {
		if s.ID == want {
			return s.ID, nil
		}
		if strings.HasPrefix(s.ID, want) {
			matches = append(matches, s.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if n, err := strconv.Atoi(want); err == nil && len(matches) == 0 && n >= 1 && n <= len(sessions) {
		return sessions[n-1].ID, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("%q matches %d sessions; type more of the ID", want, len(matches))
	}
	return "", fmt.Errorf("%s has no session %q (see /sessions)", a.cur.Name, want)
}

func (a *App) showSessions() error {
	if a.agent == nil {
		return errors.New("not connected")
	}
	if !a.info.CanList {
		return fmt.Errorf("%s cannot list sessions", a.har.Name)
	}
	ctx, cancel := context.WithTimeout(a.ctx, connectTimeout)
	defer cancel()
	sessions, err := a.agent.ListSessions(ctx, a.cwd)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		a.Out.line("  no sessions yet")
	}
	for i, s := range sessions {
		mark := " "
		if s.ID == a.session {
			mark = "●"
		}
		title := oneLine(s.Title)
		if title == "" {
			title = "(untitled)"
		}
		a.Out.line(fmt.Sprintf("%s %2d  %s  %s  %s", mark, i+1, short(s.ID), a.Out.dim(when(s.UpdatedAt)), title))
	}
	return nil
}
