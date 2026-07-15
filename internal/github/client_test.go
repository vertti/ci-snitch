package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	// go-github v87 sends requests under /api/v3/ when configured via
	// WithEnterpriseURLs, so strip that prefix before dispatching to the
	// caller's handler so existing test routes keep working.
	root := http.NewServeMux()
	root.Handle("/api/v3/", http.StripPrefix("/api/v3", handler))
	// GraphQL endpoint lives at the server root (no /api/v3 prefix).
	root.Handle("/graphql", handler)
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)

	ghClient, err := gh.NewClient(
		gh.WithAuthToken("test-token"),
		gh.WithEnterpriseURLs(srv.URL, srv.URL),
	)
	require.NoError(t, err)

	return &Client{
		gh:         ghClient,
		owner:      "test-owner",
		repo:       "test-repo",
		jobSem:     make(chan struct{}, defaultMaxConcurrentJobs),
		graphqlURL: srv.URL + "/graphql",
	}
}

func TestNewClient_ValidRepo(t *testing.T) {
	c, err := NewClient("token", "owner/repo")
	require.NoError(t, err)
	assert.Equal(t, "owner", c.owner)
	assert.Equal(t, "repo", c.repo)
}

func TestNewClient_InvalidRepo(t *testing.T) {
	tests := []string{"", "noslash", "/nope", "nope/"}
	for _, input := range tests {
		_, err := NewClient("token", input)
		assert.Error(t, err, "input: %q", input)
	}
}

func TestListWorkflows_ActionableErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantHints  []string
		dontLeakGo bool
	}{
		{name: "404 repo not found", status: 404,
			wantHints: []string{"not found", "private", "token"}},
		{name: "401 bad credentials", status: 401,
			wantHints: []string{"token", "expired"}},
		{name: "403 forbidden", status: 403,
			wantHints: []string{"access"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"message": "whatever"}`))
			})
			c := testClient(t, mux)
			_, err := c.ListWorkflows(context.Background())
			require.Error(t, err)
			for _, hint := range tt.wantHints {
				assert.Contains(t, strings.ToLower(err.Error()), hint,
					"a raw go-github error gives the user nothing to act on")
			}
		})
	}
}

func TestListWorkflows_GoldenFile(t *testing.T) {
	data, err := os.ReadFile("testdata/list_workflows.json")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})

	c := testClient(t, mux)
	workflows, err := c.ListWorkflows(context.Background())
	require.NoError(t, err)
	assert.Len(t, workflows, 3)
	assert.Equal(t, "CI", workflows[0].Name)
	assert.NotZero(t, workflows[0].ID)
	assert.Contains(t, workflows[0].Path, ".github/workflows/")
}

func TestFetchRuns_GoldenFile(t *testing.T) {
	data, err := os.ReadFile("testdata/list_runs.json")
	require.NoError(t, err)

	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows/12345/runs", func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})

	c := testClient(t, mux)

	since := time.Now().AddDate(0, 0, -3)
	runs, warnings, err := c.FetchRuns(context.Background(), 12345, since, "")
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Len(t, runs, 3) // golden file has 3 runs
	assert.Equal(t, 1, callCount, "should need only one window for 3 days")

	r := runs[0]
	assert.NotZero(t, r.ID)
	assert.Equal(t, "completed", r.Status)
	assert.Equal(t, "success", r.Conclusion)
	assert.NotEmpty(t, r.HeadSHA)
	assert.NotEmpty(t, r.HeadBranch)
	assert.False(t, r.StartedAt.IsZero())
	assert.False(t, r.UpdatedAt.IsZero())
}

func TestFetchRuns_SlidingWindows(t *testing.T) {
	// Empty response for all windows
	emptyResp := `{"total_count": 0, "workflow_runs": []}`
	callCount := 0

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows/1/runs", func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(emptyResp))
	})

	c := testClient(t, mux)

	// 15 days ago → 2 disjoint windows (each spans 8 calendar days inclusive;
	// the next window starts the day after the previous ends).
	since := time.Now().AddDate(0, 0, -15)
	runs, _, err := c.FetchRuns(context.Background(), 1, since, "")
	require.NoError(t, err)
	assert.Empty(t, runs)
	assert.Equal(t, 2, callCount, "15 days should produce 2 disjoint windows")
}

func TestFetchRuns_WindowsAreDisjointAndContiguous(t *testing.T) {
	// windowStart = windowEnd with an inclusive date-only created filter put
	// the seam day in BOTH windows: boundary runs were double-listed,
	// double-hydrated, double-counted by the budget, and double-saved.
	var createdParams []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		createdParams = append(createdParams, r.URL.Query().Get("created"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count": 0, "workflow_runs": []}`))
	})

	c := testClient(t, mux)
	since := time.Now().UTC().AddDate(0, 0, -15)
	_, _, err := c.FetchRuns(context.Background(), 1, since, "")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(createdParams), 2)

	for i := 1; i < len(createdParams); i++ {
		prevEnd := strings.SplitN(createdParams[i-1], "..", 2)[1]
		nextStart := strings.SplitN(createdParams[i], "..", 2)[0]
		prev, err := time.Parse("2006-01-02", prevEnd)
		require.NoError(t, err)
		next, err := time.Parse("2006-01-02", nextStart)
		require.NoError(t, err)
		assert.Equal(t, prev.AddDate(0, 0, 1), next,
			"window %d must start the day AFTER window %d ends (inclusive date filter): %v",
			i, i-1, createdParams)
	}
}

func TestRateLimit_ParsesBothPools(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resources": {
			"core":    {"limit": 5000, "remaining": 4000, "reset": 1752570000},
			"graphql": {"limit": 5000, "remaining": 3000, "reset": 1752570000}
		}}`))
	})

	c := testClient(t, mux)
	rl, err := c.RateLimit(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4000, rl.Core.Remaining)
	assert.Equal(t, 3000, rl.GraphQL.Remaining, "hydration budgeting needs the GraphQL pool")
}

func TestFetchRuns_CapWarningEmittedOncePerWindow(t *testing.T) {
	pages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		// First page links to a second one; both report the same capped total.
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<https://api.github.com/repos/test-owner/test-repo/actions/workflows/1/runs?page=2>; rel="next"`)
		}
		_, _ = w.Write([]byte(`{"total_count": 1500, "workflow_runs": [{"id": 1}]}`))
	})

	c := testClient(t, mux)
	since := time.Now().AddDate(0, 0, -3) // single 7-day window
	_, warnings, err := c.FetchRuns(context.Background(), 1, since, "")
	require.NoError(t, err)
	require.Equal(t, 2, pages, "sanity: pagination must have followed the Link header")
	assert.Len(t, warnings, 1, "cap warning must be emitted once per window, not once per page")
	assert.Contains(t, warnings[0].Message, "1000")
}

func TestFetchRuns_BranchFilter(t *testing.T) {
	var capturedBranch string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		capturedBranch = r.URL.Query().Get("branch")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count": 0, "workflow_runs": []}`))
	})

	c := testClient(t, mux)
	since := time.Now().AddDate(0, 0, -3)
	_, _, err := c.FetchRuns(context.Background(), 1, since, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", capturedBranch)
}

func TestFetchJobs_GoldenFile(t *testing.T) {
	data, err := os.ReadFile("testdata/list_jobs.json")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/200000/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})

	c := testClient(t, mux)
	jobs, err := c.FetchJobs(context.Background(), 200000)
	require.NoError(t, err)
	assert.Len(t, jobs, 2)

	// First job: build
	assert.Equal(t, "build", jobs[0].Name)
	assert.Equal(t, "completed", jobs[0].Status)
	assert.Equal(t, "success", jobs[0].Conclusion)
	assert.Equal(t, 75*time.Second, jobs[0].Duration())
	assert.Len(t, jobs[0].Steps, 4)

	// Step timing
	buildStep := jobs[0].Steps[2]
	assert.Equal(t, "Build", buildStep.Name)
	assert.Equal(t, 65*time.Second, buildStep.Duration())

	// Second job: test matrix
	assert.Equal(t, "test (ubuntu-latest, 20)", jobs[1].Name)
	assert.Len(t, jobs[1].Steps, 4)
}

func TestFetchRunDetails_PartialFailure(t *testing.T) {
	data, err := os.ReadFile("testdata/list_jobs.json")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/200000/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/999999/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message": "Not Found"}`))
	})

	c := testClient(t, mux)
	runs := []model.WorkflowRun{
		{ID: 200000, Status: "completed"},
		{ID: 999999, Status: "completed"},
	}

	details, warnings := c.FetchRunDetails(context.Background(), runs)
	assert.Len(t, details, 1, "should have 1 successful result")
	assert.Len(t, warnings, 1, "should have 1 warning for failed run")
	assert.Contains(t, warnings[0].Message, "999999")
}

func TestFetchRunDetails_Empty(t *testing.T) {
	c := testClient(t, http.NewServeMux())
	details, warnings := c.FetchRunDetails(context.Background(), nil)
	assert.Empty(t, details)
	assert.Empty(t, warnings)
}

func TestFetchRunDetails_ConcurrencyBounded(t *testing.T) {
	var mu sync.Mutex
	maxConcurrent := 0
	current := 0

	data, err := os.ReadFile("testdata/list_jobs.json")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond) // simulate latency

		mu.Lock()
		current--
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})

	c := testClient(t, mux)

	// Create 20 runs to exercise concurrency
	runs := make([]model.WorkflowRun, 20)
	for i := range runs {
		runs[i] = model.WorkflowRun{ID: int64(200000 + i), Status: "completed"}
	}

	details, warnings := c.FetchRunDetails(context.Background(), runs)
	assert.Len(t, details, 20)
	assert.Empty(t, warnings)
	assert.LessOrEqual(t, maxConcurrent, defaultMaxConcurrentJobs, "should not exceed semaphore capacity")
}

func TestFetchRunDetails_SemaphoreBoundsConcurrency(t *testing.T) {
	var mu sync.Mutex
	maxConcurrent := 0
	current := 0

	data, err := os.ReadFile("testdata/list_jobs.json")
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond)

		mu.Lock()
		current--
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})

	c := testClient(t, mux)
	c.jobSem = make(chan struct{}, 3) // tight semaphore

	runs := make([]model.WorkflowRun, 20)
	for i := range runs {
		runs[i] = model.WorkflowRun{ID: int64(200000 + i), Status: "completed"}
	}

	details, warnings := c.FetchRunDetails(context.Background(), runs)
	assert.Len(t, details, 20)
	assert.Empty(t, warnings)
	assert.LessOrEqual(t, maxConcurrent, 3, "should not exceed semaphore capacity of 3")
}

func TestFetchRuns_ContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/workflows/1/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count": 0, "workflow_runs": []}`))
	})

	c := testClient(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	since := time.Now().AddDate(0, 0, -3)
	_, _, err := c.FetchRuns(ctx, 1, since, "")
	assert.Error(t, err)
}
