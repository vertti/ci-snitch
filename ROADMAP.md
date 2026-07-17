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

### D2. Invalidate cached runs that were re-run [S] ✅ done
- Shipped 2026-07-15: `servableFromCache` compares the listing's `UpdatedAt` against the cached row's (`cachedUpdatedAt` map); a re-run bumps `UpdatedAt` so the stale attempt is re-fetched. `countRuns` uses the same rule, keeping the rate budget honest. Tests cover stale-refetch, fresh-cache-hit, and budget counting.

### D3. Surface fetch/hydration diagnostics in `result.Diagnostics` [S] ✅ done
- Shipped 2026-07-15: list/hydration/preprocess/cache-save diagnostics are collected through `fetchRunLists`/`hydrateAll`/`hydrateWorkflow` and appended to `result.Diagnostics`, so JSON/LLM output now carries partial-data caveats; the CLI prints them once at end of run (live `WARNING:` logs removed to avoid double-printing). The 1000-cap warning now fires once per window instead of once per page. Tests: `internal/app/service_test.go` (new — starts T1) with fake fetcher/store covering all four diagnostic paths; `TestFetchRuns_CapWarningEmittedOncePerWindow`.

### D4. No silent drops in GraphQL batch parsing [XS–S] ✅ done
- Shipped 2026-07-15: `parseBatchResponse` returns unaccounted runs (malformed payload, missing alias key) as `missed`; `fetchBatchGraphQL` hydrates them via REST with one aggregated `KindPartialData` warning. Null nodes keep their per-run warning. Also removed the dead `runByID` map and made the GraphQL endpoint a Client field (`graphqlURL`, default unchanged) so the layer is finally testable via httptest — first three behavior tests in new `graphql_test.go`; T2 builds on this.

### D5. GraphQL truncation diagnostic — and don't cache truncated runs [S] (was 3.2) ✅ done
- Shipped 2026-07-15: `pageInfo{hasNextPage}` selected on both connections; runs with >50 jobs or >50 steps/job are marked `RunDetail.Truncated`, produce one aggregated `KindPartialData` warning per fetch, and are analyzed but never cached (a cached row would serve incomplete data forever). Full pagination remains P2. Query verified against the live API.

### D6. Rate budget: audit the GraphQL pool, don't fall back to REST on rate-limit errors [S–M] ✅ done
- Shipped 2026-07-15: `RateLimitStatus` carries both pools; `checkRateBudget` audits GraphQL (falls back to core only when the API exposes no GraphQL pool, e.g. some GHE) and the abort names the pool. GraphQL `RATE_LIMITED` errors are typed; hydration stops with one aggregated `KindRateLimit` warning instead of falling back to REST at ~20× core spend.
- Deferred remainder: REST secondary-limit (`Retry-After`) backoff in `FetchJobs` workers — bounded by run count today; revisit after a real abuse-limit incident (consistent with the not-adopted central-limiter decision).

### D7. Ctrl+C must abort, not degrade to partial analysis [S] ✅ done
- Shipped 2026-07-15: cancellation skips REST fallback and per-run warning spam (one Ctrl+C previously manufactured N "failed to fetch jobs" warnings); `hydrateAll` propagates `gctx.Err()` so `Run` errors instead of analyzing a partial subset. Also wired `signal.NotifyContext` in `main` — SIGINT/SIGTERM now cancel gracefully (previously a hard kill; the context was never cancelled by signals at all). Verified live: SIGINT mid-run → `Error: ... context canceled`, exit 1.

### D8. Match REST's `filter=latest` in GraphQL job fetching [XS–S] ✅ done
- Shipped 2026-07-15: `filterBy:{checkType:LATEST}` on the `checkRuns` connection, matching REST's `filter=latest`; query verified against the live API.

### D9. Bounded GraphQL error-body reads [XS] (was 3.6) ✅ done
- Shipped 2026-07-15: error responses read via `LimitReader(64KiB)`; success responses guarded at 64MiB (pathological-payload guard, not a budget — an oversized payload fails JSON parse and takes the D4 REST fallback).

**D section complete** — release checkpoint (v0.25.0, fetch-layer hardening).

---

## A — Analysis correctness (make the numbers right)

### A1. Bound change-point segments by neighboring change points [S] ✅ done
- Shipped 2026-07-15: before/after segments bounded by the previous/next change point; Mann-Whitney compares adjacent segments only. Companion decision: segment means now come from the **clamped** series (what CUSUM saw) while significance stays on raw values (rank-based, outlier-robust) — with bounded segments a raw mean would let one extreme outlier manufacture a large "% change" (caught by `TestIntegration_OutlierDoesNotCauseChangepoint` during the fix).

### A2. Key change-point post-processing by (workflow, job) [XS] ✅ done
- Shipped 2026-07-15: `jobCounts` and `latestRegression` now key by `(WorkflowName, JobName)`, matching the analyzer and `groupOutliers`. Regression tests cover cross-workflow oscillation false positives and cross-workflow regression demotion.

### A3. Apply `--branch` to failure/rerun analysis [S] ✅ done
- Shipped 2026-07-15: `allDetails` is branch-filtered before rerun stats, dedup, and the engine, so failure rates, rerun stats, and cost all respect `--branch`. A branch filter matching zero runs now errors with the branch name instead of a generic preprocessing message.

### A4. Cost from all runs, not success-only [S] ✅ done
- Shipped 2026-07-15 (same PR as A3): `CostAnalyzer` reads `AllDetails` (fallback to `Details`), so failed/cancelled runs count toward billable minutes, `DailyRate`, and `PriorityScore`.

### A5. Unify runner core-count/cost label parsing [S] ✅ done
- Shipped 2026-07-15: shared `cost.ParseCoreCount` handles both the split (`-16-cores`) and adjacent (`32core`, `16vcpu`) conventions; `runners.go` delegates to it (16-core runners no longer get "undersized" advice) and `largerRunnerMultiplier` bills adjacent-convention labels — but only on recognized GitHub OS prefixes, so third-party vendor labels (Blacksmith etc.) are not billed at invented GitHub rates (pinned by test). Created `runners_test.go` (the analyzer previously had zero tests).

### A6. Skip queue-time for re-run attempts [XS] ✅ done
- Shipped 2026-07-15: queue time only computed for `RunAttempt <= 1`; a re-run's start-created gap measures "time until someone clicked re-run", not queue wait.

### A7. Fix outlier percentile gating for small n [S] ✅ done
- Shipped 2026-07-15: `percentileRank` uses midrank (ties contribute half), and the reporting gate is capped at the highest percentile a sample of size n can produce (`effectiveMinPercentile`). Small samples (5–19 runs) and duplicated worst values now report; `critical` (p99) achievable from n=50 instead of n=100. Tests: small-sample (n=8), tied-worst (n=30, two identical outliers), plus midrank unit tests.

### A8. Deterministic change-point p-values [S] (was 3.5) ✅ done
- Shipped 2026-07-15: `seededRNG(workflowID, jobName FNV-hashed, cp.Index)` feeds `MannWhitneyURand`; identical input yields identical p-values across invocations (pinned by a run-twice test on the Monte-Carlo permutation path).

### A9. CUSUM onset backtracking [S] ✅ done
- Shipped 2026-07-15: the reported index walks back over consecutive shifted-side points immediately before the alarm — commit/date attribution and segment splits now point at the onset, not the lagged detection. Deliberately consecutive-only rather than last-zero-of-the-statistic: the textbook estimator let slow noise drift drag the onset deep into the baseline (caught by `TestIntegration_GenuineSpeedupDetected`, which collapsed to −1.9%/p=0.29 under it).

### A10. Benjamini-Hochberg across change-point p-values [S] ✅ done
- Shipped 2026-07-15: `stats.BenjaminiHochberg` q-values computed across all change points in post-processing (before oscillation counting/dedup); `ChangePointDetail.QValue` exported in JSON; findings with q > α demoted to info/Minor. Live validation on cli/cli: 6 of 52 CPs had raw p < 0.05 but q > 0.05 — the predicted false-positive rate, now demoted. Post-selection caveat (CUSUM picks the split on the same data) documented here: q-values are still optimistic; treat borderline q as suggestive, not proof.

### A11. Non-overlapping failure-trend windows [XS] ✅ done
- Shipped 2026-07-15: trend compares the recent 7 days against the prior period only (both need ≥5 runs). Previously a 12%→20% week-over-week rise was diluted to "stable", and `--since 7d` made the trend structurally always stable.

### A12. Volatility labels need a minimum sample [XS] ✅ done
- Shipped 2026-07-15: `volatilityLabel` withholds the judgement below 10 runs (empty label; raw ratio still reported) — at small n, p95 is effectively the max and one slow run branded stable workflows "volatile". Applies to workflows and jobs via `computeStats`; step volatility stays numeric (its labeling lives in formatters, see U5).

### A13. Look up cost multiplier per run, not per first-seen job [S] ✅ done
- Shipped 2026-07-15: `IsSelfHosted`/`LookupMultiplier` evaluated per run; the breakdown's displayed multiplier tracks the most recent run by `CreatedAt`. Tested with a mid-window ubuntu→self-hosted migration in both orderings.

### A14. Statistical/labeling hygiene batch [S total] ✅ done
- ✅ Stats (2026-07-15): exact MW path enumerates value assignments with tie-aware U — the rank-position enumeration was **anti-conservative** on tied data (measured: p=0.31 reported where the tie-aware truth is 0.52); permutation p uses `(count+1)/(reps+1)`; normal approximation gets the continuity correction. Not adopted: tie-corrected sigma in the normal path (rare + corrects in the more-significant direction).
- ✅ Analyzers (2026-07-15): oversized-runner advice only claims "~Nx cost" with a known multiplier >1; macOS core fallback 4→3; `FailureKind` "unknown" without step data; `skipped` excluded from the failure denominator and `neutral` no longer a failure; step sort keyed per (workflow, job); `JobCostBreakdown` splits `SelfHostedMinutes` out of `BillableMinutes`.

**A section complete** — release checkpoint (v0.26.0, analysis correctness) — HOLD until `HOMEBREW_TAP_TOKEN` is rotated.

---

## T — Regression protection (pin the behavior before/while fixing it)

### T1. Tests for `internal/app` orchestration [M] (was 3.4) ✅ done
- Shipped 2026-07-15 (built up across the D2/D3/A3 PRs, completed in the T1 PR): `service_test.go` covers diagnostics plumbing (4 paths), cache partitioning incl. staleness + load-error fallback, budget abort (with no-hydration assertion) and non-fatal rate-limit read errors, branch filtering, all-filtered/no-branch-runs error paths, rerun-stats wiring into failure details, runner-label aggregation. Package coverage 0% → 91.5%.

### T2. Test the GraphQL layer; injectable endpoint [M] ✅ done
- Endpoint injection shipped with D4; the D4–D8 behavior tests plus unit tables for `parseGraphQLTime`/`graphqlConclusion`/`convertGraphQLJobs`/`truncateBody` (T2 PR) take `internal/github` from 42% (graphql.go at 0%) to **87.5%**. GHE note: `graphqlURL` is injectable but the CLI doesn't expose enterprise URLs yet — that's F-scope if ever requested.

### T3. Diagnostic consistency tests [S] (was 3.3) ✅ done
- 1000-cap once-per-window (D3 PR), no-node-ID REST fallback without false warnings, missing-runner-labels aggregation (T1 PR), truncation → one aggregated diagnostic (D5 PR).

### T4. Race detector in CI [S] ✅ done
- Shipped 2026-07-15: `mise run test-race` task + CI step after Test (full race suite ~passes locally in seconds).

### T5. Auth bootstrap and subprocess tests [S] ✅ done
- Shipped 2026-07-15: `auth_test.go` covers env-var, gh-missing, gh-unauthenticated, empty-token, and happy paths via fake `gh` scripts on PATH; `exec_test.go` covers stdout/not-found/stderr/timeout. Deadline kills now say "timed out" instead of `signal: killed` (was red).

### T6. Formatter coverage for primary features [S–M] ✅ done
- Shipped 2026-07-15: `richTestResult` fixture gains pipeline + runner findings (both table writers were 0%); `--raw-output` temp-file test (valid JSON + briefing pointer); `Progress` tested through a stderr pipe (real constructor, non-TTY branch, no-ANSI assertion). Package 68% → 78%.

**T section complete.**

---

## U — CLI & output UX

### U1. CLI paper-cut batch [S total] ✅ done
- Shipped 2026-07-15, all seven: dotted-repo regex (`next.js` detects), `SilenceUsage` on runtime errors, `--format` and `--raw-output` validated before any API call (was: full fetch then error), `--version` flag via cobra `Version:`, de-duplicated `list workflows:` prefix, `--raw-output` without `--format llm` errors, repo-detection failures surface the underlying cause.

### U2. Actionable error messages for common failures [S] ✅ done
- Shipped 2026-07-15: 404/401/403 map to guidance (spelling/private-token, expired-credentials, scope/SAML); a workflow filter matching nothing errors immediately listing available names (sorted, capped at 15); the no-runs error names an active `--workflow` filter; the client logger is wired so rate-limit sleeps and REST fallbacks are visible instead of silent hangs. Live-verified against the real API.

### U3. Record filter context in output meta [S] ✅ done
- Shipped 2026-07-15: `ResultMeta` gains `branch`/`workflow` (omitempty) and `since`; populated from the run options, flows into JSON/LLM automatically.

### U4. Markdown format parity [M] ✅ done
- Shipped 2026-07-15: markdown gains Failure Rates, Cost, Pipeline Structure, Runner Sizing, and Step-Level Timing sections (tables/lists mirroring the table formatter's content; names pipe-escaped). Unblocks F7.

### U5. LLM format quality [S] ✅ done
- Shipped 2026-07-15: deterministic ordering (category ties break lexicographically; conclusion max-pick tie-broken — both pinned by run-50-times tests); new `## Data Caveats` section narrates diagnostics and `## Glossary` defines volatility/persistence/q-value/billable semantics; volatile-step index keyed by (workflow, job); `[COST]` priorities gate on PriorityScore ≥ 50 like the suggestions.

### U6. Exit-code semantics for CI gating [M] ✅ done
- Shipped 2026-07-15: `--fail-on regression,failure-rate>N` exits 2 (vs 1 = operational error) with one `fail-on:` reason per tripped finding on stderr, printed even in quiet mode; conditions validated before any fetch. Document in README via H10.

**U section complete.**

### U7. Output polish batch [S total] ✅ done
- Shipped 2026-07-15: hours render as "2h30m"; `|` escaped in markdown/LLM table names; JSON emits `"diagnostics": []`/`"findings": []` instead of null (fixed at the formatter boundary); `Diagnostic.String()` shows the wrapped cause; percentile displays floor (no more "slower than 100% of runs"); `-q/--quiet` silences stderr entirely; `--format` shell completion; `--since 0d`/future dates rejected before any API call; `compactResult` comment matches the code.

---

## P — Performance

### P1. Batch cache hydration [M] (was 4.1) ✅ done
- Shipped 2026-07-15: `LoadRunDetailsByIDs` hydrates in 3 `IN`-clause queries per 500-ID chunk (placeholders only — bound args); `hydrateWorkflow` partitions then batch-loads (`partitionCached`), with batch-miss/error degradation to fetch; `LoadRunDetails` reimplemented on top (smoke benefits too). Benchmark, 500 runs: **13.2ms → 3.3ms (4×)**, ~1500 queries → 3.

### P2. Complete truncated runs [M] (was 4.2) ✅ done
- Shipped 2026-07-15 — simpler than the planned cursor pagination: truncated runs (>50 jobs or >50 steps/job) are refetched via REST, which paginates jobs fully and embeds complete steps. One REST call per monster run, result complete and **cacheable** (beats re-fetching it every scan); an Info note replaces the partial-data warning. REST-failure path keeps the run uncacheable with the old warning.

**P section complete.**

### P3. Eliminate sliding-window overlap [S] ✅ done
- Shipped 2026-07-15: the next window starts the day after the previous ends (the `created` filter is date-only, inclusive both ends). Seam days no longer double-listed/hydrated/budgeted/saved. Pinned by a disjoint-and-contiguous window test.

### P4. Recognize since-day boundary runs as cached [XS–S] ✅ done
- Shipped 2026-07-15: relative `--since` forms truncate to UTC midnight, matching the date-only API filter, so since-day runs finally match the cache's timestamp comparison. `--since 0d` still rejected (validated pre-truncation). Live: two consecutive 7d scans went from a steady "21 to fetch" every run to **808 cached, 0 to fetch**.

---

## F — Feature depth & scale (carried over; still valid)

### F1. Parallelism opportunity detection [S] (was 5.1) ✅ done
- Shipped 2026-07-16: `PipelineStage.PotentialSavings` = min(prev, cur) per sequential transition (thanks to F8, sequential now means genuinely-waited); table waits markers show "~Xm/run if parallel" (≥1min), LLM suggestions phrase it as an upper bound with "verify job dependencies first". No name-overlap dependency guessing — the estimate is explicitly labeled an estimate instead.

### F2. Workflow config diff at change points [S] (was 5.2) ✅ done
- Shipped 2026-07-16 (with F5): confirmed regressions are enriched post-analysis via `GetCommitInfo` — labeled `ci-config` (touched `.github/workflows/`) vs `code`. Bounded at 10 lookups per scan, deduped by SHA in-memory; the SQLite SHA cache is deferred until the call volume ever matters (H11 spirit).

### F3. Reusable workflow call-chain dedup [M] (was 5.3) — NEEDS PREMISE VALIDATION
- 2026-07-16: before building, validate the premise. `workflow_call` callees do not create separate workflow runs — their jobs execute inside the caller's run — so "duplicated findings across caller and callee" only occurs when a callee also runs standalone (e.g. both `workflow_call` and `push` triggers). Find a real repo exhibiting the duplication first; if it's rare, this is a not-adopted candidate.

**F section complete** apart from F3 (pending premise validation). Release checkpoint: v0.27.0 (pipeline insights & commit attribution).

### F4. Branch-aware failure analysis [S] (was 5.4) ✅ done
- Shipped 2026-07-16: `--branch-category {pr,main,all}` selects runs by trigger event (`pull_request` vs everything else) before all analysis — catches every PR branch at once, which `--branch` can't express. Recorded in `meta.branch_category`; validated before any API call.

### F5. Regression commit attribution [S] (was 5.5) ✅ done
- Shipped 2026-07-16 (with F2): files changed and +/− line counts in `ChangePointDetail` JSON, the markdown changepoint lines (bold **CI config change** marker), and the LLM investigation prompts ("it touched .github/workflows/ (2 files, +10/−14), so CI config is the first suspect").

### F6. Multi-repo config [M] (was 6.1) — DEFERRED (2026-07-16: keep this a simple single-repo CLI, per user)
- Config file with repo list + grouping; per-repo SQLite DBs under `~/.cache/ci-snitch/<owner>/<repo>.db`. Until then, a stopgap: the single shared `data.db` grows unboundedly with no pruning (`internal/store/sqlite.go:17-79`) — add `DELETE FROM runs WHERE created_at < ?` prune-on-open (needs a repo column or per-repo DBs to do properly).

### F7. PR comment bot [M] (was 6.2) — DEFERRED (2026-07-16: keep this a simple CLI, not a bot/integration, per user)
- `ci-snitch report --pr 123` posting a markdown base-vs-PR comparison; reusable GitHub Action wrapper. Depends on U4 (markdown parity) and U6 (exit codes).

### F8. Overlap-based stage detection [M] ✅ done
- Shipped 2026-07-16: jobs group by temporal overlap (a job starting while the stage still runs is concurrent, whatever the start stagger); a job starting after the stage ended is a new stage even under 30s. The `Sequential` flag becomes correct by construction — a stage break can only occur after the previous stage finished. Unblocks F1.

### F9. Partial re-run duration handling [M] ✅ done
- Shipped 2026-07-16: `preprocess.Run` excludes `RunAttempt > 1` from the duration series (with an aggregated diagnostic); re-run attempts remain in `AllDetails` for failure/rerun/cost analysis. A partial re-run's wall clock covers only the re-run subset and is not a comparable sample.

---

## H — Tooling & housekeeping

### H1. gitignore `local/` and coverage artifacts [XS] ✅ done (batch 1)

### H2. `mise run check`: order fmt before lint [XS] ✅ done (batch 1) — `lint` depends on `fmt`.

### H3. govulncheck in CI [S] (was 7.2) ✅ done (batch 2) — `mise run vuln` + fatal CI step (baseline verified by the introducing PR's own CI run).

### H4. `ci-snitch doctor` [S] (was 7.3) ✅ done — token, API reachability (shows both rate pools), cache openable, git remote (informational: failing it is a normal way to run the tool). All checks run even after a failure — the point is a full report; any hard failure exits non-zero.

**H section complete** (H11 stays deliberately deferred).

### H5. Fix install.sh Windows path [S] ✅ done (batch 3) — installer refuses Windows explicitly with a pointer to the release zip (the path could never succeed: archive holds `ci-snitch.exe`, target assumed `/usr/local/bin` + sudo).

### H6. Smoke test the production path [S] ✅ done (batch 2) — smoke hydrates via `FetchRunDetailsGraphQL` and finishes with engine + LLM formatter: fetch → store → analyze → render.

### H7. goreleaser: migrate deprecated `format` keys [XS] ✅ done (batch 1) — `formats: [tar.gz]` / `[zip]`.

### H8. Make the CI migration step real, or drop it [S] ✅ done (batch 2) — dropped; it re-ran `TestMigration` already covered by `mise run test`, and the migration unit tests construct genuine old-schema DBs.

### H9. Store-layer cleanups batch [XS total] ✅ done (batch 1)
- Labels stored as JSON (legacy comma rows still read); dead `idx_runs_status` dropped from schema and migrated DBs; cache read failures log a warning instead of silently re-fetching everything. (`runByID` went with D4.)

### H10. Docs batch [XS] ✅ done (batch 3) — README gains `--fail-on`/`--raw-output`/`-q` rows, a CI-gating section with exit-code semantics, `md` alias, completion, NO_COLOR. Linter adds remain opportunistic (config is already strict).

### H11. Versioned schema migrations [M] (was 7.4 — still deferred)
- Defer until a schema change actually demands it (D5's completeness column may be that moment).

---

## Q — Code quality (2026-07-17 maintainability review; no behavior bugs, debt that taxes the next change)

### Q1. Extract shared formatter decision logic + exhaustiveness guard [S] ✅ done
- `dominantFailingStep()`/`isRegressionSlowdown()` + `dominantStepShare`/`minCostPriorityScore` consts in `helpers.go`; the `> 0.6` vs `>= 0.6` drift resolved to `>=` (red test pinned the 3-of-5 disagreement). `analyze.AllTypes` + bucketing exhaustiveness test guard `groupByType`.

### Q2. Shared job-series views on AnalysisContext [M]
- Six analyzers re-declare a private `jobKey{wfID, job}` and re-implement group-by-(workflow, job) duration-series collection (`steps.go:62`, `outliers.go:113`, `changepoint.go:95`, `cost.go:44`, `runners.go:55`, `summary.go:166`; `postprocess.go` adds `wfJob`/`groupKey`). Add memoized `ac.JobSeries()` + one exported `JobKey`; analyzers keep only their own math.

### Q3. Golden-file tests for output formatters [M]
- The package whose output is the product has only substring assertions; alignment/ordering regressions pass. Add `testdata/*.golden` for table/markdown/llm with an `-update` flag (anonymized fixture data). Fold in the palette-struct change (`table.go:85`/`color.go:32` package-global mutable ANSI vars make test order significant) so colored goldens are possible.

### Q4. Store: dedupe single-item paths; hoist column lists [S]
- `SaveRunDetail`/`LoadRunDetail`/`loadSteps` single-item paths have zero production callers yet duplicate the batch SQL/scan logic (~120 lines). Reimplement as one-element wrappers over batch paths.
- The 13-column runs list is spelled out 5×, jobs 4×: hoist `runCols`/`jobCols`/`stepCols` consts shared by insert/select; funnel run scanning through one scanner.

### Q5. Export label constants; use them across analyze/output [S]
- Labels minted in analyze are string-matched in output with no compile-time link: persistence (`table.go:708` vs existing constants), volatility (`"volatile"`/`"spiky"` at `table.go:127,283`), runner `"oversized"`/`"undersized"` (`runners.go:98`). Also use the existing Type constants at all construction sites and co-locate them with their analyzers.

### Q6. Finish the diag.Diagnostic migration [S]
- Deprecated `Warning = diag.Diagnostic` alias is still the signature currency of `internal/github` (15 uses) and `preprocess.Run`'s return type. Delete both aliases, use `diag.Diagnostic` everywhere.

### Q7. Cost: OS-prefix pricing fallback; collapse speculative Model [S] ✅ done
- `osPrefixMultiplier` prices unknown GitHub images by OS prefix (macos*→10, windows*→2, ubuntu*/linux*→1) before the 1× default; red tests pinned `macos-16`→10 and `windows-2028`→2. Vendor labels still fall through. Table comment dated. `Model`/`DefaultModel` collapsed to package-level functions (no callers used the type).

### Q8. Fold event-category into the preprocess pipeline [S]
- `FilterByEventCategory` is the one preprocessing step outside `Run()` (called separately from `service.go:241`) with a stringly-typed `"pr"` category. Fold into `Options`/`Run()`; shared category constants with `app.Options.BranchCategory`.

### Q9. Vestigial cleanup sweep [S]
- `MarkdownFormatter.Verbose` set but never read (markdown ignores `-v` while table honors it) — honor it, with a red test first.
- Delete: unused `WithMaxConcurrentJobs` (`client.go:34`), unread GraphQL `DatabaseID` (`graphql.go:251`), unconstructed `diag.KindAuth`.
- Fix stale `detectStages` doc comment (still describes removed 30s-window behavior); two doc comments attached to const blocks instead of their functions; stranded `RateLimitStatus` doc sentence.
- Uniform table writer signatures (3 of 8 return always-nil errors); `ctx.Err()` check between analyzers in `Engine.Run`; `defer s.Prog.Done()` instead of 5 manual calls; postprocess drop-filter hardcodes 0.05 duplicating `warningFailureRate`.
- Finish the half-done symmetric extraction: markdown's `Format` inlines 3 sections its siblings have as `mdWrite*` helpers; `llmWritePriorityFindings` inlines 3 independent loops (clears both cyclo hotspots, zero behavior change).

### Q10. Test hygiene batch [M]
- analyze: eight near-identical fixture builders → one shared builder with option funcs, migrate incrementally.
- app: 10-line fetcher-setup clump repeated 4× in `service_test.go` → `fetcherFor()` helper.
- Fix vacuous-pass risk: `TestChangePointAnalyzer_Persistence_Inconclusive` asserts inside `if` with no require that a finding exists; assert `PostChangeRuns` as a range, not a pinned internal value.
- Deduplicate `TestFetchRunDetails_ConcurrencyBounded`/`_SemaphoreBoundsConcurrency` (same test, different semaphore size).
- `applyFailOnGate`: inject `io.Writer`, test reason-printing + exit-code-2 path (the one testable uncovered cmd function).
- Migration test: snapshot the real pre-v0.7 schema instead of a hand-written loose one; `makeDetail` in preprocess tests should set a real WorkflowID (test currently keys on zero value).

### Q11. github: shared rate-reset wait; classify errors everywhere [S]
- The "remaining < 100, sleep until reset" block is copy-pasted between `fetchRunsWindow` and `FetchJobs` → extract `waitForRateReset`.
- `classifyAPIError`'s 401/403/404 guidance applies only in `ListWorkflows`; `fetchRunsWindow`/`FetchJobs`/`GetCommitInfo` return raw go-github errors (a mid-scan SAML 403 loses the hint). Apply at every wrap site.

### Q12. app: build cache state once; slim WorkflowFetcher [M]
- `countRuns` and `partitionCached` each rebuild the same per-workflow cache state (cachedUpdatedAt map + IncompleteRunIDs, with `servableFromCache` taking the pair as a data clump) — build a `cacheIndex` once after `fetchRunLists`, pass to both budget and hydration phases; stop threading `*Options` into helpers that use one field.
- `WorkflowFetcher` exposes both `FetchRunDetails` and `FetchRunDetailsGraphQL` but the service only calls the GraphQL one; collapse to one hydrate method so the transport choice lives in `github.Client`.

**Explicitly fine as-is (do not "fix"):** the three formatters' presentation independence; `internal/github` as one package (fallback seam is one-directional and documented); `Service.Run`'s phase decomposition; `cmd/smoke` untested (it is the test harness); `cmd/ci-snitch` 49.7% / store 75.9% coverage (wiring and error tails); tiny packages `diag`/`system`/`cost`.

---

## Verification gate

Every PR:
1. `mise run check` (fmt + lint + test).
2. `go run ./cmd/smoke` — update `cmd/smoke/main.go` to exercise any new functionality (and see H6).
3. `./bin/ci-snitch analyze cli/cli --since 7d` — eyeball output for regressions.
4. New analyzers / formatters: golden tests with anonymized data in `internal/*/testdata/`.

Every release tag: the release workflow **conclusion** must be `success` before any further work — a published release page is NOT success (goreleaser publishes it before the homebrew-tap step, which is exactly what failed silently for v0.24.0/v0.25.0). `HOMEBREW_TAP_TOKEN` is a ~90-day PAT (release environment); rotate it before it expires.

## Versioning

Thematic batch releases: tag a version when a roadmap section milestone lands (e.g. the D data-integrity batch, the A analysis-correctness batch), so each release has a coherent story and number-shifting fixes ship together with one explanation. Urgent fixes can still get an ad-hoc patch tag. Semver: minor for features, patch for fixes.

## Implementation order

First eight — unblocked, small, highest trust-leverage:

1. ~~**D1** SQLite pragmas per connection~~ ✅
2. ~~**A2** postprocess (workflow, job) keying~~ ✅
3. ~~**D3** diagnostics plumbing to JSON/LLM output~~ ✅
4. ~~**A3 + A4** branch filter for AllDetails + cost from all runs~~ ✅
5. ~~**D2** re-run cache invalidation~~ ✅
6. ~~**D4** GraphQL silent-drop warnings~~ ✅
7. ~~**U1** CLI paper-cut batch~~ ✅
8. ~~**T1** `internal/app` service tests~~ ✅ (all first-eight items complete)

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
