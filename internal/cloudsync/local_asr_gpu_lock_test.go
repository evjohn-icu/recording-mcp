//go:build unix

package cloudsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/gpulock"
)

// The GPU lock contract for local ASR (plan 4.8): every CUDA inference must
// take the single user-level lock (the default path when no explicit path is
// configured), CPU-only runs must bypass it, and an explicit path replaces the
// default for controlled deployments. All of this is verified with synthetic
// stub runner child processes; no GPU is ever touched.

// holdSharedGPULock isolates XDG_RUNTIME_DIR into a per-test directory and
// holds the shared default lock. The returned path is the same one the
// production consumers resolve through gpulock.DefaultPath().
func holdSharedGPULock(t *testing.T) (string, func()) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := gpulock.DefaultPath()
	if err != nil {
		t.Fatalf("resolve default GPU lock path: %v", err)
	}
	release, err := gpulock.Acquire(context.Background(), path, time.Second)
	if err != nil {
		t.Fatalf("hold the shared GPU lock: %v", err)
	}
	return path, release
}

// localGPUTraceStub writes start/end markers around its inference so tests can
// prove two runners never overlapped.
func localGPUTraceStub(t *testing.T, sleep time.Duration) string {
	t.Helper()
	return writeStub(t, `import json, os, sys, time
args = sys.argv[1:]
pcm = args[args.index("--pcm") + 1]
duration = os.path.getsize(pcm) // 32
trace = os.environ["LOCK_TRACE"]
with open(trace, "a") as f:
    f.write("start\n")
    f.flush()
time.sleep(`+strconv.FormatFloat(sleep.Seconds(), 'f', -1, 64)+`)
print(json.dumps({"index": 1, "start_ms": 0, "end_ms": duration, "text": "x"}), flush=True)
time.sleep(0.2)
with open(trace, "a") as f:
    f.write("end\n")
    f.flush()
`)
}

func traceSteps(t *testing.T, trace string) []string {
	t.Helper()
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read lock trace: %v", err)
	}
	return strings.Fields(strings.TrimSpace(string(raw)))
}

// TestLocalASRCudaTakesSharedDefaultGPULock proves a CUDA runner with no
// explicit lock path contends on the one user-level lock instead of running
// unlocked.
func TestLocalASRCudaTakesSharedDefaultGPULock(t *testing.T) {
	requirePython(t)
	_, release := holdSharedGPULock(t)
	defer release()

	trace := filepath.Join(t.TempDir(), "trace.log")
	script := localGPUTraceStub(t, 100*time.Millisecond)
	tr := LocalTranscriber{
		Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16",
		Device: "cuda:0", SegmentMS: 30000, OverlapMS: 2000, LockPath: "",
		LockTimeout: 300 * time.Millisecond,
		Env:         []string{"LOCK_TRACE=" + trace},
	}
	err := tr.TranscribeChunks(context.Background(), writePCM(t, 1000), localTestMeta(), nil,
		func(asrinput.Result) error { return nil })
	if !errors.Is(err, gpulock.ErrBusy) {
		t.Fatalf("CUDA run without an explicit lock path returned %v, want gpulock.ErrBusy: the shared default lock was not taken", err)
	}
	if _, statErr := os.Stat(trace); statErr == nil {
		steps := traceSteps(t, trace)
		t.Fatalf("CUDA inference started while the shared lock was held: trace = %v", steps)
	}
}

// TestLocalASRBlankDeviceDefaultsToCUDA proves a blank LocalTranscriber.Device
// is CUDA too: scripts/local-asr.py defaults --device to cuda:0, so a blank
// device must not bypass the shared lock.
func TestLocalASRBlankDeviceDefaultsToCUDA(t *testing.T) {
	requirePython(t)
	_, release := holdSharedGPULock(t)
	defer release()

	trace := filepath.Join(t.TempDir(), "trace.log")
	script := localGPUTraceStub(t, 100*time.Millisecond)
	tr := LocalTranscriber{
		Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16",
		SegmentMS: 30000, OverlapMS: 2000, LockPath: "",
		LockTimeout: 300 * time.Millisecond,
		Env:         []string{"LOCK_TRACE=" + trace},
	}
	err := tr.TranscribeChunks(context.Background(), writePCM(t, 1000), localTestMeta(), nil,
		func(asrinput.Result) error { return nil })
	if !errors.Is(err, gpulock.ErrBusy) {
		t.Fatalf("blank-device run returned %v, want gpulock.ErrBusy: the runner default device is cuda:0", err)
	}
}

// TestLocalASRCPUDoesNotTakeGPULock proves CPU-only runs bypass the lock: the
// shared lock may be held by someone else while CPU inference proceeds.
func TestLocalASRCPUDoesNotTakeGPULock(t *testing.T) {
	requirePython(t)
	path, release := holdSharedGPULock(t)
	defer release()

	trace := filepath.Join(t.TempDir(), "trace.log")
	script := writeStub(t, `import json, os, sys
args = sys.argv[1:]
pcm = args[args.index("--pcm") + 1]
duration = os.path.getsize(pcm) // 32
with open(os.environ["LOCK_TRACE"], "a") as f:
    f.write("ran\n")
print(json.dumps({"index": 1, "start_ms": 0, "end_ms": duration, "text": "x"}))
`)
	tr := LocalTranscriber{
		Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16",
		Device: "cpu", SegmentMS: 30000, OverlapMS: 2000, LockPath: path,
		LockTimeout: 300 * time.Millisecond, Env: []string{"LOCK_TRACE=" + trace},
	}
	if err := tr.TranscribeChunks(context.Background(), writePCM(t, 1000), localTestMeta(), nil,
		func(asrinput.Result) error { return nil }); err != nil {
		t.Fatalf("CPU run must not wait for the GPU lock: %v", err)
	}
	if steps := traceSteps(t, trace); len(steps) != 1 || steps[0] != "ran" {
		t.Fatalf("CPU inference did not run: %v", steps)
	}
}

// TestLocalASRExplicitLockPathReplacesDefault pins controlled deployments: a
// CUDA runner with an explicit lock path uses that path (and only that path),
// whatever the device string is, while the default lock stays free for the
// audio-analysis consumer.
func TestLocalASRExplicitLockPathReplacesDefault(t *testing.T) {
	requirePython(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	explicit := filepath.Join(t.TempDir(), "deployment.lock")

	// The explicit path serialises CUDA inference.
	releaseExplicit, err := gpulock.Acquire(context.Background(), explicit, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tr := LocalTranscriber{
		Python: "python3", Script: writeStub(t, "import time\ntime.sleep(2)\n"),
		Backend: "ark", Model: "m", Dtype: "float16", Device: "cuda:0",
		SegmentMS: 30000, OverlapMS: 2000, LockPath: explicit, LockTimeout: 300 * time.Millisecond,
	}
	if err := tr.TranscribeChunks(context.Background(), writePCM(t, 1000), localTestMeta(), nil,
		func(asrinput.Result) error { return nil }); !errors.Is(err, gpulock.ErrBusy) {
		t.Fatalf("CUDA run with a held explicit lock returned %v, want gpulock.ErrBusy", err)
	}
	releaseExplicit()

	// A held default lock must not block the explicitly configured path.
	_, releaseDefault := holdSharedGPULock(t)
	defer releaseDefault()
	trace := filepath.Join(t.TempDir(), "trace.log")
	tr = LocalTranscriber{
		Python: "python3", Script: localGPUTraceStub(t, 10*time.Millisecond),
		Backend: "ark", Model: "m", Dtype: "float16", Device: "cuda:0",
		SegmentMS: 30000, OverlapMS: 2000, LockPath: explicit,
		Env: []string{"LOCK_TRACE=" + trace},
	}
	if err := tr.TranscribeChunks(context.Background(), writePCM(t, 1000), localTestMeta(), nil,
		func(asrinput.Result) error { return nil }); err != nil {
		t.Fatalf("CUDA run with an explicit lock path must ignore a held default lock: %v", err)
	}
}

// TestLocalASRTwoProfilesShareDefaultCUDALock uses the stub child-process
// fixture to prove two profiles (two independent transcribers) contend on ONE
// default lock: while it is held, both are refused; once free, their
// inference serialises as start/end/start/end.
func TestLocalASRTwoProfilesShareDefaultCUDALock(t *testing.T) {
	requirePython(t)
	_, release := holdSharedGPULock(t)

	script := localGPUTraceStub(t, 400*time.Millisecond)
	pcm := writePCM(t, 1000)
	trace := filepath.Join(t.TempDir(), "trace.log")
	newTranscriber := func(model string, lockTimeout time.Duration) LocalTranscriber {
		return LocalTranscriber{
			Python: "python3", Script: script, Backend: "ark", Model: model, Dtype: "float16",
			Device: "cuda:0", SegmentMS: 30000, OverlapMS: 2000, LockPath: "",
			LockTimeout: lockTimeout,
			Env:         []string{"LOCK_TRACE=" + trace},
		}
	}

	// Phase 1: one held lock refuses both profiles — they cannot have a lock
	// each.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, model := range []string{"profile-a/model", "profile-b/model"} {
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			errs[i] = newTranscriber(model, 300*time.Millisecond).TranscribeChunks(context.Background(), pcm, localTestMeta(), nil,
				func(asrinput.Result) error { return nil })
		}(i, model)
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, gpulock.ErrBusy) {
			t.Fatalf("profile %d ran while the shared lock was held: %v", i, err)
		}
	}
	release()

	// Phase 2: with the lock free the two profiles still never overlap.
	errs = make([]error, 2)
	for i, model := range []string{"profile-a/model", "profile-b/model"} {
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			errs[i] = newTranscriber(model, 5*time.Second).TranscribeChunks(context.Background(), pcm, localTestMeta(), nil,
				func(asrinput.Result) error { return nil })
		}(i, model)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("profile %d: %v", i, err)
		}
	}
	if steps := traceSteps(t, trace); strings.Join(steps, ",") != "start,end,start,end" {
		t.Fatalf("lock trace = %v, want start,end,start,end: the two profiles overlapped on the shared lock", steps)
	}
}
