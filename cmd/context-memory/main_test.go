package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

func TestMCPToolsExposeQueryAndConversationMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := memory.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr := memory.Transcript{AudioFileName: "test.wav", DurationSeconds: 1}
	tr.Coverage.EndMS = 1000
	tr.Turns = []memory.Turn{{ID: "turn-1", SegmentIndex: 1, SegmentStartMS: 0, SegmentEndMS: 1000, Text: "同步设备文件", GlobalStartMS: nil, GlobalEndMS: nil, SpeakerScopedID: nil}}
	a := memory.Annotations{ConversationID: "conv:mcp", RecordingID: "rec:mcp", Title: "设备体验", Summary: "讨论设备同步", AnnotationBasis: "人工解释，未核实", Topics: []memory.TopicAnnotation{{ID: "topic:sync", Title: "同步"}}, Notes: []memory.NoteAnnotation{{ID: "note:sync", Kind: "suggestion", TopicIDs: []string{"topic:sync"}, Summary: "提出同步建议", EvidenceTurnIDs: []string{"turn-1"}}}, TurnTopics: []memory.TurnTopicAnnotation{{TurnID: "turn-1", TopicIDs: []string{"topic:sync"}}}}
	if _, err = store.Import(ctx, tr, a, "test.wav", strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := newServer(store).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "query_context" {
			b, _ := json.Marshal(tool.InputSchema)
			var schema map[string]any
			_ = json.Unmarshal(b, &schema)
			props, _ := schema["properties"].(map[string]any)
			if props["query"] == nil {
				t.Fatalf("query missing from input schema: %s", b)
			}
		}
	}
	// The registered tool list is exercised by the CallTool checks below; only
	// the query_context input contract is asserted here.
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "query_context", Arguments: map[string]any{"query": "同步", "include_turns": true, "limit": 10}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("query_context rejected query input: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatal("query_context omitted structured content")
	}
	conv, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "get_conversation_context", Arguments: map[string]any{"conversation_id": "conv:mcp"}})
	if err != nil || conv.IsError {
		t.Fatalf("get_conversation_context: %v %+v", err, conv)
	}
}

func TestNeutralIngestAnnotateAndTrackCoverageOverMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := memory.Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := newServer(store).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"ingest_transcript": false, "get_processing_status": false, "apply_annotations": false, "get_annotation_history": false}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
			b, _ := json.Marshal(tool.InputSchema)
			var schema map[string]any
			_ = json.Unmarshal(b, &schema)
			if schema["properties"] == nil {
				t.Fatalf("%s has no explicit properties: %s", tool.Name, b)
			}
		}
	}
	for n, found := range want {
		if !found {
			t.Fatalf("missing tool %s", n)
		}
	}
	input := IngestInput{ContractVersion: asrinput.ContractVersion, Source: IngestSourceInput{Provider: "synthetic-provider", ProviderRequestID: "req-1", DeviceID: "device-1", ConversationID: "conversation-1", RecordingID: "recording-1", SegmentID: "segment-1", SegmentDurationMS: 1000, TranscriptRevision: "rev-1", CompletionStatus: "final", RawEnvelope: map[string]any{"synthetic": true}}, Turns: []IngestTurnInput{{ID: "turn-1", OriginalIndex: 1, Text: "The blue folder is on the desk.", SpeakerScopedID: ptr("speaker-1")}}, RawSource: map[string]any{"synthetic_final": true}}
	call := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		b, _ := json.Marshal(args)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		r, e := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: m})
		if e != nil {
			t.Fatalf("%s call: %v", name, e)
		}
		if r.IsError {
			if len(r.Content) > 0 {
				t.Fatalf("%s error %T: %#v", name, r.Content[0], r.Content[0])
			}
			t.Fatalf("%s error", name)
		}
		return r
	}
	ingest := call("ingest_transcript", input)
	var report memory.IngestReport
	b, _ := json.Marshal(ingest.StructuredContent)
	if err = json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Inserted || report.Turns != 1 || report.AnalysisStatus != "pending" {
		t.Fatalf("unexpected ingest result: %+v", report)
	}
	transcript := call("query_context", memory.Query{ConversationID: report.ConversationID, IncludeTurns: true, Query: "blue folder"})
	b, _ = json.Marshal(transcript.StructuredContent)
	var rawPage memory.Page
	_ = json.Unmarshal(b, &rawPage)
	if rawPage.Total != 1 || rawPage.Items[0].Kind != "turn" || rawPage.Items[0].Content != "The blue folder is on the desk." {
		t.Fatalf("original utterance unavailable: %+v", rawPage)
	}
	if rawPage.Items[0].SpeakerEntityID != nil {
		t.Fatalf("speaker identity must remain unconfirmed: %+v", rawPage.Items[0])
	}
	status := call("get_processing_status", StatusInput{ConversationID: report.ConversationID})
	var ps memory.ProcessingStatus
	b, _ = json.Marshal(status.StructuredContent)
	_ = json.Unmarshal(b, &ps)
	if ps.OriginalTurns != 1 || ps.UnclassifiedTurns != 1 {
		t.Fatalf("bad initial coverage: %+v", ps)
	}
	conversation := call("get_conversation_context", ConversationInput{ConversationID: report.ConversationID})
	b, _ = json.Marshal(conversation.StructuredContent)
	var conversationResult ConversationResult
	_ = json.Unmarshal(b, &conversationResult)
	if conversationResult.Processing.OriginalTurns != 1 || conversationResult.Processing.UnclassifiedTurns != 1 || len(conversationResult.Topics.Topics) != 0 {
		t.Fatalf("conversation response does not expose incomplete processing coverage: %+v", conversationResult)
	}
	originalID := "segment-1/rev-1/turn-1"
	call("apply_annotations", ApplyAnnotationsInput{ConversationID: report.ConversationID, ExpectedVersion: "", AnnotationVersion: "ann-1", Title: "Synthetic discussion", Topics: []memory.TopicAnnotation{{ID: "topic:desk", Title: "Desk"}}, Notes: []memory.NoteAnnotation{{ID: "note:folder", Kind: "observation", TopicIDs: []string{"topic:desk"}, Summary: "A folder was mentioned on the desk", EvidenceTurnIDs: []string{originalID}}}})
	status = call("get_processing_status", StatusInput{ConversationID: report.ConversationID})
	b, _ = json.Marshal(status.StructuredContent)
	_ = json.Unmarshal(b, &ps)
	if ps.TopicLinkedTurns != 1 || ps.UnclassifiedTurns != 0 {
		t.Fatalf("bad annotated coverage: %+v", ps)
	}
	q := call("query_context", memory.Query{ConversationID: report.ConversationID, Query: "folder"})
	b, _ = json.Marshal(q.StructuredContent)
	var page memory.Page
	_ = json.Unmarshal(b, &page)
	if page.Total != 1 || page.Items[0].Kind != "observation" || page.Items[0].Summary != "A folder was mentioned on the desk" {
		t.Fatalf("query did not return annotation/evidence: %+v", page)
	}
	detail := call("get_context_item", ItemInput{ID: page.Items[0].ID})
	b, _ = json.Marshal(detail.StructuredContent)
	var itemDetail memory.ItemDetail
	_ = json.Unmarshal(b, &itemDetail)
	if len(itemDetail.Evidence) != 1 || itemDetail.Evidence[0].Content != "The blue folder is on the desk." {
		t.Fatalf("annotation evidence did not resolve to original turn: %+v", itemDetail)
	}
	h := call("get_annotation_history", HistoryInput{ConversationID: report.ConversationID})
	b, _ = json.Marshal(h.StructuredContent)
	var history HistoryResult
	_ = json.Unmarshal(b, &history)
	if len(history.Revisions) != 2 || !history.Revisions[1].Current || history.Revisions[1].Version != "ann-1" {
		t.Fatalf("bad history: %+v", history)
	}
	call("apply_annotations", ApplyAnnotationsInput{ConversationID: report.ConversationID, ExpectedVersion: "ann-1", AnnotationVersion: "ann-2", Title: "Synthetic discussion revised", Topics: []memory.TopicAnnotation{{ID: "topic:desk", Title: "Desk"}}, Notes: []memory.NoteAnnotation{{ID: "note:folder", Kind: "observation", TopicIDs: []string{"topic:desk"}, Summary: "A folder was mentioned on the desk", EvidenceTurnIDs: []string{originalID}}}})
	h = call("get_annotation_history", HistoryInput{ConversationID: report.ConversationID})
	b, _ = json.Marshal(h.StructuredContent)
	_ = json.Unmarshal(b, &history)
	if len(history.Revisions) != 3 || history.Revisions[1].Version != "ann-1" || history.Revisions[1].Current || !history.Revisions[2].Current || history.Revisions[2].Version != "ann-2" {
		t.Fatalf("old revision not preserved: %+v", history)
	}
	other := input
	other.Source.Provider = "synthetic-other"
	other.Source.ProviderRequestID = "req-other"
	other.Source.DeviceID = "device-other"
	other.Source.RecordingID = "recording-other"
	other.Source.SegmentID = "segment-other"
	other.Turns = []IngestTurnInput{{ID: "turn-other", OriginalIndex: 1, Text: "A second synthetic utterance."}}
	r2 := call("ingest_transcript", other)
	b, _ = json.Marshal(r2.StructuredContent)
	var report2 memory.IngestReport
	_ = json.Unmarshal(b, &report2)
	if !report2.Inserted || report2.ConversationID == report.ConversationID {
		t.Fatalf("second neutral provider was not independently ingested: %+v", report2)
	}
}

func ptr(s string) *string { return &s }

func TestRetrievalToolsOverMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := memory.Open(filepath.Join(t.TempDir(), "retrieval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServer(store).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	page, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_conversations", Arguments: map[string]any{"started_after": "2024-01-01T00:00:00Z", "limit": 5}})
	if err != nil {
		t.Fatal(err)
	}
	if page.IsError {
		t.Fatalf("list_conversations rejected valid input: %+v", page.Content)
	}
	var cp memory.ConversationPage
	b, _ := json.Marshal(page.StructuredContent)
	if err = json.Unmarshal(b, &cp); err != nil {
		t.Fatal(err)
	}
	if len(cp.Items) != 0 || cp.NextCursor != "" {
		t.Fatalf("empty database should list no conversations: %+v", cp)
	}

	ev, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_evidence_context", Arguments: map[string]any{"turn_id": "missing-turn"}})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsError {
		t.Fatal("get_evidence_context accepted an unknown turn")
	}

	sr, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_memory", Arguments: map[string]any{"text": "   "}})
	if err != nil {
		t.Fatal(err)
	}
	if !sr.IsError {
		t.Fatal("search_memory accepted blank text")
	}

	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_conversations", Arguments: map[string]any{"started_after": "not-a-time"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bad.IsError {
		t.Fatal("list_conversations accepted a malformed timestamp")
	}
}

func TestMCPReplayPreservesRawProviderNumbers(t *testing.T) {
	ctx := context.Background()
	store, err := memory.Open(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := asrinput.Result{ContractVersion: asrinput.ContractVersion, Source: asrinput.Source{Provider: "synthetic", ProviderRequestID: "r", DeviceID: "d", RecordingID: "rec", ConversationID: "conv", SegmentID: "s", SegmentDurationMS: 1000, TranscriptRevision: "v1", CompletionStatus: "final", RawEnvelope: json.RawMessage(`{"z":9007199254740993,"a":true}`)}, Turns: []asrinput.Turn{{ID: "t", OriginalIndex: 1, Text: "Synthetic evidence"}}, RawSource: json.RawMessage(`{"z":9007199254740993,"a":{"second":2,"first":1}}`)}
	if _, err = store.IngestTranscript(ctx, input); err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServer(store).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	input.Source.RawEnvelope = json.RawMessage(`{"a":true,"z":9007199254740993}`)
	input.RawSource = json.RawMessage(`{"a":{"first":1,"second":2},"z":9007199254740993}`)
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ingest_transcript", Arguments: input})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("same semantic input rejected: %+v", r.Content)
	}
	var report memory.IngestReport
	b, _ := json.Marshal(r.StructuredContent)
	if err = json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report.Inserted {
		t.Fatal("replay inserted duplicate")
	}
	input.RawSource = json.RawMessage(`{"a":{"first":1,"second":2},"z":9007199254740992}`)
	r, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "ingest_transcript", Arguments: input})
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsError {
		t.Fatal("changed large integer silently accepted")
	}
}

func TestMemoryLinkToolsOverMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := memory.Open(filepath.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`INSERT INTO topics(id,title) VALUES('topic-1','先去本地营业厅问模板'),('topic-2','再去深圳营业厅办销户')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(
		`INSERT INTO memory_links(id, from_kind, from_id, to_kind, to_id, relation, reason, evidence_turn_ids_json, confidence, status, model, created_at)
		 VALUES('link:test','topic','topic-1','topic','topic-2','follow_up','先问模板再办业务','["turn-1"]',0.85,'proposed','curator',1700000000000)`); err != nil {
		t.Fatal(err)
	}

	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServer(store).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	decode := func(t *testing.T, res *mcp.CallToolResult, out any) {
		t.Helper()
		if res.IsError {
			t.Fatalf("tool rejected valid input: %+v", res.Content)
		}
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}

	var list MemoryLinkList
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_memory_links", Arguments: map[string]any{"status": "proposed"}})
	if err != nil {
		t.Fatal(err)
	}
	decode(t, res, &list)
	if len(list.Links) != 1 {
		t.Fatalf("proposed links = %d, want 1", len(list.Links))
	}
	link := list.Links[0]
	if link.LinkID != "link:test" || link.Status != "proposed" || link.Relation != "follow_up" {
		t.Fatalf("link = %+v", link)
	}
	if link.FromTitle != "先去本地营业厅问模板" || link.ToTitle != "再去深圳营业厅办销户" {
		t.Errorf("titles = %q -> %q, want seeded topic titles", link.FromTitle, link.ToTitle)
	}
	if len(link.EvidenceTurnIDs) != 1 || link.EvidenceTurnIDs[0] != "turn-1" {
		t.Errorf("evidence = %v, want [turn-1]", link.EvidenceTurnIDs)
	}

	// Filters: an unrelated topic yields nothing, a non-status value is refused.
	var empty MemoryLinkList
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_memory_links", Arguments: map[string]any{"topic_id": "topic-9"}})
	if err != nil {
		t.Fatal(err)
	}
	decode(t, res, &empty)
	if len(empty.Links) != 0 {
		t.Fatalf("links for unknown topic = %+v, want none", empty.Links)
	}
	badStatus, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_memory_links", Arguments: map[string]any{"status": "reviewed"}})
	if err != nil {
		t.Fatal(err)
	}
	if !badStatus.IsError {
		t.Fatal("list_memory_links accepted an unknown status")
	}

	// Confirming is idempotent and persists.
	for i := 0; i < 2; i++ {
		var reviewed MemoryLinkView
		res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "review_memory_link", Arguments: map[string]any{"link_id": "link:test", "action": "confirm"}})
		if err != nil {
			t.Fatal(err)
		}
		decode(t, res, &reviewed)
		if reviewed.Status != "confirmed" {
			t.Fatalf("review %d status = %q, want confirmed", i, reviewed.Status)
		}
	}
	var stored string
	if err := store.DB().QueryRow(`SELECT status FROM memory_links WHERE id = 'link:test'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "confirmed" {
		t.Fatalf("persisted status = %q, want confirmed", stored)
	}

	badAction, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "review_memory_link", Arguments: map[string]any{"link_id": "link:test", "action": "merge"}})
	if err != nil {
		t.Fatal(err)
	}
	if !badAction.IsError {
		t.Fatal("review_memory_link accepted an unknown action")
	}
	missing, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "review_memory_link", Arguments: map[string]any{"link_id": "link:missing", "action": "confirm"}})
	if err != nil {
		t.Fatal(err)
	}
	if !missing.IsError {
		t.Fatal("review_memory_link accepted an unknown link id")
	}
}
