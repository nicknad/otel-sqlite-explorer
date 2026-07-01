// Command server runs the log-explorer HTTP sidecar service.
//
// Usage:
//
//	log-explorer -db /path/to/logs.db [-addr :8080]
package main

import (
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

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "", "Path to SQLite database (required)")
	flag.Parse()

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
		database.Close()
		log.Fatalf("create server: %v", err)
	}

	defer database.Close()

	// Set up routes.
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// Start HTTP server.
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("log-explorer listening on %s", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-quit
	log.Println("shutting down...")
	httpSrv.Close()
}
