package audioanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/process"
	"recordingmcp/internal/retention"
)

// The staging tests exercise the managed-root analysis staging contract:
//
//   - Runner.AudioRoot makes every real adapter run stage its PCM copy and
//     request under <audioRoot>/.analysis-tmp/<sha256(recordingID)>/<runID>,
//     with 0700 directories and 0600 files,
//   - the runner holds a shared retention lock on the audio root for the whole
//     run and hands it to the adapter child, so a worker that is killed leaves
//     the lock held by the child until the child exits,
//   - the staging of one recording is removed again after a normal run while
//     sibling recordings' staging directories stay untouched.
//
// The path layout below is the documented retention.AnalysisStagingRel
// contract; keep it in sync with that helper.
func stagingHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func stagingRecDir(audioRoot, recordingID string) string {
	return filepath.Join(audioRoot, ".analysis-tmp", stagingHash(recordingID))
}

const stagingStubPython = `import json, os, stat, sys, time

def write_diag():
    pcm = sys.argv[4]
    info = {
        "pid": os.getpid(),
        "pcm": pcm,
        "pcm_mode": stat.S_IMODE(os.stat(pcm).st_mode),
        "run_dir_mode": stat.S_IMODE(os.stat(os.path.dirname(pcm)).st_mode),
        "rec_dir_mode": stat.S_IMODE(os.stat(os.path.dirname(os.path.dirname(pcm))).st_mode),
        "rec_dir": os.path.dirname(os.path.dirname(pcm)),
        "lock_fd": None,
        "lock_held": None,
    }
    for fd in sorted(os.listdir("/proc/self/fd"), key=int):
        try:
            target = os.readlink("/proc/self/fd/" + fd)
        except OSError:
            continue
        if target.endswith(".retention.lock"):
            info["lock_fd"] = int(fd)
            with open("/proc/self/fdinfo/" + fd) as fh:
                info["lock_held"] = any(line.startswith("lock") for line in fh)
    with open(os.environ["AUDIO_STUB_DIAG"], "w") as fh:
        json.dump(info, fh)

write_diag()
if os.environ.get("AUDIO_STUB_HOLD"):
    time.sleep(600)
request = json.load(open(sys.argv[2]))
result = {
    "schema_version": request["schema_version"],
    "run_id": request["run_id"],
    "recording_id": request["recording_id"],
    "source_audio_sha256": request["source_audio_sha256"],
    "input_sha256": request["input_sha256"],
    "transcript_revision": request["transcript_revision"],
    "model_id": request["model_id"],
    "model_revision": request["model_revision"],
    "model_files_sha256": request["model_files_sha256"],
    "capability": request["capability"],
    "start_ms": request["start_ms"],
    "end_ms": request["end_ms"],
    "config_sha256": request["config_sha256"],
    "source_attachment_ids": request["source_attachment_ids"],
    "source_snapshot_sha256": request["source_snapshot_sha256"],
    "status": "succeeded",
    "events": [],
    "provenance": {
        "adapter_version": "stub",
        "frontend_id": "stub",
        "device": "cpu",
        "input_sha256": request["input_sha256"],
        "model_files_sha256": request["model_files_sha256"],
        "parent_run_ids": [],
        "source_snapshot_sha256": request["source_snapshot_sha256"],
    },
}
with open(sys.argv[8], "w") as fh:
    json.dump(result, fh)
`

type stagingDiag struct {
	Pid        int    `json:"pid"`
	Pcm        string `json:"pcm"`
	PcmMode    int    `json:"pcm_mode"`
	RunDirMode int    `json:"run_dir_mode"`
	RecDirMode int    `json:"rec_dir_mode"`
	RecDir     string `json:"rec_dir"`
	LockFD     *int   `json:"lock_fd"`
	LockHeld   *bool  `json:"lock_held"`
}

// stagingFixture extends the forget fixture with a real subprocess adapter:
// a pinned model file, a config document and a Python stub that records what
// it saw (paths, permissions, inherited lock) before writing its result.
type stagingFixture struct {
	forgetRegFixture
	cfg      *Config
	cfgPath  string
	resolved Resolved
	diagPath string
	analyzer SubprocessAnalyzer
}

func newStagingFixture(t *testing.T, withSiblingStaging bool) *stagingFixture {
	t.Helper()
	f := newForgetRegFixture(t)
	s := &stagingFixture{forgetRegFixture: *f, diagPath: filepath.Join(f.dir, "stub-diag.json")}

	modelsDir := filepath.Join(f.dir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(modelsDir, "stub-model.bin")
	modelBytes := bytes.Repeat([]byte("m"), 4096)
	if err := os.WriteFile(modelPath, modelBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	modelSum := sha256.Sum256(modelBytes)
	modelHash := hex.EncodeToString(modelSum[:])

	stubsDir := filepath.Join(f.dir, "stubs")
	if err := os.MkdirAll(stubsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stubPath := filepath.Join(stubsDir, "audio-stub.py")
	if err := os.WriteFile(stubPath, []byte(stagingStubPython), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgJSON := fmt.Sprintf(`{
  "schema_version": %q,
  "python": "python3",
  "runner": %q,
  "run_timeout_seconds": 300,
  "capabilities": {
    "vad": {
      "enabled": true,
      "model_id": "stub-model",
      "model_revision": "rev-1",
      "device": "cpu",
      "frontend_id": "stub",
      "files": [{"path": %q, "sha256": %q}]
    }
  }
}`, ConfigSchema, stubPath, modelPath, modelHash)
	s.cfgPath = filepath.Join(f.dir, "audio-config.json")
	if err := os.WriteFile(s.cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(s.cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	resolved, err := cfg.Resolve(CapabilityVAD)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	s.cfg, s.resolved = cfg, resolved
	s.analyzer = SubprocessAnalyzer{
		Config: cfg, ConfigPath: s.cfgPath, Resolved: resolved,
		Env: []string{"AUDIO_STUB_DIAG=" + s.diagPath},
	}

	if withSiblingStaging {
		// Pre-existing staging of the sibling recording that must survive the
		// target recording's run and cleanup.
		keepDir := filepath.Join(stagingRecDir(f.audioDir, f.sibling), "keep-run")
		if err := os.MkdirAll(keepDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keepDir, "keep.bin"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *stagingFixture) request(generation string) Request {
	file := s.resolved.Files[0]
	// The 4.7 identity contract pins the verified model files by absolute
	// path, exactly as verifyModelIdentity looks them up.
	req := Request{
		SchemaVersion:     ContractSchema,
		RecordingID:       s.target,
		SourceAudioSHA256: s.pcmHash,
		InputSHA256:       s.pcmHash,
		ModelID:           s.resolved.ModelID,
		ModelRevision:     s.resolved.ModelRevision,
		ModelFilesSHA256:  map[string]string{file.Path: file.SHA256},
		Capability:        CapabilityVAD,
		StartMS:           0,
		EndMS:             2000,
		ConfigSHA256:      s.resolved.ConfigSHA256,
		SourceAttachmentIDs: []string{},
	}
	req.RunID = ComputeRunID(req, generation)
	return req
}

func (s *stagingFixture) enqueue(generation string) EnqueueOutput {
	s.t.Helper()
	out, err := s.audio.Enqueue(context.Background(), EnqueueInput{
		RecordingID: s.target, Capability: CapabilityVAD, Generation: generation,
		Request: s.request(generation), MaxAttempts: 3,
	})
	if err != nil {
		s.t.Fatalf("enqueue staging vad: %v", err)
	}
	return out
}

func (s *stagingFixture) readDiag() stagingDiag {
	s.t.Helper()
	data, err := os.ReadFile(s.diagPath)
	if err != nil {
		s.t.Fatalf("read adapter diagnostics: %v", err)
	}
	var diag stagingDiag
	if err := json.Unmarshal(data, &diag); err != nil {
		s.t.Fatalf("decode adapter diagnostics: %v", err)
	}
	return diag
}

// TestStagingAttemptsUseDistinctDirectories pins the retry-race guard: two
// attempts of the same run id get unique staging directories (exclusive
// mkdir), one attempt's cleanup never touches the other attempt's staging,
// and the empty recording bucket may stay behind for the exclusive forget.
func TestStagingAttemptsUseDistinctDirectories(t *testing.T) {
	f := newStagingFixture(t, false)
	req := f.request("gen-1")
	// The managed staging workspace comes from the runner's runtime context;
	// inject it the way Runner.RunJob does.
	ctx := context.WithValue(context.Background(), analysisRuntimeKey{}, analysisRuntime{audioRoot: f.audioDir})

	first, err := openAnalysisStaging(ctx, req)
	if err != nil {
		t.Fatalf("open first attempt staging: %v", err)
	}
	defer first.close()
	second, err := openAnalysisStaging(ctx, req)
	if err != nil {
		t.Fatalf("open second attempt staging: %v", err)
	}
	defer second.close()

	if first.runRel == second.runRel {
		t.Fatalf("concurrent attempts share staging directory %s", first.runRel)
	}
	if filepath.Dir(first.runRel) != filepath.Dir(second.runRel) ||
		filepath.Dir(first.runRel) != retention.AnalysisStagingRel(f.target) {
		t.Fatalf("attempt staging escaped the recording bucket: %q and %q", first.runRel, second.runRel)
	}

	// Both attempts write into their own directory; neither sees the other's
	// bytes.
	for _, attempt := range []*analysisStaging{first, second} {
		file, err := attempt.create("input.pcm")
		if err != nil {
			t.Fatalf("attempt staging create: %v", err)
		}
		if _, err := file.Write([]byte(attempt.runRel)); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := attempt.read("input.pcm")
		if err != nil || string(data) != attempt.runRel {
			t.Fatalf("attempt read its own bytes: %q, %v", data, err)
		}
	}

	// Closing the first attempt removes only its own run directory.
	firstAbs := first.absPath(".")
	first.close()
	if _, err := os.Stat(firstAbs); !os.IsNotExist(err) {
		t.Fatalf("first attempt staging survived close: %v", err)
	}
	data, err := second.read("input.pcm")
	if err != nil || string(data) != second.runRel {
		t.Fatalf("second attempt staging damaged by first close: %q, %v", data, err)
	}
}

// TestStagingIsPerRecordingAndRunWithSharedLock verifies the managed-root
// staging contract of a normal successful run.
func TestStagingIsPerRecordingAndRunWithSharedLock(t *testing.T) {
	f := newStagingFixture(t, true)
	out := f.enqueue("gen-1")
	job := f.claim("audio_vad", out.JobID)

	runner := &Runner{Store: f.audio, AudioRoot: f.audioDir, Analyzer: f.analyzer, Resolver: forgetRegResolver{data: f.pcm}}
	if err := runner.RunJob(context.Background(), job); err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	diag := f.readDiag()
	recDir := stagingRecDir(f.audioDir, f.target)
	if got := filepath.Dir(filepath.Dir(diag.Pcm)); got != recDir {
		t.Fatalf("staging per-recording dir = %q, want %q", got, recDir)
	}
	if diag.PcmMode != 0o600 {
		t.Fatalf("staged PCM mode = %o, want 600", diag.PcmMode)
	}
	if diag.RunDirMode != 0o700 || diag.RecDirMode != 0o700 {
		t.Fatalf("staging dir modes = run %o rec %o, want 700/700", diag.RunDirMode, diag.RecDirMode)
	}
	if diag.LockFD == nil || diag.LockHeld == nil || !*diag.LockHeld {
		t.Fatalf("adapter child did not observe the shared audio-root lock: %+v", diag)
	}

	// The normal run cleaned up after itself without touching the sibling.
	if _, err := os.Stat(filepath.Dir(diag.Pcm)); !os.IsNotExist(err) {
		t.Fatalf("run staging directory survived the run: %v", err)
	}
	if entries, err := os.ReadDir(recDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				t.Fatalf("stale staging file left behind: %s", filepath.Join(recDir, entry.Name()))
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat staging root: %v", err)
	}
	keep, err := os.Stat(filepath.Join(stagingRecDir(f.audioDir, f.sibling), "keep-run", "keep.bin"))
	if err != nil || !keep.Mode().IsRegular() {
		t.Fatalf("sibling staging was damaged: %v", err)
	}

	runs, err := f.audio.CurrentRuns(context.Background(), f.target)
	if err != nil || len(runs) != 1 || runs[0].Capability != CapabilityVAD || runs[0].Status != StatusSucceeded {
		t.Fatalf("CurrentRuns after stub run = %v, %v", runs, err)
	}
}

// TestChildHoldsSharedAudioRootLockAfterWorkerKill kills the worker process
// mid-analysis. The adapter child inherits the shared audio-root lock, so an
// exclusive cleanup must stay blocked until the child is gone, and the
// worker's staging must remain for a later retry cleanup.
func TestChildHoldsSharedAudioRootLockAfterWorkerKill(t *testing.T) {
	if os.Getenv("AUDIO_STAGING_HELPER") == "1" {
		runStagingHelperWorker()
		return // unreachable: the helper blocks until it is killed
	}

	f := newStagingFixture(t, false)
	out := f.enqueue("gen-1") // stays queued; the helper claims it

	marker := filepath.Join(f.dir, "helper-marker")
	env := append(os.Environ(),
		"AUDIO_STAGING_HELPER=1",
		"AUDIO_STAGING_DB="+filepath.Join(f.dir, "audio.db"),
		"AUDIO_STAGING_ROOT="+f.audioDir,
		"AUDIO_STAGING_PCM="+filepath.Join(f.audioDir, f.target+".pcm"),
		"AUDIO_STAGING_CFG="+f.cfgPath,
		"AUDIO_STAGING_MARKER="+marker,
		"AUDIO_STUB_DIAG="+f.diagPath,
	)
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildHoldsSharedAudioRootLockAfterWorkerKill$", "-test.timeout=180s")
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	waitForMarker := func(want string, limit time.Duration) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for {
			data, err := os.ReadFile(marker)
			if err == nil {
				if string(data) != want {
					t.Fatalf("helper marker = %q (stderr: %s)", string(data), stderr.String())
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("helper marker %q never appeared as %q (stderr: %s)", marker, want, stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitForMarker("ready", 30*time.Second)

	blockExclusive := func(limit time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		release, err := retention.LockFile(ctx, f.audioDir, true)
		if err == nil {
			release.Close()
		}
		return err
	}
	if err := blockExclusive(300 * time.Millisecond); err == nil {
		t.Fatal("exclusive lock acquired while the worker runs")
	}
	if release, err := retention.LockFile(context.Background(), f.audioDir, false); err != nil {
		t.Fatalf("shared LockFile during analysis: %v", err)
	} else {
		release.Close()
	}

	// Kill the worker the way a crash would: SIGKILL, no cleanup.
	killed = true
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()

	// The adapter child outlives the worker and keeps the inherited lock.
	if err := blockExclusive(300 * time.Millisecond); err == nil {
		t.Fatal("exclusive lock acquired while only the adapter child is left")
	}
	diag := f.readDiag()
	child, err := os.FindProcess(diag.Pid)
	if err != nil {
		t.Fatalf("find adapter child: %v", err)
	}
	_ = child.Signal(syscall.SIGKILL)
	_, _ = child.Wait()

	// With the child gone the retry cleanup can take the exclusive lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := blockExclusive(500 * time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exclusive lock still blocked after both processes died: %v", err)
		}
	}

	// The killed worker left its staging behind for the retry cleanup. The
	// run directory carries a unique per-attempt suffix; never hardcode it.
	bucket := stagingRecDir(f.audioDir, f.target)
	entries, err := os.ReadDir(bucket)
	if err != nil {
		t.Fatalf("read staging bucket: %v", err)
	}
	if len(entries) != 1 || !entries[0].IsDir() || !strings.HasPrefix(entries[0].Name(), out.RunID) {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("unexpected staging runs after kill: %v", names)
	}
	info, err := os.Stat(filepath.Join(bucket, entries[0].Name(), "input.pcm"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("worker staging vanished before retry cleanup: %v", err)
	}
	stagingRoots, err := os.ReadDir(filepath.Join(f.audioDir, ".analysis-tmp"))
	if err != nil {
		t.Fatalf("read staging root: %v", err)
	}
	if len(stagingRoots) != 1 || stagingRoots[0].Name() != stagingHash(f.target) {
		var names []string
		for _, entry := range stagingRoots {
			names = append(names, entry.Name())
		}
		t.Fatalf("unexpected staging roots after kill: %v", names)
	}
}

// runStagingHelperWorker simulates the audio worker process in the child-lock
// test: claim, acquire the shared lock through the runner and block inside the
// adapter until the test kills this process.
func runStagingHelperWorker() {
	dbPath := os.Getenv("AUDIO_STAGING_DB")
	audioRoot := os.Getenv("AUDIO_STAGING_ROOT")
	pcmPath := os.Getenv("AUDIO_STAGING_PCM")
	cfgPath := os.Getenv("AUDIO_STAGING_CFG")
	marker := os.Getenv("AUDIO_STAGING_MARKER")
	diagPath := os.Getenv("AUDIO_STUB_DIAG")
	fail := func(format string, args ...any) {
		_ = os.WriteFile(marker, []byte("error "+fmt.Sprintf(format, args...)), 0o600)
		os.Exit(1)
	}
	store, err := memory.Open(dbPath)
	if err != nil {
		fail("open db: %v", err)
	}
	defer store.Close()
	audio := NewStore(store.DB())
	job, err := process.Claim(context.Background(), store.DB(), process.ClaimOptions{
		WorkerID: "staging-helper", Stages: []string{"audio_vad"}, Lease: time.Minute, ProcessorVersion: "staging-test",
	})
	if err != nil {
		fail("claim: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fail("load config: %v", err)
	}
	resolved, err := cfg.Resolve(CapabilityVAD)
	if err != nil {
		fail("resolve: %v", err)
	}
	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		fail("read pcm: %v", err)
	}
	analyzer := SubprocessAnalyzer{
		Config: cfg, ConfigPath: cfgPath, Resolved: resolved,
		Env: []string{"AUDIO_STUB_DIAG=" + diagPath, "AUDIO_STUB_HOLD=1"},
	}
	runner := &Runner{Store: audio, AudioRoot: audioRoot, Analyzer: analyzer, Resolver: forgetRegResolver{data: pcm}}
	go func() {
		if err := runner.RunJob(context.Background(), *job); err != nil {
			fail("runjob: %v", err)
		}
		fail("runjob returned while the adapter was still held")
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if info, err := os.Stat(diagPath); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			fail("adapter diagnostics never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
		fail("write marker: %v", err)
	}
	select {} // blocked mid-analysis until the test kills this process
}
