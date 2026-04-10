# ci-snitch: Implementation Roadmap

## Context

CLI tool to analyze GitHub Actions CI workflow performance over time. Answers: "are my pipelines getting slower?", "when did this slowdown start?", "was this a hiccup or a trend?", "did my fix actually help?"

Core is real code (no LLM dependency), but output is structured for easy LLM consumption.

## Tech Stack

- Go 1.26 via mise.toml
- Cobra CLI framework
- golangci-lint v2
- testify for testing
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
cmd/smoke/main.go         — manual smoke test against real repos
internal/
  github/                  — go-github client, rate-limit-aware fetcher
  github/testdata/         — golden file API response fixtures
  model/                   — data types (Run, Job, Step, TimeSeries)
  store/                   — SQLite storage layer
  preprocess/              — branch filter, failure exclusion, retry dedup, matrix grouping
  analyze/                 — analysis engine, analyzer interface, AnalysisContext
  stats/                   — shared stats (log-IQR, MAD, CUSUM, Mann-Whitney U)
  output/                  — formatters (table, JSON, markdown)
mise.toml
.golangci.yml
```

## Key Architecture Decisions

1. **`go-github` + `gh auth token`** — real HTTP client with connection pooling and rate limit awareness; auth piggybacks on user's existing `gh` login
2. **SQLite storage** — normalized tables (runs, jobs, steps), only stores completed runs, re-fetches in-progress runs on next invocation
3. **Per-workflow run listing** — uses `actions/workflows/{id}/runs` with sliding date windows to avoid the 1,000-result cap on filtered queries
4. **`*AnalysisContext`** — shared across analyzers, avoiding duplicate sort-and-extract logic
5. **Typed `FindingDetail` interface** — `SummaryDetail`, `OutlierDetail`, `ChangePointDetail` structs; compile-time safe, clean formatting
6. **Mandatory preprocessing** — filter to default branch, exclude failed/cancelled, deduplicate retries (keep latest attempt), group matrix jobs by full key
7. **Log-IQR for outliers** — CI durations are right-skewed; log-transform then IQR. MAD as alternative
8. **CUSUM for change-points** — adaptive thresholds based on local coefficient of variation, not fixed percentages
9. **Partial results with warnings** — fail only if zero runs succeed; warnings to stderr, findings to stdout
10. **Grouping**: per-workflow summary → per-job detail → per-step on drill-down

## PR Roadmap

### PR 1: Scaffolding — DONE
- [x] Go module, Cobra CLI skeleton, mise.toml, CI workflow, golangci-lint v2

### PR 2: Data Model + GitHub Client — Fetch Runs — DONE
- [x] Core model types (Workflow, WorkflowRun, Job, Step, RunDetail, TimeSeries)
- [x] GitHub client with sliding date windows, proactive rate limiting
- [x] Golden file tests with anonymized API responses

### PR 3: GitHub Client — Fetch Jobs & Steps — DONE
- [x] FetchJobs, FetchRunDetails with worker pool (10 concurrent)
- [x] Partial failure handling, null-safe timestamps
- [x] Smoke test (`cmd/smoke/main.go`)

### PR 4: SQLite Storage Layer — DONE
- [x] Normalized schema (runs, jobs, steps), INSERT OR REPLACE upserts
- [x] RunsSince, IncompleteRunIDs, LoadRunDetail, LoadRunDetails

### PR 5: Preprocessing Pipeline — DONE
- [x] FilterByBranch, ExcludeFailures, DeduplicateRetries, GroupMatrixJobs
- [x] Composable Run() pipeline with warnings

### PR 6: Analyzer Framework + Summary Analyzer — DONE
- [x] Analyzer interface, AnalysisContext, Engine, typed FindingDetail
- [x] SummaryAnalyzer (mean/median/p95/p99/min/max per workflow and job)
- [x] `analyze` CLI command with --repo, --branch, --since, --workflow, --no-cache, --include-failures

### PR 7: Outlier Detection — DONE
- [x] stats package (Median, Percentile, IQR, Mean, Stddev)
- [x] LogIQROutliers, MADOutliers
- [x] OutlierAnalyzer with percentile rank and severity levels
- [x] Verified: 1487 runs from a large repo (30 days), 8 outliers from cli/cli (14 days)

### PR 8: Change-Point Detection
- [ ] `internal/stats/cusum.go` — two-sided CUSUM with adaptive thresholds
- [ ] `internal/analyze/changepoint.go` — ChangePointAnalyzer on per-job time series
- [ ] `internal/stats/significance.go` — Mann-Whitney U test
- [ ] Tests: flat-then-jump, gradual drift, step-down, no change, multiple change points

### PR 9: Output Formatters
- [ ] `internal/output/` — Formatter interface, JSON, table, markdown
- [ ] `--format` flag (default `table` for TTY, `json` for pipes)

### PR 10: Polish & UX
- [ ] Progress indicator, graceful errors, `--verbose` flag
- [ ] Human-friendly `--since` (already done: `2w`, `30d`, `3mo`)
- [ ] README with usage and examples
- [ ] Fall back to cached data when rate-limited

## GitHub API Gotchas (must handle)

- **1,000-result cap**: filtered queries silently truncate → sliding date windows
- **In-progress runs**: don't cache as complete → re-fetch on next invocation
- **Cancelled runs**: partial timestamps → exclude from duration stats by default
- **Retries**: `run_attempt > 1` → keep latest only
- **Matrix jobs**: parameters in job name string → parse and group
- **Rate limit**: 5,000/hour REST → proactive sleep before hitting wall
