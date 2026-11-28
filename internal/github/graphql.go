package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
func (c *Client) FetchRunDetailsGraphQL(ctx context.Context, runs []model.WorkflowRun) (details []model.RunDetail, warnings []diag.Diagnostic) {
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
	timeouts := 0
	for start := 0; start < len(graphqlRuns); start += GraphQLBatchSize {
		end := min(start+GraphQLBatchSize, len(graphqlRuns))
		batch := graphqlRuns[start:end]

		batchDetails, batchWarnings, err := c.fetchBatchGraphQL(ctx, batch)
		details = append(details, batchDetails...)
		warnings = append(warnings, batchWarnings...)
		if limited, ok := errors.AsType[*rateLimitError](err); ok {
			// Falling back to REST here would burn ~20x the core budget the
			// pre-flight check approved. Skip the remaining hydration instead.
			skipped := len(graphqlRuns) - start
			msg := fmt.Sprintf("GitHub rate limit hit (%s); skipped hydration of %d runs (not falling back to REST to protect the core pool)", limited.reason, skipped)
			if !limited.retryAt.IsZero() {
				msg += "; retry after " + limited.retryAt.UTC().Format("15:04:05 MST")
			}
			warning := diag.New(diag.Warn, diag.KindRateLimit, "graphql", msg)
			warning.RetryAt = limited.retryAt
			warnings = append(warnings, warning)
			break
		}
		if ctx.Err() != nil {
			// Cancellation is not a data problem: no warnings, no fallback —
			// the caller observes ctx.Err() and aborts the run.
			return details, warnings
		}
		if _, timedOut := errors.AsType[*tooHeavyError](err); !timedOut {
			timeouts = 0
			continue
		}
		timeouts++
		if timeouts == maxConsecutiveTimeouts {
			remaining := graphqlRuns[end:]
			c.log("GraphQL keeps timing out on this workflow, fetching its remaining runs via REST", "runs", len(remaining))
			restRuns = append(restRuns, remaining...)
			break
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
func (c *Client) completeTruncated(ctx context.Context, details []model.RunDetail, warnings []diag.Diagnostic) (outDetails []model.RunDetail, outWarnings []diag.Diagnostic) {
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

// fetchBatchGraphQL hydrates one batch. A rate limit or a cancelled context
// comes back as an error with nothing fetched. A tooHeavyError comes back
// together with the details already refetched over REST, so the caller can
// count it.
func (c *Client) fetchBatchGraphQL(ctx context.Context, runs []model.WorkflowRun) (details []model.RunDetail, warnings []diag.Diagnostic, err error) {
	query := buildBatchQuery(runs)

	raw, err := c.doGraphQL(ctx, query)
	if err != nil {
		if _, limited := errors.AsType[*rateLimitError](err); limited || ctx.Err() != nil {
			return nil, nil, err
		}
		c.log("GraphQL batch failed, falling back to REST", "error", err, "batch_size", len(runs))
		details, warnings = c.FetchRunDetails(ctx, runs)
		if _, timedOut := errors.AsType[*tooHeavyError](err); timedOut {
			return details, warnings, err
		}
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
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if msg := truncateBody(apiErrorMessage(body)); msg != "" {
			reason += ": " + msg
		}
		if resp.StatusCode == http.StatusTooManyRequests || mentionsRateLimit(string(body)) {
			return nil, &rateLimitError{reason, retryTime(resp.Header, time.Now())}
		}
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
			return nil, &tooHeavyError{reason}
		}
		return nil, errors.New("graphql: " + reason)
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
		return nil, &tooHeavyError{fmt.Sprintf("HTTP 200 with a body that does not parse (%d bytes): %v", len(respBody), err)}
	}
	if len(result.Errors) > 0 {
		first := result.Errors[0]
		if first.Type == "RATE_LIMITED" || mentionsRateLimit(first.Message) {
			return nil, &rateLimitError{first.Message, retryTime(resp.Header, time.Now())}
		}
		return nil, fmt.Errorf("graphql: %s", first.Message)
	}

	return result.Data, nil
}

// rateLimitError is a GitHub rate limit, primary or secondary. Callers must
// not fall back to REST on it (that multiplies core-pool spend ~20x).
type rateLimitError struct {
	reason  string
	retryAt time.Time // zero when GitHub did not say
}

func (e *rateLimitError) Error() string { return "graphql rate limited: " + e.reason }

// A workflow whose batches time out twice in a row is too heavy for GraphQL:
// every further batch would wait out GitHub's time limit and then be fetched
// over REST anyway. One timeout between successes is a hiccup.
const maxConsecutiveTimeouts = 2

// tooHeavyError is GitHub giving up on a query it could not finish in time:
// a 502 or 504, or a 200 with an empty or cut-short body.
type tooHeavyError struct{ reason string }

func (e *tooHeavyError) Error() string { return "graphql: " + e.reason }

// retryTime reads when GitHub will take requests again: Retry-After on a
// secondary limit, the reset time when the primary pool is empty. The reset
// of a pool that still has points says nothing about a secondary limit.
func retryTime(h http.Header, now time.Time) time.Time {
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs > 0 {
		return now.Add(time.Duration(secs) * time.Second)
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if epoch, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(epoch, 0)
		}
	}
	return time.Time{}
}

// GitHub marks only some rate-limit responses with the RATE_LIMITED type;
// the wording is the one thing every shape of them shares.
func mentionsRateLimit(s string) bool {
	return strings.Contains(strings.ToLower(s), "rate limit")
}

func apiErrorMessage(body []byte) []byte {
	var apiErr struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
		return []byte(apiErr.Message)
	}
	return body
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

	// filterBy:{checkType:LATEST} matches REST's filter=latest: only the
	// latest attempt's check runs, so re-run runs don't carry duplicate
	// old-attempt jobs.
	fragment := fmt.Sprintf(`...on WorkflowRun{checkSuite{checkRuns(first:%d,filterBy:{checkType:LATEST}){pageInfo{hasNextPage} nodes{name databaseId startedAt completedAt status conclusion steps(first:%d){pageInfo{hasNextPage} nodes{name number startedAt completedAt status conclusion}}}}}}`,
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
func parseBatchResponse(raw json.RawMessage, runs []model.WorkflowRun) (details []model.RunDetail, missed []model.WorkflowRun, warnings []diag.Diagnostic) {
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

func newGraphQLWarning(runID int64, msg string) diag.Diagnostic {
	return diag.New(diag.Warn, diag.KindNetwork, fmt.Sprintf("run-%d", runID),
		fmt.Sprintf("run %d: %s", runID, msg))
}
