package cloudsync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TranscriberConfig selects and configures one ASR adapter for the web process
// or the worker. Both processes share this constructor so a provider accepted
// by one is accepted by the other.
type TranscriberConfig struct {
	Provider, KeyEnv, Endpoint, ResourceID, Model string
	Revision                                      string
	SegmentMS, OverlapMS                          int64
	Concurrency                                   int
	FixtureWindowMS                               int64
	LocalBackend, Python, Script, Dtype, Device   string
	Language                                      string
	WindowMS                                      int64
	MaxNewTokens                                  int
	// Timeout, LoadTimeout, WindowTimeout and the lock fields bound one local
	// ASR run and serialize GPU inference across processes.
	Timeout       time.Duration
	LoadTimeout   time.Duration
	WindowTimeout time.Duration
	LockPath      string
	LockTimeout   time.Duration
	// ProbePython verifies the interpreter, runner script and required modules
	// at startup. The worker sets it; tests can disable it.
	ProbePython bool
}

// Default model ids and the snapshots verified on 2026-11-09. Pinning matters
// because ARK ships remote code; a moving main branch can change it silently.
const (
	defaultARKModel     = "Edge0/ARK-ASR-0.6B"
	defaultARKRevision  = "45776b56d58cdfb2e2eb632f7e110f38684633e0"
	defaultQwenModel    = "Qwen/Qwen3-ASR-1.7B"
	defaultQwenRevision = "7278e1e70fe206f11671096ffdd38061171dd6e5"
)

// IsLocalProvider reports whether the provider runs model code on this host.
// Only these may degrade at startup: a missing doubao key stays a hard error.
func IsLocalProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ark", "qwen", "local":
		return true
	}
	return false
}

// SelectTranscriber builds the configured adapter. "off" returns (nil, nil) so
// callers can advertise uploads as unavailable.
func SelectTranscriber(cfg TranscriberConfig) (Transcriber, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch name {
	case "", "off":
		return nil, nil
	case "fixture":
		return &FixtureTranscriber{WindowMS: cfg.FixtureWindowMS}, nil
	case "doubao":
		apiKey := ""
		if strings.TrimSpace(cfg.KeyEnv) != "" {
			apiKey = os.Getenv(cfg.KeyEnv)
		}
		if strings.TrimSpace(apiKey) == "" {
			return nil, fmt.Errorf("--asr-provider doubao requires %s in the environment", cfg.KeyEnv)
		}
		return &DoubaoTranscriber{
			APIKey: apiKey, Endpoint: cfg.Endpoint, ResourceID: cfg.ResourceID, ModelName: cfg.Model,
			SegmentMS: cfg.SegmentMS, OverlapMS: cfg.OverlapMS, MaxConcurrent: cfg.Concurrency,
		}, nil
	case "ark", "qwen", "local":
		backend := name
		if name == "local" {
			backend = strings.ToLower(strings.TrimSpace(cfg.LocalBackend))
		}
		if backend != "ark" && backend != "qwen" {
			return nil, fmt.Errorf("--asr-provider local requires --asr-backend ark or qwen, got %q", cfg.LocalBackend)
		}
		model := strings.TrimSpace(cfg.Model)
		if model == "" {
			if backend == "ark" {
				model = defaultARKModel
			} else {
				model = defaultQwenModel
			}
		}
		revision := strings.TrimSpace(cfg.Revision)
		if revision == "" {
			switch model {
			case defaultARKModel:
				revision = defaultARKRevision
			case defaultQwenModel:
				revision = defaultQwenRevision
			}
		}
		if revision == "" {
			if info, err := os.Stat(model); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("local ASR model %q is not pinned; pass --asr-revision <commit> or use a local model directory", model)
			}
		}
		dtype := strings.TrimSpace(cfg.Dtype)
		if dtype == "" {
			dtype = "float16"
		}
		if dtype != "float16" {
			return nil, fmt.Errorf("--asr-dtype %q is unsupported; local ASR runs float16 only", dtype)
		}
		windowMS := cfg.WindowMS
		if windowMS <= 0 {
			windowMS = 30000
		}
		if cfg.OverlapMS < 0 || cfg.OverlapMS >= windowMS {
			return nil, fmt.Errorf("--asr-overlap-ms %d must be in [0, --asr-window-ms %d) for local ASR", cfg.OverlapMS, windowMS)
		}
		python := strings.TrimSpace(cfg.Python)
		if python == "" {
			python = defaultLocalPython()
		}
		script := strings.TrimSpace(cfg.Script)
		if script == "" {
			script = "scripts/local-asr.py"
		}
		if cfg.ProbePython {
			if err := probeLocalPython(python, script, backend); err != nil {
				return nil, err
			}
		}
		return &LocalTranscriber{
			Python: python, Script: script, Backend: backend, Model: model, Revision: revision,
			Dtype: dtype, Device: cfg.Device, Language: cfg.Language,
			SegmentMS: windowMS, OverlapMS: cfg.OverlapMS, MaxNewTokens: cfg.MaxNewTokens,
			Timeout: cfg.Timeout, LoadTimeout: cfg.LoadTimeout, WindowTimeout: cfg.WindowTimeout,
			LockPath: cfg.LockPath, LockTimeout: cfg.LockTimeout,
		}, nil
	default:
		return nil, fmt.Errorf("unknown --asr-provider %q (want off, fixture, doubao, ark, qwen or local)", cfg.Provider)
	}
}

// defaultLocalPython prefers the project venv because the system interpreter
// may not have torch/qwen-asr wheels for its version.
func defaultLocalPython() string {
	for _, candidate := range []string{
		filepath.Join(".venv-asr", "bin", "python"),
		filepath.Join(".venv-asr", "bin", "python3"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return "python3"
}

func probeLocalPython(python, script, backend string) error {
	if info, err := os.Stat(script); err != nil || info.IsDir() {
		return fmt.Errorf("local ASR runner %q not found; run from the repository root or pass --asr-script", script)
	}
	modules := []string{"torch", "transformers", "numpy"}
	if backend == "qwen" {
		modules = append(modules, "qwen_asr")
	}
	quoted := make([]string, len(modules))
	for i, module := range modules {
		quoted[i] = strconv.Quote(module)
	}
	code := "import importlib.util as u, sys\n" +
		"missing=[m for m in [" + strings.Join(quoted, ",") + "] if u.find_spec(m) is None]\n" +
		"print(','.join(missing))\n" +
		"sys.exit(3 if missing else 0)\n"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "-c", code).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = "interpreter did not start"
		}
		return fmt.Errorf("local ASR python %q cannot run the %s backend (missing modules: %s); create .venv-asr per docs/productization/local-asr.md: %w", python, backend, detail, err)
	}
	return nil
}
