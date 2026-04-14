package cost

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLookupMultiplier(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   float64
	}{
		{"ubuntu standard", []string{"ubuntu-latest"}, 1},
		{"macos standard", []string{"macos-latest"}, 10},
		{"windows standard", []string{"windows-latest"}, 2},
		{"case insensitive", []string{"Ubuntu-Latest"}, 1},
		{"nil labels", nil, 1},
		{"empty labels", []string{}, 1},
		// Self-hosted runners are free
		{"self-hosted linux", []string{"self-hosted", "linux"}, 0},
		{"self-hosted macos", []string{"self-hosted", "macos-14"}, 0},
		{"self-hosted case insensitive", []string{"Self-Hosted", "linux"}, 0},
		// Larger runners
		{"ubuntu 4 cores", []string{"ubuntu-latest-4-cores"}, 2},
		{"ubuntu 8 cores", []string{"ubuntu-latest-8-cores"}, 4},
		{"ubuntu 16 cores", []string{"ubuntu-latest-16-cores"}, 8},
		{"ubuntu 32 cores", []string{"ubuntu-latest-32-cores"}, 16},
		{"ubuntu 64 cores", []string{"ubuntu-latest-64-cores"}, 32},
		{"windows 8 cores", []string{"windows-latest-8-cores"}, 8},
		{"macos 12 core", []string{"macos-latest-12-core"}, 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, LookupMultiplier(tt.labels), 0.001)
		})
	}
}

func TestIsSelfHosted(t *testing.T) {
	assert.True(t, IsSelfHosted([]string{"self-hosted", "linux"}))
	assert.True(t, IsSelfHosted([]string{"Self-Hosted", "ARM64"}))
	assert.False(t, IsSelfHosted([]string{"ubuntu-latest"}))
	assert.False(t, IsSelfHosted(nil))
}

func TestBillableMinutes(t *testing.T) {
	tests := []struct {
		dur  time.Duration
		want float64
	}{
		{30 * time.Second, 1}, // rounds up to 1 minute
		{1 * time.Minute, 1},  // exactly 1 minute
		{61 * time.Second, 2}, // rounds up to 2 minutes
		{5*time.Minute + 1*time.Second, 6},
		{0, 0},
		{-1 * time.Minute, 0},
	}

	for _, tt := range tests {
		t.Run(tt.dur.String(), func(t *testing.T) {
			assert.InDelta(t, tt.want, BillableMinutes(tt.dur), 0.001)
		})
	}
}
