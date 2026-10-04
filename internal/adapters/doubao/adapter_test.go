package doubao

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"recordingmcp/internal/asrinput"
)

func testMeta() asrinput.Metadata {
	return asrinput.Metadata{Provider: "doubao", ProviderRequestID: "req-a", DeviceID: "device-a", RecordingID: "rec-a", ConversationID: "conv-a", SegmentID: "seg-01", SegmentOffsetMS: 300000, SegmentDurationMS: 1000, TranscriptRevision: "rev-1", SupersedesRevision: "rev-0", CompletionStatus: "final"}
}
func response(flags int, last bool, payload any) string {
	b, _ := json.Marshal(payload)
	f := map[string]any{"message_type": 9, "flags": flags, "sequence": 1, "is_last_package": last, "code": 0, "payload": json.RawMessage(b), "provider_frame_extension": "preserve-me"}
	out, _ := json.Marshal(f)
	return string(out) + "\n"
}
func makeUtt(text any, start, end, speaker any) map[string]any {
	return map[string]any{"text": text, "start_time": start, "end_time": end, "definite": false, "additions": map[string]any{"speaker_id": speaker, "unknown_addition": "keep"}, "words": []any{map[string]any{"text": "x", "start_time": -1, "end_time": 20, "provider_word_field": "keep"}}, "future_utterance_field": map[string]any{"keep": true}}
}
func finalPayload(us ...any) map[string]any {
	return map[string]any{"audio_info": map[string]any{"duration": 1000, "new_info": "keep"}, "result": map[string]any{"text": "cumulative full transcription", "utterances": us, "future_result_field": 17}, "future_payload_field": "preserve"}
}

func TestConvertUsesFinalSnapshotOnlyAndPreservesProviderEvidence(t *testing.T) {
	partial := finalPayload(makeUtt("PARTIAL", 1, 50, "speaker-0"))
	fin := finalPayload(makeUtt("first", nil, 100, nil), makeUtt(nil, -1, 200, "speaker-0"))
	input := response(1, false, partial) + response(3, true, fin)
	meta := testMeta()
	meta.RawEnvelope = json.RawMessage(`{"provider":"doubao","provider_request_id":"req-a","extension":{"kept":true}}`)
	r, err := Convert(meta, strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Turns) != 2 {
		t.Fatalf("expected only final two utterances, got %d", len(r.Turns))
	}
	if r.Turns[0].Text != "first" || r.Turns[0].LocalStartMS != nil || r.Turns[0].GlobalStartMS != nil || r.Turns[0].GlobalEndMS == nil || *r.Turns[0].GlobalEndMS != 300100 {
		t.Fatalf("unexpected first normalized turn: %+v", r.Turns[0])
	}
	if r.Turns[0].SpeakerScopedID != nil || r.Turns[0].SpeakerEntityID != nil || r.Turns[0].Confidence != nil {
		t.Fatalf("unknown identity/confidence invented: %+v", r.Turns[0])
	}
	if r.Turns[1].Text != "" || r.Turns[1].LocalStartMS != nil || r.Turns[1].SpeakerScopedID == nil || !strings.Contains(*r.Turns[1].SpeakerScopedID, "req-a:rev-1:speaker-0") {
		t.Fatalf("missing fields not preserved as unknown: %+v", r.Turns[1])
	}
	var raw map[string]any
	if err = json.Unmarshal(r.RawSource, &raw); err != nil {
		t.Fatal(err)
	}
	payload := raw["provider_payload"].(map[string]any)
	if payload["future_payload_field"] != "preserve" || payload["result"].(map[string]any)["future_result_field"].(float64) != 17 {
		t.Fatalf("provider unknown fields lost: %+v", payload)
	}
	finalFrame := raw["final_response_frame"].(map[string]any)
	if finalFrame["provider_frame_extension"] != "preserve-me" {
		t.Fatal("unknown final frame field lost")
	}
	var env map[string]any
	if err = json.Unmarshal(r.Source.RawEnvelope, &env); err != nil || env["extension"] == nil {
		t.Fatal("unknown metadata field lost")
	}
	if !strings.Contains(string(r.RawSource), "provider_word_field") || !strings.Contains(string(r.RawSource), "-1") {
		t.Fatal("raw word timing/evidence lost")
	}
	if r.Source.ConversationID != "conv-a" || r.Source.SupersedesRevision != "rev-0" {
		t.Fatalf("source envelope fields not propagated: %+v", r.Source)
	}
}

func TestRejectMissingFinalErrorFrameAndContradictoryFinal(t *testing.T) {
	meta := testMeta()
	partial := response(1, false, finalPayload(makeUtt("x", 0, 20, "0")))
	if _, e := Convert(meta, strings.NewReader(partial)); e == nil || !strings.Contains(e.Error(), "final package missing") {
		t.Fatalf("partial stream accepted: %v", e)
	}
	errorFrame := `{"message_type":15,"flags":0,"is_last_package":false,"code":45000000,"payload":{"error":"failed"}}` + "\n"
	if _, e := Convert(meta, strings.NewReader(errorFrame)); e == nil || !strings.Contains(e.Error(), "service error") {
		t.Fatalf("error frame accepted: %v", e)
	}
	first := response(3, true, finalPayload(makeUtt("x", 0, 20, "0")))
	second := response(3, true, finalPayload(makeUtt("y", 0, 20, "0")))
	if _, e := Convert(meta, strings.NewReader(first+second)); e == nil || !strings.Contains(e.Error(), "contradictory") {
		t.Fatalf("conflicting finals accepted: %v", e)
	}
	if _, e := Convert(meta, strings.NewReader(first+partial)); e == nil || !strings.Contains(e.Error(), "follows final") {
		t.Fatalf("partial after final accepted: %v", e)
	}
}

func TestStableIDsAreScopedByRequestRevisionAndSegmentBoundaries(t *testing.T) {
	fin := response(3, true, finalPayload(makeUtt("edge", 0, 1002, "7")))
	m := testMeta()
	r, e := Convert(m, strings.NewReader(fin))
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[0].GlobalEndMS == nil || *r.Turns[0].GlobalEndMS != 301002 {
		t.Fatalf("timestamp was not offset: %+v", r.Turns[0])
	}
	if len(r.Turns[0].Warnings) == 0 {
		t.Fatal("5ms provider rounding warning missing")
	}
	m.ProviderRequestID = "req-b"
	r2, e := Convert(m, strings.NewReader(fin))
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[0].ID == r2.Turns[0].ID {
		t.Fatal("turn IDs collide across requests")
	}
	m.ProviderRequestID = "req-a"
	m.TranscriptRevision = "rev-2"
	r3, e := Convert(m, strings.NewReader(fin))
	if e != nil {
		t.Fatal(e)
	}
	if r.Turns[0].ID == r3.Turns[0].ID {
		t.Fatal("turn IDs collide across transcript revisions")
	}
	tooFar := response(3, true, finalPayload(makeUtt("edge", 0, 1100, "7")))
	if _, e = Convert(testMeta(), strings.NewReader(tooFar)); e == nil {
		t.Fatal("100ms boundary overflow accepted")
	}
	// Zero/blank speaker labels stay null instead of being represented as an identity.
	blank := response(3, true, finalPayload(makeUtt("blank", 0, 2, "")))
	r4, e := Convert(testMeta(), strings.NewReader(blank))
	if e != nil || r4.Turns[0].SpeakerScopedID != nil {
		t.Fatalf("blank speaker should be unknown: %+v %v", r4, e)
	}
}

func TestMetadataAndFinalStructureValidation(t *testing.T) {
	m := testMeta()
	m.CompletionStatus = "partial"
	if _, e := Convert(m, strings.NewReader(response(3, true, finalPayload()))); e == nil {
		t.Fatal("non-final metadata accepted")
	}
	m = testMeta()
	bad := response(3, true, map[string]any{"result": map[string]any{"text": "nonempty but no utterance array"}})
	if _, e := Convert(m, strings.NewReader(bad)); e == nil {
		t.Fatal("nonempty final without utterances accepted")
	}
	m = testMeta()
	bad = response(3, true, finalPayload(makeUtt("x", 10, 1, "0")))
	if _, e := Convert(m, strings.NewReader(bad)); e == nil {
		t.Fatal("end before start accepted")
	}
	if _, e := Convert(m, bytes.NewBufferString("not json\n")); e == nil {
		t.Fatal("invalid response JSON accepted")
	}
}
