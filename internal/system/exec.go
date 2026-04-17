// Package system provides helpers for running external commands.
package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const defaultTimeout = 10 * time.Second

var (
	ErrCommandNotFound = errors.New("command not found")
	ErrCommandFailed   = errors.New("command failed")
)

// Run executes a command with a 10-second timeout and returns its stdout.
// Returns ErrCommandNotFound if the binary is not in PATH,
// or ErrCommandFailed with stderr details on non-zero exit.
func Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers pass hardcoded command names
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", ErrCommandNotFound, name)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%w: %s %s: %s", ErrCommandFailed, name, strings.Join(args, " "), detail)
	}

	return strings.TrimSpace(stdout.String()), nil
}
