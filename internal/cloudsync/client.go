// Package cloudsync pulls recording events and audio from an ingest-v1
// capture service (Cloudflare Workers + D1 + R2) into the local memory
// database. The HTTP client speaks the frozen contracts/ingest-v1 wire
// protocol; the engine persists every event's local effects in one SQLite
// transaction together with the cursor advance, so a crash never loses a
// downloaded chunk's bookkeeping.
package cloudsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProtocolVersion is the frozen wire-protocol identifier; every request
// carries it in X-Ingest-Version and every JSON body repeats it.
const ProtocolVersion = "ingest-v1"

// defaultLimit is the sync page size used when callers pass limit <= 0.
const defaultLimit = 200

// requestTimeout bounds every client HTTP round trip.
const requestTimeout = 30 * time.Second

// Client is a minimal ingest-v1 sync-credential client. A nil HTTP field
// means a default client with requestTimeout applied.
type Client struct {
	BaseURL   string
	SyncToken string
	HTTP      *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: requestTimeout}
}

// APIError is a parsed ingest-v1 error envelope. Code is the frozen literal
// from contracts/ingest-v1 (e.g. cursor_expired); Status is the HTTP status.
type APIError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("cloudsync: HTTP %d", e.Status)
	}
	return fmt.Sprintf("cloudsync: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

// IsCursorExpired reports whether err is the 410 cursor_expired response that
// triggers snapshot recovery.
func IsCursorExpired(err error) bool {
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusGone || apiErr.Code == "cursor_expired"
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// Event is one durable event-log row from GET /v1/sync.
type Event struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	RecordingID string          `json:"recording_id"`
	Seq         int64           `json:"seq"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   string          `json:"created_at"`
}

// EventPage is one GET /v1/sync response.
type EventPage struct {
	Events                []Event `json:"events"`
	NextCursor            int64   `json:"next_cursor"`
	HasMore               bool    `json:"has_more"`
	MaxEventID            int64   `json:"max_event_id"`
	ServerProtocolVersion string  `json:"server_protocol_version"`
}

// Event payloads, exactly as frozen in contracts/ingest-v1 README section 4.7.
type RecordingCreatedPayload struct {
	Codec      string  `json:"codec"`
	SampleRate int     `json:"sample_rate"`
	Channels   int     `json:"channels"`
	RecordedAt *string `json:"recorded_at"`
	Timezone   *string `json:"timezone"`
	// Owner and DeviceID are optional convenience echoes; the frozen payload
	// does not include them, so the engine falls back to defaults when absent.
	Owner    *string `json:"owner,omitempty"`
	DeviceID *string `json:"device_id,omitempty"`
}

type ChunkCommittedPayload struct {
	Seq           int64  `json:"seq"`
	SampleOffset  int64  `json:"sample_offset"`
	SampleCount   int64  `json:"sample_count"`
	Bytes         int64  `json:"bytes"`
	DurationMS    int64  `json:"duration_ms"`
	ContentSHA256 string `json:"content_sha256"`
	R2Key         string `json:"r2_key"`
}

type RecordingFinishedPayload struct {
	TotalChunks     int64 `json:"total_chunks"`
	TotalSamples    int64 `json:"total_samples"`
	TotalDurationMS int64 `json:"total_duration_ms"`
}

type RecordingDeletedPayload struct {
	Reason string `json:"reason"`
}

// ChunkInfo is one entry of the endpoint-6 chunk manifest.
type ChunkInfo struct {
	Seq           int64  `json:"seq"`
	Bytes         int64  `json:"bytes"`
	SampleOffset  int64  `json:"sample_offset"`
	SampleCount   int64  `json:"sample_count"`
	ContentSHA256 string `json:"content_sha256"`
	UploadedAt    string `json:"uploaded_at"`
}

// RecordingDetail is the endpoint-6 recording structure, also used by the
// /v1/sync/recordings snapshot listing.
type RecordingDetail struct {
	RecordingID     string      `json:"recording_id"`
	State           string      `json:"state"`
	Codec           string      `json:"codec"`
	SampleRate      int         `json:"sample_rate"`
	Channels        int         `json:"channels"`
	RecordedAt      *string     `json:"recorded_at"`
	CreatedAt       string      `json:"created_at"`
	FinishedAt      *string     `json:"finished_at"`
	TotalChunks     *int64      `json:"total_chunks"`
	TotalSamples    *int64      `json:"total_samples"`
	TotalDurationMS *int64      `json:"total_duration_ms"`
	Chunks          []ChunkInfo `json:"chunks"`
	Sync            struct {
		SyncedAt           *string `json:"synced_at"`
		TranscriptionState string  `json:"transcription_state"`
	} `json:"sync"`
}

// RecordingPage is one GET /v1/sync/recordings response.
type RecordingPage struct {
	Recordings []RecordingDetail `json:"recordings"`
	NextAfter  *string           `json:"next_after"`
	HasMore    bool              `json:"has_more"`
}

// ChunkMeta describes one downloaded chunk audio body: the server-asserted
// content hash from X-Content-SHA256 and the exact byte count received.
type ChunkMeta struct {
	SHA256 string
	Bytes  int64
}

// do issues one authenticated ingest-v1 request. It always sends
// X-Ingest-Version and the Bearer token, decodes the frozen error envelope on
// non-2xx, and returns the response for streaming bodies.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := strings.TrimSuffix(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Ingest-Version", ProtocolVersion)
	if c.SyncToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.SyncToken)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res, nil
	}
	defer res.Body.Close()
	apiErr := &APIError{Status: res.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var env errorEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
		apiErr.Code = env.Error.Code
		apiErr.Message = env.Error.Message
		apiErr.Details = env.Error.Details
	} else {
		apiErr.Message = strings.TrimSpace(string(raw))
	}
	return nil, apiErr
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	res, err := c.do(ctx, http.MethodGet, path, query, nil, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("cloudsync: decode %s response: %w", path, err)
	}
	return nil
}

// SyncEvents fetches one page of events strictly after cursor, in ascending
// id order. limit <= 0 uses the server default page size.
func (c *Client) SyncEvents(ctx context.Context, cursor int64, limit int) (EventPage, error) {
	q := url.Values{"cursor": {strconv.FormatInt(cursor, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var page EventPage
	if err := c.getJSON(ctx, "/v1/sync", q, &page); err != nil {
		return EventPage{}, err
	}
	return page, nil
}

// DownloadChunk streams one chunk audio body into w while hashing it. The
// returned meta carries the server's X-Content-SHA256 assertion and the exact
// byte count written; callers verify both against the chunk.committed payload.
func (c *Client) DownloadChunk(ctx context.Context, recordingID string, seq int, w io.Writer) (ChunkMeta, error) {
	path := fmt.Sprintf("/v1/recordings/%s/chunks/%d/audio", recordingID, seq)
	res, err := c.do(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return ChunkMeta{}, err
	}
	defer res.Body.Close()
	hash := sha256.New()
	counting := &countingWriter{w: w, hash: hash}
	if _, err := io.Copy(counting, res.Body); err != nil {
		return ChunkMeta{}, fmt.Errorf("cloudsync: download %s: %w", path, err)
	}
	return ChunkMeta{SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: counting.n}, nil
}

type countingWriter struct {
	w    io.Writer
	hash io.Writer // sha256 hasher; nil when the caller only counts
	n    int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	if cw.hash != nil && n > 0 {
		if _, herr := cw.hash.Write(p[:n]); herr != nil {
			return n, herr
		}
	}
	return n, err
}

// ListRecordings pages the full snapshot listing used for cursor-expiry
// recovery. after is the last recording_id of the previous page ("" for the
// first page); limit <= 0 uses the server default.
func (c *Client) ListRecordings(ctx context.Context, after string, limit int) (RecordingPage, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var page RecordingPage
	if err := c.getJSON(ctx, "/v1/sync/recordings", q, &page); err != nil {
		return RecordingPage{}, err
	}
	return page, nil
}

// Ack reports transcription progress for one recording back to the server so
// the Android client can read synced_at/transcription_state via endpoint 6.
func (c *Client) Ack(ctx context.Context, recordingID string, lastEventID int64, state, detail string) error {
	body := struct {
		ProtocolVersion    string  `json:"protocol_version"`
		LastEventID        int64   `json:"last_event_id"`
		TranscriptionState string  `json:"transcription_state"`
		Detail             *string `json:"detail"`
	}{ProtocolVersion: ProtocolVersion, LastEventID: lastEventID, TranscriptionState: state}
	if detail != "" {
		body.Detail = &detail
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/v1/sync/recordings/%s/ack", recordingID)
	res, err := c.do(ctx, http.MethodPost, path, nil, strings.NewReader(string(raw)), "application/json")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// Health checks the unauthenticated /healthz endpoint.
func (c *Client) Health(ctx context.Context) error {
	res, err := c.do(ctx, http.MethodGet, "/healthz", nil, nil, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return fmt.Errorf("cloudsync: decode /healthz: %w", err)
	}
	if body.Status != "ok" {
		return fmt.Errorf("cloudsync: healthz status %q", body.Status)
	}
	return nil
}
