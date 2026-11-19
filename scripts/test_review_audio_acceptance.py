#!/usr/bin/env python3
"""Tests for the human gold loop and offline re-scoring.

Every fixture is synthetic; no real recording, model or service is touched.
The audio is a tiny 16 kHz mono WAV written with the standard library.
"""
from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import sys
import tempfile
import unittest
import wave
from argparse import Namespace
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import audio_acceptance as acceptance  # noqa: E402

SPEC = importlib.util.spec_from_file_location("review_audio_acceptance",
                                              ROOT / "scripts/review-audio-acceptance.py")
review = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
sys.modules["review_audio_acceptance"] = review
SPEC.loader.exec_module(review)


def write_wav(path: Path, seconds: float = 1.0, rate: int = 16000) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as stream:
        stream.setnchannels(1)
        stream.setsampwidth(2)
        stream.setframerate(rate)
        stream.writeframes(b"\x00\x00" * int(seconds * rate))


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def prepare_args(tmp: Path, **overrides) -> Namespace:
    audio = overrides.pop("audio", None) or (tmp / "authorized.wav")
    if "reference" not in overrides and not audio.exists():
        write_wav(audio)
    values = {"audio": str(audio), "sample_id": "s1", "out": str(tmp / "prep"),
              "reference": "", "duration_ms": 0}
    values.update(overrides)
    return Namespace(**values)


def reviewed_draft(prep_root: Path, **overrides) -> dict:
    draft = json.loads((prep_root / "gold-draft.json").read_text())
    draft.update({
        "provenance": "human",
        "category": "quiet_two_person",
        "annotator": "anon-1",
        "reviewed_at": "2026-11-13T10:00:00+08:00",
        "scored_regions": [{"start_ms": 0, "end_ms": 900}],
        "transcript": [
            {"id": "g1", "text": "预算十五分钟", "speaker": "A", "start_ms": 0, "end_ms": 500},
            {"id": "g2", "text": "收到", "speaker": "B", "start_ms": 500, "end_ms": 900},
        ],
        "acoustic_events": [{"id": "e1", "label": "speech", "start_ms": 0, "end_ms": 500}],
        "queries": [
            {"id": "q1", "query": "预算是多久", "expected_sufficiency": "sufficient",
             "evidence_completeness": "complete",
             "required_spans": [{"sample_id": "s1", "start_ms": 0, "end_ms": 500}],
             "scope_sample_ids": ["s1"]},
            {"id": "q2", "query": "谁负责财务", "expected_sufficiency": "insufficient",
             "evidence_completeness": "none", "required_spans": [], "scope_sample_ids": ["s1"]},
        ],
        "claims": [{"id": "c1", "kind": "decision", "text": "预算定为十五分钟",
                    "source_spans": [{"start_ms": 0, "end_ms": 500}]}],
    })
    draft.update(overrides)
    return draft


class PrepareTests(unittest.TestCase):
    def test_prepare_writes_private_artifacts_and_an_unsigned_draft(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = prepare_args(Path(tmp))
            result = review.prepare(args)
            root = Path(result["root"])
            self.assertEqual(stat_mode(root), 0o700)
            self.assertEqual(stat_mode(root / "gold-draft.json"), 0o600)
            self.assertEqual(stat_mode(root / "annotate.html"), 0o600)
            self.assertEqual(stat_mode(root / "audio.wav"), 0o600)
            self.assertEqual(result["audio_duration_ms"], 1000)
            draft = json.loads((root / "gold-draft.json").read_text())
            self.assertEqual(draft["provenance"], "draft")
            self.assertEqual(draft["annotator"], "")
            self.assertEqual(draft["reviewed_at"], "")
            self.assertEqual(draft["category"], "")
            self.assertEqual(draft["audio_file"], "audio.wav")
            page = (root / "annotate.html").read_text()
            self.assertIn('src="audio.wav"', page)
            self.assertNotIn("http://", page)
            self.assertNotIn("https://", page)

    def test_prepare_refuses_an_existing_target(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "prep"
            root.mkdir()
            with self.assertRaises(review.ReviewError) as caught:
                review.prepare(prepare_args(Path(tmp)))
            self.assertEqual(caught.exception.code, "output_exists")
            self.assertEqual(list(root.iterdir()), [])

    def test_prepare_requires_an_explicit_existing_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = prepare_args(Path(tmp))
            args.audio = str(Path(tmp) / "absent.wav")
            with self.assertRaises(review.ReviewError) as caught:
                review.prepare(args)
            self.assertEqual(caught.exception.code, "audio_unusable")
            self.assertFalse((Path(tmp) / "prep").exists())


def stat_mode(path: Path) -> int:
    return os.stat(path).st_mode & 0o777


class FinalizeTests(unittest.TestCase):
    def prepare(self, tmp: Path, **overrides) -> Path:
        result = review.prepare(prepare_args(tmp, **overrides))
        return Path(result["root"])

    def test_draft_cannot_be_finalized(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize(Namespace(draft=str(root / "gold-draft.json"),
                                          authorization_reference="operator-ok",
                                          out=str(Path(tmp) / "final" / "manifest.json")))
            self.assertEqual(caught.exception.code, "draft_not_reviewed")
            self.assertFalse((Path(tmp) / "final").exists())

    def test_reviewed_draft_becomes_a_frozen_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            reviewed = reviewed_draft(root)
            reviewed_path = root / "gold-reviewed.json"
            reviewed_path.write_text(json.dumps(reviewed), encoding="utf-8")
            result = review.finalize(Namespace(draft=str(reviewed_path),
                                               authorization_reference="operator-ok",
                                               out=str(Path(tmp) / "final" / "manifest.json")))
            manifest_path = Path(result["manifest"])
            manifest = acceptance.load_manifest(manifest_path)
            sample = manifest["samples"][0]
            self.assertEqual(sample["category"], "quiet_two_person")
            self.assertEqual(sample["audio_sha256"], reviewed["audio_sha256"])
            gold = acceptance.load_gold(Path(sample["_gold_path"]), "s1", {"s1"})
            self.assertEqual(gold["provenance"], "human")
            self.assertEqual(gold["annotator"], "anon-1")
            self.assertEqual(len(gold["transcript"]), 2)
            self.assertEqual(stat_mode(manifest_path), 0o600)
            self.assertEqual(stat_mode(Path(sample["_gold_path"])), 0o600)
            # The prepare directory keeps its audio; finalize copies.
            self.assertTrue((root / "audio.wav").is_file())

    def test_page_export_without_audio_file_finalizes_from_its_own_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            reviewed = reviewed_draft(root)
            reviewed.pop("audio_file")
            reviewed["annotator"] = "anon-1"
            path = root / "gold-reviewed.json"
            path.write_text(json.dumps(reviewed), encoding="utf-8")
            result = review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                               out=str(Path(tmp) / "final" / "manifest.json")))
            self.assertEqual(result["status"], "FINALIZED")
            manifest = acceptance.load_manifest(Path(result["manifest"]))
            self.assertEqual(manifest["samples"][0]["audio_sha256"], reviewed["audio_sha256"])

    def test_audio_file_escape_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            reviewed = reviewed_draft(root, audio_file="../elsewhere.wav")
            path = root / "gold-reviewed.json"
            path.write_text(json.dumps(reviewed), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                          out=str(Path(tmp) / "final" / "manifest.json")))
            self.assertEqual(caught.exception.code, "audio_unusable")

    def test_unsigned_or_incomplete_review_is_refused(self):
        cases = {
            "empty annotator": {"annotator": ""},
            "no timezone": {"reviewed_at": "2026-11-13T10:00:00"},
            "no category": {"category": ""},
            "no regions": {"scored_regions": []},
            "region past the recording": {"scored_regions": [{"start_ms": 0, "end_ms": 5000}]},
        }
        for label, overrides in cases.items():
            with self.subTest(label), tempfile.TemporaryDirectory() as tmp:
                root = self.prepare(Path(tmp))
                path = root / "gold-reviewed.json"
                path.write_text(json.dumps(reviewed_draft(root, **overrides)), encoding="utf-8")
                with self.assertRaises(review.ReviewError):
                    review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                              out=str(Path(tmp) / "final" / "manifest.json")))
                self.assertFalse((Path(tmp) / "final").exists())

    def test_duplicate_identifiers_and_empty_claim_ids_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            bad = reviewed_draft(root, transcript=[
                {"id": "g1", "text": "甲", "speaker": "A", "start_ms": 0, "end_ms": 200},
                {"id": "g1", "text": "乙", "speaker": "B", "start_ms": 200, "end_ms": 400},
            ])
            path = root / "gold-reviewed.json"
            path.write_text(json.dumps(bad), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                          out=str(Path(tmp) / "final" / "manifest.json")))
            self.assertEqual(caught.exception.code, "invalid_gold")

    def test_manifest_path_escape_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            path = root / "gold-reviewed.json"
            path.write_text(json.dumps(reviewed_draft(root)), encoding="utf-8")
            # A reviewed draft that points its audio outside the new root is
            # rejected before any manifest is written.
            escaped = reviewed_draft(root, audio_file=str(Path(tmp) / "elsewhere.wav"))
            path.write_text(json.dumps(escaped), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                          out=str(Path(tmp) / "final" / "manifest.json")))
            self.assertEqual(caught.exception.code, "audio_unusable")

    def test_audio_hash_mismatch_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = self.prepare(Path(tmp))
            path = root / "gold-reviewed.json"
            path.write_text(json.dumps(reviewed_draft(root, audio_sha256="0" * 64)), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                          out=str(Path(tmp) / "final" / "manifest.json")))
            self.assertEqual(caught.exception.code, "hash_mismatch")


def build_bundle(root: Path, gold_hash: str, audio_hash: str, **overrides) -> dict:
    bundle = {
        "schema": review.PREDICTIONS_SCHEMA,
        "run_id": "run-01",
        "audio_sha256": audio_hash,
        "gold_sha256": gold_hash,
        "config_sha256": "1" * 64,
        "binary_sha256": "2" * 64,
        "model": {"provider": "fixture-asr", "revision": "synthetic-test"},
        "samples": [{
            "sample_id": "s1",
            "conversation_id": "conv-1",
            "recording_id": "rec-1",
            "transcript_revision": "rev-1",
            "turns": [
                {"turn_id": "t1", "speaker": "x", "start_ms": 0, "end_ms": 500, "text": "预算十五分钟"},
                {"turn_id": "t2", "speaker": "y", "start_ms": 500, "end_ms": 900, "text": "收到"},
            ],
            "acoustic_events": [{"label": "speech", "start_ms": 0, "end_ms": 500}],
            "diarization": [{"start_ms": 0, "end_ms": 500, "speaker": "x"},
                            {"start_ms": 500, "end_ms": 900, "speaker": "y"}],
        }],
        "query_outputs": [{
            "query_id": "q1",
            "ambient_ids": [],
            "forgotten_ids": [],
            "web": {"refs": [{"ref_id": "w1", "conversation_id": "conv-1", "recording_id": "rec-1",
                              "transcript_revision": "rev-1", "turn_id": "t1",
                              "start_ms": 0, "end_ms": 500}], "sufficiency": "sufficient"},
            "mcp": {"refs": [{"ref_id": "m1", "conversation_id": "conv-1", "recording_id": "rec-1",
                              "transcript_revision": "rev-1", "turn_id": "t1",
                              "start_ms": 0, "end_ms": 500},
                             {"ref_id": "m2", "conversation_id": "conv-other", "turn_id": "t9",
                              "start_ms": 0, "end_ms": 500}], "sufficiency": "sufficient"},
        }],
        "claims": [{"claim_id": "p1", "kind": "decision", "text": "预算十五分钟"}],
        "stages": {"complete": True},
        "resources": {"cpu_percent_peak": 1.0},
    }
    bundle.update(overrides)
    return bundle


class ScoreTests(unittest.TestCase):
    def frozen(self, tmp: Path) -> tuple[Path, dict]:
        result = review.prepare(prepare_args(tmp))
        root = Path(result["root"])
        reviewed = reviewed_draft(root)
        path = root / "gold-reviewed.json"
        path.write_text(json.dumps(reviewed), encoding="utf-8")
        final = review.finalize(Namespace(draft=str(path), authorization_reference="operator-ok",
                                          out=str(tmp / "final" / "manifest.json")))
        manifest = acceptance.load_manifest(Path(final["manifest"]))
        sample = manifest["samples"][0]
        return Path(final["manifest"]), {"audio": sample["audio_sha256"], "gold": sample["gold_sha256"]}

    def test_score_reports_structure_and_the_cross_scope_ref(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            result = review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                            adjudications="", out=str(Path(tmp) / "score-01"),
                                            audio_python="", audio_analysis_config=""))
            web = next(item for item in result["queries"] if item["query_id"] == "q1")["web"]
            mcp = next(item for item in result["queries"] if item["query_id"] == "q1")["mcp"]
            self.assertEqual(web["accuracy"], 1.0)
            # One correct ref plus one out-of-scope ref: 0.5, not null.
            self.assertEqual(mcp["accuracy"], 0.5)
            self.assertEqual(mcp["error_counts"], {"out_of_scope": 1})
            self.assertEqual(web["sufficiency_matches"], True)
            self.assertTrue(result["cross_recording_eer"] is False)
            self.assertEqual(result["claims"]["status"], review.metrics.STATUS_NOT_RUN)
            sample = result["samples"][0]
            self.assertEqual(sample["transcript"]["status"], "OK")
            self.assertEqual(sample["transcript"]["cer"]["value"], 0.0)
            self.assertEqual(sample["acoustic_events"]["tp"], 1)
            self.assertEqual(sample["der"]["status"], "OK")
            self.assertEqual(sample["der"]["pyannote"]["status"], "BLOCKED")

    def test_partial_adjudication_keeps_claim_precision_unknown(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"],
                                  claims=[{"claim_id": "p1", "kind": "decision", "text": "预算十五分钟"},
                                          {"claim_id": "p2", "kind": "commitment", "text": "明天提交"}])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            adjudications = {
                "schema": review.ADJUDICATIONS_SCHEMA,
                "prediction_digest": review.prediction_digest(bundle),
                "annotator": "anon-1",
                "reviewed_at": "2026-11-13T11:00:00Z",
                "claims": [{"claim_id": "p1", "accepted": True}],
                "evidence": [{"query_id": "q1", "ref_id": "w1", "accepted": True}],
            }
            adjudication_path = Path(tmp) / "adjudications.json"
            adjudication_path.write_text(json.dumps(adjudications), encoding="utf-8")
            result = review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                            adjudications=str(adjudication_path),
                                            out=str(Path(tmp) / "score-01"),
                                            audio_python="", audio_analysis_config=""))
            claims = result["claims"]
            self.assertIsNone(claims["precision"])
            self.assertEqual(claims["status"], review.metrics.STATUS_NOT_RUN)
            self.assertEqual(claims["by_kind"]["decision"]["precision"], 1.0)
            self.assertEqual(claims["by_kind"]["commitment"]["unadjudicated"], 1)
            judged = next(item for item in result["queries"] if item["query_id"] == "q1")["web"]
            self.assertEqual(judged["semantic_status"], review.metrics.STATUS_OK)
            self.assertEqual(judged["semantic_accuracy"], 1.0)

    def test_adjudication_digest_mismatch_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            adjudication_path = Path(tmp) / "adjudications.json"
            adjudication_path.write_text(json.dumps({
                "schema": review.ADJUDICATIONS_SCHEMA, "prediction_digest": "3" * 64,
                "annotator": "anon-1", "reviewed_at": "2026-11-13T11:00:00Z",
                "claims": [], "evidence": []}), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                       adjudications=str(adjudication_path),
                                       out=str(Path(tmp) / "score-01"),
                                       audio_python="", audio_analysis_config=""))
            self.assertEqual(caught.exception.code, "digest_mismatch")
            self.assertFalse((Path(tmp) / "score-01").exists())

    def test_unknown_adjudication_ids_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            bad = [{"claim_id": "ghost", "accepted": True},
                   {"claim_id": "p1", "accepted": True}, {"claim_id": "p1", "accepted": False}]
            for items in ([bad[0]], bad[1:]):
                adjudication_path = Path(tmp) / "adjudications.json"
                adjudication_path.write_text(json.dumps({
                    "schema": review.ADJUDICATIONS_SCHEMA,
                    "prediction_digest": review.prediction_digest(bundle),
                    "annotator": "anon-1", "reviewed_at": "2026-11-13T11:00:00Z",
                    "claims": items, "evidence": []}), encoding="utf-8")
                with self.assertRaises(review.ReviewError) as caught:
                    review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                           adjudications=str(adjudication_path),
                                           out=str(Path(tmp) / "score-01"),
                                           audio_python="", audio_analysis_config=""))
                self.assertEqual(caught.exception.code, "invalid_adjudications")

    def test_bundle_hash_mismatch_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), "4" * 64, hashes["audio"])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                       adjudications="", out=str(Path(tmp) / "score-01"),
                                       audio_python="", audio_analysis_config=""))
            self.assertEqual(caught.exception.code, "hash_mismatch")

    def test_empty_reference_and_empty_prediction_stay_distinct(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"])
            bundle["samples"][0]["turns"] = []
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            result = review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                            adjudications="", out=str(Path(tmp) / "score-01"),
                                            audio_python="", audio_analysis_config=""))
            transcript = result["samples"][0]["transcript"]
            # Non-empty reference with no prediction is the full deletion error.
            self.assertEqual(transcript["cer"]["value"], 1.0)
            self.assertEqual(transcript["wer"]["value"], 1.0)

    def test_score_touches_no_model_or_service(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(build_bundle(Path(tmp), hashes["gold"], hashes["audio"])),
                                   encoding="utf-8")

            def explode(*_args, **_kwargs):
                raise AssertionError("scoring must not call a model, service or subprocess")

            original = review.subprocess.run
            review.subprocess.run = explode
            try:
                result = review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                                adjudications="", out=str(Path(tmp) / "score-01"),
                                                audio_python="", audio_analysis_config=""))
            finally:
                review.subprocess.run = original
            self.assertEqual(result["note"],
                             "offline re-scoring only: no ASR or generation service is called; "
                             "a configured voiceprint model is loaded locally")

    def test_one_broken_sample_does_not_block_another(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = self.frozen(Path(tmp))
            manifest = json.loads(manifest_path.read_text())
            broken = json.loads(json.dumps(manifest["samples"][0]))
            broken["id"] = "s2"
            broken["audio_path"] = "audio-missing.wav"
            manifest["samples"].append(broken)
            manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            result = review.score(Namespace(manifest=str(manifest_path), predictions=str(bundle_path),
                                            adjudications="", out=str(Path(tmp) / "score-01"),
                                            audio_python="", audio_analysis_config=""))
            self.assertEqual(result["excluded_samples"], ["s2"])
            self.assertEqual(len(result["samples"]), 1)
            self.assertEqual(result["samples"][0]["sample_id"], "s1")


class SpeakerTrialTests(unittest.TestCase):
    def test_missing_model_configuration_is_blocked_not_passed(self):
        result = review.speaker_trial_scores(Path("unused.json"), {}, "")
        self.assertEqual(result["status"], "BLOCKED")
        blocked = review.speaker_trial_scores(Path("unused.json"), {}, "/nonexistent/audio.json")
        self.assertEqual(blocked["status"], "BLOCKED")

    def test_eer_coverage_requires_two_speakers_and_negatives(self):
        one_speaker = {"s1": {"speaker_trials": [
            {"id": "t1", "split": "evaluation", "same_speaker": True,
             "left": {"sample_id": "s1", "speaker": "A", "start_ms": 0, "end_ms": 500},
             "right": {"sample_id": "s1", "speaker": "A", "start_ms": 500, "end_ms": 900}}]}}
        coverage = review.eer_coverage(one_speaker)
        self.assertEqual(coverage["status"], "NOT_EVALUABLE")
        self.assertTrue(any("two speakers" in item for item in coverage["deficiencies"]))


class FinalizeSetTests(unittest.TestCase):
    """A reviewed set is frozen only when every cross-sample rule holds."""

    def speaker_trials(self) -> list[dict]:
        return [
            {"id": "t-same-a", "split": "evaluation", "same_speaker": True,
             "left": {"sample_id": "s-quiet", "speaker": "A", "start_ms": 0, "end_ms": 400},
             "right": {"sample_id": "s-far", "speaker": "A", "start_ms": 0, "end_ms": 400}},
            {"id": "t-same-b", "split": "evaluation", "same_speaker": True,
             "left": {"sample_id": "s-quiet", "speaker": "B", "start_ms": 500, "end_ms": 900},
             "right": {"sample_id": "s-far", "speaker": "B", "start_ms": 500, "end_ms": 900}},
            {"id": "t-diff-a", "split": "evaluation", "same_speaker": False,
             "left": {"sample_id": "s-quiet", "speaker": "A", "start_ms": 0, "end_ms": 400},
             "right": {"sample_id": "s-far", "speaker": "B", "start_ms": 500, "end_ms": 900}},
            {"id": "t-diff-b", "split": "evaluation", "same_speaker": False,
             "left": {"sample_id": "s-quiet", "speaker": "B", "start_ms": 500, "end_ms": 900},
             "right": {"sample_id": "s-far", "speaker": "A", "start_ms": 0, "end_ms": 400}},
        ]

    def prepared(self, tmp: Path, sample_id: str, category: str, trials=None, **overrides) -> Path:
        audio = tmp / f"{sample_id}-source.wav"
        write_wav(audio)
        result = review.prepare(Namespace(audio=str(audio), sample_id=sample_id, out=str(tmp / sample_id),
                                          reference="", duration_ms=0))
        root = Path(result["root"])
        queries = [
            {"id": f"{sample_id}:q1", "query": "预算是多久", "expected_sufficiency": "sufficient",
             "evidence_completeness": "complete",
             "required_spans": [{"sample_id": sample_id, "start_ms": 0, "end_ms": 500}],
             "scope_sample_ids": [sample_id]},
            {"id": f"{sample_id}:q2", "query": "谁负责财务", "expected_sufficiency": "insufficient",
             "evidence_completeness": "none", "required_spans": [], "scope_sample_ids": [sample_id]},
        ]
        draft = reviewed_draft(root, category=category, sample_id=sample_id, queries=queries,
                               claims=[{"id": f"{sample_id}:c1", "kind": "decision",
                                        "text": "预算定为十五分钟",
                                        "source_spans": [{"start_ms": 0, "end_ms": 500}]}],
                               speaker_trials=(self.speaker_trials() if sample_id == "s-quiet" else [])
                               if trials is None else trials,
                               **overrides)
        path = root / "gold-reviewed.json"
        path.write_text(json.dumps(draft), encoding="utf-8")
        return path

    def set_document(self, tmp: Path, entries: list[tuple[Path, str]]) -> Path:
        path = tmp / "reviewed-set.json"
        path.write_text(json.dumps({
            "schema": review.REVIEWED_SET_SCHEMA,
            "samples": [{"draft": str(draft), "authorization_reference": reference}
                        for draft, reference in entries],
        }), encoding="utf-8")
        return path

    def four(self, tmp: Path, **overrides) -> list[tuple[Path, str]]:
        entries = []
        for sample_id, category in (("s-quiet", "quiet_two_person"), ("s-far", "far_field_meeting"),
                                    ("s-over", "overlapping_speech"), ("s-media", "media_interference")):
            entries.append((self.prepared(tmp, sample_id, category, **overrides), "operator-authorized-2026-11"))
        return entries

    def test_a_complete_set_becomes_one_frozen_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            set_path = self.set_document(root, self.four(root))
            result = review.finalize_set(Namespace(drafts=str(set_path),
                                                   out=str(root / "frozen" / "manifest.json")))
            manifest = acceptance.load_manifest(Path(result["manifest"]))
            self.assertEqual(sorted(sample["category"] for sample in manifest["samples"]),
                             sorted(acceptance.CATEGORIES))
            for sample in manifest["samples"]:
                self.assertTrue(Path(sample["_audio_path"]).is_file())
                self.assertTrue(Path(sample["_gold_path"]).is_file())
                self.assertEqual(Path(sample["_audio_path"]).parent.name, sample["id"])
            self.assertEqual(stat_mode(Path(result["manifest"])), 0o600)

    def test_a_duplicate_global_query_id_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = self.four(root)
            # The same query id in two samples would merge two gold rows into one
            # prediction id, so the set must be refused.
            draft = json.loads(entries[1][0].read_text())
            draft["queries"] = json.loads(entries[0][0].read_text())["queries"]
            entries[1][0].write_text(json.dumps(draft), encoding="utf-8")
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))
            self.assertEqual(caught.exception.code, "duplicate_id")
            self.assertFalse((root / "frozen").exists())

    def test_a_missing_category_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = self.four(root)[:3]
            with self.assertRaisesRegex(review.ReviewError, "category"):
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))

    def test_a_path_like_sample_id_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = self.four(root)
            draft = json.loads(entries[0][0].read_text())
            draft["sample_id"] = "../escape"
            entries[0][0].write_text(json.dumps(draft), encoding="utf-8")
            with self.assertRaisesRegex(review.ReviewError, "sample_id"):
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))

    def test_a_symlinked_audio_source_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = self.four(root)
            target = entries[0][0].parent / "audio.wav"
            real = entries[0][0].parent / "audio-real.wav"
            target.rename(real)
            target.symlink_to(real)
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))
            self.assertEqual(caught.exception.code, "audio_unusable")

    def test_an_existing_target_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = self.four(root)
            (root / "frozen").mkdir()
            with self.assertRaises(review.ReviewError) as caught:
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))
            self.assertEqual(caught.exception.code, "output_exists")
            self.assertEqual(list((root / "frozen").iterdir()), [])

    def test_a_reference_outside_the_set_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = self.four(root)
            draft = json.loads(entries[0][0].read_text())
            draft["speaker_trials"][0]["right"]["sample_id"] = "s-outside"
            entries[0][0].write_text(json.dumps(draft), encoding="utf-8")
            with self.assertRaisesRegex(review.ReviewError, "unauthorized sample"):
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))

    def test_a_set_without_speaker_evaluation_coverage_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            entries = []
            for sample_id, category in (("s-quiet", "quiet_two_person"), ("s-far", "far_field_meeting"),
                                        ("s-over", "overlapping_speech"), ("s-media", "media_interference")):
                entries.append((self.prepared(root, sample_id, category, trials=[]), "operator-authorized-2026-11"))
            with self.assertRaisesRegex(review.ReviewError, "speaker evaluation"):
                review.finalize_set(Namespace(drafts=str(self.set_document(root, entries)),
                                              out=str(root / "frozen" / "manifest.json")))


class AdjudicatePrepareTests(unittest.TestCase):
    def bundle(self) -> dict:
        return build_bundle(Path("/tmp"), "a" * 64, "b" * 64)

    def test_a_draft_keeps_every_verdict_empty(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle_path = Path(tmp) / "predictions.json"
            bundle = self.bundle()
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            out = Path(tmp) / "adjudication"
            result = review.adjudicate_prepare(Namespace(predictions=str(bundle_path), out=str(out)))
            self.assertEqual(result["status"], "PREPARED")
            self.assertEqual(stat_mode(out), 0o700)
            draft = json.loads((out / "adjudications-draft.json").read_text())
            self.assertEqual(draft["schema"], review.ADJUDICATIONS_SCHEMA)
            self.assertEqual(draft["annotator"], "")
            self.assertEqual(draft["reviewed_at"], "")
            self.assertTrue(all(item["accepted"] is None for item in draft["claims"]))
            self.assertTrue(all(item["accepted"] is None for item in draft["evidence"]))
            self.assertEqual([item["ref_id"] for item in draft["evidence"]], ["w1", "m1", "m2"])
            self.assertEqual(draft["prediction_digest"], review.prediction_digest(bundle))
            page = json.loads((out / "review.json").read_text())
            self.assertEqual(page["claims"][0]["text"], "预算十五分钟")
            self.assertEqual(page["queries"][0]["web"][0]["ref_id"], "w1")
            with self.assertRaises(review.ReviewError):
                review.validate_adjudications(draft, bundle, {"p1"}, {"q1": {"w1", "m1", "m2"}})

    def test_an_existing_output_directory_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(self.bundle()), encoding="utf-8")
            out = Path(tmp) / "adjudication"
            out.mkdir()
            with self.assertRaises(review.ReviewError) as caught:
                review.adjudicate_prepare(Namespace(predictions=str(bundle_path), out=str(out)))
            self.assertEqual(caught.exception.code, "output_exists")


class DiarizationScoreTests(unittest.TestCase):
    def frozen(self, tmp: Path) -> tuple[Path, dict]:
        return ScoreTests().frozen(tmp)

    def score(self, tmp: Path, bundle: dict) -> dict:
        manifest_path, hashes = self.frozen(tmp)
        bundle["audio_sha256"], bundle["gold_sha256"] = hashes["audio"], hashes["gold"]
        path = tmp / "predictions.json"
        path.write_text(json.dumps(bundle), encoding="utf-8")
        return review.score(Namespace(manifest=str(manifest_path), predictions=str(path),
                                      adjudications="", out=str(tmp / "score-dia"),
                                      audio_python="", audio_analysis_config=""))

    def test_der_uses_the_diarization_field(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle = build_bundle(Path(tmp), "", "")
            bundle["samples"][0]["diarization"] = [
                {"start_ms": 0, "end_ms": 500, "speaker": "x"},
                {"start_ms": 500, "end_ms": 900, "speaker": "y"},
            ]
            result = self.score(Path(tmp), bundle)
            der = result["samples"][0]["der"]
            self.assertEqual(der["status"], "OK")
            self.assertEqual(der["value"], 0.0)

    def test_a_missing_diarization_field_is_not_replaced_by_asr_labels(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle = build_bundle(Path(tmp), "", "")
            bundle["samples"][0].pop("diarization", None)
            result = self.score(Path(tmp), bundle)
            der = result["samples"][0]["der"]
            self.assertEqual(der["status"], "NOT_RUN")
            self.assertIsNone(der["value"])

    def test_an_empty_successful_diarization_counts_every_miss(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle = build_bundle(Path(tmp), "", "")
            bundle["samples"][0]["diarization"] = []
            bundle["samples"][0]["audio_capabilities"] = {"diarization": "succeeded"}
            result = self.score(Path(tmp), bundle)
            der = result["samples"][0]["der"]
            self.assertEqual(der["status"], "OK")
            self.assertEqual(der["value"], 1.0)
            self.assertEqual(der["reference_speech_ms"], 900)

    def test_a_failed_diarization_capability_is_not_an_empty_result(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle = build_bundle(Path(tmp), "", "")
            bundle["samples"][0]["diarization"] = []
            bundle["samples"][0]["audio_capabilities"] = {"diarization": "failed"}
            result = self.score(Path(tmp), bundle)
            der = result["samples"][0]["der"]
            self.assertEqual(der["status"], "NOT_RUN")
            self.assertIn("failed", der["reason"])


def gold_with_trials(trials: list[dict], category: str = "quiet_two_person") -> dict:
    return {"category": category, "speaker_trials": trials}


def trial(trial_id: str, same: bool, split: str = "evaluation") -> dict:
    return {"id": trial_id, "split": split, "same_speaker": same,
            "left": {"sample_id": "s1", "speaker": "A", "start_ms": 0, "end_ms": 400},
            "right": {"sample_id": "s2", "speaker": "A" if same else "B", "start_ms": 0, "end_ms": 400}}


class ScoreEERTests(unittest.TestCase):
    def scores(self, values: dict[str, float], status: str = "OK") -> dict:
        return {"status": status, "trials": {trial_id: {"status": "OK", "score": value}
                                             for trial_id, value in values.items()}}

    def test_separated_evaluation_scores_give_a_zero_eer(self):
        gold = {"s1": gold_with_trials([trial("t1", True), trial("t2", False)])}
        result = review.score_eer(gold, self.scores({"t1": 0.9, "t2": 0.1}))
        self.assertEqual(result["status"], "OK")
        self.assertEqual(result["value"], 0.0)
        self.assertEqual(result["per_category"]["quiet_two_person"]["same_trials"], 1)

    def test_overlapping_scores_give_a_nonzero_eer(self):
        gold = {"s1": gold_with_trials([trial("t1", True), trial("t2", False)])}
        result = review.score_eer(gold, self.scores({"t1": 0.2, "t2": 0.8}))
        self.assertEqual(result["status"], "OK")
        self.assertGreater(result["value"], 0.0)

    def test_calibration_scores_do_not_change_the_result(self):
        trials = [trial("t1", True), trial("t2", False), trial("cal", True, split="calibration")]
        gold = {"s1": gold_with_trials(trials)}
        without = review.score_eer(gold, self.scores({"t1": 0.9, "t2": 0.1}))
        with_calibration = review.score_eer(gold, self.scores({"t1": 0.9, "t2": 0.1, "cal": 0.0}))
        self.assertEqual(without["value"], with_calibration["value"])
        self.assertEqual(with_calibration["same_trials"], 1)

    def test_an_unscored_evaluation_trial_is_not_evaluable(self):
        gold = {"s1": gold_with_trials([trial("t1", True), trial("t2", False)])}
        result = review.score_eer(gold, self.scores({"t1": 0.9}))
        self.assertEqual(result["status"], "NOT_EVALUABLE")
        self.assertIn("t2", result["reason"])

    def test_a_missing_model_is_blocked(self):
        gold = {"s1": gold_with_trials([trial("t1", True), trial("t2", False)])}
        result = review.score_eer(gold, {"status": "BLOCKED", "reason": "no model"})
        self.assertEqual(result["status"], "BLOCKED")
        self.assertIn("no model", result["reason"])

    def test_a_non_finite_score_is_refused(self):
        gold = {"s1": gold_with_trials([trial("t1", True), trial("t2", False)])}
        result = review.score_eer(gold, self.scores({"t1": 0.9, "t2": float("nan")}))
        self.assertEqual(result["status"], "NOT_EVALUABLE")
        self.assertIn("finite", result["reason"])


def thresholds_document(**overrides) -> dict:
    metrics = {"cer_max": 0.15, "wer_max": 0.30, "der_max": 0.20, "event_precision_min": 0.80,
               "event_recall_min": 0.80, "eer_max": 0.05, "evidence_accuracy_min": 0.95,
               "semantic_accuracy_min": 0.95, "decision_precision_min": 0.95}
    payload = {"schema": review.PRODUCTION_THRESHOLDS_SCHEMA, "approved": True,
               "approved_at": "2026-11-19T13:55:00+08:00", "approved_by": "operator",
               "categories": list(acceptance.CATEGORIES), "metrics": metrics}
    payload.update(overrides)
    return payload


def gate_fixture(categories=None, **overrides) -> tuple[dict, dict, dict]:
    """A manifest, bundle and score result that sit exactly on every boundary."""
    categories = list(categories or acceptance.CATEGORIES)
    samples = []
    queries = []
    claims = []
    judged = []
    for category in categories:
        sample_id = f"s-{category}"
        samples.append({
            "sample_id": sample_id, "category": category,
            "transcript": {"status": "OK",
                           "cer": {"status": "OK", "numerator": 15, "denominator": 100},
                           "wer": {"status": "OK", "numerator": 30, "denominator": 100}},
            "acoustic_events": {"status": "OK", "tp": 8, "fp": 2, "fn": 2},
            "der": {"status": "OK", "false_alarm_ms": 100.0, "missed_ms": 60.0,
                    "confusion_ms": 40.0, "reference_speech_ms": 1000.0},
        })
        queries.append({
            "query_id": f"{sample_id}:q1", "sample_id": sample_id, "category": category,
            "web": {"status": "OK", "supported_refs": 19, "returned_refs": 20,
                    "semantic_accepted": 19, "semantic_adjudicated": 20},
            "mcp": {"status": "OK", "supported_refs": 19, "returned_refs": 20,
                    "semantic_accepted": 19, "semantic_adjudicated": 20},
        })
        claim_id = f"{sample_id}:cand"
        claims.append({"claim_id": claim_id, "kind": "decision", "sample_id": sample_id})
        judged.append({"claim_id": claim_id, "accepted": True})
    eer = {"status": "OK", "value": 0.05, "same_trials": 30 * len(categories),
           "different_trials": 30 * len(categories), "experimental": False,
           "per_category": {category: {"status": "OK", "value": 0.05, "same_trials": 30,
                                       "different_trials": 30, "experimental": False}
                            for category in categories}}
    result = {
        "samples": samples, "queries": queries,
        "speaker": {"eer": eer},
        "adjudication": {"report": {"claims": judged, "evidence": [], "unjudged_claims": [],
                                    "gold_claims_without_prediction": []}},
        "sample_problems": {},
        "inputs": {"thresholds_sha256": "t" * 64, "manifest_sha256": "m" * 64,
                   "predictions_sha256": "p" * 64, "adjudications_sha256": "a" * 64},
    }
    bundle = {"claims": claims, "stages": {"complete": True}}
    manifest = {"samples": [{"id": f"s-{category}", "category": category} for category in categories]}
    result.update(overrides)
    return manifest, bundle, result


class ProductionGateTests(unittest.TestCase):
    def gate(self, fixture=None, thresholds=None) -> dict:
        manifest, bundle, result = fixture or gate_fixture()
        return review.evaluate_production_quality(manifest, bundle, result,
                                                  thresholds or thresholds_document())

    def test_boundary_values_pass(self):
        gate = self.gate()
        self.assertEqual(gate["status"], "PASS", gate["failures"] + gate["blocked"])
        self.assertEqual(gate["thresholds_sha256"], "t" * 64)
        quiet = gate["categories"]["quiet_two_person"]
        self.assertEqual(quiet["cer"], {"errors": 15, "reference": 100})
        self.assertEqual(quiet["der"]["reference_speech_ms"], 1000.0)
        self.assertEqual(quiet["decision"], {"accepted": 1, "total": 1, "adjudicated": 1})

    def test_one_low_category_fails_the_whole_gate(self):
        manifest, bundle, result = gate_fixture()
        result["samples"][0]["transcript"]["cer"]["numerator"] = 16
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "FAIL")
        self.assertTrue(any("quiet_two_person" in item and "cer" in item for item in gate["failures"]))

    def test_a_missing_category_blocks(self):
        manifest, bundle, result = gate_fixture(categories=acceptance.CATEGORIES[:3])
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")
        self.assertTrue(any("media_interference" in item for item in gate["blocked"]))

    def test_an_unadjudicated_decision_blocks(self):
        manifest, bundle, result = gate_fixture()
        result["adjudication"]["report"]["claims"] = []
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")
        self.assertTrue(any("not adjudicated" in item for item in gate["blocked"]))

    def test_experimental_eer_blocks(self):
        manifest, bundle, result = gate_fixture()
        result["speaker"]["eer"]["per_category"]["quiet_two_person"]["experimental"] = True
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")
        self.assertTrue(any("experimental" in item for item in gate["blocked"]))

    def test_a_zero_evidence_denominator_blocks(self):
        manifest, bundle, result = gate_fixture()
        result["queries"][0]["web"] = {"status": "OK", "supported_refs": 0, "returned_refs": 0,
                                       "semantic_accepted": 0, "semantic_adjudicated": 0}
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")
        self.assertTrue(any("returned no evidence" in item for item in gate["blocked"]))

    def test_missing_evidence_adjudications_block(self):
        manifest, bundle, result = gate_fixture()
        result["queries"][0]["mcp"]["semantic_adjudicated"] = 19
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")
        self.assertTrue(any("unadjudicated evidence" in item for item in gate["blocked"]))

    def test_incomplete_stages_block(self):
        manifest, bundle, result = gate_fixture()
        bundle["stages"]["complete"] = False
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")

    def test_der_without_a_pyannote_comparison_still_scores(self):
        manifest, bundle, result = gate_fixture()
        result["samples"][0]["der"] = {"status": "NOT_RUN", "value": None}
        gate = self.gate((manifest, bundle, result))
        self.assertEqual(gate["status"], "BLOCKED")
        self.assertTrue(any("DER" in item for item in gate["blocked"]))

    def test_invalid_thresholds_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "thresholds.json"
            for mutate in (lambda payload: payload.update(approved=False),
                           lambda payload: payload["metrics"].pop("eer_max"),
                           lambda payload: payload["metrics"].update(cer_max=float("nan")),
                           lambda payload: payload.update(categories=["quiet_two_person"]),
                           lambda payload: payload.update(approved_at="2026-11-19T13:55:00")):
                payload = thresholds_document()
                mutate(payload)
                path.write_text(json.dumps(payload), encoding="utf-8")
                with self.assertRaises(review.ReviewError) as caught:
                    review.load_production_thresholds(path)
                self.assertEqual(caught.exception.code, "invalid_thresholds")

    def test_the_cli_exits_nonzero_when_the_gate_is_not_pass(self):
        with tempfile.TemporaryDirectory() as tmp:
            manifest_path, hashes = ScoreTests().frozen(Path(tmp))
            bundle = build_bundle(Path(tmp), hashes["gold"], hashes["audio"])
            bundle_path = Path(tmp) / "predictions.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            thresholds_path = Path(tmp) / "thresholds.json"
            thresholds_path.write_text(json.dumps(thresholds_document()), encoding="utf-8")
            argv = ["review-audio-acceptance.py", "score", "--manifest", str(manifest_path),
                    "--predictions", str(bundle_path), "--out", str(Path(tmp) / "score-gate"),
                    "--production-thresholds", str(thresholds_path)]
            original = sys.argv
            sys.argv = argv
            try:
                code = review.main()
            finally:
                sys.argv = original
            self.assertEqual(code, 1)
            written = json.loads((Path(tmp) / "score-gate" / "score.json").read_text())
            gate = written["production_gate"]
            self.assertNotEqual(gate["status"], "PASS")
            # The bundle returns an out-of-scope ref, so evidence accuracy is a
            # measured failure rather than a missing input.
            self.assertEqual(gate["status"], "FAIL")


if __name__ == "__main__":
    unittest.main()
