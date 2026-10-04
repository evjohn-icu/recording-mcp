package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"recordingmcp/fixtures"
	"recordingmcp/internal/mcpserver"
	"recordingmcp/internal/plane"
)

func main() {
	log.SetOutput(os.Stderr)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dbPath := flag.String("db", "data/recordings.db", "SQLite database file")
	seed := flag.Bool("seed", false, "Idempotently import embedded synthetic recording events")
	at := flag.String("at", "", "Optional fixed RFC3339 query clock for reproducible examples")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	var clock func() time.Time
	if *at != "" {
		fixed, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("--at must be RFC3339: %w", err)
		}
		clock = func() time.Time { return fixed }
	}
	if *dbPath == "" {
		return fmt.Errorf("--db must not be empty")
	}
	if *dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
			return err
		}
	}
	service, err := plane.Open(*dbPath)
	if err != nil {
		return err
	}
	defer service.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *seed {
		var events []plane.Event
		if err = json.Unmarshal(fixtures.Recordings, &events); err != nil {
			return fmt.Errorf("decode fixtures: %w", err)
		}
		n, err := service.Ingest(ctx, events)
		if err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		log.Printf("Imported %d synthetic events into %s", n, *dbPath)
	}
	err = mcpserver.New(service, clock).Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil {
		return nil
	}
	return err
}
