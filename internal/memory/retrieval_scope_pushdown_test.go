package memory

import (
	"context"
	"reflect"
	"testing"
)

func TestSearchMemoryV2ConversationScopeAvoidsInvalidOutOfScopeTimestamp(t *testing.T) {
	s := seedRetrieval(t)
	ctx := context.Background()
	mustExec(t, s, `INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json) VALUES('conv:bad-time','Bad time','','v1','', 'rec:bad-time','sha','tv',1000,'{}','{}')`)
	mustExec(t, s, `INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known) VALUES('rec:bad-time','conv:bad-time','file','sha',1000,'not-rfc3339',1)`)
	seedTurn(t, s, "t:bad-time", "conv:bad-time", "rec:bad-time", 0, 1000, "android matching out of scope")
	mustExec(t, s, `INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version) VALUES('n:bad-time','conv:bad-time','observation','android note matching out of scope','[]','inference','','v1')`)

	page, err := s.SearchMemoryV2(ctx, MemorySearch{Text: "android", Scope: "raw", ConversationID: "conv:b"})
	if err != nil {
		t.Fatalf("scoped v2 query parsed an out-of-scope timestamp: %v", err)
	}
	if len(page.Hits) != 1 || page.Hits[0].ID != "t:b1" {
		t.Fatalf("scoped hits = %+v", page.Hits)
	}
	derived, err := s.SearchMemoryV2(ctx, MemorySearch{Text: "android", Scope: "derived", ConversationID: "conv:b"})
	if err != nil {
		t.Fatalf("scoped v2 note query parsed an out-of-scope timestamp: %v", err)
	}
	if len(derived.Hits) != 1 || derived.Hits[0].ID != "n:b1" {
		t.Fatalf("scoped derived hits = %+v", derived.Hits)
	}
	if _, err := s.SearchMemoryV2(ctx, MemorySearch{Text: "android", Scope: "raw"}); err == nil {
		t.Fatal("unscoped query should still inspect and reject the malformed timestamp")
	}
	// V1 has intentionally never applied ConversationID in SearchMemory.
	// Keep its historical behavior even though the shared scanners support it.
	_, err = s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "raw", ConversationID: "conv:b"})
	if err == nil {
		t.Fatal("v1 must retain its historical unscoped malformed timestamp behavior")
	}
	withoutConversation, err := s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "raw"})
	if err == nil || withoutConversation.Hits != nil {
		t.Fatalf("unscoped v1 = %+v, err=%v", withoutConversation, err)
	}
}

func TestSearchMemoryV2ConversationPushdownPreservesValidResultsAndV1(t *testing.T) {
	s := seedRetrieval(t)
	ctx := context.Background()
	all, err := s.SearchMemoryV2(ctx, MemorySearch{Text: "android", Scope: "both", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := s.SearchMemoryV2(ctx, MemorySearch{Text: "android", Scope: "both", Limit: 20, ConversationID: "conv:b"})
	if err != nil {
		t.Fatal(err)
	}
	var filteredIDs, scopedIDs []string
	for _, hit := range all.Hits {
		if hit.ConversationID == "conv:b" {
			filteredIDs = append(filteredIDs, hit.ID)
		}
	}
	for _, hit := range scoped.Hits {
		if hit.ConversationID != "conv:b" || hit.Score <= 0 {
			t.Fatalf("invalid scoped hit: %+v", hit)
		}
		scopedIDs = append(scopedIDs, hit.ID)
	}
	if !reflect.DeepEqual(scopedIDs, filteredIDs) {
		t.Fatalf("scoped v2 hit IDs differ from filtering all hits: scoped=%v filtered=%v", scopedIDs, filteredIDs)
	}
	v1With, err := s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "raw", ConversationID: "conv:b"})
	if err != nil {
		t.Fatal(err)
	}
	v1Without, err := s.SearchMemory(ctx, MemorySearch{Text: "android", Scope: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v1With.Hits, v1Without.Hits) {
		t.Fatalf("v1 ConversationID semantics changed: with=%+v without=%+v", v1With.Hits, v1Without.Hits)
	}
}
