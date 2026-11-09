package semantic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// roleRecorder implements RoleProvider and records which role each call used.
type roleRecorder struct {
	dims          int
	documentCalls int
	queryCalls    int
	plainCalls    int
}

func (p *roleRecorder) ID() string    { return "role-recorder" }
func (p *roleRecorder) Model() string { return "role-recorder" }
func (p *roleRecorder) Dims() int     { return p.dims }

func (p *roleRecorder) vector() Vector {
	v := make(Vector, p.dims)
	v[0] = 1
	return v
}

func (p *roleRecorder) Embed(_ context.Context, texts []string) ([]Vector, error) {
	p.plainCalls++
	out := make([]Vector, len(texts))
	for i := range out {
		out[i] = p.vector()
	}
	return out, nil
}

func (p *roleRecorder) EmbedRole(_ context.Context, texts []string, role EmbedRole) ([]Vector, error) {
	switch role {
	case RoleQuery:
		p.queryCalls++
	case RoleDocument:
		p.documentCalls++
	}
	out := make([]Vector, len(texts))
	for i := range out {
		out[i] = p.vector()
	}
	return out, nil
}

func TestOpenAICompatProviderSendsInputType(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}},
		})
	}))
	defer server.Close()
	provider := OpenAICompatProvider{Endpoint: server.URL, ModelName: "qwen3-embedding", Dimensions: 2}
	if _, err := provider.EmbedRole(context.Background(), []string{"查预算"}, RoleQuery); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Embed(context.Background(), []string{"预算记录"}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	if bodies[0]["input_type"] != "query" {
		t.Fatalf("query request input_type = %v, want query", bodies[0]["input_type"])
	}
	if bodies[1]["input_type"] != "document" {
		t.Fatalf("plain Embed must use the document role, got %v", bodies[1]["input_type"])
	}
	for _, role := range []EmbedRole{RoleDocument, RoleQuery} {
		if _, err := provider.EmbedRole(context.Background(), []string{"x"}, role); err != nil {
			t.Fatalf("role %s: %v", role, err)
		}
	}
	if bodies[2]["input_type"] != "document" {
		t.Fatalf("document role input_type = %v, want document", bodies[2]["input_type"])
	}
}

func TestSQLiteRetrieverUsesQueryRole(t *testing.T) {
	db := openTestDB(t)
	recorder := &roleRecorder{dims: 4}
	retriever := &SQLiteRetriever{DB: db, Provider: recorder}
	if _, err := retriever.Search(context.Background(), "季度预算", SearchOptions{}); err != nil {
		t.Fatal(err)
	}
	if recorder.queryCalls != 1 || recorder.documentCalls != 0 || recorder.plainCalls != 0 {
		t.Fatalf("roles: query=%d document=%d plain=%d, want exactly one query call",
			recorder.queryCalls, recorder.documentCalls, recorder.plainCalls)
	}
}

func TestIndexerUsesDocumentRole(t *testing.T) {
	db := openTestDB(t)
	seedConversation(t, db, "conv:role", "rec:role")
	seedNote(t, db, "n:role", "conv:role", "季度预算审批")
	recorder := &roleRecorder{dims: 4}
	if _, err := (&Indexer{DB: db, Provider: recorder}).IndexAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder.documentCalls != 1 || recorder.queryCalls != 0 {
		t.Fatalf("roles: query=%d document=%d, want indexing to use document", recorder.queryCalls, recorder.documentCalls)
	}
}

func TestHashProviderFallsBackForRoles(t *testing.T) {
	provider := NewHashProvider(64)
	for _, role := range []EmbedRole{RoleDocument, RoleQuery} {
		vectors, err := EmbedTexts(context.Background(), provider, []string{"季度预算"}, role)
		if err != nil {
			t.Fatalf("role %s: %v", role, err)
		}
		if len(vectors) != 1 || len(vectors[0]) != 64 {
			t.Fatalf("role %s: vectors = %d/%d", role, len(vectors), len(vectors[0]))
		}
	}
}
