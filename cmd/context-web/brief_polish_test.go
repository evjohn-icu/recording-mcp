package main

import "testing"

func TestBriefPolisherConfigurationIsExplicit(t *testing.T){
 p,e:=configuredBriefPolisher("","","");if e!=nil||p!=nil {t.Fatal("default enabled",e)}
 for _,endpoint:=range []string{"file:///tmp/model","http://example.com/model","https://user:secret@example.com/model","https://example.com/model#token"}{
  if _,e=configuredBriefPolisher(endpoint,"model","");e==nil{t.Errorf("unsafe endpoint accepted: %s",endpoint)}
 }
 if _,e=configuredBriefPolisher("http://127.0.0.1:9876/v1/chat/completions","test","");e!=nil{t.Fatal(e)}
 if _,e=configuredBriefPolisher("https://models.example.test/completions","test","");e!=nil{t.Fatal(e)}
 if _,e=configuredBriefPolisher("https://models.example.test/completions","","");e==nil{t.Fatal("missing model allowed")}
 t.Setenv("BRIEF_TEST_EMPTY_KEY","")
 if _,e=configuredBriefPolisher("https://models.example.test/completions","test","BRIEF_TEST_EMPTY_KEY");e==nil{t.Fatal("missing key allowed")}
}
