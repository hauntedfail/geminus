package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/hauntedfail/geminus/internal/agent"
)

const (
	// versionTimeout limits how long to wait for `<agent> --version`.
	versionTimeout = 5 * time.Second
	// maxVersionLen limits the displayed version string in characters.
	maxVersionLen = 120
)

// agentView adds version information to detection results for agents and doctor.
type agentView struct {
	agent.Detection
	// Version is the first line of `<binary> --version`, or empty if unavailable.
	Version string `json:"version,omitempty"`
}

func runAgents(inv Invocation) int {
	const name = "agents"
	if err := noArgs(inv.Args); err != nil {
		return report(inv.Env, name, err)
	}
	views := detectAgents()
	var err error
	if inv.Options.JSON {
		err = writeJSON(inv.Env.Stdout, views)
	} else {
		err = writeAgentTable(inv.Env.Stdout, views)
	}
	if err != nil {
		return fail(inv.Env, name, err)
	}
	return ExitOK
}

// detectAgents checks all registered backends and probes the versions of those found in
// parallel.
func detectAgents() []agentView {
	detections := agent.DetectAll()
	views := make([]agentView, len(detections))
	var wg sync.WaitGroup
	for i, d := range detections {
		views[i] = agentView{Detection: d}
		if d.Path == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i].Version = probeVersion(d.Path)
		}()
	}
	wg.Wait()
	return views
}

// probeVersion returns the first line of `path --version`, or an empty string on error or
// timeout. A backend can still be used if its version is unknown.
func probeVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// Don't keep waiting after the timeout if a grandchild still has the output pipe open.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return firstLine(string(out))
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > maxVersionLen {
			line = string(runes[:maxVersionLen])
		}
		// Make external program output safe to display on a terminal.
		return printable(line)
	}
	return ""
}

func writeAgentTable(w io.Writer, views []agentView) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tFOUND\tVERSION\tPATH\tCAPABILITIES")
	for _, v := range views {
		found := "no"
		if v.Found {
			found = "yes"
		}
		path := v.Path
		if v.Binary == "" {
			path = "(any command)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.Name, found, orDash(v.Version), orDash(path), capabilityText(v.Capabilities))
	}
	return tw.Flush()
}

func capabilityText(c agent.Capabilities) string {
	var parts []string
	if c.Resume {
		parts = append(parts, "resume")
	}
	if c.Oneshot {
		parts = append(parts, "oneshot")
	}
	parts = append(parts, "key="+string(c.SessionKey))
	return strings.Join(parts, ", ")
}
