// Command fakeagent は E2E テスト用の偽エージェント。
//
// claude / hermes / 任意コマンドの代わりに geminus から起動され、決まった出力を返す。
//
//	fakeagent --version                         バージョンを出して終了する
//	fakeagent -p ... <prompt>                   claude のワンショットを真似る
//	fakeagent chat ... -q <prompt> --oneshot    hermes のワンショットを真似る
//	fakeagent [args...]                         対話。標準入力を1行ずつエコーする
//
// 対話では ANSI 付きの見出し、受け取った引数、標準入力が端末かどうかを出してから、
// 入力の各行を "echo:<行>" で返す。"quit" の行か EOF で終わる。
//
// 環境変数:
//
//	FAKE_EXIT=<n>       終了コード（既定は 0）
//	FAKE_RECORD=<path>  対話で受け取った行を、そのままこのファイルに書く。
//	                    PTY のエコーが混ざる出力を読まずに、届いた入力を確かめるために使う。
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

const (
	versionLine = "fakeagent 0.0.1"
	// banner は ANSI の色付き。記録と `log` の除去を確かめるために入れている。
	banner    = "\x1b[1;32mfakeagent\x1b[0m ready"
	farewell  = "\x1b[2mbye\x1b[0m"
	quitLine  = "quit"
	envExit   = "FAKE_EXIT"
	envRecord = "FAKE_RECORD"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	code, err := exitCode(os.Getenv(envExit))
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	if len(args) == 1 && args[0] == "--version" {
		say(stdout, versionLine)
		return 0
	}

	say(stdout, "argv:"+strings.Join(args, " "))
	say(stdout, "tty:"+strconv.FormatBool(term.IsTerminal(int(stdin.Fd()))))
	if prompt, ok := oneshotPrompt(args); ok {
		say(stdout, "reply:"+prompt)
		return code
	}

	received, closeRecord, err := openRecord(os.Getenv(envRecord))
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	say(stdout, banner)
	err = echoLines(stdin, stdout, received)
	if closeErr := closeRecord(); err == nil {
		err = closeErr
	}
	if err != nil {
		fmt.Fprintf(stderr, "fakeagent: %v\n", err)
		return 2
	}
	say(stdout, farewell)
	return code
}

// openRecord は受け取った行の書き込み先を開く。path が空なら捨てる。
func openRecord(path string) (io.Writer, func() error, error) {
	if path == "" {
		return io.Discard, func() error { return nil }, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// exitCode は FAKE_EXIT の値を読む。空なら 0。
func exitCode(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	code, err := strconv.Atoi(value)
	if err != nil || code < 0 || code > 255 {
		return 0, fmt.Errorf("%s=%q is not an exit code", envExit, value)
	}
	return code, nil
}

// say は1行を1回の write で出す。PTY のエコーと行の途中で混ざらないようにするため。
func say(w io.Writer, line string) {
	// 出力先が閉じていたら続ける意味が無いが、テスト用なのでエラーは終了コードに任せる。
	_, _ = io.WriteString(w, line+"\n")
}

// oneshotPrompt は args がワンショットの形なら、そのプロンプトを返す。
// claude は `-p ... <prompt>`（最後の引数）、hermes は `-q <prompt> --oneshot`。
func oneshotPrompt(args []string) (string, bool) {
	hasFlag := func(name string) bool {
		for _, arg := range args {
			if arg == name {
				return true
			}
		}
		return false
	}
	switch {
	case hasFlag("--oneshot"):
		for i, arg := range args {
			if arg == "-q" && i+1 < len(args) {
				return args[i+1], true
			}
		}
		return "", true
	case hasFlag("-p") && len(args) > 0:
		return args[len(args)-1], true
	default:
		return "", false
	}
}

// echoLines は r の各行を "echo:<行>" で w に返し、行そのものを received に書く。
// quit の行か EOF で終わる。
func echoLines(r io.Reader, w, received io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == quitLine {
			return nil
		}
		if _, err := io.WriteString(received, line+"\n"); err != nil {
			return err
		}
		say(w, "echo:"+line)
	}
	return scanner.Err()
}
