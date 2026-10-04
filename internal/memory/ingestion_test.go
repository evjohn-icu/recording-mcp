package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"recordingmcp/internal/asrinput"
)

func sampleResult(segment, revision string, offset int64) asrinput.Result {
	start, end := int64(10), int64(900)
	globalStart, globalEnd := offset+start, offset+end
	sp := "spk:device-1:1"
	return asrinput.Result{ContractVersion: asrinput.ContractVersion, Source: asrinput.Source{Provider: "provider-x", ProviderRequestID: "req-" + segment, DeviceID: "device-1", RecordingID: "rec-1", ConversationID: "meeting-1", SegmentID: segment, SegmentOffsetMS: offset, SegmentDurationMS: 1000, TranscriptRevision: revision, CompletionStatus: "final", RawEnvelope: json.RawMessage(`{"vendor_extra":"kept"}`)}, Turns: []asrinput.Turn{{ID: "utt-1", OriginalIndex: 1, Text: "hello " + segment, LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &globalStart, GlobalEndMS: &globalEnd, SpeakerScopedID: &sp}}, RawSource: json.RawMessage(`{"final_response_frame":{"future_field":true},"provider_payload":{"text":"hello"}}`)}
}

func openTempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/memory.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIngestTranscriptSegmentsReplayAndReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/memory.sqlite"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := sampleResult("seg-a", "rev-1", 0)
	first, err := s.IngestTranscript(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.IngestTranscript(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted || second.Turns != first.Turns || second.SegmentIndex != first.SegmentIndex || second.AnalysisStatus != "pending" {
		t.Fatalf("replay report lost persisted segment state: %+v", second)
	}
	b := sampleResult("seg-b", "rev-1", 1000)
	b.Source.RecordingID = "rec-2"
	b.Source.ProviderRequestID = "req-seg-b"
	if _, err = s.IngestTranscript(ctx, b); err != nil {
		t.Fatal(err)
	}
	status, err := s.GetProcessingStatus(ctx, first.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ReceivedSegments != 2 || status.OriginalTurns != 2 || status.UnclassifiedTurns != 2 || status.TopicLinkedTurns != 0 || status.IdentityUnconfirmedTurns != 2 || status.AnalysisStatus != "pending" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.Query(ctx, Query{ConversationID: first.ConversationID, IncludeTurns: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range page.Items {
		if it.Kind == "turn" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("after reopen got %d turns", n)
	}
}

func TestIngestRejectsPartialAndConflictsAtomically(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()
	r := sampleResult("seg-a", "rev-1", 0)
	r.Source.CompletionStatus = "partial"
	if _, err := s.IngestTranscript(ctx, r); err == nil {
		t.Fatal("partial result accepted")
	}
	r.Source.CompletionStatus = "final"
	if _, err := s.IngestTranscript(ctx, r); err != nil {
		t.Fatal(err)
	}
	changed := sampleResult("seg-a", "rev-1", 0)
	changed.Turns[0].Text = "changed"
	if _, err := s.IngestTranscript(ctx, changed); err == nil {
		t.Fatal("content conflict accepted")
	}
	if _, err := s.IngestTranscript(ctx, sampleResult("seg-a", "rev-2", 0)); err == nil {
		t.Fatal("revision replacement accepted")
	}
	status, err := s.GetProcessingStatus(ctx, stableKey("conversation", "device-1", "meeting-1"))
	if err != nil {
		t.Fatal(err)
	}
	if status.ReceivedSegments != 1 || status.OriginalTurns != 1 {
		t.Fatalf("failed writes were not rolled back: %+v", status)
	}
}

func TestIngestReplayCanonicalizesRawJSONWithoutLosingNumberPrecision(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()
	// Store one snapshot, then replay with reordered raw object keys.
	equalA := sampleResult("seg-b", "rev-1", 0)
	equalA.RawSource = json.RawMessage(`{"left":{"a":1,"b":2},"provider_payload":{"large":9007199254740992}}`)
	equalA.Source.RawEnvelope = json.RawMessage(`{"first":1,"second":2}`)
	if _, err := s.IngestTranscript(ctx, equalA); err != nil {
		t.Fatal(err)
	}
	equalB := sampleResult("seg-b", "rev-1", 0)
	equalB.RawSource = json.RawMessage(`{"provider_payload":{"large":9007199254740992},"left":{"b":2,"a":1}}`)
	equalB.Source.RawEnvelope = json.RawMessage(`{"second":2,"first":1}`)
	if report, err := s.IngestTranscript(ctx, equalB); err != nil || report.Inserted {
		t.Fatalf("semantic replay failed: %+v %v", report, err)
	}
	changed := sampleResult("seg-b", "rev-1", 0)
	changed.RawSource = json.RawMessage(`{"provider_payload":{"large":9007199254740993},"left":{"a":1,"b":2}}`)
	changed.Source.RawEnvelope = json.RawMessage(`{"first":1,"second":2}`)
	if _, err := s.IngestTranscript(ctx, changed); err == nil {
		t.Fatal("distinct large integer was treated as equal")
	}
}

func TestIngestPreservesRawProviderMaterial(t *testing.T) {
	s := openTempStore(t)
	r := sampleResult("seg-a", "rev-1", 0)
	report, err := s.IngestTranscript(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var source, result string
	err = s.db.QueryRow("SELECT source_json,result_json FROM source_attachments WHERE conversation_id=?", report.ConversationID).Scan(&source, &result)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"vendor_extra", "future_field", "provider_payload", "hello seg-a"} {
		if !strings.Contains(source+result, needle) {
			t.Errorf("preserved snapshot missing %q", needle)
		}
	}
}

func TestFinalWithoutTurnsAndMissingConversation(t *testing.T) {
	s := openTempStore(t)
	empty := sampleResult("silence", "rev-1", 0)
	empty.Turns = nil
	if _, err := s.IngestTranscript(context.Background(), empty); err != nil {
		t.Fatalf("final silence result should be retained: %v", err)
	}
	missing := sampleResult("unknown", "rev-1", 0)
	missing.Source.ConversationID = ""
	if _, err := s.IngestTranscript(context.Background(), missing); err == nil {
		t.Fatal("missing conversation grouping was silently inferred")
	}
}

func TestProcessingStatusIncludesLegacyImportsAndRequiresConfirmedIdentity(t *testing.T) {
	s := openTempStore(t)
	transcript, annotations := synthetic()
	if _, err := s.Import(context.Background(), transcript, annotations, "synthetic.wav", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	status, err := s.GetProcessingStatus(context.Background(), annotations.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ReceivedSegments != 1 || status.OriginalTurns != 2 || status.TopicLinkedTurns != 2 || status.UnclassifiedTurns != 0 || status.IdentityUnconfirmedTurns != 2 {
		t.Fatalf("legacy status incorrect: %+v", status)
	}
	if status.AnalysisStatus != "partial" {
		t.Fatalf("annotated conversation status = %q, want partial", status.AnalysisStatus)
	}
}

func TestIngestUsesUnknownTimesAndAnonymousSpeaker(t *testing.T) {
	s := openTempStore(t)
	r := sampleResult("seg-a", "rev-1", 5000)
	r.Turns[0].LocalStartMS = nil
	r.Turns[0].LocalEndMS = nil
	r.Turns[0].GlobalStartMS = nil
	r.Turns[0].GlobalEndMS = nil
	r.Turns[0].SpeakerScopedID = nil
	report, err := s.IngestTranscript(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	var gs, ge, sp any
	err = s.db.QueryRow("SELECT global_start_ms,global_end_ms,speaker_scoped_id FROM turns WHERE conversation_id=?", report.ConversationID).Scan(&gs, &ge, &sp)
	if err != nil {
		t.Fatal(err)
	}
	if gs != nil || ge != nil || sp != nil {
		t.Fatalf("unknown values were fabricated: %v %v %v", gs, ge, sp)
	}
	if report.SegmentStartMS != 5000 || report.SegmentEndMS != 6000 {
		t.Fatalf("wrong segment-only coverage: %+v", report)
	}
}

func TestSupersessionIsRejected(t *testing.T) {
	s := openTempStore(t)
	ctx := context.Background()
	first := sampleResult("seg-a", "rev-1", 0)
	report, err := s.IngestTranscript(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := sampleResult("seg-a", "rev-2", 0)
	second.Source.SupersedesRevision = "rev-1"
	if _, err = s.IngestTranscript(ctx, second); err == nil {
		t.Fatal("revision replacement accepted")
	}
	status, err := s.GetProcessingStatus(ctx, report.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ReceivedSegments != 1 || status.OriginalTurns != 1 {
		t.Fatalf("rejected replacement changed snapshot: %+v", status)
	}
}
