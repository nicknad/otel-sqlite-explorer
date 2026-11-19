// Command sinkdemo generates a demo SQLite database by pushing realistic OTLP
// log traffic through the published otel-sqlite sink container and harvesting
// the result — the same ingestion path a real deployment uses.
//
// Usage:
//
//	sinkdemo [-out demo/sink-logs.db] [-rows 3000] [-image ghcr.io/nicknad/otel-sqlite:latest]
//
// Unlike cmd/mockdb, which writes the synthetic demo schema directly, this
// tool proves the full round trip: OTLP/HTTP JSON -> bridge collector ->
// sink (OTLP/gRPC) -> SQLite. The produced database carries the sink's own
// migrations, search-index state and trigger-maintained FTS index, so it is
// exactly the artifact a user would mount next to the explorer. Requires a
// Docker daemon; the default image matches internal/tests/sink_image_test.go
// and OTEL_SQLITE_IMAGE=<ref> overrides it.
package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sev "log-explorer/internal/severity"

	_ "modernc.org/sqlite"
)

const (
	defaultSinkImage = "ghcr.io/nicknad/otel-sqlite:latest"
	bridgeImage      = "ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.159.0"
)

// severityWeights biases the demo traffic towards a realistic log
// distribution (INFO commonest, FATAL rare), matching cmd/mockdb.
var severityWeights = map[string]int{"DEBUG": 8, "INFO": 62, "WARN": 18, "ERROR": 10, "FATAL": 2}

var upstreams = []string{"auth-svc", "checkout-svc", "payments-svc", "inventory-svc"}

type attrSpec struct {
	key  string
	kind string // "str" | "int" | "float" | "bool" | "status" | "method"
}

type service struct {
	name   string
	host   string
	bodies []string
	attrs  []attrSpec
	strs   []string
}

var services = []service{
	{
		name: "api-gateway", host: "gw-prod-01",
		bodies: []string{
			"connection timeout to upstream %s",
			"request completed with status %d",
			"rate limit exceeded for client %d",
			"upstream 502 from %s",
			"retrying upstream call attempt %d",
			"invalid request rejected",
		},
		attrs: []attrSpec{
			{"http.method", "method"},
			{"http.route", "str"},
			{"http.status_code", "status"},
			{"http.user_agent", "str"},
			{"client.ip", "str"},
			{"request.id", "int"},
		},
		strs: []string{"/api/v1/orders", "/api/v1/auth/login", "/api/v1/payments", "/healthz", "/api/v1/inventory",
			"curl/8.0", "Go-http-client/2.0", "Mozilla/5.0", "python-requests/2.31",
			"10.0.4.21", "10.0.4.22", "172.16.8.4", "172.16.8.9"},
	},
	{
		name: "auth-svc", host: "auth-prod-02",
		bodies: []string{
			"token validation failed for user %d",
			"login success for user %d",
			"password reset requested for user %d",
			"invalid credentials attempt for user %d",
			"refresh token rotation",
			"MFA challenge issued",
		},
		attrs: []attrSpec{
			{"auth.user_id", "int"},
			{"auth.tenant_id", "int"},
			{"auth.mfa", "bool"},
			{"auth.provider", "str"},
			{"http.status_code", "status"},
		},
		strs: []string{"oidc", "okta", "github", "saml", "email", "passwordless"},
	},
	{
		name: "checkout-svc", host: "checkout-prod-01",
		bodies: []string{
			"order placed",
			"payment processing for order %d",
			"inventory reservation failed for sku %s",
			"cart updated for user %d",
			"order %d finalized",
			"coupon applied",
		},
		attrs: []attrSpec{
			{"order.id", "int"},
			{"order.total", "float"},
			{"order.currency", "str"},
			{"cart.items", "int"},
			{"user.id", "int"},
		},
		strs: []string{"USD", "EUR", "GBP", "CAD"},
	},
	{
		name: "payments-svc", host: "pay-prod-03",
		bodies: []string{
			"charge succeeded for order %d",
			"charge declined for order %d",
			"refund issued for charge %s",
			"webhook signature verification failed",
			"payout scheduled",
			"gateway timeout for charge %s",
		},
		attrs: []attrSpec{
			{"charge.id", "str"},
			{"order.id", "int"},
			{"payment.amount", "float"},
			{"payment.method", "str"},
			{"payment.status_code", "status"},
		},
		strs: []string{"ch_3NqWx9", "ch_3NrA1y", "ch_3NoC3q", "visa", "mastercard", "apple_pay", "amex"},
	},
	{
		name: "inventory-svc", host: "inv-prod-01",
		bodies: []string{
			"stock level updated for sku %s",
			"out of stock for sku %s",
			"reindex started",
			"warehouse sync completed",
			"inventory reservation expired for sku %s",
			"backorder created for sku %s",
		},
		attrs: []attrSpec{
			{"sku", "str"},
			{"warehouse.id", "str"},
			{"stock.qty", "int"},
			{"stock.reorder_level", "int"},
		},
		strs: []string{"SKU-1001", "SKU-2033", "SKU-4017", "SKU-8812", "SKU-9044",
			"wh-east-1", "wh-west-2", "wh-eu-central-1"},
	},
	{
		name: "worker-svc", host: "worker-prod-05",
		bodies: []string{
			"job dispatched to queue",
			"job %d failed after %d retries",
			"queue depth high (%d pending)",
			"processor heartbeat",
			"dead letter message consumed",
			"job %d completed in %d ms",
		},
		attrs: []attrSpec{
			{"job.id", "int"},
			{"queue.name", "str"},
			{"job.retries", "int"},
			{"worker.id", "str"},
			{"job.duration_ms", "int"},
		},
		strs: []string{"email-queue", "reindex-queue", "payout-queue", "worker-01", "worker-02", "worker-03"},
	},
}

// attrValue is one typed OTLP attribute value.
type attrValue struct {
	kind  string // "stringValue" | "intValue" | "doubleValue" | "boolValue"
	value any
}

type recAttr struct {
	key string
	v   attrValue
}

// record is one generated OTLP log record before JSON encoding.
type record struct {
	svc   *service
	lvl   sev.Level
	ts    int64
	body  string
	trace []byte // nil -> omit traceId/spanId (NULL blobs)
	span  []byte
	attrs []recAttr
	event string
}

type generator struct {
	rng    *mathrand.Rand
	traces [8][]byte
	next   int
}

func (g *generator) pick[T any](items []T) T { return items[g.rng.IntN(len(items))] }

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

func (g *generator) traceID(i int) []byte {
	if i%100 < 15 {
		return nil // exercise NULL trace/span blobs
	}
	if g.rng.IntN(100) < 25 {
		if id := g.traces[g.rng.IntN(len(g.traces))]; id != nil {
			return id // shared trace: a request spanning services
		}
	}
	id := make([]byte, 16)
	for j := range id {
		id[j] = byte(g.rng.IntN(256)) // #nosec G115 -- 0-255 by construction
	}
	g.traces[g.next%len(g.traces)] = id
	g.next++
	return id
}

func (g *generator) spanID() []byte {
	b := make([]byte, 8)
	for j := range b {
		b[j] = byte(g.rng.IntN(256)) // #nosec G115 -- 0-255 by construction
	}
	return b
}

func (g *generator) genAttrValue(a attrSpec, svc *service) attrValue {
	switch a.kind {
	case "int":
		return attrValue{"intValue", strconv.Itoa(g.rng.IntN(1_000_000))}
	case "float":
		return attrValue{"doubleValue", float64(g.rng.IntN(10_000)) / 100}
	case "bool":
		return attrValue{"boolValue", g.rng.IntN(2) == 0}
	case "status":
		var code int
		switch r := g.rng.IntN(100); {
		case r < 70:
			code = g.pick([]int{200, 201, 204})
		case r < 85:
			code = g.pick([]int{400, 401, 403, 404, 429})
		default:
			code = g.pick([]int{500, 502, 503, 504})
		}
		return attrValue{"intValue", strconv.Itoa(code)}
	case "method":
		return attrValue{"stringValue", g.pick([]string{"GET", "POST", "PUT", "DELETE", "PATCH"})}
	default:
		return attrValue{"stringValue", g.pick(svc.strs)}
	}
}

// body fills every placeholder of a randomly chosen body template, keeping
// substitutions domain-appropriate so bodies read realistically, with
// sprinkled edge cases (empty, very long, and non-ASCII bodies).
func (g *generator) body(i int, svc *service) string {
	switch {
	case i%97 == 0:
		return ""
	case i%101 == 0:
		return strings.Repeat("lorem ipsum dolor sit amet ", 160) // ~4 KB body
	case i%103 == 0:
		return "支付网关到上游 checkout-svc 的连接超时 🚨 gateway timeout (测试)"
	}
	tpl := g.pick(svc.bodies)
	var sb strings.Builder
	for j := 0; j < len(tpl); {
		if tpl[j] == '%' && j+1 < len(tpl) && (tpl[j+1] == 's' || tpl[j+1] == 'd') {
			if tpl[j+1] == 's' {
				sb.WriteString(g.stringSub(tpl, svc))
			} else {
				fmt.Fprintf(&sb, "%d", g.rng.IntN(100_000))
			}
			j += 2
			continue
		}
		sb.WriteByte(tpl[j])
		j++
	}
	return sb.String()
}

func (g *generator) stringSub(tpl string, svc *service) string {
	lower := strings.ToLower(tpl)
	switch {
	case strings.Contains(lower, "sku"):
		if v := g.pickWithPrefix(svc.strs, "SKU-"); v != "" {
			return v
		}
		for i := range services {
			if v := g.pickWithPrefix(services[i].strs, "SKU-"); v != "" {
				return v
			}
		}
	case strings.Contains(lower, "charge"):
		if v := g.pickWithPrefix(svc.strs, "ch_"); v != "" {
			return v
		}
	case strings.Contains(lower, "upstream"):
		return g.pick(upstreams)
	}
	if len(svc.strs) > 0 {
		return g.pick(svc.strs)
	}
	return g.pick(upstreams)
}

func weightedSeverity(rng *mathrand.Rand) sev.Level {
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

func weightedService(rng *mathrand.Rand) int {
	weights := make([]int, len(services))
	total := 0
	for i := range services {
		weights[i] = len(services[i].bodies) * (20 - i)
		total += weights[i]
	}
	n := rng.IntN(total)
	for i, w := range weights {
		n -= w
		if n < 0 {
			return i
		}
	}
	return len(weights) - 1
}

// recordMap renders one OTLP/HTTP JSON log record as a JSON-encodable map.
func recordMap(r record) map[string]any {
	m := map[string]any{
		"timeUnixNano":         strconv.FormatInt(r.ts, 10),
		"observedTimeUnixNano": strconv.FormatInt(r.ts, 10),
		"severityNumber":       r.lvl.Number,
		"severityText":         r.lvl.Text,
		"body":                 map[string]any{"stringValue": r.body},
	}
	if r.event != "" {
		m["eventName"] = r.event
	}
	if r.trace != nil {
		m["traceId"] = hex.EncodeToString(r.trace)
	}
	if r.span != nil {
		m["spanId"] = hex.EncodeToString(r.span)
	}
	if len(r.attrs) > 0 {
		list := make([]map[string]any, 0, len(r.attrs))
		for _, a := range r.attrs {
			list = append(list, map[string]any{"key": a.key, "value": map[string]any{a.v.kind: a.v.value}})
		}
		m["attributes"] = list
	}
	return m
}

// buildPayload groups a batch of records by service into one OTLP export body.
func buildPayload(batch []record) string {
	bySvc := map[*service][]map[string]any{}
	for _, r := range batch {
		bySvc[r.svc] = append(bySvc[r.svc], recordMap(r))
	}
	resourceLogs := make([]map[string]any, 0, len(bySvc))
	for svc, recs := range bySvc {
		resourceLogs = append(resourceLogs, map[string]any{
			"resource": map[string]any{"attributes": []map[string]any{
				{"key": "service.name", "value": map[string]any{"stringValue": svc.name}},
				{"key": "service.namespace", "value": map[string]any{"stringValue": "otel-sqlite-explorer"}},
				{"key": "host.name", "value": map[string]any{"stringValue": svc.host}},
			}},
			"scopeLogs": []map[string]any{{
				"scope":      map[string]any{"name": svc.name + ".scope", "version": "0"},
				"logRecords": recs,
			}},
		})
	}
	b, err := json.Marshal(map[string]any{"resourceLogs": resourceLogs})
	if err != nil {
		log.Fatalf("encode payload: %v", err)
	}
	return string(b)
}

// stack owns the containers, network and volume of one demo run.
type stack struct {
	image   string
	network string
	volume  string
	sink    string
	bridge  string
}

func (s *stack) teardown() {
	_ = exec.Command("docker", "rm", "-f", s.sink, s.bridge).Run()
	_ = exec.Command("docker", "volume", "rm", "-f", s.volume).Run()
	_ = exec.Command("docker", "network", "rm", s.network).Run()
}

func (s *stack) fail(format string, args ...any) {
	s.teardown()
	log.Fatalf(format, args...)
}

func (s *stack) docker(args ...string) string {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		s.fail("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (s *stack) ensureImage(ref string) {
	if err := exec.Command("docker", "image", "inspect", ref).Run(); err != nil {
		log.Printf("pulling %s (first run downloads it)", ref)
		s.docker("pull", ref)
	}
}

func (s *stack) waitHealthy(name string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		st := strings.TrimSpace(s.docker("inspect", "--format",
			"{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", name))
		if st == "healthy" {
			return
		}
		if time.Now().After(deadline) {
			s.fail("%s never became healthy (last state %q)", name, st)
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *stack) start() {
	suffix := fmt.Sprintf("demo%d", time.Now().UnixNano()%1e9)
	s.network = "sinkdemo-net-" + suffix
	s.volume = "sinkdemo-vol-" + suffix
	s.sink = "sinkdemo-sink-" + suffix
	s.bridge = "sinkdemo-bridge-" + suffix
	s.ensureImage(s.image)
	s.ensureImage(bridgeImage)
	s.docker("network", "create", s.network)
	s.docker("volume", "create", s.volume)
	// The image ships a default config that binds OTLP on all interfaces with
	// cleartext, so zero flags reproduce the documented docker run quickstart.
	s.docker("run", "-d", "--name", s.sink, "--network", s.network, "-v", s.volume+":/data", s.image)
	s.waitHealthy(s.sink, 120*time.Second)
}

func (s *stack) startBridge() string {
	cfg := strings.Join([]string{
		"receivers:",
		"  otlp:",
		"    protocols:",
		"      http:",
		"        endpoint: 0.0.0.0:4318",
		"exporters:",
		"  otlp/sqlite:",
		"    endpoint: " + s.sink + ":4317",
		"    tls:",
		"      insecure: true",
		"service:",
		"  pipelines:",
		"    logs:",
		"      receivers: [otlp]",
		"      exporters: [otlp/sqlite]",
	}, "\n") + "\n"
	dir, err := os.MkdirTemp("", "sinkdemo-*")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	cfgPath := filepath.Join(dir, "otelcol.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		log.Fatalf("write bridge config: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	s.docker("run", "-d", "--name", s.bridge, "--network", s.network,
		"-p", fmt.Sprintf("127.0.0.1:%d:4318", port),
		"-v", cfgPath+":/etc/otelcol/config.yaml:ro",
		bridgeImage, "--config=/etc/otelcol/config.yaml")
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// push posts one OTLP batch, retrying until the bridge accepts it.
func (s *stack) push(baseURL, payload string) {
	client := &http.Client{Timeout: 30 * time.Second}
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for {
		resp, err := client.Post(baseURL+"/v1/logs", "application/json", strings.NewReader(payload))
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Sprintf("status %d: %s", resp.StatusCode, body)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			s.fail("pushing OTLP logs through the bridge failed: %s", last)
		}
		time.Sleep(2 * time.Second)
	}
}

// harvest stops both containers gracefully, copies /data out, rolls any WAL
// sidecars into the database file, and returns its path.
func (s *stack) harvest() string {
	time.Sleep(5 * time.Second) // let the async collector exporter drain
	s.docker("stop", "-t", "30", s.bridge)
	s.docker("stop", "-t", "30", s.sink)
	out, err := os.MkdirTemp("", "sinkdemo-harvest-*")
	if err != nil {
		log.Fatalf("temp dir: %v", err)
	}
	dataDir := filepath.Join(out, "data")
	s.docker("cp", s.sink+":/data", dataDir)
	var dbPath string
	err = filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, "-wal") && !strings.HasSuffix(name, "-shm") {
			dbPath = p
		}
		return nil
	})
	if err != nil {
		log.Fatalf("walking harvested /data: %v", err)
	}
	if dbPath == "" {
		log.Fatalf("no SQLite database file in harvested /data (%s)", dataDir)
	}
	rec, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open harvested database for WAL recovery: %v", err)
	}
	if _, err := rec.Exec("SELECT count(*) FROM sqlite_master"); err != nil {
		_ = rec.Close()
		log.Fatalf("WAL recovery on harvested database: %v", err)
	}
	_ = rec.Close()
	return dbPath
}

func main() {
	out := flag.String("out", "demo/sink-logs.db", "output database path")
	rows := flag.Int("rows", 3000, "number of log records to push")
	seed := flag.Int64("seed", -1, "random seed (-1 for a random seed)")
	image := flag.String("image", defaultSinkImage, "sink image to drive")
	flag.Parse()
	if *rows < 1 {
		log.Fatalf("invalid -rows: %d (must be >= 1)", *rows)
	}
	sinkImage := *image
	if sinkImage == defaultSinkImage {
		if v := os.Getenv("OTEL_SQLITE_IMAGE"); v != "" {
			sinkImage = v
		}
	}

	if err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		log.Fatal("no Docker daemon reachable; sinkdemo needs Docker to run the published sink image")
	}

	var rng *mathrand.Rand
	if *seed >= 0 {
		rng = mathrand.New(mathrand.NewPCG(uint64(*seed), 0x9e3779b97f4a7c15)) // #nosec G404 -- weak RNG intentional for reproducible demo data
	} else {
		rng = mathrand.New(mathrand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid()))) // #nosec G115 G404 -- timestamp/pid are non-negative; weak RNG intentional for demo data
	}
	g := &generator{rng: rng}

	now := time.Now().UnixNano()
	day := int64(24 * time.Hour)
	batchSize := 250

	records := make([]record, 0, *rows)
	for i := 0; i < *rows; i++ {
		svc := &services[weightedService(rng)]
		lvl := weightedSeverity(rng)
		var age int64
		if rng.IntN(100) < 60 {
			age = rng.Int64N(int64(time.Hour))
		} else {
			age = rng.Int64N(day)
		}
		r := record{
			svc:   svc,
			lvl:   lvl,
			ts:    now - age,
			body:  g.body(i, svc),
			trace: g.traceID(i),
			event: "",
		}
		if r.trace != nil {
			r.span = g.spanID()
		}
		for _, a := range svc.attrs {
			r.attrs = append(r.attrs, recAttr{a.key, g.genAttrValue(a, svc)})
		}
		if lvl.Text == "ERROR" || lvl.Text == "FATAL" {
			r.attrs = append(r.attrs, recAttr{"error.code", attrValue{"stringValue", fmt.Sprintf("E%d", rng.IntN(9999))}})
		}
		if i%107 == 0 {
			r.event = "manual.check"
		}
		records = append(records, r)
	}

	s := &stack{image: sinkImage}
	s.start()
	defer s.teardown()
	base := s.startBridge()

	for start := 0; start < len(records); start += batchSize {
		end := start + batchSize
		if end > len(records) {
			end = len(records)
		}
		s.push(base, buildPayload(records[start:end]))
	}
	log.Printf("pushed %d records through %s; harvesting", len(records), sinkImage)
	dbPath := s.harvest()

	if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
		log.Fatalf("create output dir: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(*out + suffix); err != nil && !os.IsNotExist(err) {
			log.Fatalf("remove existing database %s: %v (close it in any running process first)", *out+suffix, err)
		}
	}
	b, err := os.ReadFile(dbPath)
	if err != nil {
		log.Fatalf("read harvested database: %v", err)
	}
	if err := os.WriteFile(*out, b, 0o640); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}

	// Report the contract facts of the produced database.
	q, err := sql.Open("sqlite", *out)
	if err != nil {
		log.Fatalf("open %s: %v", *out, err)
	}
	defer func() { _ = q.Close() }()
	var events, fts int
	_ = q.QueryRow("SELECT count(*) FROM log_event").Scan(&events)
	_ = q.QueryRow("SELECT count(*) FROM logs_fts").Scan(&fts)
	var versions []string
	vrows, err := q.Query("SELECT version FROM schema_migrations ORDER BY version")
	if err == nil {
		for vrows.Next() {
			var v string
			if err := vrows.Scan(&v); err == nil {
				versions = append(versions, v)
			}
		}
		_ = vrows.Close()
	}
	var mode, status string
	_ = q.QueryRow("SELECT mode, status FROM search_index_state").Scan(&mode, &status)
	log.Printf("wrote %d log events to %s (fts=%d rows, schema_migrations=%s, search_index_state=%s/%s)",
		events, *out, fts, strings.Join(versions, ","), mode, status)
}
