package github

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveToken_EnvVarWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")
	token, err := ResolveToken()
	require.NoError(t, err)
	assert.Equal(t, "env-token", token)
}

// fakeGh puts an executable `gh` script into PATH and clears GITHUB_TOKEN.
func fakeGh(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755)) //nolint:gosec // test script must be executable
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", dir)
}

func TestResolveToken_GhMissingPointsToInstall(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no gh anywhere
	_, err := ResolveToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cli.github.com")
	assert.Contains(t, err.Error(), "GITHUB_TOKEN")
}

func TestResolveToken_GhUnauthenticated(t *testing.T) {
	fakeGh(t, "echo 'not logged in' >&2; exit 1")
	_, err := ResolveToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gh auth login")
}

func TestResolveToken_GhEmptyToken(t *testing.T) {
	fakeGh(t, "exit 0")
	_, err := ResolveToken()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestResolveToken_GhToken(t *testing.T) {
	fakeGh(t, "echo cli-token")
	token, err := ResolveToken()
	require.NoError(t, err)
	assert.Equal(t, "cli-token", token)
}
