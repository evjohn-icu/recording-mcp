package web

import (
	"encoding/json"
	"net/http"
	"recordingmcp/internal/memory"
	"strings"
	"testing"
)

func TestHighlightsAndBriefAuthenticatedRoutes(t *testing.T) {
	e := newTestEnv(t, "")
	status, body := e.do(t, http.MethodGet, "/api/recordings/"+memory.RecordingID("dev-1", "rec-1")+"/highlights", "", nil)
	if status != 200 {
		t.Fatalf("list: %d %s", status, body)
	}
	var list struct {
		ContractVersion string            `json:"contract_version"`
		Highlights      []json.RawMessage `json:"highlights"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.ContractVersion != "highlights/0.1" || list.Highlights == nil {
		t.Fatalf("contract: %s %v", body, err)
	}
	status, body = e.do(t, http.MethodPost, "/api/briefs/preview", `{"scope":{"kind":"conversation","conversation_ids":["conv:web"]}}`, nil)
	if status != 200 {
		t.Fatalf("brief: %d %s", status, body)
	}
	var brief struct {
		SnapshotID string            `json:"snapshot_id"`
		Evidence   []json.RawMessage `json:"evidence"`
	}
	if err := json.Unmarshal(body, &brief); err != nil || brief.SnapshotID == "" || len(brief.Evidence) == 0 {
		t.Fatalf("brief evidence: %s %v", body, err)
	}
	status, _ = e.do(t, http.MethodPost, "/api/briefs/preview", `{"scope":{"kind":"conversation","conversation_ids":["conv:web"]}}`, func(r *http.Request) { r.Header.Del("X-CSRF-Token"); r.Header.Del("X-CSRF") })
	if status != 403 {
		t.Fatalf("missing CSRF accepted: %d", status)
	}
}

func TestHighlightUnknownIDIs404AndBriefCannotSwitchProfiles(t *testing.T) {
	e := newTestEnv(t, "")
	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		path := "/api/highlights/not-in-this-profile"
		body := ""
		if method == http.MethodPatch {
			body = `{"expected_version":1,"note_text":"private-draft"}`
		}
		if method == http.MethodDelete {
			path += "?expected_version=1"
		}
		status, raw := e.do(t, method, path, body, nil)
		if status != 404 {
			t.Fatalf("%s unknown: %d %s", method, status, raw)
		}
	}
	status, raw := e.do(t, http.MethodPost, "/api/briefs/preview", `{"scope":{"kind":"conversation","conversation_ids":["conv:web"]},"profile":"other"}`, nil)
	if status != 400 {
		t.Fatalf("profile selector accepted: %d %s", status, raw)
	}
}

func TestBriefPolishRoutesConsentAndCSRF(t *testing.T) {
	e := newTestEnv(t, "")
	status, raw := e.do(t, http.MethodGet, "/api/briefs/polish/config", "", nil)
	if status != 200 || !strings.Contains(string(raw), `"enabled":false`) {
		t.Fatalf("default config: %d %s", status, raw)
	}
	body := `{"request":{"scope":{"kind":"conversation","conversation_ids":["conv:web"]}},"snapshot_id":"x","consent":true}`
	status, raw = e.do(t, http.MethodPost, "/api/briefs/polish", body, nil)
	if status != 503 || !strings.Contains(string(raw), "polish_disabled") {
		t.Fatalf("default enabled: %d %s", status, raw)
	}
	status, _ = e.do(t, http.MethodPost, "/api/briefs/polish", body, func(r *http.Request) { r.Header.Del("X-CSRF-Token"); r.Header.Del("X-CSRF") })
	if status != 403 {
		t.Fatalf("CSRF allowed: %d", status)
	}
	status, _ = e.do(t, http.MethodPost, "/api/briefs/polish", strings.TrimSuffix(body, "}")+`,"endpoint":"http://attacker"}`, nil)
	if status != 400 {
		t.Fatalf("client endpoint allowed: %d", status)
	}
}
