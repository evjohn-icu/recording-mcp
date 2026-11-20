#!/usr/bin/env python3
"""Git snapshot tests for scripts/check_context_engine_release.py (plan 7.6).

The checker must pin its verdict to the real worktree before it publishes:

- ``decision.evidence_snapshot.candidate_head`` must be the repository HEAD, or
  an ancestor of HEAD whose ``git diff --name-only -z <candidate> HEAD``
  touches nothing but the evidence document itself (a pure report publish,
  compared with ``--no-renames`` semantics so a file renamed *into* the report
  path cannot masquerade as an evidence-only change).
- ``uncommitted_diff_sha256`` must equal the worktree fingerprint
  ``sha256(status_bytes + diff_bytes + untracked_bytes)`` where both git
  commands run ``-- . ':(exclude,literal)<report path>'`` and untracked
  entries are hashed by raw bytes in raw-byte path order, with untracked
  symlinks hashed by their link-target bytes.
- Unreadable git, unknown candidates, drifted fingerprints and drifted source
  hashes must fail the run with a descriptive error instead of a verdict.

``--print-worktree-fingerprint`` is the producer side: it prints the current
``{"candidate_head", "uncommitted_diff_sha256"}`` so the operator can refresh
the report, therefore it must succeed without enforcing the recorded digest
and while the worktree has already drifted.

Every case drives the actual CLI inside a throwaway git repository, so the
flags, exit codes and verdicts the operator and CI rely on are pinned end to
end. Run it focused with:

    python3.12 -m unittest discover -s scripts -p 'test_context_engine_release_git.py' -v
"""
from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CHECKER_PATH = ROOT / "scripts" / "check_context_engine_release.py"
SPEC = importlib.util.spec_from_file_location("release_check_git", CHECKER_PATH)
checker = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(checker)

REPORT_REL = "docs/engineering/context-engine-v04-release-results.json"
OLD_REPORT_REL = "docs/engineering/old-report.json"
CLEAN_DIGEST = hashlib.sha256(b"").hexdigest()
UNKNOWN_HEAD = "0" * 40


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _git(root: Path, *args: str) -> bytes:
    proc = subprocess.run(["git", "-C", os.fspath(root), *args], capture_output=True)
    if proc.returncode != 0:
        raise AssertionError("fixture git " + " ".join(args) + " failed: "
                             + proc.stderr.decode("utf-8", "replace").strip())
    return proc.stdout


def _current_head(root: Path) -> str:
    return _git(root, "rev-parse", "HEAD").strip().decode("ascii")


def _diff_names(root: Path, base: str, head: str, *extra: str) -> list[str]:
    out = _git(root, "diff", *extra, "--name-only", "-z", base, head)
    return [name.decode("utf-8", "surrogateescape") for name in out.split(b"\0") if name]


def _entry_payload(full: bytes) -> bytes:
    """Bytes whose SHA-256 the fingerprint must hash for one untracked entry."""
    if stat.S_ISLNK(os.lstat(full).st_mode):
        return os.readlink(full)
    with open(full, "rb") as handle:
        return handle.read()


def reference_fingerprint(root: Path, report_rel: str = REPORT_REL) -> tuple[str, str]:
    """The plan's fingerprint definition, computed independently of the CLI."""
    pathspec = ["--", ".", f":(exclude,literal){report_rel}"]
    status = _git(root, "status", "--porcelain=v1", "-z", "--untracked-files=all", *pathspec)
    diff = _git(root, "diff", "HEAD", "--binary", "--no-ext-diff", "--no-color", *pathspec)
    untracked_paths: list[bytes] = []
    fields = status.split(b"\0")
    index = 0
    while index < len(fields):
        entry = fields[index]
        index += 1
        if not entry:
            break
        if entry.startswith(b"?? "):
            untracked_paths.append(entry[3:])
        if len(entry) >= 2 and (entry[0:1] in (b"R", b"C") or entry[1:2] in (b"R", b"C")):
            index += 1  # rename/copy records pair the original path as a second field
    untracked = bytearray()
    for raw_name in sorted(untracked_paths):
        payload = _entry_payload(os.path.join(os.fsencode(root), raw_name))
        untracked += raw_name + b"\0" + hashlib.sha256(payload).hexdigest().encode("ascii") + b"\0"
    digest = hashlib.sha256(bytes(status) + bytes(diff) + bytes(untracked)).hexdigest()
    return _current_head(root), digest


def _materialize(root: Path) -> None:
    (root / "internal" / "app").mkdir(parents=True, exist_ok=True)
    (root / "internal" / "app" / "service.go").write_text("package app\n", encoding="utf-8")
    (root / "Makefile").write_text("all:\n", encoding="utf-8")
    (root / "docs" / "engineering").mkdir(parents=True, exist_ok=True)
    (root / "docs" / "engineering" / "evidence.json").write_text('{"ok": true}\n', encoding="utf-8")


def write_document(root: Path, *, head: str | None = None, digest: str | None = None,
                   source_digest: str | None = None) -> Path:
    """Rewrite the evidence document; defaults track the current worktree."""
    if head is None:
        head = _current_head(root)
    if digest is None:
        digest = reference_fingerprint(root)[1]
    source = root / "internal" / "app" / "service.go"
    if source_digest is None:
        source_digest = sha(source)
    document = {
        "schema": checker.SCHEMA,
        "results": [
            {"id": "go.test", "status": "PASS", "evidence_kind": "current",
             "command": "go test ./internal/app", "exit_code": 0,
             "artifact": "docs/engineering/evidence.json",
             "artifact_sha256": sha(root / "docs" / "engineering" / "evidence.json")},
            {"id": "audio.gold", "status": "NOT_RUN", "evidence_kind": "current",
             "reason": "operator has not returned the reviewed gold"},
        ],
        "decision": {
            "schema": checker.DECISION_SCHEMA,
            "merge_readiness": "READY",
            "release_readiness": "BLOCKED",
            "production_ready_recommendation": False,
            "scope": {"retrieval": "v1 + hash", "v3": "closed"},
            "blockers": [{"id": "human-gold", "detail": "human gold is pending",
                          "blocks_merge": False, "blocks_release": True}],
            "required_for_merge": ["go.test"],
            "required_for_release": ["go.test", "audio.gold"],
            "evidence_snapshot": {
                "generated_at": "2026-11-13T00:00:00Z",
                "candidate_head": head,
                "uncommitted_diff_sha256": digest,
                "source_sha256": {"internal/app/service.go": source_digest},
                "config_sha256": {"Makefile": sha(root / "Makefile")},
            },
        },
    }
    path = root / REPORT_REL
    path.write_text(json.dumps(document, indent=1), encoding="utf-8")
    return path


def build_repo(root: Path) -> str:
    """A one-commit repository whose report and tracked scratch report exist."""
    root.mkdir(parents=True)
    _git(root, "init", "-q", "-b", "main")
    _git(root, "config", "user.email", "release-fixture@example.com")
    _git(root, "config", "user.name", "Release Fixture")
    _git(root, "config", "diff.renames", "true")
    _materialize(root)
    placeholder = write_document(root, head=UNKNOWN_HEAD, digest=CLEAN_DIGEST)
    (root / OLD_REPORT_REL).write_text(placeholder.read_text(encoding="utf-8"), encoding="utf-8")
    _git(root, "add", "-A")
    _git(root, "commit", "-q", "-m", "base")
    return _current_head(root)


class ReleaseGitSnapshotTests(unittest.TestCase):
    """Plan 7.6: the CLI verifies the evidence snapshot against real git."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory(prefix="release-git-fixture-")
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        self.root = self.tmp / "repo"
        self.head = build_repo(self.root)

    # -- CLI helpers ---------------------------------------------------------

    def _describe(self, proc: subprocess.CompletedProcess) -> str:
        return (f"rc={proc.returncode} stdout={proc.stdout.decode('utf-8', 'replace')!r} "
                f"stderr={proc.stderr.decode('utf-8', 'replace')!r}")

    def _cli(self, root: Path, document: Path | str, *extra: str,
             cwd: Path | None = None) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, os.fspath(CHECKER_PATH), os.fspath(document),
             "--root", os.fspath(root), *extra],
            capture_output=True, cwd=os.fspath(cwd if cwd is not None else ROOT))

    def _check_json(self, root: Path, document: Path | str, cwd: Path | None = None
                    ) -> tuple[int, dict]:
        proc = self._cli(root, document, "--json", cwd=cwd)
        try:
            payload = json.loads(proc.stdout)
        except ValueError:
            self.fail("CLI did not emit a JSON verdict: " + self._describe(proc))
        return proc.returncode, payload

    def _flag(self, root: Path) -> dict:
        proc = self._cli(root, root / REPORT_REL, "--print-worktree-fingerprint")
        self.assertEqual(proc.returncode, 0, "--print-worktree-fingerprint failed: "
                         + self._describe(proc))
        try:
            payload = json.loads(proc.stdout)
        except ValueError:
            self.fail("--print-worktree-fingerprint did not print JSON: " + self._describe(proc))
        self.assertEqual(set(payload), {"candidate_head", "uncommitted_diff_sha256"},
                         f"unexpected fingerprint keys: {sorted(payload)}")
        return payload

    def _assert_pass(self, code: int, payload: dict) -> None:
        self.assertEqual(code, 0, json.dumps(payload, indent=1))
        self.assertEqual(payload.get("status"), "PASS")

    def _assert_failure(self, payload: dict, *fragments: str) -> None:
        self.assertEqual(payload.get("status"), "FAIL", json.dumps(payload, indent=1))
        errors = payload.get("errors") or []
        matched = [error for error in errors if any(f in error.lower() for f in fragments)]
        self.assertTrue(matched, f"expected an error mentioning {fragments}; got {errors}")

    # -- entry point ---------------------------------------------------------

    def test_verify_git_snapshot_entry_point_exists(self):
        self.assertTrue(callable(getattr(checker, "verify_git_snapshot", None)),
                        "main() must call verify_git_snapshot(snapshot, root, document_path); "
                        "the checker has no such entry point")

    # -- full check, happy paths --------------------------------------------

    def test_a_valid_document_against_the_current_head_passes(self):
        write_document(self.root)
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self._assert_pass(code, payload)

    def test_relative_document_path_with_root_dot_passes(self):
        write_document(self.root)
        code, payload = self._check_json(self.root, REPORT_REL, cwd=self.root)
        self._assert_pass(code, payload)

    def test_evidence_only_publish_commit_is_accepted(self):
        """A commit that touches only the report may postdate the candidate."""
        write_document(self.root)
        _git(self.root, "add", REPORT_REL)
        _git(self.root, "commit", "-q", "-m", "publish report")
        self.assertNotEqual(_current_head(self.root), self.head)
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self._assert_pass(code, payload)

    # -- full check, structural regressions already covered pre-7.6 ----------

    def test_source_hash_drift_is_rejected(self):
        document = write_document(self.root)
        recorded = (json.loads(document.read_text(encoding="utf-8"))["decision"]
                    ["evidence_snapshot"]["source_sha256"]["internal/app/service.go"])
        (self.root / "internal" / "app" / "service.go").write_text("package app\n// drift\n",
                                                                  encoding="utf-8")
        write_document(self.root, head=self.head, source_digest=recorded)
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "hash does not match internal/app/service.go")

    # -- fingerprint flag ----------------------------------------------------

    def test_the_fingerprint_flag_matches_the_spec_on_a_drifted_worktree(self):
        """The flag is a producer: it reports current truth, exit 0, no verdict."""
        (self.root / "scratch").mkdir()
        (self.root / "scratch" / "notes.txt").write_text("drift\n", encoding="utf-8")
        payload = self._flag(self.root)
        head, digest = reference_fingerprint(self.root)
        self.assertEqual(payload["candidate_head"], head)
        self.assertEqual(payload["uncommitted_diff_sha256"], digest)

    def test_report_edit_does_not_change_the_fingerprint(self):
        before = self._flag(self.root)
        document = self.root / REPORT_REL
        document.write_text(document.read_text(encoding="utf-8") + "\n<!-- refreshed -->\n",
                            encoding="utf-8")
        after = self._flag(self.root)
        self.assertEqual(after["uncommitted_diff_sha256"], before["uncommitted_diff_sha256"])
        _head, digest = reference_fingerprint(self.root)
        self.assertEqual(after["uncommitted_diff_sha256"], digest)

    def test_code_edit_changes_the_fingerprint(self):
        before = self._flag(self.root)
        (self.root / "internal" / "app" / "service.go").write_text("package app\n// tuned\n",
                                                                  encoding="utf-8")
        after = self._flag(self.root)
        self.assertNotEqual(after["uncommitted_diff_sha256"], before["uncommitted_diff_sha256"])
        _head, digest = reference_fingerprint(self.root)
        self.assertEqual(after["uncommitted_diff_sha256"], digest)
        document = self.root / REPORT_REL
        document.write_text(document.read_text(encoding="utf-8") + "\n<!-- refreshed -->\n",
                            encoding="utf-8")
        with_report_edit = self._flag(self.root)
        self.assertEqual(with_report_edit["uncommitted_diff_sha256"],
                         after["uncommitted_diff_sha256"])

    def test_untracked_same_name_content_change_is_hashed(self):
        scratch = self.root / "scratch"
        scratch.mkdir()
        first = self._flag(self.root)
        (scratch / "notes.txt").write_text("one\n", encoding="utf-8")
        with_file = self._flag(self.root)
        self.assertNotEqual(with_file["uncommitted_diff_sha256"], first["uncommitted_diff_sha256"])
        (scratch / "notes.txt").write_text("two\n", encoding="utf-8")
        with_new_content = self._flag(self.root)
        self.assertNotEqual(with_new_content["uncommitted_diff_sha256"],
                            with_file["uncommitted_diff_sha256"])
        (scratch / "other.txt").write_text("more\n", encoding="utf-8")
        with_second_file = self._flag(self.root)
        self.assertNotEqual(with_second_file["uncommitted_diff_sha256"],
                            with_new_content["uncommitted_diff_sha256"])
        _head, digest = reference_fingerprint(self.root)
        self.assertEqual(with_second_file["uncommitted_diff_sha256"], digest)

    def test_untracked_symlink_is_hashed_by_link_target_bytes(self):
        scratch = self.root / "scratch"
        scratch.mkdir()
        (scratch / "real.txt").write_text("payload\n", encoding="utf-8")
        (scratch / "twin.txt").write_text("payload\n", encoding="utf-8")
        link = scratch / "link"
        os.symlink("real.txt", link)
        with_link = self._flag(self.root)
        link.unlink()
        os.symlink("twin.txt", link)
        repointed = self._flag(self.root)
        # Identical target contents; only the link-target string moved.
        self.assertNotEqual(repointed["uncommitted_diff_sha256"],
                            with_link["uncommitted_diff_sha256"])
        link.unlink()
        os.symlink("missing.txt", link)
        dangling = self._flag(self.root)
        self.assertNotEqual(dangling["uncommitted_diff_sha256"],
                            repointed["uncommitted_diff_sha256"])
        _head, digest = reference_fingerprint(self.root)
        self.assertEqual(dangling["uncommitted_diff_sha256"], digest)

    def test_an_external_report_keeps_the_whole_repo_in_scope(self):
        """A report outside --root excludes nothing: repo drift must still count."""
        external = self.tmp / "external"
        _materialize(external)
        document = write_document(external, head=self.head, digest=CLEAN_DIGEST)
        code, payload = self._check_json(self.root, document)
        self._assert_pass(code, payload)
        report = self.root / REPORT_REL
        report.write_text(report.read_text(encoding="utf-8") + "\n<!-- repo edit -->\n",
                          encoding="utf-8")
        code, payload = self._check_json(self.root, document)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "fingerprint", "uncommitted_diff")

    def test_a_renamed_path_is_not_miscounted_as_untracked(self):
        """Porcelain rename pairs must not leak their second field into untracked."""
        with open(os.path.join(os.fsencode(self.root), b"?? old.txt"), "wb") as handle:
            handle.write(b"tracked\n")
        _git(self.root, "add", "-A")
        _git(self.root, "commit", "-q", "-m", "track an oddly named file")
        _git(self.root, "mv", "--", "?? old.txt", "renamed.txt")
        payload = self._flag(self.root)
        head, digest = reference_fingerprint(self.root)
        self.assertEqual(payload["candidate_head"], head)
        self.assertEqual(payload["uncommitted_diff_sha256"], digest)

    def test_non_utf8_untracked_name_is_hashed_by_raw_bytes(self):
        scratch = self.root / "scratch"
        scratch.mkdir()
        before = self._flag(self.root)
        with open(os.path.join(os.fsencode(scratch), b"bad\xffname.bin"), "wb") as handle:
            handle.write(b"binary\n")
        after = self._flag(self.root)
        self.assertNotEqual(after["uncommitted_diff_sha256"], before["uncommitted_diff_sha256"])
        _head, digest = reference_fingerprint(self.root)
        self.assertEqual(after["uncommitted_diff_sha256"], digest)

    def test_a_commit_moves_candidate_head_and_keeps_the_digest(self):
        first = self._flag(self.root)
        self.assertEqual(first["candidate_head"], self.head)
        (self.root / "internal" / "app" / "service.go").write_text("package app\n// tuned\n",
                                                                  encoding="utf-8")
        _git(self.root, "add", "internal/app/service.go")
        _git(self.root, "commit", "-q", "-m", "tune service")
        code_head = _current_head(self.root)
        committed = self._flag(self.root)
        self.assertEqual(committed["candidate_head"], code_head)
        self.assertEqual(committed["uncommitted_diff_sha256"], CLEAN_DIGEST)
        document = self.root / REPORT_REL
        document.write_text(document.read_text(encoding="utf-8") + "\n<!-- refreshed -->\n",
                            encoding="utf-8")
        _git(self.root, "add", REPORT_REL)
        _git(self.root, "commit", "-q", "-m", "publish report")
        published = self._flag(self.root)
        self.assertEqual(published["candidate_head"], _current_head(self.root))
        self.assertEqual(published["uncommitted_diff_sha256"],
                         committed["uncommitted_diff_sha256"])

    # -- full check, snapshot verification failures --------------------------

    def test_worktree_drift_fails_the_full_check(self):
        write_document(self.root, digest=CLEAN_DIGEST)
        scratch = self.root / "scratch"
        scratch.mkdir()
        (scratch / "notes.txt").write_text("uncommitted\n", encoding="utf-8")
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "fingerprint", "uncommitted_diff")

    def test_stale_head_after_a_code_commit_is_rejected(self):
        (self.root / "internal" / "app" / "service.go").write_text("package app\n// tuned\n",
                                                                  encoding="utf-8")
        _git(self.root, "add", "internal/app/service.go")
        _git(self.root, "commit", "-q", "-m", "tune service")
        write_document(self.root, head=self.head)
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "stale")

    def test_stale_head_when_the_candidate_diverged_is_rejected(self):
        _git(self.root, "checkout", "-q", "-b", "side")
        (self.root / "internal" / "app" / "service.go").write_text("package app\n// side\n",
                                                                  encoding="utf-8")
        _git(self.root, "add", "internal/app/service.go")
        _git(self.root, "commit", "-q", "-m", "side commit")
        side_head = _current_head(self.root)
        _git(self.root, "checkout", "-q", "main")
        write_document(self.root, head=side_head)
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "stale")

    def test_a_rename_into_the_report_path_is_not_evidence_only(self):
        """Rename detection would list only the destination; it must stay stale."""
        _git(self.root, "rm", "-q", REPORT_REL)
        _git(self.root, "commit", "-q", "-m", "drop the report from the candidate")
        base = _current_head(self.root)
        write_document(self.root, head=base, digest=CLEAN_DIGEST)
        os.unlink(self.root / OLD_REPORT_REL)
        _git(self.root, "add", "-A")
        _git(self.root, "commit", "-q", "-m", "rename scratch report into the report path")
        rename_head = _current_head(self.root)
        naive = _diff_names(self.root, base, rename_head)
        self.assertEqual(naive, [REPORT_REL],
                         "fixture lost the trap: rename-aware name-only must show the report only")
        strict = _diff_names(self.root, base, rename_head, "--no-renames")
        self.assertEqual(set(strict), {REPORT_REL, OLD_REPORT_REL},
                         "fixture lost the trap: --no-renames must expose both paths")
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "stale")

    def test_an_unknown_candidate_commit_is_rejected(self):
        write_document(self.root, head="f" * 40)
        code, payload = self._check_json(self.root, self.root / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "candidate", "unknown")

    def test_an_unreadable_git_root_is_rejected(self):
        plain = self.tmp / "plain"
        _materialize(plain)
        write_document(plain, head=UNKNOWN_HEAD, digest=CLEAN_DIGEST)
        code, payload = self._check_json(plain, plain / REPORT_REL)
        self.assertEqual(code, 1, json.dumps(payload, indent=1))
        self._assert_failure(payload, "git", "snapshot", "repository")


if __name__ == "__main__":
    unittest.main()
