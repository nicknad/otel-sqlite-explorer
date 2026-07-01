package db

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"

	"log-explorer/internal/compiler"
)

// LogRow represents a single row from the `logs` table.
type LogRow struct {
	ID          int64  `json:"id"`
	Timestamp   int64  `json:"timestamp"`
	Severity    string `json:"severity"`
	ServiceName string `json:"service_name"`
	TraceID     string `json:"trace_id"`
	SpanID      string `json:"span_id"`
	Body        string `json:"body"`
}

// Attr represents a single key/value attribute attached to a log event.
// Only the field matching ValueType is populated; the rest are NULL.
type Attr struct {
	Key         string
	ValueType   string
	StringValue sql.NullString
	IntValue    sql.NullInt64
	DoubleValue sql.NullFloat64
	BoolValue   sql.NullInt64
	BytesValue  []byte
}

// Display renders the attribute's value as a string based on its value_type.
func (a Attr) Display() string {
	switch a.ValueType {
	case "string":
		if a.StringValue.Valid {
			return a.StringValue.String
		}
	case "int":
		if a.IntValue.Valid {
			return strconv.FormatInt(a.IntValue.Int64, 10)
		}
	case "double":
		if a.DoubleValue.Valid {
			return strconv.FormatFloat(a.DoubleValue.Float64, 'f', -1, 64)
		}
	case "bool":
		if a.BoolValue.Valid {
			if a.BoolValue.Int64 != 0 {
				return "true"
			}
			return "false"
		}
	case "bytes":
		if len(a.BytesValue) > 0 {
			return hex.EncodeToString(a.BytesValue)
		}
	}
	return ""
}

// GetAttrs executes a compiled attribute query and returns the matching attributes.
func (c *Client) GetAttrs(cq *compiler.CompiledQuery) ([]Attr, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rows, err := c.db.Query(cq.SQL, cq.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w\nSQL: %s\nArgs: %v", err, cq.SQL, cq.Args)
	}
	defer rows.Close()

	var results []Attr
	for rows.Next() {
		var a Attr
		if err := rows.Scan(&a.Key, &a.ValueType, &a.StringValue, &a.IntValue, &a.DoubleValue, &a.BoolValue, &a.BytesValue); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}
	return results, nil
}

// Execute compiles a query, runs it against the database, and returns matching rows.
func (c *Client) Execute(cq *compiler.CompiledQuery) ([]LogRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rows, err := c.db.Query(cq.SQL, cq.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w\nSQL: %s\nArgs: %v", err, cq.SQL, cq.Args)
	}
	defer rows.Close()

	var results []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.Timestamp, &r.Severity, &r.ServiceName, &r.TraceID, &r.SpanID, &r.Body); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration: %w", err)
	}

	return results, nil
}
