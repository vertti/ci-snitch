package output

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite golden files")

// checkGolden compares got against testdata/<name>, or rewrites the file
// when -update is set. Golden fixtures pin the exact rendered output —
// alignment, ordering, and (for the colored table) ANSI codes.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(path, got, 0o644)) //nolint:gosec // test fixture
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // test fixture path
	require.NoError(t, err, "missing golden file %s — run: go test ./internal/output -update", path)
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (regenerate with: go test ./internal/output -update)\n%s\n--- got (full) ---\n%s",
			path, firstLineDiff(string(want), string(got)), got)
	}
}

// firstLineDiff locates the first differing line so the failure points at the
// regression instead of dumping two full renders side by side.
func firstLineDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return fmt.Sprintf("first difference at line %d:\n  want: %q\n  got:  %q", i+1, w, g)
		}
	}
	return "contents differ (no line-level difference found)"
}

func TestGolden_TablePlain(t *testing.T) {
	pal := plainPalette()
	var buf bytes.Buffer
	require.NoError(t, TableFormatter{pal: &pal}.Format(&buf, richTestResult()))
	checkGolden(t, "table_plain.golden", buf.Bytes())
}

func TestGolden_TableColored(t *testing.T) {
	// Explicit colored palette: exercises the ANSI-code path without
	// depending on TTY detection or FORCE_COLOR.
	pal := colorPalette()
	var buf bytes.Buffer
	require.NoError(t, TableFormatter{pal: &pal}.Format(&buf, richTestResult()))
	checkGolden(t, "table_colored.golden", buf.Bytes())
}

func TestGolden_Markdown(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, MarkdownFormatter{}.Format(&buf, richTestResult()))
	checkGolden(t, "markdown.golden", buf.Bytes())
}

func TestGolden_LLM(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, LLMFormatter{}.Format(&buf, richTestResult()))
	checkGolden(t, "llm.golden", buf.Bytes())
}
