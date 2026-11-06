#!/usr/bin/env python3
"""Render `weft export --json --include refs` as a chronology of artifacts.

This is the first consumer of the artifact clock (SPEC.md §6.3): each node is
placed by what the world says about the artifact — the earliest
`upstream_created` among its observed refs — never by when weft looked at it.
`observed_at` is the trust clock, and a timeline laid out by it would say
"when did we poll", which is the confusion the two-clock rule exists to end.

A node whose refs nobody has observed still lands on the line, placed by its
own `created` — when the claim was asserted — and marked `~`, because asserted
time is an honest fallback but a different fact. Dropping such nodes instead
would make the timeline quietly shrink to the imported subset.

Dates render as absolutes, not ages. Export carries absolutes precisely so a
consumer does not re-derive timestamps (SPEC.md §6.4), and a canonical page
must not depend on when it was generated: the same export renders
byte-identically forever, which is what makes the output diffable in git
(SPEC.md §9.5).

    weft export --json --include refs | tools/weft-timeline.py
    tools/weft-timeline.py --by month --mermaid < export.json

`--mermaid` emits a mermaid `timeline` block instead of text — the "hint an
external layout engine" mode. Titles are sanitized for mermaid's syntax, and
the block is fenced so it drops straight into markdown.
"""

import argparse
import json
import re
import sys
from datetime import date, datetime


def parse_stamp(s):
    """RFC3339 to datetime, tolerating the Z suffix and fractional seconds."""
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None


def artifact_stamp(node):
    """The node's place on the line: (datetime, approximate?).

    Earliest upstream_created across observed refs; the node's own created,
    marked approximate, when no ref has been observed. Refs appear as bare
    strings in a default export and as objects under --include refs; both are
    tolerated (SPEC.md §6.4), a bare string simply carrying no clock.
    """
    stamps = []
    for ref in node.get("refs") or []:
        if isinstance(ref, dict):
            t = parse_stamp(ref.get("upstream_created"))
            if t:
                stamps.append(t)
    if stamps:
        return min(stamps), False
    return parse_stamp(node.get("created")), True


def cohort_key(t, by):
    if by == "day":
        return t.date().isoformat()
    if by == "month":
        return f"{t.year:04d}-{t.month:02d}"
    iso = t.isocalendar()
    return f"{iso[0]:04d}-W{iso[1]:02d}"


def rows(nodes, by):
    """Yield (cohort, [(datetime, approx, node)]) in chronological order.

    Within a cohort the order is stamp then id — id as the tiebreak so the
    output is canonical (SPEC.md §9.5): two nodes born the same second must
    not swap lines between runs.
    """
    placed = []
    for n in nodes:
        t, approx = artifact_stamp(n)
        if t is None:
            continue
        placed.append((t, approx, n))
    placed.sort(key=lambda p: (p[0], p[2].get("id", "")))
    cohorts = []
    for t, approx, n in placed:
        key = cohort_key(t, by)
        if not cohorts or cohorts[-1][0] != key:
            cohorts.append((key, []))
        cohorts[-1][1].append((t, approx, n))
    return cohorts


def render_text(cohorts, out):
    legend_needed = any(approx for _, items in cohorts for _, approx, _ in items)
    if legend_needed:
        out.write("~ placed by weft's own created (no observed ref carries a clock)\n\n")
    for key, items in cohorts:
        out.write(f"{key}  ({len(items)} node{'s' if len(items) != 1 else ''})\n")
        for t, approx, n in items:
            mark = "~" if approx else " "
            short = (n.get("id") or "")[:7]
            ns = n.get("ns", "")
            out.write(f"  {t.date().isoformat()} {mark} {short}  {n.get('title', '')}  [{ns}]\n")
        out.write("\n")


def mermaid_safe(s):
    """Strip what mermaid's timeline grammar would read as structure."""
    return re.sub(r"[:;#`\n]", " ", s).strip()


def render_mermaid(cohorts, out):
    out.write("```mermaid\ntimeline\n")
    for key, items in cohorts:
        out.write(f"    {key}")
        for _, approx, n in items:
            title = mermaid_safe(n.get("title", ""))
            if approx:
                title = "~" + title
            out.write(f" : {title}")
        out.write("\n")
    out.write("```\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--by", choices=["day", "week", "month"], default="week",
                    help="cohort granularity (default: week)")
    ap.add_argument("--mermaid", action="store_true",
                    help="emit a mermaid timeline block instead of text")
    ap.add_argument("-i", "--input", default="-", help="export JSON (default: stdin)")
    args = ap.parse_args()

    raw = sys.stdin.read() if args.input == "-" else open(args.input, encoding="utf-8").read()
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as e:
        sys.exit(f"weft-timeline: input is not JSON ({e}); expected `weft export --json`")
    nodes = data.get("nodes")
    if nodes is None:
        sys.exit("weft-timeline: no \"nodes\" in input; expected `weft export --json`")

    cohorts = rows(nodes, args.by)
    if not cohorts:
        # An empty line is an answer, not an error: a store can hold nodes
        # whose timestamps were never recorded (hand-built samples do).
        print("no datable nodes")
        return
    if args.mermaid:
        render_mermaid(cohorts, sys.stdout)
    else:
        render_text(cohorts, sys.stdout)


if __name__ == "__main__":
    main()
