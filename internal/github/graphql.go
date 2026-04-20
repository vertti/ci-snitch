package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/model"
)

// graphqlBatchSize is the max number of runs fetched per GraphQL query.
// Each run with jobs+steps costs ~1-4 rate limit points.
const graphqlBatchSize = 20

// graphqlMaxJobs is the max jobs fetched per run in a single query.
const graphqlMaxJobs = 50

// graphqlMaxSteps is the max steps fetched per job in a single query.
const graphqlMaxSteps = 50

// FetchRunDetailsGraphQL hydrates runs with jobs+steps using batched GraphQL queries.
// Falls back to REST for runs whose node_id is empty.
func (c *Client) FetchRunDetailsGraphQL(ctx context.Context, runs []model.WorkflowRun) (details []model.RunDetail, warnings []Warning) {
	// Separate runs with and without node IDs
	var graphqlRuns, restRuns []model.WorkflowRun
	for i := range runs {
		if runs[i].NodeID != "" {
			graphqlRuns = append(graphqlRuns, runs[i])
		} else {
			restRuns = append(restRuns, runs[i])
		}
	}

	// Batch GraphQL fetches
	for start := 0; start < len(graphqlRuns); start += graphqlBatchSize {
		end := min(start+graphqlBatchSize, len(graphqlRuns))
		batch := graphqlRuns[start:end]

		batchDetails, batchWarnings := c.fetchBatchGraphQL(ctx, batch)
		details = append(details, batchDetails...)
		warnings = append(warnings, batchWarnings...)
	}

	// Fall back to REST for runs without node IDs
	if len(restRuns) > 0 {
		restDetails, restWarnings := c.FetchRunDetails(ctx, restRuns)
		details = append(details, restDetails...)
		warnings = append(warnings, restWarnings...)
	}

	return details, warnings
}

func (c *Client) fetchBatchGraphQL(ctx context.Context, runs []model.WorkflowRun) (details []model.RunDetail, warnings []Warning) {
	query := buildBatchQuery(runs)

	raw, err := c.doGraphQL(ctx, query)
	if err != nil {
		c.log("GraphQL batch failed, falling back to REST", "error", err, "batch_size", len(runs))
		return c.FetchRunDetails(ctx, runs)
	}

	return parseBatchResponse(raw, runs)
}

const graphqlEndpoint = "https://api.github.com/graphql"

func (c *Client) doGraphQL(ctx context.Context, query string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.gh.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on read path

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read graphql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql: HTTP %d: %s", resp.StatusCode, truncateBody(respBody))
	}

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("graphql: parse response: %w", err)
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}

	return result.Data, nil
}

func truncateBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func buildBatchQuery(runs []model.WorkflowRun) string {
	var b strings.Builder
	b.WriteString("query{")

	fragment := fmt.Sprintf(`...on WorkflowRun{databaseId checkSuite{checkRuns(first:%d){nodes{name databaseId startedAt completedAt status conclusion steps(first:%d){nodes{name number startedAt completedAt status conclusion}}}}}}`,
		graphqlMaxJobs, graphqlMaxSteps)

	for i := range runs {
		fmt.Fprintf(&b, "r%d:node(id:%q){%s}", i, runs[i].NodeID, fragment)
	}

	b.WriteString("}")
	return b.String()
}

// graphqlRunResponse is the structure of each aliased node in the batch response.
type graphqlRunResponse struct {
	DatabaseID int64 `json:"databaseId"`
	CheckSuite *struct {
		CheckRuns struct {
			Nodes []graphqlCheckRun `json:"nodes"`
		} `json:"checkRuns"`
	} `json:"checkSuite"`
}

type graphqlCheckRun struct {
	Name        string  `json:"name"`
	DatabaseID  int64   `json:"databaseId"`
	StartedAt   *string `json:"startedAt"`
	CompletedAt *string `json:"completedAt"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	Steps       struct {
		Nodes []graphqlStep `json:"nodes"`
	} `json:"steps"`
}

type graphqlStep struct {
	Name        string  `json:"name"`
	Number      int     `json:"number"`
	StartedAt   *string `json:"startedAt"`
	CompletedAt *string `json:"completedAt"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
}

func parseBatchResponse(raw json.RawMessage, runs []model.WorkflowRun) (details []model.RunDetail, warnings []Warning) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, nil
	}

	// Build a lookup from databaseId to the original run (for metadata)
	runByID := make(map[int64]model.WorkflowRun, len(runs))
	for i := range runs {
		runByID[runs[i].ID] = runs[i]
	}

	for i := range runs {
		key := fmt.Sprintf("r%d", i)
		nodeRaw, ok := response[key]
		if !ok {
			continue
		}

		var node graphqlRunResponse
		if err := json.Unmarshal(nodeRaw, &node); err != nil || node.CheckSuite == nil {
			warnings = append(warnings, newGraphQLWarning(runs[i].ID, "failed to parse GraphQL response"))
			continue
		}

		jobs := convertGraphQLJobs(node.CheckSuite.CheckRuns.Nodes, runs[i].ID)
		details = append(details, model.RunDetail{Run: runs[i], Jobs: jobs})
	}

	return details, warnings
}

func convertGraphQLJobs(checkRuns []graphqlCheckRun, runID int64) []model.Job {
	jobs := make([]model.Job, 0, len(checkRuns))
	for i := range checkRuns {
		cr := &checkRuns[i]
		job := model.Job{
			ID:          cr.DatabaseID,
			RunID:       runID,
			Name:        cr.Name,
			Status:      strings.ToLower(cr.Status),
			Conclusion:  graphqlConclusion(cr.Conclusion),
			StartedAt:   parseGraphQLTime(cr.StartedAt),
			CompletedAt: parseGraphQLTime(cr.CompletedAt),
			// Runner info not available via GraphQL — left as zero values
		}

		for j := range cr.Steps.Nodes {
			st := &cr.Steps.Nodes[j]
			job.Steps = append(job.Steps, model.Step{
				Name:        st.Name,
				Number:      st.Number,
				Status:      strings.ToLower(st.Status),
				Conclusion:  graphqlConclusion(st.Conclusion),
				StartedAt:   parseGraphQLTime(st.StartedAt),
				CompletedAt: parseGraphQLTime(st.CompletedAt),
			})
		}

		jobs = append(jobs, job)
	}
	return jobs
}

func graphqlConclusion(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToLower(*s)
}

func parseGraphQLTime(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func newGraphQLWarning(runID int64, msg string) Warning {
	return Warning{
		Severity: "warn",
		Kind:     "network",
		Scope:    fmt.Sprintf("run-%d", runID),
		Message:  fmt.Sprintf("run %d: %s", runID, msg),
	}
}
