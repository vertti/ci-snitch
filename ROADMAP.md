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

### P2. GraphQL pagination for truncated connections [M] (was 4.2)
- Build on D5: page the affected `checkRuns`/`steps` connection with `after: $cursor` only when truncation fired. Keep the 20-run outer batch unchanged.
- **Files:** `internal/github/graphql.go`

### P3. Eliminate sliding-window overlap [S] ✅ done
- Shipped 2026-07-15: the next window starts the day after the previous ends (the `created` filter is date-only, inclusive both ends). Seam days no longer double-listed/hydrated/budgeted/saved. Pinned by a disjoint-and-contiguous window test.

### P4. Recognize since-day boundary runs as cached [XS–S] ✅ done
- Shipped 2026-07-15: relative `--since` forms truncate to UTC midnight, matching the date-only API filter, so since-day runs finally match the cache's timestamp comparison. `--since 0d` still rejected (validated pre-truncation). Live: two consecutive 7d scans went from a steady "21 to fetch" every run to **808 cached, 0 to fetch**.

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
