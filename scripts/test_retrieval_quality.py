import importlib.util
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('retrieval_quality_runner', ROOT / 'scripts/run-retrieval-quality.py')
runner = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(runner)

class AuthoredQualityCorpusTests(unittest.TestCase):
    def test_authored_corpus_has_isolated_50_25_25_concept_groups(self):
        path = ROOT / 'fixtures/eval/quality/retrieval.json'
        summary = runner.validate_quality_questions(path)
        self.assertEqual(summary['split_group_counts'], {'train': 50, 'calibration': 25, 'test': 25})
        self.assertEqual(summary['split_question_counts'], {'train': 80, 'calibration': 40, 'test': 40})
        self.assertGreaterEqual(len(summary['buckets']), 4)

    def test_fixture_is_explicitly_authored_and_all_gold_refs_resolve_shape(self):
        import json
        data = json.loads((ROOT / 'fixtures/eval/quality/retrieval.json').read_text(encoding='utf-8'))
        positives = [q for q in data['questions'] if not q.get('unanswerable')]
        negatives = [q for q in data['questions'] if q.get('unanswerable')]
        self.assertEqual((len(positives), len(negatives)), (120, 40))
        self.assertTrue(all(q.get('bucket') and q.get('gold_turn_refs') for q in positives))
        self.assertTrue(all(not q.get('gold_turn_refs') for q in negatives))
        self.assertTrue(all(q.get('notes') for q in data['questions']))

    def test_split_group_cannot_leak_across_train_and_test(self):
        import json
        import tempfile
        data = json.loads((ROOT / 'fixtures/eval/quality/retrieval.json').read_text(encoding='utf-8'))
        data['questions'][0]['split'] = 'test'
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'retrieval.json'
            corpus = Path(temp) / 'corpus.json'
            path.write_text(json.dumps(data, ensure_ascii=False), encoding='utf-8')
            corpus.write_text((ROOT / 'fixtures/eval/quality/corpus.json').read_text(encoding='utf-8'), encoding='utf-8')
            with self.assertRaisesRegex(ValueError, 'crosses'):
                runner.validate_quality_questions(path, corpus)

    def test_calibration_gate_requires_recall_fp_and_zero_leakage(self):
        baseline = {'hybrid_recall_at_5': .8}
        candidate = {'hybrid_recall_at_5': .8, 'unanswerable_hybrid_false_positive_rate': 0,
                     'stale_leakage': 0, 'ambient_leakage': 0, 'scope_escape_count': 0,
                     'retrieval_invalid_evidence_refs': 0}
        self.assertTrue(runner._gate(candidate, baseline)['passed'])
        candidate['hybrid_recall_at_5'] = .79
        self.assertFalse(runner._gate(candidate, baseline)['passed'])
        candidate['hybrid_recall_at_5'] = .8
        candidate['unanswerable_hybrid_false_positive_rate'] = .1
        self.assertFalse(runner._gate(candidate, baseline)['passed'])

if __name__ == '__main__':
    unittest.main()
