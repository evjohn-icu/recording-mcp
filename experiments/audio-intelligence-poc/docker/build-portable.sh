#!/usr/bin/env bash
# Build the PoC images with a portable CPython 3.12 staged into the context.
# Usage: docker/build-portable.sh {cpu|gpu}
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VARIANT="${1:-cpu}"
PYTHON_PREFIX="${PYTHON_PREFIX:-$(python3.12 -c 'import sys; print(sys.prefix)')}"
CTX="$(mktemp -d)"
trap 'rm -rf "$CTX"' EXIT

cp -r "$ROOT/scripts" "$CTX/"
cp "$ROOT/requirements-${VARIANT}.txt" "$CTX/"
cp -r "$PYTHON_PREFIX" "$CTX/python"
cp /etc/ssl/certs/ca-certificates.crt "$CTX/"
cp "$ROOT/docker/Dockerfile.${VARIANT}.portable" "$CTX/Dockerfile"

docker build --network=host -t "recording-mcp-audio-poc:${VARIANT}" "$CTX"
