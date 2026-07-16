# ci-snitch

## Workflow

- After writing or editing Go code, always run `mise run fmt` first, then `mise run lint` to catch remaining issues.
- Use `mise run check` to run format + lint + test in one go.
- Never manually fix formatting issues that `mise run fmt` would handle (import ordering, gofmt alignment, etc.).

## Smoke testing

- Before opening or updating a PR, always run the smoke test against a real repo: `go run ./cmd/smoke`
- Update `cmd/smoke/main.go` to exercise any new functionality added in the PR.
- The smoke test defaults to `cli/cli` (public). You can pass any `owner/repo` as an argument.

## Releases

- goreleaser fills release notes with a raw commit list. After verifying a release succeeded, rewrite its GitHub release notes by hand (`gh release edit vX.Y.Z --notes-file …`): thematic `##` sections with benefit-first headings, `**bold lead** — explanation` bullets that say what was broken and why it matters, concrete numbers where available. Match the style of v0.17.0–v0.23.0.

## Test data

- Golden file test fixtures in `internal/*/testdata/` must use anonymized data (example-org/example-repo). Never commit real repo names, URLs, or API responses from private repositories.
