package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/app"
	"recordingmcp/internal/cloudsync"
	"recordingmcp/internal/ingest"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/profile"
)

// TestProfilesAllowsLargeImportUpload guards the outer profile router: it used
// to apply the 1 MiB JSON limit to every /api/ path, truncating multipart audio
// before the child server could accept it.
func TestProfilesAllowsLargeImportUpload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	manager, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	password := "profiles-upload-password"
	paths, err := manager.Create("alpha", password)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	events, err := plane.Open(paths.Plane)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { events.Close() })

	factory := func(paths profile.Paths, verify func(string) bool) (*Server, error) {
		svc := app.New(store, nil, nil, paths.Audio)
		svc.ASRProvider = "fixture"
		importer, err := ingest.New(ingest.Config{
			Store: store, DB: store.DB(), AudioDir: paths.Audio,
			Decoder: &fakeAudioDecoder{}, Transcription: cloudsync.FixtureTranscriber{},
			ASRProvider: "fixture", TranscriptRevision: ingest.DefaultTranscriptRevision,
		})
		if err != nil {
			return nil, err
		}
		return New(Config{
			Service: svc, Importer: importer,
			TimeService: &app.TimeService{Store: store, Events: events},
			ProfileName: paths.Name, VerifyProfilePassword: verify,
		})
	}
	profiles, err := NewProfiles(ProfilesConfig{Manager: manager, Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(profiles.Handler())
	defer server.Close()

	loginBody, _ := json.Marshal(map[string]string{"profile": "alpha", "password": password})
	login, err := http.NewRequest(http.MethodPost, server.URL+"/api/profiles/login", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatal(err)
	}
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", server.URL)
	resp, err := http.DefaultClient.Do(login)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	var loginResult struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginResult); err != nil {
		t.Fatal(err)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login issued no cookies")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "large.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("a"), 2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	upload, err := http.NewRequest(http.MethodPost, server.URL+"/api/imports", &body)
	if err != nil {
		t.Fatal(err)
	}
	upload.Header.Set("Content-Type", writer.FormDataContentType())
	upload.Header.Set("Origin", server.URL)
	upload.Header.Set("X-CSRF-Token", loginResult.CSRFToken)
	for _, cookie := range cookies {
		upload.AddCookie(cookie)
	}
	uploadResp, err := http.DefaultClient.Do(upload)
	if err != nil {
		t.Fatal(err)
	}
	defer uploadResp.Body.Close()
	payload, _ := io.ReadAll(uploadResp.Body)
	if uploadResp.StatusCode != http.StatusAccepted {
		t.Fatalf("large upload through profile router: %d %s", uploadResp.StatusCode, payload)
	}
	var imported ingest.ImportResult
	if err := json.Unmarshal(payload, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.ConversationID == "" || imported.Duplicate {
		t.Fatalf("unexpected import result: %s", payload)
	}
	if !strings.Contains(string(payload), `"duration_ms":2000`) {
		t.Fatalf("probe result missing from response: %s", payload)
	}
}
