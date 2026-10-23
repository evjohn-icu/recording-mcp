import unittest
import json
import importlib.util
from pathlib import Path

SPEC = importlib.util.spec_from_file_location('quality', Path(__file__).with_name('worldline-relation-quality.py'))
quality = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(quality)

class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.fixture = quality.load_fixture(quality.DEFAULT_FIXTURE)

    def test_fixture_is_120_balanced_and_group_isolated(self):
        cases = self.fixture['cases']
        self.assertEqual(len(cases), 120)
        self.assertEqual({s: sum(c['split'] == s for c in cases) for s in ('calibration','test')}, {'calibration':60,'test':60})
        self.assertEqual(len({c['group'] for c in cases}), 120)

    def test_malformed_fixture_rejected(self):
        broken = dict(self.fixture)
        broken['cases'] = self.fixture['cases'][:-1]
        with self.assertRaises(ValueError): quality.validate_fixture(broken)

    def test_group_leakage_rejected(self):
        broken = json.loads(json.dumps(self.fixture))
        broken['cases'][60]['group'] = broken['cases'][0]['group']
        with self.assertRaises(ValueError): quality.validate_fixture(broken)

    def test_distribution_strictly_validated(self):
        labels = set(quality.LABELS)
        valid = {'type':'choice','choice':'text_similarity','confidence':.8,'probabilities':{x:.2 for x in labels}}
        self.assertIsNone(quality.validate_answer(valid, labels)[2])
        valid['probabilities']['unrelated'] = float('nan')
        self.assertIsNotNone(quality.validate_answer(valid, labels)[2])
        self.assertIsNotNone(quality.validate_answer({'type':'choice','choice':{},'probabilities':{}}, labels)[2])

    def test_attempted_inference_failure_writes_valid_json_and_exits_nonzero(self):
        import subprocess
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / 'result.json'
            proc = subprocess.run([
                'python3', str(Path(__file__).with_name('worldline-relation-quality.py')),
                '--endpoint', 'http://127.0.0.1:1/v1/systemone', '--split', 'test',
                '--output', str(output), '--timeout', '0.1',
            ], capture_output=True, text=True)
            self.assertNotEqual(proc.returncode, 0)
            raw = output.read_text(encoding='utf8')
            self.assertTrue(raw.endswith('\n'))
            result = json.loads(raw)
            self.assertEqual(result['status'], 'FAIL')
            self.assertEqual(result['errors'], 60)
            self.assertNotEqual(result['status'], 'NOT_RUN')

    def test_fixture_sources_are_synthetic_and_label_free(self):
        for case in self.fixture['cases']:
            self.assertNotIn('expected', case['state'])
            for side in ('left', 'right'):
                self.assertIn('合成', case['state'][side]['source'])

    def test_failure_is_not_counted_as_no_judgment_label(self):
        result = quality.score([{'expected':'unrelated','prediction':None,'error':'network'}])
        self.assertEqual(result['errors'], 1)
        self.assertEqual(result['abstain'], 0)
        self.assertEqual(result['denominator'], 0)

if __name__ == '__main__': unittest.main()
