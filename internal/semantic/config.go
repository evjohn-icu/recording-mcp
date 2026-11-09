package semantic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config is the semantic-backend selection shared by every binary: the MCP
// server, the local web server and the offline evaluation harness all resolve
// their provider through ProviderFromConfig so one flag spelling produces one
// backend.
type Config struct {
	// Provider is "off", "hash" or "openai"; empty means hash.
	Provider string
	// Dims is the embedding dimension count (hash provider; the OpenAI
	// provider validates the endpoint against it).
	Dims int
	// Endpoint and Model are required when Provider is openai.
	Endpoint string
	Model    string
	// KeyEnv names the environment variable holding the API key (openai only).
	KeyEnv string
	// IndexPath opts in to a rebuildable pure-Go HNSW sidecar. Empty keeps the
	// exact SQLite scan backend.
	IndexPath string
	// InputTypeField sends the non-standard input_type field. Only the bundled
	// Qwen embedding server needs it; strict OpenAI endpoints reject it.
	InputTypeField bool
}

// ProviderFromConfig returns the embedding provider named by cfg. It returns
// (nil, nil) when the backend is off so callers can degrade instead of failing.
func ProviderFromConfig(cfg Config) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "hash":
		return NewHashProvider(cfg.Dims), nil
	case "off":
		return nil, nil
	case "openai":
		endpoint := strings.TrimSpace(cfg.Endpoint)
		model := strings.TrimSpace(cfg.Model)
		if endpoint == "" || model == "" {
			return nil, errors.New("--semantic-endpoint and --semantic-model are required when --semantic-provider=openai")
		}
		if cfg.Dims <= 0 {
			return nil, errors.New("--semantic-dims must be positive")
		}
		var key string
		if env := strings.TrimSpace(cfg.KeyEnv); env != "" {
			key = strings.TrimSpace(os.Getenv(env))
			if key == "" {
				return nil, fmt.Errorf("environment variable %s is empty", env)
			}
		}
		// Dimensions comes from configuration, not from the first response, so
		// memory_embeddings rows are keyed by the exact dims the server filters
		// on; a mismatched endpoint fails loudly at embed time instead of
		// silently poisoning the index.
		return OpenAICompatProvider{Endpoint: endpoint, APIKey: key, ModelName: model, Dimensions: cfg.Dims, SendInputType: cfg.InputTypeField}, nil
	default:
		return nil, fmt.Errorf("unsupported semantic provider %q (want off, hash, or openai)", strings.TrimSpace(cfg.Provider))
	}
}

// RetrieverFromConfig wraps ProviderFromConfig into the SQLite-backed
// retriever. A nil provider yields a nil retriever.
func RetrieverFromConfig(cfg Config, db *sql.DB) (SemanticRetriever, error) {
	provider, err := ProviderFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, nil
	}
	if strings.TrimSpace(cfg.IndexPath) != "" {
		return OpenHNSWRetriever(context.Background(), db, provider, strings.TrimSpace(cfg.IndexPath))
	}
	return &SQLiteRetriever{DB: db, Provider: provider}, nil
}
