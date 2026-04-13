// Package output provides formatters for analysis results.
package output

import (
	"io"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// Formatter writes analysis results to a writer.
type Formatter interface {
	Format(w io.Writer, result analyze.AnalysisResult) error
}

// Options controls formatter behavior.
type Options struct {
	Verbose bool
}

// Get returns a formatter by name. Supported: "json", "table", "markdown"/"md", "llm".
// Returns the formatter and true if the name was recognized, or the table
// formatter and false for unknown names.
func Get(name string, opts Options) (Formatter, bool) {
	switch name {
	case "json":
		return JSONFormatter{}, true
	case "markdown", "md":
		return MarkdownFormatter(opts), true
	case "llm":
		return LLMFormatter{}, true
	case "table":
		return TableFormatter(opts), true
	default:
		return TableFormatter(opts), false
	}
}
