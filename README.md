# ci-snitch

[![CI](https://github.com/vertti/ci-snitch/actions/workflows/ci.yml/badge.svg)](https://github.com/vertti/ci-snitch/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/vertti/ci-snitch)](https://goreportcard.com/report/github.com/vertti/ci-snitch)
[![Release](https://img.shields.io/github/v/release/vertti/ci-snitch)](https://github.com/vertti/ci-snitch/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Find your slowest CI workflows, catch the commit that broke them, and stop burning CI minutes.

ci-snitch analyzes your GitHub Actions history and tells you what's slow, what changed, and when.

```
ci-snitch analyze --repo cli/cli --since 7d

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
ci-snitch analyze --repo your-org/your-repo
```

That's it. Fetches the last 60 days of workflow data and shows you what matters.

## What it finds

**Where your CI time goes** — ranked breakdown of every workflow and job by total compute time, with median, p95, and volatility scoring.

**Abnormally slow runs** — flags runs and jobs that took way longer than normal. Uses log-IQR to handle the right-skewed distributions typical of CI durations.

**Performance regressions (and improvements)** — detects the exact point where a job got slower or faster, the commit that caused it, and whether the change stuck. Statistical significance via Mann-Whitney U test so you're not chasing noise.

## Output formats

```bash
# Human-readable table (default)
ci-snitch analyze --repo owner/repo

# JSON — pipe to jq, feed to an LLM, build dashboards
ci-snitch analyze --repo owner/repo --format json

# Markdown — paste into GitHub issues or PR comments
ci-snitch analyze --repo owner/repo --format markdown

# LLM — structured context for Claude Code or similar tools
ci-snitch analyze --repo owner/repo --format llm
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | (required) | Repository in `owner/repo` format |
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
