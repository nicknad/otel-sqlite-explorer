package api

import (
	"testing"
	"time"
)

func TestParseDateTimeBounds(t *testing.T) {
	// Date-only From starts the day; date-only To ends it (inclusive).
	from, err := parseDateTime("2024-01-15", false)
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	if want := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC).UnixNano(); from != want {
		t.Errorf("from: got %d, want %d", from, want)
	}

	to, err := parseDateTime("2024-01-15", true)
	if err != nil {
		t.Fatalf("to: %v", err)
	}
	if want := time.Date(2024, 1, 15, 23, 59, 59, 999999999, time.UTC).UnixNano(); to != want {
		t.Errorf("to: got %d, want %d", to, want)
	}

	if from >= to {
		t.Errorf("from (%d) must precede to (%d) for the same day", from, to)
	}
}

func TestParseDateTimeFormats(t *testing.T) {
	// RFC3339 honors its offset.
	got, err := parseDateTime("2024-01-15T14:30:00+02:00", false)
	if err != nil {
		t.Fatalf("rfc3339: %v", err)
	}
	if want := time.Date(2024, 1, 15, 12, 30, 0, 0, time.UTC).UnixNano(); got != want {
		t.Errorf("rfc3339: got %d, want %d", got, want)
	}

	// datetime-local has no offset; it must parse without error. When the
	// server zone is UTC (as in CI) it lands on the same civil time in UTC.
	got, err = parseDateTime("2024-01-15T14:30", false)
	if err != nil {
		t.Fatalf("datetime-local: %v", err)
	}
	if time.Local == time.UTC {
		if want := time.Date(2024, 1, 15, 14, 30, 0, 0, time.UTC).UnixNano(); got != want {
			t.Errorf("datetime-local: got %d, want %d", got, want)
		}
	}
}

func TestParseDateTimeInvalid(t *testing.T) {
	for _, s := range []string{"", "garbage", "2024-13-45", "15/01/2024", "2024-01-15T25:00"} {
		if _, err := parseDateTime(s, false); err == nil {
			t.Errorf("%q: expected error, got nil", s)
		}
	}
}
