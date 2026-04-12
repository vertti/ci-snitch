package output

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/mattn/go-isatty"
)

// Progress writes status updates to stderr.
// On a TTY, it overwrites the current line. Otherwise, it prints each update on a new line.
// All methods are safe for concurrent use.
type Progress struct {
	mu    sync.Mutex
	w     io.Writer
	isTTY bool
	dirty bool // true if we have an in-place line that needs clearing
}

// NewProgress creates a progress writer for stderr.
func NewProgress() *Progress {
	return &Progress{
		w:     os.Stderr,
		isTTY: isatty.IsTerminal(os.Stderr.Fd()),
	}
}

// Status writes a transient status line that will be overwritten by the next call.
// On non-TTY, each status is printed on its own line.
func (p *Progress) Status(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if p.isTTY {
		_, _ = fmt.Fprintf(p.w, "\r\033[K%s", msg)
		p.dirty = true
	} else {
		_, _ = fmt.Fprintln(p.w, msg)
	}
}

// Done clears the current status line (on TTY) to make room for final output.
func (p *Progress) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isTTY && p.dirty {
		_, _ = fmt.Fprint(p.w, "\r\033[K")
		p.dirty = false
	}
}

// Log prints a permanent line (not overwritten).
// Clears any in-place status first.
func (p *Progress) Log(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isTTY && p.dirty {
		_, _ = fmt.Fprint(p.w, "\r\033[K")
		p.dirty = false
	}
	_, _ = fmt.Fprintf(p.w, format+"\n", args...)
}
