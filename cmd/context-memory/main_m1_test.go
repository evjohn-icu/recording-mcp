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
	"recordingmcp/internal/generate"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/pipeline"
	"recordingmcp/internal/process"
	"recordingmcp/internal/semantic"
	"recordingmcp/internal/worker"
)

// m1StubGenerator returns drafts for the turns it recognizes in the window it
// is handed, mirroring a real generator: it only proposes from the closed
// evidence set, so per-segment extract jobs never emit out-of-window evidence.
type m1StubGenerator struct {
	intention   generate.Draft
	observation generate.Draft
}

func (g m1StubGenerator) Generate(_ context.Context, _ string, evidenceTurnIDs []string) ([]generate.Draft, string, error) {
	drafts := []generate.Draft{}
	if len(evidenceTurnIDs) > 0 && evidenceTurnIDs[0] == g.intention.EvidenceTurnIDs[0] {
		drafts = append(drafts, g.intention)
	}
	for _, id := range evidenceTurnIDs {
		if id == g.observation.EvidenceTurnIDs[0] {
			drafts = append(drafts, g.observation)
			break
		}
	}
	return drafts, "stub-model", nil
}

// TestM1IngestPublishAutoIndexServesEvidence drives the M1 loop end to end
// in-process: ingest two asr-input/0.1 segments, distill with a stub generator
// and no Decision engine, publish, and let the real worker loop drain the
// queue including the embed stage. The MCP surface must then report a ready
// semantic index, serve literal search hits carrying evidence turn ids, and
// return verbatim evidence for the published note.
func TestM1IngestPublishAutoIndexServesEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "m1.db")
	store, err := memory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()

	ingest := func(segmentID string, turns ...asrinput.Turn) string {
		t.Helper()
		result := asrinput.Result{
			ContractVersion: asrinput.ContractVersion,
			Source: asrinput.Source{
				Provider: "m1-provider", ProviderRequestID: "req-" + segmentID,
				DeviceID: "device-m1", RecordingID: "rec-m1", ConversationID: "conv-m1",
				SegmentID: segmentID, SegmentDurationMS: 4000,
				TranscriptRevision: "rev-1", CompletionStatus: "final",
			},
			Turns: turns,
		}
		report, err := store.IngestTranscript(ctx, result)
		if err != nil {
			t.Fatalf("ingest %s: %v", segmentID, err)
		}
		return report.ConversationID
	}
	start, end := int64(0), int64(2000)
	speaker := "speaker-1"
	convID := ingest("seg-1",
		asrinput.Turn{ID: "u1", OriginalIndex: 1, Text: "我周五下午要拜访客户讨论合同细节", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end, SpeakerScopedID: &speaker},
		asrinput.Turn{ID: "u2", OriginalIndex: 2, Text: "记得把会议纪要整理成文档", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end},
	)
	if ingest("seg-2",
		asrinput.Turn{ID: "u3", OriginalIndex: 1, Text: "会议室投影仪坏了需要联系行政报修", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end},
	) != convID {
		t.Fatal("segments of one recording must bind to one conversation")
	}

	turns, err := pipeline.LoadConversationTurns(ctx, db, convID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("loaded turns = %d, want 3", len(turns))
	}
	stub := m1StubGenerator{
		intention:   generate.Draft{Kind: "intention", Summary: "计划周五下午拜访客户讨论合同细节", TopicTitle: "客户拜访", EvidenceTurnIDs: []string{turns[0].ID}},
		observation: generate.Draft{Kind: "observation", Summary: "会议室投影仪损坏需要报修", TopicTitle: "设备报修", EvidenceTurnIDs: []string{turns[2].ID}},
	}

	// The real worker loop, wired exactly like cmd/context-worker but without
	// a Decision engine and with a stub generator.
	extract := &pipeline.ExtractHandler{DB: db, ProcessorVersion: "extract-v1", Generator: stub}
	publish := &pipeline.PublishHandler{DB: db, Store: store, ProcessorVersion: "publish-v1"}
	curate := &pipeline.CurateHandler{DB: db, DBPath: dbPath, Timeout: time.Second, Model: "curator", ProcessorVersion: "curate-v1"}
	embed := &pipeline.EmbedHandler{DB: db, Provider: semantic.NewHashProvider(semantic.DefaultHashDims)}
	dispatch := &stageDispatch{stages: map[string]worker.Executor{
		"extract": extract, "publish": publish, "curate": curate, "embed": embed,
	}}
	cfg := worker.Config{
		DB: db, WorkerID: "m1-test", Stages: []string{"extract", "publish", "curate", "embed"},
		Lease: 30 * time.Second, ProcessorVersion: "worker-v1", Executor: dispatch,
	}
	drained := false
	for range 30 {
		more, err := worker.RunOnce(ctx, cfg)
		if err != nil {
			t.Fatalf("worker run: %v", err)
		}
		if !more {
			drained = true
			break
		}
	}
	if !drained {
		t.Fatal("worker loop did not drain the queue")
	}

	var decisionRuns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM decision_runs`).Scan(&decisionRuns); err != nil {
		t.Fatal(err)
	}
	if decisionRuns != 0 {
		t.Fatalf("decision_runs = %d, want 0 without a Decision engine", decisionRuns)
	}
	var publishEmbedJobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM processing_jobs WHERE stage='embed' AND idempotency_key LIKE 'embed|' || ? || '|%'`, convID).Scan(&publishEmbedJobs); err != nil {
		t.Fatal(err)
	}
	if publishEmbedJobs < 1 {
		t.Fatalf("publish-keyed embed jobs = %d, want at least one", publishEmbedJobs)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := newServer(store, false, nil, nil).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "m1-test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s rejected input: %+v", name, res.Content)
		}
		return res
	}

	status := call("get_processing_status", map[string]any{"conversation_id": convID})
	var ps memory.ProcessingStatus
	decodeStructured(t, "get_processing_status", status, &ps)
	if ps.IndexingStatus != "ready" {
		t.Fatalf("indexing_status = %q, want ready", ps.IndexingStatus)
	}

	search := call("search_memory", map[string]any{"text": "投影仪"})
	var page memory.SearchPage
	decodeStructured(t, "search_memory", search, &page)
	var noteHit, turnHit *memory.SearchHit
	for i := range page.Hits {
		switch page.Hits[i].Kind {
		case "note":
			noteHit = &page.Hits[i]
		case "turn":
			turnHit = &page.Hits[i]
		}
	}
	if noteHit == nil || len(noteHit.EvidenceTurnIDs) == 0 || noteHit.EvidenceTurnIDs[0] != turns[2].ID {
		t.Fatalf("search_memory note hit = %+v, want evidence turn %s", noteHit, turns[2].ID)
	}
	if turnHit == nil || !strings.Contains(turnHit.Text, "投影仪") {
		t.Fatalf("search_memory turn hit = %+v, want verbatim 投影仪 turn", turnHit)
	}

	evidence := call("get_evidence_context", map[string]any{"turn_id": turns[2].ID})
	var ev memory.EvidenceContext
	decodeStructured(t, "get_evidence_context", evidence, &ev)
	if ev.Stale {
		t.Fatal("current evidence reported stale")
	}
	var target *memory.TurnEvidence
	for i := range ev.Turns {
		if ev.Turns[i].ID == turns[2].ID {
			target = &ev.Turns[i]
		}
	}
	if target == nil || !strings.Contains(target.Text, "投影仪") {
		t.Fatalf("evidence context = %+v, want verbatim target turn %s", ev, turns[2].ID)
	}
}

// stageDispatch mirrors cmd/context-worker's stageExecutor for in-process
// tests.
type stageDispatch struct {
	stages map[string]worker.Executor
}

func (s *stageDispatch) Execute(ctx context.Context, job process.Job) (string, error) {
	exec, ok := s.stages[job.Stage]
	if !ok {
		return "", nil
	}
	return exec.Execute(ctx, job)
}

func decodeStructured(t *testing.T, name string, res *mcp.CallToolResult, into any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: encode structured content: %v", name, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: decode structured content: %v (%s)", name, err, b)
	}
}
