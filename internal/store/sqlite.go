// Package store provides SQLite-backed persistence for workflow run data.
package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // SQLite driver registration

	"github.com/vertti/ci-snitch/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id           INTEGER PRIMARY KEY,
	workflow_id  INTEGER NOT NULL,
	workflow_name TEXT NOT NULL,
	name         TEXT NOT NULL,
	status       TEXT NOT NULL,
	conclusion   TEXT NOT NULL,
	head_branch  TEXT NOT NULL,
	head_sha     TEXT NOT NULL,
	run_attempt  INTEGER NOT NULL,
	created_at   TEXT NOT NULL,
	started_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runs_workflow_created ON runs(workflow_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

CREATE TABLE IF NOT EXISTS jobs (
	id           INTEGER PRIMARY KEY,
	run_id       INTEGER NOT NULL REFERENCES runs(id),
	name         TEXT NOT NULL,
	status       TEXT NOT NULL,
	conclusion   TEXT NOT NULL,
	started_at   TEXT NOT NULL,
	completed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_run ON jobs(run_id);

CREATE TABLE IF NOT EXISTS steps (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id       INTEGER NOT NULL REFERENCES jobs(id),
	name         TEXT NOT NULL,
	number       INTEGER NOT NULL,
	status       TEXT NOT NULL,
	conclusion   TEXT NOT NULL,
	started_at   TEXT NOT NULL,
	completed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_steps_job ON steps(job_id);
`

// Store persists and queries workflow run data in SQLite.
type Store struct {
	db *sql.DB
}

// DefaultPath returns the default database path (~/.cache/ci-snitch/data.db).
func DefaultPath() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("get cache dir: %w", err)
	}
	dir := filepath.Join(cacheDir, "ci-snitch")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	return filepath.Join(dir, "data.db"), nil
}

// Open opens or creates a SQLite database at the given path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// WAL mode allows concurrent reads and avoids SQLITE_BUSY under
	// parallel workflow writes.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveRunDetail persists a run and its jobs and steps.
// Uses INSERT OR REPLACE so re-fetched runs (e.g. previously in-progress) are updated.
func (s *Store) SaveRunDetail(d model.RunDetail) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // error on deferred close has no actionable caller

	r := d.Run
	_, err = tx.Exec(`INSERT OR REPLACE INTO runs (id, workflow_id, workflow_name, name, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkflowID, r.WorkflowName, r.Name, r.Status, r.Conclusion,
		r.HeadBranch, r.HeadSHA, r.RunAttempt,
		fmtTime(r.CreatedAt), fmtTime(r.StartedAt), fmtTime(r.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert run %d: %w", r.ID, err)
	}

	// Delete old jobs+steps on replace (cascade not supported without FK enforcement)
	if _, err := tx.Exec(`DELETE FROM steps WHERE job_id IN (SELECT id FROM jobs WHERE run_id = ?)`, r.ID); err != nil {
		return fmt.Errorf("delete old steps for run %d: %w", r.ID, err)
	}
	if _, err := tx.Exec(`DELETE FROM jobs WHERE run_id = ?`, r.ID); err != nil {
		return fmt.Errorf("delete old jobs for run %d: %w", r.ID, err)
	}

	for _, j := range d.Jobs {
		_, err := tx.Exec(`INSERT INTO jobs (id, run_id, name, status, conclusion, started_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			j.ID, r.ID, j.Name, j.Status, j.Conclusion,
			fmtTime(j.StartedAt), fmtTime(j.CompletedAt),
		)
		if err != nil {
			return fmt.Errorf("insert job %d: %w", j.ID, err)
		}

		for _, st := range j.Steps {
			_, err := tx.Exec(`INSERT INTO steps (job_id, name, number, status, conclusion, started_at, completed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				j.ID, st.Name, st.Number, st.Status, st.Conclusion,
				fmtTime(st.StartedAt), fmtTime(st.CompletedAt),
			)
			if err != nil {
				return fmt.Errorf("insert step %q for job %d: %w", st.Name, j.ID, err)
			}
		}
	}

	return tx.Commit()
}

// SaveRunDetails persists multiple run details.
func (s *Store) SaveRunDetails(details []model.RunDetail) error {
	for _, d := range details {
		if err := s.SaveRunDetail(d); err != nil {
			return err
		}
	}
	return nil
}

// RunsSince returns completed runs for a workflow since the given time.
func (s *Store) RunsSince(workflowID int64, since time.Time) ([]model.WorkflowRun, error) {
	rows, err := s.db.Query(`SELECT id, workflow_id, workflow_name, name, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at
		FROM runs WHERE workflow_id = ? AND created_at >= ? AND status = 'completed'
		ORDER BY created_at ASC`,
		workflowID, fmtTime(since),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // error on deferred close has no actionable caller

	return scanRuns(rows)
}

// IncompleteRunIDs returns IDs of runs that are not yet completed.
func (s *Store) IncompleteRunIDs() ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM runs WHERE status != 'completed'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // error on deferred close has no actionable caller

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadRunDetail loads a fully hydrated run detail from the store.
func (s *Store) LoadRunDetail(runID int64) (*model.RunDetail, error) {
	row := s.db.QueryRow(`SELECT id, workflow_id, workflow_name, name, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at
		FROM runs WHERE id = ?`, runID)

	run, err := scanRun(row)
	if err != nil {
		return nil, fmt.Errorf("load run %d: %w", runID, err)
	}

	jobRows, err := s.db.Query(`SELECT id, run_id, name, status, conclusion, started_at, completed_at
		FROM jobs WHERE run_id = ? ORDER BY started_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer jobRows.Close() //nolint:errcheck // error on deferred close has no actionable caller

	var jobs []model.Job
	for jobRows.Next() {
		var j model.Job
		var startStr, compStr string
		if err := jobRows.Scan(&j.ID, &j.RunID, &j.Name, &j.Status, &j.Conclusion, &startStr, &compStr); err != nil {
			return nil, err
		}
		j.StartedAt = parseTime(startStr)
		j.CompletedAt = parseTime(compStr)

		stepRows, err := s.db.Query(`SELECT name, number, status, conclusion, started_at, completed_at
			FROM steps WHERE job_id = ? ORDER BY number ASC`, j.ID)
		if err != nil {
			return nil, err
		}

		for stepRows.Next() {
			var st model.Step
			var sStart, sComp string
			if err := stepRows.Scan(&st.Name, &st.Number, &st.Status, &st.Conclusion, &sStart, &sComp); err != nil {
				_ = stepRows.Close()
				return nil, err
			}
			st.StartedAt = parseTime(sStart)
			st.CompletedAt = parseTime(sComp)
			j.Steps = append(j.Steps, st)
		}
		_ = stepRows.Close()
		if err := stepRows.Err(); err != nil {
			return nil, err
		}

		jobs = append(jobs, j)
	}
	if err := jobRows.Err(); err != nil {
		return nil, err
	}

	return &model.RunDetail{Run: run, Jobs: jobs}, nil
}

// LoadRunDetails loads all completed run details for a workflow since the given time.
func (s *Store) LoadRunDetails(workflowID int64, since time.Time) ([]model.RunDetail, error) {
	runs, err := s.RunsSince(workflowID, since)
	if err != nil {
		return nil, err
	}

	details := make([]model.RunDetail, 0, len(runs))
	for _, r := range runs {
		d, err := s.LoadRunDetail(r.ID)
		if err != nil {
			return nil, err
		}
		details = append(details, *d)
	}
	return details, nil
}

func scanRuns(rows *sql.Rows) ([]model.WorkflowRun, error) {
	var runs []model.WorkflowRun
	for rows.Next() {
		var r model.WorkflowRun
		var createdStr, startedStr, updatedStr string
		if err := rows.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.Name, &r.Status, &r.Conclusion,
			&r.HeadBranch, &r.HeadSHA, &r.RunAttempt, &createdStr, &startedStr, &updatedStr); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTime(createdStr)
		r.StartedAt = parseTime(startedStr)
		r.UpdatedAt = parseTime(updatedStr)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func scanRun(row *sql.Row) (model.WorkflowRun, error) {
	var r model.WorkflowRun
	var createdStr, startedStr, updatedStr string
	err := row.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.Name, &r.Status, &r.Conclusion,
		&r.HeadBranch, &r.HeadSHA, &r.RunAttempt, &createdStr, &startedStr, &updatedStr)
	if err != nil {
		return r, err
	}
	r.CreatedAt = parseTime(createdStr)
	r.StartedAt = parseTime(startedStr)
	r.UpdatedAt = parseTime(updatedStr)
	return r, nil
}

const timeFormat = time.RFC3339

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		log.Printf("WARNING: failed to parse time %q: %v", s, err)
		return time.Time{}
	}
	return t
}
