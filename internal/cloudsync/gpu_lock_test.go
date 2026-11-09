package cloudsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"recordingmcp/internal/asrinput"
)

// TestGPUFileLockSerializesLocalASR proves two local inference processes sharing
// one lock path cannot overlap: the trace must read start/end/start/end. A
// missing lock interleaves the two stubs as start/start/end/end.
func TestGPUFileLockSerializesLocalASR(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace.log")
	script := writeStub(t, `import json, os, sys, time
args = sys.argv[1:]
pcm = args[args.index("--pcm") + 1]
duration = os.path.getsize(pcm) // 32
with open(os.environ["LOCK_TRACE"], "a") as f:
    f.write("start\n")
    f.flush()
time.sleep(0.4)
print(json.dumps({"index": 1, "start_ms": 0, "end_ms": duration, "text": "x"}), flush=True)
time.sleep(0.2)
with open(os.environ["LOCK_TRACE"], "a") as f:
    f.write("end\n")
    f.flush()
`)
	pcm := writePCM(t, 1000)
	lockPath := filepath.Join(dir, "gpu.lock")
	run := func() error {
		tr := LocalTranscriber{
			Python: "python3", Script: script, Backend: "ark", Model: "m", Dtype: "float16",
			SegmentMS: 30000, OverlapMS: 2000, LockPath: lockPath, LockTimeout: 10 * time.Second,
			Env: []string{"LOCK_TRACE=" + trace},
		}
		return tr.TranscribeChunks(context.Background(), pcm, localTestMeta(), nil,
			func(asrinput.Result) error { return nil })
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = run()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Fields(strings.TrimSpace(string(raw)))
	want := []string{"start", "end", "start", "end"}
	if strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Fatalf("lock trace = %v, want %v: inference overlapped", steps, want)
	}
}

func TestGPUFileLockHonorsTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	release, err := acquireGPULock(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := acquireGPULock(context.Background(), path, 100*time.Millisecond); err == nil {
		t.Fatal("second acquire must time out while the lock is held")
	}
}
