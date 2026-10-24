package web

import (
 "encoding/json"
 "net/http"
 "testing"
 "recordingmcp/internal/memory"
)

func TestHighlightsAndBriefAuthenticatedRoutes(t *testing.T) {
 e:=newTestEnv(t, "")
 status, body:=e.do(t,http.MethodGet,"/api/recordings/"+memory.RecordingID("dev-1","rec-1")+"/highlights","",nil)
 if status!=200 {t.Fatalf("list: %d %s",status,body)}
 var list struct {ContractVersion string `json:"contract_version"`; Highlights []json.RawMessage `json:"highlights"`}
 if err:=json.Unmarshal(body,&list);err!=nil || list.ContractVersion!="highlights/0.1" || list.Highlights==nil {t.Fatalf("contract: %s %v",body,err)}
 status,body=e.do(t,http.MethodPost,"/api/briefs/preview",`{"scope":{"kind":"conversation","conversation_ids":["conv:web"]}}`,nil)
 if status!=200 {t.Fatalf("brief: %d %s",status,body)}
 var brief struct {SnapshotID string `json:"snapshot_id"`;Evidence []json.RawMessage `json:"evidence"`}
 if err:=json.Unmarshal(body,&brief);err!=nil || brief.SnapshotID=="" || len(brief.Evidence)==0 {t.Fatalf("brief evidence: %s %v",body,err)}
 status,_=e.do(t,http.MethodPost,"/api/briefs/preview",`{"scope":{"kind":"conversation","conversation_ids":["conv:web"]}}`,func(r *http.Request){r.Header.Del("X-CSRF-Token");r.Header.Del("X-CSRF")})
 if status!=403 {t.Fatalf("missing CSRF accepted: %d",status)}
}
