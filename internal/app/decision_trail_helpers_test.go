package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
)

// decisionEnv is one fixture with its own event plane and decision service.
type decisionEnv struct {
	*fixture
	events  *plane.Service
	wl      *WorldlineService
	ds      *DecisionTrailService
	scope   string
	annVer  string
	annSeq  int
	noteMap map[string]memory.NoteAnnotation
}

func newDecisionEnv(t *testing.T) *decisionEnv {
	t.Helper()
	f := newFixture(t)
	events, err := plane.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { events.Close() })
	timeSvc := &TimeService{Store: f.store, Events: events}
	wl := &WorldlineService{Time: timeSvc}
	return &decisionEnv{
		fixture: f, events: events, wl: wl,
		ds:     &DecisionTrailService{Worldline: wl, Time: timeSvc},
		scope:  "profile-decision",
		annVer: "v1",
		noteMap: map[string]memory.NoteAnnotation{
			"note:one": {ID: "note:one", Kind: "suggestion", TopicIDs: []string{"topic:a"}, Summary: "第一条归纳", EvidenceTurnIDs: []string{originalTurnID("seg-a", "t-1")}},
			"note:two": {ID: "note:two", Kind: "intention", TopicIDs: []string{"topic:a"}, Summary: "第二条归纳", EvidenceTurnIDs: []string{originalTurnID("seg-a", "t-2")}},
		},
	}
}

// setNote rewrites a note's kind and summary by applying a new annotation
// version, which is the only supported way to change a logical note.
func (e *decisionEnv) setNote(t *testing.T, noteID, kind, summary string) {
	t.Helper()
	note := e.noteMap[noteID]
	note.ID, note.Kind, note.Summary, note.TopicIDs = noteID, kind, summary, []string{"topic:a"}
	e.noteMap[noteID] = note
	e.applyNotes(t)
}

// removeNote drops a note so only one candidate remains.
func (e *decisionEnv) removeNote(t *testing.T, noteID string) {
	t.Helper()
	delete(e.noteMap, noteID)
	e.applyNotes(t)
}

func (e *decisionEnv) applyNotes(t *testing.T) {
	t.Helper()
	e.annSeq++
	e.annVer = "v" + strconv.Itoa(e.annSeq+1)
	ann := memory.Annotations{
		Version: e.annSeq + 1, ConversationID: e.conversationID, Title: "测试会话", RecordingID: e.recordingID,
		AnnotationVersion: e.annVer, AnnotationBasis: "人工解释；未核实", Summary: "修订后的归纳",
		Topics: []memory.TopicAnnotation{{ID: "topic:a", Title: "主题A"}},
	}
	// Keep a deterministic note order so repeated calls are stable.
	for _, id := range []string{"note:one", "note:two"} {
		if note, ok := e.noteMap[id]; ok {
			ann.Notes = append(ann.Notes, note)
		}
	}
	if err := e.store.ApplyAnnotations(context.Background(), e.conversationID, "v"+strconv.Itoa(e.annSeq), ann); err != nil {
		t.Fatal(err)
	}
}

func (e *decisionEnv) build(t *testing.T) DecisionTrail {
	t.Helper()
	trail, err := e.ds.Build(context.Background(), e.scope, DecisionQuery{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	return trail
}

func (e *decisionEnv) topic(t *testing.T) DecisionTopic {
	t.Helper()
	trail := e.build(t)
	if len(trail.Topics) != 1 {
		t.Fatalf("topics=%d want 1: %+v", len(trail.Topics), trail.Topics)
	}
	return trail.Topics[0]
}

// candidateID finds the candidate whose text contains the marker.
func candidateID(t *testing.T, topic DecisionTopic, marker string) string {
	t.Helper()
	for _, record := range topic.History {
		if record.State == DecisionCandidate && strings.Contains(record.Text, marker) {
			return record.CandidateID
		}
	}
	for _, record := range topic.Unresolved {
		if strings.Contains(record.Text, marker) {
			return record.CandidateID
		}
	}
	t.Fatalf("candidate %q not found in %+v", marker, topic.History)
	return ""
}

func (e *decisionEnv) review(t *testing.T, req DecisionReview) DecisionTrail {
	t.Helper()
	if req.Reviewer == "" {
		req.Reviewer = "reviewer-1"
	}
	trail, err := e.ds.Review(context.Background(), e.scope, req)
	if err != nil {
		t.Fatalf("review %+v: %v", req, err)
	}
	return trail
}

func (e *decisionEnv) reviewErr(t *testing.T, req DecisionReview) error {
	t.Helper()
	_, err := e.ds.Review(context.Background(), e.scope, req)
	return err
}

func (e *decisionEnv) eventCount(t *testing.T, eventType string) int {
	t.Helper()
	events, err := e.timeHistory(t)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func (e *decisionEnv) timeHistory(t *testing.T) ([]plane.Event, error) {
	t.Helper()
	return e.ds.Time.History(context.Background(), e.scope)
}

// supersedeSegment republishes segment seg-a under rev-2, superseding rev-1.
func (e *decisionEnv) supersedeSegment(t *testing.T) {
	t.Helper()
	segment := sampleSegment("seg-a", 0, nil, []asrinput.Turn{
		turnWithTimes("t-1", 1, "第一条原始转写", 0, 1000, 2000),
		turnWithTimes("t-2", 2, "第二条原始转写", 0, 2000, 3000),
	})
	segment.Source.TranscriptRevision = "rev-2"
	segment.Source.SupersedesRevision = "rev-1"
	segment.Source.ProviderRequestID = "req-seg-a-rev2"
	if _, err := e.store.IngestTranscript(context.Background(), segment); err != nil {
		t.Fatal(err)
	}
}

// turnIDFor resolves the stored turn row for one segment/revision/original id.
func (e *decisionEnv) turnIDFor(t *testing.T, originalSegment, revision, turn string) string {
	t.Helper()
	var id string
	if err := e.store.DB().QueryRow(`SELECT id FROM turns WHERE original_turn_id=?`,
		originalSegment+"/"+revision+"/"+turn).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// reapplyNotesVersion rewrites the notes against evidence from a newer
// transcript revision. originalTurnIDs are given as segment/turn pairs that are
// expanded with the target revision.
func (e *decisionEnv) reapplyNotesVersion(t *testing.T, revision string, notes map[string]string, evidence map[string]string) {
	t.Helper()
	e.annSeq++
	expected := e.annVer
	e.annVer = "v" + strconv.Itoa(e.annSeq+1)
	ann := memory.Annotations{
		Version: e.annSeq + 1, ConversationID: e.conversationID, Title: "测试会话", RecordingID: e.recordingID,
		AnnotationVersion: e.annVer, AnnotationBasis: "人工解释；未核实", Summary: "修订后的归纳",
		Topics: []memory.TopicAnnotation{{ID: "topic:a", Title: "主题A"}},
	}
	for _, noteID := range []string{"note:one", "note:two"} {
		summary, ok := notes[noteID]
		if !ok {
			continue
		}
		original := strings.Replace(evidence[noteID], "/rev-1/", "/"+revision+"/", 1)
		ann.Notes = append(ann.Notes, memory.NoteAnnotation{
			ID: noteID, Kind: "suggestion", TopicIDs: []string{"topic:a"}, Summary: summary,
			EvidenceTurnIDs: []string{original},
		})
	}
	if err := e.store.ApplyAnnotations(context.Background(), e.conversationID, expected, ann); err != nil {
		t.Fatal(err)
	}
}

// appendReviewEvent writes a raw decision.review event, used by tests that need
// a shape the web API refuses to create (for example missing evidence).
func (e *decisionEnv) appendReviewEvent(t *testing.T, topicID, candidateID, action, requestID string, refs []string, recordedAt time.Time) string {
	t.Helper()
	event := plane.Event{
		ID: "decision-review-" + shortHash(e.scope+"\x00"+requestID), Type: "decision.review",
		Timestamp: recordedAt, ObservedAt: recordedAt, Source: "local-web", Content: "raw review", Confidence: 0,
		StructuredData: map[string]any{
			"scope": e.scope, "origin": "decision-trail", "topic_id": topicID, "candidate_id": candidateID,
			"action": action, "text": "raw review", "reviewer": "raw", "evidence": refs,
			"request_id": requestID, "review_payload_hash": "raw",
		},
	}
	payload := map[string]any{"id": event.ID, "type": event.Type, "topic_id": topicID,
		"candidate_id": candidateID, "action": action, "evidence": refs, "request_id": requestID}
	raw, _ := json.Marshal(payload)
	event.StructuredData["payload_json"] = string(raw)
	if _, err := e.events.Ingest(context.Background(), []plane.Event{event}); err != nil {
		t.Fatal(err)
	}
	return event.ID
}

// applyCorrectedNote replaces the annotation set with one corrected note whose
// evidence points at the given revision. A corrected transcript produces a new
// note id because the old note is permanently marked stale.
func (e *decisionEnv) applyCorrectedNote(t *testing.T, revision, noteID, summary string) {
	t.Helper()
	e.annSeq++
	expected := e.annVer
	e.annVer = "v" + strconv.Itoa(e.annSeq+1)
	turnRef := strings.Replace(originalTurnID("seg-a", "t-1"), "/rev-1/", "/"+revision+"/", 1)
	ann := memory.Annotations{
		Version: e.annSeq + 1, ConversationID: e.conversationID, Title: "测试会话", RecordingID: e.recordingID,
		AnnotationVersion: e.annVer, AnnotationBasis: "人工修订；未核实", Summary: "修订后的归纳",
		Topics: []memory.TopicAnnotation{{ID: "topic:a", Title: "主题A"}},
		Notes: []memory.NoteAnnotation{{
			ID: noteID, Kind: "suggestion", TopicIDs: []string{"topic:a"}, Summary: summary,
			EvidenceTurnIDs: []string{turnRef},
		}},
	}
	if err := e.store.ApplyAnnotations(context.Background(), e.conversationID, expected, ann); err != nil {
		t.Fatal(err)
	}
}
