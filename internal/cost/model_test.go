package cost

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLookupMultiplier(t *testing.T) {
	tests := []struct {
		labels []string
		want   float64
	}{
		{[]string{"ubuntu-latest"}, 1},
		{[]string{"macos-latest"}, 10},
		{[]string{"windows-latest"}, 2},
		{[]string{"self-hosted", "linux"}, 1},     // no match, default 1
		{[]string{"Ubuntu-Latest"}, 1},            // case insensitive
		{[]string{"self-hosted", "macos-14"}, 10}, // matches macos-14
		{nil, 1},
		{[]string{}, 1},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			assert.InDelta(t, tt.want, LookupMultiplier(tt.labels), 0.001)
		})
	}
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
