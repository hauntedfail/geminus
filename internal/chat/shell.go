package chat

// Shell-style lines. "!cd DIR" and "!pwd" move around the persona's computer
// the way a shell there would: from the directory chosen so far, not from the
// bridge's own. Like /cwd they work until the conversation starts.
//
// geminus cannot look at that computer, so a path is worked out from its text
// alone and the bridge says whether it exists when the first prompt arrives.

import (
	"errors"
	"fmt"
	"strings"
)

// shellLine reports whether line is one geminus runs itself, and returns its
// command and argument. Any other line starting with "!" is a prompt.
func shellLine(line string) (cmd, arg string, ok bool) {
	if !strings.HasPrefix(line, "!") {
		return "", "", false
	}
	cmd, arg, _ = strings.Cut(strings.TrimSpace(line[1:]), " ")
	switch cmd {
	case "cd", "pwd":
		return cmd, unquote(strings.TrimSpace(arg)), true
	}
	return "", "", false
}

// unquote drops one pair of quotes around s, for a name typed the way a shell
// wants one with spaces. A name needs none here: the rest of the line is it.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// shell runs a shell-style line.
func (a *App) shell(cmd, arg string) error {
	if a.cur == nil {
		return errors.New("pick a persona first (/persona)")
	}
	if cmd == "pwd" {
		if arg != "" {
			return errors.New("usage: !pwd")
		}
		return a.chdir("")
	}
	name := oneLine(a.cur.Name)
	if a.session != "" {
		return fmt.Errorf("this conversation runs in %s on %s and stays there; /new DIR starts another one in DIR", a.where(), name)
	}
	if err := validDir(arg); err != nil {
		return err
	}
	var next string
	switch arg {
	case "":
		// Like cd alone: back to where the bridge starts.
	case "-":
		if !a.wasElsewhere {
			return errors.New("no earlier directory to go back to")
		}
		next = a.lastCwd
	default:
		next = a.resolveDir(arg)
		if err := validDir(next); err != nil {
			return err
		}
	}
	a.lastCwd, a.wasElsewhere = a.cwd, true
	a.cwd = next
	a.Out.line("directory: " + a.where() + " on " + name + a.dirNote())
	return nil
}

// resolveDir reads dir from the directory chosen so far. The result is a full
// path whenever the bridge said which directory it starts in, because the
// bridge reads a relative one from there and not from the one chosen.
func (a *App) resolveDir(dir string) string {
	home := a.cur.Home
	windows := windowsPath(home, a.cwd, dir)
	base := a.cwd
	if home != "" {
		base = joinRemote(home, a.cwd, windows)
	}
	next := joinRemote(base, dir, windows)
	if home != "" && samePath(next, cleanRemote(home, windows), windows) {
		return ""
	}
	return next
}

// windowsPath reports whether any of the paths is written the Windows way: a
// drive letter or a network share.
func windowsPath(paths ...string) bool {
	for _, p := range paths {
		if strings.HasPrefix(p, `\\`) || len(p) >= 2 && p[1] == ':' && isLetter(p[0]) && (len(p) == 2 || isSep(p[2], true)) {
			return true
		}
	}
	return false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isSep reports whether c separates the parts of a path. A backslash is an
// ordinary character in a name anywhere but on Windows.
func isSep(c byte, windows bool) bool { return c == '/' || windows && c == '\\' }

// splitRoot cuts p into what it starts from and the rest. fixed says the root
// has nothing above it; "~" does, and only that computer knows what.
func splitRoot(p string, windows bool) (root, rest string, fixed bool) {
	if windows {
		switch {
		case strings.HasPrefix(p, `\\`):
			// \\server\share is the root of a network path.
			parts := strings.FieldsFunc(p, func(r rune) bool { return r == '\\' || r == '/' })
			if len(parts) < 2 {
				return p, "", true
			}
			return `\\` + parts[0] + `\` + parts[1] + `\`, strings.Join(parts[2:], `\`), true
		case len(p) >= 2 && p[1] == ':' && isLetter(p[0]):
			return p[:2] + `\`, p[2:], true
		case p != "" && isSep(p[0], true):
			// The top of the drive the bridge is on.
			return `\`, p[1:], true
		}
	} else if strings.HasPrefix(p, "/") {
		return "/", p[1:], true
	}
	if strings.HasPrefix(p, "~") {
		end := len(p)
		for i := 0; i < len(p); i++ {
			if isSep(p[i], windows) {
				end = i
				break
			}
		}
		return p[:end], p[end:], false
	}
	return "", p, false
}

// cleanRemote writes p the shortest way: no "." or doubled separators, and
// ".." taken out with the name before it. Above "/" there is nothing; above
// "~" or a relative start there is, so those keep the "..".
func cleanRemote(p string, windows bool) string {
	root, rest, fixed := splitRoot(p, windows)
	sep := "/"
	if windows {
		sep = `\`
	}
	var parts []string
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r < 0x80 && isSep(byte(r), windows) }) {
		switch {
		case part == ".":
		case part != "..":
			parts = append(parts, part)
		case len(parts) > 0 && parts[len(parts)-1] != "..":
			parts = parts[:len(parts)-1]
		case !fixed:
			parts = append(parts, "..")
		}
	}
	tail := strings.Join(parts, sep)
	switch {
	case fixed || root == "":
		return root + tail
	case tail == "":
		return root
	}
	return root + sep + tail
}

// joinRemote reads dir from base, the way cd would. A dir with a root of its
// own, or no base to start from, stands alone.
func joinRemote(base, dir string, windows bool) string {
	if root, _, _ := splitRoot(dir, windows); root != "" || base == "" {
		return cleanRemote(dir, windows)
	}
	if dir == "" {
		return cleanRemote(base, windows)
	}
	return cleanRemote(base+"/"+dir, windows)
}

// samePath compares two cleaned paths. Windows does not tell A from a.
func samePath(a, b string, windows bool) bool {
	if windows {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// shellHint is the line under the box while a shell-style line is typed.
func shellHint(text string, v view) (string, bool) {
	if !strings.HasPrefix(text, "!") || strings.Contains(text, "\n") {
		return "", false
	}
	word, _, more := strings.Cut(text, " ")
	if _, _, ok := shellLine(word); !ok && (more || len(word) < 2 || !strings.HasPrefix("!cd", word) && !strings.HasPrefix("!pwd", word)) {
		return "", false
	}
	switch {
	case v.persona == "":
		return "pick a persona first (/persona)", true
	case v.session != "":
		return "this conversation stays in its directory · /new, then !cd DIR", true
	}
	from := v.cwd
	if from == "" {
		from = v.home
	}
	if from == "" {
		from = "the default directory"
	}
	return "!cd DIR moves on " + v.persona + " from " + from + " · .. up · - back · alone: the default · !pwd shows", true
}
