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
	// versionTimeout は `<agent> --version` を待つ時間。
	versionTimeout = 5 * time.Second
	// maxVersionLen は表示するバージョン文字列の上限（文字数）。
	maxVersionLen = 120
)

// agentView は agents / doctor の出力。検出結果にバージョンを足したもの。
type agentView struct {
	agent.Detection
	// Version は `<binary> --version` の最初の行。取れなければ空。
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

// detectAgents は登録済みの全バックエンドを検出し、見つかったものはバージョンも調べる。
// バージョンの問い合わせは並行に行う。
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

// probeVersion は `path --version` の最初の行を返す。失敗や時間切れは空を返す
// （バージョンが分からなくても、バックエンドは使える）。
func probeVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// 孫プロセスが出力を握ったままでも、時間切れの後で待ち続けない。
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
		// 他のプログラムの出力なので、端末に出せる形にしてから返す。
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
