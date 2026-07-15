package system

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_ReturnsTrimmedStdout(t *testing.T) {
	out, err := Run(context.Background(), "echo", "hello")
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
}

func TestRun_CommandNotFound(t *testing.T) {
	_, err := Run(context.Background(), "definitely-not-a-real-binary-xyz")
	require.ErrorIs(t, err, ErrCommandNotFound)
}

func TestRun_NonZeroExitIncludesStderr(t *testing.T) {
	_, err := Run(context.Background(), "sh", "-c", "echo boom >&2; exit 1")
	require.ErrorIs(t, err, ErrCommandFailed)
	assert.Contains(t, err.Error(), "boom", "stderr detail must surface")
}

func TestRun_TimeoutSaysTimedOut(t *testing.T) {
	// The user-facing symptom was an opaque "signal: killed" with no hint
	// that a deadline fired.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := Run(ctx, "sleep", "5")
	require.ErrorIs(t, err, ErrCommandFailed)
	assert.Contains(t, err.Error(), "timed out",
		"a deadline kill must say so, not 'signal: killed'")
}
