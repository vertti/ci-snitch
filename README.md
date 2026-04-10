# ci-snitch

Analyze GitHub Actions CI workflow performance. Detect outliers, slowdowns, and speedups in your pipelines.

## Install

```bash
go install github.com/vertti/ci-snitch/cmd/ci-snitch@latest
```

Requires the [GitHub CLI](https://cli.github.com) (`gh`) for authentication, or set `GITHUB_TOKEN`.

## Usage

```bash
# Analyze all workflows from the last 60 days (default)
ci-snitch analyze --repo owner/repo

# Filter to a specific workflow and branch
ci-snitch analyze --repo owner/repo --workflow "CI" --branch main

# Last 2 weeks, verbose output
ci-snitch analyze --repo owner/repo --since 2w -v

# Output as JSON (for piping to jq or an LLM)
ci-snitch analyze --repo owner/repo --format json

# Output as markdown (for GitHub issues/PR comments)
ci-snitch analyze --repo owner/repo --format markdown

# Skip cache, fetch fresh data
ci-snitch analyze --repo owner/repo --no-cache
```

## What it detects

**Summary statistics** — mean, median, p95, p99, min, max per workflow and job.

**Outliers** — runs or jobs with abnormally long durations, detected using log-IQR (handles right-skewed CI duration distributions). Reports percentile rank (e.g., "p97 — slower than 97% of runs").

**Change points** — moments when CI performance shifted, detected using CUSUM (Cumulative Sum). Each change point includes:
- Direction (slowdown or speedup) and percentage change
- Before/after mean durations
- Statistical significance via Mann-Whitney U test (p-value)
- The commit SHA at the change point

## Output formats

- `table` (default) — human-readable, grouped by finding type
- `json` — structured JSON, suitable for LLM consumption or programmatic use
- `markdown` — GitHub-flavored markdown tables

## Preprocessing

Before analysis, data is automatically cleaned:
- **Retry deduplication** — keeps only the latest attempt per run
- **Branch filtering** — scope to a single branch with `--branch`
- **Failure exclusion** — only successful runs are analyzed by default (use `--include-failures` to override)

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | (required) | Repository in `owner/repo` format |
| `--since` | `60d` | How far back to analyze (`7d`, `2w`, `3mo`, `2026-01-01`) |
| `--branch` | all | Filter to a specific branch |
| `--workflow` | all | Filter to a specific workflow name |
| `--format` | `table` | Output format: `table`, `json`, `markdown` |
| `--no-cache` | false | Bypass local SQLite cache |
| `--include-failures` | false | Include failed runs in duration analysis |
| `-v, --verbose` | false | Show detailed fetch progress |

## Cache

Run data is cached in a local SQLite database (`~/.cache/ci-snitch/data.db`). Completed runs are immutable and cached permanently. Use `--no-cache` to force a fresh fetch.

## Development

```bash
mise install              # Go 1.26 + golangci-lint
mise run test             # run tests
mise run lint             # run linter
mise run fmt              # run formatter
mise run build            # build binary
go run ./cmd/smoke        # smoke test against cli/cli
```
