package contextbuild

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recordingmcp/internal/decision"
)

// Plan 6.1: the assessor's answer contract is a four-item finite distribution
// that sums to 1 within 1e-6, and the choice must be the argmax: the highest
// probability over all four items, with the first item of relationPriority
// among those within 1e-9 of the maximum. The returned probability is
// P(supports)+P(refutes). LoadAdmissionPolicy additionally requires the raw
// policy JSON to carry an explicit, non-null threshold; threshold 0 stays
// legal.

func admissionChoiceAnswer(choice string, distribution map[string]float64) decision.Answer {
	return decision.Answer{Type: decision.TypeChoice, Choice: choice, Distribution: distribution}
}

// TestValidateAdmissionAnswerRejectsChoiceBehindHigherProbabilityItem covers
// the distribution whose argmax sits after the choice in relationPriority:
// the old loop stopped at the choice's own label, so a later, strictly larger
// item was never examined.
func TestValidateAdmissionAnswerRejectsChoiceBehindHigherProbabilityItem(t *testing.T) {
	answer := admissionChoiceAnswer(RelationSupports, map[string]float64{
		RelationSupports: 0.2, RelationRefutes: 0.1, RelationRelated: 0.6, RelationUnknown: 0.1,
	})
	probability, err := validateAdmissionAnswer(answer)
	if err == nil {
		t.Fatalf("validateAdmissionAnswer accepted a non-argmax choice, probability=%v", probability)
	}
	if !strings.Contains(err.Error(), "argmax") {
		t.Fatalf("err = %v, want an argmax rejection", err)
	}
}

// TestValidateAdmissionAnswerRejectsTiedChoiceNotFirstInPriority covers exact
// and near ties: when an earlier relationPriority item sits within 1e-9 of the
// maximum, it is the required choice, so choosing a later tied item is
// rejected even though it also reaches the maximum.
func TestValidateAdmissionAnswerRejectsTiedChoiceNotFirstInPriority(t *testing.T) {
	for name, answer := range map[string]decision.Answer{
		"exact_tie_between_supports_and_refutes": admissionChoiceAnswer(RelationRefutes, map[string]float64{
			RelationSupports: 0.4, RelationRefutes: 0.4, RelationRelated: 0.1, RelationUnknown: 0.1,
		}),
		"tie_reaches_choice_after_two_higher_items": admissionChoiceAnswer(RelationRelated, map[string]float64{
			RelationSupports: 0.4, RelationRefutes: 0.1, RelationRelated: 0.4, RelationUnknown: 0.1,
		}),
		"near_tie_where_earlier_item_is_within_tolerance": admissionChoiceAnswer(RelationRefutes, map[string]float64{
			RelationSupports: 0.4, RelationRefutes: 0.4000000005, RelationRelated: 0.0999999995, RelationUnknown: 0.1,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateAdmissionAnswer(answer); err == nil {
				t.Fatalf("validateAdmissionAnswer accepted a choice that is not the first priority item within 1e-9 of the maximum")
			}
		})
	}
}

// TestValidateAdmissionAnswerAcceptsArgmaxChoice pins the accepted side of the
// contract: the choice may be the strict maximum, the first item of a tie, or
// an earlier item within 1e-9 of a slightly larger maximum.
func TestValidateAdmissionAnswerAcceptsArgmaxChoice(t *testing.T) {
	for name, tc := range map[string]struct {
		answer decision.Answer
		want   float64
	}{
		"strict_maximum": {
			answer: admissionChoiceAnswer(RelationRefutes, map[string]float64{
				RelationSupports: 0.3, RelationRefutes: 0.5, RelationRelated: 0.1, RelationUnknown: 0.1,
			}),
			// The gate input is P(supports)+P(refutes), not the choice's own
			// probability.
			want: 0.8,
		},
		"first_item_of_exact_tie": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.4, RelationRefutes: 0.4, RelationRelated: 0.1, RelationUnknown: 0.1,
			}),
			want: 0.8,
		},
		"earlier_item_within_tolerance_of_maximum": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.4, RelationRefutes: 0.4000000005, RelationRelated: 0.0999999995, RelationUnknown: 0.1,
			}),
			want: 0.4 + 0.4000000005,
		},
		"uniform_distribution": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.25, RelationRefutes: 0.25, RelationRelated: 0.25, RelationUnknown: 0.25,
			}),
			want: 0.5,
		},
		"unknown_is_a_valid_argmax": {
			answer: admissionChoiceAnswer(RelationUnknown, map[string]float64{
				RelationSupports: 0.1, RelationRefutes: 0.1, RelationRelated: 0.2, RelationUnknown: 0.6,
			}),
			want: 0.2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			probability, err := validateAdmissionAnswer(tc.answer)
			if err != nil {
				t.Fatalf("validateAdmissionAnswer: %v", err)
			}
			if math.Abs(probability-tc.want) > 1e-12 {
				t.Fatalf("probability = %v, want %v (P(supports)+P(refutes))", probability, tc.want)
			}
		})
	}
}

// TestValidateAdmissionAnswerReturnsSupportsPlusRefutesProbability keeps the
// gate input formula independent of the chosen relation.
func TestValidateAdmissionAnswerReturnsSupportsPlusRefutesProbability(t *testing.T) {
	answer := admissionChoiceAnswer(RelationUnknown, map[string]float64{
		RelationSupports: 0.3, RelationRefutes: 0.2, RelationRelated: 0.1, RelationUnknown: 0.4,
	})
	probability, err := validateAdmissionAnswer(answer)
	if err != nil {
		t.Fatalf("validateAdmissionAnswer: %v", err)
	}
	if probability != 0.5 {
		t.Fatalf("probability = %v, want 0.5", probability)
	}
}

// TestValidateAdmissionAnswerRejectsMalformedDistributions keeps the finite
// four-item distribution and the 1e-6 sum tolerance enforced.
func TestValidateAdmissionAnswerRejectsMalformedDistributions(t *testing.T) {
	full := func() map[string]float64 {
		return map[string]float64{
			RelationSupports: 0.25, RelationRefutes: 0.25, RelationRelated: 0.25, RelationUnknown: 0.25,
		}
	}
	for name, tc := range map[string]struct {
		answer    decision.Answer
		errSubstr string
	}{
		"not_a_choice": {
			answer:    decision.Answer{Type: decision.TypeScore, Choice: RelationSupports, Distribution: full()},
			errSubstr: "choice",
		},
		"invalid_choice_label": {
			answer:    admissionChoiceAnswer("maybe", full()),
			errSubstr: "choice",
		},
		"three_item_distribution": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.4, RelationRefutes: 0.3, RelationRelated: 0.3,
			}),
			errSubstr: "distribution",
		},
		"unknown_extra_label": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.2, RelationRefutes: 0.2, RelationRelated: 0.2, RelationUnknown: 0.2, "other": 0.2,
			}),
			errSubstr: "distribution",
		},
		"missing_refutes_label": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.5, RelationRelated: 0.25, RelationUnknown: 0.25,
			}),
			errSubstr: "distribution",
		},
		"sum_beyond_tolerance": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.5, RelationRefutes: 0.25, RelationRelated: 0.125, RelationUnknown: 0.125002,
			}),
			errSubstr: "sum",
		},
		"negative_probability": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 0.7, RelationRefutes: -0.2, RelationRelated: 0.25, RelationUnknown: 0.25,
			}),
			errSubstr: "distribution",
		},
		"probability_above_one": {
			answer: admissionChoiceAnswer(RelationSupports, map[string]float64{
				RelationSupports: 1.1, RelationRefutes: 0.0, RelationRelated: 0.0, RelationUnknown: -0.1,
			}),
			errSubstr: "distribution",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateAdmissionAnswer(tc.answer)
			if err == nil {
				t.Fatalf("validateAdmissionAnswer accepted malformed answer %+v", tc.answer)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.errSubstr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.errSubstr)
			}
		})
	}
	t.Run("sum_within_tolerance_is_accepted", func(t *testing.T) {
		answer := admissionChoiceAnswer(RelationSupports, map[string]float64{
			RelationSupports: 0.5, RelationRefutes: 0.25, RelationRelated: 0.125, RelationUnknown: 0.1250000005,
		})
		if _, err := validateAdmissionAnswer(answer); err != nil {
			t.Fatalf("validateAdmissionAnswer rejected a distribution within the 1e-6 sum tolerance: %v", err)
		}
	})
	t.Run("non_finite_probability_is_rejected", func(t *testing.T) {
		answer := admissionChoiceAnswer(RelationSupports, map[string]float64{
			RelationSupports: math.NaN(), RelationRefutes: 0.25, RelationRelated: 0.25, RelationUnknown: 0.25,
		})
		if _, err := validateAdmissionAnswer(answer); err == nil {
			t.Fatal("NaN probability accepted")
		}
		answer = admissionChoiceAnswer(RelationSupports, map[string]float64{
			RelationSupports: math.Inf(1), RelationRefutes: 0.25, RelationRelated: 0.25, RelationUnknown: 0.25,
		})
		if _, err := validateAdmissionAnswer(answer); err == nil {
			t.Fatal("+Inf probability accepted")
		}
	})
}

// writeAdmissionPolicyFile writes a raw policy JSON document for
// LoadAdmissionPolicy; extra overrides raw JSON when non-empty.
func writeAdmissionPolicyFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admission-policy.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func selectedPolicyJSONWithoutThreshold() string {
	return `{"schema_version":"` + AdmissionPolicySchema + `","retrieval_version":"v3",` +
		`"provider_id":"fake","model_id":"test","model_revision":"rev-1",` +
		`"assessor_model":"stub-assessor","fixture_sha256":"` + strings.Repeat("a", 64) + `",` +
		`"phase":"selected"}`
}

// TestLoadAdmissionPolicyRequiresExplicitThreshold covers the raw-JSON gate:
// a missing or null threshold is a malformed policy even though the decoded
// float64 zero would be in range; an explicit 0 is legal.
func TestLoadAdmissionPolicyRequiresExplicitThreshold(t *testing.T) {
	base := selectedPolicyJSONWithoutThreshold()
	base = strings.TrimSuffix(base, "}")
	for name, raw := range map[string]string{
		"missing_threshold": base + "}",
		"null_threshold":    base + `,"threshold":null}`,
		"string_threshold":  base + `,"threshold":"0.5"}`,
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := LoadAdmissionPolicy(writeAdmissionPolicyFile(t, raw))
			if err == nil {
				t.Fatalf("LoadAdmissionPolicy accepted %+v", policy)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "threshold") {
				t.Fatalf("err = %v, want it to mention threshold", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"explicit_zero": base + `,"threshold":0}`,
		"explicit_one":  base + `,"threshold":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			policy, err := LoadAdmissionPolicy(writeAdmissionPolicyFile(t, raw))
			if err != nil {
				t.Fatalf("LoadAdmissionPolicy: %v", err)
			}
			want := 0.0
			if name == "explicit_one" {
				want = 1.0
			}
			if policy.Threshold != want {
				t.Fatalf("threshold = %v, want %v", policy.Threshold, want)
			}
			if err := policy.Validate(false); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestLoadAdmissionPolicyReportsMissingFile keeps the read failure distinct
// from the threshold gate so callers can tell a malformed policy from an
// unreadable one.
func TestLoadAdmissionPolicyReportsMissingFile(t *testing.T) {
	_, err := LoadAdmissionPolicy(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || errors.Is(err, os.ErrNotExist) == false {
		t.Fatalf("err = %v, want a not-exist read error", err)
	}
}
