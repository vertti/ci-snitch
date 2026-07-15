package analyze

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCoreCount(t *testing.T) {
	tests := []struct {
		label string
		want  int
	}{
		// GitHub's split convention for larger runners — previously fell
		// through to the 2-core ubuntu default, producing "undersized —
		// consider larger runner" advice for a 16-core machine.
		{"ubuntu-latest-16-cores", 16},
		{"windows-latest-8-cores", 8},
		{"macos-latest-12-core", 12},
		// Adjacent conventions used by third-party runner vendors.
		{"blacksmith-16vcpu-ubuntu-2404", 16},
		{"ubuntu-22.04-32core", 32},
		{"namespace-profile-4cores", 4},
		// Standard hosted runners fall back to documented defaults.
		{"ubuntu-latest", 2},
		{"windows-latest", 2},
		{"macos-latest", 4},
		// Unknown labels carry no core information.
		{"self-hosted", 0},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			assert.Equal(t, tt.want, parseCoreCount(tt.label))
		})
	}
}
