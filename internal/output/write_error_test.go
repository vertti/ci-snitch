package output

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// failingWriter accepts okWrites writes and fails every one after that.
type failingWriter struct {
	okWrites int
	writes   int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > f.okWrites {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func testFormatters(t *testing.T) map[string]Formatter {
	t.Helper()
	pal := plainPalette()
	return map[string]Formatter{
		"table":               TableFormatter{pal: &pal},
		"markdown":            MarkdownFormatter{},
		"llm":                 LLMFormatter{},
		"llm with raw output": LLMFormatter{RawOutputPath: filepath.Join(t.TempDir(), "raw.json")},
		"json":                JSONFormatter{},
	}
}

func TestFormatters_ReportAFailedWrite(t *testing.T) {
	for name, f := range testFormatters(t) {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, f.Format(&failingWriter{}, richTestResult()), "disk full")
		})
	}
}

func TestFormatters_ReportAWriteThatFailsOnlyAtTheEnd(t *testing.T) {
	for name, f := range testFormatters(t) {
		t.Run(name, func(t *testing.T) {
			counter := &failingWriter{okWrites: 1 << 30}
			require.NoError(t, f.Format(counter, richTestResult()))

			lastWriteFails := &failingWriter{okWrites: counter.writes - 1}
			require.ErrorContains(t, f.Format(lastWriteFails, richTestResult()), "disk full")
		})
	}
}
