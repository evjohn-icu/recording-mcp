// Command context-embed builds the semantic index for the hybrid retrieval
// layer. It embeds every current non-stale note and turn (ambient artifacts are
// indexed too; the query path filters them) into memory_embeddings and prints
// one JSON IndexReport. It never edits memory: indexing is additive and
// idempotent, and a content hash makes re-indexing an unchanged artifact a
// no-op.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/semantic"
)

func main() {
	log.SetOutput(os.Stderr)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "context-embed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath       = flag.String("db", "", "SQLite memory database (required); migrations are applied on open")
		providerName = flag.String("provider", "hash", "embedding provider: hash or openai")
		dims         = flag.Int("dims", semantic.DefaultHashDims, "embedding dimensions (hash provider only)")
		modelURL     = flag.String("model-url", "", "OpenAI-compatible embeddings endpoint (required when provider=openai)")
		model        = flag.String("model", "", "embedding model name (required when provider=openai)")
		keyEnv       = flag.String("key-env", "", "environment variable holding the API key (openai only)")
		conversation = flag.String("conversation", "", "index only this conversation id (default: all)")
		prune        = flag.Bool("prune", false, "delete embedding rows whose artifact no longer exists")
	)
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	if strings.TrimSpace(*dbPath) == "" {
		return fmt.Errorf("--db is required")
	}

	provider, err := buildProvider(*providerName, *dims, *modelURL, *model, *keyEnv)
	if err != nil {
		return err
	}
	store, err := memory.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	ix := &semantic.Indexer{DB: store.DB(), Provider: provider}
	report, err := ix.IndexConversation(ctx, strings.TrimSpace(*conversation))
	if err != nil {
		return err
	}
	if *prune {
		pruned, err := ix.Prune(ctx)
		if err != nil {
			return err
		}
		report.Pruned = pruned
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	log.Printf("indexed provider=%s model=%s notes=%d turns=%d skipped=%d pruned=%d",
		report.ProviderID, report.Model, report.Notes, report.Turns, report.Skipped, report.Pruned)
	return nil
}

func buildProvider(name string, dims int, modelURL, model, keyEnv string) (semantic.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "hash":
		return semantic.NewHashProvider(dims), nil
	case "openai":
		url := strings.TrimSpace(modelURL)
		if url == "" {
			return nil, fmt.Errorf("--model-url is required when --provider=openai")
		}
		m := strings.TrimSpace(model)
		if m == "" {
			return nil, fmt.Errorf("--model is required when --provider=openai")
		}
		if dims <= 0 {
			return nil, fmt.Errorf("--dims must be positive")
		}
		var key string
		if env := strings.TrimSpace(keyEnv); env != "" {
			key = strings.TrimSpace(os.Getenv(env))
			if key == "" {
				return nil, fmt.Errorf("environment variable %s is empty", env)
			}
		}
		return semantic.OpenAICompatProvider{Endpoint: url, APIKey: key, ModelName: m, Dimensions: dims}, nil
	default:
		return nil, fmt.Errorf("--provider must be hash or openai, got %q", name)
	}
}
