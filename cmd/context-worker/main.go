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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"recordingmcp/internal/decision"
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
	stages := flag.String("stages", "extract", "comma-separated stage names to claim")
	lease := flag.Duration("lease", 30*time.Second, "lease duration held while executing a job")
	poll := flag.Duration("poll", time.Second, "idle poll interval")
	once := flag.Bool("once", false, "claim and process at most one job, then exit")
	executor := flag.String("executor", "synthetic", "executor implementation: synthetic|pipeline")
	decisionURL := flag.String("decision-url", "http://127.0.0.1:8765/v1/systemone", "decision service endpoint used by the pipeline executor")
	decisionModel := flag.String("decision-model", "jev-style-2b-v2", "decision model name used by the pipeline executor")
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

	inner, err := buildExecutor(*executor, db, *decisionURL, *decisionModel, *syntheticDelay)
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

// buildExecutor selects the executor implementation named by --executor.
func buildExecutor(name string, db *sql.DB, decisionURL, decisionModel string, syntheticDelay time.Duration) (worker.Executor, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "synthetic":
		return worker.Synthetic{Delay: syntheticDelay}, nil
	case "pipeline":
		return &pipeline.ExtractHandler{
			DB:               db,
			Engine:           &decision.SystemOneClient{Endpoint: decisionURL, Model: decisionModel},
			Endpoint:         decisionURL,
			ProcessorVersion: "extract-v1",
		}, nil
	default:
		return nil, fmt.Errorf("unknown executor %q (want synthetic or pipeline)", name)
	}
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
