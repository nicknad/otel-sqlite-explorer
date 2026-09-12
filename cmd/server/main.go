// Command server runs the log-explorer HTTP sidecar service.
//
// Usage:
//
//	log-explorer -db /path/to/logs.db [-addr :8080]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"log-explorer/internal/api"
	"log-explorer/internal/db"
)

// revision is the build revision (commit hash), injected via
// -ldflags "-X main.revision=…" by release builds. Defaults to "dev".
var revision = "dev"

func main() {
	// The default binds to loopback only: the service has no authentication,
	// so exposing it on all interfaces must be an explicit choice.
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbPath := flag.String("db", "", "Path to SQLite database (required)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("log-explorer %s\n", revision)
		return
	}

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "error: -db flag is required")
		flag.Usage()
		os.Exit(1)
	}

	// Verify the database file exists.
	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("database file not accessible: %v", err)
	}

	// Open read-only SQLite connection.
	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}

	// Create API server with embedded templates.
	srv, err := api.NewServer(database)
	if err != nil {
		if closeErr := database.Close(); closeErr != nil {
			log.Printf("close database: %v", closeErr)
		}
		log.Fatalf("create server: %v", err)
	}

	defer func() {
		if err := database.Close(); err != nil {
			log.Printf("close database: %v", err)
		}
	}()

	// Set up routes.
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Start HTTP server. Timeouts comfortably exceed the 10s per-query
	// deadline so legitimate (large, 1000-row) responses are never cut off
	// while slow or hung connections still get reaped.
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown: stop accepting new connections on SIGINT/SIGTERM
	// and give in-flight requests a few seconds to finish. A listen failure
	// is surfaced through errCh so deferred cleanup (database close) still
	// runs instead of log.Fatalf exiting mid-goroutine.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("log-explorer listening on %s", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Printf("http server: %v", err)
	case <-quit:
		log.Println("shutting down...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("http server shutdown: %v", err)
	}
}
