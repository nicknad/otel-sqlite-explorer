// Command mockdb generates a demo SQLite database pre-populated with
// realistic OpenTelemetry log data so the explorer UI can be inspected
// against a populated database.
//
// Usage:
//
//	mockdb [-out demo/logs.db] [-rows 3000] [-seed 42] [-fts]
//
// The output database uses the synthetic demo schema from internal/schema
// (log_resource + log_event + logs view) and, by default, a contentless
// logs_fts full-text index maintained by the producer's trigger pair, so
// the FTS search box is enabled with the same semantics the real sink
// installs.
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

	"log-explorer/internal/democorpus"
	"log-explorer/internal/schema"
	sev "log-explorer/internal/severity"

	_ "modernc.org/sqlite"
)

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
			b[i] = byte(g.rng.IntN(256)) // #nosec G115 -- 0-255 by construction (crypto fallback only)
		}
	}
	return b
}

func (g *generator) newSpanID() []byte {
	b := make([]byte, 8)
	if _, err := cryptorand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(g.rng.IntN(256)) // #nosec G115 -- 0-255 by construction (crypto fallback only)
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
func (g *generator) attrValue(a democorpus.Attr, svc *democorpus.Service) any {
	switch a.Kind {
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
		return g.pick(svc.Strs)
	}
}

func (g *generator) eventAttrs(level string, svc *democorpus.Service) string {
	attrs := make(map[string]any, len(svc.Attrs)+1)
	for _, a := range svc.Attrs {
		attrs[a.Key] = g.attrValue(a, svc)
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

// body fills every placeholder of a randomly chosen body template.
// %d verbs get random numbers; %s verbs get domain-appropriate values (SKU
// ids for stock templates, charge ids for payment templates, upstream
// service names otherwise) so generated bodies read realistically.
func (g *generator) body(svc *democorpus.Service) string {
	tpl := g.pick(svc.Bodies)
	var sb strings.Builder
	for i := 0; i < len(tpl); {
		if tpl[i] == '%' && i+1 < len(tpl) && (tpl[i+1] == 's' || tpl[i+1] == 'd') {
			if tpl[i+1] == 's' {
				sb.WriteString(g.stringSub(tpl, svc))
			} else {
				fmt.Fprintf(&sb, "%d", g.rng.IntN(100_000))
			}
			i += 2
			continue
		}
		sb.WriteByte(tpl[i])
		i++
	}
	return sb.String()
}

// stringSub picks a %s substitution fitting the template's domain.
func (g *generator) stringSub(tpl string, svc *democorpus.Service) string {
	lower := strings.ToLower(tpl)
	switch {
	case strings.Contains(lower, "sku"):
		if v := g.pickWithPrefix(svc.Strs, "SKU-"); v != "" {
			return v
		}
		// Cross-service templates (e.g. checkout-svc's reservation failure)
		// mention SKUs without stocking them; borrow from any pool that has
		// them rather than dropping in a service name.
		for i := range democorpus.Services {
			if v := g.pickWithPrefix(democorpus.Services[i].Strs, "SKU-"); v != "" {
				return v
			}
		}
	case strings.Contains(lower, "charge"):
		if v := g.pickWithPrefix(svc.Strs, "ch_"); v != "" {
			return v
		}
	}
	return g.pick(democorpus.Upstreams)
}

// pickWithPrefix returns a random entry with the given prefix, or "" when
// the pool holds none.
func (g *generator) pickWithPrefix(pool []string, prefix string) string {
	var matches []string
	for _, s := range pool {
		if strings.HasPrefix(s, prefix) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return ""
	}
	return g.pick(matches)
}

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
		rng = rand.New(rand.NewPCG(uint64(*seed), 0x9e3779b97f4a7c15)) // #nosec G404 -- weak RNG intentional for reproducible mock data
	} else {
		rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid()))) // #nosec G115 G404 -- timestamp/pid are non-negative; weak RNG intentional
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
	for i, svc := range democorpus.Services {
		id := fmt.Sprintf("res-%02d", i)
		if _, err = conn.ExecContext(
			ctx,
			"INSERT INTO log_resource (id, service_name, host_name, schema_url) VALUES (?, ?, ?, ?)",
			id, svc.Name, svc.Host, "https://opentelemetry.io/schemas/1.24.0",
		); err != nil {
			log.Fatalf("insert resource %s: %v", svc.Name, err)
		}
	}

	// Pre-compute per-service weights so api-gateway/auth-svc are noisiest.
	resIDs := make([]string, 0, len(democorpus.Services))
	weights := make([]int, 0, len(democorpus.Services))
	var total int
	for i := range democorpus.Services {
		w := len(democorpus.Services[i].Bodies) * (20 - i)
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
		svc := &democorpus.Services[svcIdx]

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
				content='',
				contentless_delete=1
			);
			INSERT INTO logs_fts(rowid, body, service_name)
			SELECT id, body, service_name FROM logs;
			`+schema.FTSTriggers+`
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
		total += democorpus.SeverityWeights[l.Text]
	}
	n := rng.IntN(total)
	for _, l := range sev.All {
		n -= democorpus.SeverityWeights[l.Text]
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
