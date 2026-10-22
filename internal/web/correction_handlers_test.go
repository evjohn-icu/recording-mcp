package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"recordingmcp/internal/asrinput"
)

func TestTranscriptSourceAndCorrectionRoutes(t *testing.T) {
	env := newTestEnv(t, "")
	start, end := int64(10), int64(900)
	spk := "speaker:one"
	result := asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source:          asrinput.Source{Provider: "test", ProviderRequestID: "req-http", DeviceID: "dev-http", RecordingID: "rec-http", ConversationID: "conv-http", SegmentID: "seg-http", SegmentDurationMS: 1000, TranscriptRevision: "rev-1", CompletionStatus: "final"},
		Turns:           []asrinput.Turn{{ID: "u1", OriginalIndex: 1, Text: "原始转写", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end, SpeakerScopedID: &spk}},
	}
	report, err := env.store.IngestTranscript(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	var turnID string
	if err := env.store.DB().QueryRow(`SELECT id FROM turns WHERE conversation_id=? AND original_turn_id='seg-http/rev-1/u1'`, report.ConversationID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	status, body := env.do(t, http.MethodGet, "/api/turns/"+turnID+"/transcript-source", "", nil)
	if status != http.StatusOK {
		t.Fatalf("source lookup status=%d body=%s", status, body)
	}
	var source struct {
		SegmentID string          `json:"segment_id"`
		Result    asrinput.Result `json:"result"`
	}
	if err := json.Unmarshal(body, &source); err != nil || source.SegmentID != "seg-http" || source.Result.Source.TranscriptRevision != "rev-1" {
		t.Fatalf("source lookup=%+v err=%v", source, err)
	}
	path := fmt.Sprintf("/api/conversations/%s/recordings/%s/segments/seg-http/correction", report.ConversationID, report.RecordingID)
	payload := `{"expected_revision":"rev-1","new_revision":"rev-2","texts":{"u1":"更正后的转写"}}`
	status, body = env.do(t, http.MethodPost, path, payload, nil)
	if status != http.StatusOK {
		t.Fatalf("correction status=%d body=%s", status, body)
	}
	status, body = env.do(t, http.MethodPost, path, payload, nil)
	if status != http.StatusConflict {
		t.Fatalf("stale correction status=%d body=%s", status, body)
	}
}
