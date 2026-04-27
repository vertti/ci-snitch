# ci-snitch Roadmap

## Status

The product is in **measurement-correctness + trust + scale-hardening** mode, not "needs architecture extraction" mode. The major orchestration refactors (service layer in `internal/app`, structured `diag.Diagnostic`, immutable cost model, errgroup fetch, batched store transactions, batched GraphQL hydration) have all shipped.

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

## What is already correct (do not redo)

- Workflow duration: `RunDetail.Duration()` prefers `max(job.CompletedAt) − Run.StartedAt`, falls back to `Run.UpdatedAt − StartedAt`. Used by every analyzer.
- Failure classification: `cancelled` is tracked separately (`CancellationCount`/`CancellationRate`); only non-success/non-skipped/non-cancelled conclusions count as failures. Failure kind (systematic vs flaky) and category (infra/build/test/other) classified.
- Cost model: self-hosted detected (multiplier 0); larger runners parsed from `-N-cores` suffix per OS; per-job billing rounded up to whole minute.
- Priority score: built from per-run billable minutes (consistent units), not wall-clock variability.
- Diagnostics: `diag.Diagnostic` is the single warning type; aggregated for repeated conditions (e.g. one "missing runner labels for N jobs" diagnostic across the run); embedded in JSON and LLM output; surfaced to stderr by the CLI.
- GraphQL hydration: 20-run batched node lookup with REST fallback; ~20× fewer API calls than per-run REST.
- Rate limit budget: pre-flight estimate with safety margin; abort with actionable error before exhausting limit.
- Date-window listing: 7-day windows avoid the GitHub 1000-result-per-query cap; explicit warning when a window crosses it.

---

## Release 3 — Correctness gaps

_Highest leverage. Each item is small, independently shippable, and fixes a real bug or silent data-loss path._

### 3.1 Enable SQLite foreign-key enforcement [S] — correctness
- `internal/store/sqlite.go` declares `REFERENCES` and relies on manual two-step delete in `SaveRunDetail` because cascade is not enforced. SQLite FKs are connection-level and **off by default**.
- Add `PRAGMA foreign_keys = ON` to the pragma list in `Open` (next to `journal_mode=WAL` and `busy_timeout=5000`).
- Add `ON DELETE CASCADE` to `jobs.run_id` and `steps.job_id` declarations.
- Replace the manual `DELETE FROM steps … DELETE FROM jobs …` pair in `SaveRunDetail` and `SaveRunDetails` with a single `DELETE FROM runs WHERE id = ?` (now cascades) — only after the FK pragma is enabled.
- Test: open the store, query `PRAGMA foreign_keys` and assert it returns 1; insert a run+jobs+steps fixture, delete the run, assert dependent rows are gone.
- **Files:** `internal/store/sqlite.go`, `internal/store/sqlite_test.go`

### 3.2 GraphQL truncation diagnostic [S] — correctness
- `buildBatchQuery` requests `checkRuns(first:50)` and `steps(first:50)` with no `pageInfo` selection. Workflows with > 50 jobs (large matrices) or jobs with > 50 steps silently lose data.
- Add `pageInfo{ hasNextPage }` to both connections in the query.
- Emit one aggregated `diag.Warn` of `KindPartialData` per analysis when truncation occurs, including run count and which connection (jobs / steps) was truncated. Match the aggregation pattern already used for missing runner labels.
- Stop short of full pagination here — surface the data loss first.
- **Files:** `internal/github/graphql.go`, test in `internal/github/client_test.go`

### 3.3 Diagnostic consistency tests [S] — regression protection
- The diagnostic system aggregates and surfaces correctly today, but no tests pin this behavior. Recent output polish was driven by user feedback rather than by failing tests.
- Add tests asserting:
  - 1000-result cap → exactly one `KindPartialData` diagnostic per crossed window with the run count in the message.
  - GraphQL hydration with no node IDs → falls back to REST and emits no false warnings.
  - GraphQL with 50+ jobs (after 3.2) → exactly one aggregated truncation diagnostic.
  - Missing runner labels across multiple workflows → exactly one aggregated diagnostic, not one per workflow.
- **Files:** `internal/github/client_test.go`, `internal/app/service_test.go` (new — see 3.4)

### 3.4 Tests for `internal/app` orchestration [M] — regression protection
- `internal/app/service.go` currently has **no test file at all**. It owns workflow discovery, cache partitioning, rate-limit budgeting, hydration, dedup, preprocess, and analysis wiring — every change to this file is unverified.
- Add `service_test.go` covering:
  - cache partitioning (cached vs needs-fetch decision against `RunsSince` and `IncompleteRunIDs`)
  - rate-limit budget abort path (estimated calls > budget → error with actionable message)
  - rerun stats computed before dedup, dedup applied before analysis
  - "no runs found" and "all filtered out" error paths
- Use the existing `WorkflowFetcher` and `RunStore` interfaces — mock them with table-driven cases.
- **Files:** `internal/app/service_test.go` (new)

### 3.5 Deterministic permutation p-values in change-point analysis [S] — correctness
- `internal/stats/significance.go` provides both `MannWhitneyU` (non-deterministic — fresh PCG seed per call) and `MannWhitneyURand(rng)` (deterministic when given a seeded RNG).
- `internal/analyze/changepoint.go:136` calls `MannWhitneyU`. The exact-enumeration path (n ≤ 20) is deterministic regardless, but the permutation path (`min(n1,n2) ≤ 20` with `n1+n2 > 20`) drifts ~5% near the 0.05 boundary across runs.
- Derive a deterministic seed from stable inputs (`workflowID, jobName, cp.Index`) and pass it via `MannWhitneyURand`.
- Snapshot test: same input data → same p-values across two runs.
- **Files:** `internal/analyze/changepoint.go`, `internal/analyze/changepoint_test.go`

### 3.6 Bounded GraphQL error-body reads [XS] — security hygiene
- `doGraphQL` does `io.ReadAll(resp.Body)` without a limit. A misconfigured proxy or unexpected error page could return a large body.
- Wrap with `io.LimitReader(resp.Body, 64<<10)`.
- Keep the existing 200-byte truncation in error strings.
- **Files:** `internal/github/graphql.go`

---

## Release 4 — Performance

### 4.1 Batch cache hydration [M]
- `Store.LoadRunDetails` loops `LoadRunDetail` per run; `LoadRunDetail` then calls `loadSteps` per job. A 500-run cached scan does roughly `1 + 500 + Σjobs` queries (~1500+).
- Add `Store.LoadRunDetailsBatch(workflowID, since)`: three queries — runs, then jobs `WHERE run_id IN (…)`, then steps `WHERE job_id IN (…)` — assembled in memory with maps.
- Replace the per-run `LoadRunDetail` loop in `Service.hydrateWorkflow` with the batch call.
- Benchmark before/after on a 500-run cached scenario and put the number in the PR body.
- **Files:** `internal/store/sqlite.go`, `internal/app/service.go`, benchmark in `internal/store/sqlite_bench_test.go`

### 4.2 GraphQL pagination for jobs/steps [M]
- Build on 3.2. After detecting truncation, page the affected `checkRuns` / `steps` connection individually using `after: $cursor`.
- Keep the 20-run-per-batch outer loop unchanged. Only paginate the slow path when 3.2's diagnostic would have fired.
- **Files:** `internal/github/graphql.go`

---

## Release 5 — Pipeline depth

### 5.1 Parallelism opportunity detection [S]
- `PipelineAnalyzer` already computes parallelism efficiency, stages, and the critical path. It does **not** estimate "if these stages weren't sequential, you'd save N minutes."
- For each detected sequential transition, estimate savings: if stage B (no upstream artifact dependency, judged by job-name overlap) ran in parallel with stage A, wall-clock would drop from `dur(A) + dur(B)` to `max(dur(A), dur(B))`.
- Conservative: only flag when stages share no obvious data dependency. Surface as `Severity: Info`, not as a confident recommendation.
- **Files:** `internal/analyze/pipeline.go`

### 5.2 Workflow config diff at change points [S]
- When a change point is detected, fetch the changed file list for the captured commit SHA via `gh api /repos/{owner}/{repo}/commits/{sha}` (one call per regression).
- Label change points as "CI config change" (any `.github/workflows/*.yml` modified) vs "application code change" — would have explained real-world regressions immediately.
- Cache the result in SQLite keyed by SHA.
- **Files:** `internal/github/client.go`, `internal/analyze/changepoint.go`, `internal/store/sqlite.go`

### 5.3 Reusable workflow call-chain dedup [M]
- `workflow_call` reusables produce duplicated findings across caller and callee.
- Detect call chains from workflow YAML `jobs.*.uses` fields (one fetch per workflow definition, cached).
- Attribute findings to the leaf workflow; suppress duplicates on callers.
- **Files:** `internal/github/client.go`, `internal/preprocess/`, `internal/analyze/postprocess.go`

### 5.4 Branch-aware failure analysis [S]
- PR-branch failures (expected during development) and main-branch failures (incidents) carry different signal.
- Add `--branch-category {pr,main,all}` that filters or weights failure analysis accordingly.
- Default keeps current behavior; document the flag.
- **Files:** `internal/analyze/failures.go`, `internal/preprocess/filter.go`, `cmd/ci-snitch/analyze.go`

### 5.5 Regression commit attribution [S]
- Augment 5.2: also pull `git diff --stat`-equivalent file/line counts from the GitHub commits API and surface in change-point output, saving the user one round-trip per regression.
- **Files:** `internal/github/client.go`, `internal/analyze/changepoint.go`, formatters

---

## Release 6 — Scale & integration

### 6.1 Multi-repo config [M]
- Config file (TOML or YAML) listing repos with optional team/owner grouping.
- Cross-repo triage: top offenders across the org.
- Per-repo SQLite databases under `~/.cache/ci-snitch/<owner>/<repo>.db`; aggregate queries fan out across them.
- **Files:** `cmd/ci-snitch/`, `internal/app/`, `internal/store/`

### 6.2 PR comment bot [M]
- `ci-snitch report --pr 123` posts a markdown comparison (PR branch vs base branch) to the PR.
- Reusable GitHub Action that wraps the command.
- **Files:** new subcommand under `cmd/ci-snitch/`, action manifest under `.github/actions/`

---

## Release 7 — Polish

### 7.1 Use typed constants in `DetailType()` [XS]
- Replace string literals in `changepoint.go:33`, `cost.go:34`, `summary.go:48`, `outliers.go:22` with `TypeChangepoint`, `TypeCost`, `TypeSummary`, `TypeOutlier`. Add the few missing constants in `analyzer.go`.
- Pure mechanical; no behavior change.
- **Files:** `internal/analyze/*.go`

### 7.2 Add `govulncheck` to CI [S]
- New `mise run vuln` task running `govulncheck ./...`.
- New CI step after `lint`. Fatal from day one if the baseline is clean.
- **Files:** `mise.toml`, `.github/workflows/ci.yml`

### 7.3 `ci-snitch doctor` command [S]
- Validate: GitHub token resolvable (env or `gh`), rate limit readable, cache path writable, SQLite openable, `git remote get-url origin` succeeds in cwd.
- One line per check; non-zero exit on any failure. Reduces support-question volume.
- **Files:** `cmd/ci-snitch/doctor.go` (new)

### 7.4 Versioned schema migrations [M]
- Currently `migrate()` reads `PRAGMA table_info` and conditionally `ALTER TABLE`s known columns. This works but won't scale once we want index changes, table renames, or backfills.
- Add `schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT)`; convert the existing `event` and runner-metadata column adds into versioned `Migration{Version, Name, Up}` records.
- Defer until a schema change actually wants this — don't refactor speculatively.
- **Files:** `internal/store/sqlite.go`, `internal/store/migrations.go` (new)

---

## Verification gate

Every PR:
1. `mise run check` (fmt + lint + test).
2. `go run ./cmd/smoke` — update `cmd/smoke/main.go` to exercise any new functionality.
3. `./bin/ci-snitch analyze cli/cli --since 7d` — eyeball output for regressions.
4. New analyzers / formatters: golden tests with anonymized data in `internal/*/testdata/`.

## Versioning

Tag a new minor version after each PR merge to main. Every PR delivers value, so every merge is a release. Semver: minor for new features, patch for bug fixes.

## Implementation order

The first six items are unblocked, small, and high-leverage. Do them in order:

1. **3.1** PRAGMA foreign_keys + cascade
2. **3.6** Bounded GraphQL error reads
3. **3.5** Deterministic change-point p-values
4. **3.2** GraphQL truncation diagnostic
5. **3.3** Diagnostic consistency tests
6. **3.4** Tests for `internal/app`

Then perf and depth (4.x → 5.x). Polish (7.x) is opportunistic and can interleave.

## Items intentionally not adopted

These came up in past audits but are either lower-leverage than the work above or speculative:

- **Splitting `Service.Run` further into Planner/Hydrator/etc.** — already factored into helpers; adding more types moves code without a forcing function.
- **Adopting `golang.org/x/time/rate` central limiter** — current per-paginator sleep + GraphQL batching + pre-flight budget covers the cases we have. Revisit only after a real secondary-rate-limit incident.
- **Adopting `gonum`, `go-pretty`, `hashicorp/go-retryablehttp`** — `internal/stats` is small and tested; formatters work; `go-github` already owns HTTP. No concrete pain.
- **"Source-neutral" data interface for future GitLab/CircleCI** — speculative scope. Defer until a second backend is real.
- **Broad string-constant audit beyond 7.1** — current constants in `analyzer.go` are sufficient; only `DetailType()` literals are inconsistent.
- **Migration-framework refactor before a schema change actually demands it** — see 7.4.
