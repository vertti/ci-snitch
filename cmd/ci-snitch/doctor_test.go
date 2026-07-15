package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunChecks_AllPass(t *testing.T) {
	var buf bytes.Buffer
	err := runChecks(&buf, []doctorCheck{
		{name: "token", run: func() (string, error) { return "resolved from env", nil }},
		{name: "cache", run: func() (string, error) { return "writable", nil }},
	})
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "ok   token: resolved from env")
	assert.Contains(t, out, "ok   cache: writable")
}

func TestRunChecks_FailureIsNonFatalToLaterChecksButErrors(t *testing.T) {
	var buf bytes.Buffer
	order := []string{}
	err := runChecks(&buf, []doctorCheck{
		{name: "token", run: func() (string, error) { order = append(order, "token"); return "", errors.New("no token") }},
		{name: "cache", run: func() (string, error) { order = append(order, "cache"); return "writable", nil }},
	})
	require.Error(t, err, "any failed check must produce a non-zero exit")
	assert.Equal(t, []string{"token", "cache"}, order, "later checks still run — the point is a full report")
	out := buf.String()
	assert.Contains(t, out, "FAIL token: no token")
	assert.Contains(t, out, "ok   cache: writable")
}

func TestRunChecks_InfoOnlyChecksDoNotFail(t *testing.T) {
	var buf bytes.Buffer
	err := runChecks(&buf, []doctorCheck{
		{name: "git remote", informational: true, run: func() (string, error) {
			return "", errors.New("not a git repository")
		}},
	})
	require.NoError(t, err, "informational checks report but never fail doctor")
	assert.Contains(t, buf.String(), "note git remote: not a git repository")
}
