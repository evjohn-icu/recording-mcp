import tempfile
import unittest
from pathlib import Path

from check_spec_test_map import check

class ScenarioMapTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.spec = self.root / 'openspec/changes/demo/specs/demo/spec.md'
        self.spec.parent.mkdir(parents=True)
        self.spec.write_text('#### Scenario: demo.required\n', encoding='utf-8')
        self.test = self.root / 'demo_test.go'
        self.test.write_text('func TestDemo(t *testing.T) {}\n', encoding='utf-8')

    def tearDown(self):
        self.tmp.cleanup()

    def mapping(self, **overrides):
        item = {'id': 'demo.required', 'status': 'covered', 'spec': str(self.spec.relative_to(self.root)),
                'test_file': str(self.test.relative_to(self.root)), 'test_anchor': 'TestDemo'}
        item.update(overrides)
        return {'scenarios': [item]}

    def test_exact_test_anchor_passes(self):
        errors, pending = check(self.mapping(), self.root)
        self.assertEqual((errors, pending), ([], []))

    def test_missing_required_spec_scenario_mapping_fails(self):
        errors, _ = check({'scenarios': []}, self.root)
        self.assertIn('spec scenario has no mapping: demo.required', errors)

    def test_synced_main_spec_scenario_without_mapping_fails(self):
        main = self.root / 'openspec/specs/baseline/spec.md'
        main.parent.mkdir(parents=True)
        main.write_text('#### Scenario: baseline.required\n', encoding='utf-8')
        errors, _ = check(self.mapping(), self.root)
        self.assertIn('spec scenario has no mapping: baseline.required', errors)

    def test_dangling_mapping_fails(self):
        errors, _ = check(self.mapping(id='demo.unknown'), self.root)
        self.assertTrue(any('mapping has no spec scenario' in error for error in errors))

    def test_missing_test_file_and_anchor_fail(self):
        errors, _ = check(self.mapping(test_file='missing_test.go', test_anchor='TestMissing'), self.root)
        self.assertTrue(any('test file not found' in error for error in errors))

    def test_pending_is_explicit_and_does_not_claim_test(self):
        errors, pending = check(self.mapping(status='pending', test_file=None, test_anchor=None), self.root)
        self.assertEqual(errors, [])
        self.assertEqual(pending, ['demo.required'])

    def test_pending_mapping_cannot_claim_an_anchor(self):
        errors, _ = check(self.mapping(status='pending'), self.root)
        self.assertTrue(any('pending mapping must not claim' in error for error in errors))

if __name__ == '__main__':
    unittest.main()
