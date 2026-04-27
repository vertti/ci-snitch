// Package store provides SQLite-backed persistence for workflow run data.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	event        TEXT NOT NULL DEFAULT '',
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
	id               INTEGER PRIMARY KEY,
	run_id           INTEGER NOT NULL REFERENCES runs(id),
	name             TEXT NOT NULL,
	status           TEXT NOT NULL,
	conclusion       TEXT NOT NULL,
	started_at       TEXT NOT NULL,
	completed_at     TEXT NOT NULL,
	runner_name      TEXT NOT NULL DEFAULT '',
	runner_group_name TEXT NOT NULL DEFAULT '',
	labels           TEXT NOT NULL DEFAULT ''
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
	// parallel workflow writes. busy_timeout retries on lock contention
	// instead of failing immediately. foreign_keys enforces the REFERENCES
	// declarations in the schema (off by default in SQLite).
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("exec %s: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// migrate applies schema migrations for existing databases.
func migrate(db *sql.DB) error {
	cols, err := loadColumnSets(db, "runs", "jobs")
	if err != nil {
		return fmt.Errorf("load column info: %w", err)
	}

	// Add event column to runs table (added in v0.7.0).
	if !cols["runs"]["event"] {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN event TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add event column: %w", err)
		}
	}
	// Add runner metadata columns to jobs table (added in v0.8.0).
	for _, col := range []string{"runner_name", "runner_group_name", "labels"} {
		if !cols["jobs"][col] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE jobs ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, col)); err != nil {
				return fmt.Errorf("add %s column: %w", col, err)
			}
		}
	}
	return nil
}

var validTables = map[string]bool{"runs": true, "jobs": true, "steps": true}

// loadColumnSets queries PRAGMA table_info once per table and returns
// a map of table → set of column names.
func loadColumnSets(db *sql.DB, tables ...string) (map[string]map[string]bool, error) {
	result := make(map[string]map[string]bool, len(tables))
	for _, table := range tables {
		if !validTables[table] {
			return nil, fmt.Errorf("unknown table %q", table)
		}
		colSet, err := loadTableColumns(db, table)
		if err != nil {
			return nil, err
		}
		result[table] = colSet
	}
	return result, nil
}

func loadTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // error on deferred close has no actionable caller

	colSet := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		colSet[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return colSet, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveRunDetail persists a run and its jobs and steps.
// Uses INSERT OR REPLACE so re-fetched runs (e.g. previously in-progress) are updated.
func (s *Store) SaveRunDetail(d *model.RunDetail) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // error on deferred close has no actionable caller

	r := d.Run

	// Clear existing children before replacing the parent: with foreign_keys ON,
	// the implicit delete inside INSERT OR REPLACE on runs would otherwise fail
	// because dependent jobs/steps reference the run we are about to replace.
	if _, err := tx.Exec(`DELETE FROM steps WHERE job_id IN (SELECT id FROM jobs WHERE run_id = ?)`, r.ID); err != nil {
		return fmt.Errorf("delete old steps for run %d: %w", r.ID, err)
	}
	if _, err := tx.Exec(`DELETE FROM jobs WHERE run_id = ?`, r.ID); err != nil {
		return fmt.Errorf("delete old jobs for run %d: %w", r.ID, err)
	}

	_, err = tx.Exec(`INSERT OR REPLACE INTO runs (id, workflow_id, workflow_name, name, event, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkflowID, r.WorkflowName, r.Name, r.Event, r.Status, r.Conclusion,
		r.HeadBranch, r.HeadSHA, r.RunAttempt,
		fmtTime(r.CreatedAt), fmtTime(r.StartedAt), fmtTime(r.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert run %d: %w", r.ID, err)
	}

	for j := range d.Jobs {
		_, err := tx.Exec(`INSERT INTO jobs (id, run_id, name, status, conclusion, started_at, completed_at, runner_name, runner_group_name, labels)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.Jobs[j].ID, r.ID, d.Jobs[j].Name, d.Jobs[j].Status, d.Jobs[j].Conclusion,
			fmtTime(d.Jobs[j].StartedAt), fmtTime(d.Jobs[j].CompletedAt),
			d.Jobs[j].RunnerName, d.Jobs[j].RunnerGroupName, strings.Join(d.Jobs[j].Labels, ","),
		)
		if err != nil {
			return fmt.Errorf("insert job %d: %w", d.Jobs[j].ID, err)
		}

		for st := range d.Jobs[j].Steps {
			_, err := tx.Exec(`INSERT INTO steps (job_id, name, number, status, conclusion, started_at, completed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				d.Jobs[j].ID, d.Jobs[j].Steps[st].Name, d.Jobs[j].Steps[st].Number, d.Jobs[j].Steps[st].Status, d.Jobs[j].Steps[st].Conclusion,
				fmtTime(d.Jobs[j].Steps[st].StartedAt), fmtTime(d.Jobs[j].Steps[st].CompletedAt),
			)
			if err != nil {
				return fmt.Errorf("insert step %q for job %d: %w", d.Jobs[j].Steps[st].Name, d.Jobs[j].ID, err)
			}
		}
	}

	return tx.Commit()
}

// SaveRunDetails persists multiple run details in a single transaction
// with prepared statements for efficiency.
func (s *Store) SaveRunDetails(details []model.RunDetail) error {
	if len(details) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // error on deferred close has no actionable caller

	runStmt, err := tx.Prepare(`INSERT OR REPLACE INTO runs (id, workflow_id, workflow_name, name, event, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare run stmt: %w", err)
	}
	defer runStmt.Close() //nolint:errcheck // error on deferred close has no actionable caller

	deleteStepsStmt, err := tx.Prepare(`DELETE FROM steps WHERE job_id IN (SELECT id FROM jobs WHERE run_id = ?)`)
	if err != nil {
		return fmt.Errorf("prepare delete steps stmt: %w", err)
	}
	defer deleteStepsStmt.Close() //nolint:errcheck // error on deferred close has no actionable caller

	deleteJobsStmt, err := tx.Prepare(`DELETE FROM jobs WHERE run_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare delete jobs stmt: %w", err)
	}
	defer deleteJobsStmt.Close() //nolint:errcheck // error on deferred close has no actionable caller

	jobStmt, err := tx.Prepare(`INSERT INTO jobs (id, run_id, name, status, conclusion, started_at, completed_at, runner_name, runner_group_name, labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare job stmt: %w", err)
	}
	defer jobStmt.Close() //nolint:errcheck // error on deferred close has no actionable caller

	stepStmt, err := tx.Prepare(`INSERT INTO steps (job_id, name, number, status, conclusion, started_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare step stmt: %w", err)
	}
	defer stepStmt.Close() //nolint:errcheck // error on deferred close has no actionable caller

	for i := range details {
		d := &details[i]
		r := d.Run

		// Clear existing children before replacing the parent: with foreign_keys ON,
		// INSERT OR REPLACE on runs would otherwise fail when dependent rows exist.
		if _, err := deleteStepsStmt.Exec(r.ID); err != nil {
			return fmt.Errorf("delete old steps for run %d: %w", r.ID, err)
		}
		if _, err := deleteJobsStmt.Exec(r.ID); err != nil {
			return fmt.Errorf("delete old jobs for run %d: %w", r.ID, err)
		}

		if _, err := runStmt.Exec(
			r.ID, r.WorkflowID, r.WorkflowName, r.Name, r.Event, r.Status, r.Conclusion,
			r.HeadBranch, r.HeadSHA, r.RunAttempt,
			fmtTime(r.CreatedAt), fmtTime(r.StartedAt), fmtTime(r.UpdatedAt),
		); err != nil {
			return fmt.Errorf("insert run %d: %w", r.ID, err)
		}

		for j := range d.Jobs {
			job := &d.Jobs[j]
			if _, err := jobStmt.Exec(
				job.ID, r.ID, job.Name, job.Status, job.Conclusion,
				fmtTime(job.StartedAt), fmtTime(job.CompletedAt),
				job.RunnerName, job.RunnerGroupName, strings.Join(job.Labels, ","),
			); err != nil {
				return fmt.Errorf("insert job %d: %w", job.ID, err)
			}

			for st := range job.Steps {
				step := &job.Steps[st]
				if _, err := stepStmt.Exec(
					job.ID, step.Name, step.Number, step.Status, step.Conclusion,
					fmtTime(step.StartedAt), fmtTime(step.CompletedAt),
				); err != nil {
					return fmt.Errorf("insert step %q for job %d: %w", step.Name, job.ID, err)
				}
			}
		}
	}

	return tx.Commit()
}

// RunsSince returns completed runs for a workflow since the given time.
func (s *Store) RunsSince(workflowID int64, since time.Time) ([]model.WorkflowRun, error) {
	rows, err := s.db.Query(`SELECT id, workflow_id, workflow_name, name, event, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at
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
	row := s.db.QueryRow(`SELECT id, workflow_id, workflow_name, name, event, status, conclusion, head_branch, head_sha, run_attempt, created_at, started_at, updated_at
		FROM runs WHERE id = ?`, runID)

	run, err := scanRun(row)
	if err != nil {
		return nil, fmt.Errorf("load run %d: %w", runID, err)
	}

	jobRows, err := s.db.Query(`SELECT id, run_id, name, status, conclusion, started_at, completed_at, runner_name, runner_group_name, labels
		FROM jobs WHERE run_id = ? ORDER BY started_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer jobRows.Close() //nolint:errcheck // error on deferred close has no actionable caller

	var jobs []model.Job
	for jobRows.Next() {
		var j model.Job
		var startStr, compStr, labelsStr string
		if err := jobRows.Scan(&j.ID, &j.RunID, &j.Name, &j.Status, &j.Conclusion, &startStr, &compStr,
			&j.RunnerName, &j.RunnerGroupName, &labelsStr); err != nil {
			return nil, err
		}
		if j.StartedAt, err = parseTime(startStr); err != nil {
			return nil, err
		}
		if j.CompletedAt, err = parseTime(compStr); err != nil {
			return nil, err
		}
		if labelsStr != "" {
			j.Labels = strings.Split(labelsStr, ",")
		}

		steps, err := s.loadSteps(j.ID)
		if err != nil {
			return nil, err
		}
		j.Steps = steps

		jobs = append(jobs, j)
	}
	if err := jobRows.Err(); err != nil {
		return nil, err
	}

	return &model.RunDetail{Run: run, Jobs: jobs}, nil
}

func (s *Store) loadSteps(jobID int64) ([]model.Step, error) {
	rows, err := s.db.Query(`SELECT name, number, status, conclusion, started_at, completed_at
		FROM steps WHERE job_id = ? ORDER BY number ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // error on deferred close has no actionable caller

	var steps []model.Step
	for rows.Next() {
		var st model.Step
		var sStart, sComp string
		if err := rows.Scan(&st.Name, &st.Number, &st.Status, &st.Conclusion, &sStart, &sComp); err != nil {
			return nil, err
		}
		if st.StartedAt, err = parseTime(sStart); err != nil {
			return nil, err
		}
		if st.CompletedAt, err = parseTime(sComp); err != nil {
			return nil, err
		}
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

// LoadRunDetails loads all completed run details for a workflow since the given time.
func (s *Store) LoadRunDetails(workflowID int64, since time.Time) ([]model.RunDetail, error) {
	runs, err := s.RunsSince(workflowID, since)
	if err != nil {
		return nil, err
	}

	details := make([]model.RunDetail, 0, len(runs))
	for i := range runs {
		d, err := s.LoadRunDetail(runs[i].ID)
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
		var err error
		if err := rows.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.Name, &r.Event, &r.Status, &r.Conclusion,
			&r.HeadBranch, &r.HeadSHA, &r.RunAttempt, &createdStr, &startedStr, &updatedStr); err != nil {
			return nil, err
		}
		if r.CreatedAt, err = parseTime(createdStr); err != nil {
			return nil, err
		}
		if r.StartedAt, err = parseTime(startedStr); err != nil {
			return nil, err
		}
		if r.UpdatedAt, err = parseTime(updatedStr); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func scanRun(row *sql.Row) (model.WorkflowRun, error) {
	var r model.WorkflowRun
	var createdStr, startedStr, updatedStr string
	err := row.Scan(&r.ID, &r.WorkflowID, &r.WorkflowName, &r.Name, &r.Event, &r.Status, &r.Conclusion,
		&r.HeadBranch, &r.HeadSHA, &r.RunAttempt, &createdStr, &startedStr, &updatedStr)
	if err != nil {
		return r, err
	}
	if r.CreatedAt, err = parseTime(createdStr); err != nil {
		return r, err
	}
	if r.StartedAt, err = parseTime(startedStr); err != nil {
		return r, err
	}
	if r.UpdatedAt, err = parseTime(updatedStr); err != nil {
		return r, err
	}
	return r, nil
}

const timeFormat = time.RFC3339

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t, nil
}
