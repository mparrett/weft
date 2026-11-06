#!/usr/bin/env python3
"""Emit markdown describing what a commit range touches in the weft graph.

A diff says what changed. It cannot say what the change is *connected* to —
which issue it closes, what is still blocking the work, who is waiting on it, or
what was learned while doing it. That lives in weft, and the join between them
is the issue and PR numbers a commit range already mentions.

Written for `preview-diff --body`, which renders markdown above the diff:

    tools/weft-range-context.py -C ~/src/repo origin/main HEAD > /tmp/ctx.md
    preview-diff --body /tmp/ctx.md origin/main HEAD

It reads the graph with a single `weft export --json`, not one `weft show` per
reference. N shows would be N folds at N instants, so a write landing mid-loop
would describe a graph that never existed — and the cost would grow with the
number of references rather than staying flat.

Nothing here writes to the store, so it is safe to run against work in progress.
"""

import argparse
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys

# Squash-merge subjects look like `fix(aot): ... (#554) (#653)` — issues first,
# PR number last — so every number in the message is a candidate and none of
# them can be told apart without asking GitHub. Resolving each against the graph
# is cheaper and needs no network.
REF_IN_MESSAGE = re.compile(r"#(\d+)")


def run(cmd, cwd=None):
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit(f"weft-range-context: {' '.join(cmd)} failed: {p.stderr.strip()}")
    return p.stdout


def repo_slug(cwd):
    """owner/repo from the origin remote, which is what a gh: ref is keyed on."""
    url = run(["git", "remote", "get-url", "origin"], cwd).strip()
    m = re.search(r"github\.com[:/](.+?)(?:\.git)?$", url)
    if not m:
        sys.exit(f"weft-range-context: origin is not a GitHub remote: {url}")
    return m.group(1)


def numbers_in_range(cwd, base, head):
    """Issue/PR numbers mentioned by the commits in base..head, in first-seen
    order — commit order is more useful than numeric order, since the newest
    commit is usually the one being previewed."""
    log = run(["git", "log", "--format=%s%n%b", f"{base}..{head}"], cwd)
    seen, out = set(), []
    for n in REF_IN_MESSAGE.findall(log):
        if n not in seen:
            seen.add(n)
            out.append(n)
    return out


def weft_bin():
    """Where to find weft: $WEFT_BIN, then a local build, then PATH.

    Naming only the PATH entry meant this worked on a machine that had run
    `make install` and nowhere else — including a clean checkout, where it
    exited with a raw FileNotFoundError traceback. Invisible until something
    ran it somewhere clean, which is exactly what CI does now.
    """
    if os.environ.get("WEFT_BIN"):
        return os.environ["WEFT_BIN"]
    local = pathlib.Path(__file__).resolve().parent.parent / "dist" / "weft"
    if local.is_file() and os.access(local, os.X_OK):
        return str(local)
    return shutil.which("weft") or "weft"


def load_graph():
    binary = weft_bin()
    try:
        raw = subprocess.run([binary, "export", "--json"], capture_output=True, text=True)
    except OSError as e:
        sys.exit(f"weft-range-context: cannot run {binary!r}: {e}\n"
                 f"  Build it with `make`, install it, or set $WEFT_BIN.")
    if raw.returncode != 0:
        sys.exit(f"weft-range-context: weft export failed: {raw.stderr.strip()}")
    return json.loads(raw.stdout)


def md_escape(s):
    """Titles are free text and routinely contain markdown punctuation."""
    return re.sub(r"([*_`\[\]])", r"\\\1", s)


def gh_url(slug, num):
    # /issues/N redirects to /pull/N when N is a PR, so one form serves both.
    return f"https://github.com/{slug}/issues/{num}"


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("base", nargs="?", default="origin/main")
    ap.add_argument("head", nargs="?", default="HEAD")
    ap.add_argument("-C", dest="cwd", default=".", help="run git in this repo")
    ap.add_argument("--title", default="weft context")
    args = ap.parse_args()

    slug = repo_slug(args.cwd)
    nums = numbers_in_range(args.cwd, args.base, args.head)

    # Nothing to look up means nothing to load. Folding the store to answer a
    # question with no subject was pure waste, and it made a range that mentions
    # no issues depend on having a weft binary at all.
    if not nums:
        print(f"## {args.title}\n\n"
              f"No issue or PR numbers are mentioned in `{args.base}..{args.head}`.")
        return

    graph = load_graph()

    nodes = {n["id"]: n for n in graph["nodes"]}
    by_ref = {}
    for n in graph["nodes"]:
        for r in n.get("refs", []):
            by_ref[r] = n

    matched, missing = [], []
    for num in nums:
        node = by_ref.get(f"gh:{slug}#{num}") or by_ref.get(f"pr:{slug}#{num}")
        (matched.append((num, node)) if node else missing.append(num))

    out = [f"## {args.title}", ""]
    out.append(f"`{args.base}..{args.head}` mentions **{len(nums)}** issue/PR "
               f"numbers; **{len(matched)}** are tracked in weft.")
    out.append("")

    if not matched:
        out.append("_None of them are in the graph, so there is no context to add. "
                   "`weft add --ref gh:%s#N` binds one._" % slug)
        print("\n".join(out))
        return

    # Direct edges only, deliberately: a transitive closure of a real graph
    # buries the change being previewed under work several hops away. The one
    # thing a reviewer needs is what sits immediately either side of it.
    deps, rdeps, context = {}, {}, {}
    for e in graph["edges"]:
        if e["kind"] == "depends-on":
            deps.setdefault(e["from"], []).append(e["to"])
            rdeps.setdefault(e["to"], []).append(e["from"])
        elif e["kind"] == "derived-from":
            context.setdefault(e["to"], []).append(e["from"])

    out.append("### In this range")
    out.append("")
    for num, node in matched:
        state = node["state"]
        mark = {"done": "done", "dropped": "dropped"}.get(state, "open")
        out.append(f"- [**#{num}**]({gh_url(slug, num)}) {md_escape(node['title'])} "
                   f"— `{mark}` · `{node['kind']}` · `weft show {node['id'][:7]}`")

        blocking = [nodes[b] for b in deps.get(node["id"], [])
                    if b in nodes and nodes[b]["state"] == "open"]
        for b in blocking:
            out.append(f"    - **still blocked by** {md_escape(b['title'])} "
                       f"(`weft show {b['id'][:7]}`)")

        waiting = [nodes[d] for d in rdeps.get(node["id"], [])
                   if d in nodes and nodes[d]["state"] == "open"]
        for w in waiting:
            out.append(f"    - unblocks {md_escape(w['title'])}")

        # Findings recorded *from* this work are the part no tracker holds, and
        # the part a reviewer is least likely to have seen.
        for c in (nodes[f] for f in context.get(node["id"], []) if f in nodes):
            out.append(f"    - learned here: {md_escape(c['title'])}")
    out.append("")

    if missing:
        shown = ", ".join(f"#{n}" for n in missing[:12])
        more = f" (+{len(missing) - 12} more)" if len(missing) > 12 else ""
        out.append(f"_Not in weft: {shown}{more}._")

    print("\n".join(out))


if __name__ == "__main__":
    main()
