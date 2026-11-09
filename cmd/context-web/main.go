// Command context-web serves the local memory browser: a JSON API over the
// shared internal/app service plus the built frontend. It is loopback-only by
// default and authenticates one browser with a single-use startup token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/contextbrief"
	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/ingest"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/profile"
	"recordingmcp/internal/semantic"
	"recordingmcp/internal/web"
)

// repeatedFlag collects a flag that may be given several times.
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func validateListenAddress(addr string, containerBind bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("context-web: invalid listen address: %w", err)
	}
	if host == "" {
		return errors.New("context-web: listen address must be an explicit host")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	// A container must listen on its own interface to be reachable through a
	// published port. The compose binding keeps the host side on 127.0.0.1;
	// this flag never widens that.
	if containerBind && ip != nil && ip.IsUnspecified() {
		return nil
	}
	return errors.New("context-web: listen address must be loopback (127.0.0.1, ::1, or localhost) unless --container-bind selects 0.0.0.0 or ::")
}

func serveHTTP(addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Minute,
		WriteTimeout: 10 * time.Minute, IdleTimeout: 120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("context-web: shutdown: %v", err)
		}
		return <-serveErr
	}
}

func main() {
	log.SetOutput(os.Stderr)
	dbPath := flag.String("db", "", "SQLite memory database (legacy mode; mutually exclusive with --profile)")
	profileName := flag.String("profile", "", "local password-protected profile name")
	profilesDir := flag.String("profiles-dir", "", "deprecated alias for --profiles-root")
	profilesRoot := flag.String("profiles-root", "", "trusted profile root; without --profile enables profile selection")
	profilePasswordEnv := flag.String("profile-password-env", profile.DefaultPasswordEnv, "environment variable holding the selected profile password")
	addr := flag.String("addr", "127.0.0.1:8788", "listen address (loopback by default)")
	containerBind := flag.Bool("container-bind", false, "allow binding 0.0.0.0/:: inside a container; the host side stays loopback")
	asrProvider := flag.String("asr-provider", "off", "ASR provider for uploads: off|fixture|doubao|ark|qwen|local")
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
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "ffmpeg binary used by uploads")
	ffprobePath := flag.String("ffprobe", "ffprobe", "ffprobe binary used by uploads")
	audioDir := flag.String("audio-dir", "", "cloud-sync audio directory (legacy mode; profile mode uses its private audio directory)")
	webDir := flag.String("web-dir", "web/dist", "built frontend directory")
	useSemanticIndex := flag.Bool("semantic-index", false, "opt in to experimental HNSW; exact search remains default")
	semanticProvider := flag.String("semantic-provider", "hash", "semantic recall backend: off, hash (offline deterministic embedder) or openai")
	semanticDims := flag.Int("semantic-dims", semantic.DefaultHashDims, "embedding dimensions (hash provider; openai provider validates the endpoint against it)")
	semanticEndpoint := flag.String("semantic-endpoint", "", "OpenAI-compatible embeddings endpoint (required when --semantic-provider=openai)")
	semanticModel := flag.String("semantic-model", "", "embedding model name (required when --semantic-provider=openai)")
	semanticKeyEnv := flag.String("semantic-key-env", "", "environment variable holding the embeddings API key (openai only)")
	decisionURL := flag.String("decision-url", "", "optional Decision engine endpoint used to rerank hybrid search items (empty disables reranking)")
	sufficiencyURL := flag.String("sufficiency-url", "", "optional Decision endpoint for v2 evidence sufficiency (empty disables assessment)")
	sufficiencyModel := flag.String("sufficiency-model", "", "model identity for v2 evidence sufficiency")
	decisionModel := flag.String("decision-model", "jev-style-2b-v2", "model name sent to the Decision engine for reranking")
	worldlineURL := flag.String("worldline-decision-url", "", "local loopback system_one endpoint for worldline semantic suggestions")
	worldlineModel := flag.String("worldline-decision-model", "decision-v3-0.8b", "actual local decision model for worldline suggestions")
	var devOrigins repeatedFlag
	flag.Var(&devOrigins, "dev-origin", "additional allowed Origin header value for development (repeatable), e.g. http://127.0.0.1:5173")
	briefPolishEndpoint := flag.String("brief-polish-endpoint", "", "optional chat-completions endpoint; empty disables brief polishing")
	briefPolishModel := flag.String("brief-polish-model", "", "model used only for explicitly consented brief polishing")
	briefPolishKeyEnv := flag.String("brief-polish-key-env", "", "environment variable containing optional brief model API key")
	briefPolishPiProvider := flag.String("brief-polish-pi-provider", "", "explicit Pi provider for brief polishing; never executes key commands")
	briefPolishPiModels := flag.String("brief-polish-pi-models-file", "", "Pi models.json path (only read with --brief-polish-pi-provider)")
	flag.Parse()
	var briefPolisher contextbrief.Polisher
	var briefPolishErr error
	if *briefPolishPiProvider != "" {
		if *briefPolishEndpoint != "" || *briefPolishKeyEnv != "" {
			log.Fatal("brief polish: Pi provider and explicit endpoint/key settings are mutually exclusive")
		}
		briefPolisher, briefPolishErr = configuredPiBriefPolisher(*briefPolishPiModels, *briefPolishPiProvider, *briefPolishModel)
	} else {
		if *briefPolishPiModels != "" {
			log.Fatal("brief polish: Pi models file requires explicit Pi provider")
		}
		briefPolisher, briefPolishErr = configuredBriefPolisher(*briefPolishEndpoint, *briefPolishModel, *briefPolishKeyEnv)
	}
	if briefPolishErr != nil {
		log.Fatal(briefPolishErr)
	}
	worldlineEngine, worldlineErr := localWorldlineDecision(*worldlineURL, *worldlineModel)
	if worldlineErr != nil {
		log.Fatal(worldlineErr)
	}
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}
	if err := validateListenAddress(*addr, *containerBind); err != nil {
		log.Fatal(err)
	}
	transcriber, asrInitErr := cloudsync.SelectTranscriber(cloudsync.TranscriberConfig{
		Provider: *asrProvider, KeyEnv: *asrKeyEnv, Endpoint: *asrEndpoint,
		ResourceID: *asrResourceID, Model: *asrModel, Revision: *asrRevision,
		SegmentMS: *asrSegmentMS, OverlapMS: *asrOverlapMS, Concurrency: *asrConcurrency,
		FixtureWindowMS: *fixtureWindowMS,
		LocalBackend:    *asrBackend, Python: *asrPython, Script: *asrScript,
		Dtype: *asrDtype, Device: *asrDevice, Language: *asrLanguage,
		WindowMS: *asrWindowMS, MaxNewTokens: *asrMaxNewTokens, ProbePython: true,
	})
	asrInitMessage := ""
	if asrInitErr != nil {
		if !cloudsync.IsLocalProvider(*asrProvider) {
			log.Fatal(asrInitErr)
		}
		log.Printf("context-web: ASR unavailable: %v", asrInitErr)
		asrInitMessage = asrInitErr.Error()
		transcriber = nil
	}
	newImporter := func(store *memory.Store, audioDir string) (*ingest.Service, error) {
		return ingest.New(ingest.Config{
			Store: store, DB: store.DB(), AudioDir: audioDir,
			Decoder:       &ingest.FFmpegDecoder{FFmpeg: *ffmpegPath, FFprobe: *ffprobePath},
			Transcription: transcriber,
			ASRProvider:   *asrProvider, ASRModel: *asrModel, ASRResourceID: *asrResourceID,
			TranscriptRevision: ingest.DefaultTranscriptRevision,
		})
	}
	root := strings.TrimSpace(*profilesRoot)
	if root == "" {
		root = strings.TrimSpace(*profilesDir)
	}
	rootWasSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "profiles-root" || f.Name == "profiles-dir" {
			rootWasSet = true
		}
	})
	if strings.TrimSpace(*profileName) != "" && root == "" {
		root = "data/profiles"
	}
	if rootWasSet && strings.TrimSpace(*profileName) == "" {
		if *dbPath != "" || *audioDir != "" {
			log.Fatal("context-web: profile selection cannot be combined with --db or --audio-dir")
		}
		manager, err := profile.Open(root)
		if err != nil {
			log.Fatal(err)
		}
		var storesMu sync.Mutex
		var stores []*memory.Store
		var eventStores []*plane.Service
		factory := func(paths profile.Paths, verifier func(string) bool) (*web.Server, error) {
			store, err := memory.Open(paths.Database)
			if err != nil {
				return nil, err
			}
			storesMu.Lock()
			stores = append(stores, store)
			storesMu.Unlock()
			events, err := plane.Open(paths.Plane)
			if err != nil {
				store.Close()
				return nil, err
			}
			storesMu.Lock()
			eventStores = append(eventStores, events)
			storesMu.Unlock()
			indexPath := ""
			if *useSemanticIndex {
				indexPath = filepath.Join(paths.Index, "semantic.vec")
			}
			retriever, err := semantic.RetrieverFromConfig(semantic.Config{IndexPath: indexPath,
				Provider: *semanticProvider, Dims: *semanticDims, Endpoint: *semanticEndpoint,
				Model: *semanticModel, KeyEnv: *semanticKeyEnv,
			}, store.DB())
			if err != nil {
				events.Close()
				store.Close()
				return nil, err
			}
			var rerank contextbuild.Reranker
			if strings.TrimSpace(*decisionURL) != "" {
				rerank = &contextbuild.DecisionReranker{Engine: &decision.SystemOneClient{Endpoint: strings.TrimSpace(*decisionURL), Model: *decisionModel}}
			}
			svc := app.New(store, retriever, rerank, paths.Audio)
			svc.ASRProvider = *asrProvider
			svc.ASRInitError = asrInitMessage
			svc.BriefPolisher = briefPolisher
			if strings.TrimSpace(*sufficiencyURL) != "" {
				svc.Assessor = &contextbuild.DecisionEvidenceAssessor{Engine: &decision.SystemOneClient{Endpoint: strings.TrimSpace(*sufficiencyURL), Model: *sufficiencyModel}}
			}
			svc.ForgetPlane = events
			svc.ManagedIndexDir = paths.Index
			svc.ManagedBackupDir = paths.Backups
			importer, err := newImporter(store, paths.Audio)
			if err != nil {
				events.Close()
				store.Close()
				return nil, err
			}
			timeSvc := &app.TimeService{Store: store, Events: events}
			worldlineSvc := &app.WorldlineService{Time: timeSvc, DecisionEngine: worldlineEngine}
			return web.New(web.Config{
				Service:           svc,
				TimeService:       timeSvc,
				Worldline:         worldlineSvc,
				DecisionTrail:     &app.DecisionTrailService{Worldline: worldlineSvc, Time: timeSvc},
				WorldlineDecision: worldlineEngine,
				WebDir:            *webDir, DevOrigins: devOrigins, Logger: log.Default(),
				Importer:    importer,
				ProfileName: paths.Name, VerifyProfilePassword: verifier,
				ChangeProfilePassword: func(oldPassword, newPassword string) error {
					return manager.ChangePassword(paths.Name, oldPassword, newPassword)
				},
			})
		}
		profilesServer, err := web.NewProfiles(web.ProfilesConfig{Manager: manager, Factory: factory, WebDir: *webDir, DevOrigins: devOrigins, Logger: log.Default()})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "context-web: profiles-root=%s\n", manager.Root)
		fmt.Fprintf(os.Stderr, "context-web: listening on http://%s\n", *addr)
		if err := serveHTTP(*addr, profilesServer.Handler()); err != nil {
			log.Fatal(err)
		}
		storesMu.Lock()
		defer storesMu.Unlock()
		for _, store := range stores {
			_ = store.Close()
		}
		for _, events := range eventStores {
			_ = events.Close()
		}
		return
	}
	var profileManager *profile.Manager
	planePath := ""
	selectedProfile := strings.TrimSpace(*profileName)
	var verifyProfilePassword func(string) bool
	var changeProfilePassword func(string, string) error
	if selectedProfile != "" {
		if *dbPath != "" || *audioDir != "" {
			log.Fatal("context-web: --profile cannot be combined with --db or --audio-dir")
		}
		password := os.Getenv(*profilePasswordEnv)
		if password == "" {
			log.Fatalf("context-web: set %s to unlock profile %q", *profilePasswordEnv, selectedProfile)
		}
		var err error
		profileManager, err = profile.Open(root)
		if err != nil {
			log.Fatal(err)
		}
		paths, err := profileManager.OpenProfile(selectedProfile, password)
		if err != nil {
			log.Fatal(err)
		}
		*dbPath = paths.Database
		*audioDir = paths.Audio
		planePath = paths.Plane
		verifyProfilePassword = func(candidate string) bool { return profileManager.Verify(selectedProfile, candidate) }
		changeProfilePassword = func(oldPassword, newPassword string) error {
			return profileManager.ChangePassword(selectedProfile, oldPassword, newPassword)
		}
	} else {
		if *dbPath == "" {
			*dbPath = "data/context-memory.db"
		}
		if *audioDir == "" {
			absDBPath, err := filepath.Abs(*dbPath)
			if err != nil {
				log.Fatalf("context-web: resolve database path: %v", err)
			}
			*audioDir = filepath.Join(filepath.Dir(absDBPath), "synced-audio")
		}
	}
	if planePath == "" {
		planePath = *dbPath + ".events.db"
	}
	if *dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
			log.Fatal(err)
		}
	}
	store, err := memory.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	events, err := plane.Open(planePath)
	if err != nil {
		log.Fatal(err)
	}
	defer events.Close()

	indexDir := *dbPath + ".index"
	if selectedProfile != "" {
		paths, err := profileManager.Resolve(selectedProfile)
		if err != nil {
			log.Fatal(err)
		}
		indexDir = paths.Index
	}
	indexPath := ""
	if *useSemanticIndex {
		indexPath = filepath.Join(indexDir, "semantic.vec")
	}
	retriever, err := semantic.RetrieverFromConfig(semantic.Config{IndexPath: indexPath,
		Provider: *semanticProvider,
		Dims:     *semanticDims,
		Endpoint: *semanticEndpoint,
		Model:    *semanticModel,
		KeyEnv:   *semanticKeyEnv,
	}, store.DB())
	if err != nil {
		log.Fatal(err)
	}
	var rerank contextbuild.Reranker
	if strings.TrimSpace(*decisionURL) != "" {
		rerank = &contextbuild.DecisionReranker{Engine: &decision.SystemOneClient{Endpoint: strings.TrimSpace(*decisionURL), Model: *decisionModel}}
	}
	svc := app.New(store, retriever, rerank, *audioDir)
	svc.ASRProvider = *asrProvider
	svc.ASRInitError = asrInitMessage
	svc.BriefPolisher = briefPolisher
	if strings.TrimSpace(*sufficiencyURL) != "" {
		svc.Assessor = &contextbuild.DecisionEvidenceAssessor{Engine: &decision.SystemOneClient{Endpoint: strings.TrimSpace(*sufficiencyURL), Model: *sufficiencyModel}}
	}
	svc.ForgetPlane = events
	if *useSemanticIndex {
		svc.ManagedIndexDir = indexDir
	}
	if selectedProfile != "" {
		paths, err := profileManager.Resolve(selectedProfile)
		if err != nil {
			log.Fatal(err)
		}
		svc.ManagedIndexDir = paths.Index
		svc.ManagedBackupDir = paths.Backups
	}
	importer, err := newImporter(store, *audioDir)
	if err != nil {
		log.Fatal(err)
	}
	timeSvc := &app.TimeService{Store: store, Events: events}
	worldlineSvc := &app.WorldlineService{Time: timeSvc, DecisionEngine: worldlineEngine}
	server, err := web.New(web.Config{
		Service:               svc,
		Importer:              importer,
		TimeService:           timeSvc,
		Worldline:             worldlineSvc,
		DecisionTrail:         &app.DecisionTrailService{Worldline: worldlineSvc, Time: timeSvc},
		WorldlineDecision:     worldlineEngine,
		WebDir:                *webDir,
		DevOrigins:            devOrigins,
		Logger:                log.Default(),
		ProfileName:           selectedProfile,
		VerifyProfilePassword: verifyProfilePassword,
		ChangeProfilePassword: changeProfilePassword,
	})
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "context-web: profile=%s db=%s audio-dir=%s web-dir=%s\n", selectedProfile, *dbPath, *audioDir, *webDir)
	fmt.Fprintf(os.Stderr, "context-web: listening on http://%s\n", listener.Addr())
	if selectedProfile == "" {
		fmt.Fprintf(os.Stderr, "context-web: open http://%s/#token=%s (single use, 10 min)\n", listener.Addr(), server.BootstrapToken())
	} else {
		fmt.Fprintf(os.Stderr, "context-web: open http://%s/ and sign in to profile %q\n", listener.Addr(), selectedProfile)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	select {
	case err := <-serveErr:
		if err != nil {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("context-web: shutdown: %v", err)
		}
		<-serveErr
	}
}

// (ASR selection lives in internal/cloudsync.SelectTranscriber so the web
// process and the worker accept exactly the same providers.)
