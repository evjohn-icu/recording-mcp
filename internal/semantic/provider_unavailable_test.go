package semantic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReadyPropagatesDatabaseErrors pins the distinction that matters for
// graceful degradation: a broken store must be an error, not an empty index,
// and a loaded in-memory graph must not hide a canonical database failure.
func TestReadyPropagatesDatabaseErrors(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db := openTestDB(t)
		r := &SQLiteRetriever{DB: db, Provider: NewHashProvider(64)}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		ready, err := r.Ready(context.Background())
		if err == nil {
			t.Fatalf("Ready = %v, nil; want a database error", ready)
		}
		if ready {
			t.Fatal("Ready reported ready on a closed database")
		}
	})

	t.Run("hnsw_loaded_graph", func(t *testing.T) {
		db := openTestDB(t)
		r := &HNSWRetriever{
			DB:       db,
			Provider: NewHashProvider(64),
			Exact:    &SQLiteRetriever{DB: db, Provider: NewHashProvider(64)},
			index:    &HNSW{Nodes: []hnswNode{{}}},
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		ready, err := r.Ready(context.Background())
		if err == nil {
			t.Fatalf("Ready = %v, nil; want a database error behind a loaded graph", ready)
		}
		if ready {
			t.Fatal("loaded graph masked the database failure")
		}
	})

	t.Run("hnsw_without_graph", func(t *testing.T) {
		db := openTestDB(t)
		r := &HNSWRetriever{
			DB:       db,
			Provider: NewHashProvider(64),
			Exact:    &SQLiteRetriever{DB: db, Provider: NewHashProvider(64)},
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		ready, err := r.Ready(context.Background())
		if err == nil {
			t.Fatalf("Ready = %v, nil; want a database error", ready)
		}
		if ready {
			t.Fatal("Ready reported ready on a closed database")
		}
	})

	t.Run("empty_index_is_not_an_error", func(t *testing.T) {
		db := openTestDB(t)
		r := &SQLiteRetriever{DB: db, Provider: NewHashProvider(64)}
		ready, err := r.Ready(context.Background())
		if err != nil || ready {
			t.Fatalf("Ready = %v, %v; want false, nil on an empty index", ready, err)
		}
	})
}

// TestProviderUnavailableClassification maps real HTTP failures to the closed
// reason set and proves no response body, token or URL leaks into the error.
func TestProviderUnavailableClassification(t *testing.T) {
	const secret = "SECRET-BODY-TOKEN-123"
	newProvider := func(endpoint string, timeout int) OpenAICompatProvider {
		p := OpenAICompatProvider{Endpoint: endpoint, APIKey: "sk-secret-key", ModelName: "m", Dimensions: 2}
		if timeout > 0 {
			p.Timeout = time.Duration(timeout) * time.Millisecond
		}
		return p
	}

	t.Run("http_503", func(t *testing.T) {
		srv := newEmbeddingServer(t, 503, `{"error":{"message":"`+secret+`"}}`)
		defer srv.Close()
		_, err := newProvider(srv.URL+"?token="+secret, 0).Embed(context.Background(), []string{"q"})
		assertUnavailable(t, err, ReasonProviderUnavailable, secret)
	})

	t.Run("model_not_found_structured_code", func(t *testing.T) {
		srv := newEmbeddingServer(t, 404, `{"error":{"message":"`+secret+`","code":"model_not_found"}}`)
		defer srv.Close()
		_, err := newProvider(srv.URL, 0).Embed(context.Background(), []string{"q"})
		assertUnavailable(t, err, ReasonModelUnavailable, secret)
	})

	t.Run("connection_refused", func(t *testing.T) {
		endpoint := closedEndpoint(t)
		_, err := newProvider(endpoint+"?token="+secret, 0).Embed(context.Background(), []string{"q"})
		assertUnavailable(t, err, ReasonConnectionRefused, secret)
	})

	t.Run("provider_timeout", func(t *testing.T) {
		srv := slowEmbeddingServer(t)
		defer srv.Close()
		_, err := newProvider(srv.URL, 50).Embed(context.Background(), []string{"q"})
		assertUnavailable(t, err, ReasonProviderTimeout, secret)
	})

	t.Run("request_canceled", func(t *testing.T) {
		srv := slowEmbeddingServer(t)
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newProvider(srv.URL, 0).Embed(ctx, []string{"q"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if IsProviderUnavailable(err) {
			t.Fatalf("caller cancellation classified as provider outage: %v", err)
		}
	})

	t.Run("request_deadline", func(t *testing.T) {
		srv := slowEmbeddingServer(t)
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1)*time.Millisecond)
		defer cancel()
		<-ctx.Done()
		_, err := newProvider(srv.URL, 0).Embed(ctx, []string{"q"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if IsProviderUnavailable(err) {
			t.Fatalf("caller deadline classified as provider outage: %v", err)
		}
	})

	t.Run("http_400_is_not_an_outage", func(t *testing.T) {
		srv := newEmbeddingServer(t, 400, `{"error":{"message":"bad request"}}`)
		defer srv.Close()
		_, err := newProvider(srv.URL, 0).Embed(context.Background(), []string{"q"})
		if err == nil || IsProviderUnavailable(err) {
			t.Fatalf("err = %v, want a non-degradable error", err)
		}
	})

	t.Run("invalid_vector_shape_is_not_an_outage", func(t *testing.T) {
		srv := newEmbeddingServer(t, 200, `{"data":[{"embedding":[1,2,3]}]}`)
		defer srv.Close()
		_, err := newProvider(srv.URL, 0).Embed(context.Background(), []string{"q"})
		if err == nil || IsProviderUnavailable(err) {
			t.Fatalf("err = %v, want a non-degradable shape error", err)
		}
	})
}

func assertUnavailable(t *testing.T, err error, reason, secret string) {
	t.Helper()
	var typed *ProviderUnavailableError
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want *ProviderUnavailableError", err)
	}
	if typed.Reason != reason {
		t.Fatalf("reason = %q, want %q", typed.Reason, reason)
	}
	message := typed.Error()
	for _, leak := range []string{secret, "sk-secret-key", "http://", "https://", "token="} {
		if strings.Contains(message, leak) {
			t.Fatalf("error leaked %q: %s", leak, message)
		}
	}
}

// newEmbeddingServer returns a test server that answers every embedding call
// with one fixed status and body.
func newEmbeddingServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// slowEmbeddingServer answers after a delay long enough for a short provider
// timeout to fire first.
func slowEmbeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
}

// closedEndpoint binds a loopback port and closes it so the connect attempt is
// refused.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + addr + "/v1/embeddings"
}
