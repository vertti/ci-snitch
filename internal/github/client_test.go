package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ghClient := gh.NewClient(nil).WithAuthToken("test-token")
	ghClient.BaseURL, _ = ghClient.BaseURL.Parse(srv.URL + "/")

	return &Client{
		gh:    ghClient,
		owner: "test-owner",
		repo:  "test-repo",
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
	assert.Equal(t, "Auto Cherry-Pick", workflows[0].Name)
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
	runs, err := c.FetchRuns(context.Background(), 12345, since, "")
	require.NoError(t, err)
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
	runs, err := c.FetchRuns(context.Background(), 1, since, "")
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
	_, err := c.FetchRuns(context.Background(), 1, since, "main")
	require.NoError(t, err)
	assert.Equal(t, "main", capturedBranch)
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
	_, err := c.FetchRuns(ctx, 1, since, "")
	assert.Error(t, err)
}
