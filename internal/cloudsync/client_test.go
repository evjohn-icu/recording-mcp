package cloudsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// requireSyncHeaders asserts the frozen transport baseline on every /v1 request.
func requireSyncHeaders(t testing.TB, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("X-Ingest-Version"); got != ProtocolVersion {
		t.Errorf("X-Ingest-Version = %q, want %q", got, ProtocolVersion)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer test-sync-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, SyncToken: "test-sync-token"}
}

func TestClientSyncEventsPaging(t *testing.T) {
	events := make([]Event, 5)
	for i := range events {
		events[i] = Event{ID: int64(i + 1), Type: "recording.created", RecordingID: "r", Payload: json.RawMessage(`{}`), CreatedAt: "2026-10-14T00:00:00Z"}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		requireSyncHeaders(t, r)
		cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 200
		}
		var page []Event
		for _, ev := range events {
			if ev.ID > cursor && len(page) < limit {
				page = append(page, ev)
			}
		}
		next := cursor
		if len(page) > 0 {
			next = page[len(page)-1].ID
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(EventPage{
			Events:                page,
			NextCursor:            next,
			HasMore:               next < events[len(events)-1].ID,
			MaxEventID:            events[len(events)-1].ID,
			ServerProtocolVersion: ProtocolVersion,
		})
	})
	c := newTestClient(t, mux)

	page1, err := c.SyncEvents(context.Background(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Events) != 2 || page1.Events[0].ID != 1 || page1.Events[1].ID != 2 {
		t.Fatalf("page1 = %+v", page1.Events)
	}
	if page1.NextCursor != 2 || !page1.HasMore || page1.MaxEventID != 5 {
		t.Errorf("page1 paging fields = %d %v %d", page1.NextCursor, page1.HasMore, page1.MaxEventID)
	}
	page2, err := c.SyncEvents(context.Background(), page1.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Events) != 2 || page2.Events[0].ID != 3 {
		t.Fatalf("page2 = %+v", page2.Events)
	}
	if page2.NextCursor != 4 || !page2.HasMore {
		t.Errorf("page2 paging fields = %d %v", page2.NextCursor, page2.HasMore)
	}
	empty, err := c.SyncEvents(context.Background(), 5, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Events) != 0 || empty.NextCursor != 5 || empty.HasMore {
		t.Errorf("empty page = %+v", empty)
	}
}

func TestClientCursorExpired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "cursor_expired",
				"message": "cursor older than retention window",
				"details": map[string]any{"min_available_cursor": 42},
			},
		})
	})
	c := newTestClient(t, mux)

	_, err := c.SyncEvents(context.Background(), 10, 200)
	if err == nil {
		t.Fatal("want error")
	}
	if !IsCursorExpired(err) {
		t.Fatalf("IsCursorExpired(%v) = false", err)
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatal("error is not *APIError")
	}
	if apiErr.Status != 410 || apiErr.Code != "cursor_expired" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if apiErr.Details["min_available_cursor"].(float64) != 42 {
		t.Errorf("details = %+v", apiErr.Details)
	}
}

func TestClientErrorMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sync/recordings/r1/ack", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "internal", "message": "boom", "details": map[string]any{}},
		})
	})
	mux.HandleFunc("POST /v1/sync/recordings/r2/ack", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not json", http.StatusBadGateway)
	})
	c := newTestClient(t, mux)

	err := c.Ack(context.Background(), "r1", 7, "ingested", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Status != 500 || apiErr.Code != "internal" || apiErr.Message != "boom" {
		t.Fatalf("r1 err = %v (%+v)", err, apiErr)
	}
	if IsCursorExpired(err) {
		t.Error("500 must not be cursor_expired")
	}
	err = c.Ack(context.Background(), "r2", 7, "ingested", "")
	if !asAPIError(err, &apiErr) || apiErr.Status != 502 || apiErr.Code != "" {
		t.Fatalf("r2 err = %v (%+v)", err, apiErr)
	}
}

func TestClientAckBody(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sync/recordings/r1/ack", func(w http.ResponseWriter, r *http.Request) {
		requireSyncHeaders(t, r)
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"synced_at": "2026-10-14T00:00:00Z", "transcription_state": "ingested"})
	})
	c := newTestClient(t, mux)

	if err := c.Ack(context.Background(), "r1", 12, "ingested", ""); err != nil {
		t.Fatal(err)
	}
	if gotBody["protocol_version"] != ProtocolVersion || gotBody["last_event_id"].(float64) != 12 || gotBody["transcription_state"] != "ingested" {
		t.Errorf("ack body = %+v", gotBody)
	}
	if detail, ok := gotBody["detail"]; !ok || detail != nil {
		t.Errorf("empty detail must serialize as null, got %v", detail)
	}
	if err := c.Ack(context.Background(), "r1", 12, "failed", "checksum_mismatch"); err != nil {
		t.Fatal(err)
	}
	if gotBody["detail"] != "checksum_mismatch" {
		t.Errorf("detail = %v", gotBody["detail"])
	}
}

func TestClientDownloadChunk(t *testing.T) {
	body := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/recordings/r1/chunks/3/audio", func(w http.ResponseWriter, r *http.Request) {
		requireSyncHeaders(t, r)
		w.Header().Set("X-Content-SHA256", sha256HexTest(body))
		w.Header().Set("Content-Type", "audio/L16")
		w.Write(body)
	})
	c := newTestClient(t, mux)

	var out []byte
	meta, err := c.DownloadChunk(context.Background(), "r1", 3, &sliceWriter{&out})
	if err != nil {
		t.Fatal(err)
	}
	if meta.SHA256 != sha256HexTest(body) || meta.Bytes != int64(len(body)) {
		t.Errorf("meta = %+v", meta)
	}
	if string(out) != string(body) {
		t.Errorf("body = %v", out)
	}
}

type sliceWriter struct{ b *[]byte }

func (w *sliceWriter) Write(p []byte) (int, error) { *w.b = append(*w.b, p...); return len(p), nil }

func TestClientListRecordings(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sync/recordings", func(w http.ResponseWriter, r *http.Request) {
		requireSyncHeaders(t, r)
		if r.URL.Query().Get("after") != "rec-1" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("query = %v", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(RecordingPage{
			Recordings: []RecordingDetail{{RecordingID: "rec-2", State: "complete"}},
			NextAfter:  new("rec-2"),
			HasMore:    false,
		})
	})
	c := newTestClient(t, mux)
	page, err := c.ListRecordings(context.Background(), "rec-1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Recordings) != 1 || page.Recordings[0].RecordingID != "rec-2" || page.NextAfter == nil || *page.NextAfter != "rec-2" || page.HasMore {
		t.Errorf("page = %+v", page)
	}
}

func TestClientHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": ProtocolVersion, "now": "2026-10-14T00:00:00Z"})
	})
	c := newTestClient(t, mux)
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientDefaultsHTTPTimeout(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", SyncToken: "t"}
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected connection error")
	}
}

func sha256HexTest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
