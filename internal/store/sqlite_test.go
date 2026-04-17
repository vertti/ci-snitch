package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/vertti/ci-snitch/internal/model"
)

const statusInProgress = "in_progress"

func testStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testRunDetail() model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	return model.RunDetail{
		Run: model.WorkflowRun{
			ID:           1001,
			WorkflowID:   100,
			WorkflowName: "CI",
			Name:         "Fix tests",
			Event:        "push",
			Status:       "completed",
			Conclusion:   "success",
			HeadBranch:   "main",
			HeadSHA:      "abc123",
			RunAttempt:   1,
			CreatedAt:    base,
			StartedAt:    base.Add(5 * time.Second),
			UpdatedAt:    base.Add(3 * time.Minute),
		},
		Jobs: []model.Job{
			{
				ID:              2001,
				RunID:           1001,
				Name:            "build",
				Status:          "completed",
				Conclusion:      "success",
				StartedAt:       base.Add(10 * time.Second),
				CompletedAt:     base.Add(2 * time.Minute),
				RunnerName:      "GitHub Actions 4",
				RunnerGroupName: "GitHub Actions",
				Labels:          []string{"ubuntu-latest"},
				Steps: []model.Step{
					{
						Name:        "Checkout",
						Number:      1,
						Status:      "completed",
						Conclusion:  "success",
						StartedAt:   base.Add(10 * time.Second),
						CompletedAt: base.Add(15 * time.Second),
					},
					{
						Name:        "Build",
						Number:      2,
						Status:      "completed",
						Conclusion:  "success",
						StartedAt:   base.Add(15 * time.Second),
						CompletedAt: base.Add(2 * time.Minute),
					},
				},
			},
		},
	}
}

func TestSaveAndLoadRunDetail(t *testing.T) {
	s := testStore(t)
	detail := testRunDetail()

	err := s.SaveRunDetail(detail)
	require.NoError(t, err)

	loaded, err := s.LoadRunDetail(1001)
	require.NoError(t, err)

	assert.Equal(t, detail.Run.ID, loaded.Run.ID)
	assert.Equal(t, detail.Run.WorkflowName, loaded.Run.WorkflowName)
	assert.Equal(t, detail.Run.HeadSHA, loaded.Run.HeadSHA)
	assert.Equal(t, detail.Run.Conclusion, loaded.Run.Conclusion)
	assert.Equal(t, "push", loaded.Run.Event)
	assert.WithinDuration(t, detail.Run.CreatedAt, loaded.Run.CreatedAt, time.Second)
	assert.WithinDuration(t, detail.Run.StartedAt, loaded.Run.StartedAt, time.Second)

	require.Len(t, loaded.Jobs, 1)
	assert.Equal(t, "build", loaded.Jobs[0].Name)
	assert.WithinDuration(t, detail.Jobs[0].StartedAt, loaded.Jobs[0].StartedAt, time.Second)
	assert.Equal(t, "GitHub Actions 4", loaded.Jobs[0].RunnerName)
	assert.Equal(t, "GitHub Actions", loaded.Jobs[0].RunnerGroupName)
	assert.Equal(t, []string{"ubuntu-latest"}, loaded.Jobs[0].Labels)

	require.Len(t, loaded.Jobs[0].Steps, 2)
	assert.Equal(t, "Checkout", loaded.Jobs[0].Steps[0].Name)
	assert.Equal(t, 1, loaded.Jobs[0].Steps[0].Number)
	assert.Equal(t, "Build", loaded.Jobs[0].Steps[1].Name)
}

func TestSaveRunDetail_Upsert(t *testing.T) {
	s := testStore(t)
	detail := testRunDetail()

	// Save initially as in-progress
	detail.Run.Status = statusInProgress
	detail.Run.Conclusion = ""
	require.NoError(t, s.SaveRunDetail(detail))

	// Update to completed
	detail.Run.Status = "completed"
	detail.Run.Conclusion = "success"
	require.NoError(t, s.SaveRunDetail(detail))

	loaded, err := s.LoadRunDetail(1001)
	require.NoError(t, err)
	assert.Equal(t, "completed", loaded.Run.Status)
	assert.Equal(t, "success", loaded.Run.Conclusion)
}

func TestRunsSince(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	// Save 3 runs at different times
	for i := range 3 {
		d := testRunDetail()
		d.Run.ID = int64(1001 + i)
		d.Run.CreatedAt = base.Add(time.Duration(i) * 24 * time.Hour)
		d.Run.StartedAt = d.Run.CreatedAt.Add(5 * time.Second)
		d.Run.UpdatedAt = d.Run.CreatedAt.Add(3 * time.Minute)
		d.Jobs[0].ID = int64(2001 + i)
		require.NoError(t, s.SaveRunDetail(d))
	}

	// Query since day 1 — should get runs from day 1 and day 2
	since := base.Add(24 * time.Hour)
	runs, err := s.RunsSince(100, since)
	require.NoError(t, err)
	assert.Len(t, runs, 2)
	assert.Equal(t, int64(1002), runs[0].ID)
	assert.Equal(t, int64(1003), runs[1].ID)
}

func TestRunsSince_ExcludesIncomplete(t *testing.T) {
	s := testStore(t)

	detail := testRunDetail()
	detail.Run.Status = statusInProgress
	detail.Run.Conclusion = ""
	require.NoError(t, s.SaveRunDetail(detail))

	runs, err := s.RunsSince(100, time.Time{})
	require.NoError(t, err)
	assert.Empty(t, runs, "in-progress runs should not be returned")
}

func TestIncompleteRunIDs(t *testing.T) {
	s := testStore(t)

	// One completed, one in-progress
	d1 := testRunDetail()
	require.NoError(t, s.SaveRunDetail(d1))

	d2 := testRunDetail()
	d2.Run.ID = 1002
	d2.Run.Status = statusInProgress
	d2.Run.Conclusion = ""
	d2.Jobs[0].ID = 2002
	require.NoError(t, s.SaveRunDetail(d2))

	ids, err := s.IncompleteRunIDs()
	require.NoError(t, err)
	assert.Equal(t, []int64{1002}, ids)
}

func TestLoadRunDetails(t *testing.T) {
	s := testStore(t)

	for i := range 3 {
		d := testRunDetail()
		d.Run.ID = int64(1001 + i)
		d.Jobs[0].ID = int64(2001 + i)
		require.NoError(t, s.SaveRunDetail(d))
	}

	details, err := s.LoadRunDetails(100, time.Time{})
	require.NoError(t, err)
	assert.Len(t, details, 3)

	for _, d := range details {
		assert.Len(t, d.Jobs, 1)
		assert.Len(t, d.Jobs[0].Steps, 2)
	}
}

func TestSaveRunDetails_Batch(t *testing.T) {
	s := testStore(t)

	var details []model.RunDetail
	for i := range 5 {
		d := testRunDetail()
		d.Run.ID = int64(1001 + i)
		d.Jobs[0].ID = int64(2001 + i)
		details = append(details, d)
	}

	err := s.SaveRunDetails(details)
	require.NoError(t, err)

	runs, err := s.RunsSince(100, time.Time{})
	require.NoError(t, err)
	assert.Len(t, runs, 5)
}

func TestMigration_AddsEventColumn(t *testing.T) {
	// Simulate a pre-migration database: create schema without the event column,
	// then open with Open() which should migrate.
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)

	// Create old schema without event column
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS runs (
		id INTEGER PRIMARY KEY, workflow_id INTEGER, workflow_name TEXT,
		name TEXT, status TEXT, conclusion TEXT, head_branch TEXT,
		head_sha TEXT, run_attempt INTEGER, created_at TEXT, started_at TEXT, updated_at TEXT
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS jobs (
		id INTEGER PRIMARY KEY, run_id INTEGER, name TEXT, status TEXT,
		conclusion TEXT, started_at TEXT, completed_at TEXT
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS steps (
		id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER, name TEXT,
		number INTEGER, status TEXT, conclusion TEXT, started_at TEXT, completed_at TEXT
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Open should migrate successfully
	s, err := Open(path)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	// Should be able to save a run with the event field
	detail := testRunDetail()
	err = s.SaveRunDetail(detail)
	require.NoError(t, err, "save should work after migration adds event column")

	// Round-trip the event field
	loaded, err := s.LoadRunDetail(detail.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, "push", loaded.Run.Event)
}

func TestLoadRunDetail_CorruptTimeReturnsError(t *testing.T) {
	s := testStore(t)

	// Insert a run with a corrupt created_at timestamp directly via SQL.
	_, err := s.db.Exec(`INSERT INTO runs (id, workflow_id, workflow_name, name, event, status, conclusion,
		head_branch, head_sha, run_attempt, created_at, started_at, updated_at)
		VALUES (999, 100, 'CI', 'push', 'push', 'completed', 'success',
		'main', 'abc123', 1, 'not-a-timestamp', '2026-04-01T12:00:00Z', '2026-04-01T12:30:00Z')`)
	require.NoError(t, err)

	_, err = s.LoadRunDetail(999)
	require.Error(t, err, "corrupt time string should return an error")
	assert.Contains(t, err.Error(), "parse time")
}

func TestRunsSince_CorruptTimeReturnsError(t *testing.T) {
	s := testStore(t)

	// Insert a run with valid created_at but corrupt started_at.
	_, err := s.db.Exec(`INSERT INTO runs (id, workflow_id, workflow_name, name, event, status, conclusion,
		head_branch, head_sha, run_attempt, created_at, started_at, updated_at)
		VALUES (998, 100, 'CI', 'push', 'push', 'completed', 'success',
		'main', 'abc123', 1, '2026-04-01T12:00:00Z', 'garbage', '2026-04-01T12:30:00Z')`)
	require.NoError(t, err)

	_, err = s.RunsSince(100, time.Time{})
	require.Error(t, err, "corrupt time string in runs should return an error")
	assert.Contains(t, err.Error(), "parse time")
}

func BenchmarkSaveRunDetails(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	s, err := Open(path)
	require.NoError(b, err)
	b.Cleanup(func() { _ = s.Close() })

	// Build 500 run details with 1 job and 2 steps each.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := make([]model.RunDetail, 500)
	for i := range details {
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(10000 + i), WorkflowID: 100, WorkflowName: "CI",
				Name: "push", Event: "push", Status: "completed", Conclusion: "success",
				HeadBranch: "main", HeadSHA: fmt.Sprintf("sha%d", i), RunAttempt: 1,
				CreatedAt: base.Add(time.Duration(i) * time.Minute),
				StartedAt: base.Add(time.Duration(i)*time.Minute + 5*time.Second),
				UpdatedAt: base.Add(time.Duration(i)*time.Minute + 3*time.Minute),
			},
			Jobs: []model.Job{{
				ID: int64(20000 + i), RunID: int64(10000 + i),
				Name: "build", Status: "completed", Conclusion: "success",
				StartedAt:   base.Add(time.Duration(i)*time.Minute + 10*time.Second),
				CompletedAt: base.Add(time.Duration(i)*time.Minute + 2*time.Minute),
				Labels:      []string{"ubuntu-latest"},
				Steps: []model.Step{
					{Name: "Checkout", Number: 1, Status: "completed", Conclusion: "success",
						StartedAt: base, CompletedAt: base.Add(5 * time.Second)},
					{Name: "Build", Number: 2, Status: "completed", Conclusion: "success",
						StartedAt: base.Add(5 * time.Second), CompletedAt: base.Add(2 * time.Minute)},
				},
			}},
		}
	}

	b.ResetTimer()
	for range b.N {
		if err := s.SaveRunDetails(details); err != nil {
			b.Fatal(err)
		}
	}
}
