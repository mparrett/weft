#!/usr/bin/env python3
"""Render `weft export --json` as one HTML page.

`weft view` is the command for this; it fills the same template
(cmd/weft/view.html) from the live store. This tool is the same page built from
an export you already have — a saved file, or one from another machine — and
`TestViewMatchesWeftHTML` pins the two to the same output. mermaid stays out of
both: weft's premise is a 2-5 ms cold start (BUILD.md §1.1), and the ~3.2 MB
renderer would cost every invocation of every command to serve one that draws.

By default the page loads mermaid from a pinned CDN build, checked by its
subresource-integrity hash, so it needs the network to draw. With
--self-contained it inlines a local mermaid.min.js instead, and the file works
offline and anywhere it is copied. mermaid is not vendored into this repo; pass
--mermaid-js, or let --self-contained find a local copy. Neither mode falls back
to the other: a failed local find is an error, never a quiet fetch, and the CDN
default never quietly inlines.

The visual language is deliberately borrowed from the pr-dashboard skill's
template, which solves the same problem — a self-contained local tool page — so
the two read as siblings rather than as two unrelated one-offs.

    weft export --json --include all | tools/weft-html.py -o graph.html
    tools/weft-html.py --self-contained -o graph.html < graph.json
    tools/weft-html.py --mermaid-js ./mermaid.min.js -o graph.html < graph.json

`--include all` is the recipe rather than a refinement of it. The default
export carries identity and no provenance, so a page built from it draws an
inferred edge exactly like an asserted one, shows no claims, and dates no
upstream observation — the three losses this renderer exists to undo. The page
reads either shape, so a plain export still works; it just says less.

One page serves every view. Scope, filter, and layout live in the URL fragment,
so `graph.html#ns=acme/api` and `graph.html#q=export+%21test` are links rather
than instructions for which dropdowns to set. `--ns` only chooses what an
unadorned URL opens on; the page still holds every namespace.

`--only-ns` is the other thing, and the difference matters the moment the file
leaves the machine: it drops the other namespaces from the *data*, so what is
not in the page cannot be read out of it.
"""

import argparse
import glob
import html
import json
import os
import re
import sys

# The CDN build the default page loads. Pinned to an exact version with its
# integrity hash rather than tracking a tag: a page whose drawing changes because
# someone upstream shipped is not reproducible, and reproducibility is most of
# why the fragment carries view state at all. tiny is the flowchart-capable
# subset, a drop-in for the full bundle (size measured when the CDN build was
# chosen). Bumping it means changing both lines together;
# tools/test_weft_html.py's network test checks that the hash still matches
# the file.
MERMAID_CDN_URL = "https://cdn.jsdelivr.net/npm/@mermaid-js/tiny@11.17.2/dist/mermaid.tiny.js"
MERMAID_CDN_SRI = "sha384-10s6o+N3M2s7dB3bn+t7QT0KKY4aJUPH3JgMK/M0DCc/C23fI5mm0h2dPQcRtVdd"

# Where a local mermaid tends to live, most-specific first. Only used for
# --self-contained when neither --mermaid-js nor $WEFT_MERMAID_JS is set; a miss
# is a clear error, never a silent CDN fallback, because the user asked for a
# page that does not fetch.
#
# These are conventional install locations, not anyone's project layout: a
# personal directory tree hardcoded here would be dead weight for every other
# reader and would still not help the person who has it.
MERMAID_HINTS = [
    "./mermaid.min.js",
    "./node_modules/mermaid/dist/mermaid.min.js",
    os.path.expanduser("~/.npm/_npx/*/node_modules/mermaid/dist/mermaid.min.js"),
    os.path.expanduser("~/.cache/npm/_npx/*/node_modules/mermaid/dist/mermaid.min.js"),
    "/usr/local/lib/node_modules/mermaid/dist/mermaid.min.js",
    "/opt/homebrew/lib/node_modules/mermaid/dist/mermaid.min.js",
]


def find_mermaid(explicit):
    for candidate in (explicit, os.environ.get("WEFT_MERMAID_JS")):
        if candidate:
            if not os.path.isfile(candidate):
                sys.exit(f"weft-html: no such file: {candidate}")
            return candidate
    for pattern in MERMAID_HINTS:
        for hit in sorted(glob.glob(pattern)):
            return hit
    sys.exit(
        "weft-html: --self-contained could not find mermaid.min.js.\n"
        "  Pass --mermaid-js <path>, or set $WEFT_MERMAID_JS, or run:\n"
        "    npm install mermaid   # then it is found automatically\n"
        "  Or drop --self-contained to load mermaid from the CDN."
    )


# The page template is shared with `weft view`, which embeds it with go:embed;
# go:embed cannot reach out of its package directory, so the file lives under
# cmd/weft/ and this tool reads it from there. One template, two fillers — the
# Go test TestViewMatchesWeftHTML pins that both produce the same page.
# realpath, not abspath: a symlink to this script from ~/bin must still find the
# checkout it lives in.
TEMPLATE = os.path.join(os.path.dirname(os.path.realpath(__file__)), "..", "cmd", "weft", "view.html")


def load_page():
    try:
        with open(TEMPLATE, encoding="utf-8") as f:
            return f.read()
    except FileNotFoundError:
        # A copy of this script outside a weft checkout has no template beside
        # it. `weft view` carries the template in the binary, so it is the fix.
        sys.exit(f"weft-html: page template not found at {os.path.normpath(TEMPLATE)}.\n"
                 "  This tool reads it from the weft checkout it lives in. Run it from\n"
                 "  there (a symlink is fine), or use `weft view`, which embeds it.")


def scope_to_ns(data, ns):
    """Keep only `ns`, stubbing the far end of every edge that leaves it.

    A cross-boundary edge is kept and its outside endpoint is reduced to a stub:
    id and namespace. Dropping those edges instead would make the page
    assert that a node has no blockers when it has one in another project, which
    is the failure SPEC §5 exists to prevent — and a page that quietly hides a
    blocker is worse than no page, because the whole reason to scope one is to
    send it to somebody.
    """
    kept = [n for n in data["nodes"] if n.get("ns") == ns]
    if not kept:
        # Failing loudly beats writing a page with nothing in it: the usual
        # cause is a typo, and an empty artifact looks like an empty graph.
        have = ", ".join(sorted({n.get("ns", "") for n in data["nodes"]})) or "nothing"
        sys.exit(f"weft-html: no nodes in namespace {ns!r}. This export holds: {have}")

    inside = {n["id"] for n in kept}
    edges = [e for e in data["edges"]
             if e.get("from") in inside or e.get("to") in inside]

    by_id = {n["id"]: n for n in data["nodes"]}
    stubs = {}
    for e in edges:
        for end in (e.get("from"), e.get("to")):
            if not end or end in inside or end in stubs:
                continue
            # A dangling endpoint names a node the export does not carry, which
            # is a normal state rather than an error (SPEC §9.3). It stubs the
            # same way, with an unknown namespace.
            src = by_id.get(end, {})
            stubs[end] = {"id": end, "ns": src.get("ns", ""), "title": "",
                          "external": True}

    # This is a disclosure boundary, so whitelist the two fields the viewer
    # consumes rather than carrying unknown top-level data into a shareable
    # artifact. A future export can add metadata without silently defeating the
    # promise that only the scoped graph leaves the machine.
    return {"nodes": kept + list(stubs.values()), "edges": edges}


def default_out(title):
    """A cache path, not the working directory.

    The obvious default writes a 3.2 MB artifact into whatever repo you happen
    to be standing in — which is how one ended up untracked in a third-party
    checkout. XDG_CACHE_HOME is right rather than DATA: the file is wholly
    derived from the log and regenerating it is one command.
    """
    base = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
    slug = re.sub(r"[^a-z0-9]+", "-", title.lower()).strip("-") or "graph"
    return os.path.join(base, "weft", slug + ".html")


def main():
    ap = argparse.ArgumentParser(description="Render weft export --json as an HTML page.")
    ap.add_argument("-o", "--out", help="output file ('-' for stdout; "
                                        "default: $XDG_CACHE_HOME/weft/<title>.html)")
    ap.add_argument("--self-contained", action="store_true",
                    help="inline a local mermaid.min.js so the page draws offline and can be "
                         "shared as one file (default: load a pinned build from the CDN)")
    ap.add_argument("--mermaid-js", help="path to mermaid.min.js to inline; implies "
                                         "--self-contained")
    ap.add_argument("--title", default="weft", help="page title")
    ap.add_argument("--ns", default="", help="namespace the viewer opens scoped to when the "
                                             "URL says nothing (the page holds them all, and "
                                             "#ns=... overrides this)")
    ap.add_argument("--only-ns", default="", help="build the page from this namespace alone, so "
                                                  "the others are not in the file at all; edges "
                                                  "leaving it keep a stub of the far endpoint")
    ap.add_argument("input", nargs="?", default="-", help="weft export --json (default: stdin)")
    args = ap.parse_args()
    if args.out is None:
        args.out = default_out(args.title)

    raw = sys.stdin.read() if args.input == "-" else open(args.input).read()
    try:
        data = json.loads(raw)
    except json.JSONDecodeError as e:
        sys.exit(f"weft-html: input is not JSON ({e}). Expected `weft export --json`.")
    if "nodes" not in data:
        sys.exit("weft-html: no `nodes` key. Expected `weft export --json`, not `ls --json`.")
    data.setdefault("edges", [])
    if args.only_ns:
        data = scope_to_ns(data, args.only_ns)
        # A scoped page that opened on "all" would show the stubs as peers of
        # real work on first paint. The reader can still widen it; the default
        # just matches what the file is for.
        args.ns = args.ns or args.only_ns

    # An explicit bundle path is a request to inline it, so it selects the mode.
    # $WEFT_MERMAID_JS does not: it says where a bundle is, and an environment
    # variable silently turning the default page offline-only is the quiet mode
    # switch the CDN-by-default design rules out.
    inline = args.self_contained or bool(args.mermaid_js)
    if inline:
        bundle = open(find_mermaid(args.mermaid_js), encoding="utf-8").read()
        mermaid_tag = "<script>" + bundle + "</script>"
        mermaid_from = ""
    else:
        mermaid_tag = (f'<script src="{MERMAID_CDN_URL}" integrity="{MERMAID_CDN_SRI}" '
                       'crossorigin="anonymous"></script>')
        mermaid_from = MERMAID_CDN_URL

    # `</script>` inside any inline-script value ends the element early, no
    # matter how valid the JSON is. `<\\/` is a valid JSON escape for `/` and
    # parses back to the original string. Reachable inputs include imported
    # issue titles and a namespace supplied to --ns/--only-ns.
    def script_json(value):
        return json.dumps(value).replace("</", "<\\/")

    data_js = script_json(data)

    # One pass, not three chained replaces: chaining exposes each substitution's
    # payload to the next, so a node titled "__MERMAID__" would splice the
    # bundle into the data literal and a --title of "__DATA__" would dump the
    # graph into <title>.
    filling = {
        "__TITLE__": html.escape(args.title),
        "__DATA__": data_js,
        "__INITIAL_NS__": script_json(args.ns),
        "__SCOPE__": script_json(args.only_ns),
        "__MERMAID_SRC__": script_json(mermaid_from),
        "__MERMAID__": mermaid_tag,
    }
    page = re.sub("__(?:TITLE|DATA|INITIAL_NS|SCOPE|MERMAID_SRC|MERMAID)__",
                  lambda m: filling[m.group(0)], load_page())

    if args.out == "-":
        sys.stdout.write(page)
    else:
        os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(page)
        how = "self-contained" if inline else "loads mermaid from the CDN; --self-contained to inline"
        print(f"{args.out}  ({len(page)/1_048_576:.1f} MB, {how})", file=sys.stderr)


if __name__ == "__main__":
    main()
