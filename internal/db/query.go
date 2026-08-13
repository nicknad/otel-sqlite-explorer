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
	ServiceName    string `json:"service_name"`
	TraceID        string `json:"trace_id"`
	SpanID         string `json:"span_id"`
	Body           string `json:"body"`
	AttributesJSON string `json:"attributes_json"`
}

// Execute compiles a query, runs it against the database, and returns matching rows.
func (c *Client) Execute(cq *compiler.CompiledQuery) ([]LogRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rows, err := c.db.QueryContext(context.Background(), cq.SQL, cq.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w\nSQL: %s\nArgs: %v", err, cq.SQL, cq.Args)
	}
	defer func() { _ = rows.Close() }()

	var results []LogRow
	for rows.Next() {
		var r LogRow
		// text columns are nullable
		// default to ""
		var severity, serviceName, traceId, spanId, body, attrs sql.NullString

		if err := rows.Scan(&r.ID, &r.Timestamp, &severity, &serviceName, &traceId, &spanId, &body, &attrs); err != nil {
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
