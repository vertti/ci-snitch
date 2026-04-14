package analyze

import (
	"fmt"
	"time"
)

// Duration wraps time.Duration with human-readable JSON marshaling.
// Serializes as "5m30s" instead of nanoseconds.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Round delegates to time.Duration.Round.
func (d Duration) Round(m time.Duration) time.Duration { return time.Duration(d).Round(m) }

// MarshalJSON outputs the duration as a human-readable string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "%q", time.Duration(d).Round(time.Second).String()), nil
}
