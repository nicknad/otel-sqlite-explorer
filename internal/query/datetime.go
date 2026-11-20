package query

import (
	"fmt"
	"math"
	"time"
)

// parseDateTime parses a date or datetime string from the HTML form and returns
// the corresponding epoch nanoseconds (UTC). Supported formats:
//
//   - "2006-01-02T15:04"   (datetime-local, interpreted in the server's local
//     time zone, since browsers submit it without an offset)
//   - "2006-01-02T15:04:05" (same, with seconds)
//   - "2006-01-02"         (date only: start of day, or end of day when
//     endOfDay is true for an inclusive upper bound)
//   - RFC3339 / RFC3339Nano (e.g. "2024-01-15T14:30:00Z", offset honored)
func parseDateTime(s string, endOfDay bool) (int64, error) {
	// datetime-local carries no offset; interpret it in local time.
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return toUnixNano(t)
		}
	}
	// RFC3339 timestamps carry their own offset.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return toUnixNano(t)
		}
	}
	// Date only — start or end of that day (UTC).
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse date: %q", s)
	}
	if endOfDay {
		// End of day: 23:59:59.999999999 UTC
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, time.UTC)
		return toUnixNano(end)
	}
	return toUnixNano(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
}

// unixNanoMin/unixNanoMax bound the representable int64 nanosecond range
// (1677-09-21T00:12:43Z to 2262-04-11T23:47:16Z). time.Time.UnixNano is
// undefined outside it, so out-of-range form values must be rejected.
var (
	unixNanoMin = time.Unix(0, math.MinInt64).UTC()
	unixNanoMax = time.Unix(0, math.MaxInt64).UTC()
)

// toUnixNano converts t to UTC epoch nanoseconds, rejecting timestamps whose
// UnixNano would silently wrap.
func toUnixNano(t time.Time) (int64, error) {
	t = t.UTC()
	if t.Before(unixNanoMin) || t.After(unixNanoMax) {
		return 0, fmt.Errorf("%s is outside the supported range (1677-09-21 to 2262-04-11)", t.Format("2006-01-02"))
	}
	return t.UnixNano(), nil
}
