package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hauntedfail/geminus/internal/wire"
)

type permissionRoundTrip func(*http.Request) (*http.Response, error)

func (f permissionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDirectoryPinNeverReportsSavedWithoutReadback(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	w.app.cwd = "/project/pin"
	base := http.DefaultTransport
	w.app.Honcho.HTTP().Transport = permissionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			// Simulate a host that acknowledges the request but does not persist it.
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		}
		return base.RoundTrip(r)
	})
	if err := w.app.handle("/cwd save"); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("unconfirmed pin: %v", err)
	}
	if w.app.defaults.Personas[w.app.cur.Name].CWD != "" {
		t.Fatal("unconfirmed pin became active")
	}
	w.expectNot("saved in Honcho")
}

func TestPermissionDetailsFitCompactTerminal(t *testing.T) {
	for _, rows := range []int{8, 10, 14, 24, 40} {
		m := newSelectMenu(menuConfig{title: "Permission", noSearch: true, details: strings.Repeat("full input\n", 30) + "LAST",
			items: []menuItem{{value: "allow", label: "Allow"}, {value: "allow_session", label: "Session"}, {value: "allow_always", label: "Always"}, {value: "deny", label: "Deny"}}}, nil)
		in := frameInput{cols: 40, rows: rows}
		for i := 0; i < 40; i++ {
			m.handleKey(key{Kind: keyPageDown})
			_ = renderMenu(in, m)
		}
		f := renderMenu(in, m)
		text := strings.Join(f.rows, "\n")
		if len(f.rows) > rows-2 || !strings.Contains(text, "LAST") {
			t.Fatalf("rows=%d unusable picker:\n%s", rows, text)
		}
		m.handleKey(key{Kind: keyEnd})
		text = strings.Join(renderMenu(in, m).rows, "\n")
		if !strings.Contains(text, "Deny") {
			t.Fatalf("rows=%d last choice inaccessible", rows)
		}
	}
}

func TestSignedButUnofferedAnswerDoesNotResolveAsk(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	appendPermission(w, "offered", []string{"allow", "deny"}, now.Unix()+100)
	label := wire.AnswerLabel(w.app.session, "permission-turn", "offered", w.prof.Human)
	sig, alg := w.app.humanAuth().Sign("allow_always", label, now.Unix())
	w.srv.Append(w.app.session, w.prof.Human, "allow_always", map[string]any{wire.Kind: wire.Message{Role: wire.RoleAnswer, Turn: "permission-turn", Request: "offered", TS: now.Unix(), Sig: sig, SigAlg: alg}})
	if _, _, err := w.app.poll(""); err != nil {
		t.Fatal(err)
	}
	if w.app.pendingAsk == nil {
		t.Fatal("unoffered answer suppressed the genuine ask")
	}
	if err := w.app.selectPersona("chrome"); err != nil {
		t.Fatal(err)
	}
	if w.app.pendingAsk != nil {
		t.Fatal("ask crossed persona boundary")
	}
}

func TestHostReportedDefaultsRefreshAndUnknown(t *testing.T) {
	w := newWorld(t)
	w.talk([]string{"persona", "sibyl"}, [2]string{"hello", "echo: hello"})
	if err := w.app.handle("/model"); err != nil {
		t.Fatal(err)
	}
	w.expect("unknown; host has not reported a value")
	var card wire.NodeCard
	raw := w.srv.PeerMetadata("sibyl", wire.NodeKey)
	// The fixture card is already signed; add separately signed host values.
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	card.Harnesses[0].Defaults = map[string]string{"model": "reported-model", "effort": "high"}
	card.DefaultsSig = wire.Sign(w.prof.Secret, wire.HostDefaultsText(card.Harnesses), wire.HostDefaultsLabel("sibyl"), card.TS)
	w.srv.SetPeerMetadata("sibyl", wire.NodeKey, card)
	if err := w.app.refresh(); err != nil {
		t.Fatal(err)
	}
	if err := w.app.handle("/model"); err != nil {
		t.Fatal(err)
	}
	w.expect("default (reported-model)")
}
