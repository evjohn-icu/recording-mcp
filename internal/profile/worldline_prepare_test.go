package profile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

func TestParseWorldlineFilenameStrictFormatsAndTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, want string }{
		{"20260922011540.mp3", "2026-09-22T01:15:40+08:00"},
		{"PH260922011540.wav", "2026-09-22T01:15:40+08:00"},
	} {
		got, e := ParseWorldlineFilename(tc.name, loc)
		if e != nil {
			t.Fatalf("%s: %v", tc.name, e)
		}
		if got.Format(time.RFC3339) != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, got.Format(time.RFC3339), tc.want)
		}
	}
	for _, name := range []string{"20260230011540.mp3", "2026092201154.mp3", "../20260922011540.mp3", "PH20260922011540.mp3"} {
		if _, e := ParseWorldlineFilename(name, loc); e == nil {
			t.Errorf("accepted invalid timestamp %q", name)
		}
	}
}

func TestPrepareWorldlineSetsLocalTimeAndStitchesVerifiedPCMIdempotently(t *testing.T) {
	f := newWorldlineFixture(t, false)
	ctx := context.Background()
	report, err := f.manager.PrepareWorldline(ctx, "alice", "correct horse battery staple", WorldlinePrepareOptions{LibraryDir: f.library, AudioRoot: f.originals, Timezone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Recordings != 1 || report.TimesSet != 1 || report.AudioReady != 1 || report.Issues != 0 {
		t.Fatalf("unexpected aggregate: %+v", report)
	}
	var started sql.NullString
	var known int
	if err = f.db.QueryRow(`SELECT recording_started_at,absolute_time_known FROM recordings WHERE id=?`, f.recordingID).Scan(&started, &known); err != nil {
		t.Fatal(err)
	}
	if !started.Valid || started.String != "2026-09-22T01:15:40+08:00" || known != 1 {
		t.Fatalf("recording time not set: %q known=%d", started.String, known)
	}
	var path string
	var samples int64
	if err = f.db.QueryRow(`SELECT stitched_path,total_samples FROM sync_recordings WHERE recording_id=?`, f.providerID).Scan(&path, &samples); err != nil {
		t.Fatal(err)
	}
	if samples != 16000 || !pathWithin(f.audio, path) {
		t.Fatalf("bad local PCM mapping %q samples=%d", path, samples)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1000*worldlineBytesPerMS {
		t.Fatalf("stitched PCM size=%d", len(got))
	}
	want := append(append([]byte(nil), f.pcm[:600*worldlineBytesPerMS]...), f.pcm[600*worldlineBytesPerMS:]...)
	if len(want) != len(got) {
		t.Fatal("bad test audio fixture")
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("PCM differs at byte %d", i)
		}
	}
	var saved worldlineManifest
	b, err := os.ReadFile(report.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Recordings) != 1 || saved.Recordings[0].Basis != "filename_capture_timestamp" || saved.Recordings[0].EndBasis != "original_mtime" || !saved.Recordings[0].OriginalAudioVerified {
		t.Fatalf("bad provenance manifest: %+v", saved)
	}
	second, err := f.manager.PrepareWorldline(ctx, "alice", "correct horse battery staple", WorldlinePrepareOptions{LibraryDir: f.library, AudioRoot: f.originals, Timezone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	if second.TimesSet != 0 || second.AudioReady != 1 {
		t.Fatalf("rerun was not idempotent: %+v", second)
	}
}

func TestPrepareWorldlineRejectsWrongOriginalHashBeforeMutation(t *testing.T) {
	f := newWorldlineFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.originals, "PH260922011540.mp3"), []byte("wrong synthetic bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := f.manager.PrepareWorldline(context.Background(), "alice", "correct horse battery staple", WorldlinePrepareOptions{LibraryDir: f.library, AudioRoot: f.originals})
	if err == nil {
		t.Fatal("mismatched original hash accepted")
	}
	var known int
	if err = f.db.QueryRow(`SELECT absolute_time_known FROM recordings WHERE id=?`, f.recordingID).Scan(&known); err != nil {
		t.Fatal(err)
	}
	if known != 0 {
		t.Fatal("database mutated before source hash validation")
	}
	if _, err = os.Stat(filepath.Join(f.audio, "worldline")); !os.IsNotExist(err) {
		t.Fatalf("audio output created before source validation: %v", err)
	}
}

func TestPrepareWorldlineRecordsInconsistentOriginalMtime(t *testing.T) {
	f := newWorldlineFixture(t, false)
	path := filepath.Join(f.originals, "PH260922011540.mp3")
	before := time.Date(2026, 9, 22, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if err := os.Chtimes(path, before, before); err != nil {
		t.Fatal(err)
	}
	report, err := f.manager.PrepareWorldline(context.Background(), "alice", "correct horse battery staple", WorldlinePrepareOptions{LibraryDir: f.library, AudioRoot: f.originals})
	if err != nil {
		t.Fatal(err)
	}
	var saved worldlineManifest
	b, err := os.ReadFile(report.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	issues := map[string]bool{}
	for _, issue := range saved.Recordings[0].Issues {
		issues[issue] = true
	}
	if saved.Recordings[0].EndBasis != "media_duration_invalid_mtime" || saved.Recordings[0].OriginalMtime == "" || saved.Recordings[0].EndAt <= saved.Recordings[0].StartedAt || !issues["original_mtime_precedes_capture_start"] || !issues["original_mtime_differs_from_media_duration"] {
		t.Fatalf("inconsistent original mtime not disclosed: %+v", saved.Recordings[0])
	}
}

func TestPrepareWorldlineLeavesAudioUnavailableAcrossGap(t *testing.T) {
	f := newWorldlineFixture(t, true)
	report, err := f.manager.PrepareWorldline(context.Background(), "alice", "correct horse battery staple", WorldlinePrepareOptions{LibraryDir: f.library})
	if err != nil {
		t.Fatal(err)
	}
	if report.AudioReady != 0 || report.Unavailable != 1 || report.Issues == 0 {
		t.Fatalf("gap was not reported unavailable: %+v", report)
	}
	var path sql.NullString
	if err = f.db.QueryRow(`SELECT stitched_path FROM sync_recordings WHERE recording_id=?`, f.providerID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path.Valid {
		t.Fatalf("audio mapping fabricated across gap: %q", path.String)
	}
	var saved worldlineManifest
	b, err := os.ReadFile(report.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Recordings[0].EndBasis != "media_duration" || saved.Recordings[0].OriginalAudioVerified {
		t.Fatalf("unexpected unavailable-source provenance: %+v", saved.Recordings[0])
	}
}

func TestPrepareWorldlineDoesNotRecreateAudioForForgetTombstone(t *testing.T) {
	for _, scope := range []string{"turn", "recording", "conversation"} {
		t.Run(scope, func(t *testing.T) {
			f := newWorldlineFixture(t, false)
			conversationID := ""
			recordingID := ""
			if scope == "conversation" {
				if err := f.db.QueryRow(`SELECT conversation_id FROM recordings WHERE id=?`, f.recordingID).Scan(&conversationID); err != nil {
					t.Fatal(err)
				}
			} else {
				recordingID = f.recordingID
			}
			if _, err := f.db.Exec(`INSERT INTO forget_tombstones(id,scope,conversation_id,recording_id,requested_id,locator_key,expected_state_hash,status,created_at) VALUES('tombstone-test',?,?,?,?,?,'state','pending_external_cleanup',1)`, scope, conversationID, recordingID, "target", "locator-test"); err != nil {
				t.Fatal(err)
			}
			report, err := f.manager.PrepareWorldline(context.Background(), "alice", "correct horse battery staple", WorldlinePrepareOptions{LibraryDir: f.library})
			if err != nil {
				t.Fatal(err)
			}
			if report.AudioReady != 0 || report.TimesSet != 0 || report.Issues == 0 || report.Entries[0].AudioStatus != "fenced" {
				t.Fatalf("forgotten source was not skipped: %+v", report)
			}
			var known int
			if err = f.db.QueryRow(`SELECT absolute_time_known FROM recordings WHERE id=?`, f.recordingID).Scan(&known); err != nil {
				t.Fatal(err)
			}
			if known != 0 {
				t.Fatal("forgotten recording time was republished")
			}
			if _, err = os.Stat(filepath.Join(f.audio, "worldline")); !os.IsNotExist(err) {
				t.Fatalf("audio output recreated for forgotten recording: %v", err)
			}
		})
	}
}

type worldlineFixture struct {
	manager                   *Manager
	db                        *sql.DB
	recordingID, providerID   string
	library, originals, audio string
	pcm                       []byte
}

func newWorldlineFixture(t *testing.T, gap bool) worldlineFixture {
	t.Helper()
	root := t.TempDir()
	m, err := Open(filepath.Join(root, "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := m.Create("alice", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	start, end := int64(0), int64(1000)
	turn := asrinput.Turn{ID: "turn-1", OriginalIndex: 1, Text: "synthetic fixture only", LocalStartMS: &start, LocalEndMS: &end, GlobalStartMS: &start, GlobalEndMS: &end}
	pcm := make([]byte, 1000*worldlineBytesPerMS)
	for i := range pcm {
		pcm[i] = byte((i*37 + 11) % 251)
	}
	audio := []byte("synthetic original audio bytes")
	sum := sha256.Sum256(audio)
	audioHash := hex.EncodeToString(sum[:])
	started := (*string)(nil)
	result := asrinput.Result{ContractVersion: asrinput.ContractVersion, Source: asrinput.Source{Provider: "synthetic", ProviderRequestID: "request-1", DeviceID: "device:worldline-test", RecordingID: "recording:20260922011540", ConversationID: "conversation:worldline-test", SegmentID: "segment-1", SegmentOffsetMS: 0, SegmentDurationMS: 1000, TranscriptRevision: "rev-1", CompletionStatus: "final", RecordingStartedAt: started, SourceAudioSHA256: &audioHash, SourceRef: strPtr("synthetic-source")}, Turns: []asrinput.Turn{turn}}
	report, err := store.IngestTranscript(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	library := filepath.Join(root, "library")
	runDir := filepath.Join(library, "sample-run")
	segmentsDir := filepath.Join(runDir, "segments")
	if err = os.MkdirAll(segmentsDir, 0700); err != nil {
		t.Fatal(err)
	}
	seg2Start := int64(500)
	if gap {
		seg2Start = 700
	}
	pcm1 := pcm[:600*worldlineBytesPerMS]
	pcm2 := pcm[seg2Start*worldlineBytesPerMS:]
	for name, data := range map[string][]byte{"segment-01.pcm": pcm1, "segment-02.pcm": pcm2} {
		if err = os.WriteFile(filepath.Join(segmentsDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	segs := []runSegment{{Index: 1, StartMS: 0, EndMS: 600, PCM: "segment-01.pcm"}, {Index: 2, StartMS: seg2Start, EndMS: 1000, PCM: "segment-02.pcm"}}
	run := runManifest{AudioFileName: "PH260922011540.mp3", AudioSHA256: audioHash, Duration: 1, Segments: segs}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(runDir, "run.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	originals := filepath.Join(root, "originals")
	if err = os.MkdirAll(originals, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(originals, "PH260922011540.mp3"), audio, 0600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 9, 22, 1, 15, 41, 0, time.FixedZone("CST", 8*3600))
	if err = os.Chtimes(filepath.Join(originals, "PH260922011540.mp3"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	return worldlineFixture{manager: m, db: db, recordingID: report.RecordingID, providerID: "recording:20260922011540", library: library, originals: originals, audio: paths.Audio, pcm: pcm}
}

func strPtr(s string) *string { return &s }
