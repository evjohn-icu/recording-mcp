package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
	"recordingmcp/internal/pipeline"
)

// seededDBPath creates a migrated SQLite database in a temp dir, inserts the
// minimal conversation/recording/turn/topic rows the validator checks, closes
// the seeding handle and returns the file path for the CLI to open.
func seededDBPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "links.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("configure seed db: %v", err)
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("migrate seed db: %v", err)
	}
	stmts := []string{
		`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
		 VALUES('conv:1','t','s','v1','basis','rec:1','sha','tv',1000,'{}','{}')`,
		`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms)
		 VALUES('rec:1','conv:1','a.wav','sha',1000)`,
		`INSERT INTO turns(id,conversation_id,recording_id,segment_index,content,source_sha256,transcript_version,original_turn_id,turn_json)
		 VALUES('turn:1','conv:1','rec:1',1,'hello','sha','tv','o1','{}')`,
		`INSERT INTO topics(id,title) VALUES('topic:a','A')`,
		`INSERT INTO topics(id,title) VALUES('topic:b','B')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	return path
}

// openReadDB reopens a CLI-written database for assertions.
func openReadDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// writeProposals writes the proposals JSON and returns its path.
func writeProposals(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proposals.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write proposals: %v", err)
	}
	return path
}

const validProposalJSON = `{"proposals":[{"kind":"topic_link","from_topic_id":"topic:a","to_topic_id":"topic:b","relation":"follow_up","reason":"follows the earlier topic","evidence_turn_ids":["turn:1"],"confidence":0.7}]}`

func TestRunPersistsValidatedLink(t *testing.T) {
	dbPath := seededDBPath(t)
	path := writeProposals(t, validProposalJSON)

	var out, errOut bytes.Buffer
	if err := run([]string{"--db", dbPath, "--input", path, "--model", "pi-test"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v (stderr %q)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "accepted=1 rejected=0") {
		t.Fatalf("unexpected stdout: %q", out.String())
	}

	links, err := pipeline.ListLinks(context.Background(), openReadDB(t, dbPath), pipeline.LinkFilter{Status: "proposed"})
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 stored link, got %d: %+v", len(links), links)
	}
	got := links[0]
	if got.FromKind != "topic" || got.FromID != "topic:a" || got.ToKind != "topic" || got.ToID != "topic:b" {
		t.Fatalf("unexpected endpoints: %+v", got)
	}
	if got.Relation != "follow_up" || got.Status != "proposed" || got.Model != "pi-test" {
		t.Fatalf("unexpected link fields: %+v", got)
	}
	if len(got.EvidenceTurnIDs) != 1 || got.EvidenceTurnIDs[0] != "turn:1" {
		t.Fatalf("unexpected evidence: %+v", got.EvidenceTurnIDs)
	}
	if got.Confidence != 0.7 {
		t.Fatalf("unexpected confidence: %v", got.Confidence)
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	dbPath := seededDBPath(t)
	path := writeProposals(t, validProposalJSON)

	var out, errOut bytes.Buffer
	if err := run([]string{"--db", dbPath, "--input", path, "--dry-run"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v (stderr %q)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "accepted=1 rejected=0 (dry-run)") {
		t.Fatalf("unexpected stdout: %q", out.String())
	}

	links, err := pipeline.ListLinks(context.Background(), openReadDB(t, dbPath), pipeline.LinkFilter{})
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("dry-run wrote links: %+v", links)
	}
}

func TestRunRejectsInvalidProposalsButExitsZero(t *testing.T) {
	dbPath := seededDBPath(t)
	// First proposal links a topic to itself; second targets a missing topic.
	path := writeProposals(t, `{"proposals":[
		{"kind":"topic_link","from_topic_id":"topic:a","to_topic_id":"topic:a","relation":"same_topic","reason":"self link","evidence_turn_ids":["turn:1"],"confidence":0.5},
		{"kind":"topic_link","from_topic_id":"topic:a","to_topic_id":"topic:missing","relation":"update","reason":"unknown target","evidence_turn_ids":["turn:1"],"confidence":0.5}
	]}`)

	var out, errOut bytes.Buffer
	if err := run([]string{"--db", dbPath, "--input", path}, &out, &errOut); err != nil {
		t.Fatalf("run should succeed even when all proposals are rejected: %v (stderr %q)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "accepted=0 rejected=2") {
		t.Fatalf("unexpected stdout: %q", out.String())
	}
	if strings.Count(out.String(), "rejected ") != 2 {
		t.Fatalf("expected two rejected lines, got %q", out.String())
	}

	links, err := pipeline.ListLinks(context.Background(), openReadDB(t, dbPath), pipeline.LinkFilter{})
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("rejected proposals were written: %+v", links)
	}
}

func TestRunRequiresFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(nil, &out, &errOut); err == nil || !strings.Contains(err.Error(), "--db") {
		t.Fatalf("expected --db required error, got %v", err)
	}
	if err := run([]string{"--db", filepath.Join(t.TempDir(), "x.db")}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "--input") {
		t.Fatalf("expected --input required error, got %v", err)
	}
}

func TestRunRejectsBadJSON(t *testing.T) {
	path := writeProposals(t, `{"proposals": [`)
	var out, errOut bytes.Buffer
	err := run([]string{"--db", filepath.Join(t.TempDir(), "x.db"), "--input", path}, &out, &errOut)
	if err == nil {
		t.Fatal("expected non-nil error for malformed JSON")
	}
}

func TestRunRejectsMissingInputFile(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"--db", filepath.Join(t.TempDir(), "x.db"), "--input", filepath.Join(t.TempDir(), "nope.json")}, &out, &errOut)
	if err == nil {
		t.Fatal("expected non-nil error for missing input file")
	}
}

// TestProposalJSONFieldNames guards the wire contract used by the agent-side
// proposal file.
func TestProposalJSONFieldNames(t *testing.T) {
	var file proposalFile
	if err := json.Unmarshal([]byte(validProposalJSON), &file); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(file.Proposals) != 1 {
		t.Fatalf("expected one proposal, got %d", len(file.Proposals))
	}
	p := file.Proposals[0]
	if p.Kind != "topic_link" || p.FromTopicID != "topic:a" || p.ToTopicID != "topic:b" || p.Relation != "follow_up" {
		t.Fatalf("unexpected proposal: %+v", p)
	}
	if p.Confidence != 0.7 || len(p.EvidenceTurnIDs) != 1 {
		t.Fatalf("unexpected proposal fields: %+v", p)
	}
}
