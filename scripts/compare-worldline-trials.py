#!/usr/bin/env python3
"""Validate and locally compare two source-matched Worldline private trials.

The report contains private source excerpts. It is written only to a mode-0700
directory with mode-0600 files. Standard output contains aggregate statistics.
"""

from __future__ import annotations

import argparse
import collections
import hashlib
import html
import importlib.util
import ipaddress
import json
import os
import sys
import tempfile
import urllib.parse
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
DECISION_RUNNER = ROOT / "scripts" / "worldline-decision-experiment.py"
TRIAL_SCHEMA = "worldline-private-trial-v1"
RESULT_SCHEMA = "worldline-private-trial-results-v1"
DEFAULT_OUTPUT = ROOT / "data" / "worldline-trial-comparison"
LABEL_ZH = {
    "same_topic": "同一主题建议",
    "related_topic": "相关但不同事项",
    "different_topic": "不同主题",
    "insufficient_evidence": "无法判断",
    "supported": "原话支持归纳（不确认现实事实）",
    "plan_not_fact": "可能把计划写成事实",
    "wrong_subject": "可能归错主体",
    "wrong_time": "时间、数值或状态可能不一致",
    "no_evidence": "缺少引用证据",
    "unsupported": "原文未支持",
}
STRUCTURAL_WARNING_TEXT = {
    "no_evidence_with_source": "模型称缺少引用，但本次已提供原文，不能直接采用该判断。",
    "supported_without_evidence": "模型称原话支持归纳，但本次没有提供证据，不能直接采用该判断。",
}


def _load_decision_runner() -> Any:
    spec = importlib.util.spec_from_file_location("worldline_decision_experiment_compare", DECISION_RUNNER)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load the fixed decision runner")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


decision_runner = _load_decision_runner()
TASKS = decision_runner.TASKS


def json_hash(value: Any) -> str:
    raw = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(raw).hexdigest()


def file_hash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def validate_web_base(value: str) -> str:
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme != "http" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        raise ValueError("web URL must be plain HTTP loopback without credentials, path, query, or fragment")
    try:
        address = ipaddress.ip_address(parsed.hostname)
    except ValueError as exc:
        raise ValueError("web URL host must be a literal loopback IP address") from exc
    if not address.is_loopback:
        raise ValueError("web URL host must be a literal loopback IP address")
    return f"http://{parsed.netloc}"


def prepared_hash(prepared: dict[str, Any]) -> str:
    return json_hash({
        "api_version": prepared.get("api_version"),
        "cases": [{"id": case["id"], "input_hash": case["input_hash"]} for case in prepared["cases"]],
    })


def _read_json(path: Path) -> dict[str, Any]:
    if path.is_symlink() or not path.is_file():
        raise ValueError("trial input/result files must be regular non-symlink files")
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError("trial JSON root must be an object")
    return value


def _case_map(prepared: dict[str, Any]) -> dict[str, dict[str, Any]]:
    rows = prepared.get("cases")
    if not isinstance(rows, list) or not rows:
        raise ValueError("prepared input has no cases")
    output: dict[str, dict[str, Any]] = {}
    for case in rows:
        if not isinstance(case, dict):
            raise ValueError("prepared input contains a malformed case")
        case_id, task = case.get("id"), case.get("task")
        if not isinstance(case_id, str) or not case_id or case_id in output or task not in TASKS:
            raise ValueError("prepared input has duplicate IDs or unsupported tasks")
        if "expected" in case or "gold" in case:
            raise ValueError("private trial input must not contain gold labels")
        state, sources = case.get("state"), case.get("sources")
        if not isinstance(state, dict) or not isinstance(sources, list):
            raise ValueError("prepared case has invalid state/source data")
        actual_input_hash = json_hash({"state": state, "sources": sources})
        if case.get("input_hash") != actual_input_hash:
            raise ValueError("prepared case input hash does not match its state and sources")
        output[case_id] = case
    return output


def _verify_frozen_hashes(prepared: dict[str, Any], results: dict[str, Any]) -> None:
    expected = {
        "decision_runner_sha256": file_hash(DECISION_RUNNER),
        "synthetic_fixture_sha256": file_hash(ROOT / "fixtures" / "worldline-decision-experiment.json"),
    }
    # The prompt hash uses the same canonical labels + instructions object as
    # the private-trial preparation/evaluation code.
    prompt_value = {name: {"labels": labels, "instructions": instructions} for name, (labels, instructions) in TASKS.items()}
    expected["task_prompt_sha256"] = json_hash(prompt_value)
    for name, value in expected.items():
        if not isinstance(prepared.get(name), str) or prepared[name] != value:
            raise ValueError(f"prepared {name} does not match the current frozen runner/fixture/prompt")
        if results.get(name) != value:
            raise ValueError(f"result {name} does not match its prepared input")


def _validate_pair(prepared_a: dict[str, Any], result_a: dict[str, Any], prepared_b: dict[str, Any], result_b: dict[str, Any]) -> tuple[dict[str, dict[str, Any]], dict[str, dict[str, Any]], dict[str, Any]]:
    for prepared in (prepared_a, prepared_b):
        if prepared.get("schema") != TRIAL_SCHEMA or prepared.get("no_gold_labels") is not True:
            raise ValueError("unsupported or labeled prepared input")
        if prepared.get("human_review_required") is not True:
            raise ValueError("prepared input does not require human review")
        if not isinstance(prepared.get("api_version"), str) or not prepared["api_version"]:
            raise ValueError("prepared input omitted its source API version")
        page_checks = prepared.get("page_checks")
        if not isinstance(page_checks, list) or not page_checks:
            raise ValueError("prepared input omitted its source snapshot manifest")
        for check in page_checks:
            if not isinstance(check, dict) or not isinstance(check.get("path"), str) or not isinstance(check.get("params"), dict) or not isinstance(check.get("version"), str) or not isinstance(check.get("digest"), str) or len(check["digest"]) != 64:
                raise ValueError("prepared source snapshot manifest is malformed")
            if check["version"] != prepared["api_version"]:
                raise ValueError("prepared source page version does not match the snapshot version")
            try:
                bytes.fromhex(check["digest"])
            except ValueError as exc:
                raise ValueError("prepared source page digest is malformed") from exc
    for results in (result_a, result_b):
        if results.get("schema") != RESULT_SCHEMA or results.get("no_gold_labels") is not True:
            raise ValueError("unsupported or labeled trial results")
        if "stale" not in results or results.get("stale") is not False or "stale_reason" not in results or results.get("stale_reason") not in ("", None):
            raise ValueError("stale or unverified trial results cannot be compared")
        for identity_key in ("actual_models", "weights_versions"):
            identity_values = results.get(identity_key)
            if not isinstance(identity_values, list) or not identity_values or any(not isinstance(value, str) or not value.strip() for value in identity_values):
                raise ValueError(f"trial results have no valid {identity_key}")

    _verify_frozen_hashes(prepared_a, result_a)
    _verify_frozen_hashes(prepared_b, result_b)
    for name in ("api_version", "decision_runner_sha256", "synthetic_fixture_sha256", "task_prompt_sha256", "page_checks"):
        if json_hash(prepared_a.get(name)) != json_hash(prepared_b.get(name)):
            raise ValueError(f"the two prepared trials do not share the same {name}")

    cases_a, cases_b = _case_map(prepared_a), _case_map(prepared_b)
    if cases_a.keys() != cases_b.keys():
        raise ValueError("the two prepared trials have different case IDs")
    for case_id in cases_a:
        a, b = cases_a[case_id], cases_b[case_id]
        if a["task"] != b["task"] or a["input_hash"] != b["input_hash"]:
            raise ValueError(f"case {case_id} differs between prepared trials")
        # Verify the exact serialized model input and the evidence metadata too,
        # not only the case ID/hash declaration.
        if json_hash({"state": a["state"], "sources": a["sources"]}) != json_hash({"state": b["state"], "sources": b["sources"]}):
            raise ValueError(f"case {case_id} source content differs between prepared trials")

    expected_prepared_hash = prepared_hash(prepared_a)
    if prepared_hash(prepared_b) != expected_prepared_hash:
        raise ValueError("prepared input hashes differ")
    for result in (result_a, result_b):
        if result.get("prepared_input_hash") != expected_prepared_hash:
            raise ValueError("result prepared_input_hash does not match the shared source snapshot")
        if result.get("invalid_or_failed") != 0 or result.get("valid_predictions") != len(cases_a):
            raise ValueError("both trial runs must have a valid prediction for every prepared case")

    answers_a = _result_case_map(result_a, cases_a)
    answers_b = _result_case_map(result_b, cases_b)
    return cases_a, answers_a, {"answers_b": answers_b, "prepared_hash": expected_prepared_hash}


def _result_case_map(results: dict[str, Any], prepared_cases: dict[str, dict[str, Any]]) -> dict[str, dict[str, Any]]:
    rows = results.get("cases")
    if not isinstance(rows, list) or len(rows) != len(prepared_cases):
        raise ValueError("trial result case count does not match prepared input")
    output: dict[str, dict[str, Any]] = {}
    for row in rows:
        if not isinstance(row, dict):
            raise ValueError("trial results contain a malformed case")
        case_id = row.get("id")
        if not isinstance(case_id, str) or case_id not in prepared_cases or case_id in output:
            raise ValueError("trial results have missing/duplicate case IDs")
        source = prepared_cases[case_id]
        if row.get("task") != source["task"] or row.get("input_hash") != source["input_hash"]:
            raise ValueError(f"result case {case_id} does not match the prepared input")
        if row.get("error") or not isinstance(row.get("prediction"), str):
            raise ValueError(f"result case {case_id} has no valid prediction")
        labels = set(TASKS[source["task"]][0])
        if row["prediction"] not in labels:
            raise ValueError(f"result case {case_id} has an unsupported prediction label")
        distribution = row.get("distribution")
        if not isinstance(distribution, dict) or set(distribution) != labels:
            raise ValueError(f"result case {case_id} has an incomplete distribution")
        values: list[float] = []
        for value in distribution.values():
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                raise ValueError(f"result case {case_id} has a nonnumeric distribution")
            number = float(value)
            if not 0 <= number <= 1 or number != number or number in (float("inf"), float("-inf")):
                raise ValueError(f"result case {case_id} has an invalid distribution")
            values.append(number)
        if abs(sum(values) - 1.0) > 1e-6:
            raise ValueError(f"result case {case_id} distribution is not normalized")
        output[case_id] = row
    if output.keys() != prepared_cases.keys():
        raise ValueError("trial results are missing prepared case IDs")
    return output


def compare_trials(prepared_a: dict[str, Any], result_a: dict[str, Any], prepared_b: dict[str, Any], result_b: dict[str, Any], label_a: str, label_b: str) -> tuple[dict[str, Any], str]:
    cases_a, answers_a, extra = _validate_pair(prepared_a, result_a, prepared_b, result_b)
    answers_b = extra["answers_b"]
    rows = []
    task_stats: dict[str, dict[str, int]] = collections.defaultdict(lambda: {"cases": 0, "agreement": 0, "disagreement": 0})
    pair_counts: dict[str, collections.Counter[str]] = collections.defaultdict(collections.Counter)
    overall_agreement = 0
    label_counts: dict[str, dict[str, collections.Counter[str]]] = collections.defaultdict(lambda: {"a": collections.Counter(), "b": collections.Counter()})
    structural_by_code: collections.Counter[str] = collections.Counter()
    structural_by_case: list[dict[str, Any]] = []
    for case_id in sorted(cases_a):
        case = cases_a[case_id]
        left, right = answers_a[case_id], answers_b[case_id]
        same = left["prediction"] == right["prediction"]
        task = case["task"]
        task_stats[task]["cases"] += 1
        task_stats[task]["agreement" if same else "disagreement"] += 1
        pair_counts[task][f"{left['prediction']} -> {right['prediction']}"] += 1
        label_counts[task]["a"][left["prediction"]] += 1
        label_counts[task]["b"][right["prediction"]] += 1
        overall_agreement += int(same)
        warnings = _case_structural_warnings(case, left["prediction"], "a", label_a)
        warnings.extend(_case_structural_warnings(case, right["prediction"], "b", label_b))
        for warning in warnings:
            structural_by_code[warning["code"]] += 1
        if warnings:
            structural_by_case.append({"id": case_id, "task": task, "warnings": warnings})
        rows.append({
            "id": case_id,
            "task": task,
            "input_hash": case["input_hash"],
            "model_a_label": left["prediction"],
            "model_b_label": right["prediction"],
            "agreement": same,
            "structural_warnings": warnings,
        })
    result = {
        "schema": "worldline-private-trial-comparison-v1",
        "no_gold_labels": True,
        "accuracy_claimed": False,
        "prepared_input_hash": extra["prepared_hash"],
        "models": {
            "a": {"label": label_a, "actual_models": result_a.get("actual_models"), "weights_versions": result_a.get("weights_versions")},
            "b": {"label": label_b, "actual_models": result_b.get("actual_models"), "weights_versions": result_b.get("weights_versions")},
        },
        "total_cases": len(rows),
        "agreement": overall_agreement,
        "disagreement": len(rows) - overall_agreement,
        "agreement_rate": overall_agreement / len(rows) if rows else None,
        "by_task": dict(sorted(task_stats.items())),
        "label_counts_by_task": {task: {side: dict(sorted(counts.items())) for side, counts in sides.items()} for task, sides in sorted(label_counts.items())},
        "pair_labels_by_task": {task: dict(sorted(counts.items())) for task, counts in sorted(pair_counts.items())},
        "structural_inconsistencies": {
            "count": sum(structural_by_code.values()),
            "by_code": dict(sorted(structural_by_code.items())),
            "by_case": structural_by_case,
        },
        "cases": rows,
    }
    html_text = render_comparison_html(prepared_a, result, result_a, result_b, label_a, label_b)
    return result, html_text


def _case_structural_warnings(case: dict[str, Any], prediction: str, model_key: str, model_label: str) -> list[dict[str, str]]:
    if case.get("task") != "summary_support":
        return []
    state = case.get("state", {})
    evidence = state.get("evidence", []) if isinstance(state, dict) else []
    has_evidence = isinstance(evidence, list) and len(evidence) > 0
    code = None
    if prediction == "no_evidence" and has_evidence:
        code = "no_evidence_with_source"
    elif prediction == "supported" and not has_evidence:
        code = "supported_without_evidence"
    if code is None:
        return []
    return [{"model": model_key, "model_label": model_label, "label": prediction, "code": code, "message": STRUCTURAL_WARNING_TEXT[code]}]


def _render_sources(case: dict[str, Any], web_url: str) -> str:
    chunks: list[str] = []
    for source in case.get("sources", []):
        if not isinstance(source, dict):
            continue
        title = html.escape(str(source.get("title") or ""))
        summary = html.escape(str(source.get("summary") or ""))
        kind = html.escape(str(source.get("claim_kind") or ""))
        chunks.append(f'<section class="source"><strong>{title}</strong><p>{summary} <small>{kind}</small></p>')
        for evidence in source.get("evidence_refs", []):
            if not isinstance(evidence, dict):
                continue
            chunks.append(f"<blockquote>{html.escape(str(evidence.get('text') or ''))}</blockquote>")
            conversation_id = evidence.get("conversation_id")
            if isinstance(conversation_id, str) and conversation_id:
                href = web_url + "/c/" + urllib.parse.quote(conversation_id, safe="")
                chunks.append(f'<a href="{html.escape(href, quote=True)}">在本机打开来源会话</a>')
        chunks.append("</section>")
    if not chunks:
        # Summary-support input may have sources separate from transcript state.
        state = case.get("state", {})
        for evidence in state.get("evidence", []) if isinstance(state, dict) else []:
            if isinstance(evidence, dict):
                chunks.append(f"<blockquote>{html.escape(str(evidence.get('text') or ''))}</blockquote>")
    return "".join(chunks)


def render_comparison_html(prepared: dict[str, Any], comparison: dict[str, Any], result_a: dict[str, Any], result_b: dict[str, Any], label_a: str, label_b: str, web_url: str = "http://127.0.0.1:8787") -> str:
    web_url = validate_web_base(web_url)
    cases = {case["id"]: case for case in prepared["cases"]}
    answers = []
    for result in (result_a, result_b):
        answers.append({case["id"]: case["prediction"] for case in result["cases"]})
    parts = ["<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Worldline 模型对照</title>",
             "<style>body{font:15px/1.55 system-ui,sans-serif;max-width:1100px;margin:2rem auto;padding:0 1rem;color:#223}h1{font-size:1.6rem}.notice{padding:1rem;background:#fff5d9;border:1px solid #d7bd70;border-radius:8px}.warning{padding:.7rem;background:#fff0cf;border-left:4px solid #bd7b16;margin:.5rem 0}article{border-top:1px solid #ddd;padding:1rem 0}.models{display:grid;grid-template-columns:1fr 1fr;gap:1rem}.model{padding:.8rem;background:#f2f5f7;border-radius:6px}.source{margin:.5rem 0;padding:.6rem;background:#fafafa}blockquote{margin:.4rem 0;padding-left:.8rem;border-left:3px solid #ccd;color:#465}small{color:#667}.same{color:#285b42}.diff{color:#9b3c25}</style><body>",
             "<h1>Worldline 模型对照</h1><p class=\"notice\">两组输入与来源快照已校验一致，且模型响应均有效、非过期。标签相同只表示模型意见一致，不代表真实准确或事实成立；本报告没有 gold label，不计算准确率，不自动合并主题。</p>",
             f"<p>输入快照：<code>{html.escape(comparison['prepared_input_hash'])}</code> · 案例 {comparison['total_cases']} · 意见一致 {comparison['agreement']} · 意见不同 {comparison['disagreement']} · 结构一致性提醒 {comparison['structural_inconsistencies']['count']}</p>"]
    display_rows = sorted(comparison["cases"], key=lambda row: (not bool(row.get("structural_warnings")), row["id"]))
    for row in display_rows:
        case = cases[row["id"]]
        agreement_class = "same" if row["agreement"] else "diff"
        agreement_text = "意见一致" if row["agreement"] else "意见不同"
        parts.append(f'<article><h2>{html.escape(row["id"])} · {html.escape(row["task"])}</h2><p class="{agreement_class}">{agreement_text}</p>')
        for warning in row.get("structural_warnings", []):
            parts.append(f'<p class="warning"><strong>结构一致性提醒 · {html.escape(warning["model_label"])}</strong>：{html.escape(warning["message"])}</p>')
        parts.append('<div class="models">')
        pred_a, pred_b = answers[0][row["id"]], answers[1][row["id"]]
        model_a = ", ".join(str(x) for x in (result_a.get("actual_models") or []) + (result_a.get("weights_versions") or []))
        model_b = ", ".join(str(x) for x in (result_b.get("actual_models") or []) + (result_b.get("weights_versions") or []))
        parts.append(f'<section class="model"><strong>{html.escape(label_a)}</strong><small>{html.escape(model_a)}</small><p>{html.escape(pred_a)} <small>· {html.escape(LABEL_ZH[pred_a])}</small></p></section>')
        parts.append(f'<section class="model"><strong>{html.escape(label_b)}</strong><small>{html.escape(model_b)}</small><p>{html.escape(pred_b)} <small>· {html.escape(LABEL_ZH[pred_b])}</small></p></section></div>')
        parts.append(_render_sources(case, web_url))
        parts.append("</article>")
    parts.append("</body></html>")
    return "".join(parts)


def private_output_dir(path: Path) -> Path:
    if path.exists() and path.is_symlink():
        raise ValueError("output directory may not be a symlink")
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(path, 0o700)
    if not path.is_dir():
        raise ValueError("output path is not a directory")
    return path


def write_private(path: Path, content: bytes) -> None:
    fd, temp_name = tempfile.mkstemp(prefix=".trial-compare-", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, path)
        os.chmod(path, 0o600)
    except Exception:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def load_directory(path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    prepared = _read_json(path / "trial-input.json")
    results = _read_json(path / "trial-results.json")
    if (path / "report.html").is_symlink() or not (path / "report.html").is_file():
        raise ValueError("trial directory is missing report.html")
    return prepared, results


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("trial_a", type=Path)
    parser.add_argument("trial_b", type=Path)
    parser.add_argument("--label-a", default="Model A")
    parser.add_argument("--label-b", default="Model B")
    parser.add_argument("--web-url", default="http://127.0.0.1:8787")
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    args = parser.parse_args()
    try:
        prepared_a, results_a = load_directory(args.trial_a)
        prepared_b, results_b = load_directory(args.trial_b)
        comparison, report = compare_trials(prepared_a, results_a, prepared_b, results_b, args.label_a, args.label_b)
        # Validate link base before writing. Rendering itself never makes a request.
        web_url = validate_web_base(args.web_url)
        report = render_comparison_html(prepared_a, comparison, results_a, results_b, args.label_a, args.label_b, web_url)
        output_dir = private_output_dir(args.output_dir)
        write_private(output_dir / "comparison.json", (json.dumps(comparison, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))
        write_private(output_dir / "comparison.html", report.encode("utf-8"))
        by_task = " ".join(f"{task}={value['agreement']}/{value['cases']}" for task, value in sorted(comparison["by_task"].items()))
        print(f"cases={comparison['total_cases']} agreement={comparison['agreement']} disagreement={comparison['disagreement']} {by_task}")
        print("Private comparison files saved.")
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"trial comparison failed: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
