package chat

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/acp"
	"github.com/hauntedfail/geminus/internal/harness"
	"github.com/hauntedfail/geminus/internal/persona"
)

const esc = "\x1b[2J\x1b]0;owned\x07"

// hostileAgent puts terminal escape sequences in every string an agent can
// supply outside the reply text.
type hostileAgent struct{ done chan struct{} }

func (hostileAgent) Initialize(context.Context, string) (acp.Info, error) {
	return acp.Info{Name: "agent" + esc, Version: "1" + esc, CanLoad: true, CanList: true}, nil
}
func (hostileAgent) NewSession(context.Context, string) (string, error) { return "sess" + esc, nil }
func (hostileAgent) LoadSession(context.Context, string, string, *acp.Handler) error {
	return &acp.RPCError{Method: "session/load", Message: "nope" + esc}
}
func (hostileAgent) ListSessions(context.Context, string) ([]acp.SessionInfo, error) {
	return []acp.SessionInfo{{ID: "id" + esc, Title: "title" + esc, UpdatedAt: "2026-11-19T07:00:00" + esc}}, nil
}
func (hostileAgent) Prompt(context.Context, string, string, *acp.Handler) (string, error) {
	return "", &acp.RPCError{Method: "session/prompt", Message: "bad" + esc}
}
func (hostileAgent) Commands(string) []acp.Command {
	return []acp.Command{{Name: "cmd" + esc, Description: "desc" + esc}}
}
func (h hostileAgent) Done() <-chan struct{} { return h.done }
func (hostileAgent) Err() error              { return nil }
func (hostileAgent) Close() error            { return nil }

func TestNoAgentOrDirectoryStringCanControlTheTerminal(t *testing.T) {
	out, errs := &screen{}, &bytes.Buffer{}
	hostile := harness.Harness{ID: "hermes", Name: "Hermes" + esc, Command: "hermes acp"}
	app := &App{
		Out: NewPrinter(out),
		Load: func(context.Context) (persona.Directory, error) {
			return persona.Directory{
				Workspace:  "ws" + esc,
				Warning:    "warn" + esc,
				ConfigPath: "/cfg",
				Harnesses:  []harness.Harness{hostile, {ID: "other", Name: "Other", Command: "other acp", CLI: "othercli", Install: "install" + esc}},
				Personas: []persona.Persona{
					{Name: "sibyl", Node: &persona.Node{SSH: "host", CWD: "/w" + esc}},
					{Name: "evil" + esc},
				},
			}, nil
		},
		Probe: func(context.Context, persona.Node, []harness.Harness) (harness.Probe, error) {
			return harness.Probe{Home: "/home" + esc, Have: map[string]bool{"hermes": true, "othercli": true}}, nil
		},
		Dial: func(persona.Node, harness.Harness) (Agent, error) {
			return hostileAgent{done: make(chan struct{})}, nil
		},
		SaveDefaults: func(persona.Defaults) error { return nil },
		SaveNode:     func(string, persona.Node) error { return nil },
		ctx:          context.Background(),
		interrupt:    make(chan os.Signal),
	}
	input := strings.Join([]string{"sibyl", "hello", "/sessions", "/resume 1", "/status", "/help", "/harness", "", "/default", "/persona evil" + esc, "/nope" + esc}, "\n") + "\n"
	app.Main(nil, strings.NewReader(input), errs)

	shown := out.String() + errs.String()
	for _, want := range []string{"● connected", "bad", "title", "nope", "warn", "evil", "cmd", "desc", "install", "Harnesses on"} {
		if !strings.Contains(shown, want) {
			t.Errorf("scenario did not reach %q:\n%q", want, shown)
		}
	}
	// The only control characters allowed out are the ones geminus typed
	// itself: newlines. (Color is off in this printer; the echoed input
	// lines are the person's own keystrokes and are excluded below.)
	for _, line := range strings.Split(shown, "\n") {
		if strings.Contains(line, "❯ ") {
			continue
		}
		if strings.ContainsAny(line, "\x1b\x07") {
			t.Errorf("escape sequence reached the screen: %q", line)
		}
	}
}

func TestColorPrinterKeepsOnlyItsOwnStyling(t *testing.T) {
	var buf bytes.Buffer
	p := newPrinter(&buf, true)
	p.line(p.bold("sibyl") + " \x1b[2J\x1b[5;1Hmoved \x1b]0;title\x07 \x9b31m " + p.fail("lost"))
	got := buf.String()
	// The escape, bell and CSI bytes are gone; what is left of a foreign
	// sequence is inert text. geminus's own bold and red survive.
	if want := "\x1b[1msibyl\x1b[0m [2J[5;1Hmoved ]0;title 31m \x1b[31mlost\x1b[0m\n"; got != want {
		t.Errorf("printed %q\n   want %q", got, want)
	}
}

func TestOneLineFlattensAndStrips(t *testing.T) {
	if got := oneLine("a\nb\t\x1b[2Jc ‮d  "); got != "a b [2Jc d" {
		t.Errorf("oneLine = %q", got)
	}
}
