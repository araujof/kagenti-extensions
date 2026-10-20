"""Checks that hr-mcp reads a request the way the sidecar's policy did.

Run: python3 -m unittest -q test_server  (needs fastapi, as server.py does)
"""

import unittest

import server


def _adjust(amount):
    server.EMPLOYEES["EMP-001234"]["salary"] = 125000
    return server.tool_adjust_compensation({"employee_id": "EMP-001234", "amount": amount})


class ParseBody(unittest.TestCase):
    def test_refuses_what_the_sidecar_cannot_parse(self):
        for literal in ("NaN", "Infinity", "-Infinity", "1e400", "-1e400"):
            body = b'{"method":"tools/call","params":{"arguments":{"amount":%s}}}' % literal.encode()
            with self.assertRaises(ValueError, msg=literal):
                server.parse_body(body)

    def test_reads_numbers_as_json_does(self):
        rpc = server.parse_body(b'{"a":15000,"b":15000.0,"c":1.5e4,"d":-0,"e":9007199254740993}')
        self.assertEqual(rpc, {"a": 15000, "b": 15000.0, "c": 15000.0, "d": 0, "e": 9007199254740993})


class AdjustAmount(unittest.TestCase):
    def test_applies_whole_dollar_numbers(self):
        for amount, want in ((5000, 5000), (5000.0, 5000), (1.5e4, 15000), (-0.0, 0), (-25000, -25000)):
            out = _adjust(amount)
            self.assertEqual(out.get("adjustment"), want, amount)
            self.assertIsInstance(out["adjustment"], int)

    def test_refuses_what_the_threshold_did_not_see(self):
        for amount in ("15000", True, False, None, [15000], {"v": 1}, 10000.5, float("nan"), float("inf")):
            self.assertIn("error", _adjust(amount), repr(amount))


if __name__ == "__main__":
    unittest.main()
