#!/usr/bin/env python3
"""Tests for weft-timeline.py.

The one property that matters most is which clock places a node: the artifact's
own (`upstream_created`), never the observation's (`observed_at`) and never
weft's when an observed clock exists. A timeline that quietly falls back to the
wrong clock still renders beautifully, which is why it needs a test.

    python3 tools/test_weft_timeline.py
"""

import json
import subprocess
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
TOOL = HERE / "weft-timeline.py"


def render(nodes, args=()):
    out = subprocess.run(
        [sys.executable, str(TOOL), *args],
        input=json.dumps({"nodes": list(nodes), "edges": []}),
        capture_output=True, text=True, check=True,
    )
    return out.stdout


def node(id_char, title, created="2026-08-01T10:00:00Z", refs=()):
    return {"id": id_char * 64, "ns": "acme/api", "kind": "issue",
            "title": title, "state": "open", "created": created,
            "refs": list(refs)}


class TestClockChoice(unittest.TestCase):
    def test_placed_by_the_artifact_clock_not_the_observation(self):
        # Created upstream in 2024; weft imported (created + observed) in 2026.
        n = node("a", "old issue", refs=[{
            "ref": "gh:acme/api#1",
            "observed_at": "2026-08-19T12:00:00Z",
            "upstream_created": "2024-03-05T09:00:00Z",
        }])
        out = render([n])
        self.assertIn("2024-03-05", out)
        self.assertIn("2024-W10", out)
        # Neither the import date nor the observation date places it.
        self.assertNotIn("2026-08", out.split("\n")[0])

    def test_earliest_observed_ref_wins(self):
        n = node("a", "double-reffed", refs=[
            {"ref": "gh:acme/api#1", "upstream_created": "2025-06-01T00:00:00Z"},
            {"ref": "pr:acme/api#2", "upstream_created": "2025-01-01T00:00:00Z"},
        ])
        self.assertIn("2025-01-01", render([n]))

    def test_unobserved_node_falls_back_marked(self):
        out = render([node("a", "hand-made", created="2026-07-01T00:00:00Z")])
        self.assertIn("2026-07-01 ~", out)
        self.assertIn("placed by weft's own created", out)

    def test_bare_string_refs_are_tolerated(self):
        # The default export's ref shape (SPEC.md 6.4): strings, no clock.
        out = render([node("a", "default export", refs=["gh:acme/api#1"])])
        self.assertIn("2026-08-01 ~", out)


class TestCanonicalOutput(unittest.TestCase):
    def test_two_runs_are_byte_identical(self):
        nodes = [
            node("b", "second", refs=[{"ref": "gh:x/y#2", "upstream_created": "2025-02-01T00:00:00Z"}]),
            node("a", "first", refs=[{"ref": "gh:x/y#1", "upstream_created": "2025-01-01T00:00:00Z"}]),
        ]
        self.assertEqual(render(nodes), render(nodes))

    def test_same_stamp_breaks_ties_by_id(self):
        stamp = "2025-01-01T00:00:00Z"
        nodes = [
            node("b", "bee", refs=[{"ref": "gh:x/y#2", "upstream_created": stamp}]),
            node("a", "ay", refs=[{"ref": "gh:x/y#1", "upstream_created": stamp}]),
        ]
        out = render(nodes)
        self.assertLess(out.index("ay"), out.index("bee"))


class TestCohorts(unittest.TestCase):
    def test_month_granularity(self):
        n = node("a", "x", refs=[{"ref": "gh:x/y#1", "upstream_created": "2025-03-15T00:00:00Z"}])
        out = render([n], args=["--by", "month"])
        self.assertIn("2025-03  (1 node)", out)


class TestMermaid(unittest.TestCase):
    def test_emits_a_fenced_timeline(self):
        n = node("a", "plain", refs=[{"ref": "gh:x/y#1", "upstream_created": "2025-03-15T00:00:00Z"}])
        out = render([n], args=["--mermaid"])
        self.assertIn("```mermaid", out)
        self.assertIn("timeline", out)
        self.assertIn(" : plain", out)

    def test_titles_cannot_carry_mermaid_syntax(self):
        n = node("a", "fix: the #1 bug; really", refs=[{
            "ref": "gh:x/y#1", "upstream_created": "2025-03-15T00:00:00Z"}])
        body = render([n], args=["--mermaid"]).split("timeline")[1]
        for c in [";", "#"]:
            self.assertNotIn(c, body.split("```")[0])


class TestDegenerate(unittest.TestCase):
    def test_no_datable_nodes_is_an_answer(self):
        out = render([{"id": "c" * 64, "title": "no stamps", "refs": []}])
        self.assertIn("no datable nodes", out)

    def test_non_export_input_fails_loudly(self):
        r = subprocess.run([sys.executable, str(TOOL)], input="{}",
                           capture_output=True, text=True)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("nodes", r.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
