package semantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/memory"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := memory.Open(filepath.Join(t.TempDir(), "semantic.db"))
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.DB()
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// seedConversation inserts a conversation and its recording.
func seedConversation(t *testing.T, db *sql.DB, convID, recID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO conversations(id,title,summary,annotation_version,annotation_basis,recording_id,source_sha256,transcript_version,duration_ms,transcript_json,annotations_json)
 VALUES(?,?,'','v1','',?,'sha','tv',1000,'{}','{}')`, convID, convID, recID)
	mustExec(t, db, `INSERT INTO recordings(id,conversation_id,source_file,source_sha256,duration_ms,recording_started_at,absolute_time_known)
 VALUES(?,?,'file','sha',1000,NULL,0)`, recID, convID)
}

func seedTurn(t *testing.T, db *sql.DB, turnID, convID, recID, content string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES(?,?,?,0,1000,1500,NULL,NULL,?,'sha','rev-1',?,'{}')`, turnID, convID, recID, content, turnID)
}

// markTurnStale attaches a superseded (active=0) source attachment to a turn.
func markTurnStale(t *testing.T, db *sql.DB, turnID, convID, recID string) {
	t.Helper()
	attID := "att-" + turnID
	mustExec(t, db, `INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
 VALUES(?,?,?,0,?, 'rev-1','{}','{}','sha',0,NULL)`, attID, convID, recID, "seg-"+turnID)
	mustExec(t, db, `INSERT INTO attachment_turns(attachment_id,turn_id) VALUES(?,?)`, attID, turnID)
}

func markTurnAmbient(t *testing.T, db *sql.DB, turnID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,'ambient','voice_assistant','rule',0)`, turnID)
}

func seedNote(t *testing.T, db *sql.DB, noteID, convID, summary string, evidenceTurnIDs ...string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO notes(id,conversation_id,kind,summary,topic_ids_json,claim_mode,interpretation_note,annotation_version)
 VALUES(?,?,'observation',?,'[]','inference','','v1')`, noteID, convID, summary)
	for i, turnID := range evidenceTurnIDs {
		mustExec(t, db, `INSERT INTO note_evidence(note_id,turn_id,ordinal) VALUES(?,?,?)`, noteID, turnID, i)
	}
}

func TestHashProviderDeterministic(t *testing.T) {
	p := NewHashProvider(64)
	ctx := context.Background()
	a, err := p.Embed(ctx, []string{"搬去珠海定居"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	b, err := p.Embed(ctx, []string{"搬去珠海定居"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	c, err := p.Embed(ctx, []string{"完全不同的内容"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(a[0]) != 64 {
		t.Fatalf("dims = %d, want 64", len(a[0]))
	}
	if string(Encode(a[0])) != string(Encode(b[0])) {
		t.Fatal("same text produced different vectors")
	}
	if string(Encode(a[0])) == string(Encode(c[0])) {
		t.Fatal("different text produced the same vector")
	}
	self, err := Cosine(a[0], a[0])
	if err != nil {
		t.Fatalf("cosine: %v", err)
	}
	if self < 0.999 {
		t.Fatalf("cosine(v,v) = %f, want ~1", self)
	}
	if _, err := p.Embed(ctx, nil); err == nil {
		t.Fatal("empty input must error")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	v := Vector{0.5, -0.25, 0, 1}
	got, err := Decode(Encode(v), len(v))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(v) {
		t.Fatalf("dims = %d, want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Fatalf("element %d = %f, want %f", i, got[i], v[i])
		}
	}
}

func TestDecodeRejectsWrongLength(t *testing.T) {
	if _, err := Decode([]byte{0, 0, 0}, 1); err == nil {
		t.Fatal("wrong length must error")
	}
	if _, err := Decode(nil, 0); err == nil {
		t.Fatal("zero dims must error")
	}
	if _, err := Cosine(Vector{1}, Vector{1, 2}); err == nil {
		t.Fatal("cosine length mismatch must error")
	}
}

func TestOpenAICompatProviderParsesEmbeddings(t *testing.T) {
	var gotBody embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{1, 0, 0}},
				{"embedding": []float32{0, 1, 0}},
			},
		})
	}))
	defer srv.Close()

	p := OpenAICompatProvider{Endpoint: srv.URL, APIKey: "secret", ModelName: "test-model", Dimensions: 3}
	vecs, err := p.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("vectors = %v", vecs)
	}
	if p.ID() != "openai-compat" || p.Model() != "test-model" || p.Dims() != 3 {
		t.Fatalf("identity = %s/%s@%d", p.ID(), p.Model(), p.Dims())
	}
	if gotBody.Model != "test-model" || len(gotBody.Input) != 2 {
		t.Fatalf("request body = %+v", gotBody)
	}
}

func TestOpenAICompatProviderRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := OpenAICompatProvider{Endpoint: srv.URL, ModelName: "m", Dimensions: 2}
	_, err := p.Embed(context.Background(), []string{"a"})
	if err == nil || !contains(err.Error(), "500") {
		t.Fatalf("err = %v, want a 500 status mention", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestIndexIsIdempotentAndHashKeyed(t *testing.T) {
	db := openTestDB(t)
	seedConversation(t, db, "conv:a", "rec:a")
	seedTurn(t, db, "t:1", "conv:a", "rec:a", "讨论搬迁计划")
	seedNote(t, db, "n:1", "conv:a", "计划搬去珠海", "t:1")

	ix := &Indexer{DB: db, Provider: NewHashProvider(64)}
	first, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if first.Notes != 1 || first.Turns != 1 || first.Skipped != 0 {
		t.Fatalf("first report = %+v", first)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_embeddings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rows = %d, want 2", count)
	}
	second, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if second.Skipped != 2 || second.Notes != 0 || second.Turns != 0 {
		t.Fatalf("second report = %+v, want skipped=2", second)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_embeddings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rows after reindex = %d, want 2", count)
	}
	// Changing one note's summary re-embeds exactly that note.
	mustExec(t, db, `UPDATE notes SET summary='改成搬去广州' WHERE id='n:1'`)
	third, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("reindex after edit: %v", err)
	}
	if third.Notes != 1 || third.Skipped != 1 {
		t.Fatalf("third report = %+v, want notes=1 skipped=1", third)
	}
}

func TestIndexIncludesStaleAndAmbientButSearchFiltersThem(t *testing.T) {
	db := openTestDB(t)
	seedConversation(t, db, "conv:a", "rec:a")
	seedTurn(t, db, "t:current", "conv:a", "rec:a", "珠海搬家会议记录")
	seedTurn(t, db, "t:stale", "conv:a", "rec:a", "珠海搬家会议记录")
	seedTurn(t, db, "t:ambient", "conv:a", "rec:a", "珠海搬家会议记录")
	markTurnAmbient(t, db, "t:ambient")
	seedNote(t, db, "n:ambient", "conv:a", "珠海搬家会议记录", "t:ambient")

	ix := &Indexer{DB: db, Provider: NewHashProvider(64)}
	rep, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	// Every current artifact, including ambient ones, is indexed; the ambient
	// and stale rows must then be filtered by the query path.
	if rep.Turns != 3 || rep.Notes != 1 {
		t.Fatalf("index report = %+v, want turns=3 notes=1", rep)
	}
	// Supersede t:stale after indexing: its embedding row stays, exactly like a
	// turn indexed while current and superseded later.
	markTurnStale(t, db, "t:stale", "conv:a", "rec:a")
	r := &SQLiteRetriever{DB: db, Provider: NewHashProvider(64)}
	if !r.Ready() {
		t.Fatal("retriever should be ready after indexing")
	}
	hits, err := r.Search(context.Background(), "珠海搬家会议记录", SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.ArtifactID] = true
	}
	if !got["t:current"] {
		t.Fatalf("current turn not recalled: %+v", hits)
	}
	if got["t:stale"] {
		t.Fatal("superseded turn leaked into semantic hits")
	}
	if got["t:ambient"] {
		t.Fatal("ambient turn leaked into semantic hits")
	}
	if got["n:ambient"] {
		t.Fatal("ambient-primary note leaked into semantic hits")
	}
}

func TestPruneConversationIsScoped(t *testing.T) {
	db := openTestDB(t)
	seedConversation(t, db, "conv:a", "rec:a")
	seedConversation(t, db, "conv:b", "rec:b")
	seedTurn(t, db, "t:a", "conv:a", "rec:a", "甲会话内容")
	seedTurn(t, db, "t:b", "conv:b", "rec:b", "乙会话内容")

	ix := &Indexer{DB: db, Provider: NewHashProvider(64)}
	if _, err := ix.IndexAll(context.Background()); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Orphan the artifacts of both conversations, then prune only conv:a.
	mustExec(t, db, `DELETE FROM turns WHERE id IN ('t:a','t:b')`)
	pruned, err := ix.PruneConversation(context.Background(), "conv:a")
	if err != nil {
		t.Fatalf("prune conversation: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("scoped prune removed %d rows, want 1", pruned)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_embeddings WHERE conversation_id='conv:b'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("out-of-scope rows removed: %d left in conv:b", remaining)
	}
	// An unscoped Prune then removes the rest.
	pruned, err = ix.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("global prune removed %d rows, want 1", pruned)
	}
}

func TestSearchRespectsConversationFilterAndLimit(t *testing.T) {
	db := openTestDB(t)
	seedConversation(t, db, "conv:a", "rec:a")
	seedConversation(t, db, "conv:b", "rec:b")
	seedTurn(t, db, "t:a1", "conv:a", "rec:a", "季度预算讨论")
	seedTurn(t, db, "t:a2", "conv:a", "rec:a", "季度预算复盘")
	seedTurn(t, db, "t:b1", "conv:b", "rec:b", "季度预算讨论")

	ix := &Indexer{DB: db, Provider: NewHashProvider(64)}
	if _, err := ix.IndexAll(context.Background()); err != nil {
		t.Fatalf("index: %v", err)
	}
	r := &SQLiteRetriever{DB: db, Provider: NewHashProvider(64)}
	hits, err := r.Search(context.Background(), "季度预算", SearchOptions{ConversationID: "conv:a"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range hits {
		if h.ConversationID != "conv:a" {
			t.Fatalf("conversation filter leaked %q", h.ConversationID)
		}
	}
	limited, err := r.Search(context.Background(), "季度预算", SearchOptions{Limit: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit ignored: %d hits", len(limited))
	}
	if _, err := r.Search(context.Background(), "", SearchOptions{}); err != nil {
		t.Fatalf("empty query must not error: %v", err)
	}
}
