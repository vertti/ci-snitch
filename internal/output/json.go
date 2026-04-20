package output

import (
	"encoding/json"
	"io"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// JSONFormatter outputs results as indented JSON.
type JSONFormatter struct{}

// Format implements Formatter.
func (JSONFormatter) Format(w io.Writer, result *analyze.AnalysisResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
