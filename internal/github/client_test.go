package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v84/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ghClient := gh.NewClient(nil).WithAuthToken("test-token")
	ghClient.BaseURL, _ = ghClient.BaseURL.Parse(srv.URL + "/")

	return &Client{
		gh:     ghClient,
		owner:  "test-owner",
		repo:   "test-repo",
		jobSem: make(chan struct{}, defaultMaxConcurrentJobs),
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

	// 15 days ago → should produce 3 windows (7 + 7 + 1 days)
	since := time.Now().AddDate(0, 0, -15)
	runs, _, err := c.FetchRuns(context.Background(), 1, since, "")
	require.NoError(t, err)
	assert.Empty(t, runs)
	assert.Equal(t, 3, callCount, "15 days should produce 3 windows of 7 days each")
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
