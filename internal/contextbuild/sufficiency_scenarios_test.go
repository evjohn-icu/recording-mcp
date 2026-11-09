package contextbuild

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type sufficiencyScenario struct {
	ID                           string        `json:"id"`
	Category                     string        `json:"category"`
	Setup                        scenarioSetup `json:"setup"`
	Query                        string        `json:"query"`
	ExpectedStatus               string        `json:"expected_status"`
	ExpectedEvidenceTurns        []string      `json:"expected_evidence_turns"`
	ForbiddenEvidenceTurns       []string      `json:"forbidden_evidence_turns"`
	RequiredTerms                []string      `json:"required_terms"`
	ForbiddenTerms               []string      `json:"forbidden_terms"`
	ExpectedGaps                 []string      `json:"expected_gaps"`
	ExpectNullEvidenceTimestamps []string      `json:"expect_null_evidence_timestamps"`
	Unanswerable                 bool          `json:"unanswerable"`
	Notes                        string        `json:"notes"`
}

type scenarioSetup struct {
	Conversations     []scenarioConversation `json:"conversations"`
	Topics            []scenarioTopic        `json:"topics"`
	ScopeConversation string                 `json:"scope_conversation"`
	MaxBytes          int                    `json:"max_bytes"`
	Assessor          string                 `json:"assessor"`
}

type scenarioConversation struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	StartedAt      *string        `json:"started_at"`
	Turns          []scenarioTurn `json:"turns"`
	Notes          []scenarioNote `json:"notes"`
	AmbientTurns   []string       `json:"ambient_turns"`
	ForgottenTurns []string       `json:"forgotten_turns"`
}

type scenarioTurn struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	StartMS *int64 `json:"start_ms"`
}

type scenarioNote struct {
	ID            string   `json:"id"`
	Summary       string   `json:"summary"`
	Kind          string   `json:"kind"`
	EvidenceTurns []string `json:"evidence_turns"`
	Stale         bool     `json:"stale"`
}

type scenarioTopic struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Turns []string `json:"turns"`
}

// termAssessor is the deterministic assessment contract: it answers from the
// final budgeted evidence only. That keeps the expected status independent of
// any real assessor implementation.
type termAssessor struct {
	required  []string
	forbidden []string
}

func (a termAssessor) Assess(_ context.Context, _ string, evidence []EvidenceRef) (EvidenceAssessment, error) {
	var text strings.Builder
	refs := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		text.WriteString(ev.Text)
		text.WriteString("\n")
		refs = append(refs, ev.TurnID)
	}
	for _, term := range a.required {
		if !strings.Contains(text.String(), term) {
			return EvidenceAssessment{Status: "insufficient", EvidenceRefs: refs}, nil
		}
	}
	for _, term := range a.forbidden {
		if strings.Contains(text.String(), term) {
			return EvidenceAssessment{Status: "insufficient", EvidenceRefs: refs}, nil
		}
	}
	return EvidenceAssessment{Status: "sufficient", EvidenceRefs: refs}, nil
}

type scriptedAssessor struct{ mode string }

func (a scriptedAssessor) Assess(_ context.Context, _ string, evidence []EvidenceRef) (EvidenceAssessment, error) {
	refs := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		refs = append(refs, ev.TurnID)
	}
	switch a.mode {
	case "error":
		return EvidenceAssessment{}, errors.New("assessor unavailable")
	case "invalid_refs":
		return EvidenceAssessment{Status: "sufficient", EvidenceRefs: []string{"turn:does-not-exist"}}, nil
	case "empty_sufficient":
		return EvidenceAssessment{Status: "sufficient", EvidenceRefs: []string{}}, nil
	}
	return EvidenceAssessment{Status: "unknown", EvidenceRefs: refs}, nil
}

func loadSufficiencyScenarios(t *testing.T) []sufficiencyScenario {
	t.Helper()
	path := filepath.Join("..", "..", "fixtures", "sufficiency", "v1", "scenarios.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scenarios: %v", err)
	}
	var doc struct {
		Scenarios []sufficiencyScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode scenarios: %v", err)
	}
	return doc.Scenarios
}

func seedScenario(t *testing.T, f *fixture, setup scenarioSetup) {
	t.Helper()
	for _, conv := range setup.Conversations {
		recordingID := "rec:" + conv.ID
		f.conversation(conv.ID, conv.Title, recordingID)
		f.recording(recordingID, conv.ID, conv.StartedAt)
		for i, turn := range conv.Turns {
			if turn.StartMS == nil {
				f.exec(`INSERT INTO turns(id,conversation_id,recording_id,segment_index,global_start_ms,global_end_ms,speaker_scoped_id,speaker_entity_id,content,source_sha256,transcript_version,original_turn_id,turn_json)
 VALUES(?,?,?,?,NULL,NULL,NULL,NULL,?,'sha','rev-1',?,'{}')`, turn.ID, conv.ID, recordingID, i, turn.Text, turn.ID)
				continue
			}
			f.turn(turn.ID, conv.ID, recordingID, i, *turn.StartMS, nil, turn.Text)
		}
		for _, note := range conv.Notes {
			kind := note.Kind
			if kind == "" {
				kind = "observation"
			}
			f.note(note.ID, conv.ID, kind, note.Summary, nil, note.EvidenceTurns)
			if note.Stale {
				f.markStaleNote(note.ID, conv.ID)
			}
		}
		for _, turnID := range conv.AmbientTurns {
			f.exec(`INSERT INTO turn_classes(turn_id,class,subtype,method,created_at) VALUES(?,'ambient','','rule',1)`, turnID)
		}
		for _, turnID := range conv.ForgottenTurns {
			f.exec(`DELETE FROM note_evidence WHERE turn_id=?`, turnID)
			f.exec(`DELETE FROM turn_topics WHERE turn_id=?`, turnID)
			f.exec(`DELETE FROM turns WHERE id=?`, turnID)
		}
	}
	for _, topic := range setup.Topics {
		f.topic(topic.ID, topic.Title)
		for _, turnID := range topic.Turns {
			f.turnTopic(turnID, topic.ID)
		}
	}
}

func scenarioAssessor(setup scenarioSetup, scenario sufficiencyScenario) EvidenceAssessor {
	switch setup.Assessor {
	case "none":
		return nil
	case "error", "invalid_refs", "empty_sufficient":
		return scriptedAssessor{mode: setup.Assessor}
	default:
		return termAssessor{required: scenario.RequiredTerms, forbidden: scenario.ForbiddenTerms}
	}
}

// TestSufficiencyScenarios runs every labeled scenario through the real v2
// builder. A failure means either the fixture or the contract regressed; do
// not edit the expected status to make it pass.
func TestSufficiencyScenarios(t *testing.T) {
	scenarios := loadSufficiencyScenarios(t)
	if len(scenarios) < 20 {
		t.Fatalf("scenario count = %d, want at least 20", len(scenarios))
	}
	unanswerable := 0
	for _, scenario := range scenarios {
		if scenario.Unanswerable {
			unanswerable++
		}
	}
	if unanswerable < 5 {
		t.Fatalf("unanswerable scenarios = %d, want at least 5", unanswerable)
	}
	type result struct {
		ID             string   `json:"id"`
		Category       string   `json:"category"`
		ExpectedStatus string   `json:"expected_status"`
		ActualStatus   string   `json:"actual_status"`
		EvidenceRefs   []string `json:"evidence_refs"`
		Gaps           []string `json:"gaps"`
		Passed         bool     `json:"passed"`
	}
	results := make([]result, 0, len(scenarios))
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.ID, func(t *testing.T) {
			f := newFixture(t)
			seedScenario(t, f, scenario.Setup)
			deps := Deps{Store: f.store, DB: f.db, Assessor: scenarioAssessor(scenario.Setup, scenario)}
			pkg, err := Build(context.Background(), deps, Request{
				Mode: ModeHybrid, Query: scenario.Query, RetrievalVersion: "v2",
				ConversationID: scenario.Setup.ScopeConversation, MaxBytes: scenario.Setup.MaxBytes,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if pkg.Sufficiency == nil {
				t.Fatal("v2 package has no sufficiency assessment")
			}
			refs := append([]string(nil), pkg.Sufficiency.EvidenceRefs...)
			sort.Strings(refs)
			gaps := make([]string, 0, len(pkg.Gaps))
			for _, gap := range pkg.Gaps {
				gaps = append(gaps, gap.Kind)
			}
			passed := pkg.Sufficiency.Status == scenario.ExpectedStatus
			if passed {
				for _, want := range scenario.ExpectedEvidenceTurns {
					if !containsString(refs, want) {
						passed = false
						t.Errorf("evidence refs %v missing %q", refs, want)
					}
				}
				for _, forbidden := range scenario.ForbiddenEvidenceTurns {
					if containsString(refs, forbidden) {
						passed = false
						t.Errorf("forbidden evidence %q cited: %v", forbidden, refs)
					}
				}
				for _, wanted := range scenario.ExpectedGaps {
					if !containsString(gaps, wanted) {
						passed = false
						t.Errorf("gaps %v missing %q", gaps, wanted)
					}
				}
				for _, turnID := range scenario.ExpectNullEvidenceTimestamps {
					ref := findEvidence(pkg.Evidence, turnID)
					if ref == nil {
						passed = false
						t.Errorf("evidence %q absent", turnID)
						continue
					}
					if ref.GlobalStartMS != nil || ref.GlobalEndMS != nil {
						passed = false
						t.Errorf("evidence %q has invented timestamps %v-%v", turnID, *ref.GlobalStartMS, *ref.GlobalEndMS)
					}
				}
			}
			if pkg.Sufficiency.Status != scenario.ExpectedStatus {
				t.Errorf("status = %q, want %q (evidence=%v gaps=%v)", pkg.Sufficiency.Status, scenario.ExpectedStatus, refs, gaps)
			}
			results = append(results, result{
				ID: scenario.ID, Category: scenario.Category,
				ExpectedStatus: scenario.ExpectedStatus, ActualStatus: pkg.Sufficiency.Status,
				EvidenceRefs: refs, Gaps: gaps, Passed: passed,
			})
		})
	}
	if path := os.Getenv("SUFFICIENCY_REPORT"); path != "" {
		raw, err := json.MarshalIndent(map[string]any{
			"scenarios": results, "count": len(results), "unanswerable": unanswerable,
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func findEvidence(evidence []EvidenceRef, turnID string) *EvidenceRef {
	for i := range evidence {
		if evidence[i].TurnID == turnID {
			return &evidence[i]
		}
	}
	return nil
}
