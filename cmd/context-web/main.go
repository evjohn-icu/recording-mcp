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
	"syscall"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/contextbuild"
	"recordingmcp/internal/decision"
	"recordingmcp/internal/memory"
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

func validateListenAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("context-web: invalid listen address: %w", err)
	}
	if host == "" {
		return errors.New("context-web: listen address must be an explicit loopback host")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("context-web: listen address must be loopback (127.0.0.1, ::1, or localhost)")
	}
	return nil
}

func main() {
	log.SetOutput(os.Stderr)
	dbPath := flag.String("db", "data/context-memory.db", "SQLite memory database")
	addr := flag.String("addr", "127.0.0.1:8788", "listen address (loopback by default)")
	audioDir := flag.String("audio-dir", "data/synced-audio", "cloud-sync audio directory; only files beneath it are served")
	webDir := flag.String("web-dir", "web/dist", "built frontend directory")
	semanticProvider := flag.String("semantic-provider", "hash", "semantic recall backend: off, hash (offline deterministic embedder) or openai")
	semanticDims := flag.Int("semantic-dims", semantic.DefaultHashDims, "embedding dimensions (hash provider; openai provider validates the endpoint against it)")
	semanticEndpoint := flag.String("semantic-endpoint", "", "OpenAI-compatible embeddings endpoint (required when --semantic-provider=openai)")
	semanticModel := flag.String("semantic-model", "", "embedding model name (required when --semantic-provider=openai)")
	semanticKeyEnv := flag.String("semantic-key-env", "", "environment variable holding the embeddings API key (openai only)")
	decisionURL := flag.String("decision-url", "", "optional Decision engine endpoint used to rerank hybrid search items (empty disables reranking)")
	decisionModel := flag.String("decision-model", "jev-style-2b-v2", "model name sent to the Decision engine for reranking")
	var devOrigins repeatedFlag
	flag.Var(&devOrigins, "dev-origin", "additional allowed Origin header value for development (repeatable), e.g. http://127.0.0.1:5173")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments: %v", flag.Args())
	}
	if err := validateListenAddress(*addr); err != nil {
		log.Fatal(err)
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

	retriever, err := semantic.RetrieverFromConfig(semantic.Config{
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
	server, err := web.New(web.Config{
		Service:    svc,
		WebDir:     *webDir,
		DevOrigins: devOrigins,
		Logger:     log.Default(),
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
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "context-web: db=%s audio-dir=%s web-dir=%s\n", *dbPath, *audioDir, *webDir)
	fmt.Fprintf(os.Stderr, "context-web: listening on http://%s\n", listener.Addr())
	fmt.Fprintf(os.Stderr, "context-web: open http://%s/#token=%s (single use, 10 min)\n", listener.Addr(), server.BootstrapToken())

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
