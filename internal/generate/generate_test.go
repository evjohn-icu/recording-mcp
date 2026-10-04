package generate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatReply builds an OpenAI chat-completions response wrapping content.
func chatReply(content string) string {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	return string(body)
}

func TestOpenAICompatGenerate(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []Draft
	}{
		{
			name:    "fenced json",
			content: "```json\n" + `{"candidates":[{"kind":"observation","summary":"用户偏好本地设备","topic":"设备","evidence_turn_ids":["seg01-turn0001"],"interpretation_note":"明确陈述"}]}` + "\n```",
			want: []Draft{{
				Kind:               "observation",
				Summary:            "用户偏好本地设备",
				TopicTitle:         "设备",
				EvidenceTurnIDs:    []string{"seg01-turn0001"},
				InterpretationNote: "明确陈述",
			}},
		},
		{
			name: "bare json",
			content: `{"candidates":[` +
				`{"kind":"request","summary":"请求整理会议","topic":"会议","evidence_turn_ids":["seg01-turn0002","seg01-turn0003"]},` +
				`{"kind":"intention","summary":"计划下周汇报","topic":"计划","evidence_turn_ids":["seg01-turn0002"],"interpretation_note":"推断"}]}`,
			want: []Draft{
				{Kind: "request", Summary: "请求整理会议", TopicTitle: "会议", EvidenceTurnIDs: []string{"seg01-turn0002", "seg01-turn0003"}},
				{Kind: "intention", Summary: "计划下周汇报", TopicTitle: "计划", EvidenceTurnIDs: []string{"seg01-turn0002"}, InterpretationNote: "推断"},
			},
		},
		{
			name:    "empty candidates",
			content: `{"candidates":[]}`,
			want:    []Draft{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("content-type = %q", ct)
				}
				if auth := r.Header.Get("Authorization"); auth != "Bearer secret" {
					t.Errorf("authorization = %q, want Bearer secret", auth)
				}
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &gotBody); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, chatReply(tc.content))
			}))
			defer srv.Close()

			gen := &OpenAICompat{Endpoint: srv.URL, APIKey: "secret", Model: "glm-5.3-flash"}
			drafts, model, err := gen.Generate(context.Background(), "状态：用户讨论设备", []string{"seg01-turn0001", "seg01-turn0002", "seg01-turn0003"})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if model != "glm-5.3-flash" {
				t.Errorf("model = %q", model)
			}
			if len(drafts) != len(tc.want) {
				t.Fatalf("drafts = %d, want %d (%+v)", len(drafts), len(tc.want), drafts)
			}
			for i := range tc.want {
				if !draftEqual(drafts[i], tc.want[i]) {
					t.Errorf("draft[%d] = %+v, want %+v", i, drafts[i], tc.want[i])
				}
			}

			if gotBody["model"] != "glm-5.3-flash" {
				t.Errorf("request model = %v", gotBody["model"])
			}
			if temp, ok := gotBody["temperature"].(float64); !ok || temp != 0 {
				t.Errorf("request temperature = %v, want 0", gotBody["temperature"])
			}
			msgs, ok := gotBody["messages"].([]any)
			if !ok || len(msgs) != 2 {
				t.Fatalf("request messages = %v", gotBody["messages"])
			}
			user, _ := msgs[1].(map[string]any)
			content, _ := user["content"].(string)
			if !strings.Contains(content, "状态：用户讨论设备") {
				t.Errorf("user message missing state: %q", content)
			}
			for _, id := range []string{"seg01-turn0001", "seg01-turn0002", "seg01-turn0003"} {
				if !strings.Contains(content, id) {
					t.Errorf("user message missing evidence id %q", id)
				}
			}
		})
	}
}

func TestOpenAICompatGenerateNoAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("authorization = %q, want empty", auth)
		}
		_, _ = io.WriteString(w, chatReply(`{"candidates":[]}`))
	}))
	defer srv.Close()

	gen := &OpenAICompat{Endpoint: srv.URL, Model: "m"}
	if _, _, err := gen.Generate(context.Background(), "s", nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestOpenAICompatGenerateErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		content string
		raw     bool
	}{
		{name: "bad json", status: 200, content: "这不是 JSON"},
		{name: "bad fenced json", status: 200, content: "```json\n{broken\n```"},
		{name: "empty content", status: 200, content: ""},
		{name: "server error", status: 500, content: `{"error":"boom"}`},
		{name: "endpoint error object", status: 200, content: `{"error":{"message":"rate limited"}}`, raw: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.raw {
					_, _ = io.WriteString(w, tc.content)
					return
				}
				_, _ = io.WriteString(w, chatReply(tc.content))
			}))
			defer srv.Close()

			gen := &OpenAICompat{Endpoint: srv.URL, Model: "m"}
			if _, _, err := gen.Generate(context.Background(), "state", []string{"t1"}); err == nil {
				t.Fatal("Generate succeeded, want error")
			}
		})
	}
}

func TestOpenAICompatGenerateEmptyEndpoint(t *testing.T) {
	gen := &OpenAICompat{Model: "m"}
	if _, _, err := gen.Generate(context.Background(), "state", nil); err == nil {
		t.Fatal("Generate succeeded, want error for empty endpoint")
	}
}

func TestSyntheticGenerate(t *testing.T) {
	evidence := []string{"seg01-turn0001", "seg01-turn0002", "seg01-turn0003"}

	first, model, err := Synthetic{}.Generate(context.Background(), "状态内容", evidence)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if model != syntheticModel {
		t.Errorf("model = %q", model)
	}
	if len(first) > 2 {
		t.Fatalf("drafts = %d, want 0-2", len(first))
	}
	second, _, err := Synthetic{}.Generate(context.Background(), "状态内容", evidence)
	if err != nil {
		t.Fatalf("Generate second: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("non-deterministic count: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if !draftEqual(first[i], second[i]) {
			t.Errorf("draft[%d] not deterministic: %+v vs %+v", i, first[i], second[i])
		}
		if !ValidKind(first[i].Kind) {
			t.Errorf("draft[%d] kind %q invalid", i, first[i].Kind)
		}
		if first[i].Summary == "" {
			t.Errorf("draft[%d] empty summary", i)
		}
		if len(first[i].EvidenceTurnIDs) == 0 {
			t.Errorf("draft[%d] empty evidence", i)
		}
		for _, id := range first[i].EvidenceTurnIDs {
			if !contains(evidence, id) {
				t.Errorf("draft[%d] evidence %q not in window", i, id)
			}
		}
	}
}

func TestSyntheticGenerateNoEvidence(t *testing.T) {
	drafts, model, err := Synthetic{}.Generate(context.Background(), "状态内容", nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(drafts) != 0 {
		t.Fatalf("drafts = %d, want 0 without evidence", len(drafts))
	}
	if model != syntheticModel {
		t.Errorf("model = %q", model)
	}
}

func TestValidKind(t *testing.T) {
	for _, k := range Kinds {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%q) = false", k)
		}
	}
	if ValidKind("summary") || ValidKind("") {
		t.Error("ValidKind accepted an unknown kind")
	}
}

func TestStripJSONFence(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```":     `{"a":1}`,
		"{\"a\":1}":               `{"a":1}`,
		"  ```json\n[]\n```  ":    `[]`,
	}
	for in, want := range cases {
		if got := stripJSONFence(in); got != want {
			t.Errorf("stripJSONFence(%q) = %q, want %q", in, got, want)
		}
	}
}

func draftEqual(a, b Draft) bool {
	if a.Kind != b.Kind || a.Summary != b.Summary || a.TopicTitle != b.TopicTitle || a.InterpretationNote != b.InterpretationNote {
		return false
	}
	if len(a.EvidenceTurnIDs) != len(b.EvidenceTurnIDs) {
		return false
	}
	for i := range a.EvidenceTurnIDs {
		if a.EvidenceTurnIDs[i] != b.EvidenceTurnIDs[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
