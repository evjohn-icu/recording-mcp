#!/usr/bin/env python3
"""Pure helpers and capability inference for the audio-analysis-v1 adapter.

This module backs ``scripts/audio-analysis.py`` (the managed subprocess the Go
``internal/audioanalysis`` runner drives) and mirrors the frozen contract in
``docs/engineering/audio-analysis-v1.md`` plus the Go types in
``internal/audioanalysis/types.go``:

- one JSON request in, one Result document out; events are emitted on the
  window clock ``0..(end_ms - start_ms)`` because the managed PCM file contains
  exactly the analysed window;
- speaker vectors never travel inside the Result; the orchestrator reads them
  from the ``OUT.json.embeddings.json`` side channel keyed by event id;
- model weights are only ever read from hash-pinned local files; nothing is
  downloaded at runtime (``HF_HUB_OFFLINE=1`` is forced before any import).

Constants come from the verified PoC in ``experiments/audio-intelligence-poc``
(the PoC itself is never imported):

- Silero VAD v5: 512-sample frames (32 ms) + 64 prepended context samples,
  recursive ``(2, 1, 128)`` state, final frame zero-padded and timestamps
  clamped to the real length, raw model probability without a second sigmoid
  (``scripts/run_vad_silero.py``).
- Zipformer tagging: non-overlapping 5 s windows, last window padded but its
  label/score range clamped (``scripts/run_audio_tagging.py``).
- CAM++ speaker: explicit Kaldi fbank (16 kHz, 25 ms frame, 10 ms shift,
  ``snip_edges``/``dither=0``/80 mel/low 20/high 0/energy floor 0/log power)
  with per-bin CMN over all frames of the span, 192-dim L2-normalised
  embedding, ``x`` input / ``embedding`` output
  (``scripts/run_voiceprint.py`` / ``run_voiceprint_onnx.py``).
- Community-1 diarization: local ``Pipeline.from_pretrained`` directory,
  ``batch_size=1``, run-local speaker labels (``scripts/run_diarization.py``).

numpy is imported lazily inside the functions that need it so protocol tests
can inject a stand-in (same convention as ``scripts/test_local_asr.py``); the
pure helpers here (framing math, normalisation, CMN, validation) work without
it. The adapter never imports anything from ``experiments/``.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re

# Network access is a contract violation: models arrive only as hash-pinned
# local files. Set these before any heavy dependency is imported.
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

CONTRACT_SCHEMA = "audio-analysis-v1"
CONFIG_SCHEMA = "audio-analysis-config-v1"
ADAPTER_VERSION = "audio-analysis-py/1.0.0"

SAMPLE_RATE = 16000
BYTES_PER_MS = 32  # 16 kHz mono S16LE = 2 bytes * 16000 samples/s

# Silero VAD v5 (PoC-verified framing).
VAD_FRAME = 512  # 32 ms at 16 kHz
VAD_CONTEXT = 64  # samples of previous audio prepended to every frame
VAD_DEFAULT_THRESHOLD = 0.5  # uncalibrated PoC default; display evidence only

# Zipformer tagging.
TAGGING_WINDOW_SECONDS = 5
TAGGING_TOP_K = 5

# CAM++ speaker.
SPEAKER_DEFAULT_SPAN_SECONDS = 10.0
SPEAKER_EMBEDDING_DIMS = 192

CAPABILITIES = ("vad", "tagging", "diarization", "speaker")
EVENT_TYPES = (
    "speech_activity",
    "acoustic_event",
    "speaker_segment",
    "speaker_identity_proposal",
)
STATUS_VALUES = ("succeeded", "failed", "invalidated")

# Stable codes shared with internal/audioanalysis/types.go.
ERROR_CODES = frozenset(
    {
        "audio_request_conflict",
        "audio_input_invalid",
        "audio_disabled",
        "audio_model_unavailable",
        "audio_dependency_unavailable",
        "audio_result_invalid",
        "audio_source_superseded",
        "audio_barrier_blocked",
        "audio_lease_lost",
        "audio_gpu_embedding_conflict",
        "audio_gpu_busy",
    }
)

# Exact Kaldi fbank options used by the CAM++ frontend (PoC make_fbank).
FBANK_OPTIONS = {
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
}
_FBANK_FLOOR = 1.1920929e-07  # float epsilon; energy_floor=0 clamps here
_FBANK_PREEMPHASIS = 0.97  # kaldi-native-fbank default, unset in the PoC
_FBANK_FRAME_LENGTH = 400  # 25 ms at 16 kHz
_FBANK_FRAME_SHIFT = 160  # 10 ms at 16 kHz
_FBANK_FFT = 512  # power-of-two FFT length for the 400-sample frame

_HEX64 = re.compile(r"[0-9a-fA-F]{64}")


class AdapterError(Exception):
    """Typed adapter failure carrying the safe public error surface.

    ``code`` is one of the stable ``audio_*`` identifiers, ``retryable`` hints
    the worker's attempt policy and ``action`` is a short, safe instruction
    that must never contain model paths, response bodies or stderr text.
    """

    def __init__(self, code: str, retryable: bool = False, action: str = ""):
        super().__init__(code)
        self.code = code
        self.retryable = bool(retryable)
        self.action = action


class RunOutcome:
    """Events, the device actually used and optional speaker embeddings."""

    __slots__ = ("events", "device", "embeddings")

    def __init__(self, events, device, embeddings=None):
        self.events = list(events or [])
        self.device = device
        self.embeddings = embeddings


# ---------------------------------------------------------------------------
# lazy numpy access
# ---------------------------------------------------------------------------


def _numpy():
    """Import numpy lazily so tests can inject a stand-in module."""
    try:
        import numpy
    except Exception as exc:  # pragma: no cover - real envs always have numpy
        raise AdapterError(
            "audio_model_unavailable", False, "numpy is required for model inference"
        ) from exc
    return numpy


def sha256_file(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json_atomic(path: str, payload) -> None:
    """Write JSON atomically; refuse NaN/Inf so the document stays valid."""
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json_safe_dump(payload, handle)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)


def json_safe_dump(payload, handle) -> None:
    json.dump(payload, handle, allow_nan=False)


# ---------------------------------------------------------------------------
# PCM input
# ---------------------------------------------------------------------------


def read_pcm(path, start_ms, end_ms):
    """Read the analysed window from a raw 16 kHz mono S16LE PCM file.

    The managed PCM file contains exactly the analysed window, so reading is
    window-relative: ``end_ms - start_ms`` samples from the start of the file
    (a longer file is clipped to the window, never padded past it). A window
    that extends beyond the real file is an error instead of silent clipping,
    mirroring the Go runner which stages exactly ``end_ms - start_ms`` worth
    of bytes. Returns float32 samples in ``[-1, 1)``.
    """
    np = _numpy()
    if not isinstance(start_ms, int) or isinstance(start_ms, bool) or not isinstance(
        end_ms, int
    ) or isinstance(end_ms, bool):
        raise AdapterError("audio_input_invalid", False, "start_ms and end_ms must be integers")
    if start_ms < 0 or end_ms <= start_ms:
        raise AdapterError(
            "audio_input_invalid", False, "the request window must satisfy 0 <= start_ms < end_ms"
        )
    window_bytes = (end_ms - start_ms) * BYTES_PER_MS
    try:
        size = os.path.getsize(path)
    except OSError as exc:
        raise AdapterError("audio_input_invalid", False, "the PCM window could not be read") from exc
    if size < window_bytes:
        raise AdapterError(
            "audio_input_invalid", False, "the requested window exceeds the real PCM duration"
        )
    try:
        with open(path, "rb") as handle:
            raw = handle.read(window_bytes)
    except OSError as exc:
        raise AdapterError("audio_input_invalid", False, "the PCM window could not be read") from exc
    if len(raw) < window_bytes:
        raise AdapterError(
            "audio_input_invalid", False, "the requested window exceeds the real PCM duration"
        )
    samples = np.frombuffer(raw, dtype="int16")
    return samples.astype("float32") / 32768.0


# ---------------------------------------------------------------------------
# Silero VAD framing
# ---------------------------------------------------------------------------


def frame_signal(samples, frame=VAD_FRAME, context=VAD_CONTEXT, scorer=None):
    """Frame samples the way Silero VAD v5 expects.

    Every frame is ``context`` carried samples from the previous frame
    followed by ``frame`` fresh samples; the carried context is the tail of
    the previous frame, which preserves the model's recursive continuity when
    ``scorer`` closes over the ONNX state. A short final frame is zero-padded
    and still scored, while its emitted timestamps are clamped to the real
    signal length. ``scorer`` receives the 1-D ``context + frame`` window and
    returns one raw probability (no extra sigmoid). Returns
    ``(probabilities, ranges)`` where ``ranges[i]`` is that frame's
    ``(start_ms, end_ms)`` on the 16 kHz clock.
    """
    if frame <= 0 or context < 0:
        raise ValueError("frame must be positive and context non-negative")
    if scorer is None:
        raise ValueError("frame_signal requires a scorer callable")
    np = _numpy()
    total = len(samples)
    carried = np.zeros(context)
    probabilities = []
    ranges = []
    for offset in range(0, total, frame):
        chunk = samples[offset : offset + frame]
        if len(chunk) < frame:
            chunk = np.pad(chunk, (0, frame - len(chunk)))
        model_input = np.concatenate([carried, chunk])
        probabilities.append(float(scorer(model_input)))
        carried = model_input[-context:] if context else model_input[:0]
        start_ms = offset * 1000 // SAMPLE_RATE
        end_ms = min(offset + frame, total) * 1000 // SAMPLE_RATE
        ranges.append((start_ms, end_ms))
    return probabilities, ranges


def active_spans(probabilities, ranges, threshold=VAD_DEFAULT_THRESHOLD):
    """Merge consecutive frames above ``threshold`` into ``(start, end, peak)``."""
    spans = []
    current = None
    for probability, bounds in zip(probabilities, ranges):
        start_ms, end_ms = bounds
        if float(probability) >= threshold:
            if current is None:
                current = [start_ms, end_ms, float(probability)]
            else:
                current[1] = end_ms
                current[2] = max(current[2], float(probability))
        elif current is not None:
            spans.append((current[0], current[1], current[2]))
            current = None
    if current is not None:
        spans.append((current[0], current[1], current[2]))
    return spans


# ---------------------------------------------------------------------------
# Zipformer tagging windows
# ---------------------------------------------------------------------------


def window_5s(samples, sr=SAMPLE_RATE):
    """Split into non-overlapping 5 s windows for Zipformer tagging.

    The last window is zero-padded to the full 5 s so the model always sees a
    fixed shape, but its emitted ``(start_ms, end_ms)`` is clamped to the real
    signal length. Returns a list of ``(start_ms, end_ms, padded_window)``.
    """
    np = _numpy()
    window_samples = TAGGING_WINDOW_SECONDS * sr
    windows = []
    total = len(samples)
    for offset in range(0, total, window_samples):
        chunk = samples[offset : offset + window_samples]
        real_end = min(offset + len(chunk), total)
        if len(chunk) < window_samples:
            padded = np.pad(chunk, (0, window_samples - len(chunk)))
        else:
            padded = chunk
        windows.append(
            (offset * 1000 // sr, real_end * 1000 // sr, padded)
        )
    return windows


# ---------------------------------------------------------------------------
# CAM++ fbank frontend
# ---------------------------------------------------------------------------


def apply_fbank_options(opts):
    """Apply :data:`FBANK_OPTIONS` to a kaldi_native_fbank ``FbankOptions``."""
    opts.frame_opts.samp_freq = FBANK_OPTIONS["samp_freq"]
    opts.frame_opts.frame_length_ms = FBANK_OPTIONS["frame_length_ms"]
    opts.frame_opts.frame_shift_ms = FBANK_OPTIONS["frame_shift_ms"]
    opts.frame_opts.snip_edges = FBANK_OPTIONS["snip_edges"]
    opts.frame_opts.dither = FBANK_OPTIONS["dither"]
    opts.mel_opts.num_bins = FBANK_OPTIONS["num_bins"]
    opts.mel_opts.low_freq = FBANK_OPTIONS["low_freq"]
    opts.mel_opts.high_freq = FBANK_OPTIONS["high_freq"]
    opts.energy_floor = FBANK_OPTIONS["energy_floor"]
    opts.use_log_fbank = FBANK_OPTIONS["use_log_fbank"]
    opts.use_power = FBANK_OPTIONS["use_power"]
    return opts


def cepstral_mean_normalize(frames):
    """Per-bin mean subtraction over all frames of one span (pure Python)."""
    if not frames:
        return []
    width = len(frames[0])
    count = float(len(frames))
    means = [math.fsum(row[bin] for row in frames) / count for bin in range(width)]
    return [[row[bin] - means[bin] for bin in range(width)] for row in frames]


def fbank_features(samples, backend="auto"):
    """Compute the CAM++ fbank features of one span, CMN-normalised.

    ``backend`` selects ``knf`` (kaldi-native-fbank, the PoC-verified real
    path), ``numpy`` (pure numpy fallback with the same constants) or
    ``python`` (stdlib fallback used by hermetic protocol tests). ``auto``
    prefers knf, then numpy, then the python fallback. Returns
    ``list[list[float]]`` with per-bin zero mean over the span's frames, or
    ``[]`` when the span is shorter than one 25 ms frame.
    """
    if backend == "auto":
        try:
            import kaldi_native_fbank  # noqa: F401
            backend = "knf"
        except Exception:
            try:
                import numpy  # noqa: F401
                backend = "numpy"
            except Exception:
                backend = "python"
    if backend == "knf":
        frames = _fbank_frames_knf(samples)
    elif backend == "numpy":
        frames = _fbank_frames(samples, _spectrum_numpy)
    elif backend == "python":
        frames = _fbank_frames(samples, _spectrum_python)
    else:
        raise AdapterError("audio_model_unavailable", False, "unknown fbank backend")
    return cepstral_mean_normalize(frames)


def _fbank_frames_knf(samples):
    np = _numpy()
    import kaldi_native_fbank as knf

    fbank = knf.OnlineFbank(apply_fbank_options(knf.FbankOptions()))
    fbank.accept_waveform(SAMPLE_RATE, np.asarray(samples, dtype="float32"))
    fbank.input_finished()
    ready = fbank.num_frames_ready
    return [list(fbank.get_frame(index)) for index in range(ready)]


def _spectrum_numpy(windowed):
    np = _numpy()
    spectrum = np.fft.rfft(np.asarray(windowed, dtype="float32"), n=_FBANK_FFT)
    power = np.abs(spectrum)
    return (power * power).tolist()


def _fbank_frames(samples, spectrum):
    """Shared pure-Python framing pipeline: window, normalise, mel, log."""
    values = [float(value) for value in samples]
    frame_length = _FBANK_FRAME_LENGTH
    if len(values) < frame_length:
        return []
    window = _povey_window(frame_length)
    bank = _mel_filterbank()
    frames = []
    count = (len(values) - frame_length) // _FBANK_FRAME_SHIFT + 1  # snip_edges
    for index in range(count):
        start = index * _FBANK_FRAME_SHIFT
        piece = values[start : start + frame_length]
        mean = math.fsum(piece) / frame_length
        piece = [value - mean for value in piece]
        piece = [piece[0]] + [
            piece[i] - _FBANK_PREEMPHASIS * piece[i - 1] for i in range(1, frame_length)
        ]
        windowed = [w * v for w, v in zip(window, piece)]
        power = spectrum(windowed)
        energies = [math.fsum(p * w for p, w in zip(power, row)) for row in bank]
        frames.append([math.log(max(e, _FBANK_FLOOR)) for e in energies])
    return frames


def _povey_window(length):
    cached = _POVEY_CACHE.get(length)
    if cached is None:
        cached = [
            (0.5 - 0.5 * math.cos(2.0 * math.pi * i / (length - 1))) ** 0.85
            for i in range(length)
        ]
        _POVEY_CACHE[length] = cached
    return cached


_POVEY_CACHE = {}


def _mel_filterbank():
    cached = _MEL_CACHE.get(_FBANK_FFT)
    if cached is not None:
        return cached
    num_bins = FBANK_OPTIONS["num_bins"]
    low = FBANK_OPTIONS["low_freq"]
    # high_freq=0 means "up to Nyquist" in Kaldi.
    high = SAMPLE_RATE / 2.0

    def hz_to_mel(frequency):
        return 1127.0 * math.log(1.0 + frequency / 700.0)

    def mel_to_hz(mel):
        return 700.0 * (math.exp(mel / 1127.0) - 1.0)

    mel_low = hz_to_mel(low)
    mel_high = hz_to_mel(high)
    points = [
        mel_to_hz(mel_low + (mel_high - mel_low) * i / (num_bins + 1))
        for i in range(num_bins + 2)
    ]
    freqs = [k * SAMPLE_RATE / _FBANK_FFT for k in range(_FBANK_FFT // 2 + 1)]
    bank = []
    for bin_index in range(num_bins):
        left, center, right = points[bin_index], points[bin_index + 1], points[bin_index + 2]
        row = []
        for frequency in freqs:
            if center > left and left <= frequency <= center:
                weight = (frequency - left) / (center - left)
            elif right > center and center < frequency <= right:
                weight = (right - frequency) / (right - center)
            else:
                weight = 0.0
            row.append(weight)
        bank.append(row)
    _MEL_CACHE[_FBANK_FFT] = bank
    return bank


_MEL_CACHE = {}

_DFT_TABLES = {}


def _spectrum_python(windowed):
    """Pure-stdlib 512-point power spectrum (hermetic tests / no numpy)."""
    padded = list(windowed) + [0.0] * (_FBANK_FFT - len(windowed))
    tables = _dft_tables(_FBANK_FFT)
    power = []
    for cos_row, sin_row in zip(*tables):
        real = 0.0
        imaginary = 0.0
        for value, cosine, sine in zip(padded, cos_row, sin_row):
            if value:
                real += value * cosine
                imaginary -= value * sine
        power.append(real * real + imaginary * imaginary)
    return power


def _dft_tables(n_fft):
    tables = _DFT_TABLES.get(n_fft)
    if tables is None:
        half = n_fft // 2 + 1
        cosines = []
        sines = []
        for k in range(half):
            angle = 2.0 * math.pi * k / n_fft
            cosines.append([math.cos(angle * n) for n in range(n_fft)])
            sines.append([math.sin(angle * n) for n in range(n_fft)])
        tables = (cosines, sines)
        _DFT_TABLES[n_fft] = tables
    return tables


# ---------------------------------------------------------------------------
# vector helpers
# ---------------------------------------------------------------------------


def l2_normalize(vector):
    """L2-normalise a sequence; ``None`` for empty/non-finite/zero-norm input."""
    if vector is None:
        return None
    values = [float(value) for value in vector]
    if not values:
        return None
    total = 0.0
    for value in values:
        if not math.isfinite(value):
            return None
        total += value * value
    norm = math.sqrt(total)
    if not math.isfinite(norm) or norm <= 0.0:
        return None
    return [value / norm for value in values]


def cluster_embedding(vectors_with_durations):
    """Duration-weighted mean of vectors, then L2 normalisation.

    ``vectors_with_durations`` is an iterable of ``(vector, duration_seconds)``.
    Invalid entries (empty, non-finite vectors or non-positive/non-finite
    durations) are skipped; ``None`` when no valid entry remains.
    """
    dimensions = None
    accumulator = None
    weight = 0.0
    for vector, duration in vectors_with_durations:
        if vector is None:
            continue
        values = [float(value) for value in vector]
        if not values or not all(math.isfinite(value) for value in values):
            continue
        if duration is None:
            continue
        seconds = float(duration)
        if not math.isfinite(seconds) or seconds <= 0.0:
            continue
        if dimensions is None:
            dimensions = len(values)
            accumulator = [0.0] * dimensions
        elif len(values) != dimensions:
            continue
        accumulator = [acc + seconds * value for acc, value in zip(accumulator, values)]
        weight += seconds
    if weight <= 0.0:
        return None
    return l2_normalize([value / weight for value in accumulator])


def event_id(run_id, index, event_type, start_ms, end_ms):
    """Deterministic SHA-256 prefix over the run's stable event coordinates."""
    canonical = "\x00".join(
        [
            str(run_id),
            str(int(index)),
            str(event_type),
            str(int(start_ms)),
            str(int(end_ms)),
        ]
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()[:16]


# ---------------------------------------------------------------------------
# validation (mirrors internal/audioanalysis/types.go)
# ---------------------------------------------------------------------------


def _hash_ok(value):
    return isinstance(value, str) and bool(_HEX64.fullmatch(value))


def _finite_float(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return math.isfinite(float(value))


def validate_request(request):
    """Mirror ``Request.Validate``; raises ``audio_input_invalid`` on violation."""
    def bad(reason):
        return AdapterError("audio_input_invalid", False, reason)

    if request.get("schema_version") != CONTRACT_SCHEMA:
        raise bad("schema_version must be " + CONTRACT_SCHEMA)
    if not str(request.get("run_id") or "").strip() or not str(
        request.get("recording_id") or ""
    ).strip():
        raise bad("run_id and recording_id are required")
    for field in ("source_audio_sha256", "input_sha256", "config_sha256"):
        if not _hash_ok(request.get(field)):
            raise bad(field + " must be 64 hex characters")
    if request.get("capability") not in CAPABILITIES:
        raise bad("unknown capability")
    if not str(request.get("model_id") or "") or not str(request.get("model_revision") or ""):
        raise bad("model_id and model_revision are required")
    model_files = request.get("model_files_sha256")
    if not isinstance(model_files, dict) or not model_files:
        raise bad("model_files_sha256 must pin every model file")
    for name, digest in model_files.items():
        if not isinstance(name, str) or not name.strip() or not _hash_ok(digest):
            raise bad("model_files_sha256 entries must be name to 64 hex hash")
    start_ms = request.get("start_ms")
    end_ms = request.get("end_ms")
    if not _finite_int(start_ms) or not _finite_int(end_ms) or start_ms < 0 or end_ms <= start_ms:
        raise bad("start_ms/end_ms must satisfy 0 <= start < end")
    snapshot = request.get("source_snapshot_sha256")
    attachments = request.get("source_attachment_ids")
    attachments = list(attachments) if isinstance(attachments, list) else []
    if not snapshot:
        if attachments:
            raise bad("source_attachment_ids requires source_snapshot_sha256")
    elif not _hash_ok(snapshot):
        raise bad("source_snapshot_sha256 must be 64 hex characters")
    if request.get("capability") != "speaker" and attachments:
        raise bad("only speaker mapping runs may pin source attachments")


def _finite_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def validate_result(result, request, pcm_duration_ms):
    """Mirror ``Result.ValidateResult``; raises ``audio_result_invalid``."""
    def bad(reason):
        return AdapterError("audio_result_invalid", False, reason)

    if not isinstance(result, dict):
        raise bad("result must be a JSON object")
    if result.get("schema_version") != CONTRACT_SCHEMA or result.get("run_id") != request.get(
        "run_id"
    ):
        raise bad("result identity does not match the request")
    for field in (
        "recording_id",
        "source_audio_sha256",
        "input_sha256",
        "model_id",
        "model_revision",
        "capability",
        "config_sha256",
        "source_snapshot_sha256",
    ):
        if result.get(field) != request.get(field):
            raise bad(field + " does not match the request")
    request_revision = request.get("transcript_revision")
    result_revision = result.get("transcript_revision")
    if (result_revision is None) != (request_revision is None) or (
        result_revision is not None and result_revision != request_revision
    ):
        raise bad("transcript_revision does not match the request")
    result_attachments = result.get("source_attachment_ids")
    result_attachments = (
        list(result_attachments) if isinstance(result_attachments, list) else []
    )
    request_attachments = request.get("source_attachment_ids")
    request_attachments = (
        list(request_attachments) if isinstance(request_attachments, list) else []
    )
    if "\x00".join(result_attachments) != "\x00".join(request_attachments):
        raise bad("source_attachment_ids do not match the request")
    status = result.get("status")
    if status == "succeeded":
        if result.get("error") is not None:
            raise bad("succeeded result must not carry an error")
    elif status in ("failed", "invalidated"):
        if result.get("events"):
            raise bad("failed/invalidated result must not carry events")
    else:
        raise bad("status must be succeeded, failed or invalidated")
    events = result.get("events") or []
    seen = set()
    for event in events:
        if not isinstance(event, dict):
            raise bad("events must be JSON objects")
        identifier = event.get("id")
        if not identifier or identifier in seen:
            raise bad("event ids must be present and unique")
        seen.add(identifier)
        if event.get("event_type") not in EVENT_TYPES:
            raise bad("unknown event type")
        start_ms = event.get("start_ms")
        end_ms = event.get("end_ms")
        if (
            not _finite_int(start_ms)
            or not _finite_int(end_ms)
            or start_ms < 0
            or end_ms <= start_ms
            or end_ms > pcm_duration_ms
        ):
            raise bad("event time is outside the analysed PCM")
        confidence = event.get("confidence")
        if confidence is not None:
            if not _finite_float(confidence) or not 0.0 <= float(confidence) <= 1.0:
                raise bad("confidence must be within [0,1] or null")
        if event.get("event_type") == "speaker_identity_proposal":
            payload = event.get("payload") or {}
            if payload.get("status") != "proposed":
                raise bad("identity proposals must carry status=proposed")


def build_result(request, status, events, error=None, provenance=None):
    """Assemble a Result document echoing the request identity verbatim."""
    if provenance is None:
        provenance = {
            "adapter_version": ADAPTER_VERSION,
            "frontend_id": "",
            "device": "",
            "input_sha256": request.get("input_sha256") or "",
            "model_files_sha256": _echo_model_files(request),
            "parent_run_ids": [],
            "source_snapshot_sha256": request.get("source_snapshot_sha256") or "",
        }
    result = {
        "schema_version": CONTRACT_SCHEMA,
        "run_id": request.get("run_id") or "",
        "recording_id": request.get("recording_id") or "",
        "source_audio_sha256": request.get("source_audio_sha256") or "",
        "input_sha256": request.get("input_sha256") or "",
        "transcript_revision": request.get("transcript_revision"),
        "model_id": request.get("model_id") or "",
        "model_revision": request.get("model_revision") or "",
        "model_files_sha256": _echo_model_files(request),
        "capability": request.get("capability") or "",
        "start_ms": request.get("start_ms") if _finite_int(request.get("start_ms")) else 0,
        "end_ms": request.get("end_ms") if _finite_int(request.get("end_ms")) else 0,
        "config_sha256": request.get("config_sha256") or "",
        "source_attachment_ids": _echo_attachments(request),
        "source_snapshot_sha256": request.get("source_snapshot_sha256") or "",
        "status": status,
        "events": list(events or []),
        "provenance": provenance,
    }
    if error is not None:
        result["error"] = {
            "code": error.code,
            "retryable": bool(error.retryable),
            "action": error.action or "",
        }
    return result


def _echo_model_files(request):
    files = request.get("model_files_sha256")
    return dict(files) if isinstance(files, dict) else {}


def _echo_attachments(request):
    attachments = request.get("source_attachment_ids")
    return list(attachments) if isinstance(attachments, list) else []


# ---------------------------------------------------------------------------
# configuration and model pin validation
# ---------------------------------------------------------------------------


def validate_config(config):
    """Structural validation of an ``audio-analysis-config-v1`` document."""
    if not isinstance(config, dict):
        raise AdapterError(
            "audio_model_unavailable", False, "the audio analysis configuration must be an object"
        )
    if config.get("schema_version") != CONFIG_SCHEMA:
        raise AdapterError(
            "audio_model_unavailable", False, "config schema_version must be " + CONFIG_SCHEMA
        )
    capabilities = config.get("capabilities")
    if capabilities is not None and not isinstance(capabilities, dict):
        raise AdapterError(
            "audio_model_unavailable", False, "config capabilities must be an object"
        )
    for key in ("python", "runner", "gpu_lock"):
        value = config.get(key)
        if value is not None and not isinstance(value, str):
            raise AdapterError(
                "audio_model_unavailable", False, "config " + key + " must be a string"
            )
    for key in ("lock_timeout_seconds", "load_timeout_seconds", "run_timeout_seconds"):
        value = config.get(key)
        if value is not None and (not _finite_int(value) or value <= 0):
            raise AdapterError(
                "audio_model_unavailable", False, "config " + key + " must be a positive integer"
            )


def resolve_capability(config, capability):
    """Verify one capability's pins; ``audio_disabled`` / ``audio_model_unavailable``."""
    validate_config(config)
    if capability not in CAPABILITIES:
        raise AdapterError("audio_input_invalid", False, "unknown capability")
    capabilities = config.get("capabilities") or {}
    cap = capabilities.get(capability)
    if not isinstance(cap, dict):
        raise AdapterError(
            "audio_disabled", False, "capability " + capability + " is not configured"
        )
    if cap.get("enabled") is not True:
        raise AdapterError(
            "audio_disabled", False, "capability " + capability + " is not enabled"
        )
    files = cap.get("files")
    if not isinstance(files, list) or not files:
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "capability " + capability + " pins no model files",
        )
    verified = []
    for entry in files:
        if not isinstance(entry, dict):
            raise AdapterError(
                "audio_model_unavailable", False, "model file entries must be objects"
            )
        path = entry.get("path")
        expected = entry.get("sha256")
        if not isinstance(path, str) or not path.strip():
            raise AdapterError(
                "audio_model_unavailable", False, "a model file entry has no path"
            )
        if not _hash_ok(expected):
            raise AdapterError(
                "audio_model_unavailable",
                False,
                "a pinned model file sha256 is not 64 hex characters",
            )
        if not os.path.isfile(path):
            raise AdapterError(
                "audio_model_unavailable", False, "a pinned model file is missing"
            )
        try:
            actual = sha256_file(path)
        except OSError as exc:
            raise AdapterError(
                "audio_model_unavailable", False, "a pinned model file could not be read"
            ) from exc
        if actual.lower() != expected.lower():
            raise AdapterError(
                "audio_model_unavailable",
                False,
                "a pinned model file hash does not match",
            )
        verified.append(path)
    device = cap.get("device") or "cpu"
    if not isinstance(device, str) or device.lower() not in ("cpu", "cuda"):
        raise AdapterError(
            "audio_model_unavailable", False, "capability device must be cpu or cuda"
        )
    frontend_id = cap.get("frontend_id") or ""
    if not isinstance(frontend_id, str):
        frontend_id = str(frontend_id)
    resolved = {
        "capability": capability,
        "device": device.lower(),
        "frontend_id": frontend_id,
        "model_id": cap.get("model_id") or "",
        "model_revision": cap.get("model_revision") or "",
        "files": verified,
    }
    threshold = cap.get("threshold")
    if threshold is not None:
        if not _finite_float(threshold) or not 0.0 < float(threshold) < 1.0:
            raise AdapterError(
                "audio_model_unavailable",
                False,
                "capability threshold must be a number within (0,1)",
            )
        resolved["threshold"] = float(threshold)
    span_seconds = cap.get("span_seconds")
    if span_seconds is not None:
        if not _finite_float(span_seconds) or not 0.05 <= float(span_seconds) <= 3600.0:
            raise AdapterError(
                "audio_model_unavailable",
                False,
                "capability span_seconds must be a number within [0.05, 3600]",
            )
        resolved["span_seconds"] = float(span_seconds)
    fbank_backend = cap.get("fbank_backend")
    if fbank_backend is not None:
        if fbank_backend not in ("auto", "knf", "numpy", "python"):
            raise AdapterError(
                "audio_model_unavailable",
                False,
                "capability fbank_backend must be auto, knf, numpy or python",
            )
        resolved["fbank_backend"] = fbank_backend
    return resolved


# ---------------------------------------------------------------------------
# capability inference
# ---------------------------------------------------------------------------


def _require_cpu(resolved, capability):
    if resolved["device"] != "cpu":
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "capability " + capability + " is pinned to the CPU build; configure device cpu",
        )
    return "cpu"


def _single_onnx_file(resolved):
    onnx = [path for path in resolved["files"] if path.lower().endswith(".onnx")]
    if not onnx:
        raise AdapterError(
            "audio_model_unavailable", False, "the capability pins no .onnx model file"
        )
    if len(onnx) > 1:
        raise AdapterError(
            "audio_model_unavailable", False, "the capability pins more than one .onnx model file"
        )
    return onnx[0]


def _finite_or_error(value, code, retryable, action):
    value = float(value)
    if not math.isfinite(value):
        raise AdapterError(code, retryable, action)
    return value


def _onnx_session(model_path, capability):
    try:
        import onnxruntime as ort
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "onnxruntime is not installed for the " + capability + " capability",
        ) from exc
    options = ort.SessionOptions()
    options.intra_op_num_threads = 1
    options.inter_op_num_threads = 1
    options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    try:
        return ort.InferenceSession(
            model_path, sess_options=options, providers=["CPUExecutionProvider"]
        )
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "the pinned " + capability + " model could not be loaded",
        ) from exc


def _onnx_io_names(session, wanted_input, wanted_output):
    inputs = session.get_inputs()
    outputs = session.get_outputs()
    input_name = wanted_input if any(i.name == wanted_input for i in inputs) else inputs[0].name
    output_name = (
        wanted_output if any(o.name == wanted_output for o in outputs) else outputs[0].name
    )
    return input_name, output_name


def run_vad(request, samples, resolved):
    """Silero VAD on the analysed window; speech activity evidence only.

    The threshold (default 0.5, the PoC value) is uncalibrated and exists for
    visualising activity intervals; ASR always processes the original timeline
    and this capability never removes or gates ASR input. Each merged active
    span carries the maximum frame probability as its confidence and documents
    the threshold and its uncalibrated status in the payload.
    """
    device = _require_cpu(resolved, "vad")
    model_path = _single_onnx_file(resolved)
    session = _onnx_session(model_path, "vad")
    np = _numpy()
    threshold = resolved.get("threshold") or VAD_DEFAULT_THRESHOLD
    state = np.zeros((2, 1, 128), dtype="float32")
    sr = np.asarray(SAMPLE_RATE, dtype="int64")
    holder = {"state": state}

    def scorer(model_input):
        feed_input = np.asarray(model_input, dtype="float32").reshape(1, -1)
        output, state_next = session.run(
            None, {"input": feed_input, "state": holder["state"], "sr": sr}
        )
        holder["state"] = state_next
        return float(output.reshape(-1)[0])

    try:
        probabilities, ranges = frame_signal(samples, VAD_FRAME, VAD_CONTEXT, scorer)
    except AdapterError:
        raise
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable", True, "the pinned VAD model failed during inference"
        ) from exc
    events = []
    for index, (start_ms, end_ms, confidence) in enumerate(
        active_spans(probabilities, ranges, threshold)
    ):
        events.append(
            {
                "id": event_id(request["run_id"], index, "speech_activity", start_ms, end_ms),
                "event_type": "speech_activity",
                "start_ms": start_ms,
                "end_ms": end_ms,
                "confidence": confidence,
                "payload": {"threshold": threshold, "uncalibrated": True},
            }
        )
    return RunOutcome(events, device)


def _tagging_files(resolved):
    model = None
    labels = None
    for path in resolved["files"]:
        name = os.path.basename(path).lower()
        if name.endswith(".onnx"):
            if model is not None:
                raise AdapterError(
                    "audio_model_unavailable",
                    False,
                    "the tagging capability pins more than one .onnx model file",
                )
            model = path
        elif name.endswith(".csv"):
            labels = path
    if model is None or labels is None:
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "the tagging capability pins no model.onnx and class label csv",
        )
    return model, labels


def run_tagging(request, samples, resolved):
    """Zipformer acoustic tagging on fixed 5 s windows.

    Every window contributes one ``acoustic_event`` carrying the top label,
    its raw score and the top-k list. ``source_kind`` stays ``unknown``:
    music, broadcast or overlapping audio can never prove a real person.
    """
    device = _require_cpu(resolved, "tagging")
    model_path, labels_path = _tagging_files(resolved)
    try:
        import sherpa_onnx
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "sherpa-onnx is not installed for the tagging capability",
        ) from exc
    try:
        config = sherpa_onnx.AudioTaggingConfig(
            model=sherpa_onnx.AudioTaggingModelConfig(
                zipformer=sherpa_onnx.OfflineZipformerAudioTaggingModelConfig(
                    model=model_path
                ),
                num_threads=1,
                provider="cpu",
            ),
            labels=labels_path,
            top_k=TAGGING_TOP_K,
        )
        tagging = sherpa_onnx.AudioTagging(config)
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable", False, "the pinned tagging model could not be loaded"
        ) from exc
    with open(labels_path, "r", encoding="utf-8") as handle:
        import csv

        labels = [
            row.get("display_name") or ""
            for row in csv.DictReader(handle)
        ]
    run_id = request["run_id"]
    events = []
    try:
        for index, (start_ms, end_ms, window) in enumerate(window_5s(samples)):
            stream = tagging.create_stream()
            stream.accept_waveform(SAMPLE_RATE, window)
            found = tagging.compute(stream, top_k=TAGGING_TOP_K)
            if not found:
                continue
            top = found[0]
            score = _finite_or_error(
                top.prob,
                "audio_result_invalid",
                False,
                "the tagging model produced a non-finite score",
            )
            payload = {
                "label": _label_of(labels, top.index),
                "score": score,
                "source_kind": "unknown",
                "top_k": [
                    {"label": _label_of(labels, event.index), "score": float(event.prob)}
                    for event in found[:TAGGING_TOP_K]
                ],
            }
            events.append(
                {
                    "id": event_id(run_id, index, "acoustic_event", start_ms, end_ms),
                    "event_type": "acoustic_event",
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "confidence": score,
                    "payload": payload,
                }
            )
    except AdapterError:
        raise
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable", True, "the pinned tagging model failed during inference"
        ) from exc
    return RunOutcome(events, device)


def _label_of(labels, index):
    index = int(index)
    if 0 <= index < len(labels):
        return labels[index]
    return str(index)


def _common_parent(paths):
    directories = [os.path.dirname(os.path.abspath(path)) for path in paths]
    try:
        return os.path.commonpath(directories)
    except ValueError:
        return directories[0]


def run_diarization(request, samples, resolved):
    """Community-1 speaker diarization with run-local labels.

    Overlapping regions are marked ``ambiguous`` and are never used for
    cross-recording identity. Unknown times produce no events instead of
    zero-padded placeholders.
    """
    device = resolved["device"]
    try:
        import torch
        from pyannote.audio import Pipeline
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "pyannote.audio and torch are not installed for the diarization capability",
        ) from exc
    if device == "cuda" and not torch.cuda.is_available():
        raise AdapterError(
            "audio_model_unavailable",
            False,
            "cuda is configured for diarization but no CUDA device is available",
        )
    np = _numpy()
    pipeline_dir = _common_parent(resolved["files"])
    actual = "cpu"
    original_load = torch.load
    try:
        # pyannote.audio 4.0.0 predates torch's weights_only default flip; the
        # local checkpoint is hash-verified in this isolated subprocess, so the
        # opt-out is scoped here and restored afterwards.
        def _load(*args, **kwargs):
            kwargs["weights_only"] = False
            return original_load(*args, **kwargs)

        torch.load = _load
        try:
            pipeline = Pipeline.from_pretrained(str(pipeline_dir))
        except Exception as exc:
            raise AdapterError(
                "audio_model_unavailable",
                False,
                "the pinned diarization pipeline could not be loaded",
            ) from exc
        if device == "cuda":
            pipeline.to(torch.device("cuda"))
            actual = "cuda"
        # batch_size=1 is a deployment parameter for the 8 GiB card, not an
        # accuracy parameter (PoC run_diarization.py).
        if hasattr(pipeline, "segmentation_batch_size"):
            pipeline.segmentation_batch_size = 1
        if hasattr(pipeline, "embedding_batch_size"):
            pipeline.embedding_batch_size = 1
        waveform = torch.from_numpy(np.asarray(samples, dtype="float32")).unsqueeze(0)
        try:
            with torch.no_grad():
                output = pipeline({"waveform": waveform, "sample_rate": SAMPLE_RATE})
        except Exception as exc:
            raise AdapterError(
                "audio_model_unavailable",
                True,
                "the pinned diarization pipeline failed during inference",
            ) from exc
        annotation = getattr(output, "speaker_diarization", output)
        raw = [
            (float(segment.start), float(segment.end), str(label))
            for segment, _track, label in annotation.itertracks(yield_label=True)
        ]
    finally:
        torch.load = original_load
    duration_ms = len(samples) * 1000 // SAMPLE_RATE
    clamped = []
    for start_s, end_s, label in raw:
        if not (math.isfinite(start_s) and math.isfinite(end_s)):
            continue
        start_ms = max(0, int(round(start_s * 1000)))
        end_ms = min(duration_ms, int(round(end_s * 1000)))
        if end_ms <= start_ms:
            continue
        clamped.append((start_ms, end_ms, label))
    overlaps = overlap_flags([(start, end) for start, end, _ in clamped])
    events = []
    for index, (start_ms, end_ms, label) in enumerate(clamped):
        events.append(
            {
                "id": event_id(request["run_id"], index, "speaker_segment", start_ms, end_ms),
                "event_type": "speaker_segment",
                "start_ms": start_ms,
                "end_ms": end_ms,
                "confidence": None,
                "payload": {
                    "speaker": label,
                    "source_kind": "ambiguous" if overlaps[index] else "unknown",
                },
            }
        )
    return RunOutcome(events, actual)


def overlap_flags(intervals):
    """Pairwise overlap detection; ``True`` for every member of an overlap."""
    flags = [False] * len(intervals)
    for i in range(len(intervals)):
        start_i, end_i = intervals[i]
        for j in range(i + 1, len(intervals)):
            start_j, end_j = intervals[j]
            if start_i < end_j and start_j < end_i:
                flags[i] = True
                flags[j] = True
    return flags


def speaker_span_bounds(total_samples, span_samples):
    """Non-overlapping speaker spans; the final span may be shorter."""
    if span_samples <= 0 or total_samples <= 0:
        return []
    return [
        (start, min(start + span_samples, total_samples))
        for start in range(0, total_samples, span_samples)
    ]


def speaker_embedding_from_features(session, input_name, frames):
    """One CAM++ voiceprint from already-extracted fbank features."""
    np = _numpy()
    feats = np.asarray(frames, dtype="float32")
    try:
        raw = session.run(None, {input_name: feats[None, :, :]})[0][0]
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable",
            True,
            "the pinned speaker model failed during inference",
        ) from exc
    return l2_normalize(raw)


def speaker_span_embedding(session, input_name, samples, fbank_backend="auto"):
    """One CAM++ voiceprint for an exact sample span.

    This is the inference path shared with the adapter's window loop: fbank
    features, one ONNX forward pass and L2 normalisation. It returns the
    normalised 192-dim vector, or None when the frontend produced no frames or
    the network returned an unusable vector. AdapterError keeps its meaning, so
    a real inference failure is never reported as "no speaker present".
    """
    frames = fbank_features(samples, backend=fbank_backend)
    if not frames:
        return None
    return speaker_embedding_from_features(session, input_name, frames)


def run_speaker(request, samples, resolved):
    """CAM++ voiceprint embeddings for non-overlapping spans.

    Each span is one independent inference with its own per-span CMN; the
    192-dim L2-normalised vectors travel only through the embeddings side
    channel. Identity proposals are deliberately not produced here: they need
    confirmed, current reference bindings that only the Go binding service can
    see, so a bare run never asserts a real person.
    """
    device = _require_cpu(resolved, "speaker")
    model_path = _single_onnx_file(resolved)
    session = _onnx_session(model_path, "speaker")
    input_name, _output_name = _onnx_io_names(session, "x", "embedding")
    span_seconds = resolved.get("span_seconds") or SPEAKER_DEFAULT_SPAN_SECONDS
    fbank_backend = resolved.get("fbank_backend") or "auto"
    span_samples = max(int(round(span_seconds * SAMPLE_RATE)), 1)
    bounds = speaker_span_bounds(len(samples), span_samples)
    if not bounds:
        raise AdapterError(
            "audio_input_invalid", False, "the analysed window is too short for a speaker embedding"
        )
    run_id = request["run_id"]
    events = []
    embeddings = {}
    saw_frames = False
    try:
        for index, (start, end) in enumerate(bounds):
            frames = fbank_features(samples[start:end], backend=fbank_backend)
            if not frames:
                continue
            saw_frames = True
            normalized = speaker_embedding_from_features(session, input_name, frames)
            if normalized is None:
                continue
            start_ms = start * 1000 // SAMPLE_RATE
            end_ms = end * 1000 // SAMPLE_RATE
            identifier = event_id(run_id, index, "speaker_segment", start_ms, end_ms)
            events.append(
                {
                    "id": identifier,
                    "event_type": "speaker_segment",
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "confidence": None,
                    "payload": {
                        "source_kind": "unknown",
                        "span_seconds": span_seconds,
                    },
                }
            )
            embeddings[identifier] = normalized
    except AdapterError:
        raise
    except Exception as exc:
        raise AdapterError(
            "audio_model_unavailable", True, "the pinned speaker model failed during inference"
        ) from exc
    if not events:
        if not saw_frames:
            raise AdapterError(
                "audio_input_invalid",
                False,
                "the analysed window is too short for the speaker frontend",
            )
        raise AdapterError(
            "audio_result_invalid",
            False,
            "the pinned speaker model produced no valid voiceprint for this window",
        )
    return RunOutcome(events, device, embeddings)
