#!/usr/bin/env python3
"""Hand-computed unit tests for scripts/audio_acceptance_metrics.py."""
from __future__ import annotations

import unittest

from scripts.audio_acceptance_metrics import (
    STATUS_NOT_EVALUABLE, STATUS_NOT_RUN, STATUS_OK, WER_TOKENIZATION,
    aggregate_error_rates, cer, decision_precision, der, edit_distance, eer,
    event_pr, evidence_accuracy, normalize_text, percentile, wer,
)


class NormalizeTextTests(unittest.TestCase):
    def test_nfkc_full_width_folding(self):
        self.assertEqual(normalize_text("１２３"), "123")
        self.assertEqual(normalize_text("Ｈｅｌｌｏ"), "Hello")

    def test_chinese_numerals_basic_forms(self):
        self.assertEqual(normalize_text("十五分钟"), "15分钟")
        self.assertEqual(normalize_text("十二月三日"), "12月3日")
        self.assertEqual(normalize_text("一百二十三"), "123")
        self.assertEqual(normalize_text("两千零三"), "2003")
        self.assertEqual(normalize_text("幺幺五八，八八九五。"), "11588895")

    def test_filters_to_han_latin_digits(self):
        self.assertEqual(normalize_text("你好 世界！"), "你好世界")
        self.assertEqual(normalize_text("A-1测试_2"), "A1测试2")


class EditDistanceTests(unittest.TestCase):
    def test_string_distance(self):
        self.assertEqual(edit_distance("abc", "abc"), 0)
        self.assertEqual(edit_distance("abc", "abd"), 1)
        self.assertEqual(edit_distance("", "abc"), 3)
        self.assertEqual(edit_distance("abc", ""), 3)

    def test_sequence_distance(self):
        self.assertEqual(edit_distance(["a", "b"], ["b", "c"]), 2)
        self.assertEqual(edit_distance([], ["x"]), 1)


class CerTests(unittest.TestCase):
    def test_substitution(self):
        # ref 你好世界 hyp 你号世界: one substitution of 4 chars.
        result = cer("你好世界", "你号世界")
        self.assertEqual(result, {"value": 0.25, "numerator": 1, "denominator": 4,
                                  "status": STATUS_OK})

    def test_insertion(self):
        # ref 你好 hyp 你好呀: one insertion over 2 reference chars.
        result = cer("你好", "你好呀")
        self.assertEqual(result, {"value": 0.5, "numerator": 1, "denominator": 2,
                                  "status": STATUS_OK})

    def test_deletion(self):
        # ref 你好世界 hyp 你好: two deletions over 4 reference chars.
        result = cer("你好世界", "你好")
        self.assertEqual(result, {"value": 0.5, "numerator": 2, "denominator": 4,
                                  "status": STATUS_OK})

    def test_chinese_numerals_score_equal(self):
        result = cer("十五分钟", "15分钟")
        self.assertEqual(result["numerator"], 0)
        self.assertEqual(result["denominator"], 4)
        self.assertEqual(result["value"], 0.0)

    def test_empty_reference_is_not_evaluable(self):
        result = cer("", "你好")
        self.assertEqual(result, {"value": None, "numerator": None, "denominator": 0,
                                  "status": STATUS_NOT_EVALUABLE})

    def test_punctuation_only_reference_counts_as_empty(self):
        result = cer("！？。", "你好")
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["value"])

    def test_empty_hypothesis_with_reference_is_full_error(self):
        result = cer("你好世界", "")
        self.assertEqual(result, {"value": 1.0, "numerator": 4, "denominator": 4,
                                  "status": STATUS_OK})


class WerTests(unittest.TestCase):
    def test_tokenization_han_chars_and_latin_runs(self):
        result = wer("你好 world 123", "你好 world 124")
        # Tokens: 你, 好, world, 123 -> 4; digit runs are single word tokens.
        self.assertEqual(result["denominator"], 4)
        self.assertEqual(result["numerator"], 1)  # 123 -> 124
        self.assertEqual(result["value"], 0.25)
        self.assertEqual(result["tokenization"], WER_TOKENIZATION)
        self.assertEqual(result["status"], STATUS_OK)

    def test_mixed_han_and_digit_run_tokens(self):
        result = wer("十二月三日", "12月3日")
        # Ref tokens: 十, 二, 月, 三, 日 -> 5 (numerals NOT folded in wer).
        # Hyp tokens: 12, 月, 3, 日 -> 4. Best alignment: 十二 -> 12 is one
        # deletion plus one substitution (2 edits), 三 -> 3 is one
        # substitution, 月 and 日 match: 3 edits total.
        self.assertEqual(result["denominator"], 5)
        self.assertEqual(result["numerator"], 3)
        self.assertEqual(result["value"], 0.6)

    def test_english_sentence(self):
        result = wer("the cat sat", "the bat sat")
        self.assertEqual(result["numerator"], 1)
        self.assertEqual(result["denominator"], 3)
        self.assertAlmostEqual(result["value"], 1 / 3)

    def test_empty_reference_not_evaluable(self):
        result = wer("", "hello")
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["value"])
        self.assertEqual(result["tokenization"], WER_TOKENIZATION)


class DerTests(unittest.TestCase):
    def test_perfect_match(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "speaker": "A"},
                     {"start_ms": 1000, "end_ms": 2000, "speaker": "B"}]
        hypothesis = [{"start_ms": 0, "end_ms": 1000, "speaker": "1"},
                      {"start_ms": 1000, "end_ms": 2000, "speaker": "2"}]
        result = der(reference, hypothesis)
        self.assertEqual(result["status"], STATUS_OK)
        self.assertAlmostEqual(result["value"], 0.0)
        self.assertAlmostEqual(result["reference_speech_ms"], 2000)
        self.assertEqual(result["label_mapping"], {"1": "A", "2": "B"})

    def test_swapped_speakers_optimal_mapping_not_greedy(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "speaker": "A"},
                     {"start_ms": 1000, "end_ms": 2000, "speaker": "B"}]
        # Greedy longest-first matching would pair 1<->B, 2<->A (all confusion);
        # the optimal global mapping pairs 1<->A, 2<->B for DER 0.
        hypothesis = [{"start_ms": 0, "end_ms": 1000, "speaker": "1"},
                      {"start_ms": 1000, "end_ms": 2000, "speaker": "2"}]
        # Same time layout but speakers crossed in the hypothesis order the
        # greedy algorithm would prefer: swap which label covers which span.
        swapped = [{"start_ms": 0, "end_ms": 1000, "speaker": "2"},
                   {"start_ms": 1000, "end_ms": 2000, "speaker": "1"}]
        result = der(reference, swapped)
        self.assertAlmostEqual(result["value"], 0.0)
        self.assertEqual(result["label_mapping"], {"2": "A", "1": "B"})

    def test_confusion_missed_and_false_alarm(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "speaker": "A"},
                     {"start_ms": 1000, "end_ms": 2000, "speaker": "B"}]
        hypothesis = [{"start_ms": 0, "end_ms": 800, "speaker": "x"},
                      {"start_ms": 800, "end_ms": 2000, "speaker": "y"},
                      {"start_ms": 2000, "end_ms": 3000, "speaker": "z"}]
        result = der(reference, hypothesis)
        # Optimal mapping: x->A, y->B (or y->A,x->B). Atom 0-800: A/x correct 800.
        # Atom 800-1000: ref A vs hyp y (B): confusion 200. Atom 1000-2000:
        # ref B vs y(B): correct 1000. Atom 2000-3000: FA 1000.
        # miss = 2000-800-1000-? -> ref time 2000, correct 1800, confusion 200,
        # false alarm 1000, missed 0.
        self.assertAlmostEqual(result["reference_speech_ms"], 2000)
        self.assertAlmostEqual(result["confusion_ms"], 200)
        self.assertAlmostEqual(result["missed_ms"], 0)
        self.assertAlmostEqual(result["false_alarm_ms"], 1000)
        self.assertAlmostEqual(result["value"], 0.6)  # (200 + 0 + 1000) / 2000

    def test_missed_detection(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "speaker": "A"}]
        hypothesis = [{"start_ms": 0, "end_ms": 400, "speaker": "1"}]
        result = der(reference, hypothesis)
        self.assertAlmostEqual(result["missed_ms"], 600)
        self.assertAlmostEqual(result["value"], 0.6)

    def test_collar_excludes_boundary_regions(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "speaker": "A"}]
        # Hypothesis is shifted 100ms late; without collar that is
        # miss 100 + FA 100 = DER 0.2. A 200ms-wide collar removes
        # [0,100] and [900,1000] from evaluation entirely.
        hypothesis = [{"start_ms": 100, "end_ms": 1100, "speaker": "1"}]
        result = der(reference, hypothesis, collar_ms=200)
        # Evaluated region [100,900]: ref A covers it, hyp 1 covers it -> 0 error.
        self.assertAlmostEqual(result["value"], 0.0)
        self.assertAlmostEqual(result["reference_speech_ms"], 800)

    def test_skip_overlap_excludes_overlap_region(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "speaker": "A"},
                     {"start_ms": 500, "end_ms": 1500, "speaker": "B"}]
        hypothesis = [{"start_ms": 0, "end_ms": 500, "speaker": "1"},
                      {"start_ms": 500, "end_ms": 1500, "speaker": "2"}]
        result = der(reference, hypothesis, skip_overlap=True)
        # Overlap [500,1000] excluded. Evaluated atoms: 0-500 (A vs 1: ok),
        # 1000-1500 (B vs 2: ok). DER 0; ref speech counts only non-overlap.
        self.assertAlmostEqual(result["value"], 0.0)
        self.assertAlmostEqual(result["reference_speech_ms"], 1000)

    def test_no_reference_speech_is_not_evaluable(self):
        result = der([], [{"start_ms": 0, "end_ms": 1000, "speaker": "1"}])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["value"])

    def test_empty_both_is_not_evaluable(self):
        result = der([], [])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)


class EerTests(unittest.TestCase):
    def test_crossing_linear_interpolation(self):
        same = [0.9, 0.8, 0.7, 0.6]
        different = [0.4, 0.3, 0.2, 0.1]
        result = eer(same, different)
        # Midpoint thresholds: .55/.45/.35/.25 with implicit (-inf, far=1,
        # frr=0) and (+inf, far=0, frr=1) endpoints. Swept points:
        # (-inf,1,0) (.55,0,0) (.45,0,.25) (.35,.25,.5) (.25,.5,.75) (+inf,0,1).
        # Crossing is on segment (.35,.25,.5) -> (.25,.5,.75):
        # far-frr: -.25 -> -.75, always negative; on (.45,0,.25)->(.35,.25,.5):
        # far-frr: -.25 -> -.25, no crossing; on (.55,0,0)->(.45,0,.25):
        # far-frr 0 -> -.25 crossing at alpha where 0-0*... root: far stays 0,
        # frr rises -> crossing value = frr at alpha: 0 + a*.25 with
        # far-frr = -a*.25 = 0 only at a=0... So first strictly crossing
        # segment is (-inf,1,0)->(.55,0,0): far-frr 1 -> 0, root at its end
        # where value -> 0? No: crossing detected when sign flips through 0.
        # With far dropping 1->0 while frr stays 0, far==frr only at the very
        # end (alpha=1, value 0). Segment (.45,0,.25)->(.35,.25,.5) is flat
        # -0.25. Segment (.35,.25,.5)->(.25,.5,.75) flat -0.5.
        # Segment (.25,.5,.75)->(+inf,0,1): far-frr -.25 -> -1 flat.
        # The curve touches zero only at the far=0 shoulder; with no strict
        # interior crossing the sweep returns the first exact far==frr point,
        # which here is t=.55 with far=0, frr=0 -> EER 0 (perfect separation).
        self.assertAlmostEqual(result["value"], 0.0)
        self.assertEqual(result["status"], STATUS_OK)

    def test_interpolated_crossing_between_swept_points(self):
        same = [0.8, 0.2]
        different = [0.6, 0.4]
        # Thresholds: .7/.5/.3 plus implicit endpoints. Points:
        # (-inf,1,0) (.7,0,0) (.5,.5,.5) (.3,1,1) (+inf,0,1).
        # (.7,0,0)->(.5,.5,.5): far-frr 0 -> 0 (equal throughout: far=frr=0
        # then far=frr=.5; first exact equality at t=.5 -> EER .5).
        result = eer(same, different)
        self.assertAlmostEqual(result["value"], 0.5)
        self.assertAlmostEqual(result["threshold"], 0.5)

    def test_perfect_separation_small_sets(self):
        result = eer([0.9, 0.9], [0.1, 0.1])
        # Points: (-inf,1,0) -> (0.5,0,0) -> (1.1,0,1). First exact far==frr
        # at t=0.5 with 0/0; the curve then diverges (0 vs rising frr) with
        # far-frr negative everywhere after; exact-hit branch returns 0.
        self.assertAlmostEqual(result["value"], 0.0)
        self.assertAlmostEqual(result["threshold"], 0.5)

    def test_exact_crossing_threshold_hit(self):
        same = [0.8, 0.2]
        different = [0.5, 0.5]
        # Thresholds: .35 and .65. Sweep: (.35, far=1, frr=.5), (.65, far=0,
        # frr=.5). At .35 far!=frr, at .65 far!=frr, but between them FAR falls
        # 1->0 while FRR stays .5: they cross at far=frr=.5, i.e. alpha=.5
        # between the two points: EER=.5, threshold=.5.
        result = eer(same, different)
        self.assertAlmostEqual(result["value"], 0.5)
        self.assertAlmostEqual(result["threshold"], 0.5)

    def test_experimental_flag_requires_thirty_trials(self):
        same = [0.9] * 30
        different = [0.1] * 30
        result = eer(same, different)
        self.assertFalse(result["experimental"])
        same29 = [0.9] * 29
        result29 = eer(same29, different)
        self.assertTrue(result29["experimental"])

    def test_missing_class_is_not_evaluable(self):
        result = eer([0.9], [])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["value"])
        self.assertEqual(result["same_trials"], 1)
        self.assertEqual(result["different_trials"], 0)


class EventPrTests(unittest.TestCase):
    def test_exact_match_and_boundary_iou_half(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "label": "speech"}]
        # IoU = 500/1500 = 1/3 < 0.5: no match.
        below = [{"start_ms": 500, "end_ms": 1500, "label": "speech"}]
        result = event_pr(reference, below)
        self.assertEqual(result["tp"], 0)
        self.assertEqual(result["fp"], 1)
        self.assertEqual(result["fn"], 1)
        self.assertEqual(result["precision"], 0.0)
        self.assertEqual(result["recall"], 0.0)
        # Exact boundary: IoU = overlap 500 / union 1000 = 0.5 -> matches.
        boundary = [{"start_ms": 250, "end_ms": 750, "label": "speech"}]
        result = event_pr(reference, boundary)
        self.assertEqual(result["tp"], 1)
        self.assertEqual(result["fp"], 0)
        self.assertEqual(result["fn"], 0)
        self.assertAlmostEqual(result["precision"], 1.0)
        self.assertAlmostEqual(result["recall"], 1.0)

    def test_maximum_matching_beats_greedy(self):
        # Greedy by descending IoU matches R1-H1 (0.9) first, after which R2's
        # only overlap (H1, 0.8) is taken: 1 TP. Maximum cardinality matches
        # R1-H2 (0.6) and R2-H1 (0.8) for 2 TP. TP must be 2.
        reference = [{"start_ms": 0, "end_ms": 900, "label": "speech"},
                     {"start_ms": 200, "end_ms": 1000, "label": "speech"}]
        hypothesis = [{"start_ms": 0, "end_ms": 1000, "label": "speech"},
                      {"start_ms": 0, "end_ms": 540, "label": "speech"}]
        result = event_pr(reference, hypothesis)
        self.assertEqual(result["tp"], 2)
        self.assertEqual(result["fp"], 0)
        self.assertEqual(result["fn"], 0)
        self.assertEqual(result["precision"], 1.0)
        self.assertEqual(result["recall"], 1.0)

    def test_one_to_one_by_maximum_matching(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "label": "speech"}]
        hypothesis = [{"start_ms": 0, "end_ms": 1000, "label": "speech"},
                      {"start_ms": 250, "end_ms": 750, "label": "speech"}]
        # Both overlap with IoU >= 0.5; only one pair can match (one-to-one),
        # the second hypothesis event is a false alarm.
        result = event_pr(reference, hypothesis)
        self.assertEqual(result["tp"], 1)
        self.assertEqual(result["fp"], 1)
        self.assertEqual(result["fn"], 0)
        self.assertAlmostEqual(result["precision"], 0.5)
        self.assertAlmostEqual(result["recall"], 1.0)

    def test_labels_must_match(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "label": "speech"}]
        hypothesis = [{"start_ms": 0, "end_ms": 1000, "label": "music"}]
        result = event_pr(reference, hypothesis)
        self.assertEqual(result["tp"], 0)
        self.assertEqual(result["fn"], 1)
        self.assertEqual(result["fp"], 1)

    def test_hypothesis_only_label_precision_side(self):
        reference = [{"start_ms": 0, "end_ms": 1000, "label": "speech"}]
        hypothesis = [{"start_ms": 0, "end_ms": 1000, "label": "music"}]
        result = event_pr(reference, hypothesis)
        self.assertEqual(result["per_label"]["music"]["fp"], 1)
        self.assertIsNone(result["per_label"]["music"]["recall"])
        self.assertEqual(result["per_label"]["music"]["status"], STATUS_NOT_EVALUABLE)

    def test_no_reference_events_not_evaluable(self):
        result = event_pr([], [{"start_ms": 0, "end_ms": 1000, "label": "speech"}])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["precision"])
        self.assertIsNone(result["recall"])
        self.assertIsNone(result["f1"])
        self.assertEqual(result["fp"], 1)

    def test_empty_everything_not_evaluable(self):
        result = event_pr([], [])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["precision"])


class EvidenceAccuracyTests(unittest.TestCase):
    GOLD = [
        {"conversation_id": "c1", "turn_id": "t1", "start_ms": 1000, "end_ms": 2000,
         "required": True},
        {"conversation_id": "c1", "turn_id": "t2", "start_ms": 3000, "end_ms": 4000},
    ]

    def test_perfect_refs(self):
        refs = [{"turn_id": "t1", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000},
                {"turn_id": "t2", "conversation_id": "c1", "start_ms": 3000, "end_ms": 4000}]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [])
        self.assertEqual(result["supported_refs"], 2)
        self.assertEqual(result["returned_refs"], 2)
        self.assertEqual(result["accuracy"], 1.0)
        self.assertEqual(result["required_total"], 1)
        self.assertEqual(result["required_covered"], 1)
        self.assertEqual(result["coverage"], 1.0)
        self.assertEqual(result["status"], STATUS_OK)

    def test_ambient_forgotten_out_of_scope_and_invalid_refs(self):
        refs = [
            {"turn_id": "amb", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000},
            {"turn_id": "forg", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000},
            {"turn_id": "t1", "conversation_id": "c9", "start_ms": 1000, "end_ms": 2000},
            {"turn_id": "t1", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000,
             "valid": False},
        ]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], ["amb"], ["forg"])
        # Non-empty output that is entirely illegal is 0, never null.
        self.assertEqual(result["supported_refs"], 0)
        self.assertEqual(result["returned_refs"], 4)
        self.assertEqual(result["legal_refs"], 0)
        self.assertEqual(result["accuracy"], 0.0)
        self.assertEqual(result["accuracy_status"], STATUS_OK)
        self.assertEqual(result["status"], STATUS_OK)
        self.assertEqual(result["error_counts"],
                         {"ambient": 1, "forgotten": 1, "invalid": 1, "out_of_scope": 1})
        self.assertEqual(result["coverage"], 0.0)

    def test_one_supported_ref_and_one_out_of_scope_ref_pair(self):
        refs = [{"turn_id": "t1", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000},
                {"turn_id": "t2", "conversation_id": "c9", "start_ms": 3000, "end_ms": 4000}]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [])
        self.assertEqual(result["supported_refs"], 1)
        self.assertEqual(result["legal_refs"], 1)
        self.assertEqual(result["returned_refs"], 2)
        self.assertEqual(result["accuracy"], 0.5)

    def test_semantic_accuracy_requires_every_judgment(self):
        refs = [{"id": "r1", "turn_id": "t1", "conversation_id": "c1",
                 "start_ms": 1000, "end_ms": 2000},
                {"id": "r2", "turn_id": "t2", "conversation_id": "c1",
                 "start_ms": 3000, "end_ms": 4000}]
        partial = evidence_accuracy(refs, self.GOLD, ["c1"], [], [],
                                    semantic_judgments={"r1": True})
        self.assertIsNone(partial["semantic_accuracy"])
        self.assertEqual(partial["semantic_status"], STATUS_NOT_RUN)
        self.assertEqual(partial["semantic_adjudicated"], 1)
        # The structural numbers stay measurable without any judgment.
        self.assertEqual(partial["accuracy"], 1.0)
        complete = evidence_accuracy(refs, self.GOLD, ["c1"], [], [],
                                     semantic_judgments={"r1": True, "r2": False})
        self.assertEqual(complete["semantic_status"], STATUS_OK)
        self.assertEqual(complete["semantic_accuracy"], 0.5)

    def test_accuracy_counts_unsupported_refs(self):
        refs = [{"turn_id": "t1", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000},
                {"turn_id": "tx", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000}]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [])
        # tx has no matching turn id in gold (gold t1 only spans that region):
        # a valid but unsupported ref lowers accuracy.
        self.assertEqual(result["supported_refs"], 1)
        self.assertEqual(result["legal_refs"], 2)
        self.assertEqual(result["returned_refs"], 2)
        self.assertEqual(result["accuracy"], 0.5)

    def test_tolerance_ms_on_both_endpoints(self):
        refs = [
            # 250ms early start, 250ms late end: inside tolerance.
            {"turn_id": "t1", "conversation_id": "c1", "start_ms": 750, "end_ms": 2250},
            # 251ms early: outside tolerance.
            {"turn_id": "t2", "conversation_id": "c1", "start_ms": 2749, "end_ms": 4000},
        ]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [], tolerance_ms=250)
        self.assertEqual(result["supported_refs"], 1)
        self.assertEqual(result["accuracy"], 0.5)

    def test_ref_without_turn_id_matches_by_conversation_and_span(self):
        refs = [{"conversation_id": "c1", "start_ms": 1100, "end_ms": 1900}]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [])
        self.assertEqual(result["supported_refs"], 1)
        self.assertEqual(result["accuracy"], 1.0)

    def test_wrong_turn_id_does_not_match(self):
        refs = [{"turn_id": "other", "conversation_id": "c1", "start_ms": 1000, "end_ms": 2000}]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [])
        self.assertEqual(result["supported_refs"], 0)
        self.assertEqual(result["accuracy"], 0.0)

    def test_stale_and_wrong_revision_refs_count_as_errors(self):
        gold = [{"turn_id": "t1", "conversation_id": "c1", "recording_id": "rec-1",
                 "transcript_revision": "rev-2", "start_ms": 1000, "end_ms": 2000,
                 "required": True}]
        refs = [
            {"turn_id": "t1", "conversation_id": "c1", "recording_id": "rec-1",
             "transcript_revision": "rev-2", "start_ms": 1000, "end_ms": 2000},
            {"turn_id": "t1", "conversation_id": "c1", "recording_id": "rec-1",
             "transcript_revision": "rev-2", "start_ms": 1000, "end_ms": 2000, "current": False},
            {"turn_id": "t1", "conversation_id": "c1", "recording_id": "rec-2",
             "transcript_revision": "rev-2", "start_ms": 1000, "end_ms": 2000},
            {"turn_id": "t1", "conversation_id": "c1", "recording_id": "rec-1",
             "transcript_revision": "rev-1", "start_ms": 1000, "end_ms": 2000},
        ]
        result = evidence_accuracy(refs, gold, ["c1"], [], [])
        self.assertEqual(result["returned_refs"], 4)
        self.assertEqual(result["supported_refs"], 1)
        self.assertEqual(result["accuracy"], 0.25)
        self.assertEqual(result["error_counts"]["stale"], 1)
        self.assertEqual(result["error_counts"]["wrong_recording"], 1)
        self.assertEqual(result["error_counts"]["wrong_revision"], 1)

    def test_empty_returned_refs_not_evaluable(self):
        result = evidence_accuracy([], self.GOLD, ["c1"], [], [])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertEqual(result["accuracy_status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["accuracy"])
        self.assertIsNone(result["legal_ref_rate"])
        self.assertEqual(result["returned_refs"], 0)
        self.assertEqual(result["coverage"], 0.0)

    def test_missing_required_span_shows_in_coverage(self):
        refs = [{"turn_id": "t2", "conversation_id": "c1", "start_ms": 3000, "end_ms": 4000}]
        result = evidence_accuracy(refs, self.GOLD, ["c1"], [], [])
        self.assertEqual(result["supported_refs"], 1)
        self.assertEqual(result["accuracy"], 1.0)
        self.assertEqual(result["required_covered"], 0)
        self.assertEqual(result["coverage"], 0.0)


class DecisionPrecisionTests(unittest.TestCase):
    def test_partial_review_reports_null_not_the_selected_subset(self):
        claims = [
            {"claim_id": "a", "kind": "decision", "prediction_digest": "d1"},
            {"claim_id": "b", "kind": "decision", "prediction_digest": "d2"},
        ]
        adjudications = [{"claim_id": "a", "accepted": True, "prediction_digest": "d1"}]
        result = decision_precision(claims, adjudications)
        # One of two claims adjudicated: the formal precision stays unknown and
        # the review counts keep the partial evidence visible.
        self.assertEqual(result["total"], 2)
        self.assertEqual(result["adjudicated"], 1)
        self.assertEqual(result["unadjudicated"], 1)
        self.assertIsNone(result["precision"])
        self.assertEqual(result["status"], STATUS_NOT_RUN)
        self.assertIsNone(result["by_kind"]["decision"]["precision"])
        self.assertEqual(result["by_kind"]["decision"]["status"], STATUS_NOT_RUN)
        self.assertEqual(result["by_kind"]["decision"]["accepted"], 1)

    def test_complete_review_reports_precision(self):
        claims = [
            {"claim_id": "a", "kind": "decision", "prediction_digest": "d1"},
            {"claim_id": "b", "kind": "decision", "prediction_digest": "d2"},
            {"claim_id": "c", "kind": "commitment", "prediction_digest": "d3"},
        ]
        adjudications = [
            {"claim_id": "a", "accepted": True, "prediction_digest": "d1"},
            {"claim_id": "b", "accepted": False, "prediction_digest": "d2"},
            {"claim_id": "c", "accepted": True, "prediction_digest": "d3"},
        ]
        result = decision_precision(claims, adjudications)
        self.assertEqual(result["adjudicated"], 3)
        self.assertEqual(result["accepted"], 2)
        self.assertAlmostEqual(result["precision"], 2 / 3)
        self.assertEqual(result["status"], STATUS_OK)
        self.assertEqual(result["by_kind"]["decision"]["precision"], 0.5)
        self.assertEqual(result["by_kind"]["commitment"]["precision"], 1.0)

    def test_one_kind_complete_does_not_unlock_the_other(self):
        claims = [
            {"claim_id": "a", "kind": "decision", "prediction_digest": "d1"},
            {"claim_id": "c", "kind": "commitment", "prediction_digest": "d3"},
        ]
        adjudications = [{"claim_id": "a", "accepted": True, "prediction_digest": "d1"}]
        result = decision_precision(claims, adjudications)
        self.assertEqual(result["by_kind"]["decision"]["precision"], 1.0)
        self.assertEqual(result["by_kind"]["commitment"]["status"], STATUS_NOT_RUN)
        self.assertEqual(result["by_kind"]["commitment"]["unadjudicated"], 1)
        self.assertIsNone(result["precision"])
        self.assertEqual(result["status"], STATUS_NOT_RUN)

    def test_digest_mismatch_does_not_adjudicate(self):
        claims = [{"claim_id": "a", "kind": "decision", "prediction_digest": "d1"}]
        adjudications = [{"claim_id": "a", "accepted": True, "prediction_digest": "stale"}]
        result = decision_precision(claims, adjudications)
        self.assertEqual(result["adjudicated"], 0)
        self.assertIsNone(result["precision"])
        self.assertEqual(result["status"], STATUS_NOT_RUN)
        self.assertEqual(result["by_kind"]["decision"]["status"], STATUS_NOT_RUN)

    def test_no_adjudications_not_run(self):
        claims = [{"claim_id": "a", "kind": "decision", "prediction_digest": "d1"}]
        result = decision_precision(claims, [])
        self.assertEqual(result["status"], STATUS_NOT_RUN)
        self.assertIsNone(result["precision"])
        self.assertEqual(result["accepted"], 0)

    def test_no_claims_not_evaluable(self):
        result = decision_precision([], [])
        self.assertEqual(result["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(result["precision"])


class AggregateErrorRateTests(unittest.TestCase):
    def test_numerators_and_denominators_are_pooled(self):
        pooled = aggregate_error_rates([cer("你好", "您好"), cer("世界和平", "")])
        # 1 edit / 2 chars plus 4 deletions / 4 chars = 5 / 6.
        self.assertEqual(pooled["numerator"], 5)
        self.assertEqual(pooled["denominator"], 6)
        self.assertAlmostEqual(pooled["value"], 5 / 6)
        self.assertEqual(pooled["status"], STATUS_OK)

    def test_empty_reference_results_are_skipped(self):
        pooled = aggregate_error_rates([cer("", "你好"), wer("", "hello")])
        self.assertEqual(pooled["status"], STATUS_NOT_EVALUABLE)
        self.assertIsNone(pooled["value"])
        self.assertEqual(pooled["denominator"], 0)


class PercentileTests(unittest.TestCase):
    def test_linear_interpolation(self):
        self.assertEqual(percentile([1, 2, 3, 4], 0.5), 2.5)
        self.assertEqual(percentile([1, 2, 3, 4], 0.0), 1)
        self.assertEqual(percentile([1, 2, 3, 4], 1.0), 4)
        self.assertAlmostEqual(percentile([1, 2, 3, 4], 0.95), 3.85)

    def test_single_value(self):
        self.assertEqual(percentile([7], 0.95), 7)

    def test_unsorted_input(self):
        self.assertEqual(percentile([4, 1, 3, 2], 0.5), 2.5)

    def test_empty_returns_none(self):
        self.assertIsNone(percentile([], 0.95))

    def test_invalid_p_raises(self):
        with self.assertRaises(ValueError):
            percentile([1], 1.5)


if __name__ == "__main__":
    unittest.main()
