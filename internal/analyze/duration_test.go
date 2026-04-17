package analyze

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuration_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		dur  Duration
		json string
	}{
		{"zero", Duration(0), `"0s"`},
		{"seconds", Duration(30 * time.Second), `"30s"`},
		{"minutes", Duration(5 * time.Minute), `"5m0s"`},
		{"mixed", Duration(5*time.Minute + 30*time.Second), `"5m30s"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.dur)
			require.NoError(t, err)
			assert.Equal(t, tt.json, string(data))

			var got Duration
			err = json.Unmarshal(data, &got)
			require.NoError(t, err)
			assert.Equal(t, tt.dur, got)
		})
	}
}

func TestDuration_UnmarshalJSON_Invalid(t *testing.T) {
	var d Duration
	err := json.Unmarshal([]byte(`"not-a-duration"`), &d)
	assert.Error(t, err)
}
