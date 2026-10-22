package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recordingmcp/internal/plane"

	"recordingmcp/internal/app"
	"recordingmcp/internal/memory"
	"recordingmcp/internal/profile"
)

func TestProfilesDispatcherSwitchesAndIsolatesDatabases(t *testing.T) {
	manager, err := profile.Open(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	for name, password := range map[string]string{"alice": "alice password long", "bob": "bob password long"} {
		if _, err := manager.Create(name, password); err != nil {
			t.Fatal(err)
		}
	}
	var stores []*memory.Store
	router, err := NewProfiles(ProfilesConfig{
		Manager: manager,
		Factory: func(paths profile.Paths, verifier func(string) bool) (*Server, error) {
			store, err := memory.Open(paths.Database)
			if err != nil {
				return nil, err
			}
			stores = append(stores, store)
			ann := testAnnotations("dev-"+paths.Name, "rec-"+paths.Name)
			ann.ConversationID = "conv:" + paths.Name
			if _, err := store.Import(context.Background(), testTranscript(), ann, "synthetic.wav", strings.Repeat("c", 64)); err != nil {
				return nil, err
			}
			return New(Config{Service: app.New(store, nil, nil, paths.Audio), ProfileName: paths.Name, VerifyProfilePassword: verifier})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, store := range stores {
			_ = store.Close()
		}
	})
	httpServer := httptest.NewServer(router.Handler())
	defer httpServer.Close()
	client := &http.Client{}
	request := func(method, path, body string, cookies []*http.Cookie, csrf string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, httpServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", httpServer.URL)
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := request(http.MethodGet, "/api/auth-info", "", nil, "")
	var authInfo map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&authInfo); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if authInfo["mode"] != "profiles" {
		t.Fatalf("auth info = %v", authInfo)
	}

	login := func(name, password string, oldCookies []*http.Cookie) ([]*http.Cookie, string) {
		t.Helper()
		body := `{"profile":"` + name + `","password":"` + password + `"}`
		resp := request(http.MethodPost, "/api/profiles/login", body, oldCookies, "")
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("login %s: %d %s", name, resp.StatusCode, b)
		}
		var result struct {
			CSRF    string `json:"csrf_token"`
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		cookies := resp.Cookies()
		resp.Body.Close()
		if result.Profile != name || result.CSRF == "" || len(cookies) < 2 {
			t.Fatalf("incomplete profile login: %+v, %v", result, cookies)
		}
		return cookies, result.CSRF
	}
	aliceCookies, aliceCSRF := login("alice", "alice password long", nil)
	resp = request(http.MethodGet, "/api/conversations", "", aliceCookies, "")
	aliceBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(aliceBody), "conv:alice") || strings.Contains(string(aliceBody), "conv:bob") {
		t.Fatalf("alice view crossed profile boundary: %d %s", resp.StatusCode, aliceBody)
	}
	bobCookies, _ := login("bob", "bob password long", nil)
	resp = request(http.MethodGet, "/api/conversations", "", bobCookies, "")
	bobBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(bobBody), "conv:bob") || strings.Contains(string(bobBody), "conv:alice") {
		t.Fatalf("bob view crossed profile boundary: %d %s", resp.StatusCode, bobBody)
	}
	// The outer profile session chooses the child; a mismatched child cookie is
	// ignored and cannot redirect a request into another profile database.
	resp = request(http.MethodGet, "/api/conversations", "", append(aliceCookies[:1:1], bobCookies[1:]...), "")
	wrongChildBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(wrongChildBody), "conv:alice") || strings.Contains(string(wrongChildBody), "conv:bob") {
		t.Fatalf("client child cookie selected another profile: %d %s", resp.StatusCode, wrongChildBody)
	}
	resp = request(http.MethodPost, "/api/logout", "{}", aliceCookies, aliceCSRF)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}
	resp = request(http.MethodGet, "/api/conversations", "", aliceCookies, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logged-out profile session status = %d", resp.StatusCode)
	}
}

func TestProfilesCanPermanentlyRetireWithPasswordAndExactName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiles")
	manager, err := profile.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := manager.Create("alice", "alice retirement password")
	if err != nil {
		t.Fatal(err)
	}
	var store *memory.Store
	var events *plane.Service
	router, err := NewProfiles(ProfilesConfig{Manager: manager, Factory: func(paths profile.Paths, verifier func(string) bool) (*Server, error) {
		store, err = memory.Open(paths.Database)
		if err != nil {
			return nil, err
		}
		events, err = plane.Open(paths.Plane)
		if err != nil {
			return nil, err
		}
		svc := app.New(store, nil, nil, paths.Audio)
		svc.ForgetPlane = events
		svc.ManagedIndexDir = paths.Index
		svc.ManagedBackupDir = paths.Backups
		return New(Config{Service: svc, ProfileName: paths.Name, VerifyProfilePassword: verifier})
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if events != nil {
			_ = events.Close()
		}
		if store != nil {
			_ = store.Close()
		}
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()
	call := func(body string) *http.Response {
		req, e := http.NewRequest(http.MethodPost, server.URL+"/api/profiles/retire", strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", server.URL)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return resp
	}
	wrong := call(`{"profile":"alice","confirm_profile":"bob","password":"alice retirement password"}`)
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong confirmation status=%d", wrong.StatusCode)
	}
	bad := call(`{"profile":"alice","confirm_profile":"alice","password":"wrong retirement password"}`)
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status=%d", bad.StatusCode)
	}
	resp := call(`{"profile":"alice","confirm_profile":"alice","password":"alice retirement password"}`)
	var body map[string]string
	if err = json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || body["status"] != "complete" {
		t.Fatalf("retirement response %d %+v", resp.StatusCode, body)
	}
	if manager.Verify("alice", "alice retirement password") {
		t.Fatal("retired profile can log in")
	}
	if _, err = manager.Resolve("alice"); err == nil {
		t.Fatal("retired profile still resolves")
	}
	var n int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("canonical content retained %d %v", n, err)
	}
	if _, err = events.Ingest(context.Background(), []plane.Event{{ID: "late", Type: "task.created", Timestamp: time.Now(), Source: "test"}}); err == nil {
		t.Fatal("retired event store accepted append")
	}
	// The credential-gated endpoint remains available only for retry after a
	// crash; successful repeat is idempotent.
	retry := call(`{"profile":"alice","confirm_profile":"alice","password":"alice retirement password"}`)
	retry.Body.Close()
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("retirement retry status=%d", retry.StatusCode)
	}
	_ = paths
}
