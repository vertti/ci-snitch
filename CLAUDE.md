# ci-snitch

## Workflow

- After writing or editing Go code, always run `mise run fmt` first, then `mise run lint` to catch remaining issues.
- Use `mise run check` to run format + lint + test in one go.
- Never manually fix formatting issues that `mise run fmt` would handle (import ordering, gofmt alignment, etc.).

## Test data

- Golden file test fixtures in `internal/*/testdata/` must use anonymized data (example-org/example-repo). Never commit real repo names, URLs, or API responses from private repositories.
