package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"recordingmcp/fixtures"
	"recordingmcp/internal/mcpserver"
	"recordingmcp/internal/plane"
)

func decodeResult[T any](t *testing.T, r *mcp.CallToolResult) T {
	t.Helper()
	if r.IsError {
		t.Fatalf("tool error: %+v", r.Content)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Exercise the actual executable over stdio, not a direct call to Go handlers.
func TestStdioDatabaseRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "recording-mcp")
	db := filepath.Join(tmp, "recordings.db")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/server")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connect := func() *mcp.ClientSession {
		client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
		command := exec.CommandContext(ctx, binary, "--db", db, "--seed", "--at", "2026-10-03T21:00:00+08:00")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
		if err != nil {
			t.Fatalf("connect: %v\n%s", err, stderr.String())
		}
		return session
	}
	session := connect()
	defer func() {
		if session != nil {
			session.Close()
		}
	}()
	call := func(name string, args any) *mcp.CallToolResult {
		t.Helper()
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := []string{"get_commitments", "get_conversation_context", "get_current_state", "get_person_context", "get_timeline", "ingest_events", "record_feedback", "search_events"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}
	var seeded []plane.Event
	if err = json.Unmarshal(fixtures.Recordings, &seeded); err != nil {
		t.Fatal(err)
	}
	page := decodeResult[mcpserver.EventsResult](t, call("search_events", map[string]any{"query": "", "limit": 2}))
	if page.Total != len(seeded) || len(page.Events) != 2 || page.NextOffset == nil || *page.NextOffset != 2 {
		t.Fatalf("bad page: %+v", page)
	}
	person := decodeResult[mcpserver.EventsResult](t, call("get_person_context", map[string]any{"person_id": "示例联系人甲"}))
	if len(person.Events) == 0 {
		t.Fatal("entity name should match person query")
	}
	conv := decodeResult[mcpserver.ConversationResult](t, call("get_conversation_context", map[string]any{"conversation_id": "conv-person-b-payment", "detail_level": "full"}))
	wantConversationEvents := 0
	for _, e := range seeded {
		if e.ConversationID == "conv-person-b-payment" {
			wantConversationEvents++
		}
	}
	if wantConversationEvents == 0 || len(conv.Events) != wantConversationEvents || len(conv.EventIDs) != wantConversationEvents {
		t.Fatalf("conversation: %+v", conv)
	}
	decodeResult[mcpserver.EventsResult](t, call("get_timeline", map[string]any{"start_time": "2026-10-03T00:00:00+08:00", "end_time": "2026-10-04T00:00:00+08:00"}))
	decodeResult[mcpserver.StatesResult](t, call("get_current_state", map[string]any{"dimensions": []string{"task"}}))
	before := decodeResult[mcpserver.StatesResult](t, call("get_commitments", map[string]any{"person": "示例联系人甲"}))
	if len(before.States) != 1 {
		t.Fatalf("expected one BOM commitment: %+v", before)
	}
	var created plane.Event
	for _, e := range seeded {
		if e.Type == "commitment.created" {
			created = e
			break
		}
	}
	if created.ID == "" {
		t.Fatal("missing commitment fixture")
	}
	feedback := plane.Event{ID: "test-feedback-complete", Type: "commitment.completed", Timestamp: time.Date(2026, 10, 3, 12, 5, 0, 0, time.UTC), ObservedAt: time.Date(2026, 10, 3, 12, 5, 0, 0, time.UTC), Source: "agent:test", Content: "网络 BOM 已完成交付（测试）", Confidence: 0.99, Priority: "high", StructuredData: map[string]any{"target_id": created.StructuredData["commitment_id"]}}
	result := decodeResult[mcpserver.FeedbackResult](t, call("record_feedback", map[string]any{"event": feedback}))
	if result.Inserted != 1 {
		t.Fatalf("feedback: %+v", result)
	}
	if r := decodeResult[mcpserver.FeedbackResult](t, call("record_feedback", map[string]any{"event": feedback})); r.Inserted != 0 {
		t.Fatal("feedback replay not idempotent")
	}
	after := decodeResult[mcpserver.StatesResult](t, call("get_commitments", map[string]any{"status": "completed", "person": "示例联系人甲"}))
	if len(after.States) != 1 {
		t.Fatalf("feedback not projected: %+v", after)
	}
	historical := decodeResult[mcpserver.StatesResult](t, call("get_commitments", map[string]any{"time": "2026-10-03T20:00:00+08:00"}))
	if len(historical.States) != 1 || historical.States[0].Status == "completed" {
		t.Fatal("feedback leaked into past")
	}
	clone := created
	clone.ID = "test-import"
	clone.StructuredData = map[string]any{"commitment_id": "test-import"}
	imported := decodeResult[mcpserver.IngestResult](t, call("ingest_events", map[string]any{"events": []plane.Event{clone}}))
	if imported.Inserted != 1 {
		t.Fatal("import failed")
	}
	clone.Content = "conflicting content"
	if !call("ingest_events", map[string]any{"events": []plane.Event{clone}}).IsError {
		t.Fatal("conflicting ID accepted")
	}
	if !call("search_events", map[string]any{"query": "", "min_confidence": 2}).IsError {
		t.Fatal("invalid confidence accepted")
	}
	if !call("get_timeline", map[string]any{"start_time": "bad", "end_time": "bad"}).IsError {
		t.Fatal("invalid timestamp accepted")
	}
	if !call("get_conversation_context", map[string]any{"conversation_id": "missing"}).IsError {
		t.Fatal("missing conversation accepted")
	}
	if !call("ingest_events", map[string]any{"events": []plane.Event{}}).IsError {
		t.Fatal("empty import accepted")
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	session = nil
	session = connect()
	reopened := decodeResult[mcpserver.EventsResult](t, call("search_events", map[string]any{"query": ""}))
	if reopened.Total != len(seeded)+2 {
		t.Fatalf("persistence/reseed mismatch: %d", reopened.Total)
	}
}
