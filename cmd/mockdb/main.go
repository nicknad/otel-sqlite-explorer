// Command mockdb generates a demo SQLite database pre-populated with
// realistic OpenTelemetry log data so the explorer UI can be inspected
// against a populated database.
//
// Usage:
//
//	mockdb [-out demo/logs.db] [-rows 3000] [-seed 42] [-fts]
//
// The output database uses the exact production schema (log_resource +
// log_event + logs view) and, by default, includes a contentless logs_fts
// full-text index so the FTS search box is enabled.
package main

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"log-explorer/internal/schema"
	sev "log-explorer/internal/severity"

	_ "modernc.org/sqlite"
)

// severityWeights biases the shared severity model towards a realistic log
// distribution (INFO commonest, FATAL rare). Text and number pairs come from
// severity.All so they stay in sync with the UI's severity filter.
var severityWeights = map[string]int{
	"DEBUG": 8,
	"INFO":  62,
	"WARN":  18,
	"ERROR": 10,
	"FATAL": 2,
}

// attr is a single attribute emitted on a log event.
type attr struct {
	key  string
	kind string // "str" | "int" | "float" | "bool" | "http_status" | "http_method"
}

// service describes a mock emitting service: its host, body templates
// (filled with %s / %d placeholders), the attributes it emits, and the pool
// of string values used for "str" kind attributes.
type service struct {
	name   string
	host   string
	bodies []string
	attrs  []attr
	strs   []string
}

var services = []service{
	{
		name: "api-gateway",
		host: "gw-prod-01",
		bodies: []string{
			"connection timeout to upstream %s",
			"request completed with status %d",
			"rate limit exceeded for client %d",
			"upstream 502 from %s",
			"retrying upstream call attempt %d",
			"invalid request rejected",
		},
		attrs: []attr{
			{"http.method", "http_method"},
			{"http.route", "str"},
			{"http.status_code", "http_status"},
			{"http.user_agent", "str"},
			{"client.ip", "str"},
			{"request.id", "int"},
		},
		strs: []string{"/api/v1/orders", "/api/v1/auth/login", "/api/v1/payments", "/healthz", "/api/v1/inventory",
			"curl/8.0", "Go-http-client/2.0", "Mozilla/5.0", "python-requests/2.31",
			"10.0.4.21", "10.0.4.22", "172.16.8.4", "172.16.8.9"},
	},
	{
		name: "auth-svc",
		host: "auth-prod-02",
		bodies: []string{
			"token validation failed for user %d",
			"login success for user %d",
			"password reset requested for user %d",
			"invalid credentials attempt for user %d",
			"refresh token rotation",
			"MFA challenge issued",
		},
		attrs: []attr{
			{"auth.user_id", "int"},
			{"auth.tenant_id", "int"},
			{"auth.mfa", "bool"},
			{"auth.provider", "str"},
			{"http.status_code", "http_status"},
		},
		strs: []string{"oidc", "okta", "github", "saml", "email", "passwordless"},
	},
	{
		name: "checkout-svc",
		host: "checkout-prod-01",
		bodies: []string{
			"order placed",
			"payment processing for order %d",
			"inventory reservation failed for sku %s",
			"cart updated for user %d",
			"order %d finalized",
			"coupon applied",
		},
		attrs: []attr{
			{"order.id", "int"},
			{"order.total", "float"},
			{"order.currency", "str"},
			{"cart.items", "int"},
			{"user.id", "int"},
		},
		strs: []string{"USD", "EUR", "GBP", "CAD"},
	},
	{
		name: "payments-svc",
		host: "pay-prod-03",
		bodies: []string{
			"charge succeeded for order %d",
			"charge declined for order %d",
			"refund issued for charge %s",
			"webhook signature verification failed",
			"payout scheduled",
			"gateway timeout for charge %s",
		},
		attrs: []attr{
			{"charge.id", "str"},
			{"order.id", "int"},
			{"payment.amount", "float"},
			{"payment.method", "str"},
			{"payment.status_code", "http_status"},
			{"card.brand", "str"},
		},
		strs: []string{"ch_3NqWx9", "ch_3NrA1y", "ch_3NoC3q", "visa", "mastercard", "apple_pay", "amex"},
	},
	{
		name: "inventory-svc",
		host: "inv-prod-01",
		bodies: []string{
			"stock level updated for sku %s",
			"out of stock for sku %s",
			"reindex started",
			"warehouse sync completed",
			"inventory reservation expired for sku %s",
			"backorder created for sku %s",
		},
		attrs: []attr{
			{"sku", "str"},
			{"warehouse.id", "str"},
			{"stock.qty", "int"},
			{"stock.reorder_level", "int"},
		},
		strs: []string{"SKU-1001", "SKU-2033", "SKU-4017", "SKU-8812", "SKU-9044",
			"wh-east-1", "wh-west-2", "wh-eu-central-1"},
	},
	{
		name: "worker-svc",
		host: "worker-prod-05",
		bodies: []string{
			"job dispatched to queue",
			"job %d failed after %d retries",
			"queue depth high (%d pending)",
			"processor heartbeat",
			"dead letter message consumed",
			"job %d completed in %d ms",
		},
		attrs: []attr{
			{"job.id", "int"},
			{"queue.name", "str"},
			{"job.retries", "int"},
			{"worker.id", "str"},
			{"job.duration_ms", "int"},
		},
		strs: []string{"email-queue", "reindex-queue", "payout-queue", "worker-01", "worker-02", "worker-03"},
	},
}

// traceRing keeps a short history of recently generated trace IDs so a
// fraction of events share a trace (mimicking a multi-span request).
const traceRingSize = 8

type generator struct {
	rng    *rand.Rand
	traces [traceRingSize][]byte
	next   int
}

func (g *generator) newTraceID() []byte {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(g.rng.IntN(256)) //nolint:gosec // G115: 0-255 by construction (crypto fallback only)
		}
	}
	return b
}

func (g *generator) newSpanID() []byte {
	b := make([]byte, 8)
	if _, err := cryptorand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(g.rng.IntN(256)) //nolint:gosec // G115: 0-255 by construction (crypto fallback only)
		}
	}
	return b
}

// traceID returns a trace id, reusing a recent one 25% of the time so that
// several events share the same trace (like a request spanning services).
func (g *generator) traceID() []byte {
	if g.rng.IntN(100) < 25 {
		if id := g.traces[g.rng.IntN(traceRingSize)]; id != nil {
			return id
		}
	}
	id := g.newTraceID()
	g.traces[g.next%traceRingSize] = id
	g.next++
	return id
}

func (g *generator) pick[T any](items []T) T {
	return items[g.rng.IntN(len(items))]
}

// attrValue produces a typed value for the given attribute.
func (g *generator) attrValue(a attr, svc *service) any {
	switch a.kind {
	case "int":
		return g.rng.IntN(1_000_000)
	case "float":
		return float64(g.rng.IntN(10_000)) / 100
	case "bool":
		return g.rng.IntN(2) == 0
	case "http_status":
		switch r := g.rng.IntN(100); {
		case r < 70:
			return g.pick([]int{200, 201, 204})
		case r < 85:
			return g.pick([]int{400, 401, 403, 404, 429})
		default:
			return g.pick([]int{500, 502, 503, 504})
		}
	case "http_method":
		return g.pick([]string{"GET", "POST", "PUT", "DELETE", "PATCH"})
	default:
		return g.pick(svc.strs)
	}
}

func (g *generator) eventAttrs(level string, svc *service) string {
	attrs := make(map[string]any, len(svc.attrs)+1)
	for _, a := range svc.attrs {
		attrs[a.key] = g.attrValue(a, svc)
	}
	if level == "ERROR" || level == "FATAL" {
		attrs["error.code"] = fmt.Sprintf("E%d", g.rng.IntN(9999))
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// body fills the placeholders of a randomly chosen body template with
// domain-appropriate values.
func (g *generator) body(svc *service) string {
	tpl := g.pick(svc.bodies)
	switch {
	case strings.Contains(tpl, "%s") && strings.Contains(tpl, "%d"):
		return fmt.Sprintf(tpl, g.pick(upstreams), g.rng.IntN(100_000))
	case strings.Contains(tpl, "%s"):
		return fmt.Sprintf(tpl, g.pick(upstreams))
	case strings.Contains(tpl, "%d"):
		return fmt.Sprintf(tpl, g.rng.IntN(100_000))
	default:
		return tpl
	}
}

// upstreams are the service names substituted into %s body placeholders.
var upstreams = []string{"auth-svc", "checkout-svc", "payments-svc", "inventory-svc"}

func main() {
	out := flag.String("out", "demo/logs.db", "output database path")
	rows := flag.Int("rows", 3000, "number of log events to generate")
	seed := flag.Int64("seed", 42, "random seed (use -1 for a random seed)")
	withFTS := flag.Bool("fts", true, "build a contentless logs_fts full-text index")
	flag.Parse()

	if *rows < 1 {
		log.Fatalf("invalid -rows: %d (must be >= 1)", *rows)
	}

	// A seeded PRNG is intentional here so mock data is reproducible.
	var rng *rand.Rand
	if *seed >= 0 {
		rng = rand.New(rand.NewPCG(uint64(*seed), 0x9e3779b97f4a7c15)) //nolint:gosec // G404: as above
	} else {
		rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid()))) //nolint:gosec // G404: as above
	}
	gen := &generator{rng: rng}
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	// Remove any existing database (and its WAL/shm sidecars) so the tool is
	// safe to re-run for a fresh dataset. Fail loudly if a stale database is
	// still open elsewhere (e.g. a running server), since recreating the schema
	// over it is impossible.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(*out + suffix); err != nil && !os.IsNotExist(err) {
			log.Fatalf("remove existing database %s: %v (close it in any running process first)", *out+suffix, err)
		}
	}

	conn, err := sql.Open("sqlite", *out)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// The mock db is disposable, so disable journaling and fsync for speed.
	for _, p := range []string{"PRAGMA journal_mode=OFF", "PRAGMA synchronous=OFF"} {
		if _, err = conn.ExecContext(ctx, p); err != nil {
			log.Fatalf("%s: %v", p, err)
		}
	}

	if _, err = conn.ExecContext(ctx, schema.DDL); err != nil {
		log.Fatalf("create schema: %v", err)
	}

	// Insert resources (one per service config).
	for i, svc := range services {
		id := fmt.Sprintf("res-%02d", i)
		if _, err = conn.ExecContext(
			ctx,
			"INSERT INTO log_resource (id, service_name, host_name, schema_url) VALUES (?, ?, ?, ?)",
			id, svc.name, svc.host, "https://opentelemetry.io/schemas/1.24.0",
		); err != nil {
			log.Fatalf("insert resource %s: %v", svc.name, err)
		}
	}

	// Pre-compute per-service weights so api-gateway/auth-svc are noisiest.
	resIDs := make([]string, 0, len(services))
	weights := make([]int, 0, len(services))
	var total int
	for i := range services {
		w := len(services[i].bodies) * (20 - i)
		resIDs = append(resIDs, fmt.Sprintf("res-%02d", i))
		weights = append(weights, w)
		total += w
	}

	now := time.Now().UnixNano()
	day := int64(24 * time.Hour)

	// Wrap the inserts in a single transaction for speed.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		log.Fatalf("begin tx: %v", err)
	}

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO log_event
		(timestamp_ns, observed_timestamp_ns, resource_id,
		 severity_number, severity_text, trace_id, span_id, body, attributes_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		log.Fatalf("prepare insert: %v", err)
	}
	defer func() { _ = stmt.Close() }()

	// Events are inserted oldest-first so row ids increase with time.
	for i := range *rows {
		svcIdx := weightedPick(rng, weights, total)
		svc := &services[svcIdx]

		lvl := weightedSeverity(rng)
		// Bias timestamps toward the most recent hour: 60% of events land in
		// the last hour, the rest are spread across the last 24h.
		var age time.Duration
		if rng.IntN(100) < 60 {
			age = time.Duration(rng.Int64N(int64(time.Hour)))
		} else {
			age = time.Duration(rng.Int64N(day))
		}
		ts := now - age.Nanoseconds()

		body := gen.body(svc)
		traceID := gen.traceID()
		spanID := gen.newSpanID()
		attrs := gen.eventAttrs(lvl.Text, svc)

		if _, err = stmt.ExecContext(
			ctx, ts, ts, resIDs[svcIdx],
			lvl.Number, lvl.Text, traceID, spanID, body, attrs,
		); err != nil {
			log.Fatalf("insert event %d: %v", i, err)
		}
	}
	_ = stmt.Close()

	if err = tx.Commit(); err != nil {
		log.Fatalf("commit tx: %v", err)
	}

	if *withFTS {
		if _, err = conn.ExecContext(ctx, `
			CREATE VIRTUAL TABLE logs_fts USING fts5(
				body,
				service_name,
				content=''
			);
			INSERT INTO logs_fts(rowid, body, service_name)
			SELECT id, body, service_name FROM logs;
		`); err != nil {
			log.Fatalf("build fts index: %v", err)
		}
	}

	log.Printf("wrote %d log events to %s (fts=%v)", *rows, *out, *withFTS)
}

// weightedSeverity picks a severity level according to its sampling weight.
func weightedSeverity(rng *rand.Rand) sev.Level {
	total := 0
	for _, l := range sev.All {
		total += severityWeights[l.Text]
	}
	n := rng.IntN(total)
	for _, l := range sev.All {
		n -= severityWeights[l.Text]
		if n < 0 {
			return l
		}
	}
	return sev.All[0]
}

// weightedPick returns an index into items sampled proportionally to weights.
func weightedPick(rng *rand.Rand, weights []int, total int) int {
	n := rng.IntN(total)
	for i, w := range weights {
		n -= w
		if n < 0 {
			return i
		}
	}
	return len(weights) - 1
}
