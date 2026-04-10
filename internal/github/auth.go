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

	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("no GITHUB_TOKEN set and `gh auth token` failed: %w", err)
	}

	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gh auth token returned empty string")
	}
	return token, nil
}
