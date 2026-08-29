// Package severity defines the OpenTelemetry log severity levels and their
// severity_number values, mirroring otel-sqlite/internal/model/logrecord.go
// (Debug=5, Info=9, Warn=13, Error=17, Fatal=21). It is shared by the UI's
// "this level and above" filter and the mock data generator.
package severity

import "strings"

// Level is an OTel severity level: its text and numeric value.
type Level struct {
	Text   string
	Number int64
}

// All lists the known levels in ascending severity_number order.
var All = []Level{
	{"DEBUG", 5},
	{"INFO", 9},
	{"WARN", 13},
	{"ERROR", 17},
	{"FATAL", 21},
}

// Number returns the severity_number for a case-insensitive severity text,
// or 0 when the text does not match a known level.
func Number(text string) int64 {
	for _, l := range All {
		if strings.EqualFold(l.Text, text) {
			return l.Number
		}
	}
	return 0
}
