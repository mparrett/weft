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
the block is fenced so it drops straight into markdown. Past a few dozen nodes
a mermaid timeline is one very tall column per period.

`--html` emits a self-contained page instead: periods newest first, each node
with its state, namespace and kind, and a filter box. Done and dropped work is
listed but hidden until asked for, as `weft ls` hides dropped; it stays in the
page because "what finished that week" is half of what a timeline is for. The
page loads nothing, and like the other outputs it is canonical: the same
export renders byte-identically, so it carries no generation time.

Every output marks state: ✓ done, ✗ dropped, nothing for open. Without it a
timeline reads finished and abandoned work as live.

    weft export --json --include refs | tools/weft-timeline.py --html > timeline.html
"""

import argparse
import html
import json
import re
import sys
from datetime import date, datetime


def parse_stamp(s):
    """RFC3339 to datetime, tolerating the Z suffix and fractional seconds."""
    if not s:
        return None
    # weft writes nanoseconds (EVENTS.md §2.5). Before Python 3.11,
    # fromisoformat takes exactly 3 or 6 fractional digits and raises on 9,
    # which would drop every node as undatable; microseconds are plenty here.
    s = re.sub(r"\.(\d+)", lambda m: "." + m.group(1)[:6].ljust(6, "0"), s.replace("Z", "+00:00"))
    try:
        return datetime.fromisoformat(s)
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


STATE_MARK = {"done": "\u2713", "dropped": "\u2717"}


def state_mark(n):
    """✓ done, ✗ dropped; open (or unknown) carries no mark."""
    return STATE_MARK.get(n.get("state"), "")


def render_text(cohorts, out):
    legend_needed = any(approx for _, items in cohorts for _, approx, _ in items)
    if legend_needed:
        out.write("~ placed by weft's own created (no observed ref carries a clock)\n\n")
    for key, items in cohorts:
        out.write(f"{key}  ({len(items)} node{'s' if len(items) != 1 else ''})\n")
        for t, approx, n in items:
            mark = "~" if approx else " "
            state = state_mark(n) or " "
            short = (n.get("id") or "")[:7]
            ns = n.get("ns", "")
            out.write(f"  {t.date().isoformat()} {mark} {state} {short}  {n.get('title', '')}  [{ns}]\n")
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
            if state_mark(n):
                title = state_mark(n) + " " + title
            if approx:
                title = "~" + title
            out.write(f" : {title}")
        out.write("\n")
    out.write("```\n")


HTML_STYLE = """
:root{--bg:#fbfbfc;--card:#fff;--ink:#141a24;--muted:#5b6675;--faint:#8a94a3;--line:#e3e7ec;
  --open:#1d4ed8;--done:#15803d;--drop:#b45309}
@media (prefers-color-scheme:dark){:root{--bg:#0f1318;--card:#161b22;--ink:#e6e9ee;--muted:#9aa4b2;
  --faint:#6b7686;--line:#2b333f;--open:#7ea2ff;--done:#4ade80;--drop:#f59e0b}}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 system-ui,sans-serif}
header{position:sticky;top:0;background:var(--bg);border-bottom:1px solid var(--line);
  padding:12px 16px;display:flex;gap:12px;flex-wrap:wrap;align-items:center}
h1{font-size:16px;margin:0}
input[type=search]{flex:1;min-width:180px;padding:6px 8px;border:1px solid var(--line);
  border-radius:6px;background:var(--card);color:var(--ink)}
label{color:var(--muted);font-size:13px}
main{max-width:960px;margin:0 auto;padding:8px 16px 40px}
.note{color:var(--muted);font-size:12px;margin:10px 0}
h2{font-size:14px;margin:18px 0 6px}.c{color:var(--faint);font-weight:400}
ul{list-style:none;margin:0;padding:0;background:var(--card);border:1px solid var(--line);border-radius:8px}
li{padding:5px 10px;border-top:1px solid var(--line);display:flex;gap:8px;align-items:baseline;flex-wrap:wrap}
li:first-child{border-top:0}
.d{color:var(--faint);font-variant-numeric:tabular-nums;min-width:92px}
code{color:var(--muted);font-size:12px}
.s{width:14px;text-align:center}.open .s{color:var(--open)}.done .s{color:var(--done)}.dropped .s{color:var(--drop)}
.done,.dropped{color:var(--muted)}.dropped .t{text-decoration:line-through;text-decoration-color:var(--faint)}
.ns,.k{font-size:11px;color:var(--faint);border:1px solid var(--line);border-radius:4px;padding:0 5px}
.ns{margin-left:auto}.hide{display:none}
"""

# Hides done and dropped until asked, filters on the text each row carries, and
# recounts each period so a header never claims rows the filter removed.
HTML_SCRIPT = """
const q=document.getElementById("q"),fin=document.getElementById("fin"),drop=document.getElementById("drop");
function apply(){const t=q.value.trim().toLowerCase();
for(const li of document.querySelectorAll("li.n")){const s=li.classList;
s.toggle("hide",(s.contains("done")&&!fin.checked)||(s.contains("dropped")&&!drop.checked)||
(t!==""&&!li.dataset.text.includes(t)));}
for(const sec of document.querySelectorAll("section")){const n=sec.querySelectorAll("li.n:not(.hide)").length;
sec.classList.toggle("hide",!n);sec.querySelector(".c").textContent=n;}
const shown=document.querySelectorAll("li.n:not(.hide)").length;
document.getElementById("shown").textContent=shown+(shown===1?" node shown":" nodes shown");}
[q,fin,drop].forEach(e=>e.addEventListener("input",apply));apply();
"""


def render_html(cohorts, out):
    """A page that works offline: periods newest first, rows newest first.

    Reversed from the text order because a page is read from the top, and the
    question it is opened for is "what happened lately"."""
    esc = html.escape
    approx_any = any(approx for _, items in cohorts for _, approx, _ in items)
    approx_all = all(approx for _, items in cohorts for _, approx, _ in items)
    total = sum(len(items) for _, items in cohorts)
    # Bare-string refs mean the export was made without --include refs, which
    # drops every upstream date; recording more observations would change
    # nothing, so the note has to name the flag instead.
    bare_refs = any(isinstance(r, str) for _, items in cohorts for _, _, n in items
                    for r in n.get("refs") or [])
    if approx_all and bare_refs:
        note = ("Every node is placed by when weft recorded it (~): this export carries refs "
                "without their upstream dates. Export with <code>--include refs</code> to use them.")
    elif approx_all:
        note = ("Every node is placed by when weft recorded it (~): no ref carries an "
                "upstream date yet. <code>weft import gh</code> and <code>weft ref observe</code> record them.")
    elif approx_any:
        note = "~ placed by weft's own created, because no observed ref carries a clock."
    else:
        note = ""
    sections = []
    for key, items in reversed(cohorts):
        rows = []
        for t, approx, n in reversed(items):
            # `or ""` throughout, as render_text does: a hand-built export can
            # carry null fields, and a page must not crash where text renders.
            f = lambda k: n.get(k) or ""
            state = f("state") if f("state") in ("done", "dropped") else "open"
            text = " ".join([f("title"), f("ns"), f("kind"), f("id")[:7]]).lower()
            # Bound outside the f-string: a backslash inside an f-string
            # expression is a syntax error before Python 3.12.
            mark = state_mark(n) or "\u25cf"
            rows.append(
                f'<li class="n {state}" data-text="{esc(text)}">'
                f'<span class="d">{t.date().isoformat()}{" ~" if approx else ""}</span>'
                f'<span class="s" title="{state}">{mark}</span>'
                f'<code>{esc(f("id")[:7])}</code> <span class="t">{esc(f("title"))}</span>'
                f'<span class="ns">{esc(f("ns"))}</span>'
                f'<span class="k">{esc(f("kind"))}</span></li>')
        sections.append(f'<section><h2>{esc(key)} <span class="c">{len(items)}</span></h2>'
                        f'<ul>{"".join(rows)}</ul></section>')
    out.write(
        '<!doctype html>\n<html lang="en"><head><meta charset="utf-8">\n'
        '<meta name="viewport" content="width=device-width,initial-scale=1">\n'
        f'<title>weft timeline</title>\n<style>{HTML_STYLE}</style></head><body>\n'
        '<header><h1>weft timeline</h1>\n'
        '<input type="search" id="q" placeholder="filter by title, namespace, kind, id">\n'
        '<label><input type="checkbox" id="fin"> show done</label>\n'
        '<label><input type="checkbox" id="drop"> show dropped</label></header>\n'
        # The count is filled in by the script, because done and dropped start
        # hidden; a static total would disagree with the rows on screen.
        f'<main><p class="note">{note} <span id="shown"></span> of {total}. '
        '\u25cf open \u00b7 \u2713 done \u00b7 \u2717 dropped.</p>\n'
        + "\n".join(sections) +
        f'\n</main>\n<script>{HTML_SCRIPT}</script>\n</body></html>\n')


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--by", choices=["day", "week", "month"], default="week",
                    help="cohort granularity (default: week)")
    fmt = ap.add_mutually_exclusive_group()
    fmt.add_argument("--mermaid", action="store_true",
                     help="emit a mermaid timeline block instead of text")
    fmt.add_argument("--html", action="store_true",
                     help="emit a self-contained HTML page instead of text")
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
    elif args.html:
        render_html(cohorts, sys.stdout)
    else:
        render_text(cohorts, sys.stdout)


if __name__ == "__main__":
    main()
