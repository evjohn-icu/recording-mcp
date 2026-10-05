package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestExecutableStdioRoundTrip runs the real context-memory executable over
// real stdio: it ingests one segment through the write tool and reads it back
// through the current-state tools. The read calls deliberately omit optional
// fields (include_stale) so a schema regression that marks them required is
// caught at the wire level, not just in-process.
func TestExecutableStdioRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "context-memory")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	command := exec.CommandContext(ctx, binary, "--db", filepath.Join(tmp, "smoke.db"))
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.IsError {
			for _, c := range r.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					t.Fatalf("%s error: %s", name, tc.Text)
				}
			}
			t.Fatalf("%s error: %#v", name, r.Content)
		}
		return r
	}
	mustJSON := func(r *mcp.CallToolResult, out any) {
		t.Helper()
		b, _ := json.Marshal(r.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range listed.Tools {
		got[tool.Name] = true
	}
	want := []string{
		"query_context", "get_conversation_context", "get_context_item", "list_topics",
		"get_processing_status", "get_annotation_history", "list_conversations",
		"get_evidence_context", "search_memory", "build_context", "list_memory_links",
		"ingest_transcript", "apply_annotations", "review_memory_link",
	}
	if len(got) != len(want) {
		t.Fatalf("tool surface = %v, want %d tools", got, len(want))
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("missing tool %s", name)
		}
	}

	ingest := call("ingest_transcript", map[string]any{
		"contract_version": "asr-input/0.1",
		"source": map[string]any{
			"provider": "synthetic-provider", "provider_request_id": "req-1",
			"device_id": "device-1", "recording_id": "rec-1", "conversation_id": "conv-1",
			"segment_id": "seg-1", "segment_offset_ms": 0, "segment_duration_ms": 1000,
			"transcript_revision": "rev-1", "completion_status": "final",
		},
		"turns": []map[string]any{{
			"id": "utt-1", "original_index": 1,
			"text": "离线同步加入下一次讨论", "speaker_scoped_id": "speaker-1",
		}},
	})
	var report map[string]any
	mustJSON(ingest, &report)
	if report["inserted"] != true {
		t.Fatalf("ingest report = %v", report)
	}

	convs := call("list_conversations", map[string]any{"limit": 5})
	var page map[string]any
	mustJSON(convs, &page)
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("conversations = %v", page["items"])
	}
	convID, _ := items[0].(map[string]any)["conversation_id"].(string)

	status := call("get_processing_status", map[string]any{"conversation_id": convID})
	var ps map[string]any
	mustJSON(status, &ps)
	if ps["original_turns"].(float64) != 1 {
		t.Fatalf("status = %v", ps)
	}

	// Omitting include_stale must stay valid: it is an optional opt-in.
	search := call("search_memory", map[string]any{"text": "离线同步", "limit": 3})
	var sp map[string]any
	mustJSON(search, &sp)
	hits, _ := sp["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("search returned no hits: %v", sp)
	}
	hit := hits[0].(map[string]any)

	item := call("get_context_item", map[string]any{"id": hit["id"]})
	var detail map[string]any
	mustJSON(item, &detail)

	built := call("build_context", map[string]any{"query": "离线同步", "include_raw_evidence": true, "max_items": 5})
	var pkg map[string]any
	mustJSON(built, &pkg)
	pkgItems, _ := pkg["items"].([]any)
	evidence, _ := pkg["evidence"].([]any)
	if len(pkgItems) == 0 || len(evidence) == 0 {
		t.Fatalf("build_context items=%d evidence=%d", len(pkgItems), len(evidence))
	}
	for _, key := range []string{"coverage", "gaps", "truncated", "degraded", "degrade_reason", "trace"} {
		if _, ok := pkg[key]; !ok {
			t.Fatalf("build_context missing %q: %v", key, pkg)
		}
	}
}
