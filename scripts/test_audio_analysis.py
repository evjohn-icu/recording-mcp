#!/usr/bin/env python3
"""Protocol tests for the audio-analysis-v1 Python adapter.

Stdlib unittest plus a numpy stand-in (same convention as
``scripts/test_local_asr.py``): the suite needs no heavy dependency and runs
with the plain ``python3`` interpreter. Model-backed inference is exercised by
the dedicated integration tests with real pinned models; these tests prove the
framing math, validation rules and CLI protocol instead.
"""

from __future__ import annotations

import contextlib
import json
import math
import pathlib
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

try:
    from scripts import audio_analysis_models as models
except ImportError:  # unittest discovery (-s scripts) puts scripts/ on sys.path
    import audio_analysis_models as models

SCRIPT = pathlib.Path(__file__).resolve().with_name("audio-analysis.py")


# ---------------------------------------------------------------------------
# numpy stand-in
# ---------------------------------------------------------------------------


class FakeFeats:
    """Stand-in for arrays produced by np.asarray in the inference paths."""

    def __init__(self, payload):
        self.payload = payload

    def __getitem__(self, item):
        if isinstance(item, tuple):
            return self  # [None, :, :] style reshaping is a no-op stand-in
        return self.payload[item]

    def reshape(self, *shape):
        return self

    def unsqueeze(self, _dim):
        return self


class FakeArray:
    """Minimal numpy.ndarray stand-in covering the adapter's array operations."""

    def __init__(self, values):
        self.values = [float(value) for value in values]

    def __len__(self):
        return len(self.values)

    def __getitem__(self, item):
        if isinstance(item, slice):
            return FakeArray(self.values[item])
        return self.values[item]

    def __iter__(self):
        return iter(self.values)

    def astype(self, dtype):
        return self

    def __truediv__(self, other):
        return FakeArray([value / other for value in self.values])

    def reshape(self, *shape):
        return self

    def tolist(self):
        return list(self.values)


class FakeNumpy:
    """Stands in for the numpy surface read_pcm/frame_signal/window_5s use."""

    def frombuffer(self, data, dtype="float32"):
        if dtype not in ("int16", "<i2"):
            raise AssertionError("unexpected dtype " + str(dtype))
        count = len(data) // 2
        return FakeArray(struct.unpack("<" + "h" * count, data[: count * 2]))

    def zeros(self, size, dtype=None):
        if isinstance(size, tuple):
            total = 1
            for dimension in size:
                total *= dimension
            size = total
        return FakeArray([0.0] * size)

    def pad(self, array, pad_width):
        before, after = pad_width
        return FakeArray([0.0] * before + list(array) + [0.0] * after)

    def concatenate(self, arrays):
        return FakeArray([value for array in arrays for value in array])

    def asarray(self, values, dtype=None):
        return FakeFeats(values)


@contextlib.contextmanager
def fake_numpy():
    with mock.patch.dict(sys.modules, {"numpy": FakeNumpy()}):
        yield


def pcm_bytes(values_int16):
    return struct.pack("<" + "h" * len(values_int16), *values_int16)


def tone(seconds, frequency, amplitude=0.8):
    return [
        amplitude * math.sin(2.0 * math.pi * frequency * i / 16000.0)
        for i in range(int(seconds * 16000))
    ]


def make_request(**overrides):
    request = {
        "schema_version": "audio-analysis-v1",
        "run_id": "run-1",
        "recording_id": "rec-1",
        "source_audio_sha256": "a" * 64,
        "input_sha256": "b" * 64,
        "transcript_revision": None,
        "model_id": "campplus",
        "model_revision": "rev1",
        "model_files_sha256": {"campplus.onnx": "c" * 64},
        "capability": "vad",
        "start_ms": 0,
        "end_ms": 1000,
        "config_sha256": "d" * 64,
        "source_attachment_ids": [],
        "source_snapshot_sha256": "",
    }
    request.update(overrides)
    return request


def make_event(**overrides):
    event = {
        "id": "e1",
        "event_type": "speech_activity",
        "start_ms": 0,
        "end_ms": 500,
        "confidence": 0.9,
        "payload": {},
    }
    event.update(overrides)
    return event


def make_config(enabled=True, files=None, capability="vad", **overrides):
    if files is None:
        files = []
    entry = {
        "enabled": enabled,
        "model_id": "silero-vad",
        "model_revision": "rev1",
        "files": files,
        "device": "cpu",
        "frontend_id": "poc-silero",
    }
    return {
        "schema_version": "audio-analysis-config-v1",
        "python": sys.executable,
        "runner": "scripts/audio-analysis.py",
        "gpu_lock": "/tmp/gpu.lock",
        "lock_timeout_seconds": 600,
        "load_timeout_seconds": 300,
        "run_timeout_seconds": 7200,
        "capabilities": {capability: dict(entry, **overrides)},
    }


# ---------------------------------------------------------------------------
# read_pcm
# ---------------------------------------------------------------------------


class ReadPcmTests(unittest.TestCase):
    def test_window_clipping(self):
        values = [(i % 2000) - 1000 for i in range(32000)]  # 2 s
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "audio.pcm"
            path.write_bytes(pcm_bytes(values))
            with fake_numpy():
                samples = models.read_pcm(str(path), 0, 1000)
        self.assertEqual(len(samples), 16000)
        for index in (0, 1, 7999, 15999):
            self.assertAlmostEqual(
                samples[index], values[index] / 32768.0, places=6
            )

    def test_window_overrun_is_an_error(self):
        values = [100] * 32000  # exactly 2 s
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "audio.pcm"
            path.write_bytes(pcm_bytes(values))
            with fake_numpy():
                with self.assertRaises(models.AdapterError) as caught:
                    models.read_pcm(str(path), 0, 2001)
        self.assertEqual(caught.exception.code, "audio_input_invalid")

    def test_invalid_window_bounds(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "audio.pcm"
            path.write_bytes(pcm_bytes([0] * 16000))
            with fake_numpy():
                for start, end in ((0, 0), (500, 500), (10, 5), (-1, 100)):
                    with self.assertRaises(models.AdapterError) as caught:
                        models.read_pcm(str(path), start, end)
                    self.assertEqual(caught.exception.code, "audio_input_invalid")

    def test_missing_pcm_file(self):
        with fake_numpy():
            with self.assertRaises(models.AdapterError) as caught:
                models.read_pcm("/nonexistent/audio.pcm", 0, 100)
        self.assertEqual(caught.exception.code, "audio_input_invalid")

    def test_silence_versus_tone(self):
        with tempfile.TemporaryDirectory() as tmp:
            silence = pathlib.Path(tmp) / "silence.pcm"
            silence.write_bytes(pcm_bytes([0] * 16000))
            wave = [int(0.5 * 32767 * math.sin(2 * math.pi * 440 * i / 16000)) for i in range(16000)]
            tone_path = pathlib.Path(tmp) / "tone.pcm"
            tone_path.write_bytes(pcm_bytes(wave))
            with fake_numpy():
                quiet = models.read_pcm(str(silence), 0, 1000)
                sound = models.read_pcm(str(tone_path), 0, 1000)
        self.assertEqual(sum(v * v for v in quiet), 0.0)
        rms = math.sqrt(sum(v * v for v in sound) / len(sound))
        self.assertGreater(rms, 0.1)


# ---------------------------------------------------------------------------
# Silero framing
# ---------------------------------------------------------------------------


class FrameSignalTests(unittest.TestCase):
    def test_tail_padded_timestamps_clamped(self):
        values = list(range(1200))  # 75 ms: two full frames plus a 176-sample tail
        inputs = []

        def scorer(model_input):
            inputs.append(list(model_input))
            return 0.5

        with fake_numpy():
            probabilities, ranges = models.frame_signal(values, 512, 64, scorer)
        self.assertEqual(len(probabilities), 3)  # probabilities == frame count
        self.assertEqual([len(item) for item in inputs], [576, 576, 576])
        # every frame is 64 carried context samples followed by 512 fresh ones
        self.assertEqual(inputs[0][:64], [0.0] * 64)
        self.assertEqual(inputs[0][64:], [float(v) for v in values[:512]])
        # the carried context is the tail of the previous frame (recursive continuity)
        self.assertEqual(inputs[1][:64], [float(v) for v in values[448:512]])
        self.assertEqual(inputs[1][64:], [float(v) for v in values[512:1024]])
        self.assertEqual(inputs[2][:64], [float(v) for v in values[960:1024]])
        # the short final frame is zero-padded...
        self.assertEqual(inputs[2][64 : 64 + 176], [float(v) for v in values[1024:1200]])
        self.assertEqual(inputs[2][64 + 176 :], [0.0] * (512 - 176))
        # ...while its timestamps are clamped to the real length
        self.assertEqual(ranges, [(0, 32), (32, 64), (64, 75)])

    def test_single_frame_window(self):
        with fake_numpy():
            probabilities, ranges = models.frame_signal(
                [0.5] * 100, 512, 64, lambda _input: 0.25
            )
        self.assertEqual(probabilities, [0.25])
        self.assertEqual(ranges, [(0, 6)])

    def test_requires_scorer(self):
        with self.assertRaises(ValueError):
            models.frame_signal([0.0] * 600, 512, 64)


# ---------------------------------------------------------------------------
# tagging windows
# ---------------------------------------------------------------------------


class WindowTests(unittest.TestCase):
    def test_last_window_padded_and_clamped(self):
        values = [0.25] * 136000  # 8.5 s
        with fake_numpy():
            windows = models.window_5s(values)
        self.assertEqual(len(windows), 2)
        first_start, first_end, first_window = windows[0]
        second_start, second_end, second_window = windows[1]
        self.assertEqual((first_start, first_end), (0, 5000))
        self.assertEqual(len(first_window), 80000)
        self.assertEqual((second_start, second_end), (5000, 8500))
        self.assertEqual(len(second_window), 80000)  # padded to the fixed shape
        self.assertEqual(list(second_window)[:56000], values[80000:136000])
        self.assertEqual(list(second_window)[56000:], [0.0] * 24000)

    def test_single_short_window(self):
        with fake_numpy():
            windows = models.window_5s([0.1] * 16000)
        self.assertEqual(len(windows), 1)
        start, end, window = windows[0]
        self.assertEqual((start, end), (0, 1000))
        self.assertEqual(len(window), 80000)


# ---------------------------------------------------------------------------
# CAM++ fbank
# ---------------------------------------------------------------------------


class Recorder:
    def __init__(self):
        self.frame_opts = self
        self.mel_opts = self


class FbankTests(unittest.TestCase):
    def test_options_exact(self):
        self.assertEqual(
            models.FBANK_OPTIONS,
            {
                "samp_freq": 16000.0,
                "frame_length_ms": 25.0,
                "frame_shift_ms": 10.0,
                "snip_edges": True,
                "dither": 0.0,
                "num_bins": 80,
                "low_freq": 20.0,
                "high_freq": 0.0,
                "energy_floor": 0.0,
                "use_log_fbank": True,
                "use_power": True,
            },
        )

    def test_apply_options_records_constants(self):
        recorder = Recorder()
        models.apply_fbank_options(recorder)
        self.assertEqual(recorder.frame_opts.samp_freq, 16000.0)
        self.assertEqual(recorder.frame_opts.frame_length_ms, 25.0)
        self.assertEqual(recorder.frame_opts.frame_shift_ms, 10.0)
        self.assertIs(recorder.frame_opts.snip_edges, True)
        self.assertEqual(recorder.frame_opts.dither, 0.0)
        self.assertEqual(recorder.mel_opts.num_bins, 80)
        self.assertEqual(recorder.mel_opts.low_freq, 20.0)
        self.assertEqual(recorder.mel_opts.high_freq, 0.0)
        self.assertEqual(recorder.energy_floor, 0.0)
        self.assertIs(recorder.use_log_fbank, True)
        self.assertIs(recorder.use_power, True)

    def test_cmn_is_per_span(self):
        span_a = tone(0.16, 220.0)
        span_b = tone(0.16, 880.0)
        normal_a = models.fbank_features(span_a, backend="python")
        normal_b = models.fbank_features(span_b, backend="python")
        self.assertTrue(normal_a)
        self.assertTrue(normal_b)
        self.assertEqual(len(normal_a[0]), 80)
        # every span is normalised over its own frames only: per-bin mean zero
        for span in (normal_a, normal_b):
            for bin_index in range(len(span[0])):
                mean = sum(row[bin_index] for row in span) / len(span)
                self.assertLess(abs(mean), 1e-6)
        raw_a = models._fbank_frames(span_a, models._spectrum_python)
        raw_b = models._fbank_frames(span_b, models._spectrum_python)
        # joint normalisation over both spans would differ from per-span CMN
        joint = models.cepstral_mean_normalize(raw_a + raw_b)
        worst = max(abs(x - y) for x, y in zip(joint[0], normal_a[0]))
        self.assertGreater(worst, 1e-3)
        # and the public API is exactly raw frames plus per-span CMN
        self.assertEqual(normal_a, models.cepstral_mean_normalize(raw_a))

    def test_span_too_short_yields_no_frames(self):
        self.assertEqual(models.fbank_features([0.1] * 100, backend="python"), [])


# ---------------------------------------------------------------------------
# vectors
# ---------------------------------------------------------------------------


class VectorTests(unittest.TestCase):
    def test_l2_normalize_edges(self):
        self.assertIsNone(models.l2_normalize([]))
        self.assertIsNone(models.l2_normalize([float("nan")]))
        self.assertIsNone(models.l2_normalize([float("inf")]))
        self.assertIsNone(models.l2_normalize([0.0, 0.0, 0.0]))
        self.assertIsNone(models.l2_normalize(None))
        normalized = models.l2_normalize([3.0, 4.0])
        self.assertAlmostEqual(normalized[0], 0.6, places=9)
        self.assertAlmostEqual(normalized[1], 0.8, places=9)

    def test_cluster_embedding_weighting(self):
        cluster = models.cluster_embedding([([1.0, 0.0], 1.0), ([0.0, 1.0], 3.0)])
        self.assertAlmostEqual(cluster[0], 0.31622776601683794, places=9)
        self.assertAlmostEqual(cluster[1], 0.9486832980505138, places=9)
        # unit norm: the weighted mean was re-normalised
        self.assertAlmostEqual(sum(v * v for v in cluster), 1.0, places=9)

    def test_cluster_embedding_skips_invalid_entries(self):
        cluster = models.cluster_embedding(
            [
                ([1.0, 0.0], 2.0),
                ([float("nan"), 1.0], 5.0),  # non-finite vector skipped
                ([0.5, 0.5], 0.0),  # zero duration skipped
                ([1.0], 2.0),  # dimension mismatch skipped
                (None, 2.0),
            ]
        )
        self.assertAlmostEqual(cluster[0], 1.0, places=9)
        self.assertAlmostEqual(cluster[1], 0.0, places=9)

    def test_cluster_embedding_all_invalid(self):
        self.assertIsNone(models.cluster_embedding([]))
        self.assertIsNone(models.cluster_embedding([([0.0, 0.0], 1.0)]))


# ---------------------------------------------------------------------------
# event ids
# ---------------------------------------------------------------------------


class EventIdTests(unittest.TestCase):
    def test_deterministic(self):
        first = models.event_id("run-1", 0, "speech_activity", 0, 320)
        second = models.event_id("run-1", 0, "speech_activity", 0, 320)
        self.assertEqual(first, second)
        self.assertEqual(len(first), 16)
        int(first, 16)  # hex prefix

    def test_unique_across_coordinates(self):
        base = models.event_id("run-1", 0, "speech_activity", 0, 320)
        variants = {
            models.event_id("run-1", 1, "speech_activity", 0, 320),
            models.event_id("run-1", 0, "acoustic_event", 0, 320),
            models.event_id("run-1", 0, "speech_activity", 32, 352),
            models.event_id("run-2", 0, "speech_activity", 0, 320),
        }
        self.assertNotIn(base, variants)
        self.assertEqual(len(variants), 4)


# ---------------------------------------------------------------------------
# validation
# ---------------------------------------------------------------------------


class ValidateRequestTests(unittest.TestCase):
    def test_valid_request_passes(self):
        models.validate_request(make_request())

    def test_rejections(self):
        cases = [
            make_request(schema_version="audio-analysis-v0"),
            make_request(run_id=""),
            make_request(recording_id="  "),
            make_request(source_audio_sha256="short"),
            make_request(input_sha256="z" * 64),
            make_request(config_sha256=None),
            make_request(capability="whisper"),
            make_request(model_id=""),
            make_request(model_revision=""),
            make_request(model_files_sha256={}),
            make_request(model_files_sha256={"m.onnx": "abc"}),
            make_request(start_ms=1000, end_ms=500),
            make_request(start_ms=0, end_ms=0),
            make_request(source_attachment_ids=["att-1"]),
            make_request(
                capability="speaker",
                source_attachment_ids=["att-1"],
                source_snapshot_sha256="",
            ),
            make_request(source_snapshot_sha256="nothex"),
        ]
        for request in cases:
            with self.assertRaises(models.AdapterError) as caught:
                models.validate_request(request)
            self.assertEqual(caught.exception.code, "audio_input_invalid")

    def test_speaker_run_may_pin_attachments(self):
        request = make_request(
            capability="speaker",
            source_attachment_ids=["att-1", "att-2"],
            source_snapshot_sha256="e" * 64,
        )
        models.validate_request(request)


class ValidateResultTests(unittest.TestCase):
    def build(self, **overrides):
        request = make_request(**overrides.pop("request_overrides", {}))
        events = overrides.pop("events", [make_event()])
        result = models.build_result(request, overrides.pop("status", "succeeded"), events)
        if overrides.pop("with_error", False):
            result["error"] = {"code": "audio_model_unavailable", "retryable": False, "action": "x"}
        result.update(overrides)
        return request, result

    def test_valid_result_passes(self):
        request, result = self.build()
        models.validate_result(result, request, 1000)

    def test_zero_events_succeeded_is_valid(self):
        request, result = self.build(events=[])
        models.validate_result(result, request, 1000)

    def test_identity_mismatch(self):
        request, result = self.build()
        result["recording_id"] = "other"
        with self.assertRaises(models.AdapterError) as caught:
            models.validate_result(result, request, 1000)
        self.assertEqual(caught.exception.code, "audio_result_invalid")

    def test_run_id_mismatch(self):
        request, result = self.build()
        result["run_id"] = "other"
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)

    def test_duplicate_event_ids(self):
        request, result = self.build(events=[make_event(), make_event()])
        with self.assertRaises(models.AdapterError) as caught:
            models.validate_result(result, request, 1000)
        self.assertEqual(caught.exception.code, "audio_result_invalid")

    def test_event_out_of_bounds(self):
        for event in (
            make_event(end_ms=1500),  # beyond the analysed window
            make_event(start_ms=-1),
            make_event(start_ms=500, end_ms=500),
        ):
            request, result = self.build(events=[event])
            with self.assertRaises(models.AdapterError):
                models.validate_result(result, request, 1000)

    def test_confidence_bounds(self):
        for confidence in (1.5, -0.1, float("nan"), float("inf"), "0.5"):
            request, result = self.build(events=[make_event(confidence=confidence)])
            with self.assertRaises(models.AdapterError):
                models.validate_result(result, request, 1000)
        request, result = self.build(events=[make_event(confidence=None)])
        models.validate_result(result, request, 1000)

    def test_failed_result_must_not_carry_events(self):
        request, result = self.build(status="failed")
        with self.assertRaises(models.AdapterError) as caught:
            models.validate_result(result, request, 1000)
        self.assertEqual(caught.exception.code, "audio_result_invalid")

    def test_failed_result_without_events_is_valid(self):
        request, result = self.build(status="failed", events=[])
        result["error"] = {"code": "audio_model_unavailable", "retryable": False, "action": "x"}
        models.validate_result(result, request, 1000)

    def test_succeeded_result_must_not_carry_error(self):
        request, result = self.build(with_error=True)
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)

    def test_status_enum(self):
        request, result = self.build(status="running")
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)

    def test_unknown_event_type(self):
        request, result = self.build(events=[make_event(event_type="whisper_word")])
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)

    def test_identity_proposal_needs_proposed_status(self):
        proposal = make_event(
            event_type="speaker_identity_proposal",
            payload={"person_id": "p1", "status": "confirmed"},
        )
        request, result = self.build(events=[proposal])
        with self.assertRaises(models.AdapterError) as caught:
            models.validate_result(result, request, 1000)
        self.assertEqual(caught.exception.code, "audio_result_invalid")
        proposal["payload"] = {"person_id": "p1", "status": "proposed"}
        models.validate_result(models.build_result(request, "succeeded", [proposal]), request, 1000)

    def test_transcript_revision_must_match(self):
        request, result = self.build(request_overrides={"transcript_revision": "rev2"})
        result["transcript_revision"] = "rev9"
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)
        request, result = self.build(request_overrides={"transcript_revision": "rev2"})
        result["transcript_revision"] = None
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)
        request, result = self.build()
        result["transcript_revision"] = "rev2"
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)
        request, result = self.build(request_overrides={"transcript_revision": "rev2"})
        models.validate_result(result, request, 1000)

    def test_attachment_list_must_match(self):
        request, result = self.build()
        result["source_attachment_ids"] = ["att-1"]
        with self.assertRaises(models.AdapterError):
            models.validate_result(result, request, 1000)


# ---------------------------------------------------------------------------
# pure helper behaviour
# ---------------------------------------------------------------------------


class HelperTests(unittest.TestCase):
    def test_active_spans_merge_and_peak(self):
        ranges = [(0, 32), (32, 64), (64, 96), (96, 128), (128, 160)]
        spans = models.active_spans([0.2, 0.9, 0.8, 0.3, 0.95], ranges, 0.5)
        self.assertEqual(spans, [(32, 96, 0.9), (128, 160, 0.95)])

    def test_active_spans_all_below_threshold(self):
        ranges = [(0, 32), (32, 64)]
        self.assertEqual(models.active_spans([0.1, 0.2], ranges, 0.5), [])

    def test_overlap_flags(self):
        flags = models.overlap_flags([(0, 100), (90, 200), (300, 400), (350, 450), (500, 600)])
        self.assertEqual(flags, [True, True, True, True, False])

    def test_speaker_span_bounds(self):
        bounds = models.speaker_span_bounds(136000, 80000)
        self.assertEqual(bounds, [(0, 80000), (80000, 136000)])
        self.assertEqual(models.speaker_span_bounds(0, 80000), [])
        self.assertEqual(models.speaker_span_bounds(100, 0), [])


# ---------------------------------------------------------------------------
# capability inference glue (model sessions stubbed; no heavy dependencies)
# ---------------------------------------------------------------------------


class FakeIO:
    def __init__(self, name):
        self.name = name


class FakeVadSession:
    """Returns one canned probability per frame call."""

    def __init__(self, probabilities):
        self.probabilities = list(probabilities)
        self.calls = 0

    def run(self, _none, feed):
        value = self.probabilities[self.calls]
        self.calls += 1
        return FakeFeats([value]), "state"


class FakeEmbeddingSession:
    def __init__(self, vector):
        self.vector = list(vector)

    def get_inputs(self):
        return [FakeIO("x")]

    def get_outputs(self):
        return [FakeIO("embedding")]

    def run(self, _none, _feed):
        return [FakeFeats([FakeArray(self.vector)])]


class RunVadTests(unittest.TestCase):
    def resolved(self, threshold=None):
        outcome = {
            "capability": "vad",
            "device": "cpu",
            "frontend_id": "poc-silero",
            "model_id": "silero-vad",
            "model_revision": "rev1",
            "files": ["model.onnx"],
        }
        if threshold is not None:
            outcome["threshold"] = threshold
        return outcome

    def run_vad(self, request, samples, resolved, probabilities):
        session = FakeVadSession(probabilities)
        with fake_numpy(), mock.patch.object(models, "_onnx_session", return_value=session):
            return models.run_vad(request, samples, resolved)

    def test_active_and_inactive_frames_become_spans(self):
        request = make_request(start_ms=0, end_ms=75)  # 1200 samples
        samples = [0.9] * 512 + [0.0] * 512 + [0.9] * 176
        outcome = self.run_vad(request, samples, self.resolved(), [0.9, 0.1, 0.9])
        self.assertEqual(outcome.device, "cpu")
        self.assertEqual(len(outcome.events), 2)
        first, second = outcome.events
        self.assertEqual((first["start_ms"], first["end_ms"]), (0, 32))
        self.assertEqual((second["start_ms"], second["end_ms"]), (64, 75))
        for event in outcome.events:
            self.assertEqual(event["event_type"], "speech_activity")
            self.assertEqual(event["confidence"], 0.9)
            self.assertEqual(event["payload"], {"threshold": 0.5, "uncalibrated": True})
        self.assertNotEqual(first["id"], second["id"])
        result = models.build_result(request, "succeeded", outcome.events)
        models.validate_result(result, request, 75)

    def test_custom_threshold_documented_in_payload(self):
        request = make_request(start_ms=0, end_ms=75)
        samples = [0.6] * 1200
        outcome = self.run_vad(request, samples, self.resolved(threshold=0.85), [0.6, 0.6, 0.6])
        self.assertEqual(outcome.events, [])  # below the configured threshold

    def test_no_detection_is_empty_success(self):
        request = make_request(start_ms=0, end_ms=75)
        outcome = self.run_vad(request, [0.1] * 1200, self.resolved(), [0.1, 0.1, 0.1])
        self.assertEqual(outcome.events, [])
        models.validate_result(
            models.build_result(request, "succeeded", outcome.events), request, 75
        )

    def test_cuda_device_is_rejected(self):
        resolved = self.resolved()
        resolved["device"] = "cuda"
        with self.assertRaises(models.AdapterError) as caught:
            models.run_vad(make_request(), [0.0] * 600, resolved)
        self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_no_onnx_file_pinned(self):
        resolved = self.resolved()
        resolved["files"] = ["weights.bin"]
        with self.assertRaises(models.AdapterError) as caught:
            models.run_vad(make_request(), [0.0] * 600, resolved)
        self.assertEqual(caught.exception.code, "audio_model_unavailable")


class RunSpeakerTests(unittest.TestCase):
    def resolved(self, span_seconds=0.2):
        return {
            "capability": "speaker",
            "device": "cpu",
            "frontend_id": "campplus",
            "model_id": "campplus",
            "model_revision": "rev1",
            "files": ["campplus.onnx"],
            "span_seconds": span_seconds,
            "fbank_backend": "python",
        }

    def run_speaker(self, request, samples, resolved, vector):
        with fake_numpy(), mock.patch.object(
            models, "_onnx_session", return_value=FakeEmbeddingSession(vector)
        ):
            return models.run_speaker(request, samples, resolved)

    def test_spans_produce_embeddings_side_channel(self):
        request = make_request(start_ms=0, end_ms=300)  # 4800 samples
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "audio.pcm"
            pcm.write_bytes(pcm_bytes([1000] * 4800))
            with fake_numpy():
                samples = models.read_pcm(str(pcm), 0, 300)
        outcome = self.run_speaker(request, samples, self.resolved(), [0.5] * 192)
        self.assertEqual(outcome.device, "cpu")
        # 4800 samples with 3200-sample spans: two non-overlapping spans
        self.assertEqual(len(outcome.events), 2)
        first, second = outcome.events
        self.assertEqual((first["start_ms"], first["end_ms"]), (0, 200))
        self.assertEqual((second["start_ms"], second["end_ms"]), (200, 300))
        for event in outcome.events:
            self.assertEqual(event["event_type"], "speaker_segment")
            self.assertIsNone(event["confidence"])
            self.assertEqual(event["payload"]["source_kind"], "unknown")
        self.assertEqual(sorted(outcome.embeddings), sorted(e["id"] for e in outcome.events))
        for vector in outcome.embeddings.values():
            self.assertEqual(len(vector), models.SPEAKER_EMBEDDING_DIMS)
            self.assertAlmostEqual(sum(v * v for v in vector), 1.0, places=6)
        result = models.build_result(request, "succeeded", outcome.events)
        models.validate_result(result, request, 300)

    def test_zero_norm_vector_is_no_valid_voiceprint(self):
        request = make_request(start_ms=0, end_ms=300)
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "audio.pcm"
            pcm.write_bytes(pcm_bytes([0] * 4800))
            with fake_numpy():
                samples = models.read_pcm(str(pcm), 0, 300)
        with self.assertRaises(models.AdapterError) as caught:
            self.run_speaker(request, samples, self.resolved(), [0.0] * 192)
        self.assertEqual(caught.exception.code, "audio_result_invalid")

    def test_window_too_short_for_frontend(self):
        request = make_request(start_ms=0, end_ms=20)  # 320 samples < one 25 ms frame
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "audio.pcm"
            pcm.write_bytes(pcm_bytes([100] * 320))
            with fake_numpy():
                samples = models.read_pcm(str(pcm), 0, 20)
        with self.assertRaises(models.AdapterError) as caught:
            self.run_speaker(request, samples, self.resolved(), [0.5] * 192)
        self.assertEqual(caught.exception.code, "audio_input_invalid")


class _FakeNoGrad:
    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False


class _FakeCuda:
    available = True

    @classmethod
    def is_available(cls):
        return cls.available


class FakeSegment:
    def __init__(self, start, end):
        self.start = start
        self.end = end


class FakeAnnotation:
    def __init__(self, segments):
        self.segments = segments

    def itertracks(self, yield_label=True):
        return [
            (FakeSegment(start, end), None, label)
            for start, end, label in self.segments
        ]


class FakePipeline:
    def __init__(self, segments):
        self.segments = segments
        self.segmentation_batch_size = None
        self.embedding_batch_size = None
        self.moved_to = None

    def to(self, device):
        self.moved_to = device
        return self

    def __call__(self, _payload):
        return FakeAnnotation(self.segments)


class RunDiarizationTests(unittest.TestCase):
    def fake_modules(self, segments):
        import types

        pipeline = FakePipeline(segments)

        class Pipeline:
            @staticmethod
            def from_pretrained(_path):
                return pipeline

        torch = types.ModuleType("torch")
        torch.load = lambda *args, **kwargs: "weights"
        torch.cuda = _FakeCuda
        torch.device = lambda name: name
        torch.no_grad = _FakeNoGrad
        torch.from_numpy = lambda array: FakeFeats(array)
        pyannote = types.ModuleType("pyannote")
        pyannote_audio = types.ModuleType("pyannote.audio")
        pyannote_audio.Pipeline = Pipeline
        return pipeline, {"torch": torch, "pyannote": pyannote, "pyannote.audio": pyannote_audio}

    def test_segments_clamped_and_overlaps_ambiguous(self):
        request = make_request(start_ms=0, end_ms=75)  # 75 ms window
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "audio.pcm"
            pcm.write_bytes(pcm_bytes([100] * 1200))
            with fake_numpy():
                samples = models.read_pcm(str(pcm), 0, 75)
        resolved = {
            "capability": "diarization",
            "device": "cpu",
            "frontend_id": "community-1",
            "model_id": "pyannote-community-1",
            "model_revision": "rev1",
            "files": ["pyannote-community-1/config.yaml"],
        }
        pipeline, modules = self.fake_modules(
            [
                (0.0, 0.05, "SPEAKER_00"),
                (0.04, 0.08, "SPEAKER_01"),  # overlaps the first segment
                (0.09, 0.20, "SPEAKER_02"),  # beyond the window: dropped
            ]
        )
        with fake_numpy(), mock.patch.dict(sys.modules, modules):
            outcome = models.run_diarization(request, samples, resolved)
        self.assertEqual(pipeline.segmentation_batch_size, 1)
        self.assertEqual(pipeline.embedding_batch_size, 1)
        self.assertEqual(pipeline.moved_to, None)  # device cpu: no .to call
        self.assertEqual(outcome.device, "cpu")
        self.assertEqual(len(outcome.events), 2)
        first, second = outcome.events
        self.assertEqual((first["start_ms"], first["end_ms"]), (0, 50))
        self.assertEqual((second["start_ms"], second["end_ms"]), (40, 75))  # clamped
        self.assertEqual(first["payload"]["speaker"], "SPEAKER_00")  # run-local label
        self.assertEqual(first["payload"]["source_kind"], "ambiguous")
        self.assertEqual(second["payload"]["source_kind"], "ambiguous")
        result = models.build_result(request, "succeeded", outcome.events)
        models.validate_result(result, request, 75)

    def test_isolated_segments_stay_unknown(self):
        request = make_request(start_ms=0, end_ms=75)
        with tempfile.TemporaryDirectory() as tmp:
            pcm = pathlib.Path(tmp) / "audio.pcm"
            pcm.write_bytes(pcm_bytes([100] * 1200))
            with fake_numpy():
                samples = models.read_pcm(str(pcm), 0, 75)
        resolved = {
            "capability": "diarization",
            "device": "cpu",
            "frontend_id": "community-1",
            "model_id": "pyannote-community-1",
            "model_revision": "rev1",
            "files": ["pyannote-community-1/config.yaml"],
        }
        _pipeline, modules = self.fake_modules([(0.0, 0.05, "SPEAKER_00")])
        with fake_numpy(), mock.patch.dict(sys.modules, modules):
            outcome = models.run_diarization(request, samples, resolved)
        self.assertEqual(len(outcome.events), 1)
        self.assertEqual(outcome.events[0]["payload"]["source_kind"], "unknown")

    def test_cuda_without_runtime_is_rejected(self):
        resolved = {
            "capability": "diarization",
            "device": "cuda",
            "frontend_id": "community-1",
            "model_id": "pyannote-community-1",
            "model_revision": "rev1",
            "files": ["pyannote-community-1/config.yaml"],
        }
        _pipeline, modules = self.fake_modules([])
        _FakeCuda.available = False
        try:
            with self.assertRaises(models.AdapterError) as caught:
                models.run_diarization(make_request(), [0.0] * 1200, resolved)
        finally:
            _FakeCuda.available = True
        self.assertEqual(caught.exception.code, "audio_model_unavailable")


# ---------------------------------------------------------------------------
# config validation
# ---------------------------------------------------------------------------


class ConfigTests(unittest.TestCase):
    def resolve(self, config, capability="vad"):
        return models.resolve_capability(config, capability)

    def test_valid_resolution(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = pathlib.Path(tmp) / "model.onnx"
            model.write_bytes(b"onnx-bytes")
            config = make_config(
                files=[{"path": str(model), "sha256": models.sha256_file(str(model))}]
            )
            resolved = self.resolve(config)
        self.assertEqual(resolved["capability"], "vad")
        self.assertEqual(resolved["device"], "cpu")
        self.assertEqual(resolved["frontend_id"], "poc-silero")
        self.assertEqual(resolved.get("threshold"), None)
        self.assertEqual(len(resolved["files"]), 1)

    def test_missing_model_file(self):
        config = make_config(
            files=[{"path": "/nonexistent/model.onnx", "sha256": "a" * 64}]
        )
        with self.assertRaises(models.AdapterError) as caught:
            self.resolve(config)
        self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_wrong_sha256_length(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = pathlib.Path(tmp) / "model.onnx"
            model.write_bytes(b"onnx-bytes")
            config = make_config(files=[{"path": str(model), "sha256": "abc"}])
            with self.assertRaises(models.AdapterError) as caught:
                self.resolve(config)
            self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_hash_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = pathlib.Path(tmp) / "model.onnx"
            model.write_bytes(b"onnx-bytes")
            config = make_config(files=[{"path": str(model), "sha256": "f" * 64}])
            with self.assertRaises(models.AdapterError) as caught:
                self.resolve(config)
            self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_disabled_capability(self):
        with self.assertRaises(models.AdapterError) as caught:
            self.resolve(make_config(enabled=False, files=[]))
        self.assertEqual(caught.exception.code, "audio_disabled")

    def test_capability_absent(self):
        config = make_config()
        config["capabilities"] = {}
        with self.assertRaises(models.AdapterError) as caught:
            self.resolve(config)
        self.assertEqual(caught.exception.code, "audio_disabled")

    def test_no_files_pinned(self):
        with self.assertRaises(models.AdapterError) as caught:
            self.resolve(make_config(files=[]))
        self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_bad_schema_version(self):
        config = make_config()
        config["schema_version"] = "audio-analysis-config-v0"
        with self.assertRaises(models.AdapterError) as caught:
            self.resolve(config)
        self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_bad_device(self):
        with self.assertRaises(models.AdapterError) as caught:
            self.resolve(make_config(device="tpu"))
        self.assertEqual(caught.exception.code, "audio_model_unavailable")

    def test_optional_threshold_and_span(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = pathlib.Path(tmp) / "model.onnx"
            model.write_bytes(b"onnx")
            digest = models.sha256_file(str(model))
            resolved = self.resolve(
                make_config(
                    files=[{"path": str(model), "sha256": digest}], threshold=0.75
                )
            )
            self.assertEqual(resolved["threshold"], 0.75)
            with self.assertRaises(models.AdapterError) as caught:
                self.resolve(
                    make_config(files=[{"path": str(model), "sha256": digest}], threshold=1.5)
                )
            self.assertEqual(caught.exception.code, "audio_model_unavailable")
            with self.assertRaises(models.AdapterError):
                self.resolve(
                    make_config(
                        capability="speaker",
                        files=[{"path": str(model), "sha256": digest}],
                        span_seconds=0.0,
                    )
                )


# ---------------------------------------------------------------------------
# CLI end to end
# ---------------------------------------------------------------------------


class CliTests(unittest.TestCase):
    def run_cli(self, tmp, request, config):
        request_path = pathlib.Path(tmp) / "request.json"
        config_path = pathlib.Path(tmp) / "config.json"
        pcm_path = pathlib.Path(tmp) / "audio.pcm"
        out_path = pathlib.Path(tmp) / "out.json"
        request_path.write_text(json.dumps(request), encoding="utf-8")
        config_path.write_text(json.dumps(config), encoding="utf-8")
        pcm_path.write_bytes(pcm_bytes([0] * 3200))  # 0.1 s window
        process = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--request",
                str(request_path),
                "--pcm",
                str(pcm_path),
                "--config",
                str(config_path),
                "--out",
                str(out_path),
            ],
            capture_output=True,
            text=True,
            timeout=120,
            cwd=str(SCRIPT.parent.parent),
        )
        return process, out_path

    def assert_failed_document(self, process, out_path, request, code):
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertEqual(process.stdout, "")  # stdout never carries output
        self.assertNotIn("Traceback", process.stderr)
        self.assertNotIn("/", process.stderr)  # no paths on stderr
        result = json.loads(out_path.read_text(encoding="utf-8"))
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["error"]["code"], code)
        self.assertEqual(result["events"], [])
        self.assertEqual(result["run_id"], request["run_id"])
        self.assertEqual(result["recording_id"], request["recording_id"])
        self.assertEqual(result["capability"], request["capability"])
        self.assertEqual(result["start_ms"], request["start_ms"])
        self.assertEqual(result["end_ms"], request["end_ms"])
        self.assertEqual(result["model_files_sha256"], request["model_files_sha256"])
        self.assertEqual(
            result["provenance"]["adapter_version"], models.ADAPTER_VERSION
        )
        self.assertFalse(out_path.with_name("out.json.embeddings.json").exists())
        return result

    def test_disabled_capability_writes_failed_result(self):
        request = make_request()
        config = make_config(enabled=False, files=[])
        with tempfile.TemporaryDirectory() as tmp:
            process, out_path = self.run_cli(tmp, request, config)
            self.assert_failed_document(process, out_path, request, "audio_disabled")

    def test_missing_model_file_writes_failed_result(self):
        request = make_request()
        config = make_config(
            files=[{"path": "/nonexistent/model.onnx", "sha256": "a" * 64}]
        )
        with tempfile.TemporaryDirectory() as tmp:
            process, out_path = self.run_cli(tmp, request, config)
            self.assert_failed_document(process, out_path, request, "audio_model_unavailable")

    def test_hash_mismatch_writes_failed_result(self):
        with tempfile.TemporaryDirectory() as tmp:
            model = pathlib.Path(tmp) / "model.onnx"
            model.write_bytes(b"onnx-bytes")
            request = make_request()
            config = make_config(files=[{"path": str(model), "sha256": "f" * 64}])
            process, out_path = self.run_cli(tmp, request, config)
            self.assert_failed_document(process, out_path, request, "audio_model_unavailable")

    def test_unreadable_request_is_usage_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            request_path = pathlib.Path(tmp) / "request.json"
            request_path.write_text("{not json", encoding="utf-8")
            config_path = pathlib.Path(tmp) / "config.json"
            config_path.write_text(json.dumps(make_config()), encoding="utf-8")
            process = subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    "--request",
                    str(request_path),
                    "--pcm",
                    str(pathlib.Path(tmp) / "missing.pcm"),
                    "--config",
                    str(config_path),
                    "--out",
                    str(pathlib.Path(tmp) / "out.json"),
                ],
                capture_output=True,
                text=True,
                timeout=120,
                cwd=str(SCRIPT.parent.parent),
            )
        self.assertNotEqual(process.returncode, 0)
        self.assertEqual(process.stdout, "")
        self.assertIn("audio_input_invalid", process.stderr)
        self.assertNotIn("Traceback", process.stderr)

    def test_missing_arguments_is_usage_error(self):
        process = subprocess.run(
            [sys.executable, str(SCRIPT)],
            capture_output=True,
            text=True,
            timeout=120,
            cwd=str(SCRIPT.parent.parent),
        )
        self.assertNotEqual(process.returncode, 0)
        self.assertNotIn("Traceback", process.stderr)


if __name__ == "__main__":
    unittest.main()
