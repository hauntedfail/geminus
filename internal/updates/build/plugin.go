package build

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hauntedfail/geminus/internal/updates"
)

const (
	// PluginTarget is the manifest target name for the architecture-independent geminish plugin bundle.
	PluginTarget    = "plugin-noarch"
	PluginSubdir    = "plugin/geminish"
	PluginTagPrefix = "geminish-v"
	PluginPath      = updates.Path + "/geminish"
)

// PluginSource packages the geminish Hermes plugin from a git tag into a
// deterministic tar.gz bundle named PluginTarget.
type PluginSource struct {
	Repo      string
	Git       string
	Subdir    string
	TagPrefix string
}

// Build checks out refs/tags/<TagPrefix><version> and writes a deterministic
// tar.gz archive of Subdir to dir/plugin-noarch.
func (s PluginSource) Build(ctx context.Context, version, dir string) error {
	if !updates.IsRelease(version) {
		return fmt.Errorf("%q is not a release version: use X.Y.Z", version)
	}
	if s.Repo == "" || strings.HasPrefix(s.Repo, "-") {
		return errors.New("the plugin build has no source repository")
	}
	if s.Git == "" {
		return errors.New("the plugin build needs git")
	}
	subdir := s.Subdir
	if subdir == "" {
		subdir = PluginSubdir
	}
	tagPrefix := s.TagPrefix
	if tagPrefix == "" {
		tagPrefix = PluginTagPrefix
	}
	src, err := os.MkdirTemp(dir, ".plugin-source-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(src)
	if err := s.checkout(ctx, tagPrefix+version, src); err != nil {
		return err
	}
	pluginRoot := filepath.Join(src, filepath.FromSlash(subdir))
	if err := verifyPluginVersion(pluginRoot, version); err != nil {
		return err
	}
	outPath := filepath.Join(dir, PluginTarget)
	return packPluginTarGz(pluginRoot, outPath)
}

func (s PluginSource) checkout(ctx context.Context, tag, src string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchLimit)
	defer cancel()
	ref := "refs/tags/" + tag
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}
	steps := [][]string{
		{"init", "--quiet", src},
		{"-C", src, "fetch", "--quiet", "--depth", "1", "--no-tags", "--", s.Repo, ref + ":" + ref},
		{"-C", src, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", ref},
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, s.Git, args...)
		cmd.Env = append(os.Environ(), env...)
		cmd.WaitDelay = killDelay
		stopWithChildren(cmd)
		said, err := cmd.CombinedOutput()
		if err != nil {
			text := strings.TrimSpace(string(said))
			if len(text) > maxSaid {
				text = "…" + strings.ToValidUTF8(text[len(text)-maxSaid:], "")
			}
			return fmt.Errorf("fetch the tag %s: %w\n%s", tag, err, text)
		}
	}
	return nil
}

func verifyPluginVersion(root, version string) error {
	yamlRaw, err := os.ReadFile(filepath.Join(root, "plugin.yaml"))
	if err != nil {
		return fmt.Errorf("read plugin.yaml: %w", err)
	}
	if !strings.Contains(string(yamlRaw), "version: "+version) && !strings.Contains(string(yamlRaw), `version: "`+version+`"`) {
		return fmt.Errorf("plugin.yaml does not declare version %s", version)
	}
	initRaw, err := os.ReadFile(filepath.Join(root, "__init__.py"))
	if err != nil {
		return fmt.Errorf("read __init__.py: %w", err)
	}
	if !strings.Contains(string(initRaw), `__version__ = "`+version+`"`) {
		return fmt.Errorf("__init__.py does not declare __version__ = %q", version)
	}
	return nil
}

func skipPluginEntry(rel string, d fs.DirEntry) bool {
	name := d.Name()
	if name == "__pycache__" || name == "tests" || name == ".DS_Store" || name == "geminise.json" || strings.HasSuffix(name, ".pyc") {
		return true
	}
	if strings.HasPrefix(name, ".") && name != "." {
		return true
	}
	return false
}

func packPluginTarGz(root, outPath string) error {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if skipPluginEntry(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("plugin contains non-regular file %q", rel)
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("plugin directory has no files")
	}
	sort.Strings(files)

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		out.Close()
		return err
	}
	gz.ModTime = time.Unix(0, 0).UTC()
	tw := tar.NewWriter(gz)

	for _, rel := range files {
		full := filepath.Join(root, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			tw.Close()
			gz.Close()
			out.Close()
			return err
		}
		mode := int64(0o644)
		if filepath.Base(rel) == "cli.py" {
			mode = 0o755
		}
		hdr := &tar.Header{
			Name:     "geminish/" + filepath.ToSlash(rel),
			Mode:     mode,
			Size:     int64(len(data)),
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			tw.Close()
			gz.Close()
			out.Close()
			return err
		}
		if _, err := io.WriteString(tw, string(data)); err != nil {
			tw.Close()
			gz.Close()
			out.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		out.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
