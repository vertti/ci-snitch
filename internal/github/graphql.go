package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/diag"
	"github.com/vertti/ci-snitch/internal/model"
)

// GraphQLBatchSize is the max number of runs fetched per GraphQL query.
// Each run with jobs+steps costs ~1-4 rate limit points.
const GraphQLBatchSize = 20

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
	for start := 0; start < len(graphqlRuns); start += GraphQLBatchSize {
		end := min(start+GraphQLBatchSize, len(graphqlRuns))
		batch := graphqlRuns[start:end]

		batchDetails, batchWarnings, err := c.fetchBatchGraphQL(ctx, batch)
		details = append(details, batchDetails...)
		warnings = append(warnings, batchWarnings...)
		if errors.Is(err, errGraphQLRateLimited) {
			// Falling back to REST here would burn ~20x the core budget the
			// pre-flight check approved. Skip the remaining hydration instead.
			skipped := len(graphqlRuns) - start
			warnings = append(warnings, diag.New(
				diag.Warn, diag.KindRateLimit, "graphql",
				fmt.Sprintf("GraphQL rate limit exhausted; skipped hydration of %d runs (not falling back to REST to protect the core pool)", skipped),
			))
			break
		}
		if ctx.Err() != nil {
			// Cancellation is not a data problem: no warnings, no fallback —
			// the caller observes ctx.Err() and aborts the run.
			return details, warnings
		}
	}

	// Fall back to REST for runs without node IDs
	if len(restRuns) > 0 {
		restDetails, restWarnings := c.FetchRunDetails(ctx, restRuns)
		details = append(details, restDetails...)
		warnings = append(warnings, restWarnings...)
	}

	details, warnings = c.completeTruncated(ctx, details, warnings)

	return details, warnings
}

// completeTruncated refetches runs whose jobs or steps exceeded the GraphQL
// per-query limits via REST, which paginates jobs fully and embeds complete
// steps. Rare (>50 jobs or >50 steps per job) and bounded to one REST fetch
// per affected run — and the completed result is cacheable, so it beats
// re-fetching the truncated run on every future scan.
func (c *Client) completeTruncated(ctx context.Context, details []model.RunDetail, warnings []Warning) (outDetails []model.RunDetail, outWarnings []Warning) {
	var truncatedRuns []model.WorkflowRun
	for i := range details {
		if details[i].Truncated {
			truncatedRuns = append(truncatedRuns, details[i].Run)
		}
	}
	if len(truncatedRuns) == 0 {
		return details, warnings
	}
	outDetails, outWarnings = details, warnings

	restDetails, restWarnings := c.FetchRunDetails(ctx, truncatedRuns)
	outWarnings = append(outWarnings, restWarnings...)
	complete := make(map[int64]model.RunDetail, len(restDetails))
	for i := range restDetails {
		complete[restDetails[i].Run.ID] = restDetails[i]
	}

	completed, stillTruncated := 0, 0
	for i := range outDetails {
		if !outDetails[i].Truncated {
			continue
		}
		if full, ok := complete[outDetails[i].Run.ID]; ok {
			outDetails[i] = full
			completed++
		} else {
			stillTruncated++ // REST failed for this run; keep it uncacheable
		}
	}
	if completed > 0 {
		outWarnings = append(outWarnings, diag.New(
			diag.Info, diag.KindPartialData, "graphql",
			fmt.Sprintf("%d runs exceeded the GraphQL page size (%d jobs or %d steps per query); fetched completely via REST",
				completed, graphqlMaxJobs, graphqlMaxSteps),
		))
	}
	if stillTruncated > 0 {
		outWarnings = append(outWarnings, diag.New(
			diag.Warn, diag.KindPartialData, "graphql",
			fmt.Sprintf("%d runs exceed the GraphQL page size and could not be completed via REST; analyzed with partial data, not cached", stillTruncated),
		))
	}
	return outDetails, outWarnings
}

// fetchBatchGraphQL hydrates one batch. A non-nil error is returned only for
// rate limiting, which the caller must handle by stopping — other failures
// fall back to REST internally.
func (c *Client) fetchBatchGraphQL(ctx context.Context, runs []model.WorkflowRun) (details []model.RunDetail, warnings []Warning, fatal error) {
	query := buildBatchQuery(runs)

	raw, err := c.doGraphQL(ctx, query)
	if err != nil {
		if errors.Is(err, errGraphQLRateLimited) || ctx.Err() != nil {
			return nil, nil, err
		}
		c.log("GraphQL batch failed, falling back to REST", "error", err, "batch_size", len(runs))
		details, warnings = c.FetchRunDetails(ctx, runs)
		return details, warnings, nil
	}

	details, missed, warnings := parseBatchResponse(raw, runs)
	if len(missed) > 0 {
		warnings = append(warnings, diag.New(
			diag.Warn, diag.KindPartialData, "graphql",
			fmt.Sprintf("%d of %d runs missing from GraphQL batch response, fetching them via REST",
				len(missed), len(runs)),
		))
		restDetails, restWarnings := c.FetchRunDetails(ctx, missed)
		details = append(details, restDetails...)
		warnings = append(warnings, restWarnings...)
	}
	return details, warnings, nil
}

const graphqlEndpoint = "https://api.github.com/graphql"

func (c *Client) doGraphQL(ctx context.Context, query string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.gh.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on read path

	// Error pages (e.g. from a misconfigured proxy) can be arbitrarily large;
	// only the first 200 bytes end up in the error string anyway.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("graphql: HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	// Successful batch responses are large but bounded by the query shape;
	// the limit is a guard against pathological payloads, not a budget.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read graphql response: %w", err)
	}

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("graphql: parse response: %w", err)
	}
	if len(result.Errors) > 0 {
		if result.Errors[0].Type == "RATE_LIMITED" {
			return nil, fmt.Errorf("graphql: %s: %w", result.Errors[0].Message, errGraphQLRateLimited)
		}
		return nil, fmt.Errorf("graphql: %s", result.Errors[0].Message)
	}

	return result.Data, nil
}

// errGraphQLRateLimited marks a GraphQL-pool exhaustion; callers must not
// respond by falling back to REST (that multiplies core-pool spend ~20x).
var errGraphQLRateLimited = errors.New("graphql rate limited")

func truncateBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func buildBatchQuery(runs []model.WorkflowRun) string {
	var b strings.Builder
	b.WriteString("query{")

	// filterBy:{checkType:LATEST} matches REST's filter=latest: only the
	// latest attempt's check runs, so re-run runs don't carry duplicate
	// old-attempt jobs.
	fragment := fmt.Sprintf(`...on WorkflowRun{databaseId checkSuite{checkRuns(first:%d,filterBy:{checkType:LATEST}){pageInfo{hasNextPage} nodes{name databaseId startedAt completedAt status conclusion steps(first:%d){pageInfo{hasNextPage} nodes{name number startedAt completedAt status conclusion}}}}}}`,
		graphqlMaxJobs, graphqlMaxSteps)

	for i := range runs {
		fmt.Fprintf(&b, "r%d:node(id:%q){%s}", i, runs[i].NodeID, fragment)
	}

	b.WriteString("}")
	return b.String()
}

// graphqlPageInfo reports whether a connection had more results than the
// per-query limit fetched.
type graphqlPageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
}

// graphqlRunResponse is the structure of each aliased node in the batch response.
type graphqlRunResponse struct {
	DatabaseID int64 `json:"databaseId"`
	CheckSuite *struct {
		CheckRuns struct {
			PageInfo graphqlPageInfo   `json:"pageInfo"`
			Nodes    []graphqlCheckRun `json:"nodes"`
		} `json:"checkRuns"`
	} `json:"checkSuite"`
}

func (r *graphqlRunResponse) truncated() bool {
	if r.CheckSuite == nil {
		return false
	}
	if r.CheckSuite.CheckRuns.PageInfo.HasNextPage {
		return true
	}
	for i := range r.CheckSuite.CheckRuns.Nodes {
		if r.CheckSuite.CheckRuns.Nodes[i].Steps.PageInfo.HasNextPage {
			return true
		}
	}
	return false
}

type graphqlCheckRun struct {
	Name        string  `json:"name"`
	DatabaseID  int64   `json:"databaseId"`
	StartedAt   *string `json:"startedAt"`
	CompletedAt *string `json:"completedAt"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	Steps       struct {
		PageInfo graphqlPageInfo `json:"pageInfo"`
		Nodes    []graphqlStep   `json:"nodes"`
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

// parseBatchResponse converts the aliased batch payload into run details.
// Runs the response does not account for — a malformed top-level payload or a
// missing alias key — are returned in missed so the caller can fetch them via
// REST; dropping them silently would lose data with no diagnostic.
func parseBatchResponse(raw json.RawMessage, runs []model.WorkflowRun) (details []model.RunDetail, missed []model.WorkflowRun, warnings []Warning) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, runs, nil
	}

	for i := range runs {
		key := fmt.Sprintf("r%d", i)
		nodeRaw, ok := response[key]
		if !ok {
			missed = append(missed, runs[i])
			continue
		}

		var node graphqlRunResponse
		if err := json.Unmarshal(nodeRaw, &node); err != nil || node.CheckSuite == nil {
			warnings = append(warnings, newGraphQLWarning(runs[i].ID, "failed to parse GraphQL response"))
			continue
		}

		jobs := convertGraphQLJobs(node.CheckSuite.CheckRuns.Nodes, runs[i].ID)
		details = append(details, model.RunDetail{Run: runs[i], Jobs: jobs, Truncated: node.truncated()})
	}

	return details, missed, warnings
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
	return diag.New(diag.Warn, diag.KindNetwork, fmt.Sprintf("run-%d", runID),
		fmt.Sprintf("run %d: %s", runID, msg))
}
