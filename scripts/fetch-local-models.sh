#!/usr/bin/env bash
# Download the pinned local model snapshots and verify the file digests.
# Explicit only: no test, launcher or server calls this automatically.
set -euo pipefail
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PYTHON=${PYTHON:-$ROOT/.venv-asr/bin/python}
if [ ! -x "$PYTHON" ]; then PYTHON=python3; fi
exec "$PYTHON" - "$ROOT/scripts/model-revisions.json" "$ROOT" <<'PY'
import hashlib
import json
import os
import sys
from pathlib import Path

from huggingface_hub import snapshot_download

pins_path = Path(sys.argv[1])
root = Path(sys.argv[2])
pins = json.loads(pins_path.read_text(encoding="utf-8"))["models"]
cache = Path(os.environ.get("HF_HOME", str(Path.home() / ".cache" / "huggingface"))) / "hub"


def digest(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            hasher.update(block)
    return hasher.hexdigest()


def tree_size(path: Path) -> int:
    return sum(item.stat().st_size for item in path.rglob("*") if item.is_file())


failed = False
for name, model in pins.items():
    print(f"fetching {name}: {model['repo_id']}@{model['revision']}", flush=True)
    path = Path(snapshot_download(repo_id=model["repo_id"], revision=model["revision"],
                                  cache_dir=cache, allow_patterns=None))
    expected = model.get("safetensors_sha256")
    status = "no digest pin"
    if expected:
        weights = path / "model.safetensors"
        if not weights.exists():
            status = f"MISSING {weights}"
            failed = True
        else:
            actual = digest(weights)
            if actual == expected:
                status = f"sha256 ok {actual[:16]}..."
            else:
                status = f"SHA256 MISMATCH actual={actual} expected={expected}"
                failed = True
    print(f"  path={path}")
    print(f"  size={tree_size(path) / (1024 ** 3):.2f} GiB")
    print(f"  {status}")
if failed:
    raise SystemExit("fetch-local-models: verification failed; do not run the model")
print("fetch-local-models: all pinned snapshots fetched; digest-pinned files verified where a SHA-256 pin exists")
PY
