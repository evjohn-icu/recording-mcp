package app

import (
 "context"
 "encoding/json"
 "errors"
 "strings"
 "testing"

 "recordingmcp/internal/contextbrief"
)

type testPolisher func(context.Context, contextbrief.Result) ([]contextbrief.PolishCandidate,error)
func (f testPolisher) Polish(c context.Context,b contextbrief.Result)([]contextbrief.PolishCandidate,error){return f(c,b)}

func TestBriefPolishExplicitConsentAndDefaultOff(t *testing.T) {
 f:=newFixture(t); ctx:=context.Background()
 req:=contextbrief.Request{Scope:contextbrief.Scope{Kind:"conversation",ConversationIDs:[]string{f.conversationID}}}
 base,err:=f.svc.BuildContextBrief(ctx,req); if err!=nil {t.Fatal(err)}
 in:=BriefPolishRequest{Request:req,SnapshotID:base.SnapshotID,Consent:true}
 if _,err=f.svc.PolishContextBrief(ctx,in); err==nil {t.Fatal("disabled polisher accepted")}
 calls:=0
 f.svc.BriefPolisher=testPolisher(func(_ context.Context,b contextbrief.Result)([]contextbrief.PolishCandidate,error){calls++;return nil,errors.New("PRIVATE MODEL BODY")})
 if _,err=f.svc.BuildContextBrief(ctx,req);err!=nil||calls!=0 {t.Fatal("deterministic preview called model",err)}
 in.Consent=false
 if _,err=f.svc.PolishContextBrief(ctx,in);err==nil||calls!=0 {t.Fatal("missing consent accepted",err,calls)}
 in.Consent=true;in.SnapshotID="outdated"
 if _,err=f.svc.PolishContextBrief(ctx,in);err==nil||calls!=0 {t.Fatal("stale snapshot called model",err,calls)}
 in.SnapshotID=base.SnapshotID
 out,err:=f.svc.PolishContextBrief(ctx,in)
 raw,_:=json.Marshal(out)
 if err!=nil||!out.Degraded||len(out.Candidates)!=0||calls!=1||strings.Contains(string(raw),"PRIVATE") {t.Fatalf("unsafe fallback: %+v %v calls=%d",out,err,calls)}
 after,err:=f.svc.BuildContextBrief(ctx,req);if err!=nil||after.SnapshotID!=base.SnapshotID {t.Fatal("polish modified source",err)}
}

func TestBriefPolishRejectsSourceChangedDuringModelCall(t *testing.T){
 f:=newFixture(t);ctx:=context.Background()
 req:=contextbrief.Request{Scope:contextbrief.Scope{Kind:"conversation",ConversationIDs:[]string{f.conversationID}}}
 base,err:=f.svc.BuildContextBrief(ctx,req);if err!=nil {t.Fatal(err)}
 f.svc.BriefPolisher=testPolisher(func(_ context.Context,b contextbrief.Result)([]contextbrief.PolishCandidate,error){
  turn:=f.turnRowID(t,"seg-a","t-1");src,e:=f.store.TranscriptSourceForTurn(ctx,turn);if e!=nil {t.Fatal(e)}
  updates:=map[string]string{};for _,tr:=range src.Result.Turns {updates[tr.ID]="revised synthetic content"}
  _,e=f.store.CorrectTranscriptSegment(ctx,f.conversationID,f.recordingID,src.SegmentID,src.Result.Source.TranscriptRevision,"polish-r2",updates);if e!=nil {t.Fatal(e)}
  return []contextbrief.PolishCandidate{},nil
 })
 out,err:=f.svc.PolishContextBrief(ctx,BriefPolishRequest{Request:req,SnapshotID:base.SnapshotID,Consent:true})
 var ae *Error
 if !errors.As(err,&ae)||ae.Status!=409||ae.Code!="snapshot_changed"||len(out.Candidates)!=0 {t.Fatalf("stale output returned %+v %v",out,err)}
}
