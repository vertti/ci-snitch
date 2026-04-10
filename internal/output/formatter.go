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

// Get returns a formatter by name. Supported: "json", "table", "markdown".
func Get(name string) Formatter {
	switch name {
	case "json":
		return JSONFormatter{}
	case "markdown", "md":
		return MarkdownFormatter{}
	default:
		return TableFormatter{}
	}
}
