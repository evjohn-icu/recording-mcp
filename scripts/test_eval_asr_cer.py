#!/usr/bin/env python3
"""Unit tests for scripts/eval-asr-cer.py normalization and distance."""
from __future__ import annotations

import importlib.util
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "eval-asr-cer.py"


def load_module():
    spec = importlib.util.spec_from_file_location("eval_asr_cer", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class NormalizeTests(unittest.TestCase):
    def test_digits_and_punctuation_are_normalized(self):
        mod = load_module()
        self.assertEqual(mod.normalize("幺幺五八，八八九五。"), "11588895")
        self.assertEqual(mod.normalize("你好 世界！"), "你好世界")

    def test_compound_and_full_width_digits(self):
        mod = load_module()
        self.assertEqual(mod.normalize("十五分钟"), "15分钟")
        self.assertEqual(mod.normalize("十二月三日"), "12月3日")
        self.assertEqual(mod.normalize("１２３"), "123")

    def test_levenshtein(self):
        mod = load_module()
        self.assertEqual(mod.levenshtein("abc", "abc"), 0)
        self.assertEqual(mod.levenshtein("abc", "abd"), 1)
        self.assertEqual(mod.levenshtein("", "abc"), 3)


if __name__ == "__main__":
    unittest.main()
