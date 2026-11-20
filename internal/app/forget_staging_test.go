package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/retention"
)

// The staging cleanup tests pin the forget contract for analysis staging:
// CleanupForget deletes <audioRoot>/.analysis-tmp/<sha256(recordingID)> for
// exactly the recordings persisted with the tombstone, keeps every other
// recording's staging, keeps pending on deletion failure and never deletes
// staging for a legacy tombstone whose list is empty. The path layout is the
// documented retention.AnalysisStagingRel contract.
func appStagingRel(recordingID string) string {
	return retention.AnalysisStagingRel(recordingID)
}

// stagingForgetFixture is one conversation with two recordings that were
// ingested through the real transcript path.
type stagingForgetFixture struct {
	t            *testing.T
	store        *memory.Store
	svc          *Service
	audioDir     string
	conversation string
	target       string
	sibling      string
}

func newStagingForgetFixture(t *testing.T) *stagingForgetFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &stagingForgetFixture{t: t, store: store, svc: New(store, nil, nil, audioDir), audioDir: audioDir}
	ctx := context.Background()
	for _, segment := range []string{"seg-a", "seg-b"} {
		tr := sampleSegment(segment, 0, nil, []asrinput.Turn{
			turnWithTimes("t-1", 1, "第一条原始转写", 0, 1000, 2000),
		})
		tr.Source.RecordingID = "prov-" + segment
		rep, err := store.IngestTranscript(ctx, tr)
		if err != nil {
			t.Fatalf("ingest %s: %v", segment, err)
		}
		if segment == "seg-a" {
			f.target = rep.RecordingID
		} else {
			f.sibling = rep.RecordingID
		}
		f.conversation = rep.ConversationID
	}
	if f.target == "" || f.sibling == "" || f.target == f.sibling {
		t.Fatalf("fixture recordings: target=%q sibling=%q", f.target, f.sibling)
	}
	return f
}

// makeForgetStagingDir creates the per-recording staging directory of one
// recording as a real adapter run would leave it.
func (f *stagingForgetFixture) makeForgetStagingDir(recordingID string) string {
	f.t.Helper()
	dir := filepath.Join(f.audioDir, appStagingRel(recordingID), "run-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "partial.bin"), []byte("partial"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *stagingForgetFixture) forgetRecording() string {
	f.t.Helper()
	ctx := context.Background()
	req := memory.ForgetRequest{Scope: "recording", RecordingID: f.target}
	preview, err := f.svc.PreviewForget(ctx, req)
	if err != nil {
		f.t.Fatalf("preview forget: %v", err)
	}
	req.ExpectedState = preview.ExpectedState
	op, err := f.svc.CommitForget(ctx, req)
	if err != nil {
		f.t.Fatalf("commit forget: %v", err)
	}
	if op.TombstoneID == "" {
		f.t.Fatalf("forget operation without tombstone id: %+v", op)
	}
	return op.TombstoneID
}

func TestForgetRecordingCleanupDeletesTargetStagingKeepsSibling(t *testing.T) {
	f := newStagingForgetFixture(t)
	targetDir := f.makeForgetStagingDir(f.target)
	siblingDir := f.makeForgetStagingDir(f.sibling)

	tombstoneID := f.forgetRecording()

	if _, err := os.Stat(targetDir); !os.IsNotExist(err) {
		t.Fatalf("forgotten recording staging survived cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(siblingDir, "partial.bin")); err != nil {
		t.Fatalf("sibling staging was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.audioDir, ".analysis-tmp")); err != nil {
		t.Fatalf("staging root was removed although a sibling remains: %v", err)
	}
	var recordings int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM recordings WHERE conversation_id=?`, f.conversation).Scan(&recordings); err != nil || recordings != 1 {
		t.Fatalf("sibling recordings = %d err=%v, want 1", recordings, err)
	}

	// The completed forget accepts a repeated cleanup without touching the
	// sibling staging again.
	if _, err := f.svc.RetryForget(context.Background(), tombstoneID); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(siblingDir, "partial.bin")); err != nil {
		t.Fatalf("sibling staging lost on retry: %v", err)
	}
}

func TestForgetCleanupStagingFailureKeepsPendingAndRetries(t *testing.T) {
	f := newStagingForgetFixture(t)
	f.makeForgetStagingDir(f.target)

	// A read-only staging root makes the targeted deletion fail; the forget
	// must stay pending instead of claiming completion.
	if err := os.Chmod(filepath.Join(f.audioDir, ".analysis-tmp"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(f.audioDir, ".analysis-tmp"), 0o700) })

	req := memory.ForgetRequest{Scope: "recording", RecordingID: f.target}
	preview, err := f.svc.PreviewForget(context.Background(), req)
	if err != nil {
		t.Fatalf("preview forget: %v", err)
	}
	req.ExpectedState = preview.ExpectedState
	op, err := f.svc.CommitForget(context.Background(), req)
	if err != nil {
		t.Fatalf("commit forget: %v", err)
	}
	if !op.CleanupPending {
		t.Fatalf("cleanup failure was not reported pending: %+v", op)
	}
	// The failed cleanup may have cleared writable run subdirectories but the
	// per-recording staging root survives; the retry finishes the deletion.
	stagingRoot := filepath.Join(f.audioDir, appStagingRel(f.target))
	if _, err := os.Stat(stagingRoot); err != nil {
		t.Fatalf("staging root disappeared while cleanup failed: %v", err)
	}

	if err := os.Chmod(filepath.Join(f.audioDir, ".analysis-tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RetryForget(context.Background(), op.TombstoneID); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := os.Stat(stagingRoot); !os.IsNotExist(err) {
		t.Fatalf("staging root survived the successful retry: %v", err)
	}
}

func TestForgetCleanupKeepsStagingForLegacyTombstoneWithoutRecordingList(t *testing.T) {
	f := newStagingForgetFixture(t)
	targetDir := f.makeForgetStagingDir(f.target)

	tombstoneID := f.forgetRecording()

	// A tombstone created before the staging manifest existed has an empty
	// recording list; its leftover staging must never be deleted by guesses.
	if _, err := f.store.DB().Exec(`UPDATE forget_tombstones SET affected_recordings_json='[]' WHERE id=?`, tombstoneID); err != nil {
		t.Fatalf("legacy affected_recordings_json column unavailable: %v", err)
	}
	targetDir = f.makeForgetStagingDir(f.target)
	if err := f.svc.CleanupForget(context.Background(), tombstoneID); err != nil {
		t.Fatalf("legacy cleanup: %v", err)
	}
	if _, err := os.Stat(targetDir); err != nil {
		t.Fatalf("legacy tombstone staging was deleted: %v", err)
	}
}
