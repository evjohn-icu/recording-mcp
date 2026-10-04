package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxResponseBytes bounds how much of a chat-completions response is read.
const maxResponseBytes = 4 << 20

// OpenAICompat is a Generator backed by an OpenAI-compatible chat completions
// endpoint. It sends a single temperature-0 completion request and parses the
// strict-JSON candidates object from the assistant message.
type OpenAICompat struct {
	Endpoint   string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// chatMessage is one entry of the chat messages array.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the OpenAI chat completions request body.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

// chatResponse decodes the subset of the chat completions response we need.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// wireCandidates is the strict-JSON object the model is asked to emit.
type wireCandidates struct {
	Candidates []wireCandidate `json:"candidates"`
}

type wireCandidate struct {
	Kind               string   `json:"kind"`
	Summary            string   `json:"summary"`
	Topic              string   `json:"topic"`
	EvidenceTurnIDs    []string `json:"evidence_turn_ids"`
	InterpretationNote string   `json:"interpretation_note"`
}

// systemPrompt instructs the model to emit only the strict-JSON candidates
// object. It is written in Chinese to match the recording domain language.
const systemPrompt = `你是记忆候选抽取器。请阅读用户提供的会话状态，抽取 0 到若干条候选记忆，并且只输出一个 JSON 对象，不要输出任何解释、Markdown 代码块以外的文字或多余内容。
输出格式必须严格为：
{"candidates":[{"kind":"...","summary":"...","topic":"...","evidence_turn_ids":["..."],"interpretation_note":"..."}]}
约束：
- kind 只能是 observation、suggestion、request、constraint、intention 之一；
- summary 为简体中文，概括该条记忆，长度不超过 1000 个字符；
- topic 为简短的主题标题；
- evidence_turn_ids 必须来自用户给出的证据 turn id 列表，且至少包含一个；
- interpretation_note 可留空字符串。
若没有可抽取的记忆，输出 {"candidates":[]}。`

// Generate implements Generator. A non-2xx status or a body that is not valid
// JSON in the expected shape returns an error carrying a response snippet.
func (c *OpenAICompat) Generate(ctx context.Context, state string, evidenceTurnIDs []string) ([]Draft, string, error) {
	if c == nil {
		return nil, "", errors.New("generate: nil OpenAICompat")
	}
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return nil, "", errors.New("generate: empty endpoint")
	}
	body, err := json.Marshal(chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt(state, evidenceTurnIDs)},
		},
		Temperature: 0,
	})
	if err != nil {
		return nil, "", fmt.Errorf("generate: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("generate: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(c.APIKey) != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("generate: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, "", fmt.Errorf("generate: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("generate: endpoint returned %s: %s", resp.Status, snippet(data))
	}

	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, "", fmt.Errorf("generate: decode response: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, "", fmt.Errorf("generate: endpoint error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, "", fmt.Errorf("generate: empty choices: %s", snippet(data))
	}
	content := stripJSONFence(out.Choices[0].Message.Content)
	if strings.TrimSpace(content) == "" {
		return nil, "", errors.New("generate: empty completion content")
	}
	var parsed wireCandidates
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, "", fmt.Errorf("generate: decode candidates: %w: %s", err, snippet([]byte(content)))
	}
	drafts := make([]Draft, 0, len(parsed.Candidates))
	for _, cand := range parsed.Candidates {
		drafts = append(drafts, Draft{
			Kind:               cand.Kind,
			Summary:            cand.Summary,
			TopicTitle:         cand.Topic,
			EvidenceTurnIDs:    cand.EvidenceTurnIDs,
			InterpretationNote: cand.InterpretationNote,
		})
	}
	return drafts, c.Model, nil
}

// userPrompt renders the state and the closed evidence turn ID set handed to
// the model.
func userPrompt(state string, evidenceTurnIDs []string) string {
	var b strings.Builder
	b.WriteString("会话状态：\n")
	b.WriteString(state)
	b.WriteString("\n\n可用证据 turn id 列表：\n")
	if len(evidenceTurnIDs) == 0 {
		b.WriteString("(无)\n")
	} else {
		for _, id := range evidenceTurnIDs {
			b.WriteString("- ")
			b.WriteString(id)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// stripJSONFence removes an optional ```json ... ``` wrapper and surrounding
// whitespace so the enclosed JSON can be unmarshaled directly.
func stripJSONFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// Drop the opening fence line (``` or ```json).
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	} else {
		return ""
	}
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
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
