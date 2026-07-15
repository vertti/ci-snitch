package output

import (
	"encoding/json"
	"io"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/diag"
)

// JSONFormatter outputs results as indented JSON.
type JSONFormatter struct{}

// Format implements Formatter.
func (JSONFormatter) Format(w io.Writer, result *analyze.AnalysisResult) error {
	// Nil slices marshal to null, which breaks `jq '.diagnostics[]'` and
	// similar consumers; emit [] instead. Shallow copy — don't mutate input.
	out := *result
	if out.Diagnostics == nil {
		out.Diagnostics = []diag.Diagnostic{}
	}
	if out.Findings == nil {
		out.Findings = []analyze.Finding{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(&out)
}
