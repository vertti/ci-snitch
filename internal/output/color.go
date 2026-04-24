package output

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// useColor reports whether ANSI color codes should be emitted to w.
// Honors the NO_COLOR standard (https://no-color.org): any non-empty NO_COLOR
// disables color. FORCE_COLOR overrides TTY detection for CI tooling that
// intentionally captures colored output. Otherwise, color is only enabled
// when w is a terminal.
func useColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd())
}

// disableColors zeroes the ANSI code package vars so all color-producing
// call sites render plain text. Safe because the CLI runs one formatter
// per invocation.
func disableColors() {
	bold = ""
	dim = ""
	red = ""
	green = ""
	yellow = ""
	cyan = ""
	reset = ""
}
