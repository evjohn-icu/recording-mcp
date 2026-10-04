package pipeline

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/migrations"
)

// newAssociationDB creates a migrated SQLite database for the association
// tests. Foreign keys are left off so tests can seed only the topics and turns
// the link validation reads.
func newAssociationDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "associations.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func seedTopic(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO topics(id,title) VALUES(?,?)`, id, "title "+id); err != nil {
		t.Fatalf("insert topic %s: %v", id, err)
	}
}

func seedTurn(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO turns(id,conversation_id,recording_id,segment_index,content,source_sha256,transcript_version,original_turn_id,turn_json)
		 VALUES(?,'conv','rec',0,'text','sha','v1',?, '{}')`, id, id); err != nil {
		t.Fatalf("insert turn %s: %v", id, err)
	}
}

func seedAssociationFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, id := range []string{"topic-1", "topic-2", "topic-3"} {
		seedTopic(t, db, id)
	}
	for _, id := range []string{"turn-1", "turn-2"} {
		seedTurn(t, db, id)
	}
}

func baseLinkProposal() LinkProposal {
	return LinkProposal{
		Kind:            "topic_link",
		FromTopicID:     "topic-1",
		ToTopicID:       "topic-2",
		Relation:        "same_topic",
		Reason:          "both turns discuss the same planning topic",
		EvidenceTurnIDs: []string{"turn-1"},
		Confidence:      0.7,
	}
}

func validateOne(t *testing.T, db *sql.DB, p LinkProposal) ValidatedLink {
	t.Helper()
	out, err := ValidateTopicLinks(context.Background(), db, []LinkProposal{p})
	if err != nil {
		t.Fatalf("ValidateTopicLinks: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d validated links, want 1", len(out))
	}
	return out[0]
}

func TestValidateTopicLinksAcceptsValid(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)

	for _, relation := range []string{"same_topic", "follow_up", "contradiction", "update"} {
		p := baseLinkProposal()
		p.Relation = relation
		p.Confidence = 0
		got := validateOne(t, db, p)
		if got.Status != linkStatusValidated {
			t.Errorf("relation %q: status = %q (%s), want validated", relation, got.Status, got.RejectionReason)
		}
		if got.RejectionReason != "" {
			t.Errorf("relation %q: unexpected rejection reason %q", relation, got.RejectionReason)
		}
		if !reflect.DeepEqual(got.LinkProposal, p) {
			t.Errorf("relation %q: proposal fields not preserved: %+v", relation, got.LinkProposal)
		}
	}

	// Boundary values: confidence exactly 1 and a reason of exactly 500 runes.
	p := baseLinkProposal()
	p.Confidence = 1
	p.Reason = strings.Repeat("x", maxLinkReasonRunes)
	if got := validateOne(t, db, p); got.Status != linkStatusValidated {
		t.Errorf("boundary proposal rejected: %s", got.RejectionReason)
	}
}

func TestValidateTopicLinksRejectsViolations(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)

	tests := []struct {
		name   string
		mutate func(*LinkProposal)
	}{
		{"wrong kind", func(p *LinkProposal) { p.Kind = "note_link" }},
		{"from topic missing", func(p *LinkProposal) { p.FromTopicID = "topic-missing" }},
		{"to topic missing", func(p *LinkProposal) { p.ToTopicID = "topic-missing" }},
		{"self loop", func(p *LinkProposal) { p.ToTopicID = p.FromTopicID }},
		{"bad relation", func(p *LinkProposal) { p.Relation = "related_to" }},
		{"empty reason", func(p *LinkProposal) { p.Reason = "" }},
		{"whitespace reason", func(p *LinkProposal) { p.Reason = "   \t\n " }},
		{"long reason", func(p *LinkProposal) { p.Reason = strings.Repeat("x", maxLinkReasonRunes+1) }},
		{"no evidence", func(p *LinkProposal) { p.EvidenceTurnIDs = nil }},
		{"evidence turn missing", func(p *LinkProposal) { p.EvidenceTurnIDs = []string{"turn-1", "turn-missing"} }},
		{"confidence above range", func(p *LinkProposal) { p.Confidence = 1.01 }},
		{"confidence below range", func(p *LinkProposal) { p.Confidence = -0.01 }},
		{"confidence NaN", func(p *LinkProposal) { p.Confidence = math.NaN() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := baseLinkProposal()
			tt.mutate(&p)
			got := validateOne(t, db, p)
			if got.Status != linkStatusRejected {
				t.Fatalf("status = %q, want rejected", got.Status)
			}
			if got.RejectionReason == "" {
				t.Fatal("rejected link has no rejection reason")
			}
			// NaN never equals itself, so skip the field comparison for it.
			if !math.IsNaN(p.Confidence) && !reflect.DeepEqual(got.LinkProposal, p) {
				t.Errorf("rejected link did not preserve proposal fields: %+v", got.LinkProposal)
			}
		})
	}
}

func TestValidateTopicLinksKeepsInputOrder(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)

	good := baseLinkProposal()
	bad := baseLinkProposal()
	bad.ToTopicID = "topic-missing"
	out, err := ValidateTopicLinks(context.Background(), db, []LinkProposal{bad, good})
	if err != nil {
		t.Fatalf("ValidateTopicLinks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d links, want 2", len(out))
	}
	if out[0].Status != linkStatusRejected || out[1].Status != linkStatusValidated {
		t.Fatalf("order not preserved: %q, %q", out[0].Status, out[1].Status)
	}
}

func TestInsertLinksIdempotent(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)
	ctx := context.Background()
	now := time.UnixMilli(1_700_000_000_000)

	p := baseLinkProposal()
	links := []ValidatedLink{{LinkProposal: p, Status: linkStatusValidated}}

	ids1, err := InsertLinks(ctx, db, "curator-model", links, now)
	if err != nil {
		t.Fatalf("first InsertLinks: %v", err)
	}
	if len(ids1) != 1 {
		t.Fatalf("got %d ids, want 1", len(ids1))
	}
	want := linkID("topic", p.FromTopicID, "topic", p.ToTopicID, p.Relation)
	if ids1[0] != want {
		t.Errorf("id = %q, want %q", ids1[0], want)
	}
	if !strings.HasPrefix(ids1[0], "link:") || len(ids1[0]) != len("link:")+32 {
		t.Errorf("id %q does not match link:<32 hex>", ids1[0])
	}

	// Re-inserting the same tuple must not create a second row or change the id.
	ids2, err := InsertLinks(ctx, db, "other-model", links, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second InsertLinks: %v", err)
	}
	if len(ids2) != 1 || ids2[0] != ids1[0] {
		t.Fatalf("second ids = %v, want [%s]", ids2, ids1[0])
	}

	stored, err := ListLinks(ctx, db, "")
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(stored))
	}
	got := stored[0]
	if got.Status != linkStatusProposed {
		t.Errorf("status = %q, want proposed", got.Status)
	}
	if got.FromKind != "topic" || got.FromID != p.FromTopicID || got.ToKind != "topic" || got.ToID != p.ToTopicID {
		t.Errorf("endpoints = %s/%s -> %s/%s", got.FromKind, got.FromID, got.ToKind, got.ToID)
	}
	if got.Relation != p.Relation || got.Reason != p.Reason || got.Confidence != p.Confidence {
		t.Errorf("payload mismatch: %+v", got)
	}
	if got.Model != "curator-model" {
		t.Errorf("model = %q, want first writer", got.Model)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, now)
	}
	if len(got.EvidenceTurnIDs) != 1 || got.EvidenceTurnIDs[0] != "turn-1" {
		t.Errorf("evidence = %v, want [turn-1]", got.EvidenceTurnIDs)
	}
}

func TestInsertLinksSkipsRejectedAndForeignKinds(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)
	ctx := context.Background()

	rejected := ValidatedLink{LinkProposal: baseLinkProposal(), Status: linkStatusRejected, RejectionReason: "reason is empty"}
	foreign := ValidatedLink{LinkProposal: baseLinkProposal(), Status: linkStatusValidated}
	foreign.Kind = "note_link"
	foreign.ToTopicID = "topic-3"

	ids, err := InsertLinks(ctx, db, "model", []ValidatedLink{rejected, foreign}, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatalf("InsertLinks: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
	stored, err := ListLinks(ctx, db, "")
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("stored rows = %d, want 0", len(stored))
	}
}

func TestListLinksFiltersAndOrders(t *testing.T) {
	db := newAssociationDB(t)
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000)

	insertRawLink := func(id, relation, status string, createdAt time.Time) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO memory_links(id, from_kind, from_id, to_kind, to_id, relation, reason, evidence_turn_ids_json, confidence, status, model, created_at)
			 VALUES(?,'topic','topic-1','topic','topic-2',?,'r','[]',0.5,?,'m',?)`,
			id, relation, status, createdAt.UnixMilli()); err != nil {
			t.Fatalf("insert raw link %s: %v", id, err)
		}
	}
	// Same created_at for two rows exercises the id tiebreak; the later row
	// sorts first by id.
	insertRawLink("link:bbb", "same_topic", "proposed", base)
	insertRawLink("link:aaa", "follow_up", "proposed", base)
	insertRawLink("link:ccc", "update", "confirmed", base.Add(time.Second))

	all, err := ListLinks(ctx, db, "")
	if err != nil {
		t.Fatalf("ListLinks all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all rows = %d, want 3", len(all))
	}
	if all[0].ID != "link:aaa" || all[1].ID != "link:bbb" || all[2].ID != "link:ccc" {
		t.Fatalf("order = %s, %s, %s; want aaa, bbb, ccc", all[0].ID, all[1].ID, all[2].ID)
	}

	proposed, err := ListLinks(ctx, db, "proposed")
	if err != nil {
		t.Fatalf("ListLinks proposed: %v", err)
	}
	if len(proposed) != 2 {
		t.Fatalf("proposed rows = %d, want 2", len(proposed))
	}
	for _, l := range proposed {
		if l.Status != "proposed" {
			t.Errorf("filter leaked status %q", l.Status)
		}
	}

	confirmed, err := ListLinks(ctx, db, "confirmed")
	if err != nil {
		t.Fatalf("ListLinks confirmed: %v", err)
	}
	if len(confirmed) != 1 || confirmed[0].ID != "link:ccc" {
		t.Fatalf("confirmed = %+v, want link:ccc", confirmed)
	}
}
