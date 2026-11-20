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
	"syscall"
	"time"
	"unicode"
)

// Provider outage reasons. They are a closed set so a caller can switch on
// them without parsing error text. Only these four conditions are degradable:
// anything else (bad request, malformed response, wrong vector shape) must
// fail the caller rather than silently degrade retrieval.
const (
	ReasonProviderTimeout     = "provider_timeout"
	ReasonConnectionRefused   = "connection_refused"
	ReasonModelUnavailable    = "model_unavailable"
	ReasonProviderUnavailable = "provider_unavailable"
)

// ProviderUnavailableError marks an embedding endpoint outage the caller may
// degrade from. Error() deliberately carries only the reason and a sanitized
// cause: never the response body, the API token or URL parameters.
type ProviderUnavailableError struct {
	Reason string
	Cause  error
}

func (e *ProviderUnavailableError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("semantic: embedding provider unavailable (%s): %v", e.Reason, e.Cause)
	}
	return fmt.Sprintf("semantic: embedding provider unavailable (%s)", e.Reason)
}

func (e *ProviderUnavailableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// IsProviderUnavailable reports whether err is a degradable provider outage.
func IsProviderUnavailable(err error) bool {
	var target *ProviderUnavailableError
	return errors.As(err, &target)
}

// errContextCancelled wraps a request-context error so callers can tell a
// caller cancellation apart from a provider-side timeout. errors.Is keeps
// working through Unwrap, so errors.Is(err, context.Canceled) stays true.
type errContextCancelled struct{ cause error }

func (e errContextCancelled) Error() string { return e.cause.Error() }

func (e errContextCancelled) Unwrap() error { return e.cause }

// sanitizedTransportError strips the URL and request metadata the http client
// embeds in *url.Error, keeping only a stable, non-sensitive description.
func sanitizedTransportError(err error) error {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("connection refused")
	case errors.Is(err, syscall.ECONNRESET):
		return errors.New("connection reset")
	case errors.Is(err, io.EOF):
		return errors.New("connection closed")
	default:
		return errors.New("transport error")
	}
}

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

// EmbedRole distinguishes how a text is embedded. Retrieval models such as
// Qwen3-Embedding are trained with a query instruction; indexing the same text
// as a document must not get that prefix.
type EmbedRole string

const (
	RoleDocument EmbedRole = "document"
	RoleQuery    EmbedRole = "query"
)

// RoleProvider is the optional extension for providers that can embed with a
// role. Providers without it keep using Embed for both sides.
type RoleProvider interface {
	Provider
	EmbedRole(ctx context.Context, texts []string, role EmbedRole) ([]Vector, error)
}

// EmbedTexts uses the role-aware path when the provider supports it and falls
// back to the plain Embed otherwise (HashProvider stays role-blind on purpose).
func EmbedTexts(ctx context.Context, p Provider, texts []string, role EmbedRole) ([]Vector, error) {
	if p == nil {
		return nil, errors.New("semantic: provider is required")
	}
	if roleProvider, ok := p.(RoleProvider); ok {
		return roleProvider.EmbedRole(ctx, texts, role)
	}
	return p.Embed(ctx, texts)
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
	// Timeout bounds one embedding call (request plus body read). Zero uses
	// DefaultProviderTimeout. It is applied through a request-scoped child
	// context so a shared http.Client is never mutated.
	Timeout time.Duration
	// SendInputType opts in to the non-standard input_type field. The bundled
	// Qwen server uses it to prepend the query instruction; strict
	// OpenAI-compatible endpoints reject unknown body fields, so it is off by
	// default.
	SendInputType bool
}

// DefaultProviderTimeout is the per-call embedding deadline used when
// OpenAICompatProvider.Timeout is unset.
const DefaultProviderTimeout = 30 * time.Second

func (p OpenAICompatProvider) ID() string { return "openai-compat" }

func (p OpenAICompatProvider) Model() string { return p.ModelName }

func (p OpenAICompatProvider) Dims() int { return p.Dimensions }

type embedRequest struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	InputType string   `json:"input_type,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		// Code is the structured error identifier (for example
		// model_not_found). Classification uses this field only; message text
		// is never matched.
		Code string `json:"code"`
	} `json:"error"`
}

// Embed implements Provider. It embeds as a document; a query caller must use
// EmbedRole(ctx, texts, RoleQuery).
func (p OpenAICompatProvider) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	return p.EmbedRole(ctx, texts, RoleDocument)
}

// EmbedRole optionally sends input_type. The bundled local server needs it for
// query instruction selection; other endpoints can reject the field.
//
// The call runs under a request-scoped timeout derived from ctx (see Timeout).
// A caller cancellation is returned verbatim, a provider-side timeout becomes
// *ProviderUnavailableError with ReasonProviderTimeout, and a refused
// connection, 502/503/504 or structured model_not_found become the other
// degradable reasons. Every other failure stays a plain error so retrieval
// never masks a programming or protocol fault as an outage.
func (p OpenAICompatProvider) EmbedRole(ctx context.Context, texts []string, role EmbedRole) ([]Vector, error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return nil, errors.New("semantic: empty embedding endpoint")
	}
	if len(texts) == 0 {
		return nil, errors.New("semantic: no texts to embed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inputType := ""
	if p.SendInputType && (role == RoleDocument || role == RoleQuery) {
		inputType = string(role)
	}
	body, err := json.Marshal(embedRequest{Model: p.ModelName, Input: texts, InputType: inputType})
	if err != nil {
		return nil, fmt.Errorf("semantic: encode embedding request: %w", err)
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProviderTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
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
		return nil, classifyTransportError(ctx, callCtx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingResponseBytes))
	if err != nil {
		return nil, classifyTransportError(ctx, callCtx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed embedResponse
		code := ""
		if json.Unmarshal(data, &parsed) == nil && parsed.Error != nil {
			code = parsed.Error.Code
		}
		switch {
		case code == "model_not_found":
			return nil, &ProviderUnavailableError{Reason: ReasonModelUnavailable, Cause: errors.New("model_not_found")}
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
			return nil, &ProviderUnavailableError{
				Reason: ReasonProviderUnavailable,
				Cause:  fmt.Errorf("http status %d", resp.StatusCode),
			}
		default:
			return nil, fmt.Errorf("semantic: embedding endpoint returned http status %d", resp.StatusCode)
		}
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		// The parent context is still live (checked above), so the derived
		// deadline is the provider's own timeout, not a caller deadline.
		return nil, &ProviderUnavailableError{Reason: ReasonProviderTimeout, Cause: errors.New("request deadline exceeded")}
	}
	var parsed embedResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("semantic: decode embedding response: %w", err)
	}
	if parsed.Error != nil {
		if parsed.Error.Code == "model_not_found" {
			return nil, &ProviderUnavailableError{Reason: ReasonModelUnavailable, Cause: errors.New("model_not_found")}
		}
		code := strings.TrimSpace(parsed.Error.Code)
		if code == "" {
			code = "provider error"
		}
		return nil, fmt.Errorf("semantic: embedding endpoint error code %s", code)
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

// classifyTransportError maps a client/read failure to either the caller's own
// context error or a degradable provider outage. The cause is sanitized so no
// URL, query string or token can leak into logs or gaps.
func classifyTransportError(ctx, callCtx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return &ProviderUnavailableError{Reason: ReasonProviderTimeout, Cause: errors.New("request deadline exceeded")}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ProviderUnavailableError{Reason: ReasonProviderTimeout, Cause: errors.New("request deadline exceeded")}
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &ProviderUnavailableError{Reason: ReasonProviderTimeout, Cause: errors.New("network timeout")}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return &ProviderUnavailableError{Reason: ReasonConnectionRefused, Cause: sanitizedTransportError(err)}
	}
	return fmt.Errorf("semantic: embedding request failed: %w", sanitizedTransportError(err))
}
