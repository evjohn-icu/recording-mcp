// Command context-associate is the write side of the memory-curator flow. An
// agent (pi or any MCP client) only reads memory and emits link proposals as
// JSON; this command re-checks every proposal against the database and records
// the surviving ones in the memory_links table with status "proposed". Nothing
// is merged automatically, and rejected proposals are reported with the rule
// that rejected them.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
	"recordingmcp/internal/pipeline"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "context-associate:", err)
		os.Exit(1)
	}
}

// run parses flags, validates the proposals against the database and, unless
// --dry-run is set, stores the accepted ones. It writes one status line per
// proposal plus an accepted/rejected summary to stdout. Argument, IO and JSON
// failures return an error so main exits non-zero; a run where every proposal
// is rejected still succeeds (exit 0) and only reports the counters.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("context-associate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to the SQLite database (required)")
	inputPath := fs.String("input", "", "path to the link-proposal JSON file (required)")
	model := fs.String("model", "", "name of the proposing model recorded on each link")
	dryRun := fs.Bool("dry-run", false, "validate and report without writing any link")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if strings.TrimSpace(*dbPath) == "" {
		return errors.New("--db is required")
	}
	if strings.TrimSpace(*inputPath) == "" {
		return errors.New("--input is required")
	}

	links, err := pipeline.DecodeProposalFile(*inputPath)
	if err != nil {
		return fmt.Errorf("decode %s: %w", *inputPath, err)
	}

	ctx := context.Background()
	db, err := openDB(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	validated, err := pipeline.ValidateTopicLinks(ctx, db, links)
	if err != nil {
		return fmt.Errorf("validate links: %w", err)
	}

	accepted, rejected := 0, 0
	for _, v := range validated {
		if v.Status == "rejected" {
			rejected++
		} else {
			accepted++
		}
	}

	var outcomes []pipeline.InsertedLink
	if !*dryRun {
		outcomes, err = pipeline.InsertLinks(ctx, db, *model, validated, time.Now())
		if err != nil {
			return fmt.Errorf("insert links: %w", err)
		}
	}

	next := 0
	written := 0
	for _, v := range validated {
		detail := v.Reason
		if v.Status == "rejected" {
			detail = v.RejectionReason
		}
		marker := ""
		// InsertLinks reports one outcome per stored proposal, in the same
		// order, skipping rejected and foreign-kind entries; mirror that skip
		// rule to attach each outcome to its line.
		if !*dryRun && v.Kind == "topic_link" && v.Status == "validated" {
			outcome := outcomes[next].Outcome
			next++
			switch outcome {
			case pipeline.LinkInserted:
				written++
			case pipeline.LinkRefreshed:
				written++
				marker = " (refreshed pending suggestion)"
			case pipeline.LinkRejectionHeld:
				marker = " (rejected earlier; unchanged)"
			case pipeline.LinkUnchanged:
				marker = " (unchanged)"
			case pipeline.LinkConfirmed:
				marker = " (already confirmed)"
			}
		}
		fmt.Fprintf(stdout, "%s %s -> %s [%s] %s%s\n", v.Status, v.FromTopicID, v.ToTopicID, v.Relation, detail, marker)
	}
	suffix := ""
	summary := fmt.Sprintf("accepted=%d rejected=%d", accepted, rejected)
	if *dryRun {
		suffix = " (dry-run)"
	} else {
		summary += fmt.Sprintf(" written=%d", written)
	}
	fmt.Fprintf(stdout, "%s%s\n", summary, suffix)
	return nil
}

// openDB opens the SQLite file, applies pending migrations and returns the
// handle. The pool is limited to one connection so the process never contends
// with itself on SQLite's writer lock.
func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if err := migrations.Apply(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return db, nil
}
