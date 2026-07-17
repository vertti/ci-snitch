package output

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// palette holds the ANSI codes the table formatter emits. A zero palette
// renders plain text. Each Format call derives its own palette, so no
// global state is mutated and formatters are safe to run in any order.
type palette struct {
	bold   string
	dim    string
	red    string
	green  string
	yellow string
	cyan   string
	reset  string
}

// colorPalette returns the palette with ANSI escape codes for colored output.
func colorPalette() palette {
	return palette{
		bold:   "\033[1m",
		dim:    "\033[2m",
		red:    "\033[31m",
		green:  "\033[32m",
		yellow: "\033[33m",
		cyan:   "\033[36m",
		reset:  "\033[0m",
	}
}

// plainPalette returns the stripped palette: every code is empty, so all
// color-producing call sites render plain text.
func plainPalette() palette {
	return palette{}
}

// paletteFor derives the palette for w: colored when useColor allows it,
// plain otherwise.
func paletteFor(w io.Writer) palette {
	if useColor(w) {
		return colorPalette()
	}
	return plainPalette()
}

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
