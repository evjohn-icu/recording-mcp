#!/usr/bin/env python3
"""Human gold authoring and offline re-scoring for the v0.4 audio harness.

Three steps, each writing into a new private directory so nothing is
overwritten and the frozen gold never changes:

  prepare   copy one explicitly authorized recording into an anonymous file,
            emit a local (offline) annotation page and a draft gold document.
  finalize  freeze the human-reviewed draft into an acceptance manifest with
            pinned hashes; it refuses a draft the operator did not sign.
  score     re-score frozen predictions against frozen gold and optional human
            adjudications. It calls no service, model or Go test.

Nothing here scans for audio: every path is explicit, and a missing input is
reported as NOT_RUN/BLOCKED with a reason instead of a fabricated pass.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import stat
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import audio_acceptance as acceptance  # noqa: E402
import audio_acceptance_metrics as metrics  # noqa: E402

PREDICTIONS_SCHEMA = "context-engine-predictions-v1"
ADJUDICATIONS_SCHEMA = "context-engine-adjudications-v1"
DRAFT_SCHEMA = "context-engine-gold-draft-v1"
REVIEW_SCHEMA = "context-engine-gold-review-v1"
REVIEWED_SET_SCHEMA = "context-engine-reviewed-set-v1"
PRODUCTION_THRESHOLDS_SCHEMA = "context-engine-production-thresholds-v1"
PRODUCTION_GATE_SCHEMA = "context-engine-production-gate-v1"
PRODUCTION_METRICS = ("cer_max", "wer_max", "der_max", "event_precision_min", "event_recall_min",
                      "eer_max", "evidence_accuracy_min", "semantic_accuracy_min",
                      "decision_precision_min")
DURATION_TOLERANCE_MS = 250


class ReviewError(RuntimeError):
    """A review step cannot continue honestly."""

    def __init__(self, code: str, detail: str):
        super().__init__(f"{code}: {detail}")
        self.code = code
        self.detail = detail


# --- canonical digests ------------------------------------------------------

def canonical_json(payload) -> str:
    return json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def _normalized_claim(claim: dict) -> dict:
    return {"claim_id": claim.get("claim_id"), "kind": claim.get("kind"), "text": claim.get("text")}


def _normalized_ref(ref: dict) -> dict:
    return {"ref_id": ref.get("ref_id"), "turn_id": ref.get("turn_id"),
            "conversation_id": ref.get("conversation_id"), "recording_id": ref.get("recording_id"),
            "transcript_revision": ref.get("transcript_revision"),
            "start_ms": ref.get("start_ms"), "end_ms": ref.get("end_ms")}


def prediction_digest(bundle: dict) -> str:
    """Digest over the scored claims and query refs only.

    Timings, resource samples, stage status and the digest itself are excluded
    so a re-run with different latency still matches the same adjudication.
    """
    queries = []
    for output in bundle.get("query_outputs") or []:
        entry = {"query_id": output.get("query_id")}
        for source in ("web", "mcp"):
            payload = output.get(source) or {}
            entry[source] = [_normalized_ref(ref) for ref in payload.get("refs") or []
                             if isinstance(ref, dict)]
        queries.append(entry)
    queries.sort(key=lambda item: str(item.get("query_id")))
    claims = sorted((_normalized_claim(claim) for claim in bundle.get("claims") or []
                     if isinstance(claim, dict)), key=lambda item: str(item.get("claim_id")))
    return hashlib.sha256(canonical_json({"claims": claims, "queries": queries}).encode("utf-8")).hexdigest()


# --- file helpers -----------------------------------------------------------

def _private_dir(path: Path) -> None:
    if path.exists():
        raise ReviewError("output_exists", f"refusing to write into existing path: {path}")
    path.mkdir(parents=True)
    os.chmod(path, 0o700)


def _write_private(path: Path, text: str) -> None:
    if path.exists():
        raise ReviewError("output_exists", f"refusing to overwrite: {path}")
    path.parent.mkdir(parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    handle = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(handle, "w", encoding="utf-8") as sink:
        sink.write(text)


def _inside(root: Path, candidate: Path) -> bool:
    try:
        candidate.resolve().relative_to(root.resolve())
        return True
    except ValueError:
        return False


def _wav_parameters(path: Path):
    """(channels, sample_width, sample_rate) for a RIFF/WAVE file, else None."""
    import wave
    try:
        with wave.open(str(path), "rb") as stream:
            return stream.getnchannels(), stream.getsampwidth(), stream.getframerate()
    except (wave.Error, OSError, EOFError):
        return None


def _audio_duration_ms(path: Path) -> int | None:
    import wave
    try:
        with wave.open(str(path), "rb") as stream:
            rate = stream.getframerate()
            if rate > 0:
                return int(round(stream.getnframes() * 1000.0 / rate))
    except (wave.Error, OSError, EOFError):
        pass
    try:
        proc = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration",
                               "-of", "default=noprint_wrappers=1:nokey=1", str(path)],
                              capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if proc.returncode != 0:
        return None
    try:
        return int(round(float(proc.stdout.strip()) * 1000))
    except ValueError:
        return None


# --- prepare ----------------------------------------------------------------

ANNOTATION_PAGE = """<!doctype html>
<html lang="zh">
<meta charset="utf-8">
<title>录音金标标注</title>
<style>
body{font-family:system-ui,sans-serif;margin:2rem;max-width:60rem;line-height:1.5}
audio{width:100%}
table{border-collapse:collapse;margin:.5rem 0}
td,th{border:1px solid #888;padding:.2rem .5rem;font-variant-numeric:tabular-nums}
textarea{width:100%;min-height:7rem;font-family:ui-monospace,monospace}
label{display:block;margin:.5rem 0}
button{margin-right:.5rem}
.warn{color:#a40}
</style>
<h1>录音金标标注</h1>
<p class="warn">本页完全离线：不加载任何外部资源，不上传音频，不含真实身份信息。导出的
JSON 需要你本人复核后才可冻结。</p>
<audio id="player" controls preload="metadata" src="__AUDIO__"></audio>
<p>播放头：<span id="clock">0.000</span> s ／ 总长 <span id="total">?</span> s</p>
<label>类别
  <select id="category">
    <option value="">（请选择）</option>
    __CATEGORIES__
  </select>
</label>
<label>匿名标注人 <input id="annotator" placeholder="anon-1"></label>
<label>复核时间（带时区 ISO-8601）<input id="reviewed_at" placeholder="2026-11-13T10:00:00+08:00"></label>
<label><input type="checkbox" id="sign"> 我已逐条复核（勾选后导出的 provenance 才是 human）</label>
<h2>评分区间 scored_regions</h2>
<p>标记播放区间后加入列表；区间代表“这些时间段被评分”，区间外内容不评分。</p>
<button id="mark-start">标记起点</button>
<button id="mark-end">标记终点</button>
<button id="add-region">加入区间</button>
<table id="regions"><tr><th>#</th><th>start_ms</th><th>end_ms</th><th></th></tr></table>
<h2>参考转写（机器草稿，不是金标）</h2>
<pre id="reference"></pre>
<h2>结构化标注（JSON）</h2>
<p>可编辑 <code>transcript</code> / <code>acoustic_events</code> / <code>speaker_trials</code> /
<code>queries</code> / <code>claims</code>。省略键表示“未标注”，空数组表示“确认没有内容”。</p>
<textarea id="annotations"></textarea>
<button id="export">导出 gold-reviewed.json</button>
<pre id="status"></pre>
<script id="draft" type="application/json">__DRAFT__</script>
<script id="reference-draft" type="application/json">__REFERENCE__</script>
<script>
const draft = JSON.parse(document.getElementById("draft").textContent);
const reference = JSON.parse(document.getElementById("reference-draft").textContent);
const player = document.getElementById("player");
const regions = draft.scored_regions ? draft.scored_regions.slice() : [];
document.getElementById("annotations").value = JSON.stringify({
  transcript: draft.transcript, acoustic_events: draft.acoustic_events,
  speaker_trials: draft.speaker_trials, queries: draft.queries, claims: draft.claims
}, null, 2);
document.getElementById("reference").textContent = reference.text || "（未提供参考转写）";
let startMs = null, endMs = null;
const clock = () => Math.round(player.currentTime * 1000);
player.addEventListener("loadedmetadata", () => {
  document.getElementById("total").textContent = (player.duration || 0).toFixed(3);
  document.getElementById("clock").textContent = player.currentTime.toFixed(3);
});
player.addEventListener("timeupdate", () => {
  document.getElementById("clock").textContent = player.currentTime.toFixed(3);
});
document.getElementById("mark-start").onclick = () => { startMs = clock(); render(); };
document.getElementById("mark-end").onclick = () => { endMs = clock(); render(); };
function render() {
  const table = document.getElementById("regions");
  table.innerHTML = "<tr><th>#</th><th>start_ms</th><th>end_ms</th><th></th></tr>";
  regions.forEach((region, index) => {
    const row = table.insertRow();
    row.insertCell().textContent = index;
    row.insertCell().textContent = region.start_ms;
    row.insertCell().textContent = region.end_ms;
    const remove = document.createElement("button");
    remove.textContent = "删除";
    remove.onclick = () => { regions.splice(index, 1); render(); };
    row.insertCell().appendChild(remove);
  });
  document.getElementById("status").textContent =
    "起点 " + (startMs === null ? "-" : startMs) + "ms / 终点 " + (endMs === null ? "-" : endMs) + "ms";
}
document.getElementById("add-region").onclick = () => {
  if (startMs === null || endMs === null || endMs <= startMs) {
    document.getElementById("status").textContent = "区间无效：需要 start_ms < end_ms";
    return;
  }
  regions.push({start_ms: startMs, end_ms: endMs});
  startMs = null; endMs = null;
  render();
};
document.getElementById("export").onclick = () => {
  const parsed = JSON.parse(document.getElementById("annotations").value || "{}");
  const payload = Object.assign({}, parsed, {
    schema: "context-engine-gold-v1",
    provenance: document.getElementById("sign").checked ? "human" : "draft",
    sample_id: draft.sample_id,
    annotator: document.getElementById("annotator").value,
    reviewed_at: document.getElementById("reviewed_at").value,
    audio_duration_ms: draft.audio_duration_ms,
    audio_sha256: draft.audio_sha256,
    category: document.getElementById("category").value,
    scored_regions: regions
  });
  const blob = new Blob([JSON.stringify(payload, null, 2)], {type: "application/json"});
  const link = document.createElement("a");
  link.href = URL.createObjectURL(blob);
  link.download = "gold-reviewed.json";
  link.click();
  URL.revokeObjectURL(link.href);
};
render();
</script>
</html>
"""


def prepare(args: argparse.Namespace) -> dict:
    """Copy one explicitly authorized recording into an anonymous 16 kHz WAV."""
    audio = Path(args.audio)
    if not audio.is_absolute():
        audio = (Path.cwd() / audio).resolve()
    if audio.is_symlink() or not audio.is_file():
        raise ReviewError("audio_unusable", f"not a regular file: {audio}")
    out = Path(args.out)
    _private_dir(out)
    target = out / "audio.wav"
    if _wav_parameters(audio) == (1, 2, 16000):
        # Already mono 16 kHz 16-bit PCM: copy the bytes so the hash covers the
        # exact audio the harness will upload.
        with audio.open("rb") as source, target.open("xb") as sink:
            for block in iter(lambda: source.read(1 << 20), b""):
                sink.write(block)
    else:
        try:
            proc = subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-i", str(audio),
                                   "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", str(target)],
                                  capture_output=True, text=True, timeout=3600)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise ReviewError("audio_unusable", f"ffmpeg could not normalize the recording: {exc}") from None
        if proc.returncode != 0 or not target.is_file():
            raise ReviewError("audio_unusable",
                              f"ffmpeg conversion failed: {(proc.stderr or '').strip().splitlines()[-1:]}")
    os.chmod(target, 0o600)
    sha = acceptance.sha256_file(target)
    duration = int(args.duration_ms) if args.duration_ms else _audio_duration_ms(target)
    if not duration or duration <= 0:
        raise ReviewError("audio_duration_unknown",
                          "ffprobe could not read the duration; pass --duration-ms explicitly")
    draft = {
        "schema": DRAFT_SCHEMA,
        "sample_id": args.sample_id,
        "provenance": "draft",
        "category": "",
        "annotator": "",
        "reviewed_at": "",
        "audio_duration_ms": duration,
        "audio_sha256": sha,
        "audio_file": target.name,
        "scored_regions": [],
        "transcript": [],
        "acoustic_events": [],
        "queries": [],
        "claims": [],
        "note": ("draft only: finalize refuses provenance=draft, an empty annotator/reviewed_at, "
                 "an empty category or an empty scored_regions"),
    }
    if args.reference:
        reference_path = Path(args.reference)
        if reference_path.is_symlink() or not reference_path.is_file():
            raise ReviewError("reference_unusable", f"not a regular file: {reference_path}")
        reference = {"provenance": "machine", "text": reference_path.read_text(encoding="utf-8", errors="replace")}
    else:
        reference = {"provenance": "machine", "text": ""}
    _write_private(out / "gold-draft.json", json.dumps(draft, ensure_ascii=False, indent=2) + "\n")
    _write_private(out / "reference-draft.json", json.dumps(reference, ensure_ascii=False, indent=2) + "\n")
    page = (ANNOTATION_PAGE
            .replace("__AUDIO__", target.name)
            .replace("__CATEGORIES__", "".join(
                f'<option value="{category}">{category}</option>' for category in acceptance.CATEGORIES))
            .replace("__DRAFT__", json.dumps(draft, ensure_ascii=False).replace("</", "<\\/"))
            .replace("__REFERENCE__", json.dumps(reference, ensure_ascii=False).replace("</", "<\\/")))
    _write_private(out / "annotate.html", page)
    return {"status": "PREPARED", "root": str(out), "audio_sha256": sha,
            "audio_duration_ms": duration, "audio_bytes": target.stat().st_size,
            "draft": str(out / "gold-draft.json"), "page": str(out / "annotate.html"),
            "reference_included": bool(args.reference),
            "note": "audio was normalized to mono 16 kHz PCM WAV; the original file is untouched",
            "next": "open annotate.html, review the audio, export gold-reviewed.json, then run finalize"}


# --- finalize ---------------------------------------------------------------

def _safe_sample_id(sample_id) -> bool:
    """A sample id names a directory; it must never be a path."""
    if not isinstance(sample_id, str) or not sample_id or sample_id in (".", ".."):
        return False
    if "/" in sample_id or "\\" in sample_id or sample_id.startswith("."):
        return False
    return True


def _reviewed_document(draft_path: Path, payload: dict, authorization_reference: str) -> tuple[dict, Path]:
    """Validate one human-reviewed document and return (gold, audio source)."""
    if payload.get("provenance") != "human":
        raise ReviewError("draft_not_reviewed",
                          "the draft still has provenance=draft; the operator must export a human-reviewed document")
    if not str(authorization_reference or "").strip():
        raise ReviewError("unauthorized", "--authorization-reference is required")
    category = payload.get("category")
    if category not in acceptance.CATEGORIES:
        raise ReviewError("category_required", "the reviewed document must select one acceptance category")
    sample_id = payload.get("sample_id")
    if not _safe_sample_id(sample_id):
        raise ReviewError("invalid_gold", "the reviewed document has a missing or path-like sample_id")
    audio_duration = payload.get("audio_duration_ms")
    if not acceptance.require_int(audio_duration) or audio_duration <= 0:
        raise ReviewError("invalid_gold", "audio_duration_ms must be a positive integer")
    if not payload.get("scored_regions"):
        raise ReviewError("regions_required", "the reviewed document must mark at least one scored region")
    recorded_audio = str(payload.get("audio_file") or "audio.wav")
    if Path(recorded_audio).name != recorded_audio:
        raise ReviewError("audio_unusable", "the reviewed document names an audio file outside its own directory")
    audio_source = draft_path.parent / recorded_audio
    if audio_source.is_symlink() or not audio_source.is_file():
        raise ReviewError("audio_unusable", f"the draft's audio is missing: {audio_source}")
    if acceptance.sha256_file(audio_source) != payload.get("audio_sha256"):
        raise ReviewError("hash_mismatch", "the draft's audio hash does not match the prepared file")
    gold_payload = {key: value for key, value in payload.items()
                    if key not in ("draft_note", "note", "audio_file")}
    gold_payload["schema"] = acceptance.GOLD_SCHEMA
    gold_payload["provenance"] = "human"
    return gold_payload, audio_source


def _copy_sample(root: Path, gold_payload: dict, audio_source: Path,
                 audio_relative: str, gold_relative: str, authorization_reference: str) -> dict:
    audio_target = root / audio_relative
    audio_target.parent.mkdir(parents=True, exist_ok=True)
    with audio_source.open("rb") as source, audio_target.open("xb") as sink:
        for block in iter(lambda: source.read(1 << 20), b""):
            sink.write(block)
    os.chmod(audio_target, 0o600)
    audio_sha = acceptance.sha256_file(audio_target)
    if audio_sha != gold_payload.get("audio_sha256"):
        raise ReviewError("hash_mismatch", "the copied audio hash differs from the draft")
    gold_target = root / gold_relative
    _write_private(gold_target, json.dumps(gold_payload, ensure_ascii=False, indent=2) + "\n")
    return {
        "id": gold_payload["sample_id"],
        "category": gold_payload["category"],
        "audio_path": audio_relative,
        "audio_sha256": audio_sha,
        "gold_path": gold_relative,
        "gold_sha256": acceptance.sha256_file(gold_target),
        "authorization": {"allowed": True, "reference": authorization_reference},
    }


def _validate_gold_or_raise(gold_payload: dict, known_ids: set[str]) -> None:
    try:
        acceptance.validate_gold(gold_payload, gold_payload["sample_id"], known_ids)
    except acceptance.AcceptanceError as exc:
        raise ReviewError(exc.code, exc.detail) from None


def finalize(args: argparse.Namespace) -> dict:
    draft_path = Path(args.draft)
    payload = json.loads(draft_path.read_text(encoding="utf-8"))
    gold_payload, audio_source = _reviewed_document(draft_path, payload, args.authorization_reference)
    sample_id = gold_payload["sample_id"]
    _validate_gold_or_raise(gold_payload, {sample_id})
    manifest_path = Path(args.out)
    root = manifest_path.parent
    _private_dir(root)
    sample = _copy_sample(root, gold_payload, audio_source, "audio.wav",
                          f"gold/{sample_id}.json", args.authorization_reference)
    manifest = {"schema": acceptance.MANIFEST_SCHEMA, "created_at": _now_iso(), "samples": [sample]}
    _write_private(manifest_path, json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
    acceptance.load_manifest(manifest_path)
    return {"status": "FINALIZED", "manifest": str(manifest_path),
            "sample_id": sample_id, "category": sample["category"],
            "gold_sha256": sample["gold_sha256"], "audio_sha256": sample["audio_sha256"]}


def finalize_set(args: argparse.Namespace) -> dict:
    """Freeze a reviewed set whose cross-sample references all resolve inside it."""
    set_path = Path(args.drafts)
    document = json.loads(set_path.read_text(encoding="utf-8"))
    if document.get("schema") != REVIEWED_SET_SCHEMA:
        raise ReviewError("invalid_set", f"schema must be {REVIEWED_SET_SCHEMA}")
    items = document.get("samples")
    if not isinstance(items, list) or not items:
        raise ReviewError("invalid_set", "samples must be a non-empty array")
    loaded: list[dict] = []
    sample_ids: list[str] = []
    for index, item in enumerate(items):
        if not isinstance(item, dict):
            raise ReviewError("invalid_set", f"samples[{index}] must be an object")
        draft_path = Path(str(item.get("draft") or ""))
        if not str(draft_path) or str(draft_path) == ".":
            raise ReviewError("invalid_set", f"samples[{index}] needs an explicit draft path")
        if not draft_path.is_absolute():
            draft_path = set_path.parent / draft_path
        if draft_path.is_symlink() or not draft_path.is_file():
            raise ReviewError("audio_unusable", f"samples[{index}] draft is not a regular file: {draft_path}")
        payload = json.loads(draft_path.read_text(encoding="utf-8"))
        authorization = str(item.get("authorization_reference") or "")
        gold_payload, audio_source = _reviewed_document(draft_path, payload, authorization)
        sample_id = gold_payload["sample_id"]
        if sample_id in sample_ids:
            raise ReviewError("duplicate_id", f"sample_id {sample_id} appears more than once")
        sample_ids.append(sample_id)
        loaded.append({"gold": gold_payload, "audio": audio_source, "authorization": authorization})
    known_ids = set(sample_ids)
    seen_ids: dict[str, set[str]] = {"queries": set(), "claims": set(), "speaker_trials": set()}
    for entry in loaded:
        for key in seen_ids:
            for item in entry["gold"].get(key) or []:
                identifier = item.get("id") if isinstance(item, dict) else None
                if identifier in seen_ids[key]:
                    raise ReviewError("duplicate_id",
                                      f"{key} id {identifier!r} appears in more than one sample")
                seen_ids[key].add(identifier)
        _validate_gold_or_raise(entry["gold"], known_ids)
    categories = {entry["gold"]["category"] for entry in loaded}
    missing = [category for category in acceptance.CATEGORIES if category not in categories]
    if missing:
        raise ReviewError("category_required",
                          f"the reviewed set has no sample for: {', '.join(missing)}")
    coverage = eer_coverage({entry["gold"]["sample_id"]: entry["gold"] for entry in loaded})
    if coverage["status"] != "OK":
        raise ReviewError("speaker_evaluation_incomplete",
                          "speaker evaluation trials do not cover an EER: "
                          + "; ".join(coverage["deficiencies"] or ["no evaluation trials"]))
    manifest_path = Path(args.out)
    root = manifest_path.parent
    _private_dir(root)
    samples = []
    try:
        for entry in loaded:
            sample_id = entry["gold"]["sample_id"]
            samples.append(_copy_sample(root, entry["gold"], entry["audio"],
                                        f"samples/{sample_id}/audio.wav",
                                        f"samples/{sample_id}/gold.json",
                                        entry["authorization"]))
        manifest = {"schema": acceptance.MANIFEST_SCHEMA, "created_at": _now_iso(), "samples": samples}
        _write_private(manifest_path, json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
        acceptance.load_manifest(manifest_path)
    except BaseException:
        # The root did not exist before this call, so removing it cannot touch
        # an earlier frozen set.
        import shutil
        shutil.rmtree(root, ignore_errors=True)
        raise
    return {"status": "FINALIZED", "manifest": str(manifest_path),
            "samples": [{key: sample[key] for key in ("id", "category", "audio_sha256", "gold_sha256")}
                        for sample in samples],
            "categories": sorted(categories),
            "speaker_coverage": coverage,
            "note": "the frozen manifest is the only input later scoring reads"}


def _now_iso() -> str:
    import time
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


# --- prediction bundle validation ------------------------------------------

def _require_keys(payload: dict, keys: tuple[str, ...], label: str) -> None:
    missing = [key for key in keys if key not in payload]
    if missing:
        raise ReviewError("invalid_predictions", f"{label} is missing {', '.join(missing)}")


def _hash_for(value, sample_id: str, label: str) -> str:
    """A bundle hash is either one digest for the single sample or a map."""
    if isinstance(value, dict):
        digest = value.get(sample_id)
    else:
        digest = value
    if not acceptance.valid_hash(digest):
        raise ReviewError("invalid_predictions", f"{label} for {sample_id} must be 64 hex characters")
    return digest


def load_predictions(path: Path) -> dict:
    payload = json.loads(path.read_text(encoding="utf-8"))
    if payload.get("schema") != PREDICTIONS_SCHEMA:
        raise ReviewError("invalid_predictions", f"schema must be {PREDICTIONS_SCHEMA}")
    _require_keys(payload, ("run_id", "audio_sha256", "gold_sha256", "config_sha256",
                            "model", "binary_sha256", "samples", "query_outputs", "claims",
                            "stages", "resources"), "predictions bundle")
    for key in ("config_sha256", "binary_sha256"):
        if not acceptance.valid_hash(payload.get(key)):
            raise ReviewError("invalid_predictions", f"{key} must be 64 hex characters")
    model = payload.get("model")
    if not isinstance(model, dict) or not model.get("provider") or not model.get("revision"):
        raise ReviewError("invalid_predictions", "model needs an actual provider and revision")
    for entry in payload.get("samples") or []:
        if not isinstance(entry, dict) or not str(entry.get("sample_id") or ""):
            raise ReviewError("invalid_predictions", "each prediction sample needs a sample_id")
        for turn in entry.get("turns") or []:
            metrics_span = (turn.get("start_ms"), turn.get("end_ms"))
            if not acceptance.require_int(metrics_span[0]) or not acceptance.require_int(metrics_span[1]) \
                    or metrics_span[1] <= metrics_span[0]:
                raise ReviewError("invalid_predictions",
                                  f"sample {entry['sample_id']} has a turn with an invalid span")
    if payload.get("prediction_digest") and payload["prediction_digest"] != prediction_digest(payload):
        raise ReviewError("digest_mismatch", "the bundle's own prediction_digest does not match its content")
    return payload


def validate_adjudications(payload: dict, bundle: dict, gold_claims: dict[str, set[str]],
                          ref_ids: dict[str, set[str]]) -> dict:
    if payload.get("schema") != ADJUDICATIONS_SCHEMA:
        raise ReviewError("invalid_adjudications", f"schema must be {ADJUDICATIONS_SCHEMA}")
    _require_keys(payload, ("prediction_digest", "annotator", "reviewed_at", "claims", "evidence"),
                  "adjudications")
    if not acceptance.valid_timestamp(payload.get("reviewed_at")):
        raise ReviewError("invalid_adjudications", "reviewed_at must be an ISO-8601 timestamp with a timezone")
    if not str(payload.get("annotator") or "").strip():
        raise ReviewError("invalid_adjudications", "annotator is required")
    if payload["prediction_digest"] != prediction_digest(bundle):
        raise ReviewError("digest_mismatch", "adjudications do not match this prediction bundle")
    known_claims = {claim.get("claim_id") for claim in bundle.get("claims") or []}
    seen_claims: set[str] = set()
    for index, item in enumerate(payload.get("claims") or []):
        claim_id = item.get("claim_id") if isinstance(item, dict) else None
        if claim_id not in known_claims:
            raise ReviewError("invalid_adjudications", f"claims[{index}] names an unknown claim {claim_id!r}")
        if claim_id in seen_claims:
            raise ReviewError("invalid_adjudications", f"claims[{index}] duplicates {claim_id}")
        seen_claims.add(claim_id)
        if not isinstance(item.get("accepted"), bool):
            raise ReviewError("invalid_adjudications", f"claims[{index}] needs a boolean accepted")
    seen_refs: set[tuple[str, str]] = set()
    for index, item in enumerate(payload.get("evidence") or []):
        query_id = item.get("query_id") if isinstance(item, dict) else None
        ref_id = item.get("ref_id") if isinstance(item, dict) else None
        if query_id not in ref_ids or ref_id not in ref_ids.get(query_id, set()):
            raise ReviewError("invalid_adjudications", f"evidence[{index}] names an unknown ref {ref_id!r}")
        if (query_id, ref_id) in seen_refs:
            raise ReviewError("invalid_adjudications", f"evidence[{index}] duplicates {ref_id}")
        seen_refs.add((query_id, ref_id))
        if not isinstance(item.get("accepted"), bool):
            raise ReviewError("invalid_adjudications", f"evidence[{index}] needs a boolean accepted")
    unknown_gold = set(gold_claims) - known_claims
    return {"claims": payload.get("claims") or [], "evidence": payload.get("evidence") or [],
            "unjudged_claims": sorted(known_claims - seen_claims),
            "gold_claims_without_prediction": sorted(unknown_gold)}


# --- adjudication drafts ----------------------------------------------------

def adjudicate_prepare(args: argparse.Namespace) -> dict:
    """Write an empty adjudication draft plus a readable review sheet."""
    bundle = load_predictions(Path(args.predictions))
    out = Path(args.out)
    _private_dir(out)
    digest = prediction_digest(bundle)
    claims = [claim for claim in bundle.get("claims") or [] if isinstance(claim, dict)]
    evidence = []
    review_queries = []
    for output in bundle.get("query_outputs") or []:
        query_id = output.get("query_id")
        entry = {"query_id": query_id, "web": [], "mcp": []}
        for source in ("web", "mcp"):
            payload = output.get(source) or {}
            for ref in payload.get("refs") or []:
                if not isinstance(ref, dict) or not ref.get("ref_id"):
                    continue
                evidence.append({"query_id": query_id, "ref_id": ref["ref_id"], "accepted": None})
                entry[source].append({key: ref.get(key) for key in (
                    "ref_id", "turn_id", "conversation_id", "recording_id", "transcript_revision",
                    "start_ms", "end_ms", "text")})
        review_queries.append(entry)
    draft = {
        "schema": ADJUDICATIONS_SCHEMA,
        "prediction_digest": digest,
        "annotator": "",
        "reviewed_at": "",
        "claims": [{"claim_id": claim.get("claim_id"), "accepted": None} for claim in claims],
        "evidence": evidence,
        "note": ("unfilled draft: accepted stays null and validate_adjudications refuses it until a "
                 "reviewer signs every row"),
    }
    review = {
        "schema": "context-engine-adjudication-review-v1",
        "run_id": bundle.get("run_id"),
        "prediction_digest": digest,
        "model": bundle.get("model"),
        "claims": [{"claim_id": claim.get("claim_id"), "kind": claim.get("kind"),
                    "sample_id": claim.get("sample_id"), "text": claim.get("text"),
                    "status": claim.get("status"), "evidence": claim.get("evidence") or []}
                   for claim in claims],
        "queries": review_queries,
        "note": "read-only view of the predictions; verdicts are entered in adjudications-draft.json",
    }
    _write_private(out / "adjudications-draft.json", json.dumps(draft, ensure_ascii=False, indent=2) + "\n")
    _write_private(out / "review.json", json.dumps(review, ensure_ascii=False, indent=2) + "\n")
    return {"status": "PREPARED", "root": str(out), "prediction_digest": digest,
            "claims": len(claims), "evidence_refs": len(evidence),
            "draft": str(out / "adjudications-draft.json"), "review": str(out / "review.json"),
            "next": ("review every claim and returned ref, fill accepted with true/false, set annotator "
                     "and a timezone-bearing reviewed_at, save as adjudications.json")}


# --- speaker trials ---------------------------------------------------------

def speaker_trial_scores(manifest_path: Path, gold_docs: dict[str, dict], audio_config: str) -> dict:
    """Cosine scores for the human speaker trials, or BLOCKED.

    The embedding path is the adapter's own ``speaker_span_embedding``; a trial
    score exists only when the pinned local model is available. Vectors are
    compared in memory and never leave this function.
    """
    if not audio_config:
        return {"status": "BLOCKED", "reason": "no --audio-analysis-config; voiceprint scoring not requested"}
    try:
        import audio_analysis_models as models
    except Exception as exc:  # noqa: BLE001
        return {"status": "BLOCKED", "reason": f"audio analysis adapter unavailable: {exc}"}
    config_path = Path(audio_config)
    if not config_path.is_file():
        return {"status": "BLOCKED", "reason": f"audio analysis config missing: {config_path.name}"}
    config = json.loads(config_path.read_text(encoding="utf-8"))
    try:
        resolved = models.resolve_capability(config, "speaker")
    except Exception as exc:  # noqa: BLE001
        return {"status": "BLOCKED", "reason": f"speaker capability unavailable: {exc}"}
    model_path = resolved.get("model_path") or resolved.get("onnx_path") or ""
    model_hash = ""
    if model_path and Path(model_path).is_file():
        model_hash = acceptance.sha256_file(Path(model_path))
    try:
        session = models._onnx_session(model_path, "speaker")
        input_name, _output = models._onnx_io_names(session, "x", "embedding")
    except Exception as exc:  # noqa: BLE001
        return {"status": "BLOCKED", "reason": f"speaker model unusable: {exc}"}
    manifest = acceptance.load_manifest(manifest_path)
    audio_paths = {sample["id"]: Path(sample["_audio_path"]) for sample in manifest["samples"]}
    samples_cache: dict[str, list[float]] = {}
    fbank_backend = resolved.get("fbank_backend") or "auto"
    scores: dict[str, dict] = {}
    for sample_id, gold in gold_docs.items():
        trials = gold.get("speaker_trials")
        if trials is None or sample_id not in audio_paths:
            continue
        spans: dict[tuple[str, str, int, int], list[float]] = {}

        def embedding(span: dict) -> list[float] | None:
            key = (span["sample_id"], span["speaker"], int(span["start_ms"]), int(span["end_ms"]))
            if key in spans:
                return spans[key]
            target = audio_paths.get(span["sample_id"])
            if target is None:
                return None
            if span["sample_id"] not in samples_cache:
                samples_cache[span["sample_id"]] = _read_pcm(target)
            pcm = samples_cache[span["sample_id"]]
            window = pcm[int(span["start_ms"]) * 16:(int(span["end_ms"]) * 16)]
            vector = models.speaker_span_embedding(session, input_name, window, fbank_backend)
            spans[key] = vector
            return vector

        for trial in trials:
            left, right = embedding(trial["left"]), embedding(trial["right"])
            if left is None or right is None:
                scores[trial["id"]] = {"status": "NOT_EVALUABLE",
                                       "reason": "voiceprint unavailable for a trial span"}
                continue
            scores[trial["id"]] = {"status": "OK", "score": _cosine(left, right),
                                   "model_sha256": model_hash}
    return {"status": "OK", "model_sha256": model_hash, "trials": scores}


def _read_pcm(path: Path) -> list[float]:
    """Mono 16-bit WAV samples as floats in [-1, 1]."""
    import array
    import wave
    with wave.open(str(path), "rb") as stream:
        if stream.getnchannels() != 1 or stream.getsampwidth() != 2:
            raise ReviewError("audio_unusable", "trial audio must be mono 16-bit PCM")
        frames = stream.readframes(stream.getnframes())
    values = array.array("h")
    values.frombytes(frames[:len(frames) - (len(frames) % 2)])
    if sys.byteorder == "big":
        values.byteswap()
    return [sample / 32768.0 for sample in values]


def _cosine(left, right) -> float:
    dot = float(sum(a * b for a, b in zip(left, right)))
    norm_left = math.sqrt(float(sum(a * a for a in left)))
    norm_right = math.sqrt(float(sum(b * b for b in right)))
    if norm_left == 0.0 or norm_right == 0.0:
        return 0.0
    return max(-1.0, min(1.0, dot / (norm_left * norm_right)))


def eer_coverage(gold_docs: dict[str, dict]) -> dict:
    """Whether the human trials support an EER at all."""
    evaluation = []
    calibration = []
    for gold in gold_docs.values():
        for trial in gold.get("speaker_trials") or []:
            item = {"trial": trial, "split": trial.get("split")}
            (evaluation if item["split"] == "evaluation" else calibration).append(item)
    speakers = sorted({item["trial"]["left"]["speaker"] for item in evaluation
                       if item["trial"].get("same_speaker")})
    deficiencies = []
    if len(speakers) < 2:
        deficiencies.append("fewer than two speakers with same-speaker trials")
    for speaker in speakers:
        cross = [item for item in evaluation
                 if item["trial"].get("same_speaker") and item["trial"]["left"]["speaker"] == speaker
                 and item["trial"]["left"]["sample_id"] != item["trial"]["right"]["sample_id"]]
        negatives = [item for item in evaluation
                     if not item["trial"].get("same_speaker")
                     and item["trial"]["left"]["speaker"] == speaker]
        if not cross:
            deficiencies.append(f"speaker {speaker} has no cross-recording same-speaker trial")
        if not negatives:
            deficiencies.append(f"speaker {speaker} has no different-speaker trial")
    return {"speakers": speakers, "evaluation_trials": len(evaluation),
            "calibration_trials": len(calibration), "deficiencies": deficiencies,
            "status": "OK" if not deficiencies and evaluation else "NOT_EVALUABLE"}


def score_eer(gold_docs: dict[str, dict], trial_scores: dict) -> dict:
    """EER over evaluation-split trials only, per gold sample category.

    Calibration trials never enter the number. A missing score, a non-finite
    or out-of-range cosine, or an absent model makes the result BLOCKED or
    NOT_EVALUABLE instead of quietly scoring a subset.
    """
    result = {"status": "BLOCKED", "reason": "", "value": None, "same_trials": 0,
              "different_trials": 0, "experimental": None, "per_category": {}}
    if not isinstance(trial_scores, dict) or trial_scores.get("status") != "OK":
        result["reason"] = (trial_scores or {}).get("reason") or "voiceprint scores unavailable"
        return result
    scores = trial_scores.get("trials") or {}
    same: dict[str, list[float]] = {}
    different: dict[str, list[float]] = {}
    problems = []
    for sample_id, gold in gold_docs.items():
        category = str(gold.get("category"))
        for trial in gold.get("speaker_trials") or []:
            if not isinstance(trial, dict) or trial.get("split") != "evaluation":
                continue
            trial_id = trial.get("id")
            scored = scores.get(trial_id)
            if not isinstance(scored, dict) or scored.get("status") != "OK":
                problems.append(f"trial {trial_id} has no voiceprint score")
                continue
            value = scored.get("score")
            if not isinstance(value, (int, float)) or isinstance(value, bool) \
                    or not math.isfinite(value) or not -1.0 <= value <= 1.0:
                problems.append(f"trial {trial_id} score is not a finite cosine in [-1, 1]")
                continue
            bucket = same if trial.get("same_speaker") else different
            bucket.setdefault(category, []).append(float(value))
    if problems:
        result.update(status="NOT_EVALUABLE", reason="; ".join(problems[:5]))
        return result
    if not same and not different:
        result.update(status="NOT_EVALUABLE", reason="no evaluation trials were declared")
        return result
    per_category = {category: metrics.eer(same.get(category, []), different.get(category, []))
                    for category in sorted(set(same) | set(different))}
    overall = metrics.eer([value for values in same.values() for value in values],
                          [value for values in different.values() for value in values])
    result.update(status=overall.get("status") or "NOT_EVALUABLE", value=overall.get("value"),
                  same_trials=overall.get("same_trials"),
                  different_trials=overall.get("different_trials"),
                  experimental=overall.get("experimental"), per_category=per_category)
    return result


def _crop(points, regions):
    out = []
    for item in points:
        for region in regions:
            low = max(item["start_ms"], region["start_ms"])
            high = min(item["end_ms"], region["end_ms"])
            if high > low:
                out.append({**item, "start_ms": low, "end_ms": high})
    return out


def _inside_regions(item, regions) -> bool:
    return any(item["start_ms"] >= region["start_ms"] and item["end_ms"] <= region["end_ms"]
               for region in regions)


def _transcript_metrics(gold_turns, predicted_turns, regions) -> dict:
    scored_gold = [turn for turn in gold_turns if _inside_regions(turn, regions)]
    scored_predicted = [turn for turn in predicted_turns if _inside_regions(turn, regions)]
    excluded_gold = [turn for turn in gold_turns if not _inside_regions(turn, regions)]
    excluded_predicted = [turn for turn in predicted_turns if not _inside_regions(turn, regions)]
    if not scored_gold:
        return {"status": "NOT_EVALUABLE", "reason": "no gold turn is fully inside a scored region",
                "scored_ms": 0, "excluded_gold_turns": len(excluded_gold),
                "excluded_predicted_turns": len(excluded_predicted),
                "lost_gold_ms": sum(turn["end_ms"] - turn["start_ms"] for turn in excluded_gold)}
    gold_text = " ".join(turn.get("text") or "" for turn in sorted(scored_gold, key=lambda t: t["start_ms"]))
    predicted_text = " ".join(turn.get("text") or ""
                              for turn in sorted(scored_predicted, key=lambda t: t["start_ms"]))
    return {"status": "OK",
            "cer": metrics.cer(gold_text, predicted_text),
            "wer": metrics.wer(gold_text, predicted_text),
            "scored_regions": len(regions),
            "scored_ms": sum(turn["end_ms"] - turn["start_ms"] for turn in scored_gold),
            "excluded_gold_turns": len(excluded_gold),
            "excluded_predicted_turns": len(excluded_predicted),
            "lost_gold_ms": sum(turn["end_ms"] - turn["start_ms"] for turn in excluded_gold)}


def _pyannote_der(reference, hypothesis, audio_python: str) -> dict:
    """Diarization error rate from pyannote.metrics in the audio interpreter."""
    if not audio_python:
        return {"status": "BLOCKED", "reason": "no --audio-python; pyannote.metrics was not consulted"}
    program = (
        "import json,sys\n"
        "try:\n"
        "    from pyannote.core import Annotation, Segment\n"
        "    from pyannote.metrics.diarization import DiarizationErrorRate\n"
        "except Exception as exc:\n"
        "    print(json.dumps({'status':'BLOCKED','reason':str(exc)})); raise SystemExit(0)\n"
        "payload=json.load(sys.stdin)\n"
        "def load(items):\n"
        "    annotation=Annotation()\n"
        "    for i,item in enumerate(items):\n"
        "        annotation[Segment(item['start_ms']/1000.0, item['end_ms']/1000.0), str(item['speaker'])] = None\n"
        "    return annotation\n"
        "metric=DiarizationErrorRate(collar=0.0, skip_overlap=False)\n"
        "value=metric(load(payload['reference']), load(payload['hypothesis']))\n"
        "print(json.dumps({'status':'OK','value':value}))\n"
    )
    try:
        proc = subprocess.run([audio_python, "-c", program], input=json.dumps(
            {"reference": reference, "hypothesis": hypothesis}), capture_output=True, text=True, timeout=300)
    except (OSError, subprocess.TimeoutExpired) as exc:
        return {"status": "BLOCKED", "reason": f"pyannote interpreter unusable: {exc}"}
    if proc.returncode != 0:
        return {"status": "BLOCKED", "reason": (proc.stderr or "interpreter failed").strip().splitlines()[-1]}
    for line in reversed((proc.stdout or "").strip().splitlines()):
        try:
            payload = json.loads(line)
        except json.JSONDecodeError:
            continue
        return payload
    return {"status": "BLOCKED", "reason": "pyannote produced no JSON result"}


# --- production quality gate ------------------------------------------------

def _canonical_digest(payload) -> str:
    return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()


def load_production_thresholds(path: Path) -> dict:
    """Read the pre-approved thresholds; refuse an incomplete or fuzzy one."""
    payload = json.loads(path.read_text(encoding="utf-8"))
    problems = []
    if payload.get("schema") != PRODUCTION_THRESHOLDS_SCHEMA:
        problems.append(f"schema must be {PRODUCTION_THRESHOLDS_SCHEMA}")
    if payload.get("approved") is not True:
        problems.append("approved must be true")
    if not acceptance.valid_timestamp(payload.get("approved_at")):
        problems.append("approved_at must be an ISO-8601 timestamp with a timezone")
    if not str(payload.get("approved_by") or "").strip():
        problems.append("approved_by is required")
    categories = payload.get("categories")
    if not isinstance(categories, list) or sorted(categories) != sorted(acceptance.CATEGORIES):
        problems.append(f"categories must be exactly {list(acceptance.CATEGORIES)}")
    metrics_payload = payload.get("metrics")
    if not isinstance(metrics_payload, dict):
        problems.append("metrics must be an object")
    else:
        unknown = sorted(set(metrics_payload) - set(PRODUCTION_METRICS))
        if unknown:
            problems.append(f"metrics has unknown keys: {', '.join(unknown)}")
        for key in PRODUCTION_METRICS:
            value = metrics_payload.get(key)
            if not isinstance(value, (int, float)) or isinstance(value, bool) \
                    or not math.isfinite(value) or not 0.0 <= value <= 1.0:
                problems.append(f"metrics.{key} must be a finite number in [0, 1]")
    if problems:
        raise ReviewError("invalid_thresholds", "; ".join(problems))
    return payload


def _empty_category_counts() -> dict:
    return {
        "cer": {"errors": 0, "reference": 0},
        "wer": {"errors": 0, "reference": 0},
        "der": {"false_alarm_ms": 0.0, "missed_ms": 0.0, "confusion_ms": 0.0,
                "reference_speech_ms": 0.0},
        "events": {"tp": 0, "fp": 0, "fn": 0},
        "web": {"supported_refs": 0, "returned_refs": 0,
                "semantic_accepted": 0, "semantic_adjudicated": 0},
        "mcp": {"supported_refs": 0, "returned_refs": 0,
                "semantic_accepted": 0, "semantic_adjudicated": 0},
        "decision": {"accepted": 0, "total": 0, "adjudicated": 0},
        "eer": None,
    }


def _ratio(numerator, denominator):
    if not denominator:
        return None
    return numerator / denominator


def evaluate_production_quality(manifest: dict, bundle: dict, result: dict, thresholds: dict) -> dict:
    """Decide PASS/FAIL/BLOCKED from stored numerators and denominators.

    A metric that is missing, non-finite, has a zero denominator, or is
    NOT_RUN/NOT_EVALUABLE can never pass; a measured value over its threshold
    is a FAIL. FAIL wins over BLOCKED.
    """
    failures: list[str] = []
    blocked: list[str] = []
    category_of = {sample["id"]: sample["category"] for sample in manifest["samples"]}
    categories = list(thresholds["categories"])
    counts = {category: _empty_category_counts() for category in categories}
    for category in categories:
        if category not in set(category_of.values()):
            blocked.append(f"{category}: no sample in the manifest")
    if result.get("sample_problems"):
        blocked.append("samples with problems: " + ", ".join(sorted(result["sample_problems"])))
    if not (bundle.get("stages") or {}).get("complete"):
        blocked.append("the run reported incomplete stages")

    for entry in result.get("samples") or []:
        category = entry.get("category")
        if category not in counts:
            blocked.append(f"sample {entry.get('sample_id')}: unknown category {category!r}")
            continue
        bucket = counts[category]
        transcript = entry.get("transcript") or {}
        for key, label in (("cer", "CER"), ("wer", "WER")):
            detail = transcript.get(key) or {}
            if transcript.get("status") != "OK" or detail.get("status") != "OK" \
                    or not detail.get("denominator"):
                blocked.append(f"{category}: {label} has no scored denominator")
                continue
            bucket[key]["errors"] += int(detail.get("numerator") or 0)
            bucket[key]["reference"] += int(detail.get("denominator") or 0)
        events = entry.get("acoustic_events") or {}
        if events.get("status") != "OK":
            blocked.append(f"{category}: acoustic events are {events.get('status')}")
        else:
            for key in ("tp", "fp", "fn"):
                bucket["events"][key] += int(events.get(key) or 0)
        der = entry.get("der") or {}
        if der.get("status") != "OK":
            blocked.append(f"{category}: DER is {der.get('status')} ({der.get('reason', 'no reason')})")
        else:
            for key in ("false_alarm_ms", "missed_ms", "confusion_ms", "reference_speech_ms"):
                bucket["der"][key] += float(der.get(key) or 0.0)

    for entry in result.get("queries") or []:
        category = entry.get("category")
        if category not in counts:
            blocked.append(f"query {entry.get('query_id')}: unknown category {category!r}")
            continue
        for source in ("web", "mcp"):
            payload = entry.get(source) or {}
            if payload.get("status") == "FAIL" or payload.get("error"):
                blocked.append(f"{category}: {source} query {entry.get('query_id')} failed")
                continue
            if entry.get("status") in (metrics.STATUS_NOT_RUN, metrics.STATUS_NOT_EVALUABLE):
                blocked.append(f"{category}: query {entry.get('query_id')} is {entry.get('status')}")
                continue
            bucket = counts[category][source]
            bucket["supported_refs"] += int(payload.get("supported_refs") or 0)
            bucket["returned_refs"] += int(payload.get("returned_refs") or 0)
            bucket["semantic_accepted"] += int(payload.get("semantic_accepted") or 0)
            bucket["semantic_adjudicated"] += int(payload.get("semantic_adjudicated") or 0)

    judged_claims: dict[str, bool] = {}
    if not result.get("adjudication"):
        blocked.append("no human adjudications were supplied")
    else:
        for item in ((result["adjudication"].get("report") or {}).get("claims") or []):
            judged_claims[item.get("claim_id")] = bool(item.get("accepted"))
    decisions = [claim for claim in bundle.get("claims") or [] if claim.get("kind") == "decision"]
    for claim in decisions:
        category = category_of.get(claim.get("sample_id"))
        if category not in counts:
            blocked.append(f"decision {claim.get('claim_id')}: no sample category")
            continue
        bucket = counts[category]["decision"]
        bucket["total"] += 1
        if claim.get("claim_id") in judged_claims:
            bucket["adjudicated"] += 1
            if judged_claims[claim["claim_id"]]:
                bucket["accepted"] += 1
    for category, bucket in counts.items():
        decision = bucket["decision"]
        if decision["total"] == 0:
            blocked.append(f"{category}: no decision predictions to adjudicate")
        elif decision["adjudicated"] != decision["total"]:
            blocked.append(f"{category}: {decision['total'] - decision['adjudicated']} decisions "
                           f"were not adjudicated")

    eer = (result.get("speaker") or {}).get("eer") or {}
    if eer.get("status") != "OK":
        blocked.append(f"speaker EER is {eer.get('status')}: {eer.get('reason', '')}".strip())
    else:
        for category, bucket in counts.items():
            detail = (eer.get("per_category") or {}).get(category)
            if not isinstance(detail, dict) or detail.get("status") != "OK":
                blocked.append(f"{category}: no evaluation EER")
                continue
            bucket["eer"] = {"value": detail.get("value"),
                             "same_trials": int(detail.get("same_trials") or 0),
                             "different_trials": int(detail.get("different_trials") or 0),
                             "experimental": bool(detail.get("experimental"))}

    for category, bucket in counts.items():
        for source in ("web", "mcp"):
            refs = bucket[source]
            if refs["returned_refs"] == 0:
                blocked.append(f"{category}: {source} returned no evidence")
            elif refs["semantic_adjudicated"] != refs["returned_refs"]:
                missing = refs["returned_refs"] - refs["semantic_adjudicated"]
                blocked.append(f"{category}: {source} has {missing} unadjudicated evidence refs")

    limits = thresholds["metrics"]
    for category, bucket in counts.items():
        returned_refs = bucket["web"]["returned_refs"] + bucket["mcp"]["returned_refs"]
        semantic_adjudicated = (bucket["web"]["semantic_adjudicated"]
                                + bucket["mcp"]["semantic_adjudicated"])
        decision = bucket["decision"]
        # A partially adjudicated precision is unknown, never zero.
        semantic_value = (_ratio(bucket["web"]["semantic_accepted"] + bucket["mcp"]["semantic_accepted"],
                                 returned_refs)
                          if returned_refs and semantic_adjudicated == returned_refs else None)
        decision_value = (_ratio(decision["accepted"], decision["total"])
                          if decision["total"] and decision["adjudicated"] == decision["total"]
                          else None)
        checks = [
            ("cer", _ratio(bucket["cer"]["errors"], bucket["cer"]["reference"]), limits["cer_max"], "max"),
            ("wer", _ratio(bucket["wer"]["errors"], bucket["wer"]["reference"]), limits["wer_max"], "max"),
            ("der", _ratio(bucket["der"]["false_alarm_ms"] + bucket["der"]["missed_ms"]
                           + bucket["der"]["confusion_ms"], bucket["der"]["reference_speech_ms"]),
             limits["der_max"], "max"),
            ("event_precision", _ratio(bucket["events"]["tp"],
                                       bucket["events"]["tp"] + bucket["events"]["fp"]),
             limits["event_precision_min"], "min"),
            ("event_recall", _ratio(bucket["events"]["tp"],
                                    bucket["events"]["tp"] + bucket["events"]["fn"]),
             limits["event_recall_min"], "min"),
            ("evidence_accuracy", _ratio(bucket["web"]["supported_refs"] + bucket["mcp"]["supported_refs"],
                                         returned_refs),
             limits["evidence_accuracy_min"], "min"),
            ("semantic_accuracy", semantic_value, limits["semantic_accuracy_min"], "min"),
            ("decision_precision", decision_value, limits["decision_precision_min"], "min"),
        ]
        for name, value, limit, direction in checks:
            if value is None:
                blocked.append(f"{category}: {name} has no complete denominator")
                continue
            if not isinstance(value, (int, float)) or not math.isfinite(value):
                blocked.append(f"{category}: {name} is not finite")
                continue
            if direction == "max" and value > limit:
                failures.append(f"{category}: {name} {value:.4f} exceeds {limit}")
            elif direction == "min" and value < limit:
                failures.append(f"{category}: {name} {value:.4f} is below {limit}")
        detail = bucket["eer"]
        if isinstance(detail, dict):
            if detail.get("experimental"):
                blocked.append(f"{category}: EER uses fewer than 30 trials per class (experimental)")
            elif isinstance(detail.get("value"), (int, float)):
                if detail["value"] > limits["eer_max"]:
                    failures.append(f"{category}: eer {detail['value']:.4f} exceeds {limits['eer_max']}")

    inputs = result.get("inputs") or {}
    failures = list(dict.fromkeys(failures))
    blocked = list(dict.fromkeys(blocked))
    status = "FAIL" if failures else ("BLOCKED" if blocked else "PASS")
    return {
        "schema": PRODUCTION_GATE_SCHEMA,
        "status": status,
        "thresholds_sha256": inputs.get("thresholds_sha256"),
        "manifest_sha256": inputs.get("manifest_sha256"),
        "predictions_sha256": inputs.get("predictions_sha256"),
        "adjudications_sha256": inputs.get("adjudications_sha256"),
        "categories": counts,
        "thresholds": dict(limits),
        "failures": failures,
        "blocked": blocked,
        "note": "computed from stored numerators and denominators; no hand-written PASS is accepted",
    }


def score(args: argparse.Namespace) -> dict:
    manifest_path = Path(args.manifest)
    manifest = acceptance.load_manifest(manifest_path)
    known_ids = {sample["id"] for sample in manifest["samples"]}
    category_of = {sample["id"]: sample["category"] for sample in manifest["samples"]}
    gold_docs: dict[str, dict] = {}
    sample_problems: dict[str, list[str]] = {}
    for sample in manifest["samples"]:
        problems = acceptance.verify_sample_files(sample)
        if problems:
            sample_problems[sample["id"]] = problems
            continue
        try:
            gold_docs[sample["id"]] = acceptance.load_gold(Path(sample["_gold_path"]), sample["id"], known_ids)
        except acceptance.AcceptanceError as exc:
            sample_problems[sample["id"]] = [f"{exc.code}: {exc.detail}"]
    bundle = load_predictions(Path(args.predictions))
    for sample in manifest["samples"]:
        if sample["id"] not in gold_docs:
            continue
        if _hash_for(bundle["gold_sha256"], sample["id"], "gold_sha256") != sample["gold_sha256"]:
            raise ReviewError("hash_mismatch",
                              f"predictions were not produced from the frozen gold of {sample['id']}")
        if _hash_for(bundle["audio_sha256"], sample["id"], "audio_sha256") != sample["audio_sha256"]:
            raise ReviewError("hash_mismatch",
                              f"predictions were not produced from the frozen audio of {sample['id']}")

    predictions_by_sample = {entry.get("sample_id"): entry for entry in bundle.get("samples") or []
                             if isinstance(entry, dict)}
    identity: dict[str, dict] = {}
    for sample_id, entry in predictions_by_sample.items():
        identity[sample_id] = {
            "conversation_id": entry.get("conversation_id"),
            "recording_id": entry.get("recording_id"),
            "transcript_revision": entry.get("transcript_revision"),
        }

    ref_ids: dict[str, set[str]] = {}
    for output in bundle.get("query_outputs") or []:
        refs = set()
        for source in ("web", "mcp"):
            for ref in (output.get(source) or {}).get("refs") or []:
                if isinstance(ref, dict) and ref.get("ref_id"):
                    refs.add(ref["ref_id"])
        ref_ids[output.get("query_id")] = refs
    gold_claim_ids = {gold_id: {claim["id"] for claim in gold.get("claims") or []}
                      for gold_id, gold in gold_docs.items()}
    adjudications = None
    if args.adjudications:
        adjudications = validate_adjudications(json.loads(Path(args.adjudications).read_text(encoding="utf-8")),
                                               bundle, gold_claim_ids, ref_ids)

    out = Path(args.out)
    _private_dir(out)
    speaker = speaker_trial_scores(manifest_path, gold_docs, args.audio_analysis_config)
    coverage = eer_coverage(gold_docs)

    claim_judgments: dict[str, bool] = {}
    evidence_judgments: dict[str, dict[str, bool]] = {}
    if adjudications:
        claim_judgments = {item["claim_id"]: item["accepted"] for item in adjudications["claims"]}
        for item in adjudications["evidence"]:
            evidence_judgments.setdefault(item["query_id"], {})[item["ref_id"]] = item["accepted"]

    per_sample = []
    pooled_cer, pooled_wer = [], []
    for sample_id, gold in sorted(gold_docs.items()):
        predicted = predictions_by_sample.get(sample_id) or {}
        predicted_turns = predicted.get("turns") or []
        regions = gold["scored_regions"]
        entry: dict = {"sample_id": sample_id, "category": category_of[sample_id]}
        entry["transcript"] = _transcript_metrics(gold.get("transcript") or [], predicted_turns, regions)
        if entry["transcript"].get("status") == "OK":
            pooled_cer.append(entry["transcript"]["cer"])
            pooled_wer.append(entry["transcript"]["wer"])
        gold_events = gold.get("acoustic_events")
        if gold_events is None:
            entry["acoustic_events"] = {"status": "NOT_RUN", "reason": "gold has no acoustic_events key"}
        else:
            predicted_events = predicted.get("acoustic_events") or []
            entry["acoustic_events"] = metrics.event_pr(
                _crop(gold_events, regions), _crop(predicted_events, regions))
        reference_segments = _crop([{"start_ms": turn["start_ms"], "end_ms": turn["end_ms"],
                                     "speaker": turn["speaker"]} for turn in gold.get("transcript") or []],
                                   regions)
        diarization = predicted.get("diarization")
        if diarization is None:
            entry["der"] = {"status": "NOT_RUN", "value": None,
                            "reason": ("predictions have no diarization field; ASR speaker labels are "
                                       "not a diarization result")}
        else:
            capability = (predicted.get("audio_capabilities") or {}).get("diarization")
            if not diarization and capability not in (None, "succeeded"):
                entry["der"] = {"status": "NOT_RUN", "value": None,
                                "reason": f"the diarization capability was {capability}"}
            else:
                hypothesis_segments = _crop([{"start_ms": segment["start_ms"],
                                              "end_ms": segment["end_ms"],
                                              "speaker": segment.get("speaker") or "unknown"}
                                             for segment in diarization], regions)
                entry["der"] = metrics.der(reference_segments, hypothesis_segments,
                                           collar_ms=0, skip_overlap=False)
                entry["der"]["pyannote"] = (
                    _pyannote_der(reference_segments, hypothesis_segments, args.audio_python)
                    if reference_segments else
                    {"status": "NOT_EVALUABLE",
                     "reason": "no reference speech inside the scored regions"})
        per_sample.append(entry)

    query_scores = []
    for query in _gold_queries(gold_docs):
        output = next((item for item in bundle.get("query_outputs") or []
                       if item.get("query_id") == query["id"]), None)
        entry = {"query_id": query["id"], "sample_id": query["sample_id"],
                 "category": category_of.get(query["sample_id"]),
                 "expected_sufficiency": query["expected_sufficiency"],
                 "evidence_completeness": query["evidence_completeness"]}
        if output is None:
            entry["status"] = "NOT_RUN"
            entry["reason"] = "the prediction bundle has no output for this query"
            query_scores.append(entry)
            continue
        scope = [_identity_scope(identity.get(sample), sample)
                 for sample in query.get("scope_sample_ids") or []]
        gold_spans = []
        unresolved = []
        for span in query.get("required_spans") or []:
            sample_id = span.get("sample_id") or query["sample_id"]
            resolved = identity.get(sample_id) or {}
            if not resolved.get("conversation_id"):
                unresolved.append(sample_id)
                continue
            gold_spans.append({**span, "required": True,
                               "conversation_id": resolved["conversation_id"],
                               "recording_id": resolved.get("recording_id"),
                               "transcript_revision": resolved.get("transcript_revision")})
        if unresolved:
            entry["status"] = "NOT_EVALUABLE"
            entry["reason"] = f"no predicted identity for sample(s) {sorted(set(unresolved))}"
            query_scores.append(entry)
            continue
        for source in ("web", "mcp"):
            payload = output.get(source) or {}
            if payload.get("error"):
                entry[source] = {"status": "FAIL", "reason": str(payload["error"])[:300]}
                continue
            judged = evidence_judgments.get(query["id"], {})
            judgments = {ref["ref_id"]: judged[ref["ref_id"]] for ref in payload.get("refs") or []
                         if isinstance(ref, dict) and ref.get("ref_id") in judged}
            entry[source] = metrics.evidence_accuracy(
                payload.get("refs") or [], gold_spans, [value for value in scope if value],
                output.get("ambient_ids") or [], output.get("forgotten_ids") or [],
                semantic_judgments=judgments)
            entry[source]["sufficiency"] = payload.get("sufficiency")
            entry[source]["sufficiency_matches"] = (
                payload.get("sufficiency") == query["expected_sufficiency"]
                if payload.get("sufficiency") in ("sufficient", "insufficient") else None)
        query_scores.append(entry)

    bundle_digest = prediction_digest(bundle)
    predicted_claims = [{"claim_id": claim.get("claim_id"), "kind": claim.get("kind"),
                         "prediction_digest": bundle_digest} for claim in bundle.get("claims") or []]
    adjudication_rows = [{"claim_id": claim_id, "accepted": accepted, "prediction_digest": bundle_digest}
                         for claim_id, accepted in sorted(claim_judgments.items())]
    claims = metrics.decision_precision(predicted_claims, adjudication_rows)
    if not (bundle.get("stages") or {}).get("complete"):
        for bucket in [claims, *claims["by_kind"].values()]:
            bucket["precision"] = None
            bucket["status"] = metrics.STATUS_NOT_RUN
        claims["reason"] = "the run reported incomplete stages; claim precision is not a quality result"

    eer = score_eer(gold_docs, speaker)
    inputs = {
        "manifest_sha256": acceptance.sha256_file(manifest_path),
        "predictions_sha256": acceptance.sha256_file(Path(args.predictions)),
        "adjudications_sha256": (acceptance.sha256_file(Path(args.adjudications))
                                 if args.adjudications else ""),
        "thresholds_sha256": "",
    }
    result = {
        "schema": "context-engine-acceptance-score-v1",
        "manifest": str(manifest_path),
        "run_id": bundle.get("run_id"),
        "prediction_digest": bundle_digest,
        "model": bundle.get("model"),
        "samples": per_sample,
        "queries": query_scores,
        "claims": claims,
        "transcript_pooled": {"cer": metrics.aggregate_error_rates(pooled_cer),
                              "wer": metrics.aggregate_error_rates(pooled_wer)},
        "speaker": {"coverage": coverage, "scores": speaker, "eer": eer},
        "adjudication": None if adjudications is None else {
            "report": adjudications,
            "note": "unjudged items stay NOT_RUN and are never counted as correct or wrong"},
        "sample_problems": sample_problems,
        "excluded_samples": sorted(sample_problems),
        "cross_recording_eer": coverage["status"] == "OK" and eer.get("status") == "OK",
        "inputs": inputs,
        "note": ("offline re-scoring only: no ASR or generation service is called; "
                 "a configured voiceprint model is loaded locally"),
    }
    if getattr(args, "production_thresholds", ""):
        thresholds = load_production_thresholds(Path(args.production_thresholds))
        result["inputs"]["thresholds_sha256"] = _canonical_digest(thresholds)
        result["production_gate"] = evaluate_production_quality(manifest, bundle, result, thresholds)
    _write_private(out / "score.json", json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    return result


def _identity_scope(identity: dict | None, sample_id: str) -> str | None:
    if identity and identity.get("conversation_id"):
        return identity["conversation_id"]
    return None


def _gold_queries(gold_docs: dict[str, dict]) -> list[dict]:
    queries = []
    for sample_id, gold in sorted(gold_docs.items()):
        for query in gold.get("queries") or []:
            queries.append({**query, "sample_id": sample_id})
    return queries


# --- CLI --------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    prep = sub.add_parser("prepare", help="copy one authorized recording and emit a draft gold + page")
    prep.add_argument("--audio", required=True, help="explicit path to the authorized recording")
    prep.add_argument("--sample-id", required=True)
    prep.add_argument("--out", required=True, help="new private root; must not exist")
    prep.add_argument("--reference", default="", help="explicit machine transcript kept out of gold")
    prep.add_argument("--duration-ms", type=int, default=0, help="override when ffprobe is unavailable")

    final = sub.add_parser("finalize", help="freeze a human-reviewed draft into an acceptance manifest")
    final.add_argument("--draft", required=True, help="the reviewed JSON exported by the operator")
    final.add_argument("--authorization-reference", required=True)
    final.add_argument("--out", required=True, help="new manifest path; its parent must not exist")

    final_set = sub.add_parser("finalize-set", help="freeze a reviewed set of drafts into one manifest")
    final_set.add_argument("--drafts", required=True,
                           help=f"a {REVIEWED_SET_SCHEMA} document naming each reviewed draft")
    final_set.add_argument("--out", required=True, help="new manifest path; its parent must not exist")

    adjudicate = sub.add_parser("adjudicate-prepare", help="emit an empty adjudication draft and review sheet")
    adjudicate.add_argument("--predictions", required=True)
    adjudicate.add_argument("--out", required=True, help="new private directory; must not exist")

    scorer = sub.add_parser("score", help="re-score frozen predictions against frozen gold")
    scorer.add_argument("--manifest", required=True)
    scorer.add_argument("--predictions", required=True)
    scorer.add_argument("--adjudications", default="")
    scorer.add_argument("--out", required=True, help="new directory; must not exist")
    scorer.add_argument("--audio-python", default="", help="interpreter with pyannote.metrics for DER")
    scorer.add_argument("--audio-analysis-config", default="", help="trusted local model config for EER")
    scorer.add_argument("--production-thresholds", default="",
                        help="pre-approved production thresholds; a non-PASS gate exits nonzero")

    args = parser.parse_args()
    try:
        if args.command == "prepare":
            payload = prepare(args)
        elif args.command == "finalize":
            payload = finalize(args)
        elif args.command == "finalize-set":
            payload = finalize_set(args)
        elif args.command == "adjudicate-prepare":
            payload = adjudicate_prepare(args)
        else:
            payload = score(args)
    except ReviewError as exc:
        print(json.dumps({"status": "FAIL", "code": exc.code, "reason": exc.detail}, ensure_ascii=False))
        return 1
    except (OSError, ValueError, json.JSONDecodeError, acceptance.AcceptanceError) as exc:
        print(json.dumps({"status": "FAIL", "code": "unexpected", "reason": str(exc)}, ensure_ascii=False))
        return 1
    print(json.dumps({key: value for key, value in payload.items() if key != "samples"},
                     ensure_ascii=False, default=str))
    gate = payload.get("production_gate") or {}
    if gate and gate.get("status") != "PASS":
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
