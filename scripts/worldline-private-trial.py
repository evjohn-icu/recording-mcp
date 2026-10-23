#!/usr/bin/env python3
"""Prepare or run a local-only, read-only Worldline semantic trial.

Private source text and results are written under a mode-0700 directory; files
are mode 0600. Standard output contains aggregate counts only.
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
import random
import re
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import http.cookiejar
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
DECISION_RUNNER = ROOT / "scripts" / "worldline-decision-experiment.py"
SYNTHETIC_FIXTURE = ROOT / "fixtures" / "worldline-decision-experiment.json"
MAX_THREAD_SUMMARIES = 300
THREAD_PAGE_SIZE = 100
DETAIL_SAMPLE_PER_THREAD = 10
MAX_SOURCE_EVIDENCE_PER_CLAIM = 2
MAX_ALIAS_PAIRS = 16
MAX_SUMMARY_CASES = 24
MAX_SUPPORT_THREADS = 32
MAX_DETAIL_THREADS = 64
MAX_RESPONSE_BYTES = 2 * 1024 * 1024
MAX_TOTAL_API_BYTES = 64 * 1024 * 1024
DEFAULT_OUTPUT = ROOT / "data" / "worldline-private-trial"


def _load_decision_runner() -> Any:
    spec = importlib.util.spec_from_file_location("worldline_decision_experiment", DECISION_RUNNER)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load the shared decision runner")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


decision_runner = _load_decision_runner()
TASKS = decision_runner.TASKS
request_one = decision_runner.request_one
validate_answer = decision_runner.validate_answer


def _json_bytes(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")


def content_hash(value: Any) -> str:
    return hashlib.sha256(_json_bytes(value)).hexdigest()


DECISION_RUNNER_SHA256 = hashlib.sha256(DECISION_RUNNER.read_bytes()).hexdigest()
SYNTHETIC_FIXTURE_SHA256 = hashlib.sha256(SYNTHETIC_FIXTURE.read_bytes()).hexdigest()
TASK_PROMPT_SHA256 = content_hash({name: {"labels": labels, "instructions": instructions} for name, (labels, instructions) in TASKS.items()})


def validate_loopback_url(value: str, *, allow_path: bool = True) -> str:
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme != "http" or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("URL must be plain HTTP loopback without credentials, query, or fragment")
    try:
        address = ipaddress.ip_address(parsed.hostname)
    except ValueError as exc:
        raise ValueError("URL host must be a literal loopback IP address") from exc
    if not address.is_loopback:
        raise ValueError("URL host must be a literal loopback IP address")
    if not allow_path and parsed.path not in ("", "/"):
        raise ValueError("web URL must not include a path")
    if allow_path and parsed.path and parsed.path != "/":
        raise ValueError("URL must not include a path")
    return f"http://{parsed.netloc}"


class NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req: Any, fp: Any, code: int, msg: str, headers: Any, newurl: str) -> None:
        return None


class LocalWebClient:
    def __init__(self, web_url: str, timeout: float = 20.0):
        self.base = validate_loopback_url(web_url)
        self.timeout = timeout
        self.cookie_jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            NoRedirectHandler(),
            urllib.request.HTTPCookieProcessor(self.cookie_jar),
        )
        self.total_bytes = 0

    def reset_budget(self) -> None:
        self.total_bytes = 0

    def _request_json(self, method: str, path: str, payload: dict[str, Any] | None = None, extra_headers: dict[str, str] | None = None) -> dict[str, Any]:
        url = self.base + path
        headers = {"Accept": "application/json"}
        data = None
        if payload is not None:
            data = _json_bytes(payload)
            headers["Content-Type"] = "application/json"
        if extra_headers:
            headers.update(extra_headers)
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                body = response.read(MAX_RESPONSE_BYTES + 1)
                if len(body) > MAX_RESPONSE_BYTES:
                    raise ValueError("local API response exceeded the per-response size bound")
                self.total_bytes += len(body)
                if self.total_bytes > MAX_TOTAL_API_BYTES:
                    raise ValueError("local API export exceeded the total size bound")
                result = json.loads(body)
        except urllib.error.HTTPError as exc:
            raise RuntimeError(f"local API request failed with HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError) as exc:
            raise RuntimeError(f"local API request failed ({type(exc).__name__})") from None
        if not isinstance(result, dict):
            raise RuntimeError("local API returned an invalid JSON object")
        return result

    def login(self, profile: str, password: str) -> None:
        result = self._request_json("POST", "/api/profiles/login", {"profile": profile, "password": password}, {"Origin": self.base})
        if result.get("profile") != profile or not isinstance(result.get("csrf_token"), str):
            raise RuntimeError("profile login did not return the requested profile session")

    def get(self, path: str, params: dict[str, Any]) -> dict[str, Any]:
        query = urllib.parse.urlencode({k: str(v) for k, v in params.items() if v is not None})
        return self._request_json("GET", path + ("?" + query if query else ""))


def _record_page(pages: list[dict[str, Any]], path: str, params: dict[str, Any], result: dict[str, Any]) -> None:
    pages.append({
        "path": path,
        "params": {k: v for k, v in params.items() if v is not None},
        "version": result.get("version"),
        "digest": content_hash(result),
    })


def fetch_thread_summaries(client: LocalWebClient) -> tuple[list[dict[str, Any]], list[dict[str, Any]], str, bool]:
    summaries: list[dict[str, Any]] = []
    page_signatures: list[dict[str, Any]] = []
    expected_version: str | None = None
    offset = 0
    truncated = False
    while len(summaries) < MAX_THREAD_SUMMARIES:
        params = {"status": "all", "offset": offset, "limit": THREAD_PAGE_SIZE}
        page = client.get("/api/worldline/threads", params)
        version = page.get("version")
        if not isinstance(version, str) or not version:
            raise RuntimeError("thread list omitted its projection version")
        if expected_version is None:
            expected_version = version
        elif version != expected_version:
            raise RuntimeError("thread list changed during export; retry from the beginning")
        _record_page(page_signatures, "/api/worldline/threads", params, page)
        entries = page.get("threads")
        if not isinstance(entries, list):
            raise RuntimeError("thread list had an invalid shape")
        remaining = MAX_THREAD_SUMMARIES - len(summaries)
        summaries.extend(x for x in entries[:remaining] if isinstance(x, dict))
        next_offset = page.get("next_offset")
        if next_offset is None:
            break
        if not isinstance(next_offset, int) or next_offset <= offset:
            raise RuntimeError("thread list pagination did not advance")
        offset = next_offset
        if len(summaries) >= MAX_THREAD_SUMMARIES:
            truncated = True
            break
    return summaries, page_signatures, expected_version or "", truncated


def fetch_thread_detail(client: LocalWebClient, thread_id: str, limit: int = DETAIL_SAMPLE_PER_THREAD) -> tuple[dict[str, Any], dict[str, Any]]:
    path = "/api/worldline/threads/" + urllib.parse.quote(thread_id, safe="")
    params = {"status": "all", "entries_offset": 0, "unknown_offset": 0, "limit": limit}
    result = client.get(path, params)
    return result, {"path": path, "params": params, "version": result.get("version"), "digest": content_hash(result)}


def normalize_title(title: str) -> str:
    return " ".join(title.casefold().split())


def chinese_bigrams(title: str) -> set[str]:
    runs = re.findall(r"[\u3400-\u4dbf\u4e00-\u9fff]+", title)
    return {run[i:i + 2] for run in runs for i in range(len(run) - 1)}


def title_jaccard(left: str, right: str) -> float:
    a, b = chinese_bigrams(left), chinese_bigrams(right)
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def shortlist_alias_pairs(threads: list[dict[str, Any]], threshold: float = 0.2, cap: int = MAX_ALIAS_PAIRS, per_thread: int = 2) -> list[dict[str, Any]]:
    groups: dict[str, list[dict[str, Any]]] = collections.defaultdict(list)
    for item in threads:
        macro = item.get("macro_topic_id")
        title = item.get("title")
        if isinstance(macro, str) and isinstance(title, str) and title.strip():
            groups[macro].append(item)
    candidates: list[tuple[float, str, str, dict[str, Any], dict[str, Any]]] = []
    for macro in sorted(groups):
        values = sorted(groups[macro], key=lambda x: (normalize_title(str(x["title"])), str(x.get("id", ""))))
        for i, left in enumerate(values):
            for right in values[i + 1:]:
                if normalize_title(str(left["title"])) == normalize_title(str(right["title"])):
                    continue
                score = title_jaccard(str(left["title"]), str(right["title"]))
                if score >= threshold:
                    candidates.append((score, normalize_title(str(left["title"])), normalize_title(str(right["title"])), left, right))
    candidates.sort(key=lambda x: (-x[0], x[1], x[2], str(x[3].get("id", "")), str(x[4].get("id", ""))))
    degree: collections.Counter[str] = collections.Counter()
    chosen: list[dict[str, Any]] = []
    seen: set[tuple[str, str]] = set()
    for score, title_a, title_b, left, right in candidates:
        left_id, right_id = str(left.get("id", "")), str(right.get("id", ""))
        key = tuple(sorted((left_id, right_id)))
        if key in seen or degree[left_id] >= per_thread or degree[right_id] >= per_thread:
            continue
        seen.add(key)
        degree[left_id] += 1
        degree[right_id] += 1
        chosen.append({"left_thread": left, "right_thread": right, "jaccard": score, "title_key": [title_a, title_b]})
        if len(chosen) >= cap:
            break
    return chosen


def source_evidence(claim: dict[str, Any]) -> tuple[list[dict[str, Any]], list[dict[str, Any]], bool]:
    raw = claim.get("evidence")
    if not isinstance(raw, list):
        return [], [], False
    if len(raw) > MAX_SOURCE_EVIDENCE_PER_CLAIM:
        return [], [], False
    visible: list[dict[str, Any]] = []
    refs: list[dict[str, Any]] = []
    for item in raw[:MAX_SOURCE_EVIDENCE_PER_CLAIM]:
        if not isinstance(item, dict):
            return [], [], False
        text = item.get("text")
        turn_id, revision = item.get("turn_id"), item.get("revision")
        if (not isinstance(text, str) or not text.strip()
                or not isinstance(turn_id, str) or not turn_id.strip()
                or not isinstance(revision, str) or not revision.strip()):
            return [], [], False
        visible.append({"kind": "transcript", "text": text, "mentioned_at": claim.get("mentioned_at")})
        refs.append({
            "turn_id": turn_id,
            "revision": revision,
            "conversation_id": item.get("conversation_id") if isinstance(item.get("conversation_id"), str) else None,
            "recording_id": item.get("recording_id") if isinstance(item.get("recording_id"), str) else None,
            "offset_ms": item.get("offset_ms") if isinstance(item.get("offset_ms"), int) else None,
            "text": text,
        })
    return visible, refs, True


def _detail_claims(detail: dict[str, Any]) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    for field in ("entries", "unknown_entries"):
        rows = detail.get(field)
        if not isinstance(rows, list):
            continue
        for row in rows:
            claim = row.get("claim") if isinstance(row, dict) else None
            if isinstance(claim, dict):
                result.append(claim)
    return result


def _claim_title(detail: dict[str, Any], fallback: str) -> str:
    thread = detail.get("thread")
    title = thread.get("title") if isinstance(thread, dict) else None
    return title if isinstance(title, str) and title.strip() else fallback


def _claim_case_source(thread: dict[str, Any], claim: dict[str, Any], evidence_refs: list[dict[str, Any]]) -> dict[str, Any]:
    return {
        "thread_id": thread.get("id"),
        "title": thread.get("title"),
        "claim_id": claim.get("id"),
        "claim_version": claim.get("version"),
        "claim_kind": claim.get("kind"),
        "summary": claim.get("summary"),
        "evidence_refs": evidence_refs,
    }


def select_support_threads(summaries: list[dict[str, Any]], cap: int = MAX_SUPPORT_THREADS) -> list[dict[str, Any]]:
    groups: dict[str, list[dict[str, Any]]] = collections.defaultdict(list)
    for item in summaries:
        groups[str(item.get("macro_topic_id", "unknown"))].append(item)
    for values in groups.values():
        values.sort(key=lambda x: (normalize_title(str(x.get("title", ""))), str(x.get("id", ""))))
    macro_ids = sorted(groups)
    selected: list[dict[str, Any]] = []
    while macro_ids and len(selected) < cap:
        next_ids: list[str] = []
        for macro_id in macro_ids:
            if groups[macro_id]:
                selected.append(groups[macro_id].pop(0))
                if groups[macro_id]:
                    next_ids.append(macro_id)
                if len(selected) >= cap:
                    break
        macro_ids = next_ids
    return selected


def make_trial_cases(summaries: list[dict[str, Any]], details: dict[str, dict[str, Any]], support_thread_ids: set[str] | None = None) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    cases: list[dict[str, Any]] = []
    selected_threads = shortlist_alias_pairs(summaries)
    skipped_alias = 0
    skipped_evidence = 0
    alias_case_number = 0
    for pair in selected_threads:
        sides = []
        sources = []
        for key in ("left_thread", "right_thread"):
            thread = pair[key]
            detail = details.get(str(thread.get("id", "")), {})
            title = _claim_title(detail, str(thread.get("title", "")))
            chosen = None
            chosen_evidence: list[dict[str, Any]] = []
            chosen_refs: list[dict[str, Any]] = []
            for claim in _detail_claims(detail):
                evidence, refs, complete = source_evidence(claim)
                if not complete:
                    skipped_evidence += 1
                    continue
                if isinstance(claim.get("summary"), str) and claim["summary"].strip() and evidence:
                    chosen, chosen_evidence, chosen_refs = claim, evidence, refs
                    break
            if chosen is None:
                sides = []
                break
            sides.append({
                "title": title,
                "summary": chosen["summary"],
                "subject": None,
                "identity_status": "unconfirmed",
                "evidence": chosen_evidence,
            })
            sources.append(_claim_case_source(thread, chosen, chosen_refs))
        if len(sides) != 2:
            skipped_alias += 1
            continue
        alias_case_number += 1
        case_id = f"alias-{alias_case_number:02d}"
        case = {"id": case_id, "task": "alias_topic", "state": {"left": sides[0], "right": sides[1]}, "sources": sources}
        case["input_hash"] = content_hash({"state": case["state"], "sources": case["sources"]})
        cases.append(case)

    support_candidates: dict[str, dict[str, Any]] = {}
    support_summaries = [thread for thread in summaries if support_thread_ids is None or str(thread.get("id", "")) in support_thread_ids]
    for thread in support_summaries:
        thread_id = str(thread.get("id", ""))
        detail = details.get(thread_id, {})
        title = _claim_title(detail, str(thread.get("title", "")))
        for claim in _detail_claims(detail):
            summary = claim.get("summary")
            kind = claim.get("kind")
            if not isinstance(summary, str) or not summary.strip() or not isinstance(kind, str) or not kind.strip():
                continue
            evidence, refs, complete = source_evidence(claim)
            if not complete:
                skipped_evidence += 1
                continue
            source = _claim_case_source(thread, claim, refs)
            dedupe_key = content_hash({"kind": kind, "summary": summary, "evidence_refs": refs})
            support_candidates.setdefault(dedupe_key, {"thread": thread, "claim": claim, "title": title, "evidence": evidence, "refs": refs, "source": source, "dedupe": dedupe_key})

    by_kind: dict[str, list[dict[str, Any]]] = collections.defaultdict(list)
    for item in support_candidates.values():
        by_kind[str(item["claim"].get("kind"))].append(item)
    for values in by_kind.values():
        values.sort(key=lambda x: (normalize_title(x["title"]), str(x["claim"].get("summary", "")), x["dedupe"]))
    chosen_support: list[dict[str, Any]] = []
    kinds = sorted(by_kind)
    while kinds and len(chosen_support) < MAX_SUMMARY_CASES:
        next_kinds = []
        for kind in kinds:
            if by_kind[kind]:
                chosen_support.append(by_kind[kind].pop(0))
                if by_kind[kind]:
                    next_kinds.append(kind)
                if len(chosen_support) >= MAX_SUMMARY_CASES:
                    break
        kinds = next_kinds
    support_case_number = 0
    for item in chosen_support:
        claim = item["claim"]
        support_case_number += 1
        case_id = f"support-{support_case_number:02d}"
        state = {
            "summary": claim["summary"],
            "summary_kind": claim.get("kind"),
            "evidence": item["evidence"],
            "identity": {"summary_subject": None, "summary_confirmed": False, "evidence_subject": None, "evidence_confirmed": False},
        }
        case = {"id": case_id, "task": "summary_support", "state": state, "sources": [item["source"]]}
        case["input_hash"] = content_hash({"state": case["state"], "sources": case["sources"]})
        cases.append(case)

    stats = {
        "thread_summaries_seen": len(summaries),
        "thread_summary_cap": MAX_THREAD_SUMMARIES,
        "thread_summaries_truncated": len(summaries) >= MAX_THREAD_SUMMARIES,
        "thread_details_sampled": len(details),
        "support_threads_sampled": len(support_summaries),
        "detail_rows_per_thread_per_bucket": DETAIL_SAMPLE_PER_THREAD,
        "detail_thread_cap": MAX_DETAIL_THREADS,
        "alias_pairs_shortlisted": len(selected_threads),
        "alias_cases_exported": sum(c["task"] == "alias_topic" for c in cases),
        "alias_pairs_skipped_for_missing_source": skipped_alias,
        "claims_skipped_for_incomplete_or_over_cap_evidence": skipped_evidence,
        "summary_support_candidates_deduplicated": len(support_candidates),
        "summary_support_cases_exported": sum(c["task"] == "summary_support" for c in cases),
        "summary_support_cases_by_kind": dict(sorted(collections.Counter(str(c["state"].get("summary_kind")) for c in cases if c["task"] == "summary_support").items())),
        "caveat": "Title bigram shortlist has limited lexical recall; source statements require human review.",
    }
    return cases, stats


def prepare_export(client: LocalWebClient) -> dict[str, Any]:
    summaries, pages, version, truncated = fetch_thread_summaries(client)
    alias_pairs = shortlist_alias_pairs(summaries)
    alias_thread_ids = {
        str(pair[side].get("id", ""))
        for pair in alias_pairs
        for side in ("left_thread", "right_thread")
        if pair[side].get("id")
    }
    support_threads = select_support_threads(summaries)
    support_thread_ids = {str(item["id"]) for item in support_threads if item.get("id")}
    detail_thread_ids = alias_thread_ids | support_thread_ids
    if len(detail_thread_ids) > MAX_DETAIL_THREADS:
        raise RuntimeError("internal detail selection exceeded its hard bound")
    summaries_by_id = {str(item.get("id")): item for item in summaries if item.get("id")}
    details: dict[str, dict[str, Any]] = {}
    detail_pages: list[dict[str, Any]] = []
    for thread_id in sorted(detail_thread_ids):
        if thread_id not in summaries_by_id:
            continue
        detail, signature = fetch_thread_detail(client, thread_id, DETAIL_SAMPLE_PER_THREAD)
        if detail.get("version") != version:
            raise RuntimeError("thread detail version changed during export; retry from the beginning")
        details[thread_id] = detail
        detail_pages.append(signature)
    cases, stats = make_trial_cases(summaries, details, support_thread_ids)
    stats["thread_summaries_truncated"] = truncated
    stats["alias_candidate_thread_count"] = len(alias_thread_ids)
    stats["detail_threads_fetched"] = len(details)
    return {
        "schema": "worldline-private-trial-v1",
        "prepared_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "api_version": version,
        "decision_runner_sha256": DECISION_RUNNER_SHA256,
        "synthetic_fixture_sha256": SYNTHETIC_FIXTURE_SHA256,
        "task_prompt_sha256": TASK_PROMPT_SHA256,
        "page_checks": pages + detail_pages,
        "cases": cases,
        "stats": stats,
        "human_review_required": True,
        "no_gold_labels": True,
    }


def load_prepared(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict) or value.get("schema") != "worldline-private-trial-v1" or value.get("no_gold_labels") is not True:
        raise ValueError("prepared input has an unsupported format")
    cases = value.get("cases")
    if not isinstance(cases, list) or any(not isinstance(c, dict) or c.get("task") not in TASKS or not isinstance(c.get("state"), dict) for c in cases):
        raise ValueError("prepared input contains malformed cases")
    if any("expected" in case or "gold" in case for case in cases):
        raise ValueError("prepared input must not contain expected/gold labels")
    expected_hashes = {
        "decision_runner_sha256": DECISION_RUNNER_SHA256,
        "synthetic_fixture_sha256": SYNTHETIC_FIXTURE_SHA256,
        "task_prompt_sha256": TASK_PROMPT_SHA256,
    }
    if any(value.get(key) != expected for key, expected in expected_hashes.items()):
        raise ValueError("prepared input runner/fixture/prompt hash does not match this script version")
    for case in cases:
        expected_case_hash = content_hash({"state": case["state"], "sources": case.get("sources", [])})
        if case.get("input_hash") != expected_case_hash:
            raise ValueError("prepared input case hash validation failed")
    return value


def verify_prepared_snapshot(client: LocalWebClient, prepared: dict[str, Any]) -> tuple[bool, str]:
    client.reset_budget()
    for page in prepared.get("page_checks", []):
        if not isinstance(page, dict) or not isinstance(page.get("path"), str) or not isinstance(page.get("params"), dict):
            return False, "prepared source snapshot manifest is malformed"
        if (page["path"] != "/api/worldline/threads" and not page["path"].startswith("/api/worldline/threads/")) or "?" in page["path"] or "#" in page["path"]:
            return False, "prepared source snapshot references an unexpected API path"
        try:
            current = client.get(page["path"], page["params"])
        except Exception as exc:
            return False, f"could not re-read a source page ({type(exc).__name__})"
        if current.get("version") != page.get("version") or content_hash(current) != page.get("digest"):
            return False, "a source page changed since preparation"
    return True, ""


def private_output_dir(path: Path) -> Path:
    if path.exists() and path.is_symlink():
        raise ValueError("output directory may not be a symlink")
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(path, 0o700)
    if not path.is_dir():
        raise ValueError("output path is not a directory")
    return path


def write_private(path: Path, content: bytes) -> None:
    fd, temp_name = tempfile.mkstemp(prefix=".worldline-trial-", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as output:
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temp_name, path)
        os.chmod(path, 0o600)
    except Exception:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def write_json_private(path: Path, value: Any) -> None:
    write_private(path, (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))


def evaluate_cases(prepared: dict[str, Any], endpoint: str, model: str | None, timeout: float) -> dict[str, Any]:
    endpoint = decision_runner.validate_loopback_endpoint(endpoint)
    case_results: list[dict[str, Any]] = []
    model_names: set[str] = set()
    weights: set[str] = set()
    latencies: list[float] = []
    invalid = 0
    for case in prepared["cases"]:
        result: dict[str, Any] = {"id": case["id"], "task": case["task"], "input_hash": case["input_hash"]}
        try:
            response, elapsed = request_one(endpoint, model, case, timeout)
            actual_model = response.get("model")
            weights_version = response.get("weights_version")
            if not isinstance(actual_model, str) or not actual_model.strip() or not isinstance(weights_version, str) or not weights_version.strip():
                raise ValueError("engine identity missing")
            if "synthetic" in actual_model.lower() or "sandbox" in actual_model.lower() or "synthetic" in weights_version.lower() or "sandbox" in weights_version.lower():
                raise ValueError("engine identity is not a production model")
            model_names.add(actual_model)
            weights.add(weights_version)
            labels = set(TASKS[case["task"]][0])
            answer_map = response.get("answers")
            answer = answer_map.get(case["id"]) if isinstance(answer_map, dict) else None
            choice, distribution, error = validate_answer(answer, labels)
            latencies.append(elapsed)
            if error:
                invalid += 1
                result["error"] = error
            else:
                result["prediction"] = choice
                result["distribution"] = distribution
                result["latency_ms"] = round(elapsed, 2)
        except Exception as exc:
            invalid += 1
            result["error"] = f"evaluation_failed:{type(exc).__name__}"
        case_results.append(result)
    return {
        "schema": "worldline-private-trial-results-v1",
        "prepared_input_hash": content_hash({"api_version": prepared.get("api_version"), "cases": [{"id": x["id"], "input_hash": x["input_hash"]} for x in prepared["cases"]]}),
        "decision_runner_sha256": DECISION_RUNNER_SHA256,
        "synthetic_fixture_sha256": SYNTHETIC_FIXTURE_SHA256,
        "task_prompt_sha256": TASK_PROMPT_SHA256,
        "no_gold_labels": True,
        "valid_predictions": sum("prediction" in x for x in case_results),
        "invalid_or_failed": invalid,
        "actual_models": sorted(model_names),
        "weights_versions": sorted(weights),
        "latency_ms": {"mean": round(sum(latencies) / len(latencies), 2) if latencies else None, "max": round(max(latencies), 2) if latencies else None},
        "cases": case_results,
    }


def _local_conversation_href(web_url: str, conversation_id: Any) -> str | None:
    if not isinstance(conversation_id, str) or not conversation_id:
        return None
    base = validate_loopback_url(web_url)
    return base + "/c/" + urllib.parse.quote(conversation_id, safe="")


def render_report(prepared: dict[str, Any], web_url: str, results: dict[str, Any] | None = None, stale: bool = False, stale_reason: str = "") -> str:
    escaped_web_url = html.escape(validate_loopback_url(web_url), quote=True)
    result_by_id = {x.get("id"): x for x in (results or {}).get("cases", []) if isinstance(x, dict)}
    parts = ["<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Worldline 私有语义试验</title>",
             "<style>body{font:15px/1.55 system-ui,sans-serif;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#223}h1{font-size:1.6rem}.notice{padding:1rem;background:#fff5d9;border:1px solid #d7bd70;border-radius:8px}.stale{padding:1rem;background:#ffe3e3;border:2px solid #a33;color:#700}article{border-top:1px solid #ddd;padding:1rem 0}blockquote{margin:.4rem 0;padding-left:.8rem;border-left:3px solid #ccd;color:#465}small{color:#667}.case-source{margin:.6rem 0;padding:.7rem;background:#f4f7f8;border-radius:6px}.label{font-weight:700}</style><body>",
             "<h1>Worldline 私有语义试验</h1>",
             "<p class=\"notice\">候选由有限主题标题词面筛选；模型输出未经人工核对，不代表事实或有效建议。此报告不包含 gold label 或真实准确率，也不会修改记录、主题或事实。</p>"]
    if stale:
        parts.append(f"<p class=\"stale\"><strong>源数据已变化，结果作废。</strong> {html.escape(stale_reason)}</p>")
    elif results is not None:
        names = ", ".join(html.escape(x) for x in results.get("actual_models", [])) or "未返回模型名称"
        weights = ", ".join(html.escape(x) for x in results.get("weights_versions", [])) or "未返回权重版本"
        parts.append(f"<p>模型：{names}。权重：{weights}。有效响应 {int(results.get('valid_predictions', 0))}；失败/无效 {int(results.get('invalid_or_failed', 0))}。</p>")
    else:
        parts.append("<p>准备阶段：尚未调用模型。请先审核导出样本，再决定是否运行。</p>")
    stats = prepared.get("stats", {})
    parts.append("<h2>候选范围</h2><ul>")
    for key, label in (("thread_summaries_seen", "读取主题数"), ("thread_summaries_truncated", "主题列表达到上限"), ("alias_pairs_shortlisted", "标题近似候选对"), ("alias_cases_exported", "导出主题对样本"), ("summary_support_cases_exported", "导出摘要核对样本")):
        parts.append(f"<li>{html.escape(label)}：{html.escape(str(stats.get(key, 0)))}</li>")
    parts.append("</ul><p>标题近似使用中文二元词交并比阈值 0.2，最多读取 300 个主题、每主题最多两个近邻、总计最多 16 对；词面方法会漏掉同义标题。摘要核对先按宏分类轮流取最多 32 个主题，再按 claim kind 轮流抽样、去重后最多 24 条。总详情请求不超过 64 个主题；每个主题只取前 10 条有提及时间和前 10 条提及时间未知记录。每条 claim 只有 1–2 条当前证据时才纳入；证据缺失、引用不完整或超过两条的项目会跳过并计数，不会静默截断。</p>")
    for case in prepared.get("cases", []):
        if not isinstance(case, dict):
            continue
        case_id = html.escape(str(case.get("id", "")))
        task = html.escape(str(case.get("task", "")))
        parts.append(f"<article><h3>{case_id} · {task}</h3>")
        output = result_by_id.get(case.get("id"), {})
        if results is not None and not stale:
            prediction = output.get("prediction")
            if isinstance(prediction, str):
                parts.append(f"<p class=\"label\">待人工核对的模型输出：{html.escape(prediction)}</p>")
            else:
                parts.append("<p>模型未返回有效分类。</p>")
        sources = case.get("sources", [])
        for source in sources:
            if not isinstance(source, dict):
                continue
            parts.append("<section class=\"case-source\">")
            parts.append(f"<strong>{html.escape(str(source.get('title') or ''))}</strong>")
            parts.append(f"<p>{html.escape(str(source.get('summary') or ''))} <small>kind: {html.escape(str(source.get('claim_kind') or ''))}</small></p>")
            for evidence in source.get("evidence_refs", []):
                if not isinstance(evidence, dict):
                    continue
                parts.append(f"<blockquote>{html.escape(str(evidence.get('text') or ''))}</blockquote>")
                href = _local_conversation_href(web_url, evidence.get("conversation_id"))
                if href:
                    parts.append(f"<a href=\"{html.escape(href, quote=True)}\">在本机打开来源会话</a>")
            parts.append("</section>")
        parts.append("</article>")
    parts.append(f"<footer><small>本机服务：{escaped_web_url} · 内容仅保存在私有输出目录。</small></footer></body></html>")
    return "".join(parts)


def _summary_counts(prepared: dict[str, Any], results: dict[str, Any] | None = None, stale: bool = False) -> str:
    stats = prepared.get("stats", {})
    counts = collections.Counter(case.get("task") for case in prepared.get("cases", []) if isinstance(case, dict))
    valid = results.get("valid_predictions", 0) if results else 0
    return f"mode={'stale' if stale else 'evaluated' if results else 'prepared'} cases={len(prepared.get('cases', []))} alias={counts.get('alias_topic', 0)} summary_support={counts.get('summary_support', 0)} threads={stats.get('thread_summaries_seen', 0)} valid_predictions={valid}"


def _write_outputs(output_dir: Path, prepared: dict[str, Any], web_url: str, results: dict[str, Any] | None = None, stale: bool = False, stale_reason: str = "") -> None:
    write_json_private(output_dir / "trial-input.json", prepared)
    if results is not None:
        results["stale"] = stale
        results["stale_reason"] = stale_reason if stale else ""
        write_json_private(output_dir / "trial-results.json", results)
    report = render_report(prepared, web_url, results, stale, stale_reason)
    write_private(output_dir / "report.html", report.encode("utf-8"))


def run_cli(args: argparse.Namespace) -> int:
    web_url = validate_loopback_url(args.web_url)
    if not args.profile:
        raise ValueError("--profile is required")
    if not args.password_file:
        raise ValueError("--password-file is required")
    password = Path(args.password_file).read_text(encoding="utf-8").rstrip("\r\n")
    if not password:
        raise ValueError("password file is empty")
    client = LocalWebClient(web_url, args.timeout)
    client.login(args.profile, password)
    requested_output = Path(args.output_dir).resolve()
    ignored_data = (ROOT / "data").resolve()
    if not requested_output.is_relative_to(ignored_data):
        raise ValueError("--output-dir must be under the ignored repository data directory")
    output_dir = private_output_dir(requested_output)

    if args.prepared_input:
        prepared_path = Path(args.prepared_input).resolve()
        if not prepared_path.is_relative_to(ignored_data):
            raise ValueError("--prepared-input must be under the ignored repository data directory")
        prepared = load_prepared(prepared_path)
    else:
        prepared = None
    if prepared is None:
        prepared = prepare_export(client)
    if args.prepare_only:
        _write_outputs(output_dir, prepared, web_url)
        print(_summary_counts(prepared))
        return 0
    if not args.decision_endpoint:
        raise ValueError("--decision-endpoint is required unless --prepare-only is set")
    decision_runner.validate_loopback_endpoint(args.decision_endpoint)
    before_ok, before_reason = verify_prepared_snapshot(client, prepared)
    if not before_ok:
        results = {"cases": [], "valid_predictions": 0, "invalid_or_failed": 0, "actual_models": [], "weights_versions": []}
        _write_outputs(output_dir, prepared, web_url, results, stale=True, stale_reason=before_reason)
        print(_summary_counts(prepared, results, stale=True))
        return 3
    results = evaluate_cases(prepared, args.decision_endpoint, args.model, args.timeout)
    after_ok, after_reason = verify_prepared_snapshot(client, prepared)
    stale = not after_ok
    _write_outputs(output_dir, prepared, web_url, results, stale=stale, stale_reason=after_reason)
    print(_summary_counts(prepared, results, stale))
    return 3 if stale else 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--web-url", default="http://127.0.0.1:8790")
    parser.add_argument("--profile")
    parser.add_argument("--password-file", type=Path)
    parser.add_argument("--decision-endpoint")
    parser.add_argument("--model")
    parser.add_argument("--output-dir", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--prepared-input", type=Path, help="Reuse the exact prepared input for another model run")
    parser.add_argument("--prepare-only", action="store_true", help="Export and render the fixed sample without calling a model")
    parser.add_argument("--timeout", type=float, default=45.0)
    return parser


def main() -> int:
    try:
        return run_cli(build_parser().parse_args())
    except Exception as exc:
        # Exception messages are intentionally suppressed; no source text or
        # server response bodies are sent to stdout/stderr.
        print(f"private trial failed ({type(exc).__name__}); no source text emitted", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
