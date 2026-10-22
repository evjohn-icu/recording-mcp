package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/app"
	"recordingmcp/internal/asrinput"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/plane"
	"recordingmcp/internal/profile"
)

const retirementTestPassword = "retirement recovery password"

func retirementRequest(t *testing.T, server *httptest.Server, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", server.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, bodyBytes
}

func newRetirementTestProfiles(t *testing.T, manager *profile.Manager, backupPath string) (*httptest.Server, *[]*memory.Store, *[]*plane.Service) {
	t.Helper()
	stores := new([]*memory.Store)
	events := new([]*plane.Service)
	router, err := NewProfiles(ProfilesConfig{Manager: manager, Factory: func(paths profile.Paths, verifier func(string) bool) (*Server, error) {
		store, err := memory.Open(paths.Database)
		if err != nil {
			return nil, err
		}
		*stores = append(*stores, store)
		planeStore, err := plane.Open(paths.Plane)
		if err != nil {
			return nil, err
		}
		*events = append(*events, planeStore)
		svc := app.New(store, nil, nil, paths.Audio)
		svc.ForgetPlane = planeStore
		svc.ManagedIndexDir = paths.Index
		svc.ManagedBackupDir = backupPath
		return New(Config{Service: svc, ProfileName: paths.Name, VerifyProfilePassword: verifier})
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router.Handler())
	t.Cleanup(func() {
		server.Close()
		for _, e := range *events {
			_ = e.Close()
		}
		for _, s := range *stores {
			_ = s.Close()
		}
	})
	return server, stores, events
}

func retireBody(name string) string {
	return `{"profile":"` + name + `","confirm_profile":"` + name + `","password":"` + retirementTestPassword + `"}`
}

func TestProfileRetirementRetryAfterDispatcherRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	manager, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create("alice", retirementTestPassword); err != nil {
		t.Fatal(err)
	}
	// A regular file at the managed backup root injects a deterministic failure
	// after the durable DB/plane fence, leaving a retryable retired profile.
	backupPath := filepath.Join(t.TempDir(), "blocked-backups")
	if err := os.WriteFile(backupPath, []byte("block cleanup"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, firstStores, firstEvents := newRetirementTestProfiles(t, manager, backupPath)
	if status, _ := retirementRequest(t, first, "/api/profiles/retire", retireBody("alice")); status != http.StatusAccepted {
		t.Fatalf("first retirement status=%d, want pending 202", status)
	}
	// Dispose old process handles before simulating service restart.
	for _, e := range *firstEvents {
		_ = e.Close()
	}
	*firstEvents = nil
	for _, s := range *firstStores {
		_ = s.Close()
	}
	*firstStores = nil

	manager2, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _ := newRetirementTestProfiles(t, manager2, backupPath)
	loginBody := `{"profile":"alice","password":"` + retirementTestPassword + `"}`
	if status, _ := retirementRequest(t, second, "/api/profiles/login", loginBody); status != http.StatusUnauthorized {
		t.Fatalf("login after pending retirement status=%d, want 401", status)
	}
	if status, _ := retirementRequest(t, second, "/api/profiles/retire", retireBody("alice")); status != http.StatusAccepted {
		t.Fatalf("password retry before external fix status=%d, want pending 202", status)
	}
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	if status, body := retirementRequest(t, second, "/api/profiles/retire", retireBody("alice")); status != http.StatusOK || !strings.Contains(string(body), `"status":"complete"`) {
		t.Fatalf("password retry after restart status=%d body=%s", status, body)
	}
}

func TestProfileRetirementPurgesServiceReadsBeforeWaitingForAudioClip(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "profiles")
	manager, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := manager.Create("alice", retirementTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := plane.Open(paths.Plane)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	started := "2026-10-20T09:00:00Z"
	startMS, endMS := int64(100), int64(400)
	result := asrinput.Result{
		ContractVersion: asrinput.ContractVersion,
		Source:          asrinput.Source{Provider: "retirement-test", ProviderRequestID: "request-retire", DeviceID: "device-retire", RecordingID: "recording-retire", ConversationID: "conversation-retire", SegmentID: "segment-retire", SegmentOffsetMS: 0, SegmentDurationMS: 1000, TranscriptRevision: "r1", CompletionStatus: "final", RecordingStartedAt: &started},
		Turns:           []asrinput.Turn{{ID: "turn-retire", OriginalIndex: 1, Text: "private retirement transcript", LocalStartMS: &startMS, LocalEndMS: &endMS, GlobalStartMS: &startMS, GlobalEndMS: &endMS}},
	}
	report, err := store.IngestTranscript(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := store.DB().QueryRowContext(ctx, `SELECT id FROM turns WHERE recording_id=?`, report.RecordingID).Scan(&id); err != nil {
		t.Fatal("ingested turn was not queryable:", err)
	}
	stitched := filepath.Join(paths.Audio, "recording-retire.pcm")
	pcm := make([]byte, 16000*2)
	if err := os.WriteFile(stitched, pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,transcription_state,updated_at) VALUES('recording-retire','default','device-retire','ingested',?,'ingested',1)`, stitched); err != nil {
		t.Fatal(err)
	}
	svc := app.New(store, nil, nil, paths.Audio)
	svc.ForgetPlane = events
	if _, err := events.Ingest(ctx, []plane.Event{{ID: "event-retire", Type: "fact.observed", Timestamp: time.Now(), ObservedAt: time.Now(), Source: "test", Content: "private plane content", StructuredData: map[string]any{"scope": "alice", "dimension": "person"}}}); err != nil {
		t.Fatal(err)
	}
	clip, err := svc.AudioClip(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer clip.Close()
	if err := svc.BeginProfileRetirement(ctx); err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- svc.CleanupRetiredProfile(ctx, paths.Tasks) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, readErr := svc.ListConversations(ctx, memory.ConversationFilter{})
		if readErr == nil && len(page.Items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation reads retained content after retirement fence: page=%+v err=%v", page, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	page, err := svc.Query(ctx, memory.Query{ConversationID: report.ConversationID, IncludeTurns: true})
	if len(page.Items) != 0 || err != nil && !strings.Contains(err.Error(), "conversation not found") {
		t.Fatalf("service query after retirement purge returned %+v, err=%v", page, err)
	}
	history, err := events.History(ctx, "alice")
	if err != nil || len(history) != 0 {
		t.Fatalf("plane history after retirement purge returned %d events, err=%v", len(history), err)
	}
	select {
	case err := <-cleanupDone:
		t.Fatalf("physical cleanup completed before active audio clip released: %v", err)
	default:
	}
	if err := clip.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cleanupDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not resume after audio clip closed")
	}
}
