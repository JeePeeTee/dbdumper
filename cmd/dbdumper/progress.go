package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/export"
)

// barWidth is the number of cells in the progress bar.
const barWidth = 16

// barFill and barEmpty are chosen once at startup: solid blocks when the
// console can render them, ASCII when it cannot. See enableUnicodeOutput.
var (
	barFill  = "#"
	barEmpty = "-"
	ellipsis = "..."
	spinner  = []rune{'|', '/', '-', '\\'}
)

// useUnicodeGlyphs switches the bar and spinner to box-drawing characters.
// Called only once the console has been confirmed able to render them.
func useUnicodeGlyphs() {
	barFill, barEmpty = "█", "░" // FULL BLOCK, LIGHT SHADE
	ellipsis = "…"
	spinner = []rune{'⠋', '⠙', '⠹', '⠸'}
}

// minName is the smallest table-name fragment still worth showing.
const minName = 8

// renderProgress lays out one transient status line.

func renderProgress(p export.Progress, width int) string {
	// The line degrades in a fixed order as the terminal narrows: first the
	// bar goes, then the byte count, then the position counter. The table name
	// and how far along it is are the parts always worth keeping.
	for _, opt := range []struct{ bar, bytes, counter bool }{
		{true, true, true},
		{false, true, true},
		{false, false, true},
		{false, false, false},
	} {
		line := composeProgress(p, width, opt.bar, opt.bytes, opt.counter)
		if len([]rune(line)) <= width {
			return line
		}
	}
	// Nothing fits; show as much of the name as there is room for.
	return clip(p.Table, width)
}

func composeProgress(p export.Progress, width int, withBar, withBytes, withCounter bool) string {
	var head strings.Builder
	if withCounter && p.TableCount > 0 {
		fmt.Fprintf(&head, "[%d/%d] ", p.TableIndex, p.TableCount)
	}
	tail := progressTail(p, withBar, withBytes)

	// Everything except the table name has a known width, so the name is what
	// gives way first.
	nameBudget := width - len([]rune(head.String())) - len([]rune(tail))
	if nameBudget < minName {
		nameBudget = minName
	}
	return head.String() + clip(p.Table, nameBudget) + tail
}

func progressTail(p export.Progress, withBar, withBytes bool) string {
	var b strings.Builder

	if f, ok := p.Fraction(); ok {
		if withBar {
			filled := int(f*barWidth + 0.5)
			fmt.Fprintf(&b, " [%s%s]",
				strings.Repeat(barFill, filled), strings.Repeat(barEmpty, barWidth-filled))
		}
		fmt.Fprintf(&b, " %3.0f%%", f*100)
	}

	if p.EstimatedRows > 0 {
		fmt.Fprintf(&b, " %s/%s", compactCount(p.Rows), compactCount(p.EstimatedRows))
	} else {
		fmt.Fprintf(&b, " %s rows", compactCount(p.Rows))
	}
	if withBytes {
		fmt.Fprintf(&b, " %s", humanBytes(p.Bytes))
	}

	if eta, ok := p.ETA(); ok {
		fmt.Fprintf(&b, " ETA %s", compactDuration(eta))
	} else {
		fmt.Fprintf(&b, " %s", compactDuration(p.Elapsed.Round(time.Second)))
	}
	return b.String()
}

// compactCount abbreviates large row counts so the line width stays stable:
// 1234 -> 1.2k, 5100000 -> 5.1M.
func compactCount(n int64) string {
	switch {
	case n < 10_000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// compactDuration prints a duration in the largest two units that matter, so
// it never grows past six characters.
func compactDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// clip shortens a string from the left, keeping the tail, because the
// distinguishing part of "dbo.SomethingSomethingLong" is at the end.
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[len(r)-max:])
	}
	return "..." + string(r[len(r)-(max-3):])
}
