#!/usr/bin/env python3
"""Tests for the v0.4 acceptance manifest rules and the real-run preflight.

No audio is opened beyond the synthetic fixture written into a temp directory,
no container is started and no model is loaded: every case here stops at
validation, before the stack would start.
"""
from __future__ import annotations

import hashlib
import importlib.util
import json
import re
import sys
import tempfile
import unittest
from argparse import Namespace
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import audio_acceptance as acceptance  # noqa: E402

SPEC = importlib.util.spec_from_file_location("long_audio_reliability",
                                              ROOT / "scripts/long-audio-reliability.py")
runner = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
sys.modules["long_audio_reliability"] = runner
SPEC.loader.exec_module(runner)


def write_manifest(root: Path, samples: list[dict]) -> Path:
    path = root / "acceptance.json"
    path.write_text(json.dumps({"schema": acceptance.MANIFEST_SCHEMA, "samples": samples}), encoding="utf-8")
    return path


def gold_document(sample_id: str) -> dict:
    return {
        "schema": acceptance.GOLD_SCHEMA, "provenance": "human", "sample_id": sample_id,
        "annotator": "anon-1", "reviewed_at": "2026-11-01T00:00:00Z",
        "audio_duration_ms": 2000,
        "scored_regions": [{"start_ms": 0, "end_ms": 1000}],
        "transcript": [{"id": "g1", "text": "合成测试", "speaker": "A", "start_ms": 0, "end_ms": 500}],
        "acoustic_events": [{"id": "e1", "label": "speech", "start_ms": 0, "end_ms": 500}],
        "speaker_trials": [{
            "id": "t1", "split": "evaluation", "same_speaker": True,
            "left": {"sample_id": sample_id, "speaker": "A", "start_ms": 0, "end_ms": 500},
            "right": {"sample_id": sample_id, "speaker": "A", "start_ms": 600, "end_ms": 1000},
        }],
    }


def sample(root: Path, sample_id: str, category: str, audio: bytes = b"RIFF", authorized: bool = True,
           audio_path: str | None = None, gold: dict | None = None) -> dict:
    audio_file = root / "audio" / f"{sample_id}.wav"
    audio_file.parent.mkdir(parents=True, exist_ok=True)
    audio_file.write_bytes(audio)
    gold_file = root / "gold" / f"{sample_id}.json"
    gold_file.parent.mkdir(parents=True, exist_ok=True)
    gold_file.write_text(json.dumps(gold or gold_document(sample_id)), encoding="utf-8")
    return {
        "id": sample_id, "category": category,
        "audio_path": audio_path or str(audio_file),
        "audio_sha256": hashlib.sha256(audio_file.read_bytes()).hexdigest(),
        "authorization": {"allowed": authorized, "reference": "operator-authorized-2026-11"},
        "gold_path": str(gold_file),
        "gold_sha256": hashlib.sha256(gold_file.read_bytes()).hexdigest(),
    }


class NoStack:
    """Fail before any binary starts, so preflight tests stay hermetic."""

    def __init__(self, *_args, **_kwargs):
        raise RuntimeError("stack must not start in a preflight test")


def run_args(manifest_path: Path | None, tmp: Path, **overrides) -> Namespace:
    """Arguments that pass config validation but stop before any stack start."""
    values = {
        "acceptance_manifest": str(manifest_path) if manifest_path else "",
        "out": str(tmp / "out"),
        "password": "synthetic-password",
        "audio_analysis_config": "",
        "asr_backend": "qwen",
        "asr_python": sys.executable,
        "asr_model": "synthetic-model-name",
        "asr_revision": "synthetic-revision",
        "asr_device": "cpu",
        "generator_config": "",
        "sample_timeout": 5.0,
    }
    values.update(overrides)
    return Namespace(**values)


class RealRunPreflightTests(unittest.TestCase):
    def test_missing_manifest_reports_every_category_not_run(self):
        entries = acceptance.no_manifest_entries()
        self.assertEqual(len(entries), 4)
        for entry in entries:
            self.assertEqual(entry["status"], "NOT_RUN")
            self.assertIn("no authorized acceptance manifest", entry["reason"])

    def test_no_manifest_reads_no_audio_and_runs_no_go_tests(self):
        with tempfile.TemporaryDirectory() as tmp:
            entries = runner.v04_real_run(run_args(None, Path(tmp)), Path(tmp))
            self.assertEqual([entry["status"] for entry in entries], ["NOT_RUN"] * 4)
            self.assertFalse((Path(tmp) / "out").exists())

    def test_missing_trusted_asr_configuration_is_blocked_not_passed(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = write_manifest(root, [sample(root, "s1", "quiet_two_person")])
            entries = runner.v04_real_run(run_args(manifest, root, asr_python="", asr_revision=""), root)
            self.assertEqual(len(entries), 1)
            self.assertEqual(entries[0]["status"], "BLOCKED")
            self.assertIn("--asr-python", entries[0]["reason"])

    def test_test_stub_model_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = write_manifest(root, [sample(root, "s1", "quiet_two_person")])
            entries = runner.v04_real_run(run_args(manifest, root, asr_model="test-stub"), root)
            self.assertEqual(entries[0]["status"], "BLOCKED")
            self.assertIn("real local model", entries[0]["reason"])

    def test_valid_gold_is_no_longer_reported_as_not_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            # Without a runnable stack the preflight still resolves the sample
            # and reports the execution failure instead of a NOT_RUN placeholder.
            manifest = write_manifest(root, [sample(root, "s1", "quiet_two_person")])
            original = runner.Stack
            runner.Stack = NoStack
            try:
                entries = runner.v04_real_run(run_args(manifest, root), root)
            finally:
                runner.Stack = original
            self.assertEqual(entries[0]["id"], "s1")
            self.assertEqual(entries[0]["status"], "FAIL")
            self.assertIn("stack must not start", entries[0]["error"])

    def test_unauthorized_sample_is_rejected_before_audio(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = write_manifest(root, [sample(root, "s1", "quiet_two_person", authorized=False)])
            entries = runner.v04_real_run(run_args(manifest, root), root)
            self.assertEqual(entries[0]["id"], "v04.real.manifest")
            self.assertEqual(entries[0]["status"], "FAIL")
            self.assertIn("unauthorized_sample", entries[0]["reason"])

    def test_path_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "root"
            root.mkdir()
            outside = Path(tmp) / "outside.wav"
            outside.write_bytes(b"RIFF")
            manifest = write_manifest(root, [sample(root, "s1", "quiet_two_person", audio_path=str(outside))])
            entries = runner.v04_real_run(run_args(manifest, root), root)
            self.assertEqual(entries[0]["id"], "v04.real.manifest")
            self.assertIn("path_escape", entries[0]["reason"])

    def test_hash_mismatch_fails_and_missing_file_is_not_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            valid = sample(root, "s1", "quiet_two_person")
            tampered = dict(valid)
            tampered["audio_sha256"] = "0" * 64
            missing = sample(root, "s2", "far_field_meeting")
            missing["audio_path"] = str(root / "audio" / "absent.wav")
            manifest = write_manifest(root, [tampered, missing])
            entries = {entry["id"]: entry for entry in runner.v04_real_run(run_args(manifest, root), root)}
            self.assertEqual(entries["s1"]["status"], "FAIL")
            self.assertIn("hash", entries["s1"]["reason"])
            self.assertEqual(entries["s2"]["status"], "NOT_RUN")

    def test_missing_categories_are_reported_not_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = write_manifest(root, [sample(root, "s1", "overlapping_speech")])
            original = runner.Stack
            runner.Stack = NoStack
            try:
                entries = runner.v04_real_run(run_args(manifest, root), root)
            finally:
                runner.Stack = original
            by_category = {entry.get("category") or entry["id"]: entry for entry in entries}
            self.assertEqual(by_category["overlapping_speech"]["status"], "FAIL")
            for category in ("quiet_two_person", "far_field_meeting", "media_interference"):
                self.assertEqual(by_category[category]["status"], "NOT_RUN")
                self.assertIn("no authorized sample", by_category[category]["reason"])

    def test_one_broken_sample_does_not_block_another(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            broken = sample(root, "s1", "quiet_two_person")
            broken["audio_sha256"] = "0" * 64
            healthy = sample(root, "s2", "far_field_meeting")
            manifest = write_manifest(root, [broken, healthy])
            original = runner.Stack
            runner.Stack = NoStack
            try:
                entries = runner.v04_real_run(run_args(manifest, root), root)
            finally:
                runner.Stack = original
            self.assertEqual(entries[0]["id"], "s1")
            self.assertEqual(entries[0]["status"], "FAIL")
            # The second sample still reached execution (and fails only because
            # this test provides no runnable stack).
            self.assertEqual(entries[1]["id"], "s2")
            self.assertIn(entries[1]["status"], ("FAIL", "NOT_RUN", "BLOCKED"))
            self.assertNotEqual(entries[1].get("error") or entries[1].get("reason"), "")

    def test_gold_requires_human_provenance(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            gold = gold_document("s1")
            gold["provenance"] = "asr"
            record = sample(root, "s1", "quiet_two_person", gold=gold)
            manifest = write_manifest(root, [record])
            entries = {entry["id"]: entry for entry in runner.v04_real_run(run_args(manifest, root), root)}
            self.assertEqual(entries["s1"]["status"], "FAIL")
            self.assertIn("human", entries["s1"]["reason"])

    def test_invalid_manifest_schema_fails_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            manifest = root / "acceptance.json"
            manifest.write_text(json.dumps({"schema": "wrong", "samples": []}), encoding="utf-8")
            entries = runner.v04_real_run(run_args(manifest, root), root)
            self.assertEqual(len(entries), 1)
            self.assertEqual(entries[0]["status"], "FAIL")


class GoldContractTests(unittest.TestCase):
    def write_gold(self, root: Path, mutate) -> Path:
        payload = gold_document("s1")
        mutate(payload)
        path = root / "gold.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        return path

    def assert_rejected(self, mutate, fragment: str):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write_gold(Path(tmp), mutate)
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                acceptance.load_gold(path, "s1", {"s1"})
            self.assertIn(fragment, caught.exception.detail)

    def test_booleans_are_not_integers(self):
        self.assert_rejected(
            lambda payload: payload["scored_regions"].__setitem__(0, {"start_ms": True, "end_ms": 10}),
            "must be integers")

    def test_regions_must_not_overlap(self):
        self.assert_rejected(
            lambda payload: payload.update(scored_regions=[{"start_ms": 0, "end_ms": 800},
                                                           {"start_ms": 700, "end_ms": 900}]),
            "must not overlap")

    def test_regions_stay_inside_the_recording(self):
        self.assert_rejected(
            lambda payload: payload.update(scored_regions=[{"start_ms": 0, "end_ms": 2001}]),
            "after audio_duration_ms")

    def test_reviewed_at_needs_a_timezone(self):
        self.assert_rejected(lambda payload: payload.update(reviewed_at="2026-11-01T00:00:00"),
                            "timezone")

    def test_omitted_and_empty_arrays_are_different(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            omitted = self.write_gold(root, lambda payload: payload.pop("acoustic_events"))
            self.assertNotIn("acoustic_events", acceptance.load_gold(omitted, "s1", {"s1"}))
            empty = self.write_gold(root, lambda payload: payload.update(acoustic_events=[]))
            self.assertEqual(acceptance.load_gold(empty, "s1", {"s1"})["acoustic_events"], [])

    def test_duplicate_transcript_ids_are_rejected(self):
        self.assert_rejected(
            lambda payload: payload["transcript"].append(dict(payload["transcript"][0])),
            "duplicate id")

    def test_trials_must_reference_authorized_samples(self):
        self.assert_rejected(
            lambda payload: payload["speaker_trials"][0]["left"].update(sample_id="outside"),
            "unauthorized sample")

    def test_query_with_no_evidence_must_not_require_spans(self):
        def mutate(payload):
            payload["queries"] = [{"id": "q1", "query": "问题", "expected_sufficiency": "insufficient",
                                   "evidence_completeness": "none",
                                   "required_spans": [{"sample_id": "s1", "start_ms": 0, "end_ms": 10}]}]
        self.assert_rejected(mutate, "must not require spans")

    def test_complete_query_must_require_spans(self):
        def mutate(payload):
            payload["queries"] = [{"id": "q1", "query": "问题", "expected_sufficiency": "sufficient",
                                   "evidence_completeness": "complete", "required_spans": []}]
        self.assert_rejected(mutate, "must require spans")

    def test_claims_need_a_known_kind(self):
        def mutate(payload):
            payload["claims"] = [{"id": "c1", "kind": "opinion", "text": "x", "source_spans": []}]
        self.assert_rejected(mutate, "decision or commitment")


class GeneratorConfigTests(unittest.TestCase):
    def write(self, root: Path, payload: dict) -> str:
        path = root / "generator.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        return str(path)

    def test_off_generator_sends_no_flags(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write(Path(tmp), {"generator": "off"})
            args, document = runner.generator_config(path)
            self.assertEqual(args, [])
            self.assertEqual(document["generator"], "off")

    def test_unknown_fields_and_synthetic_are_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for payload in ({"generator": "off", "extra": 1}, {"generator": "synthetic"}):
                with self.assertRaises(ValueError):
                    runner.generator_config(self.write(root, payload))

    def test_openai_generator_requires_loopback_and_a_model(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            remote = self.write(root, {"generator": "openai", "generator-url": "https://example.com/v1",
                                       "generator-model": "m"})
            with self.assertRaisesRegex(ValueError, "loopback"):
                runner.generator_config(remote)
            loopback = self.write(root, {"generator": "openai", "generator-url": "http://127.0.0.1:9000/v1",
                                         "generator-model": "local-model", "generator-temperature": 0.1,
                                         "generator-key-env": "GENERATOR_API_KEY"})
            args, _document = runner.generator_config(loopback)
            self.assertIn("--generator-url", args)
            self.assertIn("http://127.0.0.1:9000/v1", args)

    def test_plaintext_key_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write(Path(tmp), {"generator": "openai", "generator-url": "http://127.0.0.1:9000/v1",
                                          "generator-model": "m", "generator-key-env": "sk-live-1234"})
            with self.assertRaisesRegex(ValueError, "environment variable name"):
                runner.generator_config(path)


class PagedTurnsClient:
    """A turns endpoint that serves exactly the pages it was given."""

    def __init__(self, pages: dict[int, dict]):
        self.pages = pages
        self.paths: list[str] = []

    def fetch(self, path: str) -> dict:
        self.paths.append(path)
        offset = int(re.search(r"offset=(\d+)", path).group(1))
        if offset not in self.pages:
            raise AssertionError(f"unexpected offset {offset}")
        return self.pages[offset]


def turn(turn_id: str, start: int, end: int) -> dict:
    return {"id": turn_id, "kind": "turn", "global_start_ms": start, "global_end_ms": end}


class CollectCurrentTurnsTests(unittest.TestCase):
    def test_every_turn_of_a_multi_page_recording_is_collected(self):
        first = [turn(f"t{index}", index * 1000, index * 1000 + 900) for index in range(200)]
        client = PagedTurnsClient({0: {"items": first, "next_offset": 200},
                                   200: {"items": [turn("t200", 200000, 200900)], "next_offset": None}})
        turns = runner.collect_current_turns(client, "conv-1", "rec-1")
        self.assertEqual(len(turns), 201)
        self.assertEqual(turns[-1]["id"], "t200")
        covered, count = runner.coverage_ms(client, "conv-1", "rec-1")
        self.assertEqual(count, 201)
        self.assertEqual(covered, 201 * 900)

    def test_a_repeated_turn_id_is_an_error(self):
        client = PagedTurnsClient({0: {"items": [turn("t1", 0, 100)], "next_offset": 1},
                                   1: {"items": [turn("t1", 100, 200)], "next_offset": None}})
        with self.assertRaisesRegex(RuntimeError, "duplicate turn id"):
            runner.collect_current_turns(client, "conv-1", "rec-1")

    def test_a_stalled_offset_is_an_error(self):
        client = PagedTurnsClient({0: {"items": [turn("t1", 0, 100)], "next_offset": 0}})
        with self.assertRaisesRegex(RuntimeError, "did not advance"):
            runner.collect_current_turns(client, "conv-1", "rec-1")

    def test_missing_span_turns_still_count_as_turns(self):
        client = PagedTurnsClient({0: {"items": [turn("t1", 0, 100),
                                                {"id": "t2", "kind": "turn"}], "next_offset": None}})
        covered, count = runner.coverage_ms(client, "conv-1", "rec-1")
        self.assertEqual(count, 1)
        self.assertEqual(covered, 100)


class MCPQueryOutputTests(unittest.TestCase):
    def test_probe_uses_the_stack_profile_root_and_environment(self):
        captured = {}

        class Proc:
            returncode = 0
            stdout = '[{"query_id": "q1", "result": {"items": []}}]\n'
            stderr = ""

        def fake_run(argv, **kwargs):
            captured["argv"] = argv
            captured["env"] = kwargs.get("env")
            return Proc()

        original = runner.subprocess.run
        runner.subprocess.run = fake_run
        try:
            with tempfile.TemporaryDirectory() as tmp:
                args = Namespace(out=tmp)
                env = {"MEMORY_PROFILE_PASSWORD": "secret", "PATH": "/usr/bin"}
                outputs = runner._mcp_query_outputs(
                    args, "acc0", [{"id": "q1", "query": "预算"}], 5.0,
                    Path(tmp) / "stack" / "profiles", env)
        finally:
            runner.subprocess.run = original
        self.assertEqual(outputs[0]["query_id"], "q1")
        self.assertEqual(captured["env"], env)
        self.assertEqual(captured["argv"][6], str(Path(captured["argv"][6])))
        self.assertTrue(captured["argv"][6].endswith("stack/profiles"))
        payload = json.loads(captured["argv"][7])
        self.assertTrue(payload[0]["arguments"]["include_raw_evidence"])
        self.assertEqual(payload[0]["arguments"]["max_bytes"], 65536)


class WorkerStageTests(unittest.TestCase):
    def write_config(self, root: Path, capabilities: dict) -> str:
        path = root / "audio.json"
        path.write_text(json.dumps({"schema_version": "audio-analysis-config-v1",
                                    "capabilities": capabilities}), encoding="utf-8")
        return str(path)

    def test_enabled_capabilities_add_their_stages(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write_config(Path(tmp), {
                "vad": {"enabled": False},
                "tagging": {"enabled": True},
                "diarization": {"enabled": True},
            })
            stages = runner.worker_stages(path).split(",")
            self.assertEqual(stages[:5], ["ingest", "asr", "extract", "publish", "embed"])
            self.assertIn("audio_tagging", stages)
            self.assertIn("audio_diarization", stages)
            self.assertNotIn("audio_vad", stages)
            self.assertNotIn("audio_speaker", stages)

    def test_no_configuration_keeps_the_base_stages(self):
        self.assertEqual(runner.worker_stages(""), "ingest,asr,extract,publish,embed")

    def test_a_missing_config_is_an_error_not_a_silent_downgrade(self):
        with self.assertRaisesRegex(ValueError, "audio analysis config missing"):
            runner.worker_stages("/nonexistent/audio.json")


class AudioAnalysisCollectionTests(unittest.TestCase):
    def detail(self, runs: list[dict], capabilities: list[dict]) -> dict:
        return {"recordings": [
            {"recording_id": "rec-other", "audio_analysis": {"capabilities": [], "runs": []}},
            {"recording_id": "rec-1", "audio_analysis": {"capabilities": capabilities, "runs": runs}},
        ]}

    def test_events_use_the_recording_clock_and_only_succeeded_runs(self):
        client = PagedTurnsClient({})
        client.fetch = lambda _path: self.detail(
            runs=[
                {"run_id": "run-tag", "capability": "tagging", "model_id": "m", "model_revision": "r1",
                 "status": "succeeded", "active": True, "current": True, "start_ms": 1000,
                 "events": [
                     {"event_type": "acoustic_event", "start_ms": 100, "end_ms": 500,
                      "run_start_ms": 2000, "payload": {"label": "speech"}},
                     {"event_type": "speech_activity", "start_ms": 0, "end_ms": 900, "payload": {}},
                 ]},
                {"run_id": "run-dia", "capability": "diarization", "model_id": "m", "model_revision": "r2",
                 "status": "succeeded", "active": True, "current": True, "start_ms": 500,
                 "events": [{"event_type": "speaker_segment", "start_ms": 10, "end_ms": 410,
                             "payload": {"speaker": "S1"}}]},
                {"run_id": "run-old", "capability": "tagging", "status": "failed", "active": False,
                 "current": False, "start_ms": 0,
                 "events": [{"event_type": "acoustic_event", "start_ms": 0, "end_ms": 100,
                             "payload": {"label": "noise"}}]},
            ],
            capabilities=[{"capability": "tagging", "status": "succeeded"},
                          {"capability": "diarization", "status": "succeeded"},
                          {"capability": "vad", "status": "not_requested"}])
        collected = runner.collect_audio_analysis(client, "conv-1", "rec-1")
        self.assertEqual(collected["acoustic_events"],
                         [{"start_ms": 2100, "end_ms": 2500, "label": "speech",
                           "run_id": "run-tag", "capability": "tagging", "model_id": "m",
                           "model_revision": "r1", "generation": None}])
        self.assertEqual(collected["diarization"],
                         [{"start_ms": 510, "end_ms": 910, "speaker": "S1",
                           "run_id": "run-dia", "capability": "diarization", "model_id": "m",
                           "model_revision": "r2", "generation": None}])
        self.assertEqual(collected["capabilities"],
                         {"tagging": "succeeded", "diarization": "succeeded", "vad": "not_requested"})
        self.assertEqual([run["run_id"] for run in collected["runs"]], ["run-tag", "run-dia"])

    def test_a_recording_without_analysis_reports_no_capabilities(self):
        client = PagedTurnsClient({})
        client.fetch = lambda _path: {"recordings": [{"recording_id": "rec-1"}]}
        collected = runner.collect_audio_analysis(client, "conv-1", "rec-1")
        self.assertEqual(collected, {"capabilities": {}, "runs": [], "acoustic_events": [], "diarization": []})


class DecisionClaimTests(unittest.TestCase):
    def trail(self, records: list[dict], truncated: bool = False) -> dict:
        return {"topics": [{"id": "topic:1", "history": records}], "truncated": truncated}

    def record(self, candidate: str, state: str, evidence: list[dict], text: str = "采用方案A") -> dict:
        return {"id": f"record:{candidate}", "candidate_id": candidate, "state": state, "text": text,
                "evidence": evidence}

    def test_candidates_and_confirmed_records_become_unique_predictions(self):
        own = {"turn_id": "t1", "conversation_id": "conv-1", "recording_id": "rec-1", "revision": "rev-1"}
        trails = [{"topic_id": "topic:1", "result": self.trail([
            self.record("cand-1", "decision_candidate", [own]),
            self.record("cand-1", "confirmed", [own]),
            self.record("cand-2", "observation", [own], text="只是讨论"),
            self.record("cand-3", "decision_candidate",
                        [{"turn_id": "t9", "conversation_id": "conv-other", "recording_id": "rec-9"}]),
            self.record("cand-4", "decision_candidate", [{"turn_id": "t7"}]),
        ])}]
        claims, diagnostics = runner.decision_claims("s1", "conv-1", "rec-1", trails)
        self.assertEqual([claim["claim_id"] for claim in claims], ["s1:cand-1"])
        self.assertEqual(claims[0]["kind"], "decision")
        self.assertEqual(claims[0]["status"], "decision_candidate")
        self.assertEqual(claims[0]["evidence"][0]["turn_id"], "t1")
        self.assertTrue(diagnostics["complete"])
        self.assertEqual(diagnostics["skipped"]["foreign_evidence"], ["s1:cand-3"])
        self.assertEqual(diagnostics["skipped"]["unattributed_evidence"], ["s1:cand-4"])

    def test_a_truncated_trail_is_not_complete(self):
        own = {"turn_id": "t1", "conversation_id": "conv-1", "recording_id": "rec-1"}
        trails = [{"topic_id": "topic:1", "result": self.trail(
            [self.record("cand-1", "confirmed", [own])], truncated=True)}]
        claims, diagnostics = runner.decision_claims("s1", "conv-1", "rec-1", trails)
        self.assertEqual(len(claims), 1)
        self.assertFalse(diagnostics["complete"])
        self.assertTrue(diagnostics["truncated"])

    def test_a_failed_probe_makes_the_collection_incomplete(self):
        trails = [{"topic_id": "topic:1", "error": "decision trail is not configured"}]
        claims, diagnostics = runner.decision_claims("s1", "conv-1", "rec-1", trails)
        self.assertEqual(claims, [])
        self.assertFalse(diagnostics["complete"])
        self.assertEqual(diagnostics["errors"][0]["topic_id"], "topic:1")


class CombineBundlesTests(unittest.TestCase):
    def bundle(self, sample_id: str, **overrides) -> dict:
        bundle = {
            "schema": "context-engine-predictions-v1",
            "run_id": f"run-{sample_id}",
            "audio_sha256": "a" * 64,
            "gold_sha256": "b" * 64,
            "config_sha256": "c" * 64,
            "binary_sha256": "d" * 64,
            "model": {"provider": "local-qwen", "revision": "rev-1"},
            "samples": [{"sample_id": sample_id, "turns": [], "acoustic_events": []}],
            "query_outputs": [{"query_id": f"{sample_id}:q1", "web": {"refs": []}, "mcp": {"refs": []}}],
            "claims": [{"claim_id": f"{sample_id}:c1", "kind": "decision", "text": "x"}],
            "stages": {"complete": True},
            "resources": {},
        }
        bundle.update(overrides)
        return bundle

    def manifest(self) -> dict:
        return {"samples": [{"id": "s1", "audio_sha256": "a" * 64, "gold_sha256": "b" * 64},
                            {"id": "s2", "audio_sha256": "a" * 64, "gold_sha256": "b" * 64}]}

    def test_two_consistent_bundles_merge(self):
        combined = runner._combine_bundles([self.bundle("s1"), self.bundle("s2")], self.manifest())
        self.assertEqual([item["sample_id"] for item in combined["samples"]], ["s1", "s2"])
        self.assertEqual(len(combined["query_outputs"]), 2)

    def test_a_duplicate_sample_id_is_refused(self):
        with self.assertRaisesRegex(RuntimeError, "duplicate sample_id"):
            runner._combine_bundles([self.bundle("s1"), self.bundle("s1")], self.manifest())

    def test_a_different_binary_identity_is_refused(self):
        other = self.bundle("s2", binary_sha256="e" * 64)
        with self.assertRaisesRegex(RuntimeError, "binary_sha256"):
            runner._combine_bundles([self.bundle("s1"), other], self.manifest())

    def test_a_different_model_identity_is_refused(self):
        other = self.bundle("s2", model={"provider": "local-qwen", "revision": "rev-2"})
        with self.assertRaisesRegex(RuntimeError, "model"):
            runner._combine_bundles([self.bundle("s1"), other], self.manifest())

    def test_a_sample_outside_the_manifest_is_refused(self):
        with self.assertRaisesRegex(RuntimeError, "not authorized"):
            runner._combine_bundles([self.bundle("s1"), self.bundle("s3")], self.manifest())


if __name__ == "__main__":
    unittest.main()
