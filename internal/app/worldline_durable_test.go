package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/process"
)

type durableWorldlineEngine struct {
	calls          int
	failAt         int
	model, weights string
	afterEvaluate  func()
}

func (e *durableWorldlineEngine) ModelIdentity(context.Context) (string, string, error) {
	return e.model, e.weights, nil
}
func (e *durableWorldlineEngine) Evaluate(_ context.Context, state string, qs []decision.Question) (decision.Result, error) {
	e.calls++
	if e.failAt > 0 && e.calls == e.failAt {
		return decision.Result{}, errors.New("transient test failure")
	}
	answers := map[string]decision.Answer{}
	for _, q := range qs {
		answers[q.ID] = decision.Answer{Type: decision.TypeChoice, Choice: "insufficient_evidence", Confidence: .8, Distribution: map[string]float64{"text_similarity": .05, "complementary_context": .05, "review_required": .1, "unrelated": .1, "insufficient_evidence": .7}}
	}
	if e.afterEvaluate != nil {
		e.afterEvaluate()
	}
	return decision.Result{Model: e.model, WeightsVersion: e.weights, InputHash: fmt.Sprintf("%064x", e.calls), Answers: answers}, nil
}

func durableWorldlineFixture(t *testing.T, engine decision.Engine) (*WorldlineService, string, string) {
	t.Helper()
	f := newFixture(t)
	events, err := plane.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { events.Close() })
	var turn, revision string
	err = f.store.DB().QueryRow(`SELECT t.id,a.transcript_revision FROM turns t JOIN note_evidence ne ON ne.turn_id=t.id JOIN notes n ON n.id=ne.note_id JOIN attachment_turns at ON at.turn_id=t.id JOIN source_attachments a ON a.id=at.attachment_id WHERE t.conversation_id=? AND a.active=1 LIMIT 1`, f.conversationID).Scan(&turn, &revision)
	if err != nil {
		t.Fatal(err)
	}
	return &WorldlineService{Time: &TimeService{Store: f.store, Events: events}, DecisionEngine: engine}, turn, revision
}

func worldlineNoteVersions(t *testing.T, svc *WorldlineService) []string {
	t.Helper()
	topics, _, _, err := svc.project(context.Background(), "profile-test")
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, topic := range topics {
		for _, lane := range topic.Lanes {
			for _, claim := range lane.Claims {
				out = append(out, claimSourceNoteVersions(claim)...)
			}
		}
	}
	sort.Strings(out)
	if len(out) < 2 {
		t.Fatalf("fixture needs two source notes, found %v", out)
	}
	return out
}

func testSemanticPairs(turn, revision string, n int) []semanticPair {
	out := make([]semanticPair, n)
	for i := range out {
		ref := EvidenceReference{TurnID: turn, Revision: revision}
		out[i] = semanticPair{ID: fmt.Sprintf("pair-%04d", i), Title: "test topic", IdentityStatus: "unconfirmed_identity_text_only", Earlier: semanticPairEvidence{EntryID: fmt.Sprintf("earlier-%d", i), ClaimID: fmt.Sprintf("claim-a-%d", i), Summary: "earlier", SourceRefs: []EvidenceReference{ref}}, Later: semanticPairEvidence{EntryID: fmt.Sprintf("later-%d", i), ClaimID: fmt.Sprintf("claim-b-%d", i), Summary: fmt.Sprintf("later %d", i), SourceRefs: []EvidenceReference{ref}}}
	}
	return out
}

func TestDurableWorldlinePairBatchesResumeAfterRestart(t *testing.T) {
	ctx := context.Background()
	first := &durableWorldlineEngine{model: "test-model", weights: "weights-1", failAt: 130}
	svc, turn, rev := durableWorldlineFixture(t, first)
	pairs := testSemanticPairs(turn, rev, 300)
	if _, err := svc.runDurableWorldlinePairs(ctx, "profile-test", "job-1", 1, pairs); err == nil {
		t.Fatal("expected injected failure")
	}
	restarted := &durableWorldlineEngine{model: "test-model", weights: "weights-1"}
	svc.DecisionEngine = restarted
	summary, err := svc.runDurableWorldlinePairs(ctx, "profile-test", "job-1", 2, pairs)
	if err != nil {
		t.Fatal(err)
	}
	if summary.PairCount != 300 || summary.CompletedPairCount != 300 || summary.RemainingPairCount != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	if first.calls+restarted.calls != 301 {
		t.Fatalf("restart recomputed completed pairs: initial=%d resumed=%d", first.calls, restarted.calls)
	}
}

func TestDurableWorldlineCacheInvalidatesOnlyChangedPairAndModel(t *testing.T) {
	ctx := context.Background()
	engine := &durableWorldlineEngine{model: "model-a", weights: "weights-a"}
	svc, turn, rev := durableWorldlineFixture(t, engine)
	pairs := testSemanticPairs(turn, rev, 3)
	versions := worldlineNoteVersions(t, svc)
	pairs[0].Earlier.SourceNoteVersions = []string{versions[0]}
	pairs[1].Earlier.SourceNoteVersions = []string{versions[1]}
	if _, err := svc.runDurableWorldlinePairs(ctx, "profile-test", "job-1", 1, pairs); err != nil {
		t.Fatal(err)
	}
	engine.calls = 0
	noteID := strings.SplitN(versions[0], "@", 2)[0]
	if _, err := svc.Time.Store.DB().Exec(`UPDATE notes SET summary=summary||' edited',annotation_version='v2' WHERE id=?`, noteID); err != nil {
		t.Fatal(err)
	}
	updatedVersions := worldlineNoteVersions(t, svc)
	for _, value := range updatedVersions {
		if strings.HasPrefix(value, noteID+"@") {
			pairs[0].Earlier.SourceNoteVersions = []string{value}
			break
		}
	}
	if _, err := svc.runDurableWorldlinePairs(ctx, "profile-test", "job-2", 1, pairs); err != nil {
		t.Fatal(err)
	}
	if engine.calls != 1 {
		t.Fatalf("unrelated pair results were not reused selectively; calls=%d", engine.calls)
	}
	engine.calls = 0
	engine.model, engine.weights = "model-b", "weights-b"
	if _, err := svc.runDurableWorldlinePairs(ctx, "profile-test", "job-3", 1, pairs); err != nil {
		t.Fatal(err)
	}
	if engine.calls != 3 {
		t.Fatalf("model identity change should miss all pair caches, calls=%d", engine.calls)
	}
}

func TestDurableWorldlineCacheKeyInvalidatesRubric(t *testing.T) {
	pair := testSemanticPairs("synthetic-turn", "rev-1", 1)[0]
	key := worldlinePairCacheKey(pair, "model-a", "weights-a")
	if key != worldlinePairCacheKeyForRubric(pair, "model-a", "weights-a", worldlineSemanticRubricVersion) {
		t.Fatal("production cache key did not use the active rubric version")
	}
	if key == worldlinePairCacheKeyForRubric(pair, "model-a", "weights-a", worldlineSemanticRubricVersion+"-next") {
		t.Fatal("rubric version change reused a durable pair cache key")
	}
}

func TestDurableWorldlineStaleSourceCannotPublish(t *testing.T) {
	ctx := context.Background()
	engine := &durableWorldlineEngine{model: "test-model", weights: "weights-1"}
	svc, turn, rev := durableWorldlineFixture(t, engine)
	pairs := testSemanticPairs(turn, rev, 1)
	if _, err := svc.Time.Store.DB().Exec(`UPDATE source_attachments SET active=0 WHERE id IN (SELECT attachment_id FROM attachment_turns WHERE turn_id=?)`, turn); err != nil {
		t.Fatal(err)
	}
	summary, err := svc.runDurableWorldlinePairs(ctx, "profile-test", "job-stale", 1, pairs)
	if err != nil {
		t.Fatal(err)
	}
	if engine.calls != 0 || summary.FailedPairCount != 1 {
		t.Fatalf("stale source processed: calls=%d summary=%+v", engine.calls, summary)
	}
	events, err := svc.Time.History(ctx, "profile-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "worldline.semantic" {
			t.Fatalf("stale suggestion published: %+v", event)
		}
	}
}

func TestWorldlineAnalysisJobConsumesFrozenPairsAndReportsProgress(t *testing.T) {
	ctx := context.Background()
	engine := &durableWorldlineEngine{model: "test-model", weights: "weights-1"}
	svc, turn, rev := durableWorldlineFixture(t, engine)
	pairs := testSemanticPairs(turn, rev, 3)
	const scope, request = "profile-test", "frozen-request"
	key := "worldline|" + scope + "|" + request
	payload, _ := json.Marshal(map[string]any{"scope": scope, "request_id": request, "source_snapshot_version": "snapshot-v1", "pairs": pairs, "references": []EvidenceReference{{TurnID: turn, Revision: rev}}, "source_note_versions": map[string]string{}, "source_conversation_ids": []string{}})
	jobID, _, err := process.Enqueue(ctx, svc.Time.Store.DB(), process.Spec{Stage: worldlineJobStage, InputSnapshotID: "snapshot-v1", IdempotencyKey: key, PayloadJSON: string(payload), MaxAttempts: 5}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := process.Claim(ctx, svc.Time.Store.DB(), process.ClaimOptions{WorkerID: "test-worker", Stages: []string{worldlineJobStage}, Lease: time.Minute, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != jobID {
		t.Fatalf("claimed %q want %q", claimed.ID, jobID)
	}
	result, err := svc.RunAnalysisJob(ctx, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	if result["semantic_pair_count"] != 3 || result["completed_pair_count"] != 3 {
		t.Fatalf("worker result=%v", result)
	}
	raw, _ := json.Marshal(result)
	if err := process.Complete(ctx, svc.Time.Store.DB(), jobID, claimed.LeaseToken, string(raw), time.Now()); err != nil {
		t.Fatal(err)
	}
	status, err := svc.AnalysisStatus(ctx, scope, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status.CompletedPairCount != 3 || status.RemainingPairCount != 0 {
		t.Fatalf("durable progress=%+v", status)
	}
}

func TestWorldlineEmptyFrozenSnapshotCompletesNoCandidates(t *testing.T) {
	ctx := context.Background()
	engine := &durableWorldlineEngine{model: "model", weights: "weights"}
	svc, _, _ := durableWorldlineFixture(t, engine)
	const scope, request = "profile-test", "empty"
	payload, _ := json.Marshal(map[string]any{"scope": scope, "request_id": request, "source_snapshot_version": "snapshot-empty", "pairs": []semanticPair{}})
	jobID, _, err := process.Enqueue(ctx, svc.Time.Store.DB(), process.Spec{Stage: worldlineJobStage, InputSnapshotID: "snapshot-empty", IdempotencyKey: "worldline|" + scope + "|" + request, PayloadJSON: string(payload)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := process.Claim(ctx, svc.Time.Store.DB(), process.ClaimOptions{WorkerID: "test-worker", Stages: []string{worldlineJobStage}, Lease: time.Minute, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.RunAnalysisJob(ctx, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	if result["semantic_status"] != "no_candidates" || engine.calls != 0 {
		t.Fatalf("empty result=%v calls=%d", result, engine.calls)
	}
	raw, _ := json.Marshal(result)
	if err := process.Complete(ctx, svc.Time.Store.DB(), jobID, claimed.LeaseToken, string(raw), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledWorldlineLeaseCannotPublishPair(t *testing.T) {
	ctx := context.Background()
	engine := &durableWorldlineEngine{model: "test-model", weights: "weights-1"}
	svc, turn, rev := durableWorldlineFixture(t, engine)
	pair := testSemanticPairs(turn, rev, 1)[0]
	scope, request := "profile-test", "cancelled"
	payload, _ := json.Marshal(map[string]any{"scope": scope, "request_id": request, "source_snapshot_version": "snapshot-cancel", "pairs": []semanticPair{pair}})
	jobID, _, err := process.Enqueue(ctx, svc.Time.Store.DB(), process.Spec{Stage: worldlineJobStage, InputSnapshotID: "snapshot-cancel", IdempotencyKey: "worldline|" + scope + "|" + request, PayloadJSON: string(payload)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := process.Claim(ctx, svc.Time.Store.DB(), process.ClaimOptions{WorkerID: "test-worker", Stages: []string{worldlineJobStage}, Lease: time.Minute, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	engine.afterEvaluate = func() {
		if _, e := svc.Time.Store.DB().Exec(`UPDATE processing_jobs SET status='cancelled',lease_token=NULL,lease_expires_at=NULL WHERE id=?`, jobID); e != nil {
			t.Error(e)
		}
	}
	if _, err := svc.RunAnalysisJob(ctx, scope, request); !errors.Is(err, process.ErrLeaseLost) {
		t.Fatalf("cancelled lease error=%v", err)
	}
	events, err := svc.Time.History(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "worldline.semantic" || event.Type == "worldline.semantic_cache" {
			t.Fatalf("cancelled job published a pair result: %+v", event)
		}
	}
	_ = claimed
}
