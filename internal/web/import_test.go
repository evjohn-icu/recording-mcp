package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/ingest"
)

type fakeAudioDecoder struct {
	info     ingest.AudioInfo
	probeErr error
}

func (d *fakeAudioDecoder) Probe(context.Context, string) (ingest.AudioInfo, error) {
	if d.probeErr != nil {
		return ingest.AudioInfo{}, d.probeErr
	}
	if d.info.DurationMS == 0 {
		d.info = ingest.AudioInfo{DurationMS: 2000, Format: "wav", SampleRate: 16000, Channels: 1}
	}
	return d.info, nil
}

func (d *fakeAudioDecoder) ToPCM(context.Context, string, string) error { return nil }

func attachImporter(t *testing.T, env *testEnv, cfg ingest.Config) *ingest.Service {
	t.Helper()
	cfg.Store = env.store
	cfg.DB = env.store.DB()
	cfg.AudioDir = env.audioDir
	if cfg.Decoder == nil {
		cfg.Decoder = &fakeAudioDecoder{}
	}
	if cfg.Transcription == nil && cfg.ASRProvider != "off" {
		cfg.Transcription = cloudsync.FixtureTranscriber{}
	}
	if cfg.ASRProvider == "" {
		cfg.ASRProvider = "fixture"
	}
	svc, err := ingest.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env.server.importer = svc
	return svc
}

func multipartBody(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func TestImportRouteAcceptsLargeMultipartAndKeepsJSONLimit(t *testing.T) {
	env := newTestEnv(t, "")
	attachImporter(t, env, ingest.Config{})

	// Two megabytes: larger than the 1 MiB JSON limit, far below the upload
	// limit, so it must be accepted.
	data := bytes.Repeat([]byte("a"), 2<<20)
	body, contentType := multipartBody(t, "file", "large.wav", data)
	resp := env.raw(t, http.MethodPost, "/api/imports", "", func(r *http.Request) {
		r.Body = io.NopCloser(body)
		r.ContentLength = int64(body.Len())
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Origin", env.http.URL)
		r.Header.Set("X-CSRF-Token", env.csrf)
		r.AddCookie(env.cookie)
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("large multipart upload: %d", resp.StatusCode)
	}

	// The same server must still reject a JSON body above 1 MiB.
	big := `{"summary":"` + strings.Repeat("b", 2<<20) + `"}`
	status, _ := env.do(t, http.MethodPut, "/api/conversations/"+env.convID+"/notes/note:one", big, nil)
	if status == http.StatusOK {
		t.Fatal("2 MiB JSON body was accepted")
	}
}

func TestImportRouteMapsIngestErrors(t *testing.T) {
	env := newTestEnv(t, "")
	attachImporter(t, env, ingest.Config{})

	upload := func(body *bytes.Buffer, ct string) (int, []byte) {
		resp := env.raw(t, http.MethodPost, "/api/imports", "", func(r *http.Request) {
			r.Body = io.NopCloser(body)
			r.ContentLength = int64(body.Len())
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Origin", env.http.URL)
			r.Header.Set("X-CSRF-Token", env.csrf)
			r.AddCookie(env.cookie)
		})
		defer resp.Body.Close()
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}

	// Unsupported extension.
	body, ct := multipartBody(t, "file", "notes.txt", []byte("text"))
	status, payload := upload(body, ct)
	if status != http.StatusUnsupportedMediaType || !strings.Contains(string(payload), "unsupported_format") {
		t.Fatalf("unsupported format: %d %s", status, payload)
	}

	// Corrupt audio (probe fails) maps to 422 with an action.
	env.server.importer = nil
	attachImporter(t, env, ingest.Config{Decoder: &fakeAudioDecoder{probeErr: errors.New("no audio stream")}})
	body, ct = multipartBody(t, "file", "broken.wav", []byte("junk"))
	status, payload = upload(body, ct)
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(payload), "invalid_audio") {
		t.Fatalf("invalid audio: %d %s", status, payload)
	}

	// ASR disabled maps to 409 with a next action.
	env.server.importer = nil
	attachImporter(t, env, ingest.Config{ASRProvider: "off"})
	body, ct = multipartBody(t, "file", "ok.wav", []byte("bytes"))
	status, payload = upload(body, ct)
	if status != http.StatusConflict || !strings.Contains(string(payload), "asr_unavailable") || !strings.Contains(string(payload), `"action"`) {
		t.Fatalf("asr unavailable: %d %s", status, payload)
	}

	// Non-multipart content type.
	if status, _ := upload(bytes.NewBufferString("{}"), "application/json"); status != http.StatusUnsupportedMediaType {
		t.Fatalf("non-multipart: %d", status)
	}
}

func TestImportRouteDisabledWithoutImporter(t *testing.T) {
	env := newTestEnv(t, "")
	body, ct := multipartBody(t, "file", "x.wav", []byte("bytes"))
	resp := env.raw(t, http.MethodPost, "/api/imports", "", func(r *http.Request) {
		r.Body = io.NopCloser(body)
		r.ContentLength = int64(body.Len())
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Origin", env.http.URL)
		r.Header.Set("X-CSRF-Token", env.csrf)
		r.AddCookie(env.cookie)
	})
	payload, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(payload), "import_disabled") {
		t.Fatalf("disabled importer: %d %s", resp.StatusCode, payload)
	}
}

func TestWriteImportErrorMapsBodyLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	writeImportError(rec, &http.MaxBytesError{Limit: 10})
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusRequestEntityTooLarge || body.Error.Code != "file_too_large" || body.Error.Details["action"] == "" {
		t.Fatalf("body limit: %d %+v", rec.Code, body)
	}
}
