package db

import (
	"context"
	"database/sql"
	"fmt"

	"log-explorer/internal/compiler"
)

// LogRow represents a single row from the `logs` table.
type LogRow struct {
	ID             int64  `json:"id"`
	Timestamp      int64  `json:"timestamp"`
	Severity       string `json:"severity"`
	SeverityNumber int64  `json:"severity_number"`
	ServiceName    string `json:"service_name"`
	TraceID        string `json:"trace_id"`
	SpanID         string `json:"span_id"`
	Body           string `json:"body"`
	AttributesJSON string `json:"attributes_json"`
}

// Execute compiles a query, runs it against the database, and returns matching rows.
// Each query is bounded by the client's timeout so a hung operation cannot
// block the HTTP handler forever.
//
// Errors carry only the database message, never the SQL text or bound
// arguments: the API layer logs those server-side and returns this error to
// the client, so echoing them here would leak internals.
func (c *Client) Execute(cq *compiler.CompiledQuery) ([]LogRow, error) {
	return c.ExecuteContext(context.Background(), cq)
}

// ExecuteContext is Execute bound to the caller's context. When the HTTP
// client disconnects, the in-flight SQLite query is canceled instead of
// holding the single connection until the timeout expires.
func (c *Client) ExecuteContext(ctx context.Context, cq *compiler.CompiledQuery) ([]LogRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Fail fast when the deadline has already passed (e.g. a zero/negative
	// configured timeout) instead of starting the query.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	rows, err := c.db.QueryContext(ctx, cq.SQL, cq.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	fields := cq.Fields
	if fields == nil {
		fields = compiler.DefaultFields
	}

	// Non-nil so empty result sets encode as [] rather than null in JSON.
	results := make([]LogRow, 0)
	for rows.Next() {
		var r LogRow
		// text columns are nullable
		// default to ""
		var severity, serviceName, traceId, spanId, body, attrs sql.NullString

		targets := make([]any, len(fields))
		for i, f := range fields {
			switch f {
			case "id":
				targets[i] = &r.ID
			case "timestamp":
				targets[i] = &r.Timestamp
			case "severity":
				targets[i] = &severity
			case "severity_number":
				targets[i] = &r.SeverityNumber
			case "service_name":
				targets[i] = &serviceName
			case "trace_id":
				targets[i] = &traceId
			case "span_id":
				targets[i] = &spanId
			case "body":
				targets[i] = &body
			case "attributes_json":
				targets[i] = &attrs
			default:
				return nil, fmt.Errorf("scan: unsupported field %q", f)
			}
		}

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Severity = severity.String
		r.ServiceName = serviceName.String
		r.TraceID = traceId.String
		r.SpanID = spanId.String
		r.Body = body.String
		r.AttributesJSON = attrs.String
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}

	return results, nil
}
