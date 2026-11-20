package query

import "fmt"

// ValidationError reports a rule violation caused by the caller's input; it is
// the pipeline's only typed error. Anything else is a server-side failure.
//
// Adapters render their own client-facing text from Rule and Detail so
// response bodies stay byte-identical to the legacy handlers: the JSON API
// writes Error() (which carries the "validation: " framing) for every rule,
// while the form path renders Detail alone for the two rules whose legacy
// messages were never prefixed (limit and offset parse errors).
type ValidationError struct {
	// Rule names the violated rule: "limit", "offset", "severity",
	// "date_from", "date_to", "dsl", "match", or "compile".
	Rule string
	// Detail is the rule-specific message without path-specific framing.
	Detail string
}

// Error renders the JSON API's legacy framing.
func (e *ValidationError) Error() string { return "validation: " + e.Detail }

func validationf(rule, format string, args ...any) *ValidationError {
	return &ValidationError{Rule: rule, Detail: fmt.Sprintf(format, args...)}
}
