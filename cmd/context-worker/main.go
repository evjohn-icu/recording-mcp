// Command context-worker runs processing jobs from the SQLite queue with a
// synthetic executor. It is the first end-to-end consumer of internal/process
// and internal/worker.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/decision"
	"recordingmcp/internal/generate"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/migrations"
	"recordingmcp/internal/pipeline"
	"recordingmcp/internal/process"
	"recordingmcp/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "context-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "", "path to the SQLite database (required)")
	workerID := flag.String("worker-id", defaultWorkerID(), "worker identity recorded on claims")
	stages := flag.String("stages", "extract,publish,curate", "comma-separated stage names to claim")
	lease := flag.Duration("lease", 30*time.Second, "lease duration held while executing a job")
	poll := flag.Duration("poll", time.Second, "idle poll interval")
	once := flag.Bool("once", false, "claim and process at most one job, then exit")
	executor := flag.String("executor", "synthetic", "executor implementation: synthetic|pipeline")
	decisionURL := flag.String("decision-url", "http://127.0.0.1:8765/v1/systemone", "decision service endpoint used by the pipeline executor")
	decisionModel := flag.String("decision-model", "jev-style-2b-v2", "decision model name used by the pipeline executor")
	generatorName := flag.String("generator", "synthetic", "candidate generator used by the pipeline executor: synthetic|openai")
	generatorURL := flag.String("generator-url", "", "OpenAI-compatible chat completions endpoint used by the openai generator")
	generatorModel := flag.String("generator-model", "glm-5.3-flash", "generator model name")
	generatorKeyEnv := flag.String("generator-key-env", "GENERATOR_API_KEY", "environment variable holding the generator API key")
	generatorTimeout := flag.Duration("generator-timeout", 180*time.Second, "HTTP timeout for the openai generator")
	curatorCommand := flag.String("curator-command", "", "shell command running one curation pass; it must write proposals to $CONTEXT_PROPOSALS and may read $CONTEXT_DB. Empty completes curate jobs as skipped")
	curatorTimeout := flag.Duration("curator-timeout", 15*time.Minute, "timeout for one curator command run")
	curatorModel := flag.String("curator-model", "curator", "model identifier recorded on memory links written by the curate stage")
	syntheticDelay := flag.Duration("synthetic-delay", 0, "artificial delay for the synthetic executor")
	flag.Parse()

	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("configure database: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := migrations.Apply(ctx, db); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	store, err := memory.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	defer store.Close()

	gen, err := buildGenerator(*generatorName, *generatorURL, *generatorModel, *generatorKeyEnv, *generatorTimeout)
	if err != nil {
		return err
	}

	absDBPath, err := filepath.Abs(*dbPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	inner, err := buildExecutor(*executor, db, store, *decisionURL, *decisionModel, gen, *syntheticDelay, absDBPath, *curatorCommand, *curatorModel, *curatorTimeout)
	if err != nil {
		return err
	}

	cfg := worker.Config{
		DB:               db,
		WorkerID:         *workerID,
		Stages:           parseStages(*stages),
		Lease:            *lease,
		ProcessorVersion: "worker-v1",
		PollInterval:     *poll,
		Executor: &loggingExecutor{
			logger: logger,
			inner:  inner,
		},
	}

	if *once {
		ran, err := worker.RunOnce(ctx, cfg)
		if err != nil {
			return err
		}
		if !ran {
			logger.Print("no job available")
		}
		return nil
	}
	logger.Printf("worker %s started stages=%v lease=%s poll=%s", cfg.WorkerID, cfg.Stages, cfg.Lease, cfg.PollInterval)
	return worker.Run(ctx, cfg)
}

// buildExecutor selects the executor implementation named by --executor. The
// pipeline executor routes extract, publish and curate jobs to their stage
// handlers; the curate handler runs the configured curator command and stores
// the proposals it produces.
func buildExecutor(name string, db *sql.DB, store *memory.Store, decisionURL, decisionModel string, gen generate.Generator, syntheticDelay time.Duration, dbPath, curatorCommand, curatorModel string, curatorTimeout time.Duration) (worker.Executor, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "synthetic":
		return worker.Synthetic{Delay: syntheticDelay}, nil
	case "pipeline":
		return &stageExecutor{stages: map[string]worker.Executor{
			"extract": &pipeline.ExtractHandler{
				DB:               db,
				Engine:           &decision.SystemOneClient{Endpoint: decisionURL, Model: decisionModel},
				Endpoint:         decisionURL,
				ProcessorVersion: "extract-v1",
				Generator:        gen,
			},
			"publish": &pipeline.PublishHandler{
				DB:               db,
				Store:            store,
				ProcessorVersion: "publish-v1",
			},
			"curate": &pipeline.CurateHandler{
				DB:               db,
				DBPath:           dbPath,
				Command:          curatorCommand,
				Timeout:          curatorTimeout,
				Model:            curatorModel,
				ProcessorVersion: "curate-v1",
			},
		}}, nil
	default:
		return nil, fmt.Errorf("unknown executor %q (want synthetic or pipeline)", name)
	}
}

// buildGenerator selects the candidate generator named by --generator. The
// openai generator reads its API key from the environment variable named by
// --generator-key-env at startup and applies --generator-timeout to its HTTP
// client.
func buildGenerator(name, endpoint, model, keyEnv string, timeout time.Duration) (generate.Generator, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "synthetic":
		return generate.Synthetic{}, nil
	case "openai":
		if strings.TrimSpace(endpoint) == "" {
			return nil, fmt.Errorf("--generator-url is required for the openai generator")
		}
		apiKey := ""
		if strings.TrimSpace(keyEnv) != "" {
			apiKey = os.Getenv(keyEnv)
		}
		return &generate.OpenAICompat{
			Endpoint:   endpoint,
			APIKey:     apiKey,
			Model:      model,
			HTTPClient: &http.Client{Timeout: timeout},
		}, nil
	default:
		return nil, fmt.Errorf("unknown generator %q (want synthetic or openai)", name)
	}
}

// stageExecutor dispatches each claimed job to the executor registered for its
// stage, so a single worker can serve both extract and publish jobs.
type stageExecutor struct {
	stages map[string]worker.Executor
}

func (s *stageExecutor) Execute(ctx context.Context, job process.Job) (string, error) {
	exec, ok := s.stages[job.Stage]
	if !ok {
		return "", fmt.Errorf("context-worker: no executor for stage %q", job.Stage)
	}
	return exec.Execute(ctx, job)
}

func parseStages(raw string) []string {
	parts := strings.Split(raw, ",")
	stages := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			stages = append(stages, trimmed)
		}
	}
	return stages
}

func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return host + "-" + hex.EncodeToString(buf)
}

// loggingExecutor writes one line per job attempt to stderr so stdout stays
// clean for other consumers.
type loggingExecutor struct {
	logger *log.Logger
	inner  worker.Executor
}

func (l *loggingExecutor) Execute(ctx context.Context, job process.Job) (string, error) {
	l.logger.Printf("job=%s stage=%s attempt=%d/%d worker=%s start", job.ID, job.Stage, job.AttemptCount, job.MaxAttempts, job.WorkerID)
	resultRef, err := l.inner.Execute(ctx, job)
	if err != nil {
		l.logger.Printf("job=%s stage=%s failed: %v", job.ID, job.Stage, err)
		return resultRef, err
	}
	l.logger.Printf("job=%s stage=%s done result=%s", job.ID, job.Stage, resultRef)
	return resultRef, nil
}
