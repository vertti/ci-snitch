# ci-snitch Roadmap

## Context

ci-snitch analyzes GitHub Actions CI performance: summary stats, outlier detection (Log-IQR/MAD), change point detection (CUSUM + Mann-Whitney U), with table/JSON/markdown output. SQLite cache, clean analyzer interface, solid foundation.

**The gap:** output is information-rich but not decision-rich. Operators ask "what should I do first?" and get a wall of categorized findings instead. Trust in change points is low because the statistics are broken for small samples. Cost dimension is missing entirely. Failure/flakiness analysis doesn't exist.

**North-star questions to answer:**
1. "This build was slow — outlier, drift, or step change?"
2. "Where exactly did the time go?" (job/step attribution)
3. "Did that fix actually stick?" (persistence, not just detection)
4. "Where should I invest effort?" (frequency × cost × improvement potential)
5. "Which pipelines are flaky and wasting reruns?" (reliability + rerun tax)
6. "Give me a dump that an LLM can immediately investigate"
7. "Why is this failing?" (failing step name, not just failure rate)
8. "Is the slowness the job or the queue?" (queue/wait time vs execution time)
9. "Is this getting better or worse?" (failure rate trend direction)

---

## Release 1: Foundation — Trust & Triage

_Focus: fix correctness bugs, reduce cognitive load, make the default output answer "what should I look at first?"_

### ~~1.1 Fix change point job identity key [S] — **bug fix**~~ DONE
- `internal/analyze/changepoint.go:69` keys by `j.Name` alone
- Two workflows with a job named "Unit tests" will have their distributions mixed
- Fix: key by `(workflowName, jobName)` like outlier analyzer already does (outliers.go:76-78)
- **Files:** `internal/analyze/changepoint.go`

### ~~1.2 Fix small-sample Mann-Whitney p-values [M] — **bug fix**~~ DONE
- `internal/stats/significance.go` uses normal approximation, states "valid for n > 20"
- Change point analyzer calls it with after-window of `minSegment=5` runs (changepoint.go:101)
- **Options:**
  - (a) Exact U-test for small samples (enumerate all permutations when min(n1,n2) ≤ 20)
  - (b) Permutation test (sample 10k permutations, compute empirical p-value)
  - (c) Drop p-values for small windows, report effect size + persistence instead
- **Recommendation:** (b) permutation test — straightforward, correct for any sample size, no combinatorial explosion. Fall back to normal approximation when both n > 20.
- **Files:** `internal/stats/significance.go`, `internal/analyze/changepoint.go`

### ~~1.3 Change point persistence & classification [S]~~ DONE
- Currently `afterMean` uses only next `minSegment` points (changepoint.go:101)
- After detecting change at index `i`, compute over all remaining data `durations[i:]`:
  - `PostChangeRuns int` — how many runs since the shift
  - `PostChangeCV float64` — coefficient of variation (stability)
  - `Sustained bool` — no revert detected in the segment
- Classify each change point: **persistent** / **transient** / **inconclusive** (insufficient data)
- Add compact evidence block to output: pre/post window sizes, run count, effect size
- **Files:** `internal/analyze/changepoint.go`, all formatters

### ~~1.4 Volatility scoring [S]~~ DONE
- For each workflow/job, compute `p95/median` ratio as tail-heaviness indicator
- Categorical label: **stable** (<1.3), **variable** (1.3-2.0), **spiky** (2.0-3.0), **volatile** (>3.0) — thresholds configurable
- Add to `SummaryDetail` alongside existing stats
- **Files:** `internal/analyze/summary.go`, all formatters

### ~~1.5 Triage header [S]~~ DONE
- Above the current report, show a compact "top offenders" view:
  - Top 3 by developer wait time (workflow wall-clock)
  - Top 3 by total compute minutes (sum of job durations)
  - Top 3 by volatility score
  - Any active regressions (persistent change points from last N days)
- Operator glances at first screen → knows what to investigate
- **Files:** `internal/output/table.go`, `internal/output/markdown.go`

### ~~1.6 Capture trigger event type [S]~~ DONE
- Add `Event string` to `model.WorkflowRun` (push, pull_request, schedule, workflow_dispatch, etc.)
- Extract from `r.GetEvent()` in `convertRun()` (client.go:275)
- Store in SQLite, surface in summary (runs by trigger type)
- **Files:** `internal/model/model.go`, `internal/github/client.go`, `internal/store/sqlite.go`

---

## Release 2: Reliability & Cost Intelligence

_Focus: quantify flakiness, rerun tax, and CI spend. Answer "where is money going?" and "which pipelines waste developer time with failures?"_

### ~~2.1 Capture runner metadata [S]~~ DONE
- Add `RunnerName`, `RunnerGroupName`, `Labels []string` to `model.Job`
- Extract from go-github's `WorkflowJob` in `convertJob()` (client.go:243) — fields already exist in the library
- Add columns to SQLite `jobs` table
- **Unlocks:** cost estimation in 2.4
- **Files:** `internal/model/model.go`, `internal/github/client.go`, `internal/store/sqlite.go`

### ~~2.2 Failure rate analyzer [M]~~ DONE
- New `internal/analyze/failures.go` implementing `Analyzer`
- `FailureDetail`: failure rate, count, total runs, breakdown by conclusion type (failure/cancelled/timed_out/skipped), recent streak length, trend direction
- Needs unfiltered data → add `AllDetails []model.RunDetail` to `AnalysisContext` (analyzer.go:17)
- Feed both filtered and unfiltered data from `cmd/ci-snitch/analyze.go`
- **Files:** `internal/analyze/failures.go` (new), `internal/analyze/analyzer.go`, `cmd/ci-snitch/analyze.go`, all formatters

### ~~2.3 Rerun tax tracking [S]~~ DONE
- Currently `DeduplicateRetries()` keeps latest attempt per run ID — good for duration stats, but loses "how many reruns happened"
- Before deduplication, compute per-run-ID: max `RunAttempt`, and flag runs with attempts > 1
- Surface rerun rate per workflow and total rerun cost (extra minutes wasted on failed-then-retried runs)
- Combine with failure analyzer to identify "most expensive flaky workflows"
- **Files:** `internal/preprocess/filter.go`, `internal/analyze/failures.go`

### ~~2.4 Cost model & billable minutes estimation [M]~~ DONE
- New `internal/cost/model.go`: runner label → cost multiplier mapping
- Default multipliers from GitHub's published rates (ubuntu=1x, macos=10x, windows=2x, larger runners by label pattern)
- Apply GitHub's rounding rule: job minutes rounded up to nearest whole minute
- User-overridable via `--cost-config costs.yaml` for self-hosted/custom pricing
- New `internal/analyze/cost.go` implementing `Analyzer`
- `CostDetail`: raw minutes, billable minutes, estimated cost, runs/day, daily cost
- **Files:** `internal/cost/` (new package), `internal/analyze/cost.go` (new)

### ~~2.5 Prioritization score — "bang for buck" [S]~~ DONE
- Composite: `runs_per_day × median_duration × cost_multiplier × improvement_potential`
- `improvement_potential` = p95/median ratio (high = lots of room to optimize)
- Ranked list with estimated daily savings if median brought to p25
- Integrate into triage header from 1.5
- **Files:** `internal/analyze/cost.go` or `internal/analyze/priority.go` (new)

### ~~2.6 Wire up matrix job grouping [S]~~ DONE
- `GroupMatrixJobs` / `ParseMatrixJobName` exist in `preprocess/filter.go:101-150` but are unused
- Use in summary analyzer: group matrix variants under base name
- Show grouped aggregate; per-variant in verbose mode
- **Files:** `internal/analyze/summary.go`, `internal/preprocess/filter.go`

---

## Release 2.5: Measurement Correctness

_Focus: fix measurement inaccuracies that inflate failure rates, misattribute costs, and produce misleading priority scores. Each fix is independently shippable._

### 2.5.1 Exclude `cancelled` from failure rate [S] — **measurement fix**
- `internal/analyze/failures.go:56` counts anything not `success`/`skipped` as a failure
- `cancelled` runs (developer-initiated cancellations of superseded runs) inflate failure rates significantly in active repos
- Only count `conclusion == "failure"` and `conclusion == "timed_out"` as failures
- Track `cancelled` separately: add `CancellationCount`/`CancellationRate` to `FailureDetail`
- Keep `ByConclusion` map unchanged for full breakdown
- Formatters show cancellation rate alongside failure rate when non-zero
- **Files:** `internal/analyze/failures.go`, `internal/output/table.go`

### 2.5.2 Detect self-hosted runners and expand cost model [S] — **measurement fix**
- `internal/cost/model.go` only maps standard runner labels; self-hosted runners are billed at 1x Linux instead of $0
- Larger GitHub-hosted runners (e.g., `ubuntu-latest-16-cores` = 8x) also missing
- Add `IsSelfHosted(labels []string) bool` — returns true if any label is `"self-hosted"`
- Self-hosted → multiplier 0.0; track `SelfHostedMinutes` separately in `CostDetail`
- Add larger runner multipliers: pattern-match on `-N-cores` suffix per GitHub docs
- **Files:** `internal/cost/model.go`, `internal/analyze/cost.go`

### 2.5.3 Use max(job.CompletedAt) for workflow duration [M] — **measurement fix**
- `WorkflowRun.Duration()` uses `UpdatedAt - StartedAt`; `UpdatedAt` can be bumped by post-completion events (annotations, deployment statuses), inflating durations and creating false outliers
- Add `func (rd RunDetail) Duration() time.Duration`:
  1. Compute `max(job.CompletedAt)` across all jobs
  2. Return `maxCompletedAt - StartedAt` if valid
  3. Fall back to `WorkflowRun.Duration()` when no jobs have completion times
- Update callers: `summary.go:64`, `cost.go:65`, `outliers.go:59` — use `d.Duration()` instead of `d.Run.Duration()`
- Keep `WorkflowRun.Duration()` as-is for backward compat
- **Files:** `internal/model/model.go`, `internal/analyze/summary.go`, `internal/analyze/cost.go`, `internal/analyze/outliers.go`

### 2.5.4 Compute priority score from billable durations [S] — **measurement fix**
- `internal/analyze/cost.go:128-138` mixes wall-clock variability ratio (p95/median) with billable totals (includes OS multipliers and per-job rounding) — inconsistent units for parallel-job workflows
- Replace `wfDurations` (wall-clock) with per-run billable sums: for each run, sum `BillableMinutes(job) × multiplier` across all jobs
- Use billable-based median/p95/p25 for priority score and daily savings estimate
- Depends on 2.5.3 being merged first
- **Files:** `internal/analyze/cost.go`

### Implementation order
1. **2.5.1** (cancelled exclusion) — no dependencies
2. **2.5.3** (max job CompletedAt) — no dependencies, but 2.5.4 depends on it
3. **2.5.2** (self-hosted runners) — no dependencies
4. **2.5.4** (billable priority score) — after 2.5.3

---

## Release 3: The "Why" Dimension & LLM Integration

_Focus: answer "why is this slow/failing?", add step-level visibility, and make output LLM-ready. Validated by real-world LLM analysis feedback: the tool's detection is strong but lacks the "why" to make findings actionable without manual investigation._

### ~~3.1 LLM-optimized output format [M]~~ DONE
- New `internal/output/llm.go`, registered as `--format llm`
- Designed for copy-paste to Claude Code — narrative + structured data

### ~~3.2 Step-level timing analyzer [M]~~ DONE
- Steps are already fetched and stored in SQLite but never analyzed
- New `internal/analyze/steps.go` implementing `Analyzer`
- Per job: identify top 3 slowest steps by median duration, flag steps with high variance
- Transforms "job is slow" into "Docker build step is slow" — critical for actionability
- Surface in all formatters: nested under job in summary, inline in LLM output
- **Files:** `internal/analyze/steps.go` (new), formatters

### ~~3.3 Failing step attribution [S]~~ DONE
- When a job fails, identify which step has `conclusion: failure`
- Add `FailingSteps []FailingStep` to `FailureDetail` with step name, conclusion, frequency
- Transforms "95% failure rate" into "95% failure rate — fails at 'Run integration tests'"
- Data already available in `model.Job.Steps` — just needs analysis
- **Files:** `internal/analyze/failures.go`, formatters

### ~~3.4 Queue/wait time analysis [S]~~ DONE
- `CreatedAt` to `StartedAt` gap = runner wait time (queued, waiting for runner)
- Surface as separate metric in summary: median queue time, p95 queue time per workflow
- Distinguishes "slow job" from "no runners available" — different root causes, different fixes
- Data already in `model.WorkflowRun` — just needs extraction
- **Files:** `internal/analyze/summary.go`, formatters

### ~~3.5 Compact LLM mode — reduce noise [S]~~ DONE
- Real-world test: 54k token report → ~5k useful tokens. Most JSON is oscillating/minor changepoints
- `--format llm` should omit oscillating and minor changepoints from embedded JSON
- Only include actionable findings (regressions, speedups, high-severity outliers, failure details)
- Keep the narrative section unchanged (already well-filtered by postprocessor)
- **Files:** `internal/output/llm.go`

### 3.6 Raw JSON to separate file [S]
- `--raw-output report.json` writes full JSON to file instead of embedding in LLM output
- LLM reads the narrative; only fetches JSON if it needs to dig deeper
- Keeps the context window clean while preserving full data access
- **Files:** `cmd/ci-snitch/analyze.go`, `internal/output/llm.go`

### 3.7 LLM explain mode [M]
- `--format llm --run-id X` or `--commit SHA`: focused on a single incident
- Shows: this run's timing vs baseline, which jobs/steps were slow, step-level attribution, recent change points affecting those jobs
- Minimal output, maximum context density for LLM reasoning

### 3.8 `ci-snitch compare` subcommand [M]
- Compare two time periods: `--before 7d --after 7d`
- Compare two branches: `--base main --head feature-x`
- Runs analysis engine twice, diffs results
- Shows: what improved, degraded, new, gone — with significance tests
- Supports all output formats
- **Files:** new subcommand in `cmd/ci-snitch/compare.go`, new `internal/analyze/compare.go`

### 3.9 Failure rate trend [S]
- Add directional indicator: "23% and improving" vs "23% and getting worse"
- Compare recent window (last 7d) failure rate against full-period rate
- Simple but changes the urgency of findings significantly
- **Files:** `internal/analyze/failures.go`

### ~~3.10 Outlier-resistant changepoint detection [M]~~ DONE
- Real case: single 38-min outlier in a 10-min deploy job caused false "persistent regression" with p=0.0000
- Pre-filter: clamp extreme values via IQR fences (Q3 + 4×IQR) before CUSUM — prevents single outliers from poisoning detection
- Post-detection robustness: compute overlap ratio (fraction of after-points within before IQR). High overlap (>50%) → downgrade to minor
- CUSUM detects on clamped data; Mann-Whitney test and reported means use raw data for accuracy
- **Files:** `internal/stats/outlier.go`, `internal/analyze/changepoint.go`, `internal/analyze/postprocess.go`

### 3.11 Systematic vs flaky failure classification [S]
- When 100% of failures hit the same step (e.g. "Run Code Review" 115/115), label as `[SYSTEMATIC]` not `[FLAKY]`
- Flaky implies random; systematic means one deterministic bug — different urgency and investigation approach
- Threshold: >90% same root-cause step → systematic; otherwise flaky
- Builds on 3.3 failing step attribution + cascade filtering
- **Files:** `internal/analyze/failures.go`, `internal/output/llm.go`

### 3.12 Failure clustering by category [M]
- Group failing steps into categories: infra (step name contains "Setup"/"runner"), build ("Compile"/"Lint"/"Build"), test ("test"/"E2E")
- Headline becomes: "23% failure rate: 40% infra, 35% build, 25% test failures"
- Even simple heuristics on step names give much better signal than a flat list
- **Files:** `internal/analyze/failures.go`, `internal/output/llm.go`

### 3.13 Drift detection (separate from step-change) [M]
- CUSUM targets step-like mean shifts; gradual drift is a different phenomenon
- Add linear regression over sliding windows to detect steady trends
- Different operator guidance: "pipeline gradually slowing — look for repo growth, cache degradation, dependency bloat" vs "step change at commit X"
- **Files:** `internal/stats/drift.go` (new), `internal/analyze/changepoint.go` or new analyzer

### Implementation priority
1. ~~**3.2** (step timing) + **3.3** (failing step)~~ DONE
2. ~~**3.4** (queue time) + **3.5** (compact LLM) + **3.10** (outlier-resistant changepoints)~~ DONE
3. **3.11** (systematic vs flaky) + **3.12** (failure clustering) — next: improve failure signal quality
4. **3.6** (raw output) + **3.9** (failure trend) — small but valuable
5. **3.7** (explain) + **3.8** (compare) + **3.13** (drift) — larger features

---

## Release 4: Interactive TUI

_Depends on Releases 1-3 for data richness. A TUI over today's data would be underwhelming; a TUI over data with failure rates, cost, volatility, and trustworthy change points is genuinely useful._

### 4.1 TUI foundation & navigation [L]
- New subcommand: `ci-snitch tui --repo owner/repo [--since 30d]`
- **Dependencies:** bubbletea v2, lipgloss v2, bubbles
- New package `internal/tui/`
- Layout: workflow list → job list → job detail → step detail
- Inline summary stats in list views (median, p95, failure rate, volatility label, cost rank)
- Keyboard: j/k, enter, esc/backspace, /, q
- Reads from SQLite cache (analyze populates it; optionally auto-fetch on launch)

### 4.2 Duration sparklines & timeline charts [M]
- Unicode block character sparklines inline in list views (compact trend at a glance)
- Full-width scatter plot in detail view: duration over time, change point markers, outlier highlights
- Wire up `model.TimeSeries` (defined in `internal/model/timeseries.go`, currently unused)

### 4.3 Cost breakdown view [S]
- Horizontal stacked bar: proportional CI time/cost per workflow
- Color-coded by runner type
- Sortable by total cost, cost-per-run, improvement potential
- Terminal-adapted "pie chart"

### 4.4 "Explain this run" [M]
- Select any run in the TUI → see which jobs/steps were unusually slow vs their baselines
- Step-level timing already in SQLite; compare each step duration to its historical distribution
- Highlight the critical path and which steps deviated most

### 4.5 Findings browser [S]
- Filterable list of all findings (outliers, change points, failures, cost)
- Filter by severity, type, workflow, date range
- Jump to related workflow/job detail from any finding

---

## Release 5: Scale & CI Integration

### 5.1 Multi-repo config [M]
- Config file listing repos (optional team/owner grouping)
- Cross-repo triage: top offenders across the organization
- Shared SQLite or per-repo databases with aggregate queries

### 5.2 PR comment bot [M]
- `ci-snitch report --repo R --pr 123` posts markdown comparison on PR
- Uses `compare` logic (PR branch vs base branch)
- Ship as reusable GitHub Action

### 5.3 Workflow config diff at change points [S]
- When a changepoint is detected, use GitHub API `git diff --name-only` for that commit
- Check if `.github/workflows/*.yml` changed — if yes, label as "CI config change" vs "application code change"
- Cheap to implement: commit SHA is already captured, just need one API call per regression
- Would have immediately explained real-world regressions (commit was code change, not CI)
- **Files:** `internal/github/client.go`, `internal/analyze/changepoint.go`, formatters

### 5.4 Reusable workflow call chain dedup [M]
- `deploy-test.yml` calls `tests.yml` via `workflow_call` — findings are duplicated across both
- Detect call chains from workflow YAML `jobs.*.uses` fields
- Attribute findings to the leaf workflow, suppress duplicates from callers
- **Files:** `internal/github/client.go` (fetch workflow YAML), `internal/preprocess/`

### 5.5 Branch-aware failure analysis [S]
- PR failures (expected during development) vs main failures (critical) are very different signals
- Default: weight main-branch failures higher in severity, or separate failure rates by branch category
- Extends `--branch` flag with `--branch-category pr|main|all`
- **Files:** `internal/analyze/failures.go`, `internal/preprocess/filter.go`

### 5.6 Regression commit diff attribution [S]
- Change point detection already captures the commit SHA
- Fetch `git diff --stat` via GitHub API for that commit to show what files changed
- Saves the user a manual `git show` round-trip per regression
- **Files:** `internal/github/client.go`, `internal/analyze/changepoint.go`, formatters

---

## Key Files Reference

| File | What Changes | Releases |
|------|-------------|----------|
| `internal/analyze/changepoint.go` | Job identity fix, persistence, drift | 1.1, 1.2, 1.3 |
| `internal/stats/significance.go` | Permutation test for small samples | 1.2 |
| `internal/analyze/summary.go` | Volatility scoring, matrix grouping | 1.4, 2.6 |
| `internal/output/table.go` | Triage header, volatility labels, cost | 1.5, 2.x |
| `internal/model/model.go` | Runner fields, event type | 1.6, 2.1 |
| `internal/github/client.go` | Extract runner info, event | 1.6, 2.1 |
| `internal/store/sqlite.go` | Schema migration for new columns | 1.6, 2.1 |
| `internal/analyze/analyzer.go` | `AllDetails` on AnalysisContext | 2.2 |
| `internal/analyze/failures.go` | New analyzer, cancel exclusion | 2.2, 2.3, 2.5.1 |
| `internal/cost/model.go` | New package, self-hosted detection | 2.4, 2.5.2 |
| `internal/analyze/cost.go` | New analyzer, billable priority | 2.4, 2.5, 2.5.2, 2.5.4 |
| `internal/analyze/steps.go` | New step-level analyzer | 3.2 |
| `internal/output/llm.go` | New formatter, compact mode, raw output | 3.1, 3.5, 3.6, 3.7 |
| `cmd/ci-snitch/compare.go` | New subcommand | 3.8 |
| `internal/tui/` | New package (bubbletea) | 4.x |

## Versioning

Tag a new minor version after each PR merge to main. Every PR delivers value, so every merge is a release. Semver: bump minor for new features/analyzers, patch for bug fixes.

## Verification

Each PR:
1. `mise run check` (fmt + lint + test)
2. `go run ./cmd/smoke` — update smoke test to exercise new features
3. `./bin/ci-snitch analyze cli/cli --since 7d` — verify output
4. For new analyzers: golden file tests in `internal/*/testdata/` with anonymized data
5. For TUI: manual interactive testing
