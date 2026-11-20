#!/usr/bin/env python3
"""Stop-write backup and restore of the recording-mcp Docker data volume.

The script never copies a live volume itself: it stops the compose project that
owns the volume, refuses to continue when any other running container still
mounts it, and then runs `context-backup` inside the image so the archive is
built by the same code path that verifies hashes and forget barriers.

Usage:
  scripts/backup-profile-volume.py backup \
      --data-volume recording-mcp-data --out ./backups \
      [--compose-project recording-mcp] [--image recording-mcp:beta] \
      [--profiles-subdir profiles] [--name context-engine.tgz]

  scripts/backup-profile-volume.py restore \
      --archive ./backups/context-engine.tgz \
      --data-volume recording-mcp-restore \
      --barrier-source-volume recording-mcp-data \
      [--compose-project recording-mcp] [--image recording-mcp:beta]

  scripts/backup-profile-volume.py verify --data-volume recording-mcp-restore
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_IMAGE = "recording-mcp:beta"
DEFAULT_PROFILES_SUBDIR = "profiles"


def run(cmd: list[str], *, timeout: int = 600) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, text=True, capture_output=True, timeout=timeout)


def fail(code: str, detail: str) -> int:
    print(json.dumps({"error": {"code": code, "detail": detail}}, ensure_ascii=False), file=sys.stderr)
    return 1


def compose_stop(project: str | None) -> None:
    """Stop the compose project that writes the volume. Must succeed."""
    if not project:
        return
    result = run(["docker", "compose", "-p", project, "stop"], timeout=300)
    if result.returncode != 0:
        raise RuntimeError(f"docker compose -p {project} stop failed: {result.stderr.strip()}")


def running_containers(volume: str) -> list[str]:
    """Names of running containers that mount the volume."""
    result = run(["docker", "ps", "--filter", f"volume={volume}", "--format", "{{.Names}}"])
    if result.returncode != 0:
        raise RuntimeError(f"docker ps failed: {result.stderr.strip()}")
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def ensure_no_writers(volume: str) -> None:
    writers = running_containers(volume)
    if writers:
        raise RuntimeError(
            "volume %s is still mounted by running container(s): %s; stop them explicitly (this script never stops containers it did not start)"
            % (volume, ", ".join(writers))
        )


def volume_exists(volume: str) -> bool:
    return run(["docker", "volume", "inspect", volume]).returncode == 0


def create_volume(volume: str) -> None:
    result = run(["docker", "volume", "create", volume])
    if result.returncode != 0:
        raise RuntimeError(f"docker volume create {volume} failed: {result.stderr.strip()}")


def absolute(path: str) -> str:
    return os.path.abspath(os.path.expanduser(path))


def cmd_backup(args: argparse.Namespace) -> int:
    out_dir = absolute(args.out)
    if not os.path.isdir(out_dir):
        # The backup container runs as the image's non-root user (UID 10001),
        # so a private 0700 host directory cannot be bind-mounted writable.
        # The sticky world-writable directory lets that user create the
        # archive; the archive file itself keeps its restrictive mode.
        os.makedirs(out_dir, mode=0o1777)
        os.chmod(out_dir, 0o1777)
    if not volume_exists(args.data_volume):
        return fail("volume_missing", f"volume {args.data_volume} does not exist")
    try:
        compose_stop(args.compose_project)
        ensure_no_writers(args.data_volume)
    except RuntimeError as exc:
        return fail("writers_active", str(exc))
    archive_name = args.name or "context-engine.tgz"
    if os.path.exists(os.path.join(out_dir, archive_name)):
        return fail("archive_exists", f"{archive_name} already exists; refusing to overwrite")
    cmd = [
        "docker", "run", "--rm",
        "-v", f"{args.data_volume}:/data:ro",
        "-v", f"{out_dir}:/backup:rw",
        args.image,
        "/app/context-backup", "backup",
        "--profiles-root", f"/data/{args.profiles_subdir}",
        "--out", f"/backup/{archive_name}",
        "--confirm-writers-stopped",
    ]
    result = run(cmd, timeout=3600)
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    if result.returncode != 0:
        return result.returncode
    return 0


def cmd_restore(args: argparse.Namespace) -> int:
    archive = absolute(args.archive)
    if not os.path.isfile(archive):
        return fail("invalid_archive", f"archive not found: {archive}")
    if not volume_exists(args.barrier_source_volume):
        return fail("volume_missing", f"barrier source volume {args.barrier_source_volume} does not exist")
    try:
        compose_stop(args.compose_project)
        ensure_no_writers(args.barrier_source_volume)
    except RuntimeError as exc:
        return fail("forget_barrier_unverifiable", str(exc))
    if not volume_exists(args.data_volume):
        create_volume(args.data_volume)
    archive_dir = os.path.dirname(archive)
    cmd = [
        "docker", "run", "--rm",
        "-v", f"{args.barrier_source_volume}:/live:ro",
        "-v", f"{args.data_volume}:/data:rw",
        "-v", f"{archive_dir}:/archive:ro",
        args.image,
        "/app/context-backup", "restore",
        "--archive", f"/archive/{os.path.basename(archive)}",
        "--dest", f"/data/{args.profiles_subdir}",
        "--barrier-source", f"/live/{args.profiles_subdir}",
        "--confirm-writers-stopped",
    ]
    result = run(cmd, timeout=3600)
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    if result.returncode != 0:
        return result.returncode
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    if not volume_exists(args.data_volume):
        return fail("volume_missing", f"volume {args.data_volume} does not exist")
    result = run([
        "docker", "run", "--rm",
        "-v", f"{args.data_volume}:/data:ro",
        args.image,
        "/app/context-backup", "verify",
        "--profiles-root", f"/data/{args.profiles_subdir}",
    ])
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    return result.returncode


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default=DEFAULT_IMAGE)
    parser.add_argument("--profiles-subdir", default=DEFAULT_PROFILES_SUBDIR)
    sub = parser.add_subparsers(dest="command", required=True)

    backup = sub.add_parser("backup", help="archive a stopped data volume")
    backup.add_argument("--data-volume", required=True)
    backup.add_argument("--out", required=True)
    backup.add_argument("--compose-project")
    backup.add_argument("--name")
    backup.set_defaults(func=cmd_backup)

    restore = sub.add_parser("restore", help="restore an archive into a new volume")
    restore.add_argument("--archive", required=True)
    restore.add_argument("--data-volume", required=True)
    restore.add_argument("--barrier-source-volume", required=True)
    restore.add_argument("--compose-project")
    restore.set_defaults(func=cmd_restore)

    verify = sub.add_parser("verify", help="verify a restored volume")
    verify.add_argument("--data-volume", required=True)
    verify.set_defaults(func=cmd_verify)

    args = parser.parse_args(argv)
    if shutil.which("docker") is None:
        return fail("docker_unavailable", "docker binary not found")
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
