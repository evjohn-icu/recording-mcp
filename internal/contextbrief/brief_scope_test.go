package contextbrief

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/memory"
)

func TestBuildAppliesUnionTimeAndConfirmedPersonScopeDeterministically(t *testing.T) {
	store, err := memory.Open(filepath.Join(t.TempDir(), "brief.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	seed := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatalf("seed: %v", e)
		}
	}
	for _, x := range []struct {
		c, r  string
		start any
	}{{"c1", "r1", "2026-01-01T00:00:00Z"}, {"c2", "r2", nil}} {
		seed(`INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES(?,?,'','v1','',?,'sha','rev1',1000,'{}','{}')`, x.c, x.c, x.r)
		seed(`INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES(?,?, 'file','sha',1000,?,?)`, x.r, x.c, x.start, boolInt(x.start != nil))
	}
	seed(`INSERT INTO topics(id,title) VALUES('t1','Topic one'),('t2','Topic two')`)
	seed(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,content,source_sha256,transcript_version,original_turn_id,turn_json) VALUES('a','c1','r1',0,0,100,'spk1','first','sha','rev1','a','{}'),('a2','c1','r1',1,100,200,NULL,'second first','sha','rev1','a2','{}'),('b','c2','r2',0,0,100,'spk2','second','sha','rev1','b','{}')`)
	for _, x := range []struct{ a, c, r, turn string }{{"att-a", "c1", "r1", "a"}, {"att-b", "c2", "r2", "b"}} {
		seed(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active) VALUES(?,?,?,0,?,'rev1','{}','{}','sha',1)`, x.a, x.c, x.r, "seg-"+x.a)
		seed(`INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)`, x.a, x.turn)
	}
	seed(`INSERT INTO turn_topics(turn_id,topic_id) VALUES('a','t1'),('a2','t1'),('b','t2')`)
	seed(`INSERT INTO speaker_bindings(id,recording_id,transcript_revision,speaker_scoped_id,person_id,status,version,created_at,updated_at) VALUES('bind-a','r1','rev1','spk1','p1','confirmed',1,0,0),('bind-b','r2','rev1','spk2','p1','proposed',1,0,0)`)
	seed(`INSERT INTO speaker_bindings(id,recording_id,transcript_revision,speaker_scoped_id,person_id,status,version,created_at,updated_at) VALUES('bind-wrong-revision','r2','rev-old','spk2','p2','confirmed',2,0,0)`)
	seed(`INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES('n1','c1','intention','I plan to call','["t1"]','inference','','v1'),('n2','c1','observation','stale note','["t1"]','inference','','v1')`)
	seed(`INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES('n1','a',0),('n2','a',0)`)
	seed(`INSERT INTO derived_artifacts(kind,artifact_id,conversation_id,stale,stale_reason) VALUES('note','n2','c1',1,'old')`)
	bound := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	req := Request{Scope: Scope{Kind: "project", TopicIDs: []string{"t1"}, ConversationIDs: []string{"c2"}}, StartedAfter: &bound}
	a, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(context.Background(), store, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Evidence) != 2 || a.Evidence[0].TurnID != "a" || a.Evidence[1].TurnID != "a2" {
		t.Fatalf("union/time scope leaked or omitted evidence: %+v", a.Evidence)
	}
	if a.SnapshotID != b.SnapshotID {
		t.Fatal("snapshot id changed for identical reads")
	}
	reordered, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "project", TopicIDs: []string{"t1", "t1"}, ConversationIDs: []string{"c2", "c2"}}, StartedAfter: &bound})
	if err != nil {
		t.Fatal(err)
	}
	if reordered.SnapshotID != a.SnapshotID {
		t.Fatal("scope selector order or duplicates changed snapshot")
	}
	topicOnly, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "topic", TopicIDs: []string{"t1"}}})
	if err != nil || len(topicOnly.Evidence) != 2 {
		t.Fatalf("topic scope failed: count=%d err=%v", len(topicOnly.Evidence), err)
	}
	conversationOnly, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "conversation", ConversationIDs: []string{"c2"}}})
	if err != nil || len(conversationOnly.Evidence) != 1 || conversationOnly.Evidence[0].TurnID != "b" {
		t.Fatalf("conversation scope failed: %+v err=%v", conversationOnly.Evidence, err)
	}
	truncated, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "topic", TopicIDs: []string{"t1"}}, MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated.Truncated || truncated.Coverage.OmittedItems == 0 {
		t.Fatalf("raw-only MaxItems truncation not reported: %+v", truncated.Coverage)
	}
	beforeOutside, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "topic", TopicIDs: []string{"t1"}}})
	if err != nil {
		t.Fatal(err)
	}
	seed(`INSERT INTO memory_highlights(id,conversation_id,recording_id,anchor_kind,start_ms,end_ms,turn_id,source_attachment_id,transcript_revision,tag,note_text,version,created_at_ms,updated_at_ms) VALUES('out-highlight','c2','r2','turn',0,100,'b','att-b','rev1','question','outside text',1,1,1)`)
	afterOutside, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "topic", TopicIDs: []string{"t1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if beforeOutside.SnapshotID != afterOutside.SnapshotID {
		t.Fatal("out-of-scope highlight changed the scoped snapshot")
	}
	seed(`INSERT INTO memory_highlights(id,conversation_id,recording_id,anchor_kind,start_ms,end_ms,turn_id,source_attachment_id,transcript_revision,tag,note_text,version,created_at_ms,updated_at_ms) VALUES('in-highlight','c1','r1','turn',0,100,'a','att-a','rev1','follow_up','user reminder',3,1,1)`)
	withHighlight, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "topic", TopicIDs: []string{"t1"}}})
	if err != nil {
		t.Fatal(err)
	}
	foundVersion := false
	for _, entry := range withHighlight.Sections.FollowUpCandidates {
		if entry.Provenance == "user_authored_highlight:follow_up" {
			foundVersion = entry.SourceVersion == "3" && entry.AssertionState == "proposed"
		}
	}
	if !foundVersion {
		t.Fatal("current user highlight missing source version or proposed state")
	}
	seed(`UPDATE turns SET content=? WHERE id='a'`, strings.Repeat("long transcript sentence ", 300))
	bounded, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "topic", TopicIDs: []string{"t1"}}, MaxItems: 1, MaxBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 2048 {
		t.Fatalf("encoded brief is %d bytes over budget", len(raw))
	}
	if len(bounded.Sections.Background) != 1 || !bounded.Sections.Background[0].TextTruncated || len(bounded.Evidence) != 1 || !bounded.Evidence[0].TextTruncated {
		t.Fatalf("expected retained partial content: %+v", bounded)
	}
	for _, entry := range bounded.Sections.Background {
		for _, id := range entry.EvidenceIDs {
			found := false
			for _, ev := range bounded.Evidence {
				if ev.ID == id {
					found = true
				}
			}
			if !found {
				t.Fatalf("dangling evidence reference %q", id)
			}
		}
	}
	person, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "person", PersonIDs: []string{"p1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(person.Evidence) != 1 || person.Evidence[0].TurnID != "a" {
		t.Fatalf("proposed binding was used: %+v", person.Evidence)
	}
	wrongRevision, err := Build(context.Background(), store, Request{Scope: Scope{Kind: "person", PersonIDs: []string{"p2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrongRevision.Evidence) != 0 {
		t.Fatalf("binding from another source revision leaked: %+v", wrongRevision.Evidence)
	}
	if len(person.Sections.FollowUpCandidates) != 2 || person.Sections.FollowUpCandidates[1].AssertionState != "proposed" || person.Sections.FollowUpCandidates[1].SourceVersion != "v1" {
		t.Fatalf("intention note must remain proposed: %+v", person.Sections.FollowUpCandidates)
	}
	for _, entry := range person.Sections.FollowUpCandidates {
		if entry.Text == "stale note" {
			t.Fatal("stale note text leaked into current brief")
		}
	}
	if _, err = Build(context.Background(), store, Request{Scope: Scope{Kind: "project", TopicIDs: []string{"t1"}}, MaxBytes: 256}); err == nil {
		t.Fatal("byte budget smaller than the response skeleton must be rejected")
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
