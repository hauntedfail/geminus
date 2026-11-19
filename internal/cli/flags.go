package cli

import (
	"flag"
	"io"
	"strings"
)

// Globals は全コマンド共通のフラグ。コマンド名の前後どちらに書いてもよい。
type Globals struct {
	// Home は GEMINUS_HOME を上書きする。空なら環境変数、無ければ ~/.geminus。
	Home string
	// Agent はバックエンド名。空なら config、無ければ claude。
	Agent string
	// JSON は機械可読な出力を選ぶ。
	JSON bool
	// Verbose は診断を stderr に出す。
	Verbose bool
}

// RunOptions は `geminus run` のフラグ。
type RunOptions struct {
	Title    string
	Resume   string
	Fresh    bool
	NoRecord bool
	Force    bool
}

// AskOptions は `geminus ask` のフラグ。
type AskOptions struct {
	Session  string
	Continue bool
}

// LsOptions は `geminus ls` のフラグ。
type LsOptions struct {
	All bool
}

// LogOptions は `geminus log` のフラグ。
type LogOptions struct {
	Raw  bool
	Tail int
}

// RmOptions は `geminus rm` のフラグ。
type RmOptions struct {
	Force bool
}

// Options は解釈済みのフラグ全体。
type Options struct {
	Globals
	Run RunOptions
	Ask AskOptions
	Ls  LsOptions
	Log LogOptions
	Rm  RmOptions
}

var globalFlagNames = map[string]bool{"home": true, "agent": true, "json": true, "v": true}

// bindGlobals は共通フラグを登録する。既定値に現在値を渡すので、
// コマンド名より前で指定された値はコマンド側の再解釈でも残る。
func bindGlobals(fs *flag.FlagSet, o *Options) {
	fs.StringVar(&o.Home, "home", o.Home, "keep state in `DIR` (default: $GEMINUS_HOME, else ~/.geminus)")
	fs.StringVar(&o.Agent, "agent", o.Agent, "use backend `NAME` (default: config, else claude)")
	fs.BoolVar(&o.JSON, "json", o.JSON, "machine-readable output")
	fs.BoolVar(&o.Verbose, "v", o.Verbose, "verbose diagnostics on stderr")
}

func newQuietFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	// エラー文と使い方は cli.Run が自分で出す。
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func newGlobalFlagSet(o *Options) *flag.FlagSet {
	fs := newQuietFlagSet(programName)
	bindGlobals(fs, o)
	return fs
}

func newFlagSet(cmd command, o *Options) *flag.FlagSet {
	fs := newQuietFlagSet(programName + " " + cmd.name)
	if cmd.flags != nil {
		cmd.flags(fs, o)
	}
	bindGlobals(fs, o)
	return fs
}

// parseArgs はフラグを解釈し、位置引数を返す。
// interspersed が true なら `show <id> --json` のように位置引数の後ろのフラグも解釈する。
// false なら最初の位置引数で解釈を止める（run / ask のプロンプトをそのまま通すため）。
// どちらも `--` 以降はすべて位置引数になる。
func parseArgs(fs *flag.FlagSet, args []string, interspersed bool) ([]string, error) {
	if !interspersed {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		return fs.Args(), nil
	}

	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(positionals, args[i+1:]...), nil
		}
		if !looksLikeFlag(arg) {
			positionals = append(positionals, arg)
			continue
		}
		tokens := []string{arg}
		if flagTakesNextArg(fs, arg) && i+1 < len(args) {
			i++
			tokens = append(tokens, args[i])
		}
		if err := fs.Parse(tokens); err != nil {
			return nil, err
		}
	}
	return positionals, nil
}

func looksLikeFlag(arg string) bool {
	return len(arg) >= 2 && arg[0] == '-'
}

// flagTakesNextArg は arg が次の引数を値として消費する形かを返す。
// 未定義のフラグは false を返し、fs.Parse にエラーを出させる。
func flagTakesNextArg(fs *flag.FlagSet, arg string) bool {
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if strings.Contains(name, "=") {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return false
	}
	return true
}
