// Package cli は geminus のサブコマンドを解釈し、終了コードを決める。
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// 終了コード。これ以外の値は子プロセスの終了コードをそのまま返す（run / ask）。
const (
	ExitOK    = 0 // 正常終了
	ExitError = 1 // geminus 自体のエラー（I/O、PTY 確保失敗など）
	ExitUsage = 2 // 使い方のエラー
)

const programName = "geminus"

// Env はコマンドが使う入出力。テストではバッファに差し替える。
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Invocation は1回のコマンド実行に渡す、解釈済みの入力。
type Invocation struct {
	Env     Env
	Options Options
	// Args はフラグを除いた位置引数。
	Args []string
}

// Run は args（プログラム名を除く）を解釈して実行し、終了コードを返す。
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	env := Env{Stdin: stdin, Stdout: stdout, Stderr: stderr}

	var opts Options
	global := newGlobalFlagSet(&opts)
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(env.Stdout)
			return ExitOK
		}
		return usageError(env, "", err.Error())
	}

	rest := global.Args()
	if len(rest) == 0 {
		printUsage(env.Stderr)
		return ExitUsage
	}

	name, cmdArgs := rest[0], rest[1:]
	if name == "help" {
		return runHelp(env, cmdArgs)
	}
	cmd, ok := lookup(name)
	if !ok {
		return usageError(env, "", fmt.Sprintf("unknown command %q", name))
	}

	fs := newFlagSet(cmd, &opts)
	positionals, err := parseArgs(fs, cmdArgs, cmd.interspersed)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandHelp(env.Stdout, cmd)
			return ExitOK
		}
		return usageError(env, cmd.name, err.Error())
	}
	return cmd.run(Invocation{Env: env, Options: opts, Args: positionals})
}

func runHelp(env Env, args []string) int {
	switch len(args) {
	case 0:
		printUsage(env.Stdout)
		return ExitOK
	case 1:
		cmd, ok := lookup(args[0])
		if !ok {
			return usageError(env, "", fmt.Sprintf("unknown command %q", args[0]))
		}
		printCommandHelp(env.Stdout, cmd)
		return ExitOK
	default:
		return usageError(env, "", "help takes at most one command name")
	}
}

// usageError は使い方のエラーを stderr に出し、ExitUsage を返す。
// command が空ならトップレベルのエラーとして扱う。
func usageError(env Env, command, msg string) int {
	if command == "" {
		fmt.Fprintf(env.Stderr, "%s: %s\nRun '%s --help' for usage.\n", programName, msg, programName)
		return ExitUsage
	}
	fmt.Fprintf(env.Stderr, "%s %s: %s\nRun '%s help %s' for usage.\n", programName, command, msg, programName, command)
	return ExitUsage
}

// fail は geminus 自体のエラーを stderr に出し、ExitError を返す。
func fail(env Env, command string, err error) int {
	fmt.Fprintf(env.Stderr, "%s %s: %v\n", programName, command, err)
	return ExitError
}
