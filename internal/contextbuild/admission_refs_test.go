package contextbuild

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// Plan 6.2: v3 admission candidates carry their canonical required refs —
// for notes the note_evidence rows (batch-fetched with ordinal ordering,
// never a neighbour standing in for a citation), for turns the turn's own
// canonical row id, and for conversations a valid existing source with the
// navigation_only source status. Candidates whose required refs are missing,
// out of scope or not valid UTF-8 are rejected whole: the assessor never sees
// them and their body text never reaches candidate blocks, the final
// evidence or the trace.

// admissionEvidenceText concatenates the returned evidence texts.
func admissionEvidenceText(pkg Package) string {
	var b strings.Builder
	for _, ref := range pkg.Evidence {
		b.WriteString(ref.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// admissionSeed builds one conversation with a citable turn so admission
// tests can add their own notes on top.
func admissionSeed(t *testing.T, conv, rec string) *fixture {
	t.Helper()
	f := newFixture(t)
	f.conversation(conv, "Admission", rec)
	f.recording(rec, conv, nil)
	return f
}

// TestBuildV3AdmissionRejectsCandidateWithInvalidUTF8Evidence covers the
// non-UTF-8 rejection path: an evidence turn that is not valid UTF-8 rejects
// the whole candidate before the assessor runs, and the raw bytes never
// reach the assessor input or the returned evidence. Other candidates are
// still assessed in the same build.
func TestBuildV3AdmissionRejectsCandidateWithInvalidUTF8Evidence(t *testing.T) {
	f := newFixture(t)
	f.conversation("conv:good", "Good", "rec:good")
	f.recording("rec:good", "conv:good", nil)
	f.turn("t:good", "conv:good", "rec:good", 0, 1000, nil, "预算事实原话 clean text")
	f.note("n:good", "conv:good", "observation", "utf8 marker good note", nil, []string{"t:good"})

	f.conversation("conv:bad", "Bad", "rec:bad")
	f.recording("rec:bad", "conv:bad", nil)
	f.turn("t:bad", "conv:bad", "rec:bad", 0, 1000, nil, "预算事实原话 "+string([]byte{0xff, 0xfe}))
	f.note("n:bad", "conv:bad", "observation", "utf8 marker bad note", nil, []string{"t:bad"})

	assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
		"n:good": support(0.9),
	}}
	pkg, err := Build(context.Background(), Deps{
		Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
		CandidateAssessor: assessor, Assessor: termAssessor{required: []string{"预算事实原话"}},
		AdmissionPolicy: v3Policy(),
	}, v3Request("utf8 marker"))
	if err != nil {
		t.Fatalf("Build returned an error instead of a rejection: %v", err)
	}
	if containsString(itemIDs(pkg.Items), "n:bad") {
		t.Fatalf("invalid-UTF-8 candidate entered items: %v", itemIDs(pkg.Items))
	}
	if !containsString(itemIDs(pkg.Items), "n:good") {
		t.Fatalf("valid candidate was not assessed in the same build: seen=%d items=%v", len(assessor.seen), itemIDs(pkg.Items))
	}
	rejected := false
	if pkg.Trace.Admission != nil {
		for _, rejection := range pkg.Trace.Admission.Rejected {
			if rejection.ItemID == "n:bad" {
				rejected = true
			}
		}
	}
	if !rejected {
		t.Fatalf("n:bad missing from admission rejections: %+v", pkg.Trace.Admission)
	}
	for _, candidate := range assessor.seen {
		for _, ref := range candidate.Evidence {
			if !utf8.ValidString(ref.Text) {
				t.Fatalf("candidate %s was submitted with invalid UTF-8 evidence %q", candidate.Item.ID, ref.Text)
			}
		}
	}
	for _, ref := range pkg.Evidence {
		if ref.TurnID == "t:bad" {
			t.Fatalf("invalid-UTF-8 turn entered the returned evidence: %+v", pkg.Evidence)
		}
	}
	if !utf8.ValidString(admissionEvidenceText(pkg)) {
		t.Fatalf("returned evidence contains invalid UTF-8")
	}
}

// TestBuildV3AdmissionConversationCandidateNavigationOnly pins the
// conversation rules: a conversation item with a valid existing source is
// submitted as a navigation-only candidate that carries no fabricated
// evidence refs, while a conversation without a valid source is rejected and
// never submitted.
func TestBuildV3AdmissionConversationCandidateNavigationOnly(t *testing.T) {
	t.Run("valid_source_is_submitted_as_navigation_only", func(t *testing.T) {
		f := admissionSeed(t, "conv:nav", "rec:nav")
		f.turn("t:navfact", "conv:nav", "rec:nav", 0, 1000, nil, "导航事实原话")
		f.note("n:nav", "conv:nav", "observation", "导航准入标记 note", nil, []string{"t:navfact"})

		assessor := &stubCandidateAssessor{decisions: map[string]AdmissionDecision{
			"n:nav": support(0.9),
		}}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{required: []string{"导航事实原话"}},
			AdmissionPolicy: v3Policy(),
		}, Request{Query: "导航准入标记", ConversationID: "conv:nav", RetrievalVersion: "v3", IncludeRawEvidence: true, MaxBytes: 65536})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		var conversation *AdmissionCandidate
		for i, candidate := range assessor.seen {
			if candidate.Item.ID == "conv:nav" {
				conversation = &assessor.seen[i]
			}
		}
		if conversation == nil {
			t.Fatalf("conversation candidate missing from assessor input: seen=%d", len(assessor.seen))
		}
		if conversation.SourceStatus != SourceStatusNavigationOnly {
			t.Fatalf("conversation source status = %q, want navigation_only", conversation.SourceStatus)
		}
		if len(conversation.Evidence) != 0 {
			t.Fatalf("navigation-only candidate carries evidence: %+v", conversation.Evidence)
		}
		// The gate itself still decides admission: the assessor's unknown
		// verdict keeps the conversation out of the items.
		if containsString(itemIDs(pkg.Items), "conv:nav") {
			t.Fatalf("unknown-verdict conversation entered items: %v", itemIDs(pkg.Items))
		}
	})

	t.Run("unreadable_source_is_rejected_and_not_submitted", func(t *testing.T) {
		// A recording row alone is not a valid source: without a current,
		// readable turn in scope the conversation cannot be assessed.
		f := newFixture(t)
		f.conversation("conv:shell", "Shell", "rec:shell")
		f.recording("rec:shell", "conv:shell", nil)

		assessor := &stubCandidateAssessor{}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{},
			AdmissionPolicy: v3Policy(),
		}, Request{Query: "shell", ConversationID: "conv:shell", RetrievalVersion: "v3", MaxBytes: 65536})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		for _, candidate := range assessor.seen {
			if candidate.Item.ID == "conv:shell" {
				t.Fatalf("conversation without a readable source was submitted to the assessor")
			}
		}
		rejected := false
		if pkg.Trace.Admission != nil {
			for _, rejection := range pkg.Trace.Admission.Rejected {
				if rejection.ItemID == "conv:shell" {
					rejected = true
				}
			}
		}
		if !rejected {
			t.Fatalf("conv:shell missing from admission rejections: %+v", pkg.Trace.Admission)
		}
	})

	t.Run("missing_source_is_rejected_and_not_submitted", func(t *testing.T) {
		f := newFixture(t)
		f.conversation("conv:orphan", "Orphan", "rec:ghost")

		assessor := &stubCandidateAssessor{}
		pkg, err := Build(context.Background(), Deps{
			Store: f.store, DB: f.db, Semantic: &fakeSemantic{ready: true},
			CandidateAssessor: assessor, Assessor: termAssessor{},
			AdmissionPolicy: v3Policy(),
		}, Request{Query: "orphan", ConversationID: "conv:orphan", RetrievalVersion: "v3", MaxBytes: 65536})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		for _, candidate := range assessor.seen {
			if candidate.Item.ID == "conv:orphan" {
				t.Fatalf("source-less conversation was submitted to the assessor")
			}
		}
		rejected := false
		if pkg.Trace.Admission != nil {
			for _, rejection := range pkg.Trace.Admission.Rejected {
				if rejection.ItemID == "conv:orphan" {
					rejected = true
				}
			}
		}
		if !rejected {
			t.Fatalf("conv:orphan missing from admission rejections: %+v", pkg.Trace.Admission)
		}
	})
}
