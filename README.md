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

```
ci-snitch analyze cli/cli --since 7d

Unit and Integration Tests  73 runs, median 8m33s, p95 15m01s, total 11h53m
  ├─ Integration tests (2, 3)  73 runs  median 7m29s  p95 13m58s
  ├─ Integration tests (1, 3)  73 runs  median 7m11s  p95 13m26s
  ├─ Integration tests (3, 3)  73 runs  median 6m29s  p95 12m37s
  ├─ Unit tests                73 runs  median 3m15s  p95 3m53s
  └─ Merge artifacts           73 runs  median 14s    p95 20s

Deploy Test Environment  23 runs, median 23m17s, p95 48m34s, total 11h2m
  ├─ Deploy to Test              23 runs  median 10m40s  p95 29m40s
  ├─ tests / Integration (2, 3) 23 runs  median 7m18s   p95 13m29s
  └─ ...

── Change Points (3) ──
DIR  JOB                  CHANGE  BEFORE  AFTER  DATE        COMMIT    P-VALUE
▲    Unit tests           +16%    3m22s   3m54s  2026-04-09  1330e058  0.0258
▼    Integration (3, 3)   -20%    8m27s   6m44s  2026-04-09  31f2edc5  0.0014
▲    Build and push admin +40%    2m45s   3m51s  2026-04-10  af9a58c1  0.0660

390 runs analyzed (2026-04-05 to 2026-04-11)
```

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

**Where your CI time goes** — ranked breakdown of every workflow and job by total compute time, with median, p95, and volatility scoring.

**Abnormally slow runs** — flags runs and jobs that took way longer than normal. Uses log-IQR to handle the right-skewed distributions typical of CI durations.

**Performance regressions (and improvements)** — detects the exact point where a job got slower or faster, the commit that caused it, and whether the change stuck. Statistical significance via Mann-Whitney U test so you're not chasing noise.

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
