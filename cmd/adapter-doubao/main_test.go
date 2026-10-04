package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicCLIOutputAndFailureDoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "responses.jsonl")
	meta := filepath.Join(dir, "metadata.json")
	out := filepath.Join(dir, "normalized.json")
	metadata := `{"provider":"doubao","provider_request_id":"req","device_id":"dev","recording_id":"rec","conversation_id":"conv","segment_id":"s1","segment_offset_ms":1000,"segment_duration_ms":100,"transcript_revision":"r1","supersedes_revision":"r0","completion_status":"final","extension":{"future":true}}`
	if err := os.WriteFile(meta, []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	partial := `{"message_type":9,"flags":1,"sequence":2,"is_last_package":false,"code":0,"payload":{"result":{"text":"partial","utterances":[]}}}` + "\n"
	if err := os.WriteFile(input, []byte(partial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("previous valid content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := convertAndWrite(input, meta, out); err == nil {
		t.Fatal("partial stream unexpectedly succeeded")
	}
	got, _ := os.ReadFile(out)
	if string(got) != "previous valid content" {
		t.Fatalf("failed conversion clobbered output: %q", got)
	}
	final := `{"message_type":9,"flags":3,"sequence":3,"is_last_package":true,"code":0,"payload":{"result":{"text":"final","utterances":[{"text":"hello","start_time":4,"end_time":20,"additions":{"speaker_id":0},"words":[{"text":"hi","start_time":-1,"end_time":4}],"vendor_future":"kept"}],"vendor_payload_field":"kept"}}}` + "\n"
	if err := os.WriteFile(input, []byte(partial+final), 0600); err != nil {
		t.Fatal(err)
	}
	if err := convertAndWrite(input, meta, out); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("output mode=%o, want 600", stat.Mode().Perm())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err = json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	source := obj["source"].(map[string]any)
	if source["conversation_id"] != "conv" || source["supersedes_revision"] != "r0" {
		t.Fatalf("source metadata not propagated: %+v", source)
	}
	if !strings.Contains(string(b), "extension") || !strings.Contains(string(b), "vendor_future") || !strings.Contains(string(b), "-1") {
		t.Fatal("source metadata, provider unknown fields, or word timing lost")
	}
}
