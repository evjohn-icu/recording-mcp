package contextbuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"recordingmcp/internal/memory"
	"recordingmcp/internal/semantic"
)

// funcSemantic is a SemanticRetriever whose probe and search are supplied by
// the test, so a build can observe real typed outages, probe failures and
// mid-build database failures without a fake vector index.
type funcSemantic struct {
	readyFn  func(ctx context.Context) (bool, error)
	searchFn func(ctx context.Context) ([]semantic.SemanticHit, error)
}

func (f *funcSemantic) Ready(ctx context.Context) (bool, error) {
	if f.readyFn != nil {
		return f.readyFn(ctx)
	}
	return true, nil
}

func (f *funcSemantic) Search(ctx context.Context, _ string, _ semantic.SearchOptions) ([]semantic.SemanticHit, error) {
	if f.searchFn != nil {
		return f.searchFn(ctx)
	}
	return nil, nil
}

func (f *funcSemantic) Describe() string { return "func/test" }

// passthroughReranker makes the deterministic item order an explicit success so
// a test can assert "not degraded" without the missing-reranker warning.
type passthroughReranker struct{}

func (passthroughReranker) Rerank(_ context.Context, _ string, items []memory.Item) ([]memory.Item, error) {
	return items, nil
}

// degradationFixture seeds one lexical hit ("预算周三批准") so an outage can
// be shown to keep real results instead of returning an empty package.
func degradationFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	start := "2026-03-01T10:00:00Z"
	f.conversation("conv:a", "Alpha", "rec:a")
	f.recording("rec:a", "conv:a", &start)
	f.turn("t:budget", "conv:a", "rec:a", 0, 1000, nil, "预算周三批准")
	return f
}

func TestSemanticGracefulDegradation(t *testing.T) {
	t.Run("provider_timeout", func(t *testing.T) {
		f := degradationFixture(t)
		pkg := f.buildSemantic(&funcSemantic{searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
			return nil, &semantic.ProviderUnavailableError{Reason: semantic.ReasonProviderTimeout, Cause: errors.New("deadline exceeded")}
		}}, Request{Query: "预算"})
		if !containsString(itemIDs(pkg.Items), "t:budget") {
			t.Fatalf("lexical hit lost on provider timeout: %v", itemIDs(pkg.Items))
		}
		if !pkg.Degraded || !strings.Contains(pkg.DegradeReason, semantic.ReasonProviderTimeout) {
			t.Fatalf("degraded=%v reason=%q", pkg.Degraded, pkg.DegradeReason)
		}
		if !hasGap(pkg, "semantic_unavailable") {
			t.Fatalf("gaps = %+v, want semantic_unavailable", pkg.Gaps)
		}
	})

	t.Run("connection_refused", func(t *testing.T) {
		f := degradationFixture(t)
		pkg := f.buildSemantic(&funcSemantic{searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
			return nil, &semantic.ProviderUnavailableError{Reason: semantic.ReasonConnectionRefused, Cause: errors.New("connection refused")}
		}}, Request{Query: "预算"})
		if !containsString(itemIDs(pkg.Items), "t:budget") || !pkg.Degraded {
			t.Fatalf("items=%v degraded=%v", itemIDs(pkg.Items), pkg.Degraded)
		}
		if !strings.Contains(pkg.DegradeReason, semantic.ReasonConnectionRefused) {
			t.Fatalf("degrade reason = %q", pkg.DegradeReason)
		}
	})

	t.Run("http_503", func(t *testing.T) {
		f := degradationFixture(t)
		pkg := f.buildSemantic(&funcSemantic{searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
			return nil, &semantic.ProviderUnavailableError{Reason: semantic.ReasonProviderUnavailable, Cause: errors.New("http status 503")}
		}}, Request{Query: "预算"})
		if !containsString(itemIDs(pkg.Items), "t:budget") || !pkg.Degraded || !hasGap(pkg, "semantic_unavailable") {
			t.Fatalf("items=%v degraded=%v gaps=%+v", itemIDs(pkg.Items), pkg.Degraded, pkg.Gaps)
		}
	})

	t.Run("model_not_found", func(t *testing.T) {
		f := degradationFixture(t)
		pkg := f.buildSemantic(&funcSemantic{searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
			return nil, &semantic.ProviderUnavailableError{Reason: semantic.ReasonModelUnavailable, Cause: errors.New("model_not_found")}
		}}, Request{Query: "预算"})
		if !containsString(itemIDs(pkg.Items), "t:budget") || !pkg.Degraded {
			t.Fatalf("items=%v degraded=%v", itemIDs(pkg.Items), pkg.Degraded)
		}
		if !strings.Contains(pkg.DegradeReason, semantic.ReasonModelUnavailable) {
			t.Fatalf("degrade reason = %q", pkg.DegradeReason)
		}
	})

	t.Run("request_canceled", func(t *testing.T) {
		f := degradationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Build(ctx, Deps{Store: f.store, DB: f.db, Semantic: &funcSemantic{}}, Request{Query: "预算"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("request_deadline", func(t *testing.T) {
		f := degradationFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		<-ctx.Done()
		_, err := Build(ctx, Deps{Store: f.store, DB: f.db, Semantic: &funcSemantic{}}, Request{Query: "预算"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("readiness_db_error", func(t *testing.T) {
		f := degradationFixture(t)
		pkg := f.buildSemantic(&funcSemantic{readyFn: func(context.Context) (bool, error) {
			return false, errors.New("sql: database is closed")
		}}, Request{Query: "预算"})
		if !containsString(itemIDs(pkg.Items), "t:budget") {
			t.Fatalf("lexical hit lost on readiness failure: %v", itemIDs(pkg.Items))
		}
		if !pkg.Degraded || !strings.Contains(pkg.DegradeReason, "readiness_db_error") {
			t.Fatalf("degraded=%v reason=%q", pkg.Degraded, pkg.DegradeReason)
		}
		if hasGap(pkg, "semantic_index_empty") {
			t.Fatalf("readiness failure reported as empty index: %+v", pkg.Gaps)
		}
		if !hasGap(pkg, "semantic_unavailable") {
			t.Fatalf("gaps = %+v, want semantic_unavailable", pkg.Gaps)
		}
		foundDetail := false
		for _, g := range pkg.Gaps {
			if g.Kind == "semantic_unavailable" && strings.Contains(g.Detail, "readiness_db_error") {
				foundDetail = true
			}
		}
		if !foundDetail {
			t.Fatalf("gap detail lacks readiness_db_error: %+v", pkg.Gaps)
		}
	})

	t.Run("search_db_error", func(t *testing.T) {
		f := degradationFixture(t)
		_, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Semantic: &funcSemantic{
			searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
				return nil, errors.New("sql: database is closed")
			},
		}}, Request{Query: "预算"})
		if err == nil || !strings.Contains(err.Error(), "database is closed") {
			t.Fatalf("err = %v, want propagated database error", err)
		}
	})

	t.Run("invalid_vector", func(t *testing.T) {
		f := degradationFixture(t)
		_, decodeErr := semantic.Decode([]byte{1, 2, 3}, 64)
		if decodeErr == nil {
			t.Fatal("Decode accepted a malformed blob")
		}
		_, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Semantic: &funcSemantic{
			searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
				return nil, decodeErr
			},
		}}, Request{Query: "预算"})
		if err == nil || !strings.Contains(err.Error(), "semantic: vector") {
			t.Fatalf("err = %v, want decode error", err)
		}
	})

	t.Run("scope_filter_error", func(t *testing.T) {
		f := degradationFixture(t)
		// The retriever reports a hit and then breaks the store: the hard
		// scope guard cannot read the item, which must surface as a build
		// error, never as a semantic outage.
		retriever := &funcSemantic{searchFn: func(context.Context) ([]semantic.SemanticHit, error) {
			if err := f.db.Close(); err != nil {
				t.Fatalf("close db: %v", err)
			}
			return []semantic.SemanticHit{{Kind: "turn", ArtifactID: "t:budget", ConversationID: "conv:a", Score: 0.9}}, nil
		}}
		_, err := Build(context.Background(), Deps{Store: f.store, DB: f.db, Semantic: retriever}, Request{Query: "预算"})
		if err == nil {
			t.Fatal("build succeeded after the scope guard lost its database")
		}
		if strings.Contains(err.Error(), "provider unavailable") {
			t.Fatalf("scope failure misreported as outage: %v", err)
		}
	})

	t.Run("successful_empty", func(t *testing.T) {
		f := degradationFixture(t)
		pkg, err := Build(context.Background(), Deps{
			Store:    f.store,
			DB:       f.db,
			Semantic: &funcSemantic{},
			Reranker: passthroughReranker{},
		}, Request{Query: "预算不存在的词"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if pkg.Degraded {
			t.Fatalf("successful empty search marked degraded: %q", pkg.DegradeReason)
		}
		if hasGap(pkg, "semantic_unavailable") || hasGap(pkg, "semantic_index_empty") {
			t.Fatalf("successful empty search reported a semantic gap: %+v", pkg.Gaps)
		}
		if pkg.Trace.Semantic.Indexed != 0 {
			t.Fatalf("indexed = %d, want 0", pkg.Trace.Semantic.Indexed)
		}
	})
}

// TestSemanticReadinessEmptyIndexIsNotOutage pins the remaining distinction:
// Ready(false, nil) is an empty index, not an outage.
func TestSemanticReadinessEmptyIndexIsNotOutage(t *testing.T) {
	f := degradationFixture(t)
	pkg := f.buildSemantic(&funcSemantic{readyFn: func(context.Context) (bool, error) { return false, nil }}, Request{Query: "预算"})
	if !hasGap(pkg, "semantic_index_empty") {
		t.Fatalf("gaps = %+v, want semantic_index_empty", pkg.Gaps)
	}
	if hasGap(pkg, "semantic_unavailable") {
		t.Fatalf("empty index reported as unavailable backend: %+v", pkg.Gaps)
	}
	if !containsString(itemIDs(pkg.Items), "t:budget") {
		t.Fatalf("lexical hit lost on empty index: %v", itemIDs(pkg.Items))
	}
}
