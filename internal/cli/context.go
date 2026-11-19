package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hauntedfail/geminus/internal/agent"
	"github.com/hauntedfail/geminus/internal/session"
)

const (
	configFileName = "config.json"
	// 既定のタイトルに使う文字数の上限。
	maxTitleRunes = 60
)

// usageErr は使い方のエラー。report が ExitUsage に変える。
type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageErr{msg: fmt.Sprintf(format, args...)}
}

// report は err を stderr に出し、種類に応じた終了コードを返す。
func report(env Env, command string, err error) int {
	var usage *usageErr
	if errors.As(err, &usage) {
		return usageError(env, command, usage.msg)
	}
	return fail(env, command, err)
}

// warn は処理を止めない異常を stderr に出す。
func warn(env Env, command string, err error) {
	fmt.Fprintf(env.Stderr, "%s %s: warning: %v\n", programName, command, err)
}

// verboseLogf は -v のときだけ stderr に書く関数を返す。-v が無ければ nil。
func verboseLogf(inv Invocation) func(format string, args ...any) {
	if !inv.Options.Verbose {
		return nil
	}
	return func(format string, args ...any) {
		fmt.Fprintf(inv.Env.Stderr, programName+": "+format+"\n", args...)
	}
}

// openStore は --home / GEMINUS_HOME / ~/.geminus の順で状態ディレクトリを決めて開く。
func openStore(inv Invocation) (*session.Store, error) {
	home, err := session.ResolveHome(inv.Options.Home)
	if err != nil {
		return nil, err
	}
	return session.Open(home)
}

// config は $GEMINUS_HOME/config.json の内容。ファイルは無くてもよい。
type config struct {
	// Agent は --agent を省いたときのバックエンド。
	Agent string `json:"agent"`
}

// loadConfig は home の config.json を読む。無ければ空の config を返す。
// 読めない内容は黙って無視せず、エラーにする。
func loadConfig(home string) (config, error) {
	path := filepath.Join(home, configFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config{}, nil
		}
		return config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// defaultAgentName は新しいセッションで使うバックエンド名を決める。
// 優先順は --agent、config.json、claude。
func defaultAgentName(inv Invocation, home string) (string, error) {
	if inv.Options.Agent != "" {
		return inv.Options.Agent, nil
	}
	cfg, err := loadConfig(home)
	if err != nil {
		return "", err
	}
	if cfg.Agent != "" {
		return cfg.Agent, nil
	}
	return agent.DefaultName, nil
}

// lookupAdapter は name のアダプタを返す。未知の名前は使い方のエラーにする。
func lookupAdapter(name string) (agent.Adapter, error) {
	adapter, err := agent.Lookup(name)
	if errors.Is(err, agent.ErrUnknownAgent) {
		return nil, &usageErr{msg: err.Error()}
	}
	return adapter, err
}

// writeJSON は v を整形した JSON 1個として w に書く。
//
// encoding/json は DEL と C1 制御文字（U+0080〜U+009F）をそのまま出す。端末によっては
// 制御シーケンスとして解釈されるので、\u00XX に置き換える。これらは JSON の文字列の中に
// しか現れないので、置き換えても同じ値を表す JSON のままになる。
func writeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	var out bytes.Buffer
	for _, r := range buf.String() {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			fmt.Fprintf(&out, `\u%04x`, r)
			continue
		}
		out.WriteRune(r)
	}
	_, err := w.Write(out.Bytes())
	return err
}

// printable は、ファイルや他のプログラムから来た文字列を端末に出せる形にする。
// 制御文字（ESC、改行、C1 など）、双方向の向きを変える書式文字、壊れた UTF-8 を "?" に
// 置き換える。session.json の title や cwd に仕込まれたエスケープシーケンスを端末に
// 流さないため。人が読む出力に使い、--json の出力には使わない。
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r):
			return '?'
		case (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			return '?'
		default:
			return r
		}
	}, s)
}

// oneArg は位置引数がちょうど1つであることを確かめて返す。
func oneArg(args []string, what string) (string, error) {
	switch len(args) {
	case 0:
		return "", usagef("missing %s", what)
	case 1:
		return args[0], nil
	default:
		return "", usagef("unexpected argument %q", args[1])
	}
}

// noArgs は位置引数が無いことを確かめる。
func noArgs(args []string) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q", args[0])
	}
	return nil
}

// defaultTitle は --title が無いときのタイトルを args から作る。
// cmd の args はコマンド行なので、秘密を伏せてから使う。
func defaultTitle(adapter agent.Adapter, args []string) string {
	if adapter.Name() == agent.GenericName {
		args = agent.RedactArgv(args)
	}
	return truncateTitle(strings.Join(args, " "))
}

// truncateTitle は最初の行だけを残し、長ければ切り詰める。
func truncateTitle(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if utf8.RuneCountInString(s) <= maxTitleRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxTitleRunes-1]) + "…"
}

// sameDir は a と b が同じディレクトリを指すかを返す。
// シンボリックリンク越しの違い（/tmp と /private/tmp など）は同じと見なす。
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}
