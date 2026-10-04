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
			if got.Status != LinkStatusRejected {
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
	if out[0].Status != LinkStatusRejected || out[1].Status != linkStatusValidated {
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

	inserted1, err := InsertLinks(ctx, db, "curator-model", links, now)
	if err != nil {
		t.Fatalf("first InsertLinks: %v", err)
	}
	if len(inserted1) != 1 || inserted1[0].Outcome != LinkInserted {
		t.Fatalf("first InsertLinks = %+v, want one inserted outcome", inserted1)
	}
	want := linkID("topic", p.FromTopicID, "topic", p.ToTopicID, p.Relation)
	if inserted1[0].ID != want {
		t.Errorf("id = %q, want %q", inserted1[0].ID, want)
	}
	if !strings.HasPrefix(inserted1[0].ID, "link:") || len(inserted1[0].ID) != len("link:")+32 {
		t.Errorf("id %q does not match link:<32 hex>", inserted1[0].ID)
	}

	// Re-inserting the same tuple must not create a second row, must report
	// that nothing changed, and must keep the first writer's attribution.
	inserted2, err := InsertLinks(ctx, db, "other-model", links, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second InsertLinks: %v", err)
	}
	if len(inserted2) != 1 || inserted2[0].ID != inserted1[0].ID || inserted2[0].Outcome != LinkUnchanged {
		t.Fatalf("second InsertLinks = %+v, want one unchanged outcome for %s", inserted2, inserted1[0].ID)
	}

	stored, err := ListLinks(ctx, db, LinkFilter{})
	if err != nil {
		t.Fatalf("ListLinks: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(stored))
	}
	got := stored[0]
	if got.Status != LinkStatusProposed {
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

	rejected := ValidatedLink{LinkProposal: baseLinkProposal(), Status: LinkStatusRejected, RejectionReason: "reason is empty"}
	foreign := ValidatedLink{LinkProposal: baseLinkProposal(), Status: linkStatusValidated}
	foreign.Kind = "note_link"
	foreign.ToTopicID = "topic-3"

	outcomes, err := InsertLinks(ctx, db, "model", []ValidatedLink{rejected, foreign}, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatalf("InsertLinks: %v", err)
	}
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want none", outcomes)
	}
	stored, err := ListLinks(ctx, db, LinkFilter{})
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

	all, err := ListLinks(ctx, db, LinkFilter{})
	if err != nil {
		t.Fatalf("ListLinks all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all rows = %d, want 3", len(all))
	}
	if all[0].ID != "link:aaa" || all[1].ID != "link:bbb" || all[2].ID != "link:ccc" {
		t.Fatalf("order = %s, %s, %s; want aaa, bbb, ccc", all[0].ID, all[1].ID, all[2].ID)
	}

	proposed, err := ListLinks(ctx, db, LinkFilter{Status: "proposed"})
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

	confirmed, err := ListLinks(ctx, db, LinkFilter{Status: "confirmed"})
	if err != nil {
		t.Fatalf("ListLinks confirmed: %v", err)
	}
	if len(confirmed) != 1 || confirmed[0].ID != "link:ccc" {
		t.Fatalf("confirmed = %+v, want link:ccc", confirmed)
	}
}

func TestListLinksTopicFilterLimitAndTitles(t *testing.T) {
	db := newAssociationDB(t)
	ctx := context.Background()
	for _, id := range []string{"topic-1", "topic-2", "topic-9"} {
		seedTopic(t, db, id)
	}
	insert := func(id, from, to string, createdAt time.Time) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO memory_links(id, from_kind, from_id, to_kind, to_id, relation, reason, evidence_turn_ids_json, confidence, status, model, created_at)
			 VALUES(?,'topic',?,'topic',?,'same_topic','r','[]',0.5,'proposed','m',?)`,
			id, from, to, createdAt.UnixMilli()); err != nil {
			t.Fatalf("insert link %s: %v", id, err)
		}
	}
	base := time.UnixMilli(1_700_000_000_000)
	insert("link:1-2", "topic-1", "topic-2", base)
	insert("link:2-9", "topic-2", "topic-9", base.Add(time.Second))
	insert("link:1-9", "topic-1", "topic-9", base.Add(2*time.Second))

	got, err := ListLinks(ctx, db, LinkFilter{TopicID: "topic-2"})
	if err != nil {
		t.Fatalf("ListLinks topic filter: %v", err)
	}
	if len(got) != 2 || got[0].ID != "link:1-2" || got[1].ID != "link:2-9" {
		t.Fatalf("topic filter = %+v, want both topic-2 links in created_at order", got)
	}
	if got[0].FromTitle != "title topic-1" || got[0].ToTitle != "title topic-2" {
		t.Errorf("titles = %q/%q, want seeded titles", got[0].FromTitle, got[0].ToTitle)
	}
	if got[1].ToTitle != "title topic-9" {
		t.Errorf("to title = %q, want %q", got[1].ToTitle, "title topic-9")
	}

	limited, err := ListLinks(ctx, db, LinkFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListLinks limit: %v", err)
	}
	if len(limited) != 2 || limited[1].ID != "link:2-9" {
		t.Fatalf("limited = %+v, want the two oldest links", limited)
	}
}

func TestReviewLinkTransitions(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)
	ctx := context.Background()

	validated, err := ValidateTopicLinks(ctx, db, []LinkProposal{baseLinkProposal()})
	if err != nil {
		t.Fatalf("ValidateTopicLinks: %v", err)
	}
	inserted, err := InsertLinks(ctx, db, "curator", validated, time.UnixMilli(1_700_000_000_000))
	if err != nil || len(inserted) != 1 {
		t.Fatalf("InsertLinks: %+v err=%v", inserted, err)
	}
	id := inserted[0].ID

	confirmed, err := ReviewLink(ctx, db, id, ReviewConfirm)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if confirmed.Status != LinkStatusConfirmed {
		t.Fatalf("status after confirm = %q, want confirmed", confirmed.Status)
	}
	stored, err := ListLinks(ctx, db, LinkFilter{Status: LinkStatusConfirmed})
	if err != nil {
		t.Fatalf("ListLinks confirmed: %v", err)
	}
	if len(stored) != 1 || stored[0].ID != id {
		t.Fatalf("stored confirmed = %+v, want the confirmed link", stored)
	}

	// Replaying the same decision is a no-op, not an error.
	if again, err := ReviewLink(ctx, db, id, ReviewConfirm); err != nil || again.Status != LinkStatusConfirmed {
		t.Fatalf("repeat confirm = %q, %v; want confirmed, nil", again.Status, err)
	}

	// A mistaken confirmation can be revoked.
	rejected, err := ReviewLink(ctx, db, id, ReviewReject)
	if err != nil {
		t.Fatalf("reject confirmed: %v", err)
	}
	if rejected.Status != LinkStatusRejected {
		t.Fatalf("status = %q, want rejected", rejected.Status)
	}
	if again, err := ReviewLink(ctx, db, id, ReviewReject); err != nil || again.Status != LinkStatusRejected {
		t.Fatalf("repeat reject = %q, %v; want rejected, nil", again.Status, err)
	}

	// A rejected link cannot be confirmed; the error names the current status.
	if _, err := ReviewLink(ctx, db, id, ReviewConfirm); err == nil || !strings.Contains(err.Error(), `"rejected"`) {
		t.Fatalf("confirm after reject err = %v, want a status error", err)
	}
	// The row survives review for audit.
	all, err := ListLinks(ctx, db, LinkFilter{})
	if err != nil {
		t.Fatalf("ListLinks all: %v", err)
	}
	if len(all) != 1 || all[0].Status != LinkStatusRejected {
		t.Fatalf("rows after review = %+v", all)
	}

	if _, err := ReviewLink(ctx, db, "link:missing", ReviewConfirm); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing link err = %v, want not found", err)
	}
	if _, err := ReviewLink(ctx, db, id, "approve"); err == nil || !strings.Contains(err.Error(), "unknown review action") {
		t.Fatalf("unknown action err = %v", err)
	}
	if _, err := ReviewLink(ctx, db, "  ", ReviewConfirm); err == nil {
		t.Fatal("blank link id accepted")
	}
}

func TestInsertLinksReproposalSemantics(t *testing.T) {
	db := newAssociationDB(t)
	seedAssociationFixtures(t, db)
	ctx := context.Background()
	t0 := time.UnixMilli(1_700_000_000_000)

	validated := func(p LinkProposal) []ValidatedLink {
		t.Helper()
		out, err := ValidateTopicLinks(ctx, db, []LinkProposal{p})
		if err != nil || len(out) != 1 || out[0].Status != linkStatusValidated {
			t.Fatalf("ValidateTopicLinks(%+v) = %+v, %v", p, out, err)
		}
		return out
	}
	status := func() MemoryLink {
		t.Helper()
		rows, err := ListLinks(ctx, db, LinkFilter{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("ListLinks = %+v, %v", rows, err)
		}
		return rows[0]
	}

	base := baseLinkProposal()
	inserted, err := InsertLinks(ctx, db, "curator-a", validated(base), t0)
	if err != nil || len(inserted) != 1 || inserted[0].Outcome != LinkInserted {
		t.Fatalf("first InsertLinks = %+v, %v", inserted, err)
	}
	id := inserted[0].ID

	// New content on a pending row replaces the proposal and keeps the status,
	// the original creation time and the first proposer's attribution.
	better := base
	better.Reason = "second pass quotes the follow-up turn as well"
	better.EvidenceTurnIDs = []string{"turn-1", "turn-2"}
	better.Confidence = 0.9
	refreshed, err := InsertLinks(ctx, db, "curator-b", validated(better), t0.Add(time.Hour))
	if err != nil || len(refreshed) != 1 || refreshed[0].Outcome != LinkRefreshed {
		t.Fatalf("refresh InsertLinks = %+v, %v", refreshed, err)
	}
	got := status()
	if got.Status != LinkStatusProposed || got.Reason != better.Reason || got.Confidence != better.Confidence {
		t.Errorf("refreshed row = %+v, want the new reason, confidence and proposed status", got)
	}
	if len(got.EvidenceTurnIDs) != 2 {
		t.Errorf("evidence = %v, want both turns", got.EvidenceTurnIDs)
	}
	if got.Model != "curator-a" || !got.CreatedAt.Equal(t0) {
		t.Errorf("attribution changed: model=%q created_at=%v", got.Model, got.CreatedAt)
	}

	// A reviewer's rejection is sticky: a later agent pass proposing the same
	// pair — even with a reworded reason and an extra evidence turn — must not
	// overturn it, and the reviewed content stays untouched.
	if _, err := ReviewLink(ctx, db, id, ReviewReject); err != nil {
		t.Fatalf("reject: %v", err)
	}
	replay, err := InsertLinks(ctx, db, "curator-b", validated(better), t0.Add(2*time.Hour))
	if err != nil || len(replay) != 1 || replay[0].Outcome != LinkRejectionHeld {
		t.Fatalf("identical replay InsertLinks = %+v, %v; want the rejection held", replay, err)
	}
	if got := status(); got.Status != LinkStatusRejected || got.Reason != better.Reason {
		t.Errorf("rejected row changed on identical replay: %+v", got)
	}
	newer := better
	newer.Reason = "third pass quotes both turns and a later confirmation"
	newer.EvidenceTurnIDs = []string{"turn-1", "turn-2"}
	newer.Confidence = 0.95
	held, err := InsertLinks(ctx, db, "curator-b", validated(newer), t0.Add(3*time.Hour))
	if err != nil || len(held) != 1 || held[0].Outcome != LinkRejectionHeld {
		t.Fatalf("rewritten replay InsertLinks = %+v, %v; want the rejection held", held, err)
	}
	if got := status(); got.Status != LinkStatusRejected || got.Reason != better.Reason || got.Confidence != better.Confidence {
		t.Errorf("rejected row was rewritten by a later pass: %+v", got)
	}

	// Only an explicit human reopen clears the verdict; afterwards the curator
	// may refresh the pending row with its newer wording.
	if got, err := ReviewLink(ctx, db, id, ReviewReopen); err != nil || got.Status != LinkStatusProposed {
		t.Fatalf("reopen after reject = %q, %v", got.Status, err)
	}
	refreshedAgain, err := InsertLinks(ctx, db, "curator-b", validated(newer), t0.Add(4*time.Hour))
	if err != nil || len(refreshedAgain) != 1 || refreshedAgain[0].Outcome != LinkRefreshed {
		t.Fatalf("refresh after reopen = %+v, %v", refreshedAgain, err)
	}
	if got := status(); got.Status != LinkStatusProposed || got.Reason != newer.Reason {
		t.Errorf("row after reopen and refresh = %+v", got)
	}

	// A confirmation is never overwritten by later proposals; it can only be
	// changed by an explicit review action.
	if _, err := ReviewLink(ctx, db, id, ReviewConfirm); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	latest := newer
	latest.Reason = "fourth pass"
	confirmed, err := InsertLinks(ctx, db, "curator-c", validated(latest), t0.Add(5*time.Hour))
	if err != nil || len(confirmed) != 1 || confirmed[0].Outcome != LinkConfirmed {
		t.Fatalf("confirmed InsertLinks = %+v, %v", confirmed, err)
	}
	if got := status(); got.Status != LinkStatusConfirmed || got.Reason != newer.Reason {
		t.Errorf("confirmed row = %+v, want it untouched", got)
	}

	// Reopen returns a verdict to the pending pool, and the row can be
	// reviewed again.
	if got, err := ReviewLink(ctx, db, id, ReviewReopen); err != nil || got.Status != LinkStatusProposed {
		t.Fatalf("reopen after confirm = %q, %v", got.Status, err)
	}
	if got, err := ReviewLink(ctx, db, id, ReviewReopen); err != nil || got.Status != LinkStatusProposed {
		t.Fatalf("repeat reopen = %q, %v; want proposed, nil", got.Status, err)
	}
	if got, err := ReviewLink(ctx, db, id, ReviewConfirm); err != nil || got.Status != LinkStatusConfirmed {
		t.Fatalf("confirm after reopen = %q, %v", got.Status, err)
	}
	if got, err := ReviewLink(ctx, db, id, ReviewReject); err != nil || got.Status != LinkStatusRejected {
		t.Fatalf("reject after confirm after reopen = %q, %v", got.Status, err)
	}
	if got, err := ReviewLink(ctx, db, id, ReviewReopen); err != nil || got.Status != LinkStatusProposed {
		t.Fatalf("reopen after reject = %q, %v", got.Status, err)
	}
}
