// Package github provides a client for fetching GitHub Actions workflow data.
package github

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ResolveToken returns a GitHub API token.
// It checks GITHUB_TOKEN env var first, then falls back to `gh auth token`.
func ResolveToken() (string, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token, nil
	}

	if _, err := exec.LookPath("gh"); err != nil {
		return "", errors.New("GitHub CLI (gh) not found in PATH. Install it from https://cli.github.com or set GITHUB_TOKEN")
	}

	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("not authenticated with GitHub CLI. Run `gh auth login` or set GITHUB_TOKEN: %w", err)
	}

	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gh auth token returned empty string — try `gh auth login`")
	}
	return token, nil
}
