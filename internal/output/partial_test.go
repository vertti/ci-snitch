package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/analyze"
)

func partialTestResult() *analyze.AnalysisResult {
	result := richTestResult()
	result.Meta.Incomplete = []analyze.IncompleteWorkflow{
		{Workflow: "tests", Hydrated: 146, Listed: 854},
		{Workflow: "deploy", Hydrated: 61, Listed: 235},
	}
	return result
}

func TestFormatters_LeadWithThePartialDataNotice(t *testing.T) {
	pal := plainPalette()
	formatters := map[string]Formatter{
		"table":    TableFormatter{pal: &pal},
		"markdown": MarkdownFormatter{},
		"llm":      LLMFormatter{},
	}
	for name, f := range formatters {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, f.Format(&buf, partialTestResult()))

			at := strings.Index(buf.String(), "2 workflows are missing run details (tests: 146 of 854 runs fetched; deploy: 61 of 235 runs fetched)")
			require.NotEqual(t, -1, at, "no notice in:\n%s", buf.String())
			assert.Less(t, at, 200, "the notice must come before the findings")
		})
	}
}

func TestTableFormatter_PartialDataNoticeSurvivesAnEmptyResult(t *testing.T) {
	pal := plainPalette()
	result := &analyze.AnalysisResult{Meta: analyze.ResultMeta{
		Incomplete: []analyze.IncompleteWorkflow{{Workflow: "tests", Hydrated: 0, Listed: 854}},
	}}
	var buf bytes.Buffer
	require.NoError(t, TableFormatter{pal: &pal}.Format(&buf, result))
	assert.Contains(t, buf.String(), "1 workflow is missing run details (tests: 0 of 854 runs fetched)")
}

func TestPartialDataNotice_NamesTheFiveWorstAndCountsTheRest(t *testing.T) {
	var incomplete []analyze.IncompleteWorkflow
	for i := range 7 {
		incomplete = append(incomplete, analyze.IncompleteWorkflow{Workflow: fmt.Sprintf("wf%d", i), Hydrated: i, Listed: 10})
	}
	notice := partialDataNotice(incomplete)
	assert.Contains(t, notice, "7 workflows are missing run details")
	assert.Contains(t, notice, "wf4: 4 of 10 runs fetched; and 2 more)")
	assert.NotContains(t, notice, "wf5")

	assert.Empty(t, partialDataNotice(nil))
}

func TestJSONFormatter_ListsIncompleteWorkflowsOnlyWhenThereAreAny(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, JSONFormatter{}.Format(&buf, partialTestResult()))
	var got struct {
		Meta struct {
			Incomplete []analyze.IncompleteWorkflow `json:"incomplete"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, partialTestResult().Meta.Incomplete, got.Meta.Incomplete)

	buf.Reset()
	require.NoError(t, JSONFormatter{}.Format(&buf, richTestResult()))
	assert.NotContains(t, buf.String(), `"incomplete"`)
}
