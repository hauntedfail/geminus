package chat

// A choice replaces the screen while a list is being picked. The mascot says
// the question and the options; nothing is written into the transcript, so
// picking a persona and then a harness does not leave both lists on screen.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/hauntedfail/geminus/internal/splash"
)

type choiceResult struct {
	value string
	ok    bool
}

type choice struct {
	title, what string
	opts        []option
	cursor      int
	typed       []rune
	err         string
	mascot      splash.Mascot
	mode        splash.ColorMode
	start       time.Time
	result      chan choiceResult
	finished    bool
}

// askChoice shows the list in place of the screen until one option is picked
// or the question is cancelled. preferred, when it is one of the options, is
// where the cursor starts.
func (l *live) askChoice(ctx context.Context, title, what string, opts []option, preferred string) (string, bool) {
	enabled := 0
	for _, o := range opts {
		if !o.disabled {
			enabled++
		}
	}
	if enabled == 0 {
		return "", false
	}
	res := make(chan choiceResult, 1)
	c := &choice{title: title, what: what, opts: opts, result: res}
	for i, o := range opts {
		if !o.disabled && o.key == preferred {
			c.cursor = i
		}
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return "", false
	}
	c.start = l.now()
	if l.banner != nil {
		c.mascot, c.mode = l.banner.mascot, l.banner.mode
	} else if l.pal.On {
		c.mascot, c.mode = splash.NewMascot("", 1), splash.Color256
	} else {
		c.mascot = splash.NewMascot("", 1)
	}
	l.choice = c
	l.last = ""
	l.draw()
	l.mu.Unlock()

	select {
	case r := <-res:
		return r.value, r.ok
	case <-ctx.Done():
		l.mu.Lock()
		if l.choice == c {
			l.choice = nil
			l.last = ""
			l.draw()
		}
		l.mu.Unlock()
		return "", false
	}
}

func (l *live) choiceKey(k key) {
	c := l.choice
	if c.finished {
		return
	}
	finish := func(value string, ok bool) {
		c.finished = true
		l.choice = nil
		l.last = ""
		l.draw()
		c.result <- choiceResult{value: value, ok: ok}
	}
	switch k.Kind {
	case keyUp, keyScrollUp, keyMouseScrollUp:
		c.move(-1)
		c.typed, c.err = nil, ""
	case keyDown, keyScrollDown, keyMouseScrollDown:
		c.move(1)
		c.typed, c.err = nil, ""
	case keyEnter:
		if len(c.typed) > 0 {
			if picked := match(c.opts, string(c.typed)); picked != "" {
				finish(picked, true)
				return
			}
			c.err = "no " + c.what + " " + oneLine(string(c.typed)) + " here"
		} else if !c.opts[c.cursor].disabled {
			finish(c.opts[c.cursor].key, true)
			return
		}
	case keyBackspace:
		if len(c.typed) > 0 {
			c.typed = c.typed[:len(c.typed)-1]
			c.err = ""
			c.followTyped()
		}
	case keyRune:
		if unicode.IsPrint(k.R) && len(c.typed) < 40 {
			c.typed = append(c.typed, k.R)
			c.err = ""
			c.followTyped()
		}
	case keyInterrupt, keyEsc:
		finish("", false)
		return
	case keyRedraw:
		l.last = ""
	}
	l.draw()
}

func (c *choice) move(delta int) {
	n := len(c.opts)
	for i := 1; i <= n; i++ {
		at := (c.cursor + delta*i) % n
		if at < 0 {
			at += n
		}
		if !c.opts[at].disabled {
			c.cursor = at
			return
		}
	}
}

// followTyped moves the cursor to the option the typed text names, when it
// names exactly one.
func (c *choice) followTyped() {
	if picked := match(c.opts, string(c.typed)); picked != "" {
		for i, o := range c.opts {
			if o.key == picked {
				c.cursor = i
			}
		}
	}
}

// drawChoice paints the mascot saying the question, in place of the
// transcript and the box.
func (l *live) drawChoice() {
	c := l.choice
	cols, rows := max(l.cols, 1), max(l.rows, 1)
	screen := make([]string, rows)
	for i := range screen {
		screen[i] = ""
	}
	put := func(x, y int, s string) {
		if y < 0 || y >= rows || x >= cols {
			return
		}
		s = clipStyled(s, cols-x)
		screen[y] = strings.Repeat(" ", x) + s
	}

	var mascot []string
	mascotCols := splash.MascotCols
	if l.v.clawd {
		clawd := splash.ClawdRowsStrings(c.mode)
		mascot = clawd[:]
		mascotCols = splash.ClawdCols
	} else {
		m := c.mascot.Rows(l.now().Sub(c.start).Seconds(), c.mode)
		mascot = m[:]
	}
	mascotW := mascotCols + 1
	beside := cols - mascotW - 2
	stack := beside < 24
	speechW := cols
	if !stack {
		speechW = beside
	}
	speech := c.speech(l.pal, max(speechW, 8), l.v.clawd)
	pad := func(line string, w int) string {
		if d := w - textWidth(line); d > 0 {
			return line + strings.Repeat(" ", d)
		}
		return line
	}
	if stack {
		top := max((rows-len(mascot)-1-len(speech))/2, 0)
		for i, row := range mascot {
			put(max((cols-mascotCols)/2, 0), top+i, row)
		}
		for i, row := range speech {
			put(0, top+len(mascot)+1+i, row)
		}
	} else {
		block := max(len(mascot), len(speech))
		top := max((rows-block)/2, 0)
		for i := 0; i < block && top+i < rows; i++ {
			left, right := "", ""
			if i < len(mascot) {
				left = mascot[i]
			}
			if i < len(speech) {
				right = speech[i]
			}
			put(0, top+i, pad(left, mascotW)+"  "+right)
		}
	}
	hint := l.pal.Dim(clipWidth("↑↓ move · Enter take · type a name · Ctrl-C cancel", cols))
	put(0, rows-1, hint)

	var body strings.Builder
	body.WriteString(cursorHide)
	for i, row := range screen {
		body.WriteString(row)
		body.WriteString(clearLineEnd)
		if i < len(screen)-1 {
			body.WriteString("\r\n")
		}
	}
	rendered := body.String()
	if rendered == l.last {
		return
	}
	var out strings.Builder
	out.WriteString(syncStart)
	out.WriteString(cursorHome)
	out.WriteString(rendered)
	out.WriteString(syncEnd)
	io.WriteString(l.out, out.String())
	l.last = rendered
}

// speech is what the mascot says: the question, the options, and what has
// been typed.
func (c *choice) speech(pal palette, width int, clawd bool) []string {
	out := []string{pal.Bold(clipWidth(c.title, width)), ""}
	n := 0
	for i, o := range c.opts {
		mark := "  "
		if i == c.cursor {
			if clawd {
				mark = pal.SGR("1;"+colorCoral, "❯ ")
			} else {
				mark = pal.SGR("1;36", "> ")
			}
		}
		num := "  "
		if !o.disabled {
			n++
			num = fmt.Sprintf("%-2d", n)
		}
		line := mark + num + oneLine(o.key)
		if o.text != "" {
			line += "  " + pal.Dim(oneLine(o.text))
		}
		out = append(out, clipStyled(line, width))
	}
	typed := string(c.typed)
	if typed == "" {
		typed = pal.Dim("…")
	}
	out = append(out, "", clipStyled(pal.Dim(c.what+" > ")+typed, width))
	if c.err != "" {
		out = append(out, pal.SGR("31", clipWidth(c.err, width)))
	}
	return out
}
