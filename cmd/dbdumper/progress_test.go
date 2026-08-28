package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/JeePeeTee/dbdumper/internal/export"
)

func TestRenderProgress(t *testing.T) {
	cases := []struct {
		name string
		p    export.Progress
		want []string // substrings that must appear
		deny []string
	}{
		{
			name: "with an estimate, shows a bar, a percentage and an ETA",
			p: export.Progress{
				Table: "dbo.Document", TableIndex: 142, TableCount: 403,
				Rows: 1536, EstimatedRows: 5074, Bytes: 404 << 20, Elapsed: 170 * time.Second,
			},
			// 1536/5074 = 30.3% -> 5 of 16 cells
			want: []string{"[142/403]", "dbo.Document", "[#####-----------]", " 30%", "1536/5074", "ETA "},
		},
		{
			name: "without an estimate, falls back to a raw count and elapsed time",
			p: export.Progress{
				Table: "dbo.Thing", TableIndex: 3, TableCount: 9,
				Rows: 250, Bytes: 1024, Elapsed: 4 * time.Second,
			},
			want: []string{"[3/9]", "dbo.Thing", "250 rows", "4s"},
			deny: []string{"#", "ETA", "%"},
		},
		{
			name: "a stale estimate cannot push the bar past full",
			p: export.Progress{
				Table: "dbo.Growing", TableIndex: 1, TableCount: 1,
				Rows: 9000, EstimatedRows: 5000, Bytes: 10, Elapsed: 10 * time.Second,
			},
			want: []string{"[################]", "100%"},
			deny: []string{"-]"},
		},
	}

	for _, c := range cases {
		got := renderProgress(c.p, 78)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q missing from %q", c.name, w, got)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(got, d) {
				t.Errorf("%s: %q should not appear in %q", c.name, d, got)
			}
		}
	}
}

// TestRenderProgressFitsWidth - a transient line that wraps cannot be erased by
// a carriage return, so the renderer must never exceed the budget, however long
// the table name is.
func TestRenderProgressFitsWidth(t *testing.T) {
	long := "dbo.SomeVeryLongCompositeAssociationTableNameThatGoesOnAndOn"
	for _, width := range []int{40, 60, 78, 120} {
		for _, p := range []export.Progress{
			{Table: long, TableIndex: 142, TableCount: 403, Rows: 1234567, EstimatedRows: 9876543, Bytes: 1 << 40, Elapsed: 3 * time.Hour},
			{Table: long, TableIndex: 1, TableCount: 1, Rows: 1, Bytes: 1, Elapsed: time.Second},
		} {
			got := renderProgress(p, width)
			if n := utf8.RuneCountInString(got); n > width {
				t.Errorf("width %d: line is %d chars: %q", width, n, got)
			}
		}
	}
}

func TestClipKeepsTheTail(t *testing.T) {
	if got, want := clip("dbo.VeryLongTableName", 12), "...TableName"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := clip("dbo.Short", 20); got != "dbo.Short" {
		t.Errorf("short names should be untouched, got %q", got)
	}
}

func TestCompactDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                  "0s",
		45 * time.Second:                   "45s",
		90 * time.Second:                   "1m30s",
		2*time.Hour + 5*time.Minute:        "2h05m",
		25*time.Hour + 61*time.Second + 30: "25h01m",
	}
	for d, want := range cases {
		if got := compactDuration(d); got != want {
			t.Errorf("compactDuration(%v) = %q, want %q", d, got, want)
		}
		if len(compactDuration(d)) > 6 {
			t.Errorf("compactDuration(%v) = %q exceeds six characters", d, compactDuration(d))
		}
	}
}

func TestETA(t *testing.T) {
	// Half done after 10s implies about 10s left.
	p := export.Progress{Rows: 500, EstimatedRows: 1000, Elapsed: 10 * time.Second}
	eta, ok := p.ETA()
	if !ok || eta != 10*time.Second {
		t.Errorf("got %v (%v), want 10s", eta, ok)
	}
	// No estimate, no ETA.
	if _, ok := (export.Progress{Rows: 5, Elapsed: time.Minute}).ETA(); ok {
		t.Error("an ETA without an estimate is a guess dressed up as a fact")
	}
	// Too early to extrapolate.
	if _, ok := (export.Progress{Rows: 2, EstimatedRows: 1e9, Elapsed: 10 * time.Millisecond}).ETA(); ok {
		t.Error("should not extrapolate from a fraction of a second")
	}
}

// TestStatusLineOverwrites is the behaviour that cannot be eyeballed from a
// test run: each transient update returns to column zero and pads over the
// previous, longer line, and a permanent line wipes the transient one.
func TestStatusLineOverwrites(t *testing.T) {
	var buf bytes.Buffer
	s := &statusLine{w: &buf, tty: true}

	s.Update("a long transient line")
	s.Update("short")
	s.Print("dbo.Thing   42 rows")

	got := buf.String()
	parts := strings.Split(got, "\r")
	if len(parts) != 4 || parts[0] != "" {
		t.Fatalf("expected three carriage-returned writes, got %q", got)
	}

	// The spinner prefix makes each transient line two characters longer.
	if !strings.HasSuffix(parts[1], "a long transient line") {
		t.Errorf("first update mangled: %q", parts[1])
	}
	if !strings.Contains(parts[2], "short") {
		t.Errorf("second update mangled: %q", parts[2])
	}
	if len(parts[2]) != len(parts[1]) {
		t.Errorf("shorter line not padded to cover the longer one: %d vs %d (%q)",
			len(parts[2]), len(parts[1]), parts[2])
	}
	if !strings.HasPrefix(parts[3], "dbo.Thing   42 rows") || !strings.HasSuffix(parts[3], "\n") {
		t.Errorf("final line should overwrite and terminate: %q", parts[3])
	}
	if len(strings.TrimRight(parts[3], "\n")) != len(parts[2]) {
		t.Errorf("final line not padded over the transient one: %q", parts[3])
	}
}

// TestStatusLineNonTerminal - piping into a file or grep must produce ordinary
// lines with no control characters.
func TestStatusLineNonTerminal(t *testing.T) {
	var buf bytes.Buffer
	s := &statusLine{w: &buf, tty: false}
	s.Update("progress")
	s.Print("done")

	if got := buf.String(); got != "progress\ndone\n" {
		t.Errorf("got %q, want plain lines", got)
	}
}

func TestSpinnerAdvances(t *testing.T) {
	var buf bytes.Buffer
	s := &statusLine{w: &buf, tty: true}
	seen := map[rune]bool{}
	for i := 0; i < len(spinner); i++ {
		buf.Reset()
		s.Update("x")
		seen[[]rune(strings.TrimPrefix(buf.String(), "\r"))[0]] = true
	}
	if len(seen) != len(spinner) {
		t.Errorf("spinner should cycle through all %d frames, saw %d", len(spinner), len(seen))
	}
}

// TestUnicodeGlyphsKeepTheSameWidth - the bar switches to block-drawing
// characters on a console that can render them. Those are multi-byte in UTF-8,
// so every width calculation has to count runes, not bytes, or the line wraps
// and the carriage-return erase breaks.
func TestUnicodeGlyphsKeepTheSameWidth(t *testing.T) {
	asciiFill, asciiEmpty, asciiSpinner := barFill, barEmpty, spinner
	t.Cleanup(func() { barFill, barEmpty, spinner = asciiFill, asciiEmpty, asciiSpinner })

	p := export.Progress{
		Table: "dbo.Document", TableIndex: 142, TableCount: 403,
		Rows: 1536, EstimatedRows: 5074, Bytes: 404 << 20, Elapsed: 170 * time.Second,
	}
	ascii := renderProgress(p, 78)

	useUnicodeGlyphs()
	unicode := renderProgress(p, 78)

	if !strings.Contains(unicode, "█") || !strings.Contains(unicode, "░") {
		t.Errorf("expected block glyphs, got %q", unicode)
	}
	if a, u := utf8.RuneCountInString(ascii), utf8.RuneCountInString(unicode); a != u {
		t.Errorf("glyph choice changed the column width: ascii %d, unicode %d\n  %s\n  %s", a, u, ascii, unicode)
	}
	if len(unicode) <= utf8.RuneCountInString(unicode) {
		t.Error("test is not exercising multi-byte characters")
	}
	for _, w := range []int{40, 60, 78} {
		if n := utf8.RuneCountInString(renderProgress(p, w)); n > w {
			t.Errorf("unicode line exceeds width %d: %d chars", w, n)
		}
	}
}

// TestStatusLinePadsByRunes - padding a shorter line over a longer one has to
// be measured in columns, which for the transient line means runes.
func TestStatusLinePadsByRunes(t *testing.T) {
	var buf bytes.Buffer
	s := &statusLine{w: &buf, tty: true}
	s.Update("██████████") // 10 runes, 30 bytes
	buf.Reset()
	s.Update("ab")

	got := strings.TrimPrefix(buf.String(), "\r")
	// two spinner chars + "ab" = 4 runes, padded to cover the previous 12
	if n := utf8.RuneCountInString(got); n != 12 {
		t.Errorf("padded to %d runes, want 12: %q", n, got)
	}
}

// TestTransientLineFitsWithSpinner is the regression test for a line whose
// tail - the ETA, the part you actually watch - was being truncated away. The
// renderer was handed the full console width and the spinner was then prepended
// on top of it, overrunning by exactly the two cells the spinner occupies.
func TestTransientLineFitsWithSpinner(t *testing.T) {
	realProbe := probeConsoleWidth
	t.Cleanup(func() { probeConsoleWidth = realProbe })

	p := export.Progress{
		Table: "dbo.AuditEntryDetail", TableIndex: 22, TableCount: 413,
		Rows: 56100, EstimatedRows: 262000, Bytes: 20 << 20, Elapsed: 30 * time.Second,
	}

	for _, consoleCols := range []int{40, 80, 100, 150, 200} {
		probeConsoleWidth = func(uintptr) int { return consoleCols }

		var buf bytes.Buffer
		s := &statusLine{w: &buf, tty: true}
		s.Update("%s", renderProgress(p, s.Width()))

		line := strings.TrimPrefix(buf.String(), "\r")
		if n := utf8.RuneCountInString(line); n > consoleCols-1 {
			t.Errorf("console %d: line occupies %d columns: %q", consoleCols, n, line)
		}
		if strings.Contains(line, ellipsis) {
			t.Errorf("console %d: line was truncated, losing its tail: %q", consoleCols, line)
		}
		// The ETA is the reason the line exists; it must survive.
		if !strings.Contains(line, "ETA") {
			t.Errorf("console %d: ETA missing from %q", consoleCols, line)
		}
	}
}

// TestWidthReservesSpinnerCells pins the arithmetic that went wrong.
func TestWidthReservesSpinnerCells(t *testing.T) {
	realProbe := probeConsoleWidth
	t.Cleanup(func() { probeConsoleWidth = realProbe })

	probeConsoleWidth = func(uintptr) int { return 120 }
	s := &statusLine{tty: true}
	if got, want := s.lineWidth(), 119; got != want { // one column left free
		t.Errorf("lineWidth = %d, want %d", got, want)
	}
	if got, want := s.Width(), 117; got != want { // minus the spinner and its space
		t.Errorf("Width = %d, want %d", got, want)
	}

	// A console that will not report its size falls back rather than degenerating.
	probeConsoleWidth = func(uintptr) int { return 0 }
	if got := s.lineWidth(); got != fallbackWidth-1 {
		t.Errorf("fallback lineWidth = %d, want %d", got, fallbackWidth-1)
	}
	// An absurdly wide window is capped, and a tiny one floored.
	probeConsoleWidth = func(uintptr) int { return 5000 }
	if got := s.lineWidth(); got != widthCap-1 {
		t.Errorf("capped lineWidth = %d, want %d", got, widthCap-1)
	}
	probeConsoleWidth = func(uintptr) int { return 10 }
	if got := s.lineWidth(); got != minWidth {
		t.Errorf("floored lineWidth = %d, want %d", got, minWidth)
	}
}
