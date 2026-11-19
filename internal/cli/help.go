package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

const summaryLine = "geminus - thin PTY relay for Claude Code and Hermes Agent"

func printUsage(w io.Writer) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nUsage:\n  %s [global flags] <command> [flags] [args]\n\nCommands:\n", summaryLine, programName)

	rows := make([][2]string, 0, len(commands())+1)
	for _, cmd := range commands() {
		rows = append(rows, [2]string{cmd.name, cmd.summary})
	}
	rows = append(rows, [2]string{"help", "Show help for a command"})
	writeRows(&b, rows)

	b.WriteString("\nGlobal flags:\n")
	var opts Options
	writeRows(&b, flagRows(newGlobalFlagSet(&opts), func(string) bool { return true }))

	fmt.Fprintf(&b, "\nExit codes:\n  0 success, 1 geminus error, 2 usage error; run/ask return the agent's exit code.\n")
	fmt.Fprintf(&b, "\nRun '%s help <command>' for details.\n", programName)
	io.WriteString(w, b.String())
}

func printCommandHelp(w io.Writer, cmd command) {
	var b strings.Builder
	usage := programName + " " + cmd.name
	if cmd.usage != "" {
		usage += " " + cmd.usage
	}
	fmt.Fprintf(&b, "%s\n\nUsage:\n  %s\n", cmd.summary, usage)

	var opts Options
	fs := newFlagSet(cmd, &opts)
	if own := flagRows(fs, func(name string) bool { return !globalFlagNames[name] }); len(own) > 0 {
		b.WriteString("\nFlags:\n")
		writeRows(&b, own)
	}
	b.WriteString("\nGlobal flags:\n")
	writeRows(&b, flagRows(fs, func(name string) bool { return globalFlagNames[name] }))
	io.WriteString(w, b.String())
}

// flagRows は keep が true を返すフラグを「書式, 説明」の行にする。
func flagRows(fs *flag.FlagSet, keep func(name string) bool) [][2]string {
	var rows [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		if !keep(f.Name) {
			return
		}
		placeholder, usage := flag.UnquoteUsage(f)
		dashes := "--"
		if len(f.Name) == 1 {
			dashes = "-"
		}
		label := dashes + f.Name
		if placeholder != "" {
			label += " " + placeholder
		}
		rows = append(rows, [2]string{label, usage})
	})
	return rows
}

// writeRows は1列目の幅を揃えて書く。
func writeRows(b *strings.Builder, rows [][2]string) {
	width := 0
	for _, row := range rows {
		if len(row[0]) > width {
			width = len(row[0])
		}
	}
	for _, row := range rows {
		fmt.Fprintf(b, "  %-*s  %s\n", width, row[0], row[1])
	}
}
