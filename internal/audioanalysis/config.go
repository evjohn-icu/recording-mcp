package audioanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ConfigSchema identifies the audio-analysis-config-v1 document.
const ConfigSchema = "audio-analysis-config-v1"

// Config is the process-wide audio analysis configuration. Every capability is
// opt-in; a missing or unverified model disables only that capability.
type Config struct {
	SchemaVersion      string                      `json:"schema_version"`
	Python             string                      `json:"python"`
	Runner             string                      `json:"runner"`
	GPULock            string                      `json:"gpu_lock"`
	LockTimeoutSeconds int                         `json:"lock_timeout_seconds"`
	LoadTimeoutSeconds int                         `json:"load_timeout_seconds"`
	RunTimeoutSeconds  int                         `json:"run_timeout_seconds"`
	Capabilities       map[string]CapabilityConfig `json:"capabilities"`
	// BaseDir resolves relative model and runner paths. It is not part of the
	// JSON document.
	BaseDir string `json:"-"`
}

// CapabilityConfig pins one capability's model and frontend.
type CapabilityConfig struct {
	Enabled       bool        `json:"enabled"`
	ModelID       string      `json:"model_id"`
	ModelRevision string      `json:"model_revision"`
	Files         []ModelFile `json:"files"`
	Device        string      `json:"device"`
	FrontendID    string      `json:"frontend_id"`
}

// ModelFile pins one local model file.
type ModelFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Resolved is an enabled, fully verified capability.
type Resolved struct {
	Capability    string
	ModelID       string
	ModelRevision string
	Files         []ModelFile
	Device        string
	FrontendID    string
	ConfigSHA256  string
}

// Defaults for optional timeouts.
const (
	DefaultLockTimeout = 600 * time.Second
	DefaultLoadTimeout = 300 * time.Second
	DefaultRunTimeout  = 7200 * time.Second
)

// LoadConfig reads and structurally validates the configuration. Model files
// are verified by Resolve so one broken capability does not prevent the
// application from starting.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("audioanalysis: read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("audioanalysis: decode config: %w", err)
	}
	if cfg.SchemaVersion != ConfigSchema {
		return nil, fmt.Errorf("audioanalysis: config schema must be %s", ConfigSchema)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.BaseDir = filepath.Dir(abs)
	if cfg.Python == "" {
		cfg.Python = "python3"
	}
	if cfg.Runner == "" {
		cfg.Runner = "scripts/audio-analysis.py"
	}
	if !filepath.IsAbs(cfg.Runner) {
		cfg.Runner = filepath.Join(cfg.BaseDir, cfg.Runner)
	}
	return &cfg, nil
}

// Timeouts returns the effective lock/load/run timeouts.
func (c *Config) Timeouts() (lock, load, run time.Duration) {
	lock = DefaultLockTimeout
	if c != nil && c.LockTimeoutSeconds > 0 {
		lock = time.Duration(c.LockTimeoutSeconds) * time.Second
	}
	load = DefaultLoadTimeout
	if c != nil && c.LoadTimeoutSeconds > 0 {
		load = time.Duration(c.LoadTimeoutSeconds) * time.Second
	}
	run = DefaultRunTimeout
	if c != nil && c.RunTimeoutSeconds > 0 {
		run = time.Duration(c.RunTimeoutSeconds) * time.Second
	}
	return lock, load, run
}

// modelHashCache avoids re-hashing a multi-hundred-megabyte model on every job.
var modelHashCache = struct {
	sync.Mutex
	entries map[string]string
}{entries: map[string]string{}}

// Resolve returns the verified capability configuration or a typed error:
// audio_disabled when the capability is off, audio_model_unavailable when a
// file is missing or its hash does not match.
func (c *Config) Resolve(capability string) (Resolved, error) {
	if c == nil || c.Capabilities == nil {
		return Resolved{}, NewError(CodeDisabled, false, "audio analysis is not configured")
	}
	entry, ok := c.Capabilities[capability]
	if !ok || !entry.Enabled {
		return Resolved{}, NewError(CodeDisabled, false, "capability is not enabled")
	}
	if entry.ModelID == "" || entry.ModelRevision == "" {
		return Resolved{}, NewError(CodeModelUnavailable, false, "model_id and model_revision are required")
	}
	if len(entry.Files) == 0 {
		return Resolved{}, NewError(CodeModelUnavailable, false, "no model file is pinned")
	}
	verified := make([]ModelFile, 0, len(entry.Files))
	canonical := make([]string, 0, len(entry.Files))
	for _, file := range entry.Files {
		path := file.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.BaseDir, path)
		}
		hash, err := verifyModelFile(path, file.SHA256)
		if err != nil {
			return Resolved{}, err
		}
		verified = append(verified, ModelFile{Path: path, SHA256: hash})
		canonical = append(canonical, filepath.Base(path)+"="+hash)
	}
	device := strings.TrimSpace(entry.Device)
	if device == "" {
		device = "cpu"
	}
	configHash := sha256Hex(strings.Join([]string{capability, entry.ModelID, entry.ModelRevision, device, entry.FrontendID, strings.Join(canonical, ",")}, "\x00"))
	return Resolved{
		Capability:    capability,
		ModelID:       entry.ModelID,
		ModelRevision: entry.ModelRevision,
		Files:         verified,
		Device:        device,
		FrontendID:    entry.FrontendID,
		ConfigSHA256:  configHash,
	}, nil
}

// verifyModelFile enforces existence, regular-file shape and a 64-hex SHA-256
// match. The hash is cached by path, size and modification time.
func verifyModelFile(path, expected string) (string, error) {
	if len(expected) != 64 {
		return "", NewError(CodeModelUnavailable, false, "model file hash must be 64 hex characters")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", NewError(CodeModelUnavailable, false, "model file hash must be 64 hex characters")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", NewError(CodeModelUnavailable, false, "model file is missing")
	}
	key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	modelHashCache.Lock()
	if hash, ok := modelHashCache.entries[key]; ok {
		modelHashCache.Unlock()
		if !strings.EqualFold(hash, expected) {
			return "", NewError(CodeModelUnavailable, false, "model file hash does not match the pin")
		}
		return hash, nil
	}
	modelHashCache.Unlock()

	file, err := os.Open(path)
	if err != nil {
		return "", NewError(CodeModelUnavailable, false, "model file is unreadable")
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return "", NewError(CodeModelUnavailable, false, "model file is unreadable")
	}
	actual := hex.EncodeToString(h.Sum(nil))
	modelHashCache.Lock()
	modelHashCache.entries[key] = actual
	modelHashCache.Unlock()
	if !strings.EqualFold(actual, expected) {
		return "", NewError(CodeModelUnavailable, false, "model file hash does not match the pin")
	}
	return actual, nil
}

// sha256Hex hashes a string.
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ErrorCodeOf returns the stable code of a typed error, or "".
func ErrorCodeOf(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

// IsModelUnavailable reports a typed model/disabled error.
func IsModelUnavailable(err error) bool {
	var typed *Error
	if !errors.As(err, &typed) {
		return false
	}
	return typed.Code == CodeModelUnavailable || typed.Code == CodeDisabled
}
