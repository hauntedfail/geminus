package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeProfile puts the given files into dir, creating it; an empty content
// leaves that file out.
func writeProfile(t *testing.T, dir, honcho, group string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"honcho.json": honcho, "geminise.json": group} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func setup(t *testing.T, honcho, group string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(EnvHermesHome, home)
	// On Windows Load looks in these two without HERMES_HOME; keep it off
	// the real profile.
	t.Setenv(envLocalAppData, t.TempDir())
	t.Setenv(envUserProfile, t.TempDir())
	writeProfile(t, home, honcho, group)
}

const (
	goodHoncho = `{"baseUrl":"https://honcho.example","workspace":"ws","apiKey":"k","aiPeer":"chrome","peerName":"h1karu","other":1}`
	secretHex  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	goodGroup  = `{"from":"chrome","members":{"h1karu":"human","chrome":"agent","mirai":"agent"},"secret":"` + secretHex + `","poll_seconds":10}`
)

func TestLoadReadsTheExistingFiles(t *testing.T) {
	setup(t, goodHoncho, goodGroup)
	p, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.BaseURL != "https://honcho.example" || p.Workspace != "ws" || p.APIKey != "k" || p.Human != "h1karu" || p.Local != "chrome" {
		t.Errorf("profile = %+v", p)
	}
	if len(p.Secret) != 32 || p.Secret[31] != 0x1f {
		t.Errorf("secret has %d bytes", len(p.Secret))
	}
	if !p.IsAgent("mirai") || p.IsAgent("h1karu") || p.IsAgent("stranger") {
		t.Error("IsAgent is wrong")
	}
}

func TestLoadSaysWhatIsMissingAndNeverPrintsSecrets(t *testing.T) {
	for name, files := range map[string][2]string{
		"no honcho.json":   {"", goodGroup},
		"no geminise.json": {goodHoncho, ""},
		"short secret":     {goodHoncho, `{"members":{"h1karu":"human"},"secret":"abcd"}`},
		"owner not human":  {goodHoncho, strings.Replace(goodGroup, `"h1karu":"human"`, `"h1karu":"agent"`, 1)},
		"no token":         {`{"baseUrl":"https://h","workspace":"ws","peerName":"h1karu"}`, goodGroup},
		"broken json":      {`{"baseUrl":`, goodGroup},
	} {
		setup(t, files[0], files[1])
		_, err := Load()
		if err == nil {
			t.Errorf("%s: Load succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), secretHex) || strings.Contains(err.Error(), "abcd") {
			t.Errorf("%s: the error shows the secret: %v", name, err)
		}
	}
}

// env is a fake environment for hermesHomeFor.
type env map[string]string

func (e env) get(key string) string { return e[key] }

// honchoIn is a honcho.json whose workspace tells which directory was read.
func honchoIn(workspace string) string {
	return fmt.Sprintf(`{"baseUrl":"https://honcho.example","workspace":%q,"apiKey":"k","aiPeer":"chrome","peerName":"h1karu"}`, workspace)
}

// loadFor is Load on another operating system and environment.
func loadFor(goos string, e env) (Profile, error) {
	home, err := hermesHomeFor(goos, e.get)
	if err != nil {
		return Profile{}, err
	}
	return loadFrom(home)
}

func wantWorkspace(t *testing.T, goos string, e env, want string) {
	t.Helper()
	p, err := loadFor(goos, e)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Workspace != want {
		t.Errorf("read the profile of %q, want %q", p.Workspace, want)
	}
}

func TestWindowsReadsLocalAppDataWithoutHermesHome(t *testing.T) {
	local, user := t.TempDir(), t.TempDir()
	writeProfile(t, filepath.Join(local, "hermes"), honchoIn("local"), goodGroup)
	// The older place holds a profile too and must not win.
	writeProfile(t, filepath.Join(user, ".hermes"), honchoIn("user"), goodGroup)
	e := env{envLocalAppData: local, envUserProfile: user}
	home, err := hermesHomeFor("windows", e.get)
	if err != nil {
		t.Fatalf("hermesHomeFor: %v", err)
	}
	if want := filepath.Join(local, "hermes"); home != want {
		t.Errorf("home = %q, want %q", home, want)
	}
	wantWorkspace(t, "windows", e, "local")
}

func TestWindowsReadsLocalAppDataWithoutUserProfile(t *testing.T) {
	local := t.TempDir()
	writeProfile(t, filepath.Join(local, "hermes"), honchoIn("local"), goodGroup)
	wantWorkspace(t, "windows", env{envLocalAppData: local}, "local")
}

func TestWindowsKeepsAHermesHomeWithoutFiles(t *testing.T) {
	// Hermes and the geminish bridge take HERMES_HOME as given, so geminus
	// must not read another profile than they do.
	empty, local, user := t.TempDir(), t.TempDir(), t.TempDir()
	writeProfile(t, filepath.Join(local, "hermes"), honchoIn("local"), goodGroup)
	writeProfile(t, filepath.Join(user, ".hermes"), honchoIn("user"), goodGroup)
	_, err := loadFor("windows", env{EnvHermesHome: empty, envLocalAppData: local, envUserProfile: user})
	if err == nil {
		t.Fatal("Load fell back to another directory")
	}
	if !strings.Contains(err.Error(), filepath.Join(empty, "honcho.json")) {
		t.Errorf("the error does not name the file in HERMES_HOME: %v", err)
	}
}

func TestWindowsUsesHermesHomeWhenSet(t *testing.T) {
	chosen, local, user := filepath.Join(t.TempDir(), "my hermes"), t.TempDir(), t.TempDir()
	writeProfile(t, chosen, honchoIn("chosen"), goodGroup)
	writeProfile(t, filepath.Join(local, "hermes"), honchoIn("local"), goodGroup)
	writeProfile(t, filepath.Join(user, ".hermes"), honchoIn("user"), goodGroup)
	// The path is used as typed, space included.
	e := env{EnvHermesHome: chosen, envLocalAppData: local, envUserProfile: user}
	home, err := hermesHomeFor("windows", e.get)
	if err != nil {
		t.Fatalf("hermesHomeFor: %v", err)
	}
	if home != chosen {
		t.Errorf("home = %q, want %q", home, chosen)
	}
	wantWorkspace(t, "windows", e, "chosen")
}

func TestWindowsFallsBackToUserProfileWithoutLocalAppData(t *testing.T) {
	user, cwd := t.TempDir(), t.TempDir()
	writeProfile(t, filepath.Join(user, ".hermes"), honchoIn("user"), goodGroup)
	// An empty LOCALAPPDATA must not turn into the relative path "hermes".
	writeProfile(t, filepath.Join(cwd, "hermes"), honchoIn("relative"), goodGroup)
	// The working directory belongs to the whole process: no test in this
	// package may run in parallel with this one.
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(before); err != nil {
			t.Error(err)
		}
	})
	wantWorkspace(t, "windows", env{envUserProfile: user}, "user")
}

func TestWindowsStopsAtAHonchoJSONItCannotReach(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory that chmod can close")
	}
	local, user := t.TempDir(), t.TempDir()
	closed := filepath.Join(local, "hermes")
	writeProfile(t, closed, honchoIn("closed"), goodGroup)
	writeProfile(t, filepath.Join(user, ".hermes"), honchoIn("user"), goodGroup)
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(closed, 0o700); err != nil {
			t.Error(err)
		}
	})
	_, err := loadFor("windows", env{envLocalAppData: local, envUserProfile: user})
	if err == nil {
		t.Fatal("Load moved on to another profile")
	}
	if !strings.Contains(err.Error(), filepath.Join(closed, "honcho.json")) {
		t.Errorf("the error does not name the file under LOCALAPPDATA: %v", err)
	}
}

func TestWindowsNamesEveryPathItTried(t *testing.T) {
	local, user := t.TempDir(), t.TempDir()
	_, err := loadFor("windows", env{envLocalAppData: local, envUserProfile: user})
	if err == nil {
		t.Fatal("Load succeeded")
	}
	want := "no Hermes profile found: no honcho.json in " +
		filepath.Join(local, "hermes") + " or " + filepath.Join(user, ".hermes")
	if err.Error() != want {
		t.Errorf("error = %q\n want  %q", err, want)
	}
}

func TestWindowsNamesTheOnlyPathItTried(t *testing.T) {
	user := t.TempDir()
	_, err := loadFor("windows", env{envUserProfile: user})
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if want := "no Hermes profile found: no honcho.json in " + filepath.Join(user, ".hermes"); err.Error() != want {
		t.Errorf("error = %q\n want  %q", err, want)
	}
}

func TestWindowsWithoutAnyLocationSaysSo(t *testing.T) {
	_, err := loadFor("windows", env{})
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, name := range []string{EnvHermesHome, envLocalAppData, envUserProfile} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the error spans lines: %q", err)
	}
}

func TestUnixKeepsAHermesHomeWithoutFiles(t *testing.T) {
	empty, home, local := t.TempDir(), t.TempDir(), t.TempDir()
	writeProfile(t, filepath.Join(home, ".hermes"), honchoIn("home"), goodGroup)
	writeProfile(t, filepath.Join(local, "hermes"), honchoIn("local"), goodGroup)
	_, err := loadFor("darwin", env{EnvHermesHome: empty, "HOME": home, envLocalAppData: local, envUserProfile: home})
	if err == nil {
		t.Fatal("Load fell back to another directory")
	}
	if !strings.Contains(err.Error(), filepath.Join(empty, "honcho.json")) {
		t.Errorf("the error does not name the file in HERMES_HOME: %v", err)
	}
}

func TestUnixDefaultsToDotHermesInHome(t *testing.T) {
	home, local := t.TempDir(), t.TempDir()
	writeProfile(t, filepath.Join(local, "hermes"), honchoIn("local"), goodGroup)
	e := env{"HOME": home, envLocalAppData: local}
	// Nothing is probed: the directory does not exist yet and is still the answer.
	for _, goos := range []string{"darwin", "linux"} {
		got, err := hermesHomeFor(goos, e.get)
		if err != nil {
			t.Fatalf("%s: hermesHomeFor: %v", goos, err)
		}
		if want := filepath.Join(home, ".hermes"); got != want {
			t.Errorf("%s: home = %q, want %q", goos, got, want)
		}
	}
	writeProfile(t, filepath.Join(home, ".hermes"), honchoIn("home"), goodGroup)
	wantWorkspace(t, "darwin", e, "home")
}

func TestProfileAutoUpdateSetting(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, honchoIn("local"), goodGroup)
	p, err := loadFrom(dir)
	if err != nil {
		t.Fatalf("loadFrom default: %v", err)
	}
	if p.AutoUpdate != nil {
		t.Fatalf("default AutoUpdate = %v, want nil", *p.AutoUpdate)
	}

	for _, want := range []bool{true, false} {
		dir := t.TempDir()
		val := "false"
		if want {
			val = "true"
		}
		group := `{"from":"chrome","members":{"h1karu":"human","chrome":"agent"},"secret":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","geminus":{"auto_update":` + val + `}}`
		writeProfile(t, dir, honchoIn("local"), group)
		p, err := loadFrom(dir)
		if err != nil {
			t.Fatalf("loadFrom(%v): %v", want, err)
		}
		if p.AutoUpdate == nil || *p.AutoUpdate != want {
			t.Fatalf("AutoUpdate = %v, want %v", p.AutoUpdate, want)
		}
	}
}
