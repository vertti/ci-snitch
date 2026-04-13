package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

type mockAnalyzer struct {
	name     string
	findings []Finding
	err      error
}

func (m mockAnalyzer) Name() string { return m.name }
func (m mockAnalyzer) Analyze(_ context.Context, _ *AnalysisContext) ([]Finding, error) {
	return m.findings, m.err
}

func TestEngine_CollectsFindings(t *testing.T) {
	a1 := mockAnalyzer{
		name:     "a1",
		findings: []Finding{{Type: "test", Title: "finding 1"}},
	}
	a2 := mockAnalyzer{
		name:     "a2",
		findings: []Finding{{Type: "test", Title: "finding 2"}, {Type: "test", Title: "finding 3"}},
	}

	engine := NewEngine(a1, a2)
	result := engine.Run(context.Background(), nil, nil, nil, nil)

	assert.Len(t, result.Findings, 3)
	assert.Empty(t, result.Warnings)
}

func TestEngine_AnalyzerError_BecomesWarning(t *testing.T) {
	good := mockAnalyzer{
		name:     "good",
		findings: []Finding{{Type: "test", Title: "ok"}},
	}
	bad := mockAnalyzer{
		name: "bad",
		err:  errors.New("something broke"),
	}

	engine := NewEngine(good, bad)
	result := engine.Run(context.Background(), nil, nil, nil, nil)

	assert.Len(t, result.Findings, 1)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0].Message, "bad")
	assert.Contains(t, result.Warnings[0].Message, "something broke")
}

func TestEngine_Meta(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := []model.RunDetail{
		{Run: model.WorkflowRun{WorkflowID: 1, CreatedAt: base}},
		{Run: model.WorkflowRun{WorkflowID: 1, CreatedAt: base.Add(24 * time.Hour)}},
		{Run: model.WorkflowRun{WorkflowID: 2, CreatedAt: base.Add(48 * time.Hour)}},
	}

	engine := NewEngine()
	result := engine.Run(context.Background(), details, nil, nil, nil)

	assert.Equal(t, 3, result.Meta.TotalRuns)
	assert.Equal(t, base, result.Meta.TimeRange[0])
	assert.Equal(t, base.Add(48*time.Hour), result.Meta.TimeRange[1])
	assert.Len(t, result.Meta.WorkflowIDs, 2)
}
