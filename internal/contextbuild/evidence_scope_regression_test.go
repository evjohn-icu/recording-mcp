package contextbuild

import "testing"

func TestTopicScopeAlsoFiltersEmbeddedNoteEvidence(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv", "会话", "rec")
	f.recording("rec", "conv", nil)
	f.turn("in", "conv", "rec", 0, 0, nil, "范围内原话")
	f.turn("out", "conv", "rec", 1, 1000, nil, "范围外原话")
	f.topic("topic", "主题")
	f.turnTopic("in", "topic")
	f.note("note", "conv", "observation", "归纳标记", []string{"topic"}, []string{"in", "out"})
	pkg := f.build(nil, Request{Query: "归纳标记", TopicIDs: []string{"topic"}})
	found := false
	for _, item := range pkg.Items {
		if item.ID != "note" {
			continue
		}
		found = true
		if len(item.EvidenceTurnIDs) != 1 || item.EvidenceTurnIDs[0] != "in" {
			t.Fatalf("embedded evidence escaped scope: %+v", item)
		}
	}
	if !found {
		t.Fatal("in-scope note missing")
	}
	for _, evidence := range pkg.Evidence {
		if evidence.TurnID == "out" {
			t.Fatal("expanded evidence escaped scope")
		}
	}
}
