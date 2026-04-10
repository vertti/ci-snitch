// Package analyze provides the analysis engine and analyzer interface for CI performance analysis.
package analyze

import (
	"context"

	"github.com/vertti/ci-snitch/internal/model"
)

// Analyzer examines workflow run data and produces findings.
type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, ac *AnalysisContext) ([]Finding, error)
}

// AnalysisContext carries run data and lazily-computed derived views shared across analyzers.
type AnalysisContext struct {
	Details []model.RunDetail
}

// Finding represents a single analysis result.
type Finding struct {
	Type        string
	Severity    string // "info", "warning", "critical"
	Title       string
	Description string
	Detail      FindingDetail
}

// FindingDetail is implemented by typed detail structs for each analyzer.
type FindingDetail interface {
	DetailType() string
}
