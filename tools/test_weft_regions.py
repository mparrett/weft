#!/usr/bin/env python3
"""Tests for weft-regions.py.

A detector that finds almost nothing is indistinguishable from one that is
broken, and this one found three regions in a 267-node graph. These cases are
what made that number believable: the shapes it must find, and the shape it must
refuse.

    python3 tools/test_weft_regions.py
"""

import importlib.util
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("weft_regions", HERE / "weft-regions.py")
r = importlib.util.module_from_spec(spec)
spec.loader.exec_module(r)


def graph(ids, pairs, kind="depends-on"):
    return ([{"id": i, "title": i, "ns": "t"} for i in ids],
            [{"kind": kind, "from": a, "to": b} for a, b in pairs])


def interiors(nodes, edges, kinds=("depends-on",)):
    found, *_ = r.regions(nodes, edges, list(kinds))
    return [set(i) for _, _, i in found]


class TestSESE(unittest.TestCase):
    def test_finds_a_diamond(self):
        nodes, edges = graph("sabt", [("s", "a"), ("s", "b"), ("a", "t"), ("b", "t")])
        self.assertIn({"a", "b"}, interiors(nodes, edges))

    def test_finds_a_chain(self):
        nodes, edges = graph("sabt", [("s", "a"), ("a", "b"), ("b", "t")])
        self.assertIn({"a", "b"}, interiors(nodes, edges))

    def test_finds_a_wide_fan(self):
        members = [f"x{i}" for i in range(5)]
        nodes, edges = graph(["s", "t"] + members,
                             [("s", m) for m in members] + [(m, "t") for m in members])
        self.assertIn(set(members), interiors(nodes, edges))

    def test_an_escape_edge_breaks_the_region(self):
        """The reason real graphs have so few. One interior node reaching
        outside means the exit no longer post-dominates the entry, and in a work
        graph almost everything reaches outside."""
        nodes, edges = graph("sabte", [("s", "a"), ("s", "b"), ("a", "t"), ("b", "t"),
                                       ("a", "e")])
        self.assertEqual(interiors(nodes, edges), [])

    def test_unconnected_nodes_are_not_a_region(self):
        nodes, edges = graph("ab", [])
        self.assertEqual(interiors(nodes, edges), [])

    def test_the_virtual_endpoints_are_never_a_boundary(self):
        """A fan of roots under one parent is single-exit and zero-entry. It is
        exactly the shape this store is full of, and it is not a region — a
        boundary the reader cannot name is not one they can act on."""
        members = [f"x{i}" for i in range(4)]
        nodes, edges = graph(members + ["p"], [(m, "p") for m in members])
        self.assertEqual(interiors(nodes, edges), [])

    def test_only_the_named_kinds_are_walked(self):
        nodes, edges = graph("sabt", [("s", "a"), ("s", "b"), ("a", "t"), ("b", "t")],
                             kind="part-of")
        self.assertEqual(interiors(nodes, edges, kinds=("depends-on",)), [])
        self.assertIn({"a", "b"}, interiors(nodes, edges, kinds=("part-of",)))


class TestSiblings(unittest.TestCase):
    def test_groups_nodes_sharing_a_neighbourhood(self):
        members = [f"x{i}" for i in range(3)]
        nodes, edges = graph(["s", "t"] + members,
                             [("s", m) for m in members] + [(m, "t") for m in members])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        self.assertIn(sorted(members), [sorted(g) for g in r.siblings(succ, pred, ids)])

    def test_unconnected_nodes_do_not_form_a_class(self):
        """Left in, every node with no edge of the kind under test shares
        pred={source} succ={sink} with every other. On the real store that was
        one 107-node class reported as an 84% simplification, which was really
        "hide the unconnected nodes" — a checkbox the viewer already has."""
        nodes, edges = graph(["a", "b", "c"], [])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        self.assertEqual(r.siblings(succ, pred, ids), [])

    def test_roots_under_one_parent_are_a_class(self):
        """The shape SESE cannot reach and the one that carries the value."""
        members = [f"x{i}" for i in range(4)]
        nodes, edges = graph(members + ["p"], [(m, "p") for m in members])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        groups = r.siblings(succ, pred, ids)
        self.assertEqual([sorted(g) for g in groups], [sorted(members)])

    def test_a_differing_endpoint_splits_the_class(self):
        nodes, edges = graph(["a", "b", "p", "q"], [("a", "p"), ("b", "p"), ("b", "q")])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        self.assertEqual(r.siblings(succ, pred, ids), [])


class TestChains(unittest.TestCase):
    def test_finds_a_run(self):
        nodes, edges = graph("sabct", [("s", "a"), ("a", "b"), ("b", "c"), ("c", "t")])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        runs = [sorted(c) for c in r.chains(succ, pred, ids)]
        self.assertIn(["a", "b", "c"], runs)

    def test_a_branch_ends_the_run(self):
        nodes, edges = graph("sabct", [("s", "a"), ("a", "b"), ("a", "c"),
                                       ("b", "t"), ("c", "t")])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        for run in r.chains(succ, pred, ids):
            self.assertNotIn("a", run, "a node with two out-edges is not chain interior")

    def test_unconnected_nodes_are_not_a_chain(self):
        nodes, edges = graph(["a", "b"], [])
        succ, pred, ids = r.build(nodes, edges, ["depends-on"])
        self.assertEqual(r.chains(succ, pred, ids), [])


class TestDisjoint(unittest.TestCase):
    def test_overlapping_candidates_are_counted_once(self):
        """Regions nest, so a naive sum double-counts and overstates what a
        single collapse pass could take."""
        picked, covered = r.disjoint([["a", "b", "c"], ["b", "c"], ["d", "e"]])
        self.assertEqual(sorted(len(g) for g in picked), [2, 3])
        self.assertEqual(covered, {"a", "b", "c", "d", "e"})


if __name__ == "__main__":
    unittest.main()
