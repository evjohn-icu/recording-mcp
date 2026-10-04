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
	"encoding/json"
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

// proposal mirrors one entry of the input file. It is kept separate from
// pipeline.LinkProposal so the CLI owns the wire format (snake_case keys)
// independently of the store type used internally.
type proposal struct {
	Kind            string   `json:"kind"`
	FromTopicID     string   `json:"from_topic_id"`
	ToTopicID       string   `json:"to_topic_id"`
	Relation        string   `json:"relation"`
	Reason          string   `json:"reason"`
	EvidenceTurnIDs []string `json:"evidence_turn_ids"`
	Confidence      float64  `json:"confidence"`
}

// proposalFile is the top-level shape of the input JSON.
type proposalFile struct {
	Proposals []proposal `json:"proposals"`
}

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

	proposals, err := decodeProposals(*inputPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := openDB(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	links := make([]pipeline.LinkProposal, 0, len(proposals))
	for _, p := range proposals {
		links = append(links, pipeline.LinkProposal{
			Kind:            p.Kind,
			FromTopicID:     p.FromTopicID,
			ToTopicID:       p.ToTopicID,
			Relation:        p.Relation,
			Reason:          p.Reason,
			EvidenceTurnIDs: p.EvidenceTurnIDs,
			Confidence:      p.Confidence,
		})
	}

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

	if !*dryRun {
		if _, err := pipeline.InsertLinks(ctx, db, *model, validated, time.Now()); err != nil {
			return fmt.Errorf("insert links: %w", err)
		}
	}

	for _, v := range validated {
		detail := v.Reason
		if v.Status == "rejected" {
			detail = v.RejectionReason
		}
		fmt.Fprintf(stdout, "%s %s -> %s [%s] %s\n", v.Status, v.FromTopicID, v.ToTopicID, v.Relation, detail)
	}
	suffix := ""
	if *dryRun {
		suffix = " (dry-run)"
	}
	fmt.Fprintf(stdout, "accepted=%d rejected=%d%s\n", accepted, rejected, suffix)
	return nil
}

// decodeProposals reads the proposal file, rejecting unknown fields and any
// trailing JSON so a malformed input fails loudly rather than silently
// producing zero proposals.
func decodeProposals(path string) ([]proposal, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var file proposalFile
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s contains trailing JSON", path)
	}
	return file.Proposals, nil
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
