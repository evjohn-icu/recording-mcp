package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"recordingmcp/internal/decision"
)

// Explicit opt-in: only fixed synthetic Chinese examples are sent to the local
// model. Ordinary go test never invokes a provider.
func TestWorldlineSemanticLocalModelChinese(t *testing.T) {
	if os.Getenv("WORLDLINE_LOCAL_MODEL_TEST") != "1" {
		t.Skip("local real-model evaluation not requested")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	engine := &decision.SystemOneClient{Endpoint: "http://127.0.0.1:8799/v1/systemone", Model: "decision-v3-0.8b", HTTPClient: &http.Client{Transport: transport, Timeout: 45 * time.Second}}
	cases := []struct{ name, a, b, want string }{
		{"synonym", "我目前居住在杭州市。", "我现在住在杭州。", "text_similarity"},
		{"contradiction", "这家店每天上午九点营业。", "这家店每天上午十点才营业，九点不营业。", "review_required"},
		{"unrelated", "午饭吃了番茄鸡蛋。", "公司下个月调整工资。", "unrelated"},
		{"no_evidence", "", "", "insufficient_evidence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair := semanticPair{ID: tc.name, Title: "合成测试主题", IdentityStatus: "unconfirmed_identity_text_only",
				Earlier: semanticPairEvidence{Summary: tc.a, Sources: []semanticSourceText{{Text: tc.a}}},
				Later:   semanticPairEvidence{Summary: tc.b, Sources: []semanticSourceText{{Text: tc.b}}},
			}
			state, _ := json.Marshal(map[string]any{"pair": pair})
			result, err := engine.Evaluate(context.Background(), string(state), []decision.Question{worldlineSemanticQuestion(tc.name)})
			if err != nil {
				t.Fatal(err)
			}
			answer := result.Answers[tc.name]
			if err := validateWorldlineSemanticAnswer(answer, worldlineSemanticCriteria()); err != nil {
				t.Fatal(err)
			}
			t.Logf("model=%s weights=%s expected=%s actual=%s latency_ms=%d", result.Model, result.WeightsVersion, tc.want, answer.Choice, result.Usage.LatencyMS)
			if answer.Choice != tc.want {
				t.Errorf("classification mismatch: expected %s, got %s", tc.want, answer.Choice)
			}
		})
	}
}
