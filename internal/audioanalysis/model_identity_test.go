package audioanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plan 4.7: the subprocess adapter must only run when the resolved capability
// still matches the configuration the worker would use right now — model
// id/revision, config digest and the ordered pinned file mapping. Drift is
// rejected with audio_model_unavailable before any PCM is copied and before
// the adapter process is started. The digest covers the frontend tunings the
// Python adapter actually reads (threshold, span_seconds, fbank_backend):
// floats are IEEE-754 64-bit big-endian hex, missing fields are empty
// segments, and the three segments are only appended when at least one is
// present, keeping the legacy digest for tunings-free configs.
//
// The adapter stub is `sh` writing a marker file; "the adapter ran" is
// observable through the marker, so a rejection that accidentally still starts
// the adapter fails the test instead of coinciding on the error code.

// pcmWindow returns a synthetic 16 kHz mono S16LE window of durationMS.
func pcmWindow(durationMS int64) []byte {
	return bytes.Repeat([]byte{0x07, 0x11}, int(durationMS)*16)
}

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// identityHarness builds one temporary configuration with real pinned model
// files, resolves it, and prepares a matching request plus a marker-writing
// adapter stub.
type identityHarness struct {
	dir        string
	configPath string
	stubPath   string
	marker     string
	cfg        *Config
	resolved   Resolved
	pcm        []byte
	request    Request
}

func writeIdentityModel(t *testing.T, dir, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, sha256Bytes([]byte(content))
}

// writeIdentityConfig writes an audio-analysis-config-v1 document whose vad
// capability uses entry verbatim; python/runner point at the shared stub so
// only the capability identity varies between cases.
func writeIdentityConfig(t *testing.T, path, stubPath string, entry map[string]any) {
	t.Helper()
	document := map[string]any{
		"schema_version": ConfigSchema,
		"python":         "sh",
		"runner":         stubPath,
		"capabilities": map[string]any{
			CapabilityVAD: entry,
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadResolved(t *testing.T, path string) (*Config, Resolved) {
	t.Helper()
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", path, err)
	}
	resolved, err := cfg.Resolve(CapabilityVAD)
	if err != nil {
		t.Fatalf("Resolve(vad): %v", err)
	}
	return cfg, resolved
}

// identityRequest builds a valid request pinned to resolved's identity. Go
// producers key model_files_sha256 by the resolved absolute path.
func identityRequest(resolved Resolved, pcm []byte) Request {
	files := map[string]string{}
	for _, file := range resolved.Files {
		files[file.Path] = file.SHA256
	}
	return Request{
		SchemaVersion:     ContractSchema,
		RunID:             "run-identity",
		RecordingID:       "rec-identity",
		SourceAudioSHA256: strings.Repeat("a", 64),
		InputSHA256:       sha256Bytes(pcm),
		ModelID:           resolved.ModelID,
		ModelRevision:     resolved.ModelRevision,
		ModelFilesSHA256:  files,
		Capability:        resolved.Capability,
		StartMS:           0,
		EndMS:             int64(len(pcm) / 32),
		ConfigSHA256:      resolved.ConfigSHA256,
	}
}

func newIdentityHarness(t *testing.T, entry map[string]any) *identityHarness {
	t.Helper()
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "adapter-stub.sh")
	stub := "#!/bin/sh\necho ran > \"$AUDIO_MODEL_STUB_MARKER\"\nexit 7\n"
	if err := os.WriteFile(stubPath, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "audio-config.json")
	if entry == nil {
		modelPath, digest := writeIdentityModel(t, dir, "model-a.onnx", "primary-model-bytes")
		entry = defaultVadEntry(modelPath, digest)
	}
	writeIdentityConfig(t, configPath, stubPath, entry)
	cfg, resolved := loadResolved(t, configPath)
	pcm := pcmWindow(100)
	request := identityRequest(resolved, pcm)
	if err := request.Validate(); err != nil {
		t.Fatalf("identity request must validate: %v", err)
	}
	return &identityHarness{
		dir: dir, configPath: configPath, stubPath: stubPath,
		marker:   filepath.Join(dir, "adapter-marker"),
		cfg:      cfg,
		resolved: resolved,
		pcm:      pcm, request: request,
	}
}

func (h *identityHarness) analyzer(cfg *Config, configPath string, resolved Resolved) SubprocessAnalyzer {
	return SubprocessAnalyzer{
		Config:     cfg,
		ConfigPath: configPath,
		Resolved:   resolved,
		Env:        []string{"AUDIO_MODEL_STUB_MARKER=" + h.marker},
	}
}

func (h *identityHarness) run(t *testing.T, analyzer SubprocessAnalyzer) error {
	t.Helper()
	_, _, err := analyzer.AnalyzeWithEmbeddings(context.Background(), h.request, bytes.NewReader(h.pcm))
	return err
}

// assertAdapterRejected pins the drift contract: the safe code is reported and
// the adapter was never started.
func assertAdapterRejected(t *testing.T, marker string, err error) {
	t.Helper()
	if ErrorCodeOf(err) != CodeModelUnavailable {
		t.Fatalf("error = %v, want code %s", err, CodeModelUnavailable)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("the adapter was invoked although the resolved model identity no longer matches the config")
	}
}

// adapterStub is the marker-writing stub body shared by manual harnesses.
const adapterStub = "#!/bin/sh\necho ran > \"$AUDIO_MODEL_STUB_MARKER\"\nexit 7\n"

// defaultVadEntry pins one enabled vad capability on a single model file.
func defaultVadEntry(modelPath, digest string) map[string]any {
	return map[string]any{
		"enabled":        true,
		"model_id":       "campplus",
		"model_revision": "rev-1",
		"files":          []map[string]string{{"path": modelPath, "sha256": digest}},
		"device":         "cpu",
		"frontend_id":    "campplus",
	}
}

// TestSubprocessAnalyzerRejectsDriftedModelConfig covers the drift shapes the
// plan names: a worker whose config is B while the queued run was resolved
// under A, a reordered file mapping, a relative model path resolving
// elsewhere, and pinned files that disappear or change after the resolve.
func TestSubprocessAnalyzerRejectsDriftedModelConfig(t *testing.T) {
	t.Run("worker_config_b_after_resolve_a", func(t *testing.T) {
		h := newIdentityHarness(t, nil)
		// Worker B: same model file, but the revision changed.
		entry := defaultVadEntry(h.resolved.Files[0].Path, h.resolved.Files[0].SHA256)
		entry["model_revision"] = "rev-2"
		configB := filepath.Join(h.dir, "audio-config-b.json")
		writeIdentityConfig(t, configB, h.stubPath, entry)
		cfgB, _ := loadResolved(t, configB)
		err := h.run(t, h.analyzer(cfgB, configB, h.resolved))
		assertAdapterRejected(t, h.marker, err)
	})

	t.Run("file_mapping_reordered", func(t *testing.T) {
		h := newIdentityHarness(t, nil)
		firstPath, firstDigest := h.resolved.Files[0].Path, h.resolved.Files[0].SHA256
		secondPath, secondDigest := writeIdentityModel(t, h.dir, "model-b.onnx", "second-model-bytes")
		pairA := defaultVadEntry(firstPath, firstDigest)
		pairA["files"] = []map[string]string{
			{"path": firstPath, "sha256": firstDigest},
			{"path": secondPath, "sha256": secondDigest},
		}
		configPathA := filepath.Join(h.dir, "audio-config-pair-a.json")
		writeIdentityConfig(t, configPathA, h.stubPath, pairA)
		_, resolvedA := loadResolved(t, configPathA)
		pairB := defaultVadEntry(firstPath, firstDigest)
		pairB["files"] = []map[string]string{
			{"path": secondPath, "sha256": secondDigest},
			{"path": firstPath, "sha256": firstDigest},
		}
		configPathB := filepath.Join(h.dir, "audio-config-pair-b.json")
		writeIdentityConfig(t, configPathB, h.stubPath, pairB)
		cfgB, _ := loadResolved(t, configPathB)
		h.request = identityRequest(resolvedA, h.pcm)
		err := h.run(t, h.analyzer(cfgB, configPathB, resolvedA))
		assertAdapterRejected(t, h.marker, err)
	})

	t.Run("relative_model_path_in_other_directory", func(t *testing.T) {
		// A relative pin resolves against the config file's own directory: the
		// same relative path in another directory is a different model.
		dirA := t.TempDir()
		dirB := t.TempDir()
		stub := filepath.Join(dirA, "adapter-stub.sh")
		if err := os.WriteFile(stub, []byte(adapterStub), 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dirA, "adapter-marker")
		if err := os.MkdirAll(filepath.Join(dirA, "models"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dirB, "models"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, digestA := writeIdentityModel(t, filepath.Join(dirA, "models"), "m.onnx", "model-in-a")
		_, digestB := writeIdentityModel(t, filepath.Join(dirB, "models"), "m.onnx", "model-in-b")
		configA := filepath.Join(dirA, "audio-config.json")
		writeIdentityConfig(t, configA, stub, defaultVadEntry("models/m.onnx", digestA))
		_, resolvedA := loadResolved(t, configA)
		configB := filepath.Join(dirB, "audio-config.json")
		writeIdentityConfig(t, configB, stub, defaultVadEntry("models/m.onnx", digestB))
		cfgB, _ := loadResolved(t, configB)
		pcm := pcmWindow(100)
		request := identityRequest(resolvedA, pcm)
		if err := request.Validate(); err != nil {
			t.Fatal(err)
		}
		analyzer := SubprocessAnalyzer{
			Config: cfgB, ConfigPath: configB, Resolved: resolvedA,
			Env: []string{"AUDIO_MODEL_STUB_MARKER=" + marker},
		}
		_, _, err := analyzer.AnalyzeWithEmbeddings(context.Background(), request, bytes.NewReader(pcm))
		assertAdapterRejected(t, marker, err)
	})

	t.Run("model_file_deleted_after_resolve", func(t *testing.T) {
		h := newIdentityHarness(t, nil)
		if err := os.Remove(h.resolved.Files[0].Path); err != nil {
			t.Fatal(err)
		}
		err := h.run(t, h.analyzer(h.cfg, h.configPath, h.resolved))
		assertAdapterRejected(t, h.marker, err)
	})

	t.Run("model_file_rewritten_after_resolve", func(t *testing.T) {
		h := newIdentityHarness(t, nil)
		if err := os.WriteFile(h.resolved.Files[0].Path, []byte("tampered-model-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := h.run(t, h.analyzer(h.cfg, h.configPath, h.resolved))
		assertAdapterRejected(t, h.marker, err)
	})
}

// TestSubprocessAnalyzerRunsExactConfig proves the guard is not a wall: with
// the exact configuration that produced the resolve, the adapter still runs.
func TestSubprocessAnalyzerRunsExactConfig(t *testing.T) {
	h := newIdentityHarness(t, nil)
	err := h.run(t, h.analyzer(h.cfg, h.configPath, h.resolved))
	if _, statErr := os.Stat(h.marker); statErr != nil {
		t.Fatalf("the adapter did not run for the exact resolved config (error = %v)", err)
	}
}

// digestParts mirrors the documented digest inputs from the resolved view.
func digestParts(resolved Resolved) []string {
	canonical := make([]string, 0, len(resolved.Files))
	for _, file := range resolved.Files {
		canonical = append(canonical, filepath.Base(file.Path)+"="+file.SHA256)
	}
	return []string{
		resolved.Capability, resolved.ModelID, resolved.ModelRevision,
		resolved.Device, resolved.FrontendID, strings.Join(canonical, ","),
	}
}

func digestOf(t *testing.T, parts ...string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// floatSegment encodes one float64 as IEEE-754 64-bit big-endian hex.
func floatSegment(value float64) string {
	return hex.EncodeToString(binary.BigEndian.AppendUint64(nil, math.Float64bits(value)))
}

// TestCapabilityConfigDigestKeepsLegacyShape pins that a capability without
// any frontend tuning keeps the pre-existing digest inputs.
func TestCapabilityConfigDigestKeepsLegacyShape(t *testing.T) {
	h := newIdentityHarness(t, nil)
	want := digestOf(t, digestParts(h.resolved)...)
	if h.resolved.ConfigSHA256 != want {
		t.Fatalf("tunings-free config digest = %s, want legacy digest %s", h.resolved.ConfigSHA256, want)
	}
}

// TestCapabilityConfigDigestCoversFrontendTunings pins the digest extension
// for threshold, span_seconds and fbank_backend: fixed order, IEEE-754 64-bit
// big-endian hex for floats, empty segments for absent fields, appended only
// when at least one tuning is present. A threshold-only edit must change the
// digest, so stale runs cannot pass as the same configuration.
func TestCapabilityConfigDigestCoversFrontendTunings(t *testing.T) {
	base := t.TempDir()
	modelPath, digest := writeIdentityModel(t, base, "model-a.onnx", "shared-model-bytes")

	cases := []struct {
		name     string
		tunings  map[string]any
		segments []string
	}{
		{"threshold_only", map[string]any{"threshold": 0.5}, []string{floatSegment(0.5), "", ""}},
		{"threshold_value_matters", map[string]any{"threshold": 0.6}, []string{floatSegment(0.6), "", ""}},
		{"span_only", map[string]any{"span_seconds": 10.0}, []string{"", floatSegment(10.0), ""}},
		{"fbank_backend_only", map[string]any{"fbank_backend": "python"}, []string{"", "", "python"}},
		{"all_tunings", map[string]any{"threshold": 0.5, "span_seconds": 10.0, "fbank_backend": "python"}, []string{floatSegment(0.5), floatSegment(10.0), "python"}},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := defaultVadEntry(modelPath, digest)
			for key, value := range tc.tunings {
				entry[key] = value
			}
			configPath := filepath.Join(t.TempDir(), "audio-config.json")
			writeIdentityConfig(t, configPath, filepath.Join(base, "unused-stub.sh"), entry)
			_, resolved := loadResolved(t, configPath)
			legacy := digestParts(resolved)
			want := digestOf(t, append(append([]string{}, legacy...), tc.segments...)...)
			if resolved.ConfigSHA256 != want {
				t.Fatalf("digest = %s, want %s (tunings must be part of the config digest)", resolved.ConfigSHA256, want)
			}
			seen[tc.name] = resolved.ConfigSHA256
		})
	}
	// Distinct tunings must not collide.
	if seen["threshold_only"] == seen["threshold_value_matters"] {
		t.Fatal("changing only the threshold produced the same digest")
	}
	if seen["threshold_only"] == seen["span_only"] || seen["threshold_only"] == seen["all_tunings"] {
		t.Fatal("different tuning sets produced the same digest")
	}
}
