#!/usr/bin/env python3
"""Create disposable browser-test profiles from a synthetic web-demo database."""
from __future__ import annotations
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profiles-root', required=True, type=Path)
    parser.add_argument('--db', required=True, type=Path)
    parser.add_argument('--audio-dir', required=True, type=Path)
    parser.add_argument('--context-profile', default='bin/context-profile')
    args = parser.parse_args()
    if not args.db.is_file() or not args.audio_dir.is_dir():
        parser.error('expected synthetic database and audio directory')
    alpha, beta = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    env = os.environ.copy()
    env['MEMORY_PROFILE_PASSWORD'] = alpha
    common = [args.context_profile]
    subprocess.run([*common, 'adopt', '--profiles-root', str(args.profiles_root), '--name', 'alpha', '--db', str(args.db), '--audio-dir', str(args.audio_dir)], env=env, check=True)
    env['MEMORY_PROFILE_PASSWORD'] = beta
    subprocess.run([*common, 'create', '--profiles-root', str(args.profiles_root), '--name', 'beta'], env=env, check=True)
    print(json.dumps({'alpha_password': alpha, 'beta_password': beta}))
    return 0

if __name__ == '__main__':
    raise SystemExit(main())
