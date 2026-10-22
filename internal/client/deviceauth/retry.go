package deviceauth

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"time"
)

// jitter returns d changed by a random -20 % to +20 % (02 §11.4).
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: timing jitter, not a secret
}

// positiveSeconds reads a whole number of seconds above 0 from an error's params (slow_down's interval), as
// encoding/json decodes it (float64, or json.Number) or as Go code builds it (an int).
func positiveSeconds(v any) (time.Duration, bool) {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	// A day bounds it: the code expires long before, and the poll waits no longer than that anyway.
	if math.IsNaN(f) || f < 1 || f != math.Trunc(f) || f > 86400 {
		return 0, false
	}
	return time.Duration(f) * time.Second, true
}
