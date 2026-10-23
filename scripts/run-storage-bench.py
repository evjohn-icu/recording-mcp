#!/usr/bin/env python3
"""Run synthetic SQLite benchmark sizes; preserve results when a later size times out."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys


def run_bounded(command, timeout):
    # go run starts a second process. Killing only its parent leaves the actual
    # benchmark running, so supervise an isolated process group.
    child = subprocess.Popen(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    try:
        stdout, stderr = child.communicate(timeout=timeout)
        return subprocess.CompletedProcess(command, child.returncode, stdout, stderr)
    except BaseException:
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        child.communicate()
        raise


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--sizes', default='10000,100000')
    parser.add_argument('--output', required=True)
    parser.add_argument('--queries', type=int, default=20)
    args = parser.parse_args()
    sizes = args.sizes.split(',')
    if any(not value.isdigit() or int(value) < 1 for value in sizes) or args.queries < 1:
        parser.error('sizes and queries must be positive integers')
    report = {'samples': [], 'runs': [], 'synthetic_only': True}
    exit_code = 0
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    for size in sizes:
        command = ['go', 'run', './cmd/context-storage-bench', f'--sizes={size}', '--repeats=3', f'--queries={args.queries}']
        try:
            result = run_bounded(command, 600)
            if result.returncode:
                report['runs'].append({'items': int(size), 'status': 'FAIL', 'exit_code': result.returncode, 'error': result.stderr})
                exit_code = 1
            else:
                part = json.loads(result.stdout)
                report.update({key: value for key, value in part.items() if key not in ('samples', 'runs')})
                report['samples'].extend(part['samples'])
                report['runs'].append({'items': int(size), 'status': 'PASS'})
        except subprocess.TimeoutExpired:
            report['runs'].append({'items': int(size), 'status': 'BLOCKED', 'reason': '600 second execution budget exceeded; process group stopped'})
            exit_code = exit_code or 3
        except (ValueError, KeyError) as error:
            report['runs'].append({'items': int(size), 'status': 'FAIL', 'error': str(error)})
            exit_code = 1
        output.write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(f'wrote {output}; benchmark execution status does not imply optimization acceptance')
    return exit_code


if __name__ == '__main__':
    raise SystemExit(main())
