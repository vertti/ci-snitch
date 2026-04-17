// Package diag provides a unified diagnostic type for non-fatal issues.
package diag

import "fmt"

// Severity indicates how important a diagnostic is.
type Severity string

const (
	Info  Severity = "info"
	Warn  Severity = "warn"
	Error Severity = "error"
)

// Kind categorizes the source of a diagnostic.
type Kind string

const (
	KindRateLimit   Kind = "rate_limit"
	KindPartialData Kind = "partial_data"
	KindCache       Kind = "cache"
	KindAuth        Kind = "auth"
	KindPreprocess  Kind = "preprocess"
	KindNetwork     Kind = "network"
	KindAnalyzer    Kind = "analyzer"
)

// Diagnostic represents a non-fatal issue encountered during any stage.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Kind     Kind     `json:"kind"`
	Scope    string   `json:"scope"`
	Message  string   `json:"message"`
	Err      error    `json:"-"`
}

func (d Diagnostic) String() string {
	if d.Scope != "" {
		return fmt.Sprintf("[%s] %s: %s", d.Severity, d.Scope, d.Message)
	}
	return fmt.Sprintf("[%s] %s", d.Severity, d.Message)
}

// New creates a Diagnostic with the given severity, kind, scope, and message.
func New(sev Severity, kind Kind, scope, msg string) Diagnostic {
	return Diagnostic{Severity: sev, Kind: kind, Scope: scope, Message: msg}
}

// Errorf creates an error-severity diagnostic wrapping an error.
func Errorf(kind Kind, scope string, err error, format string, args ...any) Diagnostic {
	return Diagnostic{
		Severity: Error,
		Kind:     kind,
		Scope:    scope,
		Message:  fmt.Sprintf(format, args...),
		Err:      err,
	}
}
