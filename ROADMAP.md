# ci-snitch Roadmap

## Status

Rewritten 2026-07-15 after a full-project review (analysis core, data layer, CLI/output, tests/CI/infra, plus an audit of the previous roadmap). Of the previous roadmap's 18 items, one shipped (7.1 typed constants), one half-shipped (3.1: `PRAGMA foreign_keys` landed; CASCADE + single-delete did not). Everything else carries forward, renumbered below alongside the new findings.

Verdict: the architecture is sound and needs no extraction work. The gaps are (1) **trust** — correctness bugs in the data layer and analyzers that skew the numbers users act on, (2) **regression protection** — the production GraphQL path and the orchestration service have zero test coverage, (3) CLI paper cuts. The priority order reflects that.

Coverage snapshot (2026-07-15): preprocess 98%, stats 97%, cost 94%, model 93%, analyze 82%, store 73%, output 68%, github 42%, cmd/ci-snitch 31%, app 0% in-package (61% incidentally via cmd tests), diag/system/smoke 0%. `internal/github/graphql.go` — the production hot path — has 0% coverage.

Item IDs: **D** data integrity, **A** analysis correctness, **T** tests, **U** CLI/UX, **P** performance, **F** feature depth, **H** housekeeping. `(was N.N)` marks carryovers from the previous roadmap.

## Current architecture

```
cmd/ci-snitch/      CLI: flag parsing, dependency wiring, format selection
internal/app/       Orchestrates fetch → preprocess → analyze pipeline
internal/github/    REST + GraphQL client; sliding-window run listing; batched hydration
internal/store/     SQLite cache (WAL, busy_timeout, prepared-statement batching)
internal/preprocess/Branch filter, dedup, rerun stat extraction, matrix grouping
internal/analyze/   Analyzers + post-processing. Engine runs analyzers sequentially
internal/cost/      Runner-label → multiplier model (default GitHub rates; self-hosted; larger runners)
internal/stats/     Outlier (log-IQR/MAD), CUSUM change-points, Mann-Whitney U
internal/diag/      Unified Diagnostic{Severity, Kind, Scope, Message, Err}
internal/output/    Formatters: table, JSON, markdown, llm
internal/system/    Subprocess helper with timeouts
internal/model/     Domain types; canonical RunDetail.Duration() (uses max job CompletedAt)
```

Analyzers in default order: `summary, steps, pipeline, runner, outlier, changepoint, failure, cost`.

## Verified still correct (do not redo)

- `RunDetail.Duration()` fallback logic; dedup/rerun-stat ordering in `Service.Run`; matrix grouping and per-(workflow, job) keying inside the changepoint analyzer; `ClampOutliers` before CUSUM; the three-regime Mann-Whitney strategy (exact/permutation/normal).
- `parseSince` handles all documented forms (`7d`, `2w`, `3mo`, dates) with good errors and tests.
- ANSI never leaks into pipes; progress goes to stderr only; JSON output cannot emit NaN/Inf (divisions zero-guarded); window-boundary duplicate runs are deduped by ID before analysis.
- Tokens never appear in logs or error bodies (GraphQL error strings truncated to 200 bytes).
- install.sh checksum verification; CI hygiene (SHA-pinned actions, `persist-credentials: false`, scoped permissions, `environment: release` gating the tap token); renovate config; all `nolint`s carry enforced justifications; test fixtures properly anonymized.

Removed from the old "already correct" list — disproven by this review:

- ~~"Diagnostics embedded in JSON and LLM output"~~ — only analyzer-produced diagnostics are; fetch/hydration/preprocess warnings never reach `result.Diagnostics` (D3).
- ~~"FK enforcement shipped"~~ — the pragma binds to a single pooled connection; other pool connections run without it (D1).
- ~~"Rate limit budget with safety margin"~~ — the budget audits the REST pool while hydration spends GraphQL points (D6).

---

## D — Data integrity (highest priority: make the cache and fetch trustworthy)

### D1. Apply SQLite pragmas to every pooled connection [S] ✅ done (completes 3.1)
- Shipped 2026-07-15: pragmas moved to DSN `_pragma` params so the driver applies `busy_timeout`/`journal_mode(WAL)`/`foreign_keys` to every pooled connection. Regression tests hold two pooled connections and assert all three pragmas, verify FK enforcement on the fresh connection, and reproduce the concurrent-writer `SQLITE_BUSY` failure (8 goroutines × 40 batched saves).
- Decision — no `ON DELETE CASCADE` (drops the 3.1 remainder for good): with `INSERT OR REPLACE` semantics the explicit child deletes must remain for pre-CASCADE databases anyway, so adding CASCADE to the schema would only create fresh-vs-existing schema divergence without removing any code.

### D2. Invalidate cached runs that were re-run [S]
- Cache partitioning checks only ID membership (`internal/app/service.go:328-336`), never `UpdatedAt`/`RunAttempt`. A run cached as attempt 1 (failure) then re-run to attempt 2 (success) serves the stale attempt-1 row forever — wrong conclusion, jobs, duration; rerun stats never see attempt 2. The `IncompleteRunIDs` guard defends an impossible case (only completed runs are ever saved; `internal/github/client.go:161`).
- Fix: make `cachedSet` a `map[int64]time.Time` from `RunsSince` and re-fetch when the listing's `UpdatedAt` (or `RunAttempt`) is newer than the cached one.
- **Files:** `internal/app/service.go`, `internal/store/sqlite.go`, test in new `internal/app/service_test.go` (T1)

### D3. Surface fetch/hydration diagnostics in `result.Diagnostics` [S]
- The 1000-cap `KindPartialData` warnings and failed-hydration warnings are only `Prog.Log`'d to stderr (`internal/app/service.go:195-197,353-355`); preprocess warnings are verbose-only (`:121-125`). `--format json`/`llm` consumers cannot tell the dataset was truncated. Found independently by two reviewers.
- Fix: collect warnings from `fetchRunLists`/`hydrateWorkflow`/preprocess and append to `result.Diagnostics`. While here: the 1000-cap warning appends once **per page** (`internal/github/client.go:177-183`) — aggregate to one per window.
- **Files:** `internal/app/service.go`, `internal/github/client.go`

### D4. No silent drops in GraphQL batch parsing [XS–S]
- `parseBatchResponse`: top-level unmarshal error returns `nil, nil` — a whole 20-run batch vanishes without diagnostics; a missing alias key is `continue`d silently (`internal/github/graphql.go:171-174,184-187`). Un-cached, the runs also inflate the uncached count every subsequent scan.
- Fix: emit `KindPartialData` warnings on both paths; optionally fall back to REST for unaccounted runs (mirroring the existing error fallback).
- **Files:** `internal/github/graphql.go`, tests via T2

### D5. GraphQL truncation diagnostic — and don't cache truncated runs [S] (was 3.2)
- `buildBatchQuery` requests `checkRuns(first:50)`/`steps(first:50)` with no `pageInfo` (`internal/github/graphql.go:124-137`); >50 jobs or >50 steps silently lose data. Worse: the truncated `RunDetail` is cached permanently, so even after pagination ships, poisoned rows would be served forever.
- Fix: select `pageInfo{hasNextPage}` on both connections; one aggregated `diag.Warn` per analysis naming the connection; skip caching truncated runs (or add a completeness column). Full pagination is P2.
- **Files:** `internal/github/graphql.go`, `internal/app/service.go`

### D6. Rate budget: audit the GraphQL pool, don't fall back to REST on rate-limit errors [S–M]
- `checkRateBudget` estimates GraphQL query count but compares against `limits.Core` — the REST pool (`internal/app/service.go:268-303`, `internal/github/client.go:81-92`). False aborts when core is low; and when the GraphQL pool is exhausted mid-run, `fetchBatchGraphQL` (`graphql.go:63-67`) silently falls back to REST at ~20× the approved call count. No `Retry-After`/secondary-limit handling either: 20 workers keep hammering through an abuse-limit event.
- Fix: budget points against `limits.GraphQL`; detect `*github.RateLimitError`/`*github.AbuseRateLimitError` and back off instead of falling back; keep the core check for listing + genuine REST fallback.
- **Files:** `internal/app/service.go`, `internal/github/client.go`, `internal/github/graphql.go`

### D7. Ctrl+C must abort, not degrade to partial analysis [S]
- `hydrateWorkflow` never returns errors, `fetchBatchGraphQL` falls back to REST on any error including `ctx.Err()`, and `FetchRunDetails` converts per-run `ctx.Err()` to warnings (`internal/app/service.go:207-225`, `internal/github/graphql.go:63-67`, `client.go:288-307`). Cancel mid-hydration → a normal-looking analysis of whatever subset hydrated.
- Fix: skip fallback when `errors.Is(err, context.Canceled/DeadlineExceeded)`; propagate `ctx.Err()` out of `hydrateWorkflow`/`hydrateAll`.
- **Files:** `internal/app/service.go`, `internal/github/graphql.go`, `internal/github/client.go`

### D8. Match REST's `filter=latest` in GraphQL job fetching [XS–S]
- REST hydration requests latest-attempt jobs only (`internal/github/client.go:236`); the GraphQL fragment has no `filterBy: {checkType: LATEST}` (`graphql.go:128`), so the two paths can disagree on re-run runs (duplicate old-attempt jobs skewing job/step stats). Needs one live verification against a re-run workflow, then add the filter.
- **Files:** `internal/github/graphql.go`

### D9. Bounded GraphQL error-body reads [XS] (was 3.6)
- `doGraphQL` still does unbounded `io.ReadAll(resp.Body)` (`internal/github/graphql.go:92`). Wrap with `io.LimitReader(resp.Body, 64<<10)`.
- **Files:** `internal/github/graphql.go`

---

## A — Analysis correctness (make the numbers right)

### A1. Bound change-point segments by neighboring change points [S]
- `before := js.durations[:cp.Index]`, `after := js.durations[cp.Index:]` (`internal/analyze/changepoint.go:134-140`) span the full series even when CUSUM emits multiple CPs. For a 5m→8m→5m series the speedup CP reports −23% instead of −37%, and Mann-Whitney compares mixed-level segments — the p-value tests the wrong hypothesis. The code already bounds `postSegment` correctly; do the same for before/after.
- **Files:** `internal/analyze/changepoint.go`, `internal/analyze/changepoint_test.go`

### A2. Key change-point post-processing by (workflow, job) [XS] ✅ done
- Shipped 2026-07-15: `jobCounts` and `latestRegression` now key by `(WorkflowName, JobName)`, matching the analyzer and `groupOutliers`. Regression tests cover cross-workflow oscillation false positives and cross-workflow regression demotion.

### A3. Apply `--branch` to failure/rerun analysis [S]
- The branch filter only shapes `filtered`; `allDetails` (dedup only) goes to the engine as `AllDetails` (`internal/app/service.go:105-140`), which `FailureAnalyzer` consumes and rerun stats derive from. `--branch main` still reports PR-branch failures — the README contradicts this. Distinct from the F4 branch-category feature.
- Fix: branch-filter `allDetails` before `engine.Run` (keep rerun stats computed pre-dedup but post-branch-filter).
- **Files:** `internal/app/service.go`, `internal/preprocess/filter.go`

### A4. Cost from all runs, not success-only [S]
- `CostAnalyzer` iterates `ac.Details` (success-only by default; `internal/analyze/cost.go:43-44`), but GitHub bills failed and cancelled runs too. A 20% failure rate → ~20% underestimate in the exact numbers users act on (`DailyRate`, `PriorityScore`).
- Fix: compute cost from `AllDetails` (after A3).
- **Files:** `internal/analyze/cost.go`

### A5. Unify runner core-count/cost label parsing [S]
- Mirror-image bugs: `parseCoreCount` splits on `-` so GitHub's canonical `ubuntu-latest-16-cores` falls through to the 2-core default (`internal/analyze/runners.go:147-175`) → a 16-core runner gets "undersized — consider larger runner" advice; while `cost.largerRunnerRe` = `-(\d+)-cores?$` (`internal/cost/model.go:45`) misses the `32core`/`16vcpu` style that runners.go handles → up to 16× cost underestimate. Each parser accepts only what the other rejects.
- Fix: one shared label parser (in `internal/cost` or `internal/model`) handling both conventions; both call sites use it.
- **Files:** `internal/analyze/runners.go`, `internal/cost/model.go`, tests

### A6. Skip queue-time for re-run attempts [XS]
- Queue time = `StartedAt − CreatedAt` (`internal/analyze/summary.go:77-83`), but `run_started_at` resets to the latest attempt's start while `CreatedAt` stays at creation. A run re-run the next morning contributes a ~12h "queue time" to p95. Skip when `RunAttempt > 1`.
- **Files:** `internal/analyze/summary.go`

### A7. Fix outlier percentile gating for small n [S]
- `percentileRank` counts strictly-smaller values, so the max of n points ranks `(n−1)/n×100` — below the `MinPercentile=95` gate for all n < 20 (`internal/stats/outlier.go:140-152`, `internal/analyze/outliers.go:44-51`). Outlier findings are structurally impossible for 5–19-run series even though `minRunsForOutliers = 5`; `critical` (99) needs n ≥ 100; duplicated worst values suppress even large-n outliers. Existing tests use n=20/100, masking it.
- Fix: midrank percentile (`(less + equal/2)/n`) or gate on fence-based `IsOutlier` instead.
- **Files:** `internal/stats/outlier.go`, `internal/analyze/outliers.go`, tests with n=5..19

### A8. Deterministic change-point p-values [S] (was 3.5)
- `changepoint.go:136` still calls non-deterministic `stats.MannWhitneyU`; permutation-path p-values drift ~5% near the 0.05 boundary between runs. Derive a seed from `(workflowID, jobName, cp.Index)` and call `MannWhitneyURand`. Snapshot test: same input → same p-values.
- **Files:** `internal/analyze/changepoint.go`, `internal/analyze/changepoint_test.go`

### A9. CUSUM onset backtracking [S]
- CUSUM reports the detection index, which lags the true shift; post-change points land in the `before` segment, biasing `BeforeMean`, shrinking `PctChange`, and attributing the change to the wrong commit/date (`internal/stats/cusum.go:52-79`, `changepoint.go:127-129`). Standard fix: backtrack to the last index where the winning CUSUM statistic was 0.
- **Files:** `internal/stats/cusum.go`, `internal/analyze/changepoint.go`

### A10. Benjamini-Hochberg across change-point p-values [S]
- One test per CP per (workflow, job) at α=0.05 across dozens of jobs guarantees ~1 false "significant regression" per 20 stable jobs, further inflated by post-selection (split point chosen by CUSUM on the same data) (`changepoint.go:136,258`). Apply BH across all CP p-values per analysis; document the post-selection caveat.
- **Files:** `internal/analyze/changepoint.go` or `postprocess.go`

### A11. Non-overlapping failure-trend windows [XS]
- `recentRate` (last 7d) is compared against the overall rate that includes those 7 days (`internal/analyze/failures.go:212-222`), diluting the signal; with `--since 7d` the trend is always "stable". Compare recent vs the prior period.
- **Files:** `internal/analyze/failures.go`

### A12. Volatility labels need a minimum sample [XS]
- p95/median at n=5 means one slow run labels a stable workflow "volatile" (`internal/analyze/summary.go:219-224`; same metric in `steps.go:184-186`). Require n ≥ 10 for the label (or use p90 below that); otherwise "insufficient data".
- **Files:** `internal/analyze/summary.go`, `internal/analyze/steps.go`

### A13. Look up cost multiplier per run, not per first-seen job [S]
- `jobCosts[k]` freezes `multiplier`/`isSelfHosted` from the first run encountered in map order (`internal/analyze/cost.go:78-96`); a job migrated mid-window (ubuntu → self-hosted) is priced entirely at whichever variant was seen first.
- **Files:** `internal/analyze/cost.go`

### A14. Statistical/labeling hygiene batch [S total]
Small, individually-XS fixes; batch into one PR:
- Exact MW p-value enumerates untied ranks while observed U uses average ranks — wrong with tied (second-resolution) data (`internal/stats/significance.go:108-135`); permutation p can be exactly 0 — use `(count+1)/(reps+1)` (`:191-201`); normal approximation lacks tie/continuity correction (`:204-216`).
- Runner advice: skip "~1x cost" claims when multiplier is unknown/0, and don't give GitHub-billing advice for self-hosted/third-party runners (`internal/analyze/runners.go:99-102`); macOS core fallback documents 4, current arm64 runners have 3 (`:171-172`).
- `FailureKind` defaults to "flaky" with zero failing-step data — make it explicit "unknown" (`internal/analyze/failures.go:205-210`); `skipped` counts toward the failure-rate denominator, `neutral`/`stale` count as failures (`:148-159`).
- `medianByJob` keyed by bare job name scrambles cross-workflow sort order (`internal/analyze/steps.go:150-161`); `JobCostBreakdown.BillableMinutes` includes free self-hosted minutes (`internal/analyze/cost.go:120-125`).

---

## T — Regression protection (pin the behavior before/while fixing it)

### T1. Tests for `internal/app` orchestration [M] (was 3.4)
- Still no `service_test.go`; cmd-level tests reach 61% incidentally but miss exactly what matters: `hydrateWorkflow` 36% (all tests run with nil Store — cache partitioning untested), `checkRateBudget` abort path untested, `countRuns` 20%.
- Cover: cache partitioning (with D2's staleness rules), budget abort, rerun-stats-before-dedup ordering, "no runs"/"all filtered" paths, D3's diagnostics plumbing. Mock `WorkflowFetcher`/`RunStore`.
- **Files:** `internal/app/service_test.go` (new)

### T2. Test the GraphQL layer; injectable endpoint [M]
- `internal/github/graphql.go` has 0% coverage across all 10 functions, and `graphqlEndpoint` is a hardcoded const (`graphql.go:72`) — untestable via httptest and broken for GHE (REST honors `WithEnterpriseURLs`, GraphQL doesn't).
- Fix: endpoint as a Client field defaulting to the const; table tests for `buildBatchQuery`, `parseBatchResponse` (incl. D4's drop paths), `convertGraphQLJobs`, `graphqlConclusion`, `parseGraphQLTime`.
- **Files:** `internal/github/graphql.go`, `internal/github/graphql_test.go` (new)

### T3. Diagnostic consistency tests [S] (was 3.3)
- Assert: one aggregated `KindPartialData` per crossed 1000-cap window (not per page — D3); GraphQL no-node-ID → REST fallback without false warnings; >50-job truncation → one aggregated diagnostic (after D5); missing runner labels → one aggregated diagnostic.
- **Files:** `internal/github/client_test.go`, `internal/app/service_test.go`

### T4. Race detector in CI [S]
- CI runs plain `mise run test` (`.github/workflows/ci.yml:36-37`); the client uses bounded-concurrency goroutines and errgroup. `go test -race ./...` passes locally today — add it as a CI step so it stays that way.
- **Files:** `.github/workflows/ci.yml`, optionally a `mise` task

### T5. Auth bootstrap and subprocess tests [S]
- `ResolveToken` (`internal/github/auth.go:14-31`) and `internal/system/exec.go` are at 0% and are the first code every real invocation runs. exec's timeout also surfaces as bare `signal: killed` — wrap with "timed out after 10s" (check `ctx.Err()`).
- **Files:** `internal/github/auth_test.go`, `internal/system/exec_test.go`, `internal/system/exec.go`

### T6. Formatter coverage for primary features [S–M]
- `progress.go` 0%, `writePipelineTable`/`writeRunnerTable` 0% (`internal/output/table.go:340,377`), llm sections 12–30%, `writeJSONFile` 0%. Extend `formatter_test.go` fixtures with pipeline/runner findings; temp-file test for `--raw-output`.
- **Files:** `internal/output/formatter_test.go`

---

## U — CLI & output UX

### U1. CLI paper-cut batch [S total]
One PR of XS fixes:
- Repo auto-detect fails on dotted repo names (`next.js`, `socket.io`): regex `[^/.]+?` at `cmd/ci-snitch/analyze.go:142` — use `[^/]+?` and strip `.git`.
- Every runtime error dumps the full usage block: set `SilenceUsage: true` on the command (`analyze.go:47` only sets it for one branch).
- Unknown `--format` rejected only **after** the full fetch+analysis (`analyze.go:117`) — validate before constructing the client.
- `--version` flag missing (`main.go:13-24`): set cobra's `Version:` to get it free.
- Double-wrapped error prefix `list workflows: list workflows:` (`internal/app/service.go:64` + `internal/github/client.go:102`).
- `--raw-output` silently ignored unless `--format llm` (`internal/output/formatter.go:24-37`) — warn or error.
- `detectGitHubRepo` error detail discarded (`analyze.go:45-49`).

### U2. Actionable error messages for common failures [S]
- 404/401/403 surface raw go-github errors ("GET … 404 Not Found []") with no hint that a private repo needs a scoped token; typo'd `--workflow` silently matches nothing and the "no runs found" error omits active filters (`internal/app/service.go:67-75,101`).
- Fix: map status codes to guidance; error early on a workflow filter matching zero workflows (list available names); include branch/workflow filters in no-runs errors. Also wire the progress logger into the client (`cmd/ci-snitch/analyze.go:63` never sets `WithLogger`) so rate-limit sleeps aren't silent hangs.
- **Files:** `cmd/ci-snitch/analyze.go`, `internal/app/service.go`, `internal/github/client.go`

### U3. Record filter context in output meta [S]
- `ResultMeta` has no Branch/Workflow/Since fields (`internal/analyze/engine.go:13-18`); a JSON/LLM consumer can't tell whether results were filtered — dangerous when comparing reports.
- **Files:** `internal/analyze/engine.go`, formatters

### U4. Markdown format parity [M]
- Markdown renders only summaries, non-info changepoints, and outliers — no failures, cost, pipeline, runner, steps, or triage sections, all present in table/JSON (`internal/output/markdown.go`). README markets it for PR comments alongside those features. Also prerequisite for F7 (PR comment bot).
- **Files:** `internal/output/markdown.go`, golden tests

### U5. LLM format quality [S]
- Deterministic ordering: `categoryBreakdown` sorts non-stably by count from map iteration; the max-pick over `ByConclusion` ties on map order (`internal/output/llm.go:310-313,416-423`) — output flaps between runs on identical data and will flake any golden test. Use `SortStableFunc` + name tiebreaks.
- Add a metric glossary (volatility thresholds, persistence semantics live only in the table legend, `table.go:730-737`); narrate diagnostics in the briefing (after D3); key `buildVolatileStepIndex` by (workflow, job) not bare JobName (`llm.go:343-359`); align `[COST]` priority inclusion with the PriorityScore ≥ 50 rule used for suggestions (`llm.go:97-105`).
- **Files:** `internal/output/llm.go`

### U6. Exit-code semantics for CI gating [M]
- Exits 0/1 only (`cmd/ci-snitch/main.go:36-40`). A `--fail-on regression|failure-rate>N` mode makes ci-snitch usable as a CI gate; pairs with F7.
- **Files:** `cmd/ci-snitch/`, docs

### U7. Output polish batch [S total]
- `fmtDur` renders 2.5h as "150m" (`internal/output/helpers.go:49-60`); `|` unescaped in markdown/LLM tables (`markdown.go:38`, `llm.go:126`); `"diagnostics": null` in JSON when empty (`internal/analyze/engine.go:23`) — emit `[]`; `Diagnostic.String()` drops the wrapped `Err` (`internal/diag/diag.go:37-42`); `compactResult` comment claims it drops outliers but filters only changepoints (`llm.go:240-243`); `-q` quiet mode; flag-value completion for `--format`; reject `--since 0d`/future dates before fetching.

---

## P — Performance

### P1. Batch cache hydration [M] (was 4.1)
- `LoadRunDetails` loops `LoadRunDetail` per run, which loops `loadSteps` per job (`internal/store/sqlite.go:449-464,407`; `internal/app/service.go:330`): ~1500+ queries for a 500-run cached scan. Add `LoadRunDetailsBatch` (three `IN (…)` queries assembled with maps); benchmark before/after in the PR body.
- **Files:** `internal/store/sqlite.go`, `internal/app/service.go`, `internal/store/sqlite_bench_test.go` (new)

### P2. GraphQL pagination for truncated connections [M] (was 4.2)
- Build on D5: page the affected `checkRuns`/`steps` connection with `after: $cursor` only when truncation fired. Keep the 20-run outer batch unchanged.
- **Files:** `internal/github/graphql.go`

### P3. Eliminate sliding-window overlap [S]
- `windowStart = windowEnd` with an inclusive date-only `created: A..B` filter double-lists the seam day (`internal/github/client.go:135-158`): boundary runs are double-hydrated, double-counted by the budget, double-saved. ~4 duplicated days per default 30d scan. Advance to `windowEnd.AddDate(0,0,1)` with day-aligned windows.
- **Files:** `internal/github/client.go`, `client_test.go`

### P4. Recognize since-day boundary runs as cached [XS–S]
- Date-only `created` filters include runs from before the `since` timestamp on the first day, but `RunsSince` compares full timestamps (`internal/github/client.go:158`, `internal/store/sqlite.go:341`) — those runs are cached yet re-hydrated on every invocation, and up to ~24h of extra data enters the analysis window.
- **Files:** `internal/github/client.go` or `internal/store/sqlite.go`

---

## F — Feature depth & scale (carried over; still valid)

### F1. Parallelism opportunity detection [S] (was 5.1)
- Estimate "if stage B ran in parallel with A you'd save N minutes" for sequential transitions with no job-name-overlap dependency. Severity Info. Depends on F8 fixing stage detection first.

### F2. Workflow config diff at change points [S] (was 5.2)
- One commits-API call per regression; label change points "CI config change" (`.github/workflows/*` touched) vs "application code change". Cache by SHA.

### F3. Reusable workflow call-chain dedup [M] (was 5.3)
- Detect `workflow_call` chains from workflow YAML `uses:`; attribute findings to the leaf, suppress caller duplicates.

### F4. Branch-aware failure analysis [S] (was 5.4)
- `--branch-category {pr,main,all}` weighting PR-branch vs main-branch failures. Distinct from bug A3.

### F5. Regression commit attribution [S] (was 5.5)
- Augment F2 with file/line-count stats from the commits API in change-point output.

### F6. Multi-repo config [M] (was 6.1)
- Config file with repo list + grouping; per-repo SQLite DBs under `~/.cache/ci-snitch/<owner>/<repo>.db`. Until then, a stopgap: the single shared `data.db` grows unboundedly with no pruning (`internal/store/sqlite.go:17-79`) — add `DELETE FROM runs WHERE created_at < ?` prune-on-open (needs a repo column or per-repo DBs to do properly).

### F7. PR comment bot [M] (was 6.2)
- `ci-snitch report --pr 123` posting a markdown base-vs-PR comparison; reusable GitHub Action wrapper. Depends on U4 (markdown parity) and U6 (exit codes).

### F8. Overlap-based stage detection [M]
- Jobs group into a stage only if starting within 30s of the stage's first job (`internal/analyze/pipeline.go:276-307`); a matrix fan-out staggered by a constrained runner pool becomes several fake "sequential" stages, corrupting critical path and any F1 estimate. `Sequential: i > 0` also mislabels overlapping stages (`:201`). Group by time-overlap; set `Sequential` only when `next.start >= prev.end − ε`.

### F9. Partial re-run duration handling [M]
- Dedup keeps the latest attempt; for "re-run failed jobs" that attempt's wall clock covers only the re-run subset, so a 40-min workflow can contribute a 6-min duration to summary/outlier/changepoint series (`internal/preprocess/filter.go:81-100`, `internal/model/model.go:106-120`). Options: keep attempt 1 for duration series, or exclude `RunAttempt > 1` from duration collection.

---

## H — Tooling & housekeeping

### H1. gitignore `local/` and coverage artifacts [XS]
- `local/` (never-committed scratch, contains real repo data) is not in `.gitignore` — one `git add -A` away from violating the anonymization rule. Add `local/` and `*.out`.

### H2. `mise run check`: order fmt before lint [XS]
- `check` declares `depends = ["fmt", "lint", "test"]` with no ordering (`mise.toml:24-26`); mise runs independent deps in parallel, so lint can read files fmt is rewriting. Make `lint` depend on `fmt` or order the run.

### H3. govulncheck in CI [S] (was 7.2)
- `mise run vuln` task + CI step after lint. Fatal from day one if baseline is clean.

### H4. `ci-snitch doctor` [S] (was 7.3)
- Validate token, rate limit, cache path writable, SQLite openable, git remote detectable. One line per check.

### H5. Fix install.sh Windows path [S]
- The MINGW/MSYS/CYGWIN branch downloads the zip then `mv`s a binary named `ci-snitch` (archive contains `ci-snitch.exe`) into `/usr/local/bin` with `sudo` — can never succeed (`install.sh:17,41-46,74`). Handle `.exe` + a sensible dir, or explicitly refuse with a pointer to the release zip.

### H6. Smoke test the production path [S]
- `cmd/smoke/main.go:77` exercises REST `FetchRunDetails`, but the CLI uses `FetchRunDetailsGraphQL` (`internal/app/service.go:349`), and it stops before analyzers/formatters. The mandated pre-PR smoke test skips the most bug-prone path. Switch to the GraphQL path and run the full pipeline through a formatter.

### H7. goreleaser: migrate deprecated `format` keys [XS]
- `archives.format`/`format_overrides.format` deprecated since v2.6 (`.goreleaser.yml:19-23`); release workflow floats on `~> v2`, so this breaks on the next major. Rename to `formats:`.

### H8. Make the CI migration step real, or drop it [S]
- "Test schema migration" re-runs `TestMigration` already covered by `mise run test` (`.github/workflows/ci.yml:42-43`). Either build the last tagged release, create a DB, and open it with new code — or delete the step.

### H9. Store-layer cleanups batch [XS total]
- Labels round-trip breaks on commas — store as JSON (`internal/store/sqlite.go:225,318,403-405`); `idx_runs_status` can't serve its only (inequality) query (`sqlite.go:34` vs `:355`); dead `runByID` map (`internal/github/graphql.go:176-180`); log swallowed store read errors in verbose mode (`internal/app/service.go:240,252,313-323`).

### H10. Docs batch [XS]
- README flags table: add `--raw-output`; document `md` alias, `completion` subcommand, NO_COLOR/FORCE_COLOR support. Optional: linter adds (`errname`, `noctx`, `embeddedstructfieldcheck`).

### H11. Versioned schema migrations [M] (was 7.4 — still deferred)
- Defer until a schema change actually demands it (D5's completeness column may be that moment).

---

## Verification gate

Every PR:
1. `mise run check` (fmt + lint + test).
2. `go run ./cmd/smoke` — update `cmd/smoke/main.go` to exercise any new functionality (and see H6).
3. `./bin/ci-snitch analyze cli/cli --since 7d` — eyeball output for regressions.
4. New analyzers / formatters: golden tests with anonymized data in `internal/*/testdata/`.

## Versioning

Tag a new minor version after each PR merge to main. Semver: minor for features, patch for fixes.

## Implementation order

First eight — unblocked, small, highest trust-leverage:

1. ~~**D1** SQLite pragmas per connection~~ ✅
2. ~~**A2** postprocess (workflow, job) keying~~ ✅
3. **D3** diagnostics plumbing to JSON/LLM output
4. **A3 + A4** branch filter for AllDetails + cost from all runs (one PR)
5. **D2** re-run cache invalidation
6. **D4** GraphQL silent-drop warnings
7. **U1** CLI paper-cut batch
8. **T1** `internal/app` service tests (pins 1, 3, 4, 5)

Then: A1/A7/A8 (changepoint/outlier correctness) → D5/D6/D7 → T2/T3/T4 → remaining A → U → P → F. H items are opportunistic and can interleave; H1/H2 can ride along with any PR.

## Items intentionally not adopted

Carried forward from the previous roadmap (all still apply):

- **Splitting `Service.Run` into Planner/Hydrator/etc.** — already factored into helpers; no forcing function.
- **Central `golang.org/x/time/rate` limiter** — D6's targeted fixes cover the observed cases; revisit after a real secondary-limit incident.
- **Adopting `gonum`, `go-pretty`, `go-retryablehttp`** — no concrete pain.
- **Source-neutral data interface for GitLab/CircleCI** — defer until a second backend is real.
- **Migration-framework refactor before a schema change demands it** — see H11.

New this review:

- **Release signing / SBOM (cosign)** — checksums cover transport integrity; keyless signing is cheap but adds release-pipeline complexity with no current user demand. Revisit if distribution widens.
- **CONTRIBUTING.md** — no external contributors yet; the dev loop is documented in README/CLAUDE.md. Add when the first outside PR appears.
- **Config file for CLI defaults** — flags suffice at current surface area; F6's multi-repo config is the natural moment to introduce one.
