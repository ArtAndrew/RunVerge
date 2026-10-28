# SPDX-License-Identifier: Apache-2.0
"""Consumer tests for exact public money helpers, independent of server code."""
import sys
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "packages/python-sdk/src"))
from agent_platform import Money, parse_micro_usd


class MoneyTests(unittest.TestCase):
    def test_exact_values_above_float_precision(self):
        for source, expected in [("0", 0), ("1", 1), ("9007199254740993", 9007199254740993),
                                 ("9223372036854775807", (1 << 63) - 1)]:
            self.assertEqual(parse_micro_usd(source), expected)
        self.assertIs(Money, str)
        self.assertEqual(parse_micro_usd("-9223372036854775808", signed=True), -(1 << 63))
        self.assertEqual(parse_micro_usd("-1", signed=True), -1)

    def test_ambiguous_and_out_of_range_values_fail(self):
        for source in ["", "-0", "+1", "01", "1.0", "1e6", " 1", "1 ", "1\n", "١", "１",
                       "9223372036854775808", "-9223372036854775809", "9" * 10000]:
            for signed in (False, True):
                with self.subTest(source=source[:25], signed=signed), self.assertRaises(ValueError):
                    parse_micro_usd(source, signed=signed)
        for source in [0, 9007199254740993, 1.0, True, None, b"1"]:
            with self.subTest(source=source), self.assertRaises(TypeError):
                parse_micro_usd(source)
        with self.assertRaises(ValueError):
            parse_micro_usd("-1")


if __name__ == "__main__":
    unittest.main()
