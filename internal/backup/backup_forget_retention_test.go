package backup

// Tests for plan step 3.4: retained chunks of a completed turn forget (paths
// and lengths kept, pre-redaction hashes erased) must be archivable, verifiable
// and restorable when the tombstone is complete and the provider fence exists,
// and restore must reject any broken stored audio reference before publishing
// the destination.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
)

const forgetRetentionTombstone = "forget:turn-scrub"

// seedSynchronizedChunks adds the chunk layout the cloud sync engine produces:
// <audio-root>/<recording>/chunk-%08d.pcm files whose stored hash, byte count
// and absolute path match the files on disk, plus a stitched whole that is the
// concatenation of the chunks.
func seedSynchronizedChunks(t *testing.T, root, profile, providerRecording string, chunks int) {
	t.Helper()
	profileRoot := filepath.Join(root, profile)
	audioRoot := filepath.Join(profileRoot, "audio")
	db, err := openSQL(filepath.Join(profileRoot, profileDBRel))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var whole bytes.Buffer
	for seq := range chunks {
		// Deterministic per-chunk PCM content; 32 bytes per recorded millisecond.
		body := bytes.Repeat([]byte{byte(0x40 + seq), byte(0xA0 + seq)}, 32*100)
		chunkDir := filepath.Join(audioRoot, providerRecording)
		if err := os.MkdirAll(chunkDir, 0700); err != nil {
			t.Fatalf("mkdir chunk dir: %v", err)
		}
		path := filepath.Join(chunkDir, fmt.Sprintf("chunk-%08d.pcm", seq))
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		whole.Write(body)
		execSQL(t, db, `INSERT INTO sync_chunks(recording_id,seq,sha256,bytes,file_path,downloaded_at) VALUES(?,?,?,?,?,1)`,
			providerRecording, seq, fileHash(t, path), len(body), path)
	}
	stitched := filepath.Join(audioRoot, "rec-a.pcm")
	if err := os.WriteFile(stitched, whole.Bytes(), 0600); err != nil {
		t.Fatalf("write stitched pcm: %v", err)
	}
}

// applyCompletedTurnForget leaves the database exactly as a completed turn
// forget does: tombstone complete with redaction_done=1, a provider fence for
// the recording, chunk paths and lengths retained but hashes erased.
func applyCompletedTurnForget(t *testing.T, root, profile string) {
	t.Helper()
	completedForget(t, root, profile, forgetRetentionTombstone)
	db, err := openSQL(filepath.Join(root, profile, profileDBRel))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	execSQL(t, db, `UPDATE sync_chunks SET sha256='' WHERE recording_id='provider-rec'`)
	execSQL(t, db, `UPDATE sync_recordings SET last_error='source segment forgotten',updated_at=1 WHERE recording_id='provider-rec'`)
}

// seedForgetWithoutProviderFence records a completed tombstone but no provider
// fence: the retained chunks must stay rejected.
func seedForgetWithoutProviderFence(t *testing.T, root, profile string) {
	t.Helper()
	db, err := openSQL(filepath.Join(root, profile, profileDBRel))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	execSQL(t, db, `INSERT INTO forget_tombstones(id,scope,requested_id,locator_key,expected_state_hash,redaction_done,status,created_at)
 VALUES(?, 'turn','t:1',?,'hash',1,'complete',1)`, forgetRetentionTombstone, "locator:"+forgetRetentionTombstone)
	execSQL(t, db, `UPDATE sync_chunks SET sha256='' WHERE recording_id='provider-rec'`)
}

// TestBackupTurnForgetRetainedChunksRoundTrip proves the 3.4 round trip: after
// a completed turn forget that retained the chunks (hashes erased), the root
// can be backed up, verified and restored, and the restored root is itself
// archivable again.
func TestBackupTurnForgetRetainedChunksRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	seedProfile(t, root, "home", false)
	seedSynchronizedChunks(t, root, "home", "provider-rec", 2)
	applyCompletedTurnForget(t, root, "home")

	archive := filepath.Join(t.TempDir(), "retained.tgz")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create after completed turn forget: %v", err)
	}
	if _, err := Verify(ctx, VerifyOptions{ProfilesRoot: root}); err != nil {
		t.Fatalf("Verify after completed turn forget: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(ctx, RestoreOptions{Archive: archive, Dest: dest, BarrierSource: root, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Restore after completed turn forget: %v", err)
	}
	restoredChunk := filepath.Join(dest, "home", "audio", "provider-rec", fmt.Sprintf("chunk-%08d.pcm", 1))
	if fileHash(t, restoredChunk) != fileHash(t, filepath.Join(root, "home", "audio", "provider-rec", fmt.Sprintf("chunk-%08d.pcm", 1))) {
		t.Fatal("restored retained chunk differs from the source")
	}
	db, err := openSQL(filepath.Join(dest, "home", profileDBRel))
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	var retained, missingHash int
	if err := db.QueryRow(`SELECT COUNT(*), SUM(sha256='') FROM sync_chunks WHERE recording_id='provider-rec'`).Scan(&retained, &missingHash); err != nil {
		db.Close()
		t.Fatalf("read restored chunks: %v", err)
	}
	db.Close()
	if retained != 2 || missingHash != 2 {
		t.Fatalf("restored chunks = %d rows, %d hash-less, want 2/2", retained, missingHash)
	}

	// The restored root must be archivable again.
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: dest, Out: filepath.Join(t.TempDir(), "again.tgz"), ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create from restored root: %v", err)
	}
}

// TestBackupEmptyChunkSHARequiresForgetProof pins the negative side of the 3.4
// rule: erased chunk hashes are only tolerated for a completed turn forget with
// its provider fence. A missing fence or an unfinished tombstone keeps the root
// unarchivable.
func TestBackupEmptyChunkSHARequiresForgetProof(t *testing.T) {
	ctx := context.Background()

	t.Run("missing_provider_fence", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		seedSynchronizedChunks(t, root, "home", "provider-rec", 2)
		seedForgetWithoutProviderFence(t, root, "home")
		_, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "x.tgz"), ConfirmWritersStopped: true})
		if code := ErrorCode(err); code != CodeAudioMissing {
			t.Fatalf("Create code = %q (err=%v), want %q", code, err, CodeAudioMissing)
		}
	})

	t.Run("unfinished_tombstone", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		seedSynchronizedChunks(t, root, "home", "provider-rec", 2)
		applyCompletedTurnForget(t, root, "home")
		db, err := openSQL(filepath.Join(root, "home", profileDBRel))
		if err != nil {
			t.Fatal(err)
		}
		execSQL(t, db, `UPDATE forget_tombstones SET redaction_done=0 WHERE id=?`, forgetRetentionTombstone)
		db.Close()
		_, err = Create(ctx, CreateOptions{ProfilesRoot: root, Out: filepath.Join(t.TempDir(), "x.tgz"), ConfirmWritersStopped: true})
		if code := ErrorCode(err); code != CodeForgetUnfinished {
			t.Fatalf("Create code = %q (err=%v), want %q", code, err, CodeForgetUnfinished)
		}
	})
}

// repackArchiveWithTreeChange extracts an archive, lets the caller modify the
// extracted tree, recomputes the manifest file list so the archive itself stays
// manifest-consistent, and repacks it. A restore must then catch the damage
// through the database-referenced audio validation, not through the manifest.
func repackArchiveWithTreeChange(t *testing.T, archive string, change func(t *testing.T, work string)) string {
	t.Helper()
	work := t.TempDir()
	manifest, err := extractArchive(archive, work)
	if err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	change(t, work)
	files, err := snapshot(work)
	if err != nil {
		t.Fatalf("snapshot work dir: %v", err)
	}
	manifest.Files = fileEntries(files)
	repacked := filepath.Join(t.TempDir(), "repacked.tgz")
	if err := writeArchive(repacked, work, files, manifest); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	return repacked
}

// assertRestoreRefusedAndUnpublished asserts restore rejects the archive and
// leaves the destination absent.
func assertRestoreRefusedAndUnpublished(t *testing.T, archive, sourceRoot string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restored")
	_, err := Restore(context.Background(), RestoreOptions{
		Archive:               archive,
		Dest:                  dest,
		BarrierSource:         sourceRoot,
		ConfirmWritersStopped: true,
	})
	if code := ErrorCode(err); code != CodeAudioMissing && code != CodeAudioOutOfBounds {
		t.Fatalf("Restore code = %q (err=%v), want an audio reference failure", code, err)
	}
	if _, statErr := os.Lstat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("restore must not publish the destination (stat err=%v)", statErr)
	}
}

// bytesPerRecordedMS is the canonical PCM size of one recorded millisecond:
// 16 samples at 16 bits.
const bytesPerRecordedMS = 32

// silenceInterval stands in for the external cleanup step of a turn forget: it
// silences the forgotten byte interval in one managed audio file.
func silenceInterval(t *testing.T, path string, start, end int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if int64(len(data)) < end {
		t.Fatalf("%s is shorter than the forgotten interval", path)
	}
	for i := start; i < end; i++ {
		data[i] = 0
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestBackupActualTurnForgetRetainedChunksRoundTrip drives the real forget
// path (PreviewForget, CommitForget, MarkForgetFileDone, CompleteForget) over a
// synthetic imported attachment with genuine synchronized chunks, then proves
// the backup/verify/restore round trip. It complements
// TestBackupTurnForgetRetainedChunksRoundTrip, which uses the seeded-tombstone
// convention to validate individual broken-row states.
func TestBackupActualTurnForgetRetainedChunksRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	profileRoot := filepath.Join(root, "home")
	store, err := memory.Open(filepath.Join(profileRoot, profileDBRel))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer store.Close()
	db := store.DB()

	// One final transcript segment with a timed turn at 1000-2000 ms, imported
	// through the real ingestion path so source attachments and provider
	// identity exist for the forget fences.
	localStart, localEnd := int64(1000), int64(2000)
	result := asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source: asrinput.Source{
			Provider: "synthetic", ProviderRequestID: "req-seg-1", DeviceID: "device",
			RecordingID: "provider-rec", ConversationID: "conv-1", SegmentID: "seg-1",
			SegmentOffsetMS: 0, SegmentDurationMS: 30000,
			TranscriptRevision: "rev-1", CompletionStatus: "final",
		},
		Turns: []asrinput.Turn{{
			ID: "t-1", OriginalIndex: 1, Text: "被遗忘的片段",
			LocalStartMS: &localStart, LocalEndMS: &localEnd,
			GlobalStartMS: &localStart, GlobalEndMS: &localEnd,
		}},
	}
	if _, err := store.IngestTranscript(ctx, result); err != nil {
		t.Fatalf("IngestTranscript: %v", err)
	}
	var turnID string
	origTurnID := result.Source.SegmentID + "/" + result.Source.TranscriptRevision + "/" + result.Turns[0].ID
	if err := db.QueryRowContext(ctx, `SELECT id FROM turns WHERE original_turn_id=?`, origTurnID).Scan(&turnID); err != nil {
		t.Fatalf("locate turn row: %v", err)
	}

	// Genuine synchronized chunks: 30 s of patterned PCM split into two 10 s
	// chunk files whose concatenation is the stitched whole, with the stored
	// hash, byte count and absolute path matching the files on disk.
	audioRoot := filepath.Join(profileRoot, "audio")
	stitched := filepath.Join(audioRoot, "provider-rec.pcm")
	chunkDir := filepath.Join(audioRoot, "provider-rec")
	if err := os.MkdirAll(chunkDir, 0700); err != nil {
		t.Fatalf("mkdir chunk dir: %v", err)
	}
	chunkMS := int64(10000)
	var whole bytes.Buffer
	chunkPaths := []string{}
	for seq := range 2 {
		body := make([]byte, chunkMS*bytesPerRecordedMS)
		for i := range body {
			body[i] = byte(i % 251)
		}
		path := filepath.Join(chunkDir, fmt.Sprintf("chunk-%08d.pcm", seq))
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		whole.Write(body)
		execSQL(t, db, `INSERT INTO sync_chunks(recording_id,seq,sha256,bytes,file_path,downloaded_at) VALUES(?,?,?,?,?,1)`,
			"provider-rec", seq, fileHash(t, path), len(body), path)
		chunkPaths = append(chunkPaths, path)
	}
	if err := os.WriteFile(stitched, whole.Bytes(), 0600); err != nil {
		t.Fatalf("write stitched pcm: %v", err)
	}
	execSQL(t, db, `INSERT INTO sync_recordings(recording_id,owner,device_id,state,started_at,stitched_path,total_chunks,total_samples,transcript_revision,transcription_state,updated_at)
		 VALUES('provider-rec','default','device','ingested',NULL,?,2,480000,'rev-1','done',1)`, stitched)

	// The real forget: preview, commit with the CAS state, silence the
	// forgotten interval like the external cleanup does, mark every manifest
	// entry done and complete the tombstone.
	req := memory.ForgetRequest{Scope: "turn", TurnID: turnID}
	preview, err := store.PreviewForget(ctx, req)
	if err != nil {
		t.Fatalf("PreviewForget: %v", err)
	}
	if len(preview.AudioFiles) == 0 {
		t.Fatal("forget preview lists no audio files")
	}
	req.ExpectedState = preview.ExpectedState
	committed, err := store.CommitForget(ctx, req)
	if err != nil {
		t.Fatalf("CommitForget: %v", err)
	}
	if committed.Status != "pending_external_cleanup" {
		t.Fatalf("CommitForget status = %q", committed.Status)
	}
	tombstone := committed.TombstoneID
	startByte, endByte := int64(1000)*bytesPerRecordedMS, int64(2000)*bytesPerRecordedMS
	silenceInterval(t, stitched, startByte, endByte)
	silenceInterval(t, chunkPaths[0], startByte, endByte)
	for _, file := range committed.AudioFiles {
		if err := store.MarkForgetFileDone(ctx, tombstone, file.Path); err != nil {
			t.Fatalf("MarkForgetFileDone(%s): %v", file.Path, err)
		}
	}
	if err := store.CompleteForget(ctx, tombstone); err != nil {
		t.Fatalf("CompleteForget: %v", err)
	}
	status, err := store.ForgetStatus(ctx, tombstone)
	if err != nil {
		t.Fatalf("ForgetStatus: %v", err)
	}
	if status.Status != "complete" || !status.RedactionDone {
		t.Fatalf("forget status = %+v", status)
	}
	var pendingFiles int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forget_files WHERE done=0`).Scan(&pendingFiles); err != nil {
		t.Fatalf("count pending forget files: %v", err)
	}
	if pendingFiles != 0 {
		t.Fatalf("%d forget files still pending", pendingFiles)
	}
	var withHash int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_chunks WHERE recording_id='provider-rec' AND sha256<>''`).Scan(&withHash); err != nil {
		t.Fatalf("count chunk hashes: %v", err)
	}
	if withHash != 0 {
		t.Fatalf("%d chunks kept pre-redaction hashes after the real turn forget", withHash)
	}
	store.Close()

	// Backup, verify and restore the actually forgotten state.
	archive := filepath.Join(t.TempDir(), "actual-forget.tgz")
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create after actual turn forget: %v", err)
	}
	if _, err := Verify(ctx, VerifyOptions{ProfilesRoot: root}); err != nil {
		t.Fatalf("Verify after actual turn forget: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(ctx, RestoreOptions{Archive: archive, Dest: dest, BarrierSource: root, ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Restore after actual turn forget: %v", err)
	}
	for seq, path := range chunkPaths {
		restored := filepath.Join(dest, "home", "audio", "provider-rec", fmt.Sprintf("chunk-%08d.pcm", seq))
		if fileHash(t, restored) != fileHash(t, path) {
			t.Fatalf("restored chunk %d differs from the silenced source", seq)
		}
	}
	restoredDB, err := openSQL(filepath.Join(dest, "home", profileDBRel))
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	var restoredChunks, restoredHashless int
	if err := restoredDB.QueryRow(`SELECT COUNT(*), SUM(sha256='') FROM sync_chunks WHERE recording_id='provider-rec'`).Scan(&restoredChunks, &restoredHashless); err != nil {
		restoredDB.Close()
		t.Fatalf("read restored chunks: %v", err)
	}
	restoredDB.Close()
	if restoredChunks != 2 || restoredHashless != 2 {
		t.Fatalf("restored chunks = %d rows, %d hash-less, want 2/2", restoredChunks, restoredHashless)
	}
	if _, err := Create(ctx, CreateOptions{ProfilesRoot: dest, Out: filepath.Join(t.TempDir(), "again.tgz"), ConfirmWritersStopped: true}); err != nil {
		t.Fatalf("Create from restored root: %v", err)
	}
}

// TestBackupRestoreRejectsCorruptAudioReference covers the 3.4 restore rule:
// validateRoot must apply the same missing/out-of-bounds/hash-mismatch judgment
// as create/verify before publishing, for hash-carrying chunks, for erased-hash
// retained chunks, and for the stitched whole.
func TestBackupRestoreRejectsCorruptAudioReference(t *testing.T) {
	newHealthyRoot := func(t *testing.T) (root, archive string) {
		t.Helper()
		ctx := context.Background()
		root = filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		seedSynchronizedChunks(t, root, "home", "provider-rec", 2)
		archive = filepath.Join(t.TempDir(), "healthy.tgz")
		if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
			t.Fatalf("Create healthy root: %v", err)
		}
		return root, archive
	}

	t.Run("truncated_chunk", func(t *testing.T) {
		root, archive := newHealthyRoot(t)
		corrupt := repackArchiveWithTreeChange(t, archive, func(t *testing.T, work string) {
			path := filepath.Join(work, "home", "audio", "provider-rec", fmt.Sprintf("chunk-%08d.pcm", 0))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, info.Size()/2); err != nil {
				t.Fatal(err)
			}
		})
		assertRestoreRefusedAndUnpublished(t, corrupt, root)
	})

	t.Run("deleted_chunk", func(t *testing.T) {
		root, archive := newHealthyRoot(t)
		corrupt := repackArchiveWithTreeChange(t, archive, func(t *testing.T, work string) {
			if err := os.Remove(filepath.Join(work, "home", "audio", "provider-rec", fmt.Sprintf("chunk-%08d.pcm", 1))); err != nil {
				t.Fatal(err)
			}
		})
		assertRestoreRefusedAndUnpublished(t, corrupt, root)
	})

	t.Run("out_of_bounds_chunk", func(t *testing.T) {
		root, archive := newHealthyRoot(t)
		outside := filepath.Join(t.TempDir(), "outside.pcm")
		if err := os.WriteFile(outside, []byte("pcm"), 0600); err != nil {
			t.Fatal(err)
		}
		corrupt := repackArchiveWithTreeChange(t, archive, func(t *testing.T, work string) {
			db, err := openSQL(filepath.Join(work, "home", profileDBRel))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			execSQL(t, db, `UPDATE sync_chunks SET file_path=? WHERE seq=0`, outside)
		})
		assertRestoreRefusedAndUnpublished(t, corrupt, root)
	})

	t.Run("truncated_retained_chunk", func(t *testing.T) {
		ctx := context.Background()
		root := filepath.Join(t.TempDir(), "profiles")
		seedProfile(t, root, "home", false)
		seedSynchronizedChunks(t, root, "home", "provider-rec", 2)
		applyCompletedTurnForget(t, root, "home")
		archive := filepath.Join(t.TempDir(), "retained.tgz")
		if _, err := Create(ctx, CreateOptions{ProfilesRoot: root, Out: archive, ConfirmWritersStopped: true}); err != nil {
			t.Fatalf("Create retained-chunk root: %v", err)
		}
		corrupt := repackArchiveWithTreeChange(t, archive, func(t *testing.T, work string) {
			path := filepath.Join(work, "home", "audio", "provider-rec", fmt.Sprintf("chunk-%08d.pcm", 0))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, info.Size()-1); err != nil {
				t.Fatal(err)
			}
		})
		assertRestoreRefusedAndUnpublished(t, corrupt, root)
	})

	t.Run("deleted_stitched_audio", func(t *testing.T) {
		root, archive := newHealthyRoot(t)
		corrupt := repackArchiveWithTreeChange(t, archive, func(t *testing.T, work string) {
			if err := os.Remove(filepath.Join(work, "home", "audio", "rec-a.pcm")); err != nil {
				t.Fatal(err)
			}
		})
		assertRestoreRefusedAndUnpublished(t, corrupt, root)
	})
}
