package chat

import (
	"strings"
	"testing"
)

func TestAgentMarkdownHidesMarkers(t *testing.T) {
	plain := func(s string) string { return s }
	got := renderAgentMarkdown("&amp;**title** and *note*\n## Heading\n`**kept**`\n```\n**inside**\n```\nsee [docs](https://example.test)\n", nil, plain, plain)
	for _, want := range []string{"&title", "note", "Heading", "`**kept**`", "**inside**", "docs (https://example.test)"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered text missing %q:\n%s", want, got)
		}
	}
	for _, raw := range []string{"**title**", "## Heading", "```", "[docs]"} {
		if strings.Contains(got, raw) {
			t.Errorf("syntax still visible %q:\n%s", raw, got)
		}
	}
}

func TestAgentMarkdownUsesConsoleStyles(t *testing.T) {
	bold := func(s string) string { return "<b>" + s + "</b>" }
	dim := func(s string) string { return "<d>" + s + "</d>" }
	got := renderAgentMarkdown("**title**", nil, bold, dim)
	if got != "<b>title</b>" {
		t.Fatalf("bold = %q", got)
	}
}

func TestSplitMarkdownIsNotShownRaw(t *testing.T) {
	app := &App{Out: NewPrinter(&strings.Builder{})}
	if got := app.takeMarkdown("t", "**ti", false, true); got != "" {
		t.Fatalf("partial marker was painted: %q", got)
	}
	got := app.takeMarkdown("t", "tle**", true, true)
	if got != "title" {
		t.Fatalf("joined markdown = %q", got)
	}
}
