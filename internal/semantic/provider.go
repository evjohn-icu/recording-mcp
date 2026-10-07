package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"unicode"
)

// DefaultHashDims is the default dimension count of the offline hashing
// provider. 256 dims keeps hash collisions low for local-scale corpora while
// keeping the stored vectors small.
const DefaultHashDims = 256

// Provider is the embedding boundary. ID and Model are the exact values stored
// in memory_embeddings.provider_id / model_id, so changing either is a
// re-indexing event, never a silent reinterpretation of existing rows.
type Provider interface {
	ID() string
	Model() string
	Dims() int
	Embed(ctx context.Context, texts []string) ([]Vector, error)
}

// HashProvider is a deterministic, offline embedder: no network, no weights.
// It tokenizes text into lowercased letter/digit runs plus overlapping
// 2-character grams for CJK runs, hashes each token with FNV-1a 64, sets the
// sign from the top bit and L2-normalizes. The same text always yields the
// same vector, so indexing and querying stay reproducible.
type HashProvider struct{ dims int }

// NewHashProvider returns a HashProvider with the given dimension count. A
// non-positive dims falls back to DefaultHashDims.
func NewHashProvider(dims int) HashProvider {
	if dims <= 0 {
		dims = DefaultHashDims
	}
	return HashProvider{dims: dims}
}

func (h HashProvider) ID() string { return "hash-ngram-v1" }

func (h HashProvider) Model() string { return "hash-ngram-v1" }

func (h HashProvider) Dims() int {
	if h.dims <= 0 {
		return DefaultHashDims
	}
	return h.dims
}

// Embed implements Provider. An empty input is an error: a caller that asks
// for zero embeddings almost certainly mis-wired the index or query path.
func (h HashProvider) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, errors.New("semantic: no texts to embed")
	}
	dims := h.Dims()
	if dims <= 0 {
		return nil, errors.New("semantic: dims must be positive")
	}
	out := make([]Vector, len(texts))
	for i, text := range texts {
		out[i] = hashVector(text, dims)
	}
	return out, nil
}

func hashVector(text string, dims int) Vector {
	v := make(Vector, dims)
	for _, token := range hashTokens(text) {
		sum := fnv1a64(token)
		idx := int(sum % uint64(dims))
		if sum>>63 == 1 {
			v[idx]--
		} else {
			v[idx]++
		}
	}
	normalize(v)
	return v
}

func normalize(v Vector) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return
	}
	inv := 1 / math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) * inv)
	}
}

func fnv1a64(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// hashTokens splits text into lowercased letter/digit runs and, for CJK runs,
// overlapping 2-character grams (a single CJK character yields itself).
func hashTokens(text string) []string {
	runes := []rune(strings.ToLower(text))
	tokens := make([]string, 0, len(runes))
	var word []rune
	flush := func() {
		if len(word) > 0 {
			tokens = append(tokens, string(word))
			word = word[:0]
		}
	}
	for i := 0; i < len(runes); {
		r := runes[i]
		if isCJK(r) {
			flush()
			j := i
			for j < len(runes) && isCJK(runes[j]) {
				j++
			}
			run := runes[i:j]
			if len(run) == 1 {
				tokens = append(tokens, string(run))
			} else {
				for k := 0; k+1 < len(run); k++ {
					tokens = append(tokens, string(run[k:k+2]))
				}
			}
			i = j
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
			i++
			continue
		}
		flush()
		i++
	}
	flush()
	return tokens
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x3040 && r <= 0x30FF) ||
		(r >= 0xAC00 && r <= 0xD7AF)
}

// maxEmbeddingResponseBytes bounds how much of an embeddings response is read.
const maxEmbeddingResponseBytes = 8 << 20

// OpenAICompatProvider is an opt-in embedding provider backed by an
// OpenAI-compatible POST {model, input} endpoint. It is never the default: the
// offline HashProvider is, so no test or eval run needs the network.
type OpenAICompatProvider struct {
	Endpoint   string
	APIKey     string
	ModelName  string
	Dimensions int
	HTTPClient *http.Client
}

func (p OpenAICompatProvider) ID() string { return "openai-compat" }

func (p OpenAICompatProvider) Model() string { return p.ModelName }

func (p OpenAICompatProvider) Dims() int { return p.Dimensions }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed implements Provider. A non-2xx status, a wrong embedding count or a
// dimension mismatch is an error carrying a bounded response snippet.
func (p OpenAICompatProvider) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return nil, errors.New("semantic: empty embedding endpoint")
	}
	if len(texts) == 0 {
		return nil, errors.New("semantic: no texts to embed")
	}
	body, err := json.Marshal(embedRequest{Model: p.ModelName, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("semantic: encode embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("semantic: build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(p.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semantic: embedding request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("semantic: read embedding response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("semantic: embedding endpoint returned %s: %s", resp.Status, snippet(data))
	}
	var parsed embedResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("semantic: decode embedding response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("semantic: embedding endpoint error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("semantic: embedding endpoint returned %d embeddings for %d inputs", len(parsed.Data), len(texts))
	}
	out := make([]Vector, len(parsed.Data))
	for i, item := range parsed.Data {
		if p.Dimensions > 0 && len(item.Embedding) != p.Dimensions {
			return nil, fmt.Errorf("semantic: embedding %d has %d dims, want %d", i, len(item.Embedding), p.Dimensions)
		}
		out[i] = Vector(item.Embedding)
	}
	return out, nil
}

// snippet returns a bounded, whitespace-trimmed excerpt of a response body for
// error messages.
func snippet(data []byte) string {
	const limit = 512
	t := strings.TrimSpace(string(data))
	if len(t) > limit {
		t = t[:limit] + "..."
	}
	return t
}
