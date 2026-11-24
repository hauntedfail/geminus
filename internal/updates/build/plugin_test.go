package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestPluginSourceBuild(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	pluginDir := filepath.Join(repo, "plugin", "geminish")
	if err := os.MkdirAll(filepath.Join(pluginDir, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginDir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pluginDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("plugin.yaml", "name: geminish\nversion: 0.9.0\n")
	mustWrite("__init__.py", "__version__ = \"0.9.0\"\n")
	mustWrite("cli.py", "#!/usr/bin/env python3\n")
	mustWrite("__pycache__/cli.cpython-313.pyc", "skip")
	mustWrite("tests/test_x.py", "skip")
	mustWrite("geminise.json", "{\"secret\": \"never\"}")

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", ".")
	runGit("commit", "-q", "--no-gpg-sign", "-m", "init")
	runGit("tag", "geminish-v0.9.0")

	outDir := filepath.Join(tmp, "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := PluginSource{Repo: repo, Git: "git"}
	if err := src.Build(context.Background(), "0.9.0", outDir); err != nil {
		t.Fatalf("Build: %v", err)
	}

	f, err := os.Open(filepath.Join(outDir, PluginTarget))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	want := []string{"geminish/__init__.py", "geminish/cli.py", "geminish/plugin.yaml"}
	if !slices.Equal(names, want) {
		t.Fatalf("tar entries = %v, want %v", names, want)
	}
}
