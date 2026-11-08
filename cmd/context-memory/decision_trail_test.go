package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"recordingmcp/internal/app"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/profile"
)

type mcpDecisionEnv struct {
	store   *memory.Store
	events  *plane.Service
	trail   *app.DecisionTrailService
	scope   string
	session *mcp.ClientSession
}

func (e *mcpDecisionEnv) call(t *testing.T, args map[string]any) app.DecisionTrail {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_decision_trail", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get_decision_trail error: %+v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var trail app.DecisionTrail
	if err := json.Unmarshal(raw, &trail); err != nil {
		t.Fatal(err)
	}
	return trail
}

func (e *mcpDecisionEnv) rawJSON(t *testing.T, args map[string]any) string {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_decision_trail", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	return string(raw)
}

// seedDecisionConversation ingests one segment and applies one decision-cue
// note. Conversation and recording ids are caller-controlled so a test can
// build a multi-session topic.
func seedDecisionConversation(t *testing.T, store *memory.Store, conversationID, recordingID, noteText string) (string, string) {
	t.Helper()
	start, end := int64(0), int64(1500)
	result := asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider: "provider-x", ProviderRequestID: "req-" + conversationID, DeviceID: "dev",
			RecordingID: recordingID, ConversationID: conversationID, SegmentID: "seg-" + conversationID,
			SegmentOffsetMS: 0, SegmentDurationMS: 1500, TranscriptRevision: "rev-1",
			CompletionStatus: "final",
		},
		Turns: []asrinput.Turn{{
			ID: "t-1", OriginalIndex: 1, Text: "会议讨论内容",
			LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end,
		}},
	}
	report, err := store.IngestTranscript(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	ann := memory.Annotations{
		Version: 1, ConversationID: report.ConversationID, Title: "决策会话", RecordingID: report.RecordingID,
		AnnotationVersion: "v1", AnnotationBasis: "人工解释；未核实", Summary: "决策讨论",
		Topics: []memory.TopicAnnotation{{ID: "topic:" + conversationID, Title: "Atlas 项目"}},
		Notes: []memory.NoteAnnotation{{
			ID: "note:" + conversationID, Kind: "suggestion", TopicIDs: []string{"topic:" + conversationID},
			Summary: noteText, EvidenceTurnIDs: []string{"seg-" + conversationID + "/rev-1/t-1"},
		}},
	}
	if err := store.ApplyAnnotations(context.Background(), report.ConversationID, "", ann); err != nil {
		t.Fatal(err)
	}
	return report.ConversationID, report.RecordingID
}

func newMCPDecisionEnv(t *testing.T, noteText string, seedMore bool) *mcpDecisionEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seedDecisionConversation(t, store, "conv-a", "rec-a", noteText)
	if seedMore {
		seedDecisionConversation(t, store, "conv-b", "rec-b", "采用方案 B 更稳妥")
	}
	events, err := plane.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { events.Close() })
	timeSvc := &app.TimeService{Store: store, Events: events}
	worldline := &app.WorldlineService{Time: timeSvc}
	trail := &app.DecisionTrailService{Worldline: worldline, Time: timeSvc}
	scope := "profile-mcp"
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := newServer(store, app.New(store, nil, nil, ""), &scopedDecisionTrail{inner: trail, scope: scope}, true).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return &mcpDecisionEnv{store: store, events: events, trail: trail, scope: scope, session: clientSession}
}

// topicAndCandidate returns the topic id and the candidate containing marker.
func topicAndCandidate(t *testing.T, trail app.DecisionTrail, marker string) (string, string) {
	t.Helper()
	if len(trail.Topics) != 1 {
		t.Fatalf("topics=%d", len(trail.Topics))
	}
	topic := trail.Topics[0]
	for _, record := range topic.History {
		if record.State == app.DecisionCandidate && (marker == "" || strings.Contains(record.Text, marker)) {
			return topic.ID, record.CandidateID
		}
	}
	t.Fatalf("candidate %q not found: %+v", marker, topic.History)
	return "", ""
}

func TestMCPDecisionTrailCurrent(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{
		ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "current-1",
	}); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID})
	if len(trail.Topics) != 1 || trail.Topics[0].Current == nil || trail.Topics[0].CurrentState != app.DecisionConfirmed {
		t.Fatalf("trail=%+v", trail)
	}
}

func TestMCPDecisionTrailHistory(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "history-1"}); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID, "include_history": true})
	if len(trail.Topics[0].History) < 2 {
		t.Fatalf("history=%+v", trail.Topics[0].History)
	}
}

func TestMCPDecisionTrailNoEvidence(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	// A raw review with no evidence must degrade instead of confirming.
	if _, err := env.events.Ingest(context.Background(), []plane.Event{{
		ID: "decision-review-" + topicID + "-no-evidence", Type: "decision.review",
		Timestamp: time.Now().UTC(), ObservedAt: time.Now().UTC(), Source: "local-web", Content: "raw",
		StructuredData: map[string]any{
			"scope": env.scope, "origin": "decision-trail", "topic_id": topicID, "candidate_id": candidate,
			"action": "confirm", "text": "raw confirm", "reviewer": "raw", "evidence": []string{},
			"request_id": "no-evidence", "review_payload_hash": "raw",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID})
	if trail.Topics[0].Current != nil {
		t.Fatalf("no-evidence review produced a current decision: %+v", trail.Topics[0].Current)
	}
	if !strings.Contains(strings.Join(trail.Topics[0].UnresolvedReasons, ","), "source_unknown") {
		t.Fatalf("reasons=%v", trail.Topics[0].UnresolvedReasons)
	}
}

func TestMCPDecisionTrailConflict(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", true)
	trail := env.call(t, map[string]any{})
	if trail.Topics[0].Current != nil || trail.Topics[0].CurrentState != app.DecisionUnresolved {
		t.Fatalf("topic=%+v", trail.Topics[0])
	}
	if !strings.Contains(strings.Join(trail.Topics[0].UnresolvedReasons, ","), "multiple_candidates") {
		t.Fatalf("reasons=%v", trail.Topics[0].UnresolvedReasons)
	}
}

func TestMCPDecisionTrailStaleSource(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "stale-1"}); err != nil {
		t.Fatal(err)
	}
	// Supersede the segment so the confirmation's evidence goes stale.
	start, end := int64(0), int64(1500)
	result := asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider: "provider-x", ProviderRequestID: "req-rev2", DeviceID: "dev",
			RecordingID: "rec-a", ConversationID: "conv-a", SegmentID: "seg-conv-a",
			SegmentOffsetMS: 0, SegmentDurationMS: 1500, TranscriptRevision: "rev-2",
			SupersedesRevision: "rev-1", CompletionStatus: "final",
		},
		Turns: []asrinput.Turn{{ID: "t-1", OriginalIndex: 1, Text: "会议讨论内容", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end}},
	}
	if _, err := env.store.IngestTranscript(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID})
	if trail.Topics[0].Current != nil {
		t.Fatalf("stale confirmation is current: %+v", trail.Topics[0].Current)
	}
	reasons := strings.Join(trail.Topics[0].UnresolvedReasons, ",")
	if !strings.Contains(reasons, "source_invalidated") && !strings.Contains(reasons, "latest_confirmation_source_invalidated") {
		t.Fatalf("reasons=%s", reasons)
	}
}

func TestMCPDecisionTrailUnconfirmedCandidate(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	trail := env.call(t, map[string]any{})
	if trail.Topics[0].CurrentState != app.DecisionCandidate || trail.Topics[0].Current != nil {
		t.Fatalf("topic=%+v", trail.Topics[0])
	}
}

func TestMCPDecisionTrailConfirmed(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "confirmed-1", Reviewer: "dana"}); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID})
	current := trail.Topics[0].Current
	if current == nil || current.ReviewStatus != "accepted" || current.Reviewer != "dana" {
		t.Fatalf("current=%+v", current)
	}
}

func TestMCPDecisionTrailMultiSession(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", true)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "方案 A")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "multi-1"}); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID})
	if trail.Topics[0].Counts.Candidates != 2 {
		t.Fatalf("expected both sessions merged into one topic: %+v", trail.Topics[0].Counts)
	}
	for _, evidence := range trail.Topics[0].Current.Evidence {
		if evidence.ConversationID == "" || evidence.RecordingID == "" {
			t.Fatalf("evidence lost its source: %+v", evidence)
		}
	}
}

func TestMCPDecisionTrailKnownAt(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "known-1"}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	trail := env.call(t, map[string]any{"topic_id": topicID, "known_at": before})
	if trail.Topics[0].Current != nil || trail.Topics[0].Counts.Confirmed != 0 {
		t.Fatalf("known_at did not filter the later review: %+v", trail.Topics[0])
	}
	// A candidate whose mention time is unknown is not part of a historical
	// view: the projection must not guess when it was mentioned.
	for _, record := range trail.Topics[0].History {
		if record.State == app.DecisionCandidate {
			t.Fatalf("unknown-time candidate leaked into known_at view: %+v", record)
		}
	}
}

func TestMCPDecisionTrailProfileIsolation(t *testing.T) {
	alpha := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	beta := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := alpha.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := alpha.trail.Review(context.Background(), alpha.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "iso-1", Reviewer: "alpha"}); err != nil {
		t.Fatal(err)
	}
	betaTrail := beta.call(t, map[string]any{})
	if betaTrail.Topics[0].Current != nil || betaTrail.Topics[0].Counts.Confirmed != 0 {
		t.Fatalf("alpha review leaked into beta: %+v", betaTrail.Topics[0])
	}
	_ = topicID
}

func TestMCPDecisionTrailByteBudget(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	initial := env.call(t, map[string]any{})
	topicID, candidate := topicAndCandidate(t, initial, "")
	if _, err := env.trail.Review(context.Background(), env.scope, app.DecisionReview{ExpectedVersion: initial.Version, CandidateID: candidate, Action: "confirm", RequestID: "budget-1"}); err != nil {
		t.Fatal(err)
	}
	trail := env.call(t, map[string]any{"topic_id": topicID, "max_bytes": 1024})
	raw, _ := json.Marshal(trail)
	if len(raw) > 1024 {
		t.Fatalf("serialized trail=%d bytes > 1024", len(raw))
	}
	if len(trail.Topics[0].History) != 0 {
		t.Fatalf("history was not trimmed first: %d records", len(trail.Topics[0].History))
	}
	if !trail.Truncated {
		t.Fatal("expected truncated=true")
	}
	if trail.Topics[0].Current == nil {
		t.Fatal("byte budget dropped the current decision")
	}
}

func TestMCPDecisionTrailDeterministicRepeat(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	first := env.rawJSON(t, map[string]any{})
	second := env.rawJSON(t, map[string]any{})
	if first != second {
		t.Fatalf("repeat differs:\n%s\n%s", first, second)
	}
}

func TestMCPDecisionTrailSchemaVersion(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	trail := env.call(t, map[string]any{})
	if trail.SchemaVersion != "decision-trail-v1" {
		t.Fatalf("schema_version=%q", trail.SchemaVersion)
	}
}

func TestMCPDecisionTrailReadOnlyRegistration(t *testing.T) {
	env := newMCPDecisionEnv(t, "最终决定采用方案 A", false)
	tools, err := env.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "get_decision_trail" {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatalf("get_decision_trail is not marked read-only: %+v", tool.Annotations)
			}
		}
	}
	if !found {
		t.Fatal("get_decision_trail not registered in read-only mode")
	}
}

func TestMCPProfileUsesProfileEventPlane(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	password := "profile-password-123"
	t.Setenv(profile.DefaultPasswordEnv, password)
	manager, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := manager.Create("alpha", password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Open(paths.Database); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cli := profile.RegisterCLI(fs)
	if err := fs.Parse([]string{"--profile", "alpha", "--profiles-root", root}); err != nil {
		t.Fatal(err)
	}
	if !cli.Selected() {
		t.Fatal("profile CLI did not report the selected profile")
	}
	scope, eventsPath, err := cli.WorldlinePlane(paths.Database, "")
	if err != nil {
		t.Fatal(err)
	}
	if scope != "alpha" || eventsPath != paths.Plane {
		t.Fatalf("scope=%q events=%q want %q", scope, eventsPath, paths.Plane)
	}
	if _, err := os.Stat(filepath.Dir(eventsPath)); err != nil {
		t.Fatalf("profile plane directory missing: %v", err)
	}
}
