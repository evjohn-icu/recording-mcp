import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('storage_runner', Path(__file__).with_name('run-storage-bench.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)

class StorageRunnerTests(unittest.TestCase):
    def test_timeout_preserves_completed_size_report(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'result.json'
            completed = subprocess.CompletedProcess([], 0, json.dumps({'samples': [{'items': 10000}]}), '')
            with patch.object(runner.subprocess, 'run', side_effect=AssertionError('unbounded subprocess call')), patch.object(runner, 'run_bounded', side_effect=[completed, subprocess.TimeoutExpired('bench', 600)], create=True), patch('sys.argv', ['runner', '--output', str(output)]):
                self.assertEqual(runner.main(), 3)
            report = json.loads(output.read_text())
            self.assertEqual(report['samples'], [{'items': 10000}])
            self.assertEqual([run['status'] for run in report['runs']], ['PASS', 'BLOCKED'])

    def test_success_does_not_duplicate_first_size_samples(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'result.json'
            parts = [subprocess.CompletedProcess([], 0, json.dumps({'samples': [{'items': n}]}), '') for n in [10000, 100000]]
            with patch.object(runner.subprocess, 'run', side_effect=AssertionError('unbounded subprocess call')), patch.object(runner, 'run_bounded', side_effect=parts, create=True), patch('sys.argv', ['runner', '--output', str(output)]):
                self.assertEqual(runner.main(), 0)
            self.assertEqual(len(json.loads(output.read_text())['samples']), 2)

if __name__ == '__main__':
    unittest.main()
