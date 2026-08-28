package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

// statusLine writes to a terminal in two registers: permanent lines that scroll
// like normal output, and one transient line that is rewritten in place and
// then replaced by whatever permanent line comes next.
//
// When the output is not a terminal - piped into grep, redirected to a log -
// carriage returns would be noise, so transient updates are simply printed as
// ordinary lines instead.
type statusLine struct {
	mu      sync.Mutex
	w       io.Writer
	fd      uintptr // console handle, for querying the window width
	tty     bool
	lastLen int
	frame   int
}

// maxTransient keeps a transient line inside even a narrow console. A line that
// wraps cannot be erased by a single carriage return, which would leave torn
// fragments behind on every update.
const (
	// fallbackWidth is used when the console will not report its size.
	fallbackWidth = 80
	// widthCap keeps the line readable on a very wide window.
	widthCap = 160
	// minWidth is the narrowest line still worth composing.
	minWidth = 32
	// spinnerCells is the spinner plus the space after it. The renderer is
	// given a budget that already excludes these, or the line would overrun by
	// exactly two columns and lose its tail to truncation.
	spinnerCells = 2
)

// probeConsoleWidth is a variable so tests can pin a width without a console.
var probeConsoleWidth = consoleWidth

func newStatusLine(w *os.File) *statusLine {
	s := &statusLine{w: w, fd: w.Fd()}
	// A console reports itself as a character device; a file or pipe does not.
	if fi, err := w.Stat(); err == nil {
		s.tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return s
}

// Width is the number of columns a transient line may occupy.
func (s *statusLine) Width() int {
	if n := s.lineWidth() - spinnerCells; n >= minWidth {
		return n
	}
	return minWidth
}

// lineWidth is how many columns a transient line may fill. The last column is
// left alone: writing into it makes some consoles wrap to the next line, which
// a carriage return can no longer erase.
func (s *statusLine) lineWidth() int {
	w := probeConsoleWidth(s.fd)
	if w <= 0 {
		w = fallbackWidth
	}
	if w > widthCap {
		w = widthCap
	}
	if w -= 1; w < minWidth {
		w = minWidth
	}
	return w
}

// IsTerminal reports whether in-place updates are possible.
func (s *statusLine) IsTerminal() bool { return s != nil && s.tty }

// Update rewrites the transient line. The spinner advances on every call, so
// the display still shows life while a single very large row is being read.
func (s *statusLine) Update(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.tty {
		fmt.Fprintln(s.w, msg)
		return
	}

	msg = truncate(fmt.Sprintf("%c %s", spinner[s.frame%len(spinner)], msg), s.lineWidth())
	s.frame++
	pad := s.padding(msg)
	fmt.Fprintf(s.w, "\r%s%s", msg, pad)
	// What has to be covered next time is everything now on screen, message
	// plus padding - not just the message, or a shorter line would leave the
	// tail of a longer one behind.
	s.lastLen = utf8.RuneCountInString(msg) + len(pad)
}

// Print emits a permanent line, first wiping any transient line so the two
// never overlap.
func (s *statusLine) Print(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.tty {
		fmt.Fprintln(s.w, msg)
		return
	}
	fmt.Fprintf(s.w, "\r%s%s\n", msg, s.padding(msg))
	s.lastLen = 0
}

// padding returns the spaces needed to cover whatever the previous transient
// line left on screen. Spaces are used rather than an ANSI erase sequence
// because older Windows consoles do not process escape codes.
func (s *statusLine) padding(msg string) string {
	if n := s.lastLen - utf8.RuneCountInString(msg); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	e := []rune(ellipsis)
	if max <= len(e) {
		return string(r[:max])
	}
	return string(r[:max-len(e)]) + ellipsis
}
