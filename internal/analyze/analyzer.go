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

// Severity levels for findings.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Change point directions.
const (
	DirectionSlowdown = "slowdown"
	DirectionSpeedup  = "speedup"
)

// Finding represents a single analysis result.
type Finding struct {
	Type        string        `json:"type"`
	Severity    string        `json:"severity"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Detail      FindingDetail `json:"detail"`
}

// FindingDetail is implemented by typed detail structs for each analyzer.
type FindingDetail interface {
	DetailType() string
}
