package tests

import (
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"log-explorer/internal/compiler"
	"log-explorer/internal/db"
	"log-explorer/internal/dsl"

	_ "modernc.org/sqlite"
)

// Image-backed integration test: the read contract is proven against a
// database produced by the real otel-sqlite sink image, instead of the
// hand-mirrored DDL in collector_compat_test.go.
//
// The sink speaks OTLP/gRPC only, so a pinned OpenTelemetry Collector
// container bridges OTLP/HTTP JSON into it - the same topology as the sink's
// tests/docker/compose.stack-e2e.yml, but self-contained: containers live on
// a private network and the database is harvested by copying /data out of
// the stopped sink container.
//
// Skipping: without a reachable Docker daemon these tests skip so
// "go test ./..." stays green on Docker-less machines. Set
// OTEL_SQLITE_IMAGE=<ref> to pin or force an image; with the variable set, a
// missing daemon fails the test instead of skipping it.
//
// Because the default image ref tracks the newest published sink schema,
// this test doubles as a drift alarm: a sink release that breaks the read
// contract turns it red.
const (
	defaultSinkImage = "ghcr.io/nicknad/otel-sqlite:latest"
	bridgeImage      = "ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.159.0"
)

func dockerReachable() bool {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// requireSinkImage returns the image under test and enforces the skip policy.
func requireSinkImage(t *testing.T) string {
	t.Helper()
	image := os.Getenv("OTEL_SQLITE_IMAGE")
	if image == "" {
		if !dockerReachable() {
			t.Skip("no Docker daemon reachable; set OTEL_SQLITE_IMAGE=<ref> to force these tests")
		}
		return defaultSinkImage
	}
	if !dockerReachable() {
		t.Fatalf("OTEL_SQLITE_IMAGE=%q is set but Docker is not reachable", image)
	}
	return image
}

// sinkStack owns the containers, network and volume of one test run.
type sinkStack struct {
	t         *testing.T
	sinkImage string
	network   string
	volume    string
	sink      string
	bridge    string
}

func startSinkStack(t *testing.T, image string) *sinkStack {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	s := &sinkStack{
		t:         t,
		sinkImage: image,
		network:   "explorer-it-net-" + suffix,
		volume:    "explorer-it-vol-" + suffix,
		sink:      "explorer-it-sink-" + suffix,
		bridge:    "explorer-it-bridge-" + suffix,
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", s.sink, s.bridge).Run()
		_ = exec.Command("docker", "volume", "rm", "-f", s.volume).Run()
		_ = exec.Command("docker", "network", "rm", s.network).Run()
	})
	s.ensureImage(s.sinkImage)
	s.ensureImage(bridgeImage)
	s.docker("network", "create", s.network)
	s.docker("volume", "create", s.volume)
	// The image ships a default config that binds OTLP on all interfaces with
	// cleartext, so zero flags reproduce the documented docker run quickstart.
	s.docker("run", "-d", "--name", s.sink, "--network", s.network,
		"-v", s.volume+":/data", s.sinkImage)
	s.waitHealthy(s.sink, 120*time.Second)
	return s
}

// docker runs a docker command, failing the test on error.
func (s *sinkStack) docker(args ...string) string {
	s.t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		s.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (s *sinkStack) ensureImage(ref string) {
	s.t.Helper()
	if err := exec.Command("docker", "image", "inspect", ref).Run(); err != nil {
		s.t.Logf("pulling %s (first run downloads it)", ref)
		s.docker("pull", ref)
	}
}

func (s *sinkStack) waitHealthy(name string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st := strings.TrimSpace(s.docker("inspect", "--format",
			"{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", name))
		if st == "healthy" {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s never became healthy (last state %q)", name, st)
		}
		time.Sleep(2 * time.Second)
	}
}

// startBridge runs the collector that converts OTLP/HTTP JSON to the sink's
// OTLP/gRPC ingress, and returns the base URL to POST log records to.
func (s *sinkStack) startBridge() string {
	s.t.Helper()
	cfg := "receivers:\n" +
		"  otlp:\n" +
		"    protocols:\n" +
		"      http:\n" +
		"        endpoint: 0.0.0.0:4318\n" +
		"exporters:\n" +
		"  otlp/sqlite:\n" +
		"    endpoint: " + s.sink + ":4317\n" +
		"    tls:\n" +
		"      insecure: true\n" +
		"service:\n" +
		"  pipelines:\n" +
		"    logs:\n" +
		"      receivers: [otlp]\n" +
		"      exporters: [otlp/sqlite]\n"
	dir := s.t.TempDir()
	cfgPath := filepath.Join(dir, "otelcol.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		s.t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	s.docker("run", "-d", "--name", s.bridge, "--network", s.network,
		"-p", fmt.Sprintf("127.0.0.1:%d:4318", port),
		"-v", cfgPath+":/etc/otelcol/config.yaml:ro",
		bridgeImage, "--config=/etc/otelcol/config.yaml")
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// pushLogs POSTs one OTLP/HTTP JSON batch with a unique token in every body.
// The sink commits durably before acking, and the retry loop covers bridge
// startup, so returning means the records are persisted.
func (s *sinkStack) pushLogs(baseURL, token string, records int) {
	s.t.Helper()
	now := time.Now().UnixNano()
	recs := make([]string, 0, records)
	for i := 0; i < records; i++ {
		ts := strconv.FormatInt(now+int64(i), 10)
		recs = append(recs, fmt.Sprintf(
			"{\"timeUnixNano\":%q,\"observedTimeUnixNano\":%q,\"severityNumber\":9,\"severityText\":\"INFO\",\"body\":{\"stringValue\":\"image test needle %s record %d\"},\"traceId\":\"%032x\",\"spanId\":\"%016x\",\"attributes\":[{\"key\":\"image.test.seq\",\"value\":{\"intValue\":\"%d\"}}]}",
			ts, ts, token, i, i+1, i+1, i))
	}
	payload := "{\"resourceLogs\":[{\"resource\":{\"attributes\":[" +
		"{\"key\":\"service.name\",\"value\":{\"stringValue\":\"explorer-image-it\"}}," +
		"{\"key\":\"service.namespace\",\"value\":{\"stringValue\":\"otel-sqlite-explorer\"}}]}," +
		"\"scopeLogs\":[{\"scope\":{\"name\":\"explorer-image-it\",\"version\":\"0\"},\"logRecords\":[" +
		strings.Join(recs, ",") + "]}]}]}"
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
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
			s.t.Fatalf("pushing OTLP logs through the bridge failed: %s", last)
		}
		time.Sleep(2 * time.Second)
	}
}

// harvestDB stops both containers gracefully, copies /data out of the sink
// container, rolls any WAL sidecars into the database file, and returns its
// path. The database is a clean post-shutdown snapshot of exactly what a
// user would mount next to the explorer.
func (s *sinkStack) harvestDB() string {
	s.t.Helper()
	// The collector exports asynchronously; give the tiny batch time to
	// drain before the graceful stop below.
	time.Sleep(5 * time.Second)
	s.docker("stop", "-t", "30", s.bridge)
	s.docker("stop", "-t", "30", s.sink)
	out := s.t.TempDir()
	dataDir := filepath.Join(out, "data")
	s.docker("cp", s.sink+":/data", dataDir)
	var dbPath string
	err := filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
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
		s.t.Fatalf("walking harvested /data: %v", err)
	}
	if dbPath == "" {
		s.t.Fatalf("no SQLite database file in harvested /data (%s)", dataDir)
	}
	// Roll WAL frames in with one read-write open, so the read-only
	// production path below never needs write access.
	rec, err := sql.Open("sqlite", dbPath)
	if err != nil {
		s.t.Fatalf("open harvested database for WAL recovery: %v", err)
	}
	if _, err := rec.Exec("SELECT count(*) FROM sqlite_master"); err != nil {
		_ = rec.Close()
		s.t.Fatalf("WAL recovery on harvested database: %v", err)
	}
	_ = rec.Close()
	return dbPath
}

// copyFile copies src to dst; used by the optional harvest dump below.
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o640)
}

// TestSinkImageContract proves the read contract end to end against the
// published sink image: ingest real OTLP records, stop the sink, and open the
// database through the explorer's production read-only path.
func TestSinkImageContract(t *testing.T) {
	image := requireSinkImage(t)
	t.Logf("verifying against %s", image)
	token := fmt.Sprintf("needle%d", time.Now().UnixNano()%1e9)

	stack := startSinkStack(t, image)
	base := stack.startBridge()
	stack.pushLogs(base, token, 3)
	dbPath := stack.harvestDB()

	// Optional evidence capture: EXPLORER_SINK_HARVEST_DUMP=<dir> copies the
	// harvested database somewhere durable so ground-truth introspection and
	// demo tooling can reuse the exact artifact this test verified.
	if dir := os.Getenv("EXPLORER_SINK_HARVEST_DUMP"); dir != "" {
		dst := filepath.Join(dir, "sink-harvest.db")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create harvest dump dir: %v", err)
		}
		if err := copyFile(dbPath, dst); err != nil {
			t.Fatalf("dump harvested database: %v", err)
		}
		t.Logf("harvested database copied to %s", dst)
	}

	// The contract: a database produced by the real sink opens through the
	// production read-only path, with every floor column present.
	client, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open on sink-produced database: %v", err)
	}
	defer func() { _ = client.Close() }()

	if !client.HasFTS() {
		t.Fatal("expected the default body-mode search index on a sink-produced database")
	}

	// The sink's insert triggers must have kept the contentless index complete.
	var ftsRows int
	if err := client.DB().QueryRow("SELECT count(*) FROM logs_fts").Scan(&ftsRows); err != nil {
		t.Fatalf("count logs_fts: %v", err)
	}
	if ftsRows != 3 {
		t.Fatalf("trigger-maintained index: got %d rows, want 3", ftsRows)
	}

	// Full-text search against the unique token through the production stack.
	q := &dsl.Query{Where: dsl.MatchExpr{Query: token}, Limit: 10}
	if err := dsl.Validate(q); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dsl.Normalize(q)
	cq, err := compiler.Compile(q)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rows, err := client.Execute(cq)
	if err != nil {
		t.Fatalf("execute FTS query: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("full-text match %q: got %d rows, want 3", token, len(rows))
	}

	// Structured filter over the superset view the sink installs.
	fq := &dsl.Query{Where: dsl.BinaryExpr{Op: dsl.OpContains, Field: "service_name",
		Value: dsl.Value{Type: dsl.ValueString, String: "explorer-image-it"}}, Limit: 10}
	if err := dsl.Validate(fq); err != nil {
		t.Fatalf("validate filter: %v", err)
	}
	dsl.Normalize(fq)
	fcq, err := compiler.Compile(fq)
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	frows, err := client.Execute(fcq)
	if err != nil {
		t.Fatalf("execute filter: %v", err)
	}
	if len(frows) != 3 {
		t.Fatalf("service filter: got %d rows, want 3", len(frows))
	}

	// Detail lookup by id, including severity fidelity.
	drows, err := client.Execute(compiler.CompileGetByID(rows[0].ID))
	if err != nil {
		t.Fatalf("detail by id: %v", err)
	}
	if len(drows) != 1 || !strings.Contains(drows[0].Body, token) || drows[0].Severity != "INFO" {
		t.Fatalf("detail by id: unexpected rows %+v", drows)
	}
}
