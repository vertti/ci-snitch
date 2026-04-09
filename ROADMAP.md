# ci-snitch: Implementation Roadmap

## Context

CLI tool to analyze GitHub Actions CI workflow performance over time. Answers: "are my pipelines getting slower?", "when did this slowdown start?", "was this a hiccup or a trend?", "did my fix actually help?"

Core is real code (no LLM dependency), but output is structured for easy LLM consumption.

**Test repo:** `<your-org/your-repo>` (22 workflows, long history)

## Tech Stack

- Go 1.26 via mise.toml
- Cobra CLI framework
- golangci-lint v2
- testify for testing
- gonum for statistics
- `go-github` library for API calls (not shelling out to `gh` — see rationale below)
- `gh auth token` for auth token discovery only (single shell-out at startup)
- `modernc.org/sqlite` for local storage (pure Go, no CGO)

### Why go-github over gh CLI
A busy repo can have ~13,000+ runs in 2 months. Each run needs a separate jobs API call → ~15,000 total requests. Shelling out to `gh` for each means: no connection pooling, process spawn overhead (~10-30ms each), no proactive rate limiting (can't read `X-RateLimit-Remaining` headers), and `--paginate` silently stops at GitHub's 1,000-result cap. `go-github` gives us persistent HTTP connections, typed rate limit errors, and direct header access.

### Why SQLite over file cache
13,000+ cached run files is unwieldy. SQLite gives us: single-file portability, indexed queries for cross-run analysis, atomic writes, and `INSERT OR IGNORE` for idempotent updates. `modernc.org/sqlite` is pure Go — no CGO, cross-compiles cleanly.

## Project Layout

```
cmd/ci-snitch/main.go
internal/
  github/        — go-github client, rate-limit-aware fetcher
  github/testdata/ — golden file API response fixtures
  model/         — data types (Run, Job, Step, TimeSeries)
  store/         — SQLite storage layer
  preprocess/    — branch filter, failure exclusion, retry dedup, matrix grouping
  analyze/       — analysis engine, analyzer interface, AnalysisContext
  analyze/summary.go
  analyze/outliers.go
  analyze/changepoint.go
  stats/         — shared stats (log-IQR, MAD, CUSUM, Mann-Whitney U, bootstrap CI)
  output/        — formatters (table, JSON, markdown)
mise.toml
.golangci.yml
```

## Key Architecture Decisions

1. **`go-github` + `gh auth token`** — real HTTP client with connection pooling and rate limit awareness; auth piggybacks on user's existing `gh` login
2. **SQLite storage** — normalized tables (runs, jobs, steps), only stores completed runs, re-fetches in-progress runs on next invocation
3. **Per-workflow run listing** — uses `actions/workflows/{id}/runs` with sliding date windows to avoid the 1,000-result cap on filtered queries
4. **`*AnalysisContext`** — lazy-computed derived views (time series, step series) shared across analyzers, avoiding duplicate sort-and-extract logic
5. **Typed `FindingDetail` interface** — `SummaryDetail`, `OutlierDetail`, `ChangePointDetail` structs instead of `map[string]any`; compile-time safe, clean formatting
6. **Mandatory preprocessing** — filter to default branch, exclude failed/cancelled, deduplicate retries (keep latest attempt), group matrix jobs by full key. Without this, all stats are contaminated
7. **Log-IQR for outliers** — CI durations are right-skewed; log-transform then IQR. MAD as alternative. No raw z-score
8. **CUSUM for change-points** — adaptive thresholds based on local coefficient of variation, not fixed percentages
9. **Partial results with warnings** — fail only if zero runs succeed; warnings to stderr, findings to stdout
10. **Grouping**: per-workflow summary → per-job detail → per-step on drill-down. Auto-surface "interesting" items (change points, top duration, top variance)

## PR Roadmap

### PR 1: Scaffolding
- [ ] **Status: not started**

**`Initialize Go module, Cobra skeleton, mise.toml, CI, linter`**

- `mise.toml` — Go 1.26, golangci-lint as dev deps, tasks: `build`, `test`, `lint`
- `go.mod` (module `github.com/vertti/ci-snitch`)
- `cmd/ci-snitch/main.go` — Cobra root command with `--version`
- `.golangci.yml` — golangci-lint v2 config
- `.github/workflows/ci.yml` — lint + test using mise
- **Tests:** root command executes, `--version` prints
- **Verify:** `mise install && mise run lint && mise run test && mise run build`

### PR 2: Data Model + GitHub Client — Fetch Runs
- [ ] **Status: not started**

**`Add data model and GitHub client to fetch paginated workflow runs`**

- `internal/model/model.go` — `Workflow`, `WorkflowRun`, `Job`, `Step`, `RunDetail`
- `internal/model/timeseries.go` — `TimeSeries`, `TimePoint`
- `internal/github/auth.go` — get token via `gh auth token`
- `internal/github/client.go` — `Client` wrapping `go-github`, `ListWorkflows()`, `FetchRuns(ctx, workflowID, since, branch)`
- Per-workflow queries with sliding date windows to avoid 1,000-result cap
- Proactive rate limiting via `Response.Rate.Remaining`
- `internal/github/testdata/` — golden file fixtures captured from real API responses
- **Tests:** golden file parsing tests, sliding window logic, rate limit handling, error cases
- **Verify:** manual call against `<your-org/your-repo>`, print run count

### PR 3: GitHub Client — Fetch Jobs & Steps
- [ ] **Status: not started**

**`Add job and step fetching with bounded concurrency and partial failure handling`**

- `internal/github/client.go` additions: `FetchJobs(ctx, runID)`, `FetchRunDetails(ctx, runs)`
- Worker pool pattern (default 10 workers) reading from work channel
- Partial failure: collect warnings, continue with successful fetches
- Handle data gotchas: null timestamps on cancelled/skipped steps, `run_attempt > 1` dedup
- More golden file fixtures for jobs/steps responses
- **Tests:** golden file parsing, worker pool bounds, partial failure aggregation, cancelled run handling
- **Verify:** fetch + hydrate a small workflow from test repo, print step-level timings

### PR 4: SQLite Storage Layer
- [ ] **Status: not started**

**`Add SQLite storage for runs, jobs, and steps`**

- `internal/store/sqlite.go` — schema: `runs`, `jobs`, `steps` tables with indexes on `(workflow_id, created_at)`
- `INSERT OR IGNORE` for idempotent writes
- Re-fetch logic: mark in-progress runs, re-query on next invocation
- Query methods: `RunsSince(workflowID, since)`, `JobsForRun(runID)`, etc.
- `--no-cache` flag support (bypass store, fetch fresh)
- **Tests:** write/read round-trip, upsert idempotency, re-fetch in-progress runs, query filtering
- **Verify:** first fetch populates DB, second fetch is fast (only new runs)

### PR 5: Preprocessing Pipeline
- [ ] **Status: not started**

**`Add mandatory preprocessing: branch filter, failure exclusion, retry dedup, matrix grouping`**

- `internal/preprocess/filter.go`:
  - `FilterByBranch(runs, branch)` — default: repo's default branch
  - `ExcludeFailures(runs)` — keep only `conclusion: success` by default
  - `DeduplicateRetries(runs)` — keep latest `run_attempt` per `run_id`
  - `GroupMatrixJobs(runs)` — group by full matrix key parsed from job name
- Pipeline function: `Preprocess(runs, opts) ([]RunDetail, []Warning)`
- **Tests:** each filter independently with synthetic data, combined pipeline, edge cases (all failures, no retries, etc.)

### PR 6: Analyzer Framework + Summary Analyzer
- [ ] **Status: not started**

**`Add Analyzer interface, AnalysisContext, Engine, and summary stats analyzer`**

- `internal/analyze/context.go` — `AnalysisContext` with lazy `TimeSeries()` and `StepTimeSeries(job, step)`
- `internal/analyze/analyzer.go` — `Analyzer` interface, typed `Finding` with `FindingDetail` interface
- `internal/analyze/engine.go` — `Engine` runs analyzers sequentially, returns `AnalysisResult{Findings, Warnings, Meta}`
- `internal/analyze/finding.go` — `SummaryDetail` struct (mean, median, p95, p99, total runs, success rate)
- `internal/analyze/summary.go` — `SummaryAnalyzer`: per-workflow and per-job stats
- `cmd/ci-snitch/analyze.go` — `analyze` subcommand: `--repo`, `--branch`, `--since`, `--workflow`, wires fetch → store → preprocess → engine
- Default output: JSON to stdout
- **Tests:** canned `AnalysisContext` → verify summary stats; engine collects findings from multiple analyzers
- **Verify:** `go run ./cmd/ci-snitch analyze --repo <your-org/your-repo>` outputs JSON summary

### PR 7: Outlier Detection
- [ ] **Status: not started**

**`Add log-IQR and MAD outlier detection`**

- `internal/stats/outlier.go` — `LogIQR(data)` returns upper/lower fences, `MAD(data)` returns modified z-scores
- `internal/stats/helpers.go` — `Median`, `Percentile`, `IQR` (using gonum where beneficial)
- `internal/analyze/outliers.go` — `OutlierAnalyzer`: log-IQR by default, MAD as alternative
- `internal/analyze/finding.go` addition: `OutlierDetail` struct (expected range, actual value, percentile rank, commit SHA)
- Reports percentile rank for each flagged run (e.g., "p97 — slower than 97% of recent runs")
- **Tests:** synthetic log-normal data with planted outliers, right-skewed distributions, edge cases (<5 runs, identical values)

### PR 8: Change-Point Detection
- [ ] **Status: not started**

**`Add CUSUM change-point detection for slowdowns and speedups`**

- `internal/stats/cusum.go` — two-sided CUSUM with adaptive thresholds (slack = 0.5*stddev, threshold = 4*stddev, scaled by local CV)
- `internal/analyze/changepoint.go` — `ChangePointAnalyzer`: runs CUSUM on each job's time series
- `internal/analyze/finding.go` addition: `ChangePointDetail` struct (change index, before/after mean, confidence, % change)
- `internal/stats/significance.go` — Mann-Whitney U test to confirm change-point significance
- **Tests:** flat-then-jump, gradual drift, step-down (speedup), no change, multiple change points, high-variance noisy data

### PR 9: Output Formatters
- [ ] **Status: not started**

**`Add pluggable output formatters: table, JSON, markdown`**

- `internal/output/formatter.go` — `Formatter` interface
- `internal/output/json.go` — indented JSON with discriminator field for `FindingDetail` types
- `internal/output/table.go` — human-readable table via `text/tabwriter`, type-switches on `FindingDetail`
- `internal/output/markdown.go` — markdown report for GitHub issues/PR comments
- `--format` flag on analyze command (default `table` for TTY, `json` for pipes)
- **Tests:** round-trip JSON, snapshot tests for table/markdown output
- **Verify:** `--format table` and `--format markdown` against test repo

### PR 10: Polish & UX
- [ ] **Status: not started**

**`Progress indicator, error handling, human-friendly dates, README`**

- Progress output to stderr during fetch (`Fetching runs... 142 found`, `Hydrating jobs... 42/142`)
- Graceful errors: `gh` not installed, not authenticated, repo not found, no runs in range
- `--verbose` / `-v` flag for debug logging
- `--since` accepts `2w`, `30d`, `3mo` in addition to `YYYY-MM-DD`
- `--include-failures` flag to include failed runs in duration analysis
- Auto-surface top findings: top 3 by duration, top 3 by variance, any detected change points
- README with usage, examples, installation
- **Tests:** error scenarios, duration format parsing

## Dependency Graph

```
PR1 (scaffold)
 └─ PR2 (model + fetch runs)
     └─ PR3 (fetch jobs/steps)
         ├─ PR4 (SQLite store)
         │   └─ PR5 (preprocessing)
         │       └─ PR6 (analyzer framework + summary)
         │           ├─ PR7 (outliers)     ─┐
         │           ├─ PR8 (change-points) ├─ independent
         │           └─ PR9 (formatters)   ─┘
         │               └─ PR10 (polish)
```

PRs 7, 8, 9 are independent after PR6.

## GitHub API Gotchas (must handle)

- **1,000-result cap**: filtered queries silently truncate. Use per-workflow queries + sliding date windows, check `total_count` vs actual results
- **In-progress runs**: don't cache as complete; re-fetch on next invocation
- **Cancelled runs**: partial timestamps, exclude from duration stats by default
- **Retries**: `run_attempt > 1` means earlier attempts exist; keep latest only
- **Matrix jobs**: parameters embedded in job name string, no structured field; parse and group
- **Rate limit**: 5,000/hour REST. Read `Response.Rate.Remaining` on every call, sleep proactively before hitting wall

## Verification

After each PR, run against real repo:
```bash
mise install
mise run test
mise run lint
go run ./cmd/ci-snitch analyze --repo <your-org/your-repo> --format table --since 30d
```
