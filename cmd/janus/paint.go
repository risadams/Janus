// paint.go colors what the commands print, the same way for each: only on
// a terminal that shows color, and never in JSON, a pipe, or a file.

package main

import (
	"io"
	"os"
	"runtime"
)

// style is a color, or a weight, for a piece of a line.
type style string

const (
	dim     style = "2"
	bold    style = "1"
	red     style = "31"
	green   style = "32"
	yellow  style = "33"
	magenta style = "35"
	cyan    style = "36"
)

// painter colors text for one output, or, when false, leaves it as it is.
// Text in columns is padded before it is painted, so that columns line up
// with color on or off; a painted cell inside a tabwriter table would be
// measured with its escape codes, so tables paint only their last cell.
type painter bool

// newPainter colors for w only when w is a terminal that wants color.
// stdout and stderr are decided separately: either can be a terminal while
// the other is redirected.
func newPainter(w io.Writer) painter {
	f, ok := w.(*os.File)
	return painter(ok && wantColor(f, os.Getenv))
}

func (p painter) paint(s style, text string) string {
	if !p || text == "" {
		return text
	}
	return "\x1b[" + string(s) + "m" + text + "\x1b[0m"
}

// level is the color for a check's or an outcome's result: green for ok,
// yellow for a warning, red for a failure.
func (p painter) level(name, text string) string {
	switch name {
	case "ok":
		return p.paint(green, text)
	case "warn":
		return p.paint(yellow, text)
	case "fail":
		return p.paint(red, text)
	}
	return text
}

// used colors a share of a usage limit by how near the limit it is:
// yellow from 75%, red from 90%.
func (p painter) used(utilization float64, text string) string {
	switch {
	case utilization >= 0.9:
		return p.paint(red, text)
	case utilization >= 0.75:
		return p.paint(yellow, text)
	}
	return text
}

// wantColor reports whether output to f should be in color: only on a
// terminal, never with NO_COLOR set (https://no-color.org), always with
// FORCE_COLOR set. On Windows, only in a terminal known to understand
// color codes, as the old console shows them as text.
func wantColor(f *os.File, getenv func(string) string) bool {
	switch {
	case getenv("NO_COLOR") != "":
		return false
	case getenv("FORCE_COLOR") != "":
		return true
	case getenv("TERM") == "dumb":
		return false
	case runtime.GOOS == "windows" && getenv("WT_SESSION") == "" && getenv("TERM") == "":
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
