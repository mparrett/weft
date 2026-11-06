#!/usr/bin/env python3
"""Spike: what could a container/absorption feature actually collapse?

Reads `weft export --json` and reports the groupings a graph-simplification
feature would have to work with. It exists to answer a design question with a
number rather than an intuition, and it did: the answer was no to the shape that
was proposed and yes to a different one.

    weft export --json | tools/weft-regions.py
    tools/weft-regions.py graph.json

Three passes, weakest to strongest on this data:

*SESE regions.* A pair (s, t) where s dominates t and t post-dominates s, with
the interior everything strictly between. This is the textbook single-entry
single-exit region, and the hypothesis was that chains and sibling fans are the
same object under it. Measured on a 267-node store: 3 regions covering 7 nodes.
The hypothesis was wrong, and the reason is structural rather than a tuning
problem — the dominant shape here is a fan whose members are *roots*, pointing
at one parent with nothing pointing at them. That is single-exit and zero-entry,
a star rather than a region, and SESE needs a real entry node.

*Chains.* Runs where every interior node has one in-edge and one out-edge.
Real but small: 7 runs, mostly of length two.

*Sibling classes.* Nodes with identical in and out neighbourhoods. This is where
the value is — over `part-of` alone, 21 classes covering 117 of 267 nodes, and
17 of those 21 are roots sharing exactly one `part-of` target. Which means the
container they would collapse into already exists: it is the node they are
already `part-of`. No insertion, no new edge kind, no mutation.

The graph has many roots and many sinks, so dominance needs a single entry: a
virtual source feeds every in-degree-0 node, and every out-degree-0 node feeds a
virtual sink. Those two are stripped before any grouping is reported — left in,
every node with no edge of the kind under test shares a neighbourhood with every
other, which produced one 107-node class and an "84% simplification" that was
really "hide the unconnected nodes", something the viewer already has a checkbox
for. A metric that counts absences looks spectacular.
"""

import argparse
import json
import sys
from collections import defaultdict

SRC, SNK = "\x00source", "\x00sink"

# The directed family. conflicts-with and same-as are symmetric (SPEC §4.2), so
# they are free bidirectional shortcuts in both directions and would make a
# region boundary meaningless.
DIRECTED = ["depends-on", "part-of", "supersedes", "derived-from"]


def build(nodes, edges, kinds):
    """Adjacency over one edge-kind projection, with virtual source and sink."""
    succ, pred = defaultdict(set), defaultdict(set)
    ids = {n["id"] for n in nodes}
    for e in edges:
        if e["kind"] not in kinds:
            continue
        a, b = e["from"], e["to"]
        # A self-edge carries no structure and would dominate itself.
        if a in ids and b in ids and a != b:
            succ[a].add(b)
            pred[b].add(a)
    for n in ids:
        if not pred[n]:
            succ[SRC].add(n)
            pred[n].add(SRC)
        if not succ[n]:
            succ[n].add(SNK)
            pred[SNK].add(n)
    return succ, pred, ids


def rpo(succ, entry):
    """Reverse postorder. Iterative, so graph depth cannot exhaust the stack."""
    seen, order, stack = {entry}, [], [(entry, iter(sorted(succ[entry])))]
    while stack:
        node, it = stack[-1]
        for nxt in it:
            if nxt not in seen:
                seen.add(nxt)
                stack.append((nxt, iter(sorted(succ[nxt]))))
                break
        else:
            order.append(stack.pop()[0])
    order.reverse()
    return order


def dominators(succ, pred, entry):
    """Cooper-Harvey-Kennedy. Returns idom; walk the chain to test dominance."""
    order = rpo(succ, entry)
    rank = {n: i for i, n in enumerate(order)}
    idom = {entry: entry}

    def intersect(a, b):
        while a != b:
            while rank[a] > rank[b]:
                a = idom[a]
            while rank[b] > rank[a]:
                b = idom[b]
        return a

    changed = True
    while changed:
        changed = False
        for n in order:
            if n == entry:
                continue
            known = [p for p in pred[n] if p in idom and p in rank]
            if not known:
                continue
            new = known[0]
            for p in known[1:]:
                new = intersect(p, new)
            if idom.get(n) != new:
                idom[n] = new
                changed = True
    return idom


def dominates(idom, a, b):
    cur = b
    while True:
        if cur == a:
            return True
        nxt = idom.get(cur)
        if nxt is None or nxt == cur:
            return False
        cur = nxt


def regions(nodes, edges, kinds, min_interior=2):
    """SESE regions with at least min_interior nodes strictly inside.

    The virtual source and sink are never region endpoints. A region bounded by
    something that is not in the graph is not one a reader could name.
    """
    succ, pred, ids = build(nodes, edges, kinds)
    idom = dominators(succ, pred, SRC)
    pidom = dominators(pred, succ, SNK)  # post-dominance is dominance reversed

    found = []
    for t in ids:
        if t not in idom:
            continue
        s = idom.get(t)
        while s is not None and s != SRC:
            if s != t and dominates(pidom, t, s):
                interior = {
                    v for v in ids
                    if v not in (s, t)
                    and dominates(idom, s, v) and dominates(pidom, t, v)
                }
                if len(interior) >= min_interior:
                    found.append((s, t, interior))
            nxt = idom.get(s)
            if nxt is None or nxt == s:
                break
            s = nxt
    return found, succ, pred, ids


def chains(succ, pred, ids, minimum=2):
    """Maximal runs whose every member has exactly one real in and out edge."""
    inset = {n for n in ids
             if len(pred[n] - {SRC}) == 1 and len(succ[n] - {SNK}) == 1}
    seen, out = set(), []
    for n in sorted(inset):
        if n in seen:
            continue
        run, seen = [n], seen | {n}
        cur = next(iter(pred[n] - {SRC}))
        while cur in inset and cur not in seen:
            run.insert(0, cur)
            seen.add(cur)
            cur = next(iter(pred[cur] - {SRC}), None)
        cur = next(iter(succ[n] - {SNK}))
        while cur in inset and cur not in seen:
            run.append(cur)
            seen.add(cur)
            cur = next(iter(succ[cur] - {SNK}), None)
        if len(run) >= minimum:
            out.append(run)
    return out


def siblings(succ, pred, ids, minimum=2):
    """Nodes sharing an in and out neighbourhood, virtual endpoints stripped."""
    groups = defaultdict(list)
    for n in ids:
        p = frozenset(pred[n]) - {SRC}
        s = frozenset(succ[n]) - {SNK}
        if p or s:
            groups[(p, s)].append(n)
    return [g for g in groups.values() if len(g) >= minimum]


def disjoint(groups):
    """Greedy largest-first pick, so overlapping candidates are not double-counted."""
    taken, out = set(), []
    for g in sorted(groups, key=len, reverse=True):
        members = set(g)
        if members & taken:
            continue
        out.append(g)
        taken |= members
    return out, taken


def report(data, kinds, label, out=sys.stdout):
    nodes, edges = data["nodes"], data.get("edges", [])
    title = {n["id"]: n.get("title", "") for n in nodes}
    found, succ, pred, ids = regions(nodes, edges, kinds)
    ch, sb = chains(succ, pred, ids), siblings(succ, pred, ids)

    _, region_nodes = disjoint([sorted(i) for _, _, i in found])
    _, chain_nodes = disjoint(ch)
    sib_pick, sib_nodes = disjoint(sb)
    groups, covered = disjoint([sorted(i) for _, _, i in found] + ch + sb)
    after = len(ids) - len(covered) + len(groups)
    pct = (1 - after / len(ids)) * 100 if ids else 0

    print(f"\n=== {label}: {', '.join(kinds)} ===", file=out)
    print(f"  {len(ids)} nodes", file=out)
    print(f"  SESE regions    {len(found):3}  covering {len(region_nodes):3} nodes", file=out)
    print(f"  chains          {len(ch):3}  covering {len(chain_nodes):3} nodes", file=out)
    print(f"  sibling classes {len(sb):3}  covering {len(sib_nodes):3} nodes", file=out)
    print(f"  collapsing all three: {after} boxes instead of {len(ids)} "
          f"({pct:.0f}% fewer)", file=out)

    # The headline: how many sibling classes are roots under one parent, i.e.
    # already have the container they would collapse into.
    rooted = [g for g in sb
              if not (pred[g[0]] - {SRC}) and len(succ[g[0]] - {SNK}) == 1]
    if rooted:
        print(f"  of the {len(sb)} sibling classes, {len(rooted)} are roots sharing one "
              f"target — the container already exists:", file=out)
        for g in sorted(rooted, key=len, reverse=True)[:4]:
            parent = next(iter(succ[g[0]] - {SNK}))
            print(f"    {len(g):3} children  ->  {title[parent][:56]!r}", file=out)
    return {"nodes": len(ids), "sese": len(found), "chains": len(ch),
            "siblings": len(sb), "after": after}


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("input", nargs="?", default="-",
                    help="weft export --json (default: stdin)")
    ap.add_argument("--kinds", help="comma-separated edge kinds (default: the directed family)")
    args = ap.parse_args()

    raw = sys.stdin.read() if args.input == "-" else open(args.input).read()
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as e:
        sys.exit(f"weft-regions: input is not JSON ({e}). Expected `weft export --json`.")
    if "nodes" not in data:
        sys.exit("weft-regions: no `nodes` key. Expected `weft export --json`.")
    data.setdefault("edges", [])

    if args.kinds:
        report(data, [k.strip() for k in args.kinds.split(",") if k.strip()], "requested")
    else:
        report(data, DIRECTED, "directed family")
        report(data, ["part-of"], "part-of alone")


if __name__ == "__main__":
    main()
