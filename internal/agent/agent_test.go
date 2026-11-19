package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	testUUID = "3f2c9a52-6f0e-4c53-9a43-0d6c2b1f7e11"
	testID   = "20261119-105700-a1b2"
)

func mustLookup(t *testing.T, name string) Adapter {
	t.Helper()
	a, err := Lookup(name)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", name, err)
	}
	return a
}

func TestInteractiveArgv(t *testing.T) {
	uuidKey := SessionKey{Kind: KeyUUID, Value: testUUID}
	nameKey := SessionKey{Kind: KeyName, Value: testID}

	tests := []struct {
		name    string
		agent   string
		req     Request
		want    []string
		wantErr error
	}{
		{
			name:  "claude new session",
			agent: "claude",
			req:   Request{Key: uuidKey},
			want:  []string{"claude", "--session-id", testUUID},
		},
		{
			name:  "claude resume uses the same uuid",
			agent: "claude",
			req:   Request{Key: uuidKey, Resume: true},
			want:  []string{"claude", "--resume", testUUID},
		},
		{
			name:  "claude joins prompt words into one argument",
			agent: "claude",
			req:   Request{Key: uuidKey, Args: []string{"fix", "the", "bug"}},
			want:  []string{"claude", "--session-id", testUUID, "fix the bug"},
		},
		{
			name:  "claude resume with prompt",
			agent: "claude",
			req:   Request{Key: uuidKey, Resume: true, Args: []string{"continue"}},
			want:  []string{"claude", "--resume", testUUID, "continue"},
		},
		{
			name:    "claude rejects a non-uuid key",
			agent:   "claude",
			req:     Request{Key: SessionKey{Kind: KeyUUID, Value: "not-a-uuid"}},
			wantErr: ErrInvalidKey,
		},
		{
			name:    "claude rejects a key of another kind",
			agent:   "claude",
			req:     Request{Key: nameKey},
			wantErr: ErrInvalidKey,
		},
		{
			name:    "claude rejects a missing key",
			agent:   "claude",
			req:     Request{},
			wantErr: ErrInvalidKey,
		},
		{
			name:  "hermes new session",
			agent: "hermes",
			req:   Request{Key: nameKey},
			want:  []string{"hermes", "chat", "-c", testID, "--create-if-missing"},
		},
		{
			name:  "hermes resume is the same command",
			agent: "hermes",
			req:   Request{Key: nameKey, Resume: true},
			want:  []string{"hermes", "chat", "-c", testID, "--create-if-missing"},
		},
		{
			name:  "hermes seeds the session with -q",
			agent: "hermes",
			req:   Request{Key: nameKey, Args: []string{"fix", "the", "bug"}},
			want:  []string{"hermes", "chat", "-c", testID, "--create-if-missing", "-q", "fix the bug"},
		},
		{
			name:    "hermes rejects a name that would parse as a flag",
			agent:   "hermes",
			req:     Request{Key: SessionKey{Kind: KeyName, Value: "--yolo"}},
			wantErr: ErrInvalidKey,
		},
		{
			name:    "hermes rejects a uuid-kind key",
			agent:   "hermes",
			req:     Request{Key: uuidKey},
			wantErr: ErrInvalidKey,
		},
		{
			name:    "claude rejects a prompt that would parse as a flag",
			agent:   "claude",
			req:     Request{Key: uuidKey, Args: []string{"--dangerously-skip-permissions"}},
			wantErr: ErrInvalidPrompt,
		},
		{
			name:    "hermes rejects a prompt that would parse as a flag",
			agent:   "hermes",
			req:     Request{Key: nameKey, Args: []string{"-Q", "hi"}},
			wantErr: ErrInvalidPrompt,
		},
		{
			name:  "cmd passes arguments starting with a dash",
			agent: "cmd",
			req:   Request{Args: []string{"ls", "-la"}},
			want:  []string{"ls", "-la"},
		},
		{
			name:  "cmd runs the given command as is",
			agent: "cmd",
			req:   Request{Args: []string{"sh", "-c", "exit 7"}},
			want:  []string{"sh", "-c", "exit 7"},
		},
		{
			name:    "cmd cannot resume",
			agent:   "cmd",
			req:     Request{Resume: true, Args: []string{"sh"}},
			wantErr: ErrNoResume,
		},
		{
			name:    "cmd needs a command",
			agent:   "cmd",
			req:     Request{},
			wantErr: ErrNoCommand,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mustLookup(t, tt.agent).Interactive(tt.req)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v (argv = %q)", err, tt.wantErr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Interactive: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("argv = %q\nwant   %q", got, tt.want)
			}
		})
	}
}

func TestOneshotArgv(t *testing.T) {
	uuidKey := SessionKey{Kind: KeyUUID, Value: testUUID}
	nameKey := SessionKey{Kind: KeyName, Value: testID}

	tests := []struct {
		name    string
		agent   string
		req     Request
		prompt  string
		want    []string
		wantErr error
	}{
		{
			name:  "claude first turn",
			agent: "claude", req: Request{Key: uuidKey}, prompt: "say hi",
			want: []string{"claude", "-p", "--session-id", testUUID, "say hi"},
		},
		{
			name:  "claude later turn",
			agent: "claude", req: Request{Key: uuidKey, Resume: true}, prompt: "and again",
			want: []string{"claude", "-p", "--resume", testUUID, "and again"},
		},
		{
			name:  "claude empty prompt",
			agent: "claude", req: Request{Key: uuidKey}, prompt: "  ",
			wantErr: ErrNoPrompt,
		},
		{
			name:  "claude invalid key",
			agent: "claude", req: Request{}, prompt: "hi",
			wantErr: ErrInvalidKey,
		},
		{
			name:  "claude prompt that would parse as a flag",
			agent: "claude", req: Request{Key: uuidKey}, prompt: "--resume",
			wantErr: ErrInvalidPrompt,
		},
		{
			name:  "hermes prompt that would parse as a flag",
			agent: "hermes", req: Request{Key: nameKey}, prompt: " -x",
			wantErr: ErrInvalidPrompt,
		},
		{
			name:  "a dash inside the prompt is fine",
			agent: "claude", req: Request{Key: uuidKey}, prompt: "use -v and --json",
			want: []string{"claude", "-p", "--session-id", testUUID, "use -v and --json"},
		},
		{
			name:  "hermes first turn",
			agent: "hermes", req: Request{Key: nameKey}, prompt: "say hi",
			want: []string{"hermes", "chat", "-c", testID, "--create-if-missing", "-q", "say hi", "--oneshot"},
		},
		{
			name:  "hermes later turn",
			agent: "hermes", req: Request{Key: nameKey, Resume: true}, prompt: "and again",
			want: []string{"hermes", "chat", "-c", testID, "--create-if-missing", "-q", "and again", "--oneshot"},
		},
		{
			name:  "hermes empty prompt",
			agent: "hermes", req: Request{Key: nameKey}, prompt: "",
			wantErr: ErrNoPrompt,
		},
		{
			name:  "cmd has no one-shot mode",
			agent: "cmd", req: Request{Args: []string{"sh"}}, prompt: "hi",
			wantErr: ErrNoOneshot,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mustLookup(t, tt.agent).Oneshot(tt.req, tt.prompt)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v (argv = %q)", err, tt.wantErr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Oneshot: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("argv = %q\nwant   %q", got, tt.want)
			}
		})
	}
}

func TestArgvDoesNotAliasRequest(t *testing.T) {
	args := []string{"sh", "-c", "true"}
	got, err := mustLookup(t, "cmd").Interactive(Request{Args: args})
	if err != nil {
		t.Fatal(err)
	}
	got[0] = "changed"
	if args[0] != "sh" {
		t.Error("Interactive returned the caller's slice")
	}
}

func TestNewKey(t *testing.T) {
	t.Run("claude generates a fresh uuid every time", func(t *testing.T) {
		a := mustLookup(t, "claude")
		first, err := a.NewKey(testID, false)
		if err != nil {
			t.Fatal(err)
		}
		second, err := a.NewKey(testID, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []SessionKey{first, second} {
			if key.Kind != KeyUUID || !IsUUID(key.Value) {
				t.Errorf("key = %+v, want a valid uuid", key)
			}
		}
		if first.Value == second.Value {
			t.Error("two keys share the same uuid")
		}
		// The generated key is ready to use when launching a session.
		argv, err := a.Interactive(Request{Key: first})
		if err != nil || argv[1] != "--session-id" || argv[2] != first.Value {
			t.Errorf("argv = %q, err = %v", argv, err)
		}
	})

	t.Run("hermes uses the geminus id as the session name", func(t *testing.T) {
		a := mustLookup(t, "hermes")
		key, err := a.NewKey(testID, false)
		if err != nil {
			t.Fatal(err)
		}
		if key != (SessionKey{Kind: KeyName, Value: testID}) {
			t.Errorf("key = %+v", key)
		}
	})

	t.Run("hermes fresh key differs but stays tied to the geminus id", func(t *testing.T) {
		a := mustLookup(t, "hermes")
		first, err := a.NewKey(testID, true)
		if err != nil {
			t.Fatal(err)
		}
		second, err := a.NewKey(testID, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []SessionKey{first, second} {
			if key.Kind != KeyName || key.Value == testID || !strings.HasPrefix(key.Value, testID+"-") {
				t.Errorf("fresh key = %+v, want %s-<suffix>", key, testID)
			}
			if _, err := a.Interactive(Request{Key: key}); err != nil {
				t.Errorf("fresh key %q is not usable: %v", key.Value, err)
			}
		}
		if first == second {
			t.Error("two fresh keys are identical")
		}
	})

	t.Run("hermes rejects an unusable geminus id", func(t *testing.T) {
		if _, err := mustLookup(t, "hermes").NewKey("--yolo", false); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("err = %v, want ErrInvalidKey", err)
		}
	})

	t.Run("cmd has no key", func(t *testing.T) {
		key, err := mustLookup(t, "cmd").NewKey(testID, false)
		if err != nil {
			t.Fatal(err)
		}
		if key != (SessionKey{Kind: KeyNone}) {
			t.Errorf("key = %+v, want kind none", key)
		}
	})
}

func TestCapabilities(t *testing.T) {
	tests := []struct {
		agent  string
		binary string
		want   Capabilities
	}{
		{"claude", "claude", Capabilities{Resume: true, Oneshot: true, SessionKey: KeyUUID}},
		{"hermes", "hermes", Capabilities{Resume: true, Oneshot: true, SessionKey: KeyName}},
		{"cmd", "", Capabilities{Resume: false, Oneshot: false, SessionKey: KeyNone}},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			a := mustLookup(t, tt.agent)
			if a.Name() != tt.agent {
				t.Errorf("Name = %q", a.Name())
			}
			if a.Binary() != tt.binary {
				t.Errorf("Binary = %q, want %q", a.Binary(), tt.binary)
			}
			if got := a.Capabilities(); got != tt.want {
				t.Errorf("Capabilities = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLookupUnknownAgentListsCandidates(t *testing.T) {
	for _, name := range []string{"gpt", "", "Claude", "claude "} {
		t.Run(name, func(t *testing.T) {
			_, err := Lookup(name)
			if !errors.Is(err, ErrUnknownAgent) {
				t.Fatalf("err = %v, want ErrUnknownAgent", err)
			}
			for _, candidate := range []string{"claude", "hermes", "cmd"} {
				if !strings.Contains(err.Error(), candidate) {
					t.Errorf("err = %q, want it to list %q", err, candidate)
				}
			}
		})
	}
}

func TestNames(t *testing.T) {
	got := Names()
	if want := []string{"claude", "hermes", "cmd"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	if DefaultName != "claude" || GenericName != "cmd" {
		t.Errorf("DefaultName = %q, GenericName = %q", DefaultName, GenericName)
	}
	got[0] = "changed"
	if Names()[0] != "claude" {
		t.Error("Names returned shared state")
	}
}

// fakeBinaries places executables in dir and makes it the only directory on PATH.
func fakeBinaries(t *testing.T, names ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX executable bits")
	}
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestDetect(t *testing.T) {
	dir := fakeBinaries(t, "claude")

	t.Run("installed", func(t *testing.T) {
		d := Detect(mustLookup(t, "claude"))
		if !d.Found || d.Path != filepath.Join(dir, "claude") || d.Error != "" {
			t.Errorf("detection = %+v", d)
		}
		if d.Name != "claude" || d.Binary != "claude" || !d.Capabilities.Resume {
			t.Errorf("detection = %+v", d)
		}
	})
	t.Run("missing", func(t *testing.T) {
		d := Detect(mustLookup(t, "hermes"))
		if d.Found || d.Path != "" {
			t.Errorf("detection = %+v, want not found", d)
		}
		if !strings.Contains(d.Error, "hermes") {
			t.Errorf("Error = %q, want it to name the binary", d.Error)
		}
	})
	t.Run("cmd needs no binary", func(t *testing.T) {
		d := Detect(mustLookup(t, "cmd"))
		if !d.Found || d.Path != "" || d.Error != "" {
			t.Errorf("detection = %+v", d)
		}
	})
	t.Run("all", func(t *testing.T) {
		all := DetectAll()
		if len(all) != 3 || all[0].Name != "claude" || all[1].Name != "hermes" || all[2].Name != "cmd" {
			t.Fatalf("DetectAll = %+v", all)
		}
		if !all[0].Found || all[1].Found {
			t.Errorf("DetectAll = %+v", all)
		}
	})
}

func TestResolveArgv(t *testing.T) {
	dir := fakeBinaries(t, "claude")

	t.Run("replaces the binary with its path", func(t *testing.T) {
		argv := []string{"claude", "--session-id", testUUID}
		got, err := ResolveArgv(argv)
		if err != nil {
			t.Fatalf("ResolveArgv: %v", err)
		}
		want := []string{filepath.Join(dir, "claude"), "--session-id", testUUID}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("argv = %q, want %q", got, want)
		}
		if argv[0] != "claude" {
			t.Error("ResolveArgv modified its input")
		}
	})
	t.Run("missing binary is a clear error", func(t *testing.T) {
		_, err := ResolveArgv([]string{"hermes", "chat"})
		if !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("err = %v, want ErrNotInstalled", err)
		}
		if !strings.Contains(err.Error(), `"hermes"`) {
			t.Errorf("err = %q, want it to name the binary", err)
		}
	})
	t.Run("empty argv", func(t *testing.T) {
		if _, err := ResolveArgv(nil); !errors.Is(err, ErrNoCommand) {
			t.Errorf("err = %v, want ErrNoCommand", err)
		}
	})
	t.Run("non-executable file is not installed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveArgv([]string{"plain"}); !errors.Is(err, ErrNotInstalled) {
			t.Errorf("err = %v, want ErrNotInstalled", err)
		}
	})
}

func TestUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewUUID()
		if err != nil {
			t.Fatal(err)
		}
		if !IsUUID(id) {
			t.Fatalf("NewUUID() = %q is not a valid uuid", id)
		}
		if id[14] != '4' {
			t.Fatalf("NewUUID() = %q, want version 4", id)
		}
		if !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("NewUUID() = %q, want RFC 4122 variant", id)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid %q", id)
		}
		seen[id] = true
	}
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{testUUID, true},
		{strings.ToUpper(testUUID), true},
		{"00000000-0000-0000-0000-000000000000", true},
		{"3f2c9a52-6f0e-4c53-9a43-0d6c2b1f7e1", false},
		{"3f2c9a52-6f0e-4c53-9a43-0d6c2b1f7e111", false},
		{"3f2c9a526f0e4c539a430d6c2b1f7e11", false},
		{"3f2c9a52_6f0e_4c53_9a43_0d6c2b1f7e11", false},
		{"zf2c9a52-6f0e-4c53-9a43-0d6c2b1f7e11", false},
		{"--resume", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsUUID(tt.in); got != tt.want {
			t.Errorf("IsUUID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRedactArgv(t *testing.T) {
	const r = Redacted
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nothing secret", []string{"claude", "--session-id", testUUID, "fix the bug"}, []string{"claude", "--session-id", testUUID, "fix the bug"}},
		{"api key as separate value", []string{"hermes", "chat", "--api-key", "sk-123", "-q", "hi"}, []string{"hermes", "chat", "--api-key", r, "-q", "hi"}},
		{"api key with equals", []string{"x", "--api-key=sk-123"}, []string{"x", "--api-key=" + r}},
		{"token", []string{"x", "--token", "abc"}, []string{"x", "--token", r}},
		{"prefixed token", []string{"x", "--access-token", "abc", "--github_token=def"}, []string{"x", "--access-token", r, "--github_token=" + r}},
		{"password", []string{"x", "--password", "hunter2", "--db-password=p"}, []string{"x", "--password", r, "--db-password=" + r}},
		{"secret", []string{"x", "--client-secret", "s"}, []string{"x", "--client-secret", r}},
		{"case and separators", []string{"x", "--API-KEY", "a", "--api_key", "b", "--apikey", "c"}, []string{"x", "--API-KEY", r, "--api_key", r, "--apikey", r}},
		{"single dash long flag", []string{"x", "-token", "abc"}, []string{"x", "-token", r}},
		{"environment style assignment", []string{"env", "OPENAI_API_KEY=sk-1", "HOME=/h", "tool"}, []string{"env", "OPENAI_API_KEY=" + r, "HOME=/h", "tool"}},
		{"value that looks like a flag is still hidden", []string{"x", "--token", "-abc"}, []string{"x", "--token", r}},
		{"flag without a value", []string{"x", "--token"}, []string{"x", "--token"}},
		{"empty value with equals", []string{"x", "--token="}, []string{"x", "--token="}},
		{"similar names are kept", []string{"x", "--max-tokens", "100", "--tokenizer", "bpe", "--keyboard", "us", "-p", "hi"}, []string{"x", "--max-tokens", "100", "--tokenizer", "bpe", "--keyboard", "us", "-p", "hi"}},
		{"after double dash", []string{"x", "--", "curl", "--token", "abc"}, []string{"x", "--", "curl", "--token", r}},
		{"camel case and joined names", []string{"x", "--accessToken", "a", "--clientSecret=b", "--authtoken", "c"}, []string{"x", "--accessToken", r, "--clientSecret=" + r, "--authtoken", r}},
		{"other credential names", []string{"x", "--passphrase", "a", "--private-key", "b", "--credentials=c", "--auth", "d"}, []string{"x", "--passphrase", r, "--private-key", r, "--credentials=" + r, "--auth", r}},
		{"authorization header inside one argument", []string{"curl", "-H", "Authorization: Bearer abc.def-123", "https://example.com"}, []string{"curl", "-H", "Authorization: Bearer " + r, "https://example.com"}},
		{"basic auth header", []string{"curl", "-H", "authorization: basic dXNlcjpwdw=="}, []string{"curl", "-H", "authorization: basic " + r}},
		{"url with password", []string{"git", "clone", "https://user:hunter2@example.com/repo.git"}, []string{"git", "clone", "https://user:" + r + "@example.com/repo.git"}},
		{"assignment inside a shell command", []string{"sh", "-c", "API_KEY=abc123 curl -s https://example.com"}, []string{"sh", "-c", "API_KEY=" + r + " curl -s https://example.com"}},
		{"several secrets in one argument", []string{"sh", "-c", "GH_TOKEN=a DB_PASSWORD=b run"}, []string{"sh", "-c", "GH_TOKEN=" + r + " DB_PASSWORD=" + r + " run"}},
		{"plain url and text are kept", []string{"curl", "https://example.com:8443/a@b", "the token is in the vault", "a=b"}, []string{"curl", "https://example.com:8443/a@b", "the token is in the vault", "a=b"}},
		{"empty", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.in...)
			got := RedactArgv(in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RedactArgv = %q\nwant         %q", got, tt.want)
			}
			if !reflect.DeepEqual(in, append([]string(nil), tt.in...)) {
				t.Errorf("input was modified: %q", in)
			}
		})
	}
}
