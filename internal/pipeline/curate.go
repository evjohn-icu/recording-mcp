package pipeline

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"recordingmcp/internal/process"
)

// defaultCuratorTimeout bounds one external curator run. The curator is an
// agent that reads the memory library through MCP and writes a proposal file,
// so a single run is minutes, not milliseconds.
const defaultCuratorTimeout = 15 * time.Minute

// maxCuratorOutputBytes caps how much of the curator command's output is kept
// for the job error message.
const maxCuratorOutputBytes = 4 << 10

// CurateHandler is the curate-stage executor: after a publish commits, it runs
// the configured curator command once and then applies the same validation
// rules as cmd/context-associate to the proposal file that run produced.
//
// The handler never reads memory on the curator's behalf. The curator receives
// the database path only so its MCP client can point at the same file, and it
// writes proposals to a path the handler chooses; validation and writing stay
// in this process, so an agent cannot bypass the evidence rules.
type CurateHandler struct {
	DB               *sql.DB
	DBPath           string        // absolute database path, handed to the command as CONTEXT_DB
	Command          string        // shell command; empty means curation is skipped
	Timeout          time.Duration // 0 uses defaultCuratorTimeout
	Model            string        // recorded in memory_links.model
	ProcessorVersion string
}

// Execute implements worker.Executor. It reads conversation_id and
// annotation_version from the job payload, runs the curator and stores the
// accepted proposals. An unconfigured command is not an error: the job
// completes with a skipped result so operators can see why nothing ran.
func (h *CurateHandler) Execute(ctx context.Context, job process.Job) (string, error) {
	if h.DB == nil {
		return "", errors.New("pipeline: CurateHandler.DB is required")
	}
	var payload struct {
		ConversationID    string `json:"conversation_id"`
		AnnotationVersion string `json:"annotation_version"`
	}
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return "", fmt.Errorf("pipeline: decode curate payload: %w", err)
	}
	if payload.ConversationID == "" {
		return "", errors.New("pipeline: curate payload.conversation_id is required")
	}
	version := payload.AnnotationVersion
	if version == "" {
		version = "-"
	}
	if strings.TrimSpace(h.Command) == "" {
		return "curate:" + version + "+skipped:no-command", nil
	}

	output, workDir, err := h.runCurator(ctx, payload.ConversationID, payload.AnnotationVersion)
	if workDir != "" {
		defer os.RemoveAll(workDir)
	}
	if err != nil {
		return "", err
	}

	proposals, err := DecodeProposalFile(filepath.Join(workDir, "proposals.json"))
	if err != nil {
		return "", fmt.Errorf("pipeline: read curator proposals: %w (command output: %s)", err, output)
	}
	validated, err := ValidateTopicLinks(ctx, h.DB, proposals)
	if err != nil {
		return "", fmt.Errorf("pipeline: validate curator proposals: %w", err)
	}
	accepted := 0
	for _, v := range validated {
		if v.Status != LinkStatusRejected {
			accepted++
		}
	}
	written := 0
	if accepted > 0 {
		stored, err := InsertLinks(ctx, h.DB, h.Model, validated, time.Now())
		if err != nil {
			return "", fmt.Errorf("pipeline: store curator proposals: %w", err)
		}
		for _, l := range stored {
			switch l.Outcome {
			case LinkInserted, LinkRefreshed:
				written++
			}
		}
	}
	rejected := len(validated) - accepted
	return "curate:" + version + "+proposals:" + strconv.Itoa(accepted) +
		"+rejected:" + strconv.Itoa(rejected) + "+written:" + strconv.Itoa(written), nil
}

// runCurator executes the configured command in a scratch directory with
// CONTEXT_DB, CONTEXT_PROPOSALS, CONTEXT_CURATE_CONVERSATION and
// CONTEXT_CURATE_VERSION in the environment. It returns the capped combined
// output plus the scratch directory (empty only when the directory could not
// be created); the caller removes that directory and reads proposals.json
// from it, so cleanup never guesses a path.
func (h *CurateHandler) runCurator(ctx context.Context, conversationID, version string) (string, string, error) {
	dir, err := os.MkdirTemp("", "context-curate-")
	if err != nil {
		return "", "", fmt.Errorf("pipeline: create curator work dir: %w", err)
	}
	proposalsPath := filepath.Join(dir, "proposals.json")
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultCuratorTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", h.Command)
	cmd.Env = append(os.Environ(),
		"CONTEXT_DB="+h.DBPath,
		"CONTEXT_PROPOSALS="+proposalsPath,
		"CONTEXT_CURATE_CONVERSATION="+conversationID,
		"CONTEXT_CURATE_VERSION="+version,
	)
	out := &cappedBuffer{limit: maxCuratorOutputBytes}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return out.String(), dir, fmt.Errorf("pipeline: curator command timed out after %s (output: %s)", timeout, out.String())
		}
		return out.String(), dir, fmt.Errorf("pipeline: curator command failed: %w (output: %s)", err, out.String())
	}
	return out.String(), dir, nil
}

// enqueueCurate schedules the curate stage for one committed publish version.
// The idempotency key pairs the conversation with the published annotation
// version, so replaying a publish enqueues no second curation while the version
// is unchanged and a new version always gets its own run.
func enqueueCurate(ctx context.Context, db *sql.DB, conversationID, version string) error {
	payload, err := json.Marshal(struct {
		ConversationID    string `json:"conversation_id"`
		AnnotationVersion string `json:"annotation_version"`
	}{ConversationID: conversationID, AnnotationVersion: version})
	if err != nil {
		return fmt.Errorf("pipeline: encode curate payload: %w", err)
	}
	spec := process.Spec{
		Stage:           "curate",
		InputSnapshotID: conversationID,
		IdempotencyKey:  "curate|" + conversationID + "|" + version,
		PayloadJSON:     string(payload),
		MaxAttempts:     3,
	}
	if _, _, err := process.Enqueue(ctx, db, spec, time.Now()); err != nil {
		return fmt.Errorf("pipeline: enqueue curate: %w", err)
	}
	return nil
}

// cappedBuffer keeps at most limit bytes and silently drops the rest, so a
// chatty command cannot exhaust memory while its output is only used for error
// reporting.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.buf.Write(p)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
