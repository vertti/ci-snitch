package github

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/vertti/ci-snitch/internal/system"
)

// ResolveToken returns a GitHub API token.
// It checks GITHUB_TOKEN env var first, then falls back to `gh auth token`.
func ResolveToken() (string, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token, nil
	}

	token, err := system.Run(context.Background(), "gh", "auth", "token")
	if err != nil {
		if errors.Is(err, system.ErrCommandNotFound) {
			return "", errors.New("GitHub CLI (gh) not found in PATH. Install it from https://cli.github.com or set GITHUB_TOKEN")
		}
		return "", fmt.Errorf("not authenticated with GitHub CLI. Run `gh auth login` or set GITHUB_TOKEN: %w", err)
	}

	if token == "" {
		return "", errors.New("gh auth token returned empty string — try `gh auth login`")
	}
	return token, nil
}
