// Command migrate-fts is a maintenance tool that creates and rebuilds the
// FTS5 external-content full-text index over the logs table.
//
// It opens the database with WRITE access and must NOT be run concurrently
// with ingestion. The read-only query sidecar detects the resulting
// logs_fts table at startup and uses it for MATCH queries.
//
// Usage:
//
//	migrate-fts -db /path/to/otel-logs.db [-stats]
//
// Examples:
//
//	migrate-fts -db otel-logs.db            # create + rebuild index
//	migrate-fts -db otel-logs.db -stats     # report drift without rebuilding
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"database/sql"

	_ "modernc.org/sqlite"

	"log-explorer/internal/migrate"
)

func main() {
	dbPath := flag.String("db", "", "Path to SQLite database (required)")
	statsOnly := flag.Bool("stats", false, "Only report index stats; do not rebuild")
	probe := flag.String("probe", "", "FTS5 query string to probe index coverage (requires -stats)")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "error: -db flag is required")
		flag.Usage()
		os.Exit(1)
	}

	// Writable connection (maintenance only — never the sidecar).
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		log.Fatalf("set busy_timeout: %v", err)
	}

	if *statsOnly {
		s, err := migrate.Stats(db)
		if err != nil {
			log.Fatalf("stats: %v", err)
		}
		printStats(s)
		if *probe != "" {
			n, err := migrate.ProbeToken(db, *probe)
			if err != nil {
				log.Fatalf("probe: %v", err)
			}
			fmt.Printf("  probe %-9q : %d rows matched\n", *probe, n)
		}
		return
	}

	s, err := migrate.RebuildFTS(db)
	if err != nil {
		log.Fatalf("rebuild: %v", err)
	}
	fmt.Println("FTS5 index rebuilt successfully.")
	printStats(s)
}

func printStats(s migrate.FTSStats) {
	fmt.Printf("  logs table rows : %d\n", s.LogsCount)
	fmt.Printf("  fts index rows  : %d\n", s.FTSCount)
	fmt.Printf("  has fts table   : %v\n", s.HasFTS)
	if !s.HasFTS {
		fmt.Println("  status          : no fts table (run rebuild)")
	} else {
		fmt.Println("  status          : ready (use -probe <token> to verify coverage)")
	}
}
