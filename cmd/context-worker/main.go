// Command context-worker runs processing jobs from the SQLite queue with the
// real pipeline executor by default. It is the first end-to-end consumer of
// internal/process and internal/worker.
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

	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/generate"
	"recordingmcp/internal/ingest"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/migrations"
	"recordingmcp/internal/pipeline"
	"recordingmcp/internal/process"
	"recordingmcp/internal/profile"
	"recordingmcp/internal/semantic"
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
	profileCLI := profile.RegisterCLI(flag.CommandLine)
	workerID := flag.String("worker-id", defaultWorkerID(), "worker identity recorded on claims")
	stages := flag.String("stages", "ingest,asr,extract,publish,curate,embed", "comma-separated stage names to claim")
	worldlineEventsDB := flag.String("worldline-events-db", "", "explicit event-plane path required for worldline jobs using a legacy database")
	worldlineDecisionURL := flag.String("worldline-decision-url", "", "loopback JEV endpoint used for Worldline private-source relation analysis")
	worldlineDecisionModel := flag.String("worldline-decision-model", "jev-style-2b-v2", "JEV model for Worldline relation analysis")
	lease := flag.Duration("lease", 30*time.Second, "lease duration held while executing a job")
	poll := flag.Duration("poll", time.Second, "idle poll interval")
	once := flag.Bool("once", false, "claim and process at most one job, then exit")
	executor := flag.String("executor", "pipeline", "executor implementation: pipeline|synthetic")
	allowSynthetic := flag.Bool("allow-synthetic", false, "permit the synthetic generator (placeholder candidates); demos and tests only")
	decisionURL := flag.String("decision-url", "", "decision service endpoint used by the pipeline executor; empty = skip Decision evaluation, extract still distills")
	decisionModel := flag.String("decision-model", "jev-style-2b-v2", "decision model name used by the pipeline executor")
	generatorName := flag.String("generator", "off", "candidate generator used by the pipeline executor: off|synthetic|openai; off = only raw turns are retrievable, no candidates")
	generatorURL := flag.String("generator-url", "", "OpenAI-compatible chat completions endpoint used by the openai generator")
	generatorModel := flag.String("generator-model", "glm-5.3-flash", "generator model name")
	generatorTemperature := flag.Float64("generator-temperature", -1, "sampling temperature sent to the generator; negative = omit the field from the request")
	generatorKeyEnv := flag.String("generator-key-env", "GENERATOR_API_KEY", "environment variable holding the generator API key")
	generatorTimeout := flag.Duration("generator-timeout", 180*time.Second, "HTTP timeout for the openai generator")
	curatorCommand := flag.String("curator-command", "", "shell command running one curation pass; it must write proposals to $CONTEXT_PROPOSALS and may read $CONTEXT_DB. Empty completes curate jobs as skipped")
	curatorTimeout := flag.Duration("curator-timeout", 15*time.Minute, "timeout for one curator command run")
	curatorModel := flag.String("curator-model", "curator", "model identifier recorded on memory links written by the curate stage")
	syntheticDelay := flag.Duration("synthetic-delay", 0, "artificial delay for the synthetic executor")
	embedProvider := flag.String("embed-provider", "hash", "embedding provider for the embed stage: hash|openai|off; must match the query side (bin/context-memory) or search falls back to literal matching")
	embedDims := flag.Int("embed-dims", 256, "embedding dimension count for the embed provider")
	embedEndpoint := flag.String("embed-endpoint", "", "OpenAI-compatible embeddings endpoint used by the openai embed provider")
	embedModel := flag.String("embed-model", "", "embedding model name used by the openai embed provider")
	embedKeyEnv := flag.String("embed-key-env", "EMBED_API_KEY", "environment variable holding the embedding API key")
	embedInputType := flag.Bool("embed-input-type", false, "send the non-standard input_type field (needed by the bundled Qwen embedding server)")
	asrProvider := flag.String("asr-provider", "off", "ASR provider for the asr stage: off|fixture|doubao|ark|qwen|local")
	asrKeyEnv := flag.String("asr-key-env", "VOLC_API_KEY", "environment variable holding the doubao ASR API key")
	asrEndpoint := flag.String("asr-endpoint", "", "doubao ASR endpoint override")
	asrResourceID := flag.String("asr-resource-id", "", "doubao ASR resource id")
	asrModel := flag.String("asr-model", "", "ASR model identifier; local backends default to Edge0/ARK-ASR-0.6B or Qwen/Qwen3-ASR-1.7B")
	asrRevision := flag.String("asr-revision", "", "Hugging Face revision for local ASR; empty pins the verified snapshot of the built-in model")
	asrSegmentMS := flag.Int64("asr-segment-ms", 300000, "doubao window length in milliseconds")
	asrOverlapMS := flag.Int64("asr-overlap-ms", 2000, "ASR window overlap in milliseconds")
	asrConcurrency := flag.Int("asr-concurrency", 2, "maximum concurrent doubao recognition sessions")
	fixtureWindowMS := flag.Int64("fixture-window-ms", 5000, "fixture transcriber window length in milliseconds")
	asrBackend := flag.String("asr-backend", "ark", "local ASR model family when --asr-provider local: ark|qwen")
	asrPython := flag.String("asr-python", "", "Python interpreter for local ASR; empty auto-detects ./.venv-asr/bin/python, then python3")
	asrScript := flag.String("asr-script", "scripts/local-asr.py", "offline ASR runner script")
	asrDtype := flag.String("asr-dtype", "float16", "local ASR compute dtype (float16 only)")
	asrDevice := flag.String("asr-device", "cuda:0", "local ASR device, e.g. cuda:0 or cpu")
	asrLanguage := flag.String("asr-language", "", "optional local ASR language hint, e.g. Chinese")
	asrWindowMS := flag.Int64("asr-window-ms", 30000, "local ASR window length in milliseconds")
	asrMaxNewTokens := flag.Int("asr-max-new-tokens", 512, "local ASR generation limit per window")
	asrTimeout := flag.Duration("asr-timeout", 2*time.Hour, "total budget for one local ASR recording; zero means two hours")
	asrLoadTimeout := flag.Duration("asr-load-timeout", 5*time.Minute, "budget for loading a local ASR model")
	asrWindowTimeout := flag.Duration("asr-window-timeout", 5*time.Minute, "budget for one local ASR window")
	asrLock := flag.String("asr-lock", "", "exclusive lock file serializing local GPU inference across processes")
	asrLockTimeout := flag.Duration("asr-lock-timeout", 10*time.Minute, "how long to wait for the local ASR GPU lock")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary used by the ingest stage")
	ffprobePath := flag.String("ffprobe", "ffprobe", "ffprobe binary used by the ingest stage")
	audioDir := flag.String("audio-dir", "", "audio directory (legacy mode; profile mode uses the profile audio directory)")
	healthAddr := flag.String("health-addr", "", "address for the worker health endpoint; empty disables it")
	flag.Parse()

	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	resolvedDB, resolvedAudio, err := profileCLI.Resolve(*dbPath, *audioDir)
	if err != nil {
		return err
	}
	if *dbPath == "" {
		*dbPath = resolvedDB
	}
	if *audioDir == "" {
		*audioDir = resolvedAudio
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)

	db, err := sql.Open("sqlite", workerDSN(*dbPath))
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

	var generatorTemp *float64
	if *generatorTemperature >= 0 {
		generatorTemp = generatorTemperature
	}
	gen, err := buildGenerator(*generatorName, *generatorURL, *generatorModel, *generatorKeyEnv, generatorTemp, *generatorTimeout)
	if err != nil {
		return err
	}

	executorName := strings.ToLower(strings.TrimSpace(*executor))
	generatorChoice := strings.ToLower(strings.TrimSpace(*generatorName))
	if err := validateRealMode(executorName, generatorChoice, *allowSynthetic); err != nil {
		return err
	}
	if executorName == "synthetic" {
		fmt.Fprintln(os.Stderr, "context-worker: synthetic executor acknowledges jobs without real processing (demo/test only)")
		fmt.Fprintln(os.Stderr, "context-worker: embed jobs are acknowledged without indexing — get_processing_status.indexing_status stays pending")
	}

	absDBPath, err := filepath.Abs(*dbPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	if strings.TrimSpace(*audioDir) == "" {
		*audioDir = filepath.Join(filepath.Dir(absDBPath), "synced-audio")
	}
	transcriber, selectErr := cloudsync.SelectTranscriber(cloudsync.TranscriberConfig{
		Provider: *asrProvider, KeyEnv: *asrKeyEnv, Endpoint: *asrEndpoint,
		ResourceID: *asrResourceID, Model: *asrModel, Revision: *asrRevision,
		SegmentMS: *asrSegmentMS, OverlapMS: *asrOverlapMS, Concurrency: *asrConcurrency,
		FixtureWindowMS: *fixtureWindowMS,
		LocalBackend:    *asrBackend, Python: *asrPython, Script: *asrScript,
		Dtype: *asrDtype, Device: *asrDevice, Language: *asrLanguage,
		WindowMS: *asrWindowMS, MaxNewTokens: *asrMaxNewTokens, ProbePython: true,
		Timeout: *asrTimeout, LoadTimeout: *asrLoadTimeout, WindowTimeout: *asrWindowTimeout,
		LockPath: *asrLock, LockTimeout: *asrLockTimeout,
	})
	// A broken local runtime must not stop the worker: other stages stay
	// usable and RunASRJob reports asr_unavailable with an action.
	localASR := cloudsync.IsLocalProvider(*asrProvider)
	asrInitError := ""
	if selectErr != nil {
		if localASR {
			logger.Printf("context-worker: ASR unavailable: %v", selectErr)
			asrInitError = "model_probe_failed: " + selectErr.Error()
			transcriber = nil
		} else {
			return selectErr
		}
	}
	if transcriber != nil && localASR {
		// Probe the interpreter the transcriber actually resolved; an empty
		// --asr-python defaults to .venv-asr, not the system python3.
		probePython := strings.TrimSpace(*asrPython)
		if probePython == "" {
			if local, ok := transcriber.(*cloudsync.LocalTranscriber); ok {
				probePython = local.Python
			}
		}
		if probeErr := cloudsync.ProbeLocalCUDA(probePython, *asrDevice); probeErr != nil {
			logger.Printf("context-worker: ASR unavailable: %v", probeErr)
			asrInitError = "gpu_unavailable: " + probeErr.Error()
			// Do not hand ASR jobs to a device this worker declares unusable;
			// RunASRJob's asr_unavailable path is the correct action.
			transcriber = nil
		}
	}
	importer, err := ingest.New(ingest.Config{
		Store: store, DB: db, AudioDir: *audioDir,
		Decoder:       &ingest.FFmpegDecoder{FFmpeg: *ffmpegPath, FFprobe: *ffprobePath},
		Transcription: transcriber,
		ASRProvider:   *asrProvider, ASRModel: *asrModel, ASRResourceID: *asrResourceID,
		TranscriptRevision: ingest.DefaultTranscriptRevision,
	})
	if err != nil {
		return fmt.Errorf("configure importer: %w", err)
	}
	embedProviderImpl, err := buildEmbedProvider(*embedProvider, *embedDims, *embedEndpoint, *embedModel, *embedKeyEnv, *embedInputType)
	if err != nil {
		return err
	}
	inner, err := buildExecutor(*executor, db, store, *decisionURL, *decisionModel, gen, *syntheticDelay, absDBPath, *curatorCommand, *curatorModel, *curatorTimeout, embedProviderImpl, importer)
	if err != nil {
		return err
	}
	profileSelected := flag.Lookup("profile") != nil && strings.TrimSpace(flag.Lookup("profile").Value.String()) != ""
	explicitStages := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "stages" {
			explicitStages = true
		}
	})
	jobStages := workerStages(*stages, profileSelected, explicitStages)
	if containsStage(jobStages, "worldline") {
		scope, eventsPath, e := profileCLI.WorldlinePlane(*dbPath, *worldlineEventsDB)
		if e != nil {
			return e
		}
		var worldlineEngine decision.Engine
		if strings.TrimSpace(*worldlineDecisionURL) != "" {
			worldlineEngine, e = decision.NewLocalSystemOneClient(*worldlineDecisionURL, *worldlineDecisionModel)
			if e != nil {
				return fmt.Errorf("worldline decision: %w", e)
			}
		}
		inner = routeWorldlineJobs(inner, &WorldlineJobExecutor{Store: store, Scope: scope, EventsPath: eventsPath, Engine: worldlineEngine})
	}

	cfg := worker.Config{
		DB:               db,
		WorkerID:         *workerID,
		Stages:           jobStages,
		Lease:            *lease,
		ProcessorVersion: "worker-v1",
		PollInterval:     *poll,
		Executor: &loggingExecutor{
			logger: logger,
			inner:  inner,
		},
	}
	gpuName, vramTotal, vramFree := "", 0, 0
	if localASR {
		gpuName, vramTotal, vramFree = cloudsync.GPUInfo()
	}
	heartbeat := process.Heartbeat{
		WorkerID:         cfg.WorkerID,
		PID:              os.Getpid(),
		Stages:           cfg.Stages,
		GeneratorEnabled: gen != nil,
		ASRProvider:      strings.TrimSpace(*asrProvider),
		ASRAvailable:     transcriber != nil && containsStage(jobStages, "asr") && asrInitError == "",
		GPUName:          gpuName,
		VRAMTotalMB:      vramTotal,
		VRAMFreeMB:       vramFree,
		ASRDevice:        strings.TrimSpace(*asrDevice),
		ASRDtype:         strings.TrimSpace(*asrDtype),
		ASRError:         asrInitError,
		EmbedProvider:    strings.TrimSpace(*embedProvider),
	}
	if err := process.WriteHeartbeat(ctx, db, heartbeat, time.Now()); err != nil {
		return fmt.Errorf("write worker heartbeat: %w", err)
	}
	defer func() {
		if err := process.RemoveHeartbeat(context.Background(), db, cfg.WorkerID); err != nil {
			logger.Printf("remove worker heartbeat failed: %v", err)
		}
	}()

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
	if addr := strings.TrimSpace(*healthAddr); addr != "" {
		healthServer := &http.Server{
			Addr:              addr,
			Handler:           workerHealthHandler(db, cfg.WorkerID),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = healthServer.Shutdown(shutdown)
		}()
		go func() {
			if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Printf("worker health server failed: %v", err)
			}
		}()
		logger.Printf("worker health endpoint listening on %s", addr)
	}
	logger.Printf("worker %s started stages=%v lease=%s poll=%s generator_enabled=%t", cfg.WorkerID, cfg.Stages, cfg.Lease, cfg.PollInterval, gen != nil)
	beatCtx, cancelBeat := context.WithCancel(ctx)
	defer cancelBeat()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case now := <-ticker.C:
				if err := process.WriteHeartbeat(beatCtx, db, heartbeat, now); err != nil {
					logger.Printf("worker heartbeat failed: %v", err)
				}
			}
		}
	}()
	return worker.Run(ctx, cfg)
}

// workerDSN starts explicit transactions with BEGIN IMMEDIATE so two worker
// processes cannot deadlock on a read-then-write upgrade under WAL.
func workerDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_txlock=immediate"
}

// buildExecutor selects the executor implementation named by --executor. The
// pipeline executor routes extract, publish, curate and embed jobs to their
// stage handlers; the curate handler runs the configured curator command and
// stores the proposals it produces.
func buildExecutor(name string, db *sql.DB, store *memory.Store, decisionURL, decisionModel string, gen generate.Generator, syntheticDelay time.Duration, dbPath, curatorCommand, curatorModel string, curatorTimeout time.Duration, embedProvider semantic.Provider, importer *ingest.Service) (worker.Executor, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "synthetic":
		return worker.Synthetic{Delay: syntheticDelay}, nil
	case "pipeline":
		// Decision is optional: an empty URL runs extraction without any
		// recorded decision runs instead of calling a service that is not up.
		var engine decision.Engine
		if strings.TrimSpace(decisionURL) != "" {
			engine = &decision.SystemOneClient{
				Endpoint:   decisionURL,
				Model:      decisionModel,
				HTTPClient: &http.Client{Timeout: 30 * time.Second},
			}
		}
		return &stageExecutor{stages: map[string]worker.Executor{
			"ingest": &ingestStage{svc: importer, stage: "ingest"},
			"asr":    &ingestStage{svc: importer, stage: "asr"},
			"extract": &pipeline.ExtractHandler{
				DB:               db,
				Engine:           engine,
				Endpoint:         decisionURL,
				ProcessorVersion: "extract-v1",
				Generator:        gen,
			},
			"embed": &pipeline.EmbedHandler{
				DB:       db,
				Provider: embedProvider,
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
// client. "off" (or empty) returns a nil generator: extraction records
// coverage without proposing candidates, so only raw turns are retrievable.
func buildGenerator(name, endpoint, model, keyEnv string, temperature *float64, timeout time.Duration) (generate.Generator, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "off":
		return nil, nil
	case "synthetic":
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
			Endpoint:    endpoint,
			APIKey:      apiKey,
			Model:       model,
			Temperature: temperature,
			HTTPClient:  &http.Client{Timeout: timeout},
		}, nil
	default:
		return nil, fmt.Errorf("unknown generator %q (want off, synthetic or openai)", name)
	}
}

// validateRealMode guards against silently running placeholder analysis: the
// synthetic generator fabricates candidates that look analysed but are not.
// It is allowed only with --allow-synthetic, which is meant for demos and
// tests.
func validateRealMode(executor, generator string, allowSynthetic bool) error {
	if executor == "pipeline" && generator == "synthetic" && !allowSynthetic {
		return fmt.Errorf("--generator synthetic produces placeholder candidates; pass --allow-synthetic for demos and tests only")
	}
	return nil
}

// buildEmbedProvider selects the embedding provider named by --embed-provider.
// hash is the offline default so indexing never needs the network; openai
// requires a configured endpoint and model; off disables indexing (embed jobs
// succeed as skipped and the status surface reports indexing disabled).
func buildEmbedProvider(name string, dims int, endpoint, model, keyEnv string, inputType bool) (semantic.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "hash":
		return semantic.NewHashProvider(dims), nil
	case "off":
		return nil, nil
	case "openai":
		if strings.TrimSpace(endpoint) == "" {
			return nil, fmt.Errorf("--embed-endpoint is required for the openai embed provider")
		}
		if strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("--embed-model is required for the openai embed provider")
		}
		if dims <= 0 {
			return nil, fmt.Errorf("--embed-dims must be positive for the openai embed provider")
		}
		apiKey := ""
		if strings.TrimSpace(keyEnv) != "" {
			apiKey = os.Getenv(keyEnv)
		}
		return &semantic.OpenAICompatProvider{
			Endpoint:      endpoint,
			APIKey:        apiKey,
			ModelName:     model,
			Dimensions:    dims,
			HTTPClient:    &http.Client{Timeout: 60 * time.Second},
			SendInputType: inputType,
		}, nil
	default:
		return nil, fmt.Errorf("unknown embed provider %q (want hash, openai or off)", name)
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

// ingestStage dispatches the two importer-owned stages.
type ingestStage struct {
	svc   *ingest.Service
	stage string
}

func (i *ingestStage) Execute(ctx context.Context, job process.Job) (string, error) {
	if i.svc == nil {
		return "", fmt.Errorf("context-worker: import service is not configured")
	}
	if i.stage == "ingest" {
		return i.svc.RunIngestJob(ctx, job)
	}
	return i.svc.RunASRJob(ctx, job)
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

func containsStage(stages []string, want string) bool {
	for _, stage := range stages {
		if stage == want {
			return true
		}
	}
	return false
}

func workerStages(raw string, profileSelected, explicitStages bool) []string {
	stages := parseStages(raw)
	if profileSelected && !explicitStages && !containsStage(stages, "worldline") {
		stages = append(stages, "worldline")
	}
	return stages
}

// workerHealthHandler reports worker liveness: a healthy response requires the
// database to answer and this worker's own heartbeat to be at most 90s old.
func workerHealthHandler(db *sql.DB, workerID string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"degraded","database":"unavailable","worker":"%s"}`, workerID)
			return
		}
		var stamp int64
		err := db.QueryRowContext(ctx, `SELECT last_seen_at FROM worker_heartbeats WHERE worker_id=?`, workerID).Scan(&stamp)
		if err != nil || time.Since(time.UnixMilli(stamp)) > 90*time.Second {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"degraded","database":"ok","worker":"stale"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","database":"ok","worker":"%s"}`, workerID)
	})
	return mux
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
