# ci-snitch

[![CI](https://github.com/vertti/ci-snitch/actions/workflows/ci.yml/badge.svg)](https://github.com/vertti/ci-snitch/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/vertti/ci-snitch)](https://goreportcard.com/report/github.com/vertti/ci-snitch)
[![Release](https://img.shields.io/github/v/release/vertti/ci-snitch)](https://github.com/vertti/ci-snitch/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

CI performance intelligence for GitHub Actions. Detect regressions, flaky pipelines, cost hotspots, and volatile jobs — then pinpoint the commit that caused it.

ci-snitch analyzes your workflow history and surfaces what matters: where your CI minutes go, which pipelines are unreliable, what got slower (and whether it stuck), and where to invest effort for maximum impact.

## Features

- **Triage header** — top offenders by CI time, volatility, and active regressions at a glance
- **Change point detection** — CUSUM algorithm with Mann-Whitney significance testing finds the exact commit that made things slower (or faster), and whether the change stuck
- **Oscillation detection** — volatile jobs that bounce up/down are separated from real regressions
- **Failure & flakiness analysis** — failure rates, conclusion breakdowns, rerun tax, and failing step attribution
- **Step-level timing** — identifies which steps within a job consume the most time
- **Cost estimation** — billable minutes by runner type (including self-hosted and larger runners) with daily rate and "bang for buck" priority scoring
- **Volatility scoring** — p95/median ratio classifies each workflow as stable, variable, spiky, or volatile
- **Outlier detection** — Log-IQR and MAD methods, grouped by job with worst-case summary
- **Matrix job grouping** — collapses matrix variants into aggregate stats
- **LLM-ready output** — `--format llm` produces a briefing with context, prioritized findings, investigation prompts, and structured JSON that Claude Code or similar tools can act on immediately
- **Multiple formats** — table (ANSI), JSON, markdown, and LLM
- **Local SQLite cache** — completed runs cached permanently, incremental fetches only

<picture>
  <img alt="ci-snitch analyzing cli/cli" src="doc/demo.svg">
</picture>

## Install

```bash
# Homebrew
brew install vertti/tap/ci-snitch

# Binary (macOS, Linux)
curl -fsSL https://raw.githubusercontent.com/vertti/ci-snitch/main/install.sh | sh
```

Authenticates via `GITHUB_TOKEN` env var, or falls back to the [GitHub CLI](https://cli.github.com) (`gh auth token`).

## Quick start

```bash
# From inside a GitHub repo — auto-detects owner/repo from git remote
ci-snitch analyze

# Or specify explicitly
ci-snitch analyze your-org/your-repo
```

That's it. Fetches the last 60 days of workflow data and shows you what matters.

## What it finds

**Where your CI time goes** — ranked breakdown of every workflow and job by total compute time, with median, p95, and volatility scoring. Billable minutes by runner type with daily rate and "bang for buck" priority scoring.

**Pipeline critical path** — maps sequential and parallel stages within each workflow. Identifies what determines wall-clock time vs. what runs in parallel ("Deploy to Test: 43% of wall-clock, waits for tests").

**Performance regressions** — detects the exact commit where a job got slower or faster, and whether the change persisted. Volatile jobs that bounce up/down are separated from real regressions so you don't chase noise.

**Failure hotspots** — failure rates with conclusion breakdowns (failure vs. cancelled vs. timed out), rerun tax, and failing step attribution. "tests: 23% failure rate — fails at: Lint and Format Check" gives an immediate triage target.

**Step-level bottlenecks** — within each job, identifies which steps consume the most time and flags high-variance steps. "Docker build: 36% of job, 3.8x volatility" points at caching issues.

**Outliers** — flags runs and jobs that took way longer than normal, grouped by job with worst-case summary.

## Output formats

```bash
# Human-readable table (default)
ci-snitch analyze owner/repo

# JSON — pipe to jq, feed to an LLM, build dashboards
ci-snitch analyze owner/repo --format json

# Markdown — paste into GitHub issues or PR comments
ci-snitch analyze owner/repo --format markdown

# LLM — structured context for Claude Code or similar tools
ci-snitch analyze owner/repo --format llm
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `[owner/repo]` | auto-detect | Repository to analyze; if omitted, detected from git remote |
| `--since` | `60d` | How far back: `7d`, `2w`, `3mo`, or `2026-01-01` |
| `--branch` | all | Filter to a specific branch |
| `--workflow` | all | Filter to a specific workflow name |
| `--format` | `table` | `table`, `json`, `markdown`, or `llm` |
| `--no-cache` | false | Bypass local cache, fetch fresh |
| `--include-failures` | false | Include failed runs in analysis |
| `-v` | false | Verbose output with per-phase timing |

## Development

```bash
mise install              # Go 1.26 + golangci-lint
mise run check            # format + lint + test
go run ./cmd/smoke        # smoke test against cli/cli
```
