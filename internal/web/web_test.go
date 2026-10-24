package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/app"
	"recordingmcp/internal/memory"
)

// testAnnotations builds one conversation whose recording id is the hashed
// memory id of the provider pair the source_attachments row will carry, so the
// audio provenance bridge can be exercised end to end.
func testAnnotations(providerDevice, providerRecording string) memory.Annotations {
	return memory.Annotations{
		Version:           1,
		ConversationID:    "conv:web",
		Title:             "Web 测试会话",
		RecordingID:       memory.RecordingID(providerDevice, providerRecording),
		AnnotationVersion: "v1",
		Summary:           "一条待确认的归纳",
		Topics:            []memory.TopicAnnotation{{ID: "topic:web", Title: "预算"}},
		Notes: []memory.NoteAnnotation{
			{ID: "note:one", Kind: "observation", TopicIDs: []string{"topic:web"}, Summary: "预算需要复查", EvidenceTurnIDs: []string{"t1"}},
		},
	}
}

func testTranscript() memory.Transcript {
	var tr memory.Transcript
	tr.AudioFileName = "synthetic.wav"
	tr.DurationSeconds = 1.0
	tr.Coverage.StartMS = 0
	tr.Coverage.EndMS = 1000
	tr.Turns = []memory.Turn{{
		ID: "t1", SegmentIndex: 1, SegmentStartMS: 0, SegmentEndMS: 1000,
		GlobalStartMS: new(int64(0)), GlobalEndMS: new(int64(1000)), Text: "预算需要复查的讨论",
	}}
	return tr
}

// testEnv is one server over a fresh store, already authenticated.
type testEnv struct {
	server   *Server
	http     *httptest.Server
	store    *memory.Store
	convID   string
	audioDir string
	csrf     string
	cookie   *http.Cookie
}

func newTestEnv(t *testing.T, webDir string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	audioDir := filepath.Join(dir, "audio")
	if err := os.MkdirAll(audioDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ann := testAnnotations("dev-1", "rec-1")
	if _, err := store.Import(context.Background(), testTranscript(), ann, "synthetic.wav", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Service:        app.New(store, nil, nil, audioDir),
		WebDir:         webDir,
		BootstrapToken: "test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{server: server, http: httptest.NewServer(server.Handler()), store: store, convID: ann.ConversationID, audioDir: audioDir}
	t.Cleanup(env.http.Close)

	resp := env.raw(t, http.MethodPost, "/api/session", `{"token":"test-token"}`, func(r *http.Request) { r.Header.Set("Origin", env.http.URL) })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap failed: %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.CSRFToken == "" {
		t.Fatal("no csrf token issued")
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			env.cookie = c
		}
	}
	if env.cookie == nil {
		t.Fatal("no session cookie issued")
	}
	env.csrf = body.CSRFToken
	return env
}

// raw performs a request without the session cookie or write headers.
func (e *testEnv) raw(t *testing.T, method, path, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.http.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// do performs an authenticated request with the default write protections.
func (e *testEnv) do(t *testing.T, method, path, body string, mutate func(*http.Request)) (int, []byte) {
	t.Helper()
	resp := e.raw(t, method, path, body, func(r *http.Request) {
		r.AddCookie(e.cookie)
		if method != http.MethodGet {
			r.Header.Set("Origin", e.http.URL)
			r.Header.Set("X-CSRF-Token", e.csrf)
		}
		if mutate != nil {
			mutate(r)
		}
	})
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func decodeError(t *testing.T, data []byte) errorDetail {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("not a JSON error envelope: %s", data)
	}
	return body.Error
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	env := newTestEnv(t, "")
	for _, path := range []string{"/api/conversations", "/api/session", "/api/topics"} {
		resp := env.raw(t, http.MethodGet, path, "", nil)
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: got %d want 401", path, resp.StatusCode)
		}
		if e := decodeError(t, data); e.Code != app.CodeUnauthenticated {
			t.Fatalf("%s: unexpected code %q", path, e.Code)
		}
	}
}

func TestPendingForgetListIsReadOnlyAndPathFree(t *testing.T) {
	env := newTestEnv(t, "")
	ctx := context.Background()
	req := memory.ForgetRequest{Scope: "conversation", ConversationID: env.convID}
	preview, err := env.store.PreviewForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedState = preview.ExpectedState
	result, err := env.store.CommitForget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	before, err := env.store.ForgetStatus(ctx, result.TombstoneID)
	if err != nil || before.Status != "pending_external_cleanup" {
		t.Fatalf("fixture did not create pending tombstone: %+v err=%v", before, err)
	}
	status, body := env.do(t, http.MethodGet, "/api/forget", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET pending forget list status=%d body=%s", status, body)
	}
	var response struct {
		Operations []app.PendingForgetOperation `json:"operations"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Operations) != 1 || response.Operations[0].ID != result.TombstoneID || response.Operations[0].Status != "pending_external_cleanup" {
		t.Fatalf("unexpected pending list: %+v", response)
	}
	if strings.Contains(string(body), "path") || strings.Contains(string(body), "files") {
		t.Fatalf("pending list exposed file metadata: %s", body)
	}
	after, err := env.store.ForgetStatus(ctx, result.TombstoneID)
	if err != nil || after.Status != "pending_external_cleanup" {
		t.Fatalf("GET list triggered cleanup: %+v err=%v", after, err)
	}
}

func TestSessionBootstrapIsSingleUse(t *testing.T) {
	env := newTestEnv(t, "")
	resp := env.raw(t, http.MethodPost, "/api/session", `{"token":"test-token"}`, func(r *http.Request) { r.Header.Set("Origin", env.http.URL) })
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed bootstrap token accepted: %d", resp.StatusCode)
	}
	resp = env.raw(t, http.MethodPost, "/api/session", `{"token":"wrong"}`, func(r *http.Request) { r.Header.Set("Origin", env.http.URL) })
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", resp.StatusCode)
	}
	status, data := env.do(t, http.MethodGet, "/api/session", "", nil)
	if status != http.StatusOK {
		t.Fatalf("session read failed: %d", status)
	}
	var session map[string]any
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	if session["authenticated"] != true || session["csrf_token"] != env.csrf {
		t.Fatalf("unexpected session body: %s", data)
	}
}

func TestProfileLoginSessionIsBoundAndLogoutRevokesIt(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "profile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(Config{
		Service:               app.New(store, nil, nil, filepath.Join(dir, "audio")),
		ProfileName:           "alice",
		VerifyProfilePassword: func(password string) bool { return password == "correct password" },
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	request := func(method, path, body string, cookie *http.Cookie, csrf string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", httpServer.URL)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := request(http.MethodPost, "/api/login", `{"password":"wrong password"}`, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", resp.StatusCode)
	}
	resp = request(http.MethodPost, "/api/login", `{"password":"correct password"}`, nil, "")
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("profile login status = %d: %s", resp.StatusCode, data)
	}
	var body struct {
		CSRFToken string `json:"csrf_token"`
		Profile   string `json:"profile"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	cookies := resp.Cookies()
	resp.Body.Close()
	if body.Profile != "alice" || body.CSRFToken == "" || len(cookies) == 0 {
		t.Fatalf("incomplete login response: %+v cookies=%v", body, cookies)
	}
	var cookie *http.Cookie
	for _, candidate := range cookies {
		if candidate.Name == sessionCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("profile login did not set session cookie")
	}
	resp = request(http.MethodGet, "/api/session", "", cookie, "")
	var sessionBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sessionBody); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sessionBody["profile"] != "alice" || sessionBody["authenticated"] != true {
		t.Fatalf("session not bound to profile: %v", sessionBody)
	}
	resp = request(http.MethodPost, "/api/logout", "{}", cookie, body.CSRFToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	resp = request(http.MethodGet, "/api/session", "", cookie, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d, want 401", resp.StatusCode)
	}
}

func TestProfilePasswordChangeRevokesSessions(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "profile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	password := "current password"
	server, err := New(Config{
		Service: app.New(store, nil, nil, filepath.Join(dir, "audio")), ProfileName: "alice",
		VerifyProfilePassword: func(candidate string) bool { return candidate == password },
		ChangeProfilePassword: func(oldPassword, next string) error {
			if oldPassword != password {
				return errors.New("invalid current password")
			}
			password = next
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	request := func(method, path, body string, cookie *http.Cookie, csrf string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", httpServer.URL)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := request(http.MethodPost, "/api/login", `{"password":"current password"}`, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	resp.Body.Close()
	resp = request(http.MethodPost, "/api/profile/password", `{"current_password":"current password","new_password":"replacement password"}`, cookie, login.CSRF)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("password change: %d", resp.StatusCode)
	}
	resp = request(http.MethodGet, "/api/session", "", cookie, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session survived password change: %d", resp.StatusCode)
	}
	resp = request(http.MethodPost, "/api/login", `{"password":"current password"}`, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still worked: %d", resp.StatusCode)
	}
	resp = request(http.MethodPost, "/api/login", `{"password":"replacement password"}`, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new password failed: %d", resp.StatusCode)
	}
}

func TestBootstrapRequiresValidOriginAndHost(t *testing.T) {
	env := newTestEnv(t, "")
	resp := env.raw(t, http.MethodPost, "/api/session", `{"token":"test-token"}`, func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid bootstrap origin: %d", resp.StatusCode)
	}
	resp = env.raw(t, http.MethodGet, "/healthz", "", func(r *http.Request) { r.Host = "attacker.example" })
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback Host accepted: %d", resp.StatusCode)
	}
}

func TestAudioRejectsSymlinkEscape(t *testing.T) {
	env := newTestEnv(t, "")
	page, err := env.store.Query(context.Background(), memory.Query{ConversationID: env.convID, IncludeTurns: true})
	if err != nil {
		t.Fatal(err)
	}
	turnID := ""
	for _, item := range page.Items {
		if item.Kind == "turn" {
			turnID = item.ID
		}
	}
	if turnID == "" {
		t.Fatal("no fixture turn")
	}
	outside := filepath.Join(t.TempDir(), "outside.pcm")
	if err := os.WriteFile(outside, make([]byte, 32000), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(env.audioDir, "link.pcm")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := env.store.DB().Exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision) VALUES('att-audio',?,?,'1','seg-audio','rev-audio','{"device_id":"dev-1","recording_id":"rec-1"}','{}','x',1,NULL)`, env.convID, memory.RecordingID("dev-1", "rec-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().Exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,stitched_path,updated_at) VALUES('rec-1','default','dev-1','ingested',?,1)`, link); err != nil {
		t.Fatal(err)
	}
	status, data := env.do(t, http.MethodGet, "/api/turns/"+turnID+"/audio", "", nil)
	if status != http.StatusNotFound || decodeError(t, data).Code != app.CodeAudioMissing {
		t.Fatalf("symlink audio served: %d %s", status, data)
	}
}

func TestWriteRequestsRequireOriginAndCSRF(t *testing.T) {
	env := newTestEnv(t, "")
	path := "/api/conversations/" + env.convID + "/notes/" + memory.NoteRowID(env.convID, "note:one")
	body := `{"expected_version":"v1","new_version":"web-1","summary":"改"}`

	status, data := env.do(t, http.MethodPut, path, body, func(r *http.Request) {
		r.Header.Del("X-CSRF-Token")
	})
	if status != http.StatusForbidden {
		t.Fatalf("missing csrf: got %d want 403", status)
	}
	if e := decodeError(t, data); e.Code != app.CodeCSRFRejected {
		t.Fatalf("unexpected code: %q", e.Code)
	}

	status, data = env.do(t, http.MethodPut, path, body, func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
	})
	if status != http.StatusForbidden {
		t.Fatalf("bad origin: got %d want 403", status)
	}
	if e := decodeError(t, data); e.Code != app.CodeOriginRejected {
		t.Fatalf("unexpected code: %q", e.Code)
	}

	status, data = env.do(t, http.MethodPut, path, body, func(r *http.Request) {
		r.Header.Del("Origin")
	})
	if status != http.StatusForbidden {
		t.Fatalf("missing origin: got %d want 403", status)
	}
	if e := decodeError(t, data); e.Code != app.CodeOriginRejected {
		t.Fatalf("unexpected code: %q", e.Code)
	}

	if status, data = env.do(t, http.MethodPut, path, body, nil); status != http.StatusOK {
		t.Fatalf("valid write failed: %d %s", status, data)
	}
}

func TestValidationErrors(t *testing.T) {
	env := newTestEnv(t, "")
	cases := []struct{ path, message string }{
		{"/api/search", "q"},
		{"/api/search?q=%20", "q"},
		{"/api/search?q=x&scope=everything", "scope"},
		{"/api/search/hybrid", "q"},
		{"/api/search/hybrid?q=x&mode=fuzzy", "mode"},
		{"/api/conversations/" + "conv:nope" + "/turns", "recording_id"},
		{"/api/search?q=x&started_after=2026-03-01", "started_after"},
		{"/api/search?q=x&limit=abc", "limit"},
	}
	for _, tc := range cases {
		status, data := env.do(t, http.MethodGet, tc.path, "", nil)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: got %d want 400 (%s)", tc.path, status, data)
		}
		if e := decodeError(t, data); e.Code != app.CodeValidationFailed {
			t.Fatalf("%s: unexpected code %q", tc.path, e.Code)
		}
	}
	// max_items is clamped, never rejected.
	status, data := env.do(t, http.MethodGet, "/api/search/hybrid?q=%E9%A2%84%E7%AE%97&max_items=9999", "", nil)
	if status != http.StatusOK {
		t.Fatalf("hybrid failed: %d %s", status, data)
	}
	if _, err := os.Stat("/nonexistent"); err == nil {
		t.Fatal("unreachable")
	}
	var hybrid struct {
		Items []memory.Item `json:"items"`
	}
	if err := json.Unmarshal(data, &hybrid); err != nil {
		t.Fatal(err)
	}
	if len(hybrid.Items) > 64 {
		t.Fatalf("max_items was not clamped: %d items", len(hybrid.Items))
	}
}

func TestConflictReturnsBothSides(t *testing.T) {
	env := newTestEnv(t, "")
	rowID := memory.NoteRowID(env.convID, "note:one")
	body := `{"expected_version":"stale","new_version":"web-1","summary":"我的修改"}`
	status, data := env.do(t, http.MethodPut, "/api/conversations/"+env.convID+"/notes/"+rowID, body, nil)
	if status != http.StatusConflict {
		t.Fatalf("got %d want 409: %s", status, data)
	}
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != app.CodeAnnotationVersionConflict {
		t.Fatalf("unexpected code: %q", envelope.Error.Code)
	}
	if envelope.Error.Details["current_version"] != "v1" {
		t.Fatalf("missing current_version: %v", envelope.Error.Details)
	}
	note, ok := envelope.Error.Details["current_note"].(map[string]any)
	if !ok || note["summary"] != "预算需要复查" {
		t.Fatalf("missing current_note: %v", envelope.Error.Details["current_note"])
	}
	submitted, ok := envelope.Error.Details["submitted"].(map[string]any)
	if !ok || submitted["summary"] != "我的修改" {
		t.Fatalf("missing submitted input: %v", envelope.Error.Details["submitted"])
	}
	if _, ok := envelope.Error.Details["current"]; !ok {
		t.Fatalf("missing current view: %v", envelope.Error.Details)
	}
}

func TestSearchNeverReturnsStaleNotes(t *testing.T) {
	env := newTestEnv(t, "")
	rowID := memory.NoteRowID(env.convID, "note:one")
	if _, err := env.store.DB().Exec(`INSERT INTO derived_artifacts(kind,artifact_id,conversation_id,stale,stale_reason,stale_at) VALUES('note',?,?,1,'test',1)`, rowID, env.convID); err != nil {
		t.Fatal(err)
	}
	status, data := env.do(t, http.MethodGet, "/api/search?q=%E9%A2%84%E7%AE%97&include_stale=1", "", nil)
	if status != http.StatusOK {
		t.Fatalf("search failed: %d %s", status, data)
	}
	var page SearchPageView
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	for _, hit := range page.Hits {
		if hit.Kind != "turn" {
			t.Fatalf("stale note leaked into search: %+v", hit)
		}
	}
	// The same data is reachable through the store's audit path, which proves
	// the missing hit comes from the endpoint's fixed current-state rule.
	audit, err := env.store.SearchMemory(context.Background(), memory.MemorySearch{Text: "预算", IncludeStale: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range audit.Hits {
		if hit.Kind != "turn" {
			found = true
		}
	}
	if !found {
		t.Fatal("audit search should still find the stale note")
	}
}

func TestAudioErrorCodes(t *testing.T) {
	env := newTestEnv(t, "")
	page, err := env.store.Query(context.Background(), memory.Query{ConversationID: env.convID, IncludeTurns: true})
	if err != nil {
		t.Fatal(err)
	}
	var turnID string
	for _, item := range page.Items {
		if item.Kind == "turn" {
			turnID = item.ID
		}
	}
	if turnID == "" {
		t.Fatal("no turn in the fixture")
	}
	status, data := env.do(t, http.MethodGet, "/api/turns/"+turnID+"/audio", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("no sync row: got %d want 404 (%s)", status, data)
	}
	if e := decodeError(t, data); e.Code != app.CodeAudioUnavailable {
		t.Fatalf("unexpected code: %q", e.Code)
	}

	status, data = env.do(t, http.MethodGet, "/api/turns/turn:nope/audio", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown turn: got %d want 404", status)
	}
	if e := decodeError(t, data); e.Code != app.CodeNotFound {
		t.Fatalf("unexpected code: %q", e.Code)
	}

	// A deleted sync row makes the same turn report 410 instead of 404.
	if _, err := env.store.DB().Exec(`INSERT INTO source_attachments(id,conversation_id,recording_id,segment_index,segment_id,transcript_revision,source_json,result_json,content_sha256,active,supersedes_revision)
		VALUES('att-1',?,?,'1','seg-1','rev-1','{"device_id":"dev-1","recording_id":"rec-1"}','{}','x',1,NULL)`, env.convID, memory.RecordingID("dev-1", "rec-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.DB().Exec(`INSERT INTO sync_recordings(recording_id,owner,device_id,state,updated_at) VALUES('rec-1','default','dev-1','deleted',1)`); err != nil {
		t.Fatal(err)
	}
	status, data = env.do(t, http.MethodGet, "/api/turns/"+turnID+"/audio", "", nil)
	if status != http.StatusGone {
		t.Fatalf("deleted recording: got %d want 410 (%s)", status, data)
	}
	if e := decodeError(t, data); e.Code != app.CodeAudioDeleted {
		t.Fatalf("unexpected code: %q", e.Code)
	}
}

func TestUnknownAPIPathIsJSON(t *testing.T) {
	env := newTestEnv(t, "")
	status, data := env.do(t, http.MethodGet, "/api/does-not-exist", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("got %d want 404", status)
	}
	if e := decodeError(t, data); e.Code != app.CodeNotFound {
		t.Fatalf("unexpected code: %q", e.Code)
	}
}

func TestStaticServesSPAFallbackAndAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><div id=root></div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := newTestEnv(t, dir)

	resp := env.raw(t, http.MethodGet, "/c/conv:web", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "id=root") {
		t.Fatalf("SPA fallback failed: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("index cache-control: %q", got)
	}

	resp = env.raw(t, http.MethodGet, "/assets/app.js", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset serving: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}

	resp = env.raw(t, http.MethodGet, "/../secret.txt", "", nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "secret") && !strings.Contains(string(body), "id=root") {
		t.Fatalf("path traversal served the file: %s", body)
	}

	for _, header := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if resp.Header.Get(header) == "" {
			t.Fatalf("missing security header %s", header)
		}
	}
}

func TestStaticReportsMissingFrontend(t *testing.T) {
	env := newTestEnv(t, filepath.Join(t.TempDir(), "missing"))
	resp := env.raw(t, http.MethodGet, "/", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "frontend not built: run make web") {
		t.Fatalf("unexpected missing-frontend response: %d %s", resp.StatusCode, body)
	}
	// The API keeps working without a frontend build.
	if status, _ := env.do(t, http.MethodGet, "/api/topics", "", nil); status != http.StatusOK {
		t.Fatalf("API blocked by missing frontend: %d", status)
	}
}

func TestHealthzNeedsNoSession(t *testing.T) {
	env := newTestEnv(t, "")
	resp := env.raw(t, http.MethodGet, "/healthz", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("healthz: %d %s", resp.StatusCode, body)
	}
}

func TestConversationDetailAndTurnsHappyPath(t *testing.T) {
	env := newTestEnv(t, "")
	status, data := env.do(t, http.MethodGet, "/api/conversations", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list failed: %d", status)
	}
	var page memory.ConversationPage
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].RecordingStartedAt != nil {
		t.Fatalf("unexpected list page: %s", data)
	}

	status, data = env.do(t, http.MethodGet, "/api/conversations/"+env.convID, "", nil)
	if status != http.StatusOK {
		t.Fatalf("detail failed: %d %s", status, data)
	}
	var view app.ConversationView
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatal(err)
	}
	if view.AnnotationVersion != "v1" || len(view.Notes) != 1 || view.RecordingStartedAt != nil {
		t.Fatalf("unexpected detail: %s", data)
	}

	status, data = env.do(t, http.MethodGet, "/api/conversations/"+env.convID+"/turns?recording_id="+view.Recordings[0].RecordingID, "", nil)
	if status != http.StatusOK {
		t.Fatalf("turns failed: %d %s", status, data)
	}
	var turns app.TurnPage
	if err := json.Unmarshal(data, &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns.Items) != 1 || turns.Items[0].Kind != "turn" || turns.Items[0].AudioStatus != app.AudioUnavailable {
		t.Fatalf("unexpected turns: %s", data)
	}

	status, _ = env.do(t, http.MethodGet, "/api/conversations/conv:nope", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown conversation: got %d want 404", status)
	}
}

func TestHybridRetrievalVersionSelection(t *testing.T) {
	env := newTestEnv(t, "")
	status, data := env.do(t, http.MethodGet, "/api/search/hybrid?q=预算&retrieval_version=typo", "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid version: %d %s", status, data)
	}
	var defaultBody, v1Body []byte
	for _, version := range []string{"", "v1", "v2"} {
		status, data = env.do(t, http.MethodGet, "/api/search/hybrid?q=预算&mode=lexical&retrieval_version="+version, "", nil)
		if status != http.StatusOK {
			t.Fatalf("version %q: %d %s", version, status, data)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if version == "" {
			defaultBody = append([]byte(nil), data...)
			if len(result["sufficiency"]) != 0 {
				t.Fatalf("default retrieval unexpectedly selected v2: %s", data)
			}
		}
		if version == "v1" {
			v1Body = append([]byte(nil), data...)
			if len(result["sufficiency"]) != 0 {
				t.Fatalf("explicit v1 unexpectedly selected v2: %s", data)
			}
		}
		if version == "v2" && len(result["sufficiency"]) == 0 {
			t.Fatalf("v2 missing sufficiency: %s", data)
		}
	}
	if string(defaultBody) != string(v1Body) {
		t.Fatalf("omitted retrieval_version differs from explicit v1\ndefault: %s\nv1: %s", defaultBody, v1Body)
	}
}
