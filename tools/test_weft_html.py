#!/usr/bin/env python3
"""Tests for weft-html.py.

These exist because a review found three defects here that no amount of looking
at the page would surface: the reviewer's own store held no node title with a
`</script>` in it, so manual verification could not have caught the one that
breaks the page completely. Every case below is a string assertion over the
generated document — no browser, no mermaid, so it runs anywhere `go test` does.

    python3 tools/test_weft_html.py
"""

import json
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
TOOL = HERE / "weft-html.py"


def render_data(data, title="weft", mermaid="/*stub*/", args=()):
    """Run the generator over an export-shaped object and return the page."""
    with tempfile.TemporaryDirectory() as d:
        js = Path(d) / "mermaid.min.js"
        js.write_text(mermaid, encoding="utf-8")
        out = subprocess.run(
            [sys.executable, str(TOOL), "--mermaid-js", str(js), "--title", title, "-o", "-",
             *args],
            input=json.dumps(data),
            capture_output=True, text=True, check=True,
        )
        return out.stdout


def render(nodes, edges=(), title="weft", mermaid="/*stub*/", args=()):
    """Run the generator over a graph and return the page."""
    return render_data({"nodes": list(nodes), "edges": list(edges)},
                       title=title, mermaid=mermaid, args=args)


def page_data(page):
    """The graph as it reached the browser."""
    return json.loads(page.split("const DATA = ", 1)[1].split(";\n", 1)[0])


def node(title, **kw):
    base = {"id": "a" * 64, "ns": "acme/api", "kind": "task",
            "title": title, "state": "open"}
    base.update(kw)
    return base


class TestScriptInjection(unittest.TestCase):
    """The graph is spliced into an inline <script>, so `</script>` in any
    string ends it early — regardless of how valid the JSON is. Go escapes `<`
    on the wire but json.dumps decodes it, so the protection is lost in transit.
    Reachable input: `weft import gh` copies third-party issue titles verbatim.
    """

    def test_closing_script_tag_in_title_cannot_escape(self):
        page = render([node("</script><img src=x onerror=alert(1)>")])
        body = page.split("<script>", 2)[-1]
        self.assertNotIn("</script><img", body)
        self.assertIn("<\\/script>", page)

    def test_payload_survives_a_json_round_trip(self):
        evil = "</script>alert(1)//"
        page = render([node(evil)])
        data = page.split("const DATA = ", 1)[1].split(";\n", 1)[0]
        self.assertEqual(json.loads(data)["nodes"][0]["title"], evil)

    def test_closing_tag_in_a_ref_is_escaped_too(self):
        page = render([node("ok", refs=["x:</script>"])])
        self.assertNotIn("</script>", page.split("const DATA = ", 1)[1][:400])

    def test_closing_tag_in_only_ns_cannot_escape(self):
        evil = "</script><img src=x onerror=alert(1)>"
        page = render([node("ok", ns=evil)], args=["--only-ns", evil])
        body = page.split("<script>", 2)[-1]
        self.assertNotIn("</script><img", body)
        self.assertIn('const SCOPE = "<\\/script>', page)


class TestPlaceholderSubstitution(unittest.TestCase):
    """Substitution must be single-pass. Chained replaces expose each payload
    to the next, so content can impersonate a later placeholder.
    """

    def test_a_title_naming_a_placeholder_is_not_expanded(self):
        page = render([node("__MERMAID__")], mermaid="MERMAID_BUNDLE_SENTINEL")
        data = page.split("const DATA = ", 1)[1].split(";\n", 1)[0]
        self.assertEqual(json.loads(data)["nodes"][0]["title"], "__MERMAID__")
        self.assertEqual(page.count("MERMAID_BUNDLE_SENTINEL"), 1)

    def test_a_page_title_naming_a_placeholder_is_not_expanded(self):
        page = render([node("ok")], title="__DATA__")
        self.assertIn("<title>__DATA__</title>", page)
        self.assertEqual(page.count("const DATA = "), 1)


class TestMermaidLabelEscaping(unittest.TestCase):
    """htmlLabels is on, so a bare `<` swallows the rest of the label:
    `fix N<M comparison` renders as `fix N`.
    """

    def test_angle_brackets_are_escaped(self):
        page = render([node("x")])
        self.assertIn('.replace(/</g, "#lt;")', page)
        self.assertIn('.replace(/&/g, "#amp;")', page)

    def test_ampersand_is_escaped_before_the_others(self):
        js = page_fn(render([node("x")]), "mlabel")
        self.assertLess(js.index("/&/g"), js.index("/</g"),
                        "ampersand must be escaped first or later escapes double-encode")


def run_tool(args, data=None, env=None):
    """Run the generator with exactly these arguments, no --mermaid-js added."""
    import os
    return subprocess.run([sys.executable, str(TOOL), *args],
                          input=json.dumps(data or {"nodes": [node("x")], "edges": []}),
                          capture_output=True, text=True,
                          env={**os.environ, **(env or {})})


def load_tool():
    import importlib.util
    spec = importlib.util.spec_from_file_location("weft_html", TOOL)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def page_fn_decl(page, name):
    """The body of a `function name()` declaration, up to its closing brace at
    column 0 (page_fn handles the `const name = ` arrow form)."""
    start = page.index("function " + name + "(")
    return page[start:page.index("\n}\n", start)]


REMOTE = ("https://cdn", "unpkg.com", "jsdelivr", "<script src=")


class TestTemplateLocation(unittest.TestCase):
    """The template lives in cmd/weft/ for go:embed, so the tool finds it
    relative to its own real location — a symlink into ~/bin keeps working,
    and a copy with no checkout says what to do instead of a traceback."""

    def run_as(self, script, d):
        return subprocess.run([sys.executable, str(script), "-o", "-"],
                              input=json.dumps({"nodes": [node("x")]}),
                              capture_output=True, text=True, cwd=d)

    def test_a_symlink_finds_the_checkout(self):
        with tempfile.TemporaryDirectory() as d:
            link = Path(d) / "weft-html"
            link.symlink_to(TOOL)
            r = self.run_as(link, d)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("<!doctype html>", r.stdout)

    def test_a_copy_outside_the_checkout_points_at_weft_view(self):
        with tempfile.TemporaryDirectory() as d:
            copy = Path(d) / "weft-html.py"
            copy.write_text(TOOL.read_text(encoding="utf-8"), encoding="utf-8")
            r = self.run_as(copy, d)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("page template not found", r.stderr)
        self.assertIn("weft view", r.stderr)
        self.assertNotIn("Traceback", r.stderr)


class TestRendererModes(unittest.TestCase):
    """CDN by default, --self-contained to inline, and no silent fallback
    in either direction — each mode does what it says."""

    def test_mermaid_js_inlines_and_fetches_nothing(self):
        page = render([node("x")], mermaid="BUNDLE")
        for remote in REMOTE:
            self.assertNotIn(remote, page)
        self.assertIn("<script>BUNDLE</script>", page)

    def test_self_contained_finds_the_env_bundle_and_fetches_nothing(self):
        with tempfile.TemporaryDirectory() as d:
            js = Path(d) / "mermaid.min.js"
            js.write_text("ENV_BUNDLE", encoding="utf-8")
            r = run_tool(["--self-contained", "-o", "-"], env={"WEFT_MERMAID_JS": str(js)})
        self.assertEqual(r.returncode, 0, r.stderr)
        for remote in REMOTE:
            self.assertNotIn(remote, r.stdout)
        self.assertIn("<script>ENV_BUNDLE</script>", r.stdout)

    def test_default_loads_the_pinned_cdn_build(self):
        tool = load_tool()
        r = run_tool(["-o", "-"], env={"WEFT_MERMAID_JS": ""})
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn(f'<script src="{tool.MERMAID_CDN_URL}" integrity="{tool.MERMAID_CDN_SRI}" '
                      'crossorigin="anonymous"></script>', r.stdout)
        self.assertNotIn("__MERMAID", r.stdout)

    def test_env_bundle_does_not_switch_the_default_to_inline(self):
        """$WEFT_MERMAID_JS says where a bundle is, not which mode to use;
        letting it turn the default page offline-only is a quiet mode switch."""
        with tempfile.TemporaryDirectory() as d:
            js = Path(d) / "mermaid.min.js"
            js.write_text("ENV_BUNDLE", encoding="utf-8")
            r = run_tool(["-o", "-"], env={"WEFT_MERMAID_JS": str(js)})
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn("ENV_BUNDLE", r.stdout)
        self.assertIn("<script src=", r.stdout)

    def test_self_contained_miss_is_an_error_not_a_fetch(self):
        """Called in-process with the hint list emptied: some hints are absolute
        global-install paths, so a subprocess run would pass or fail on whether
        the host happens to have mermaid installed."""
        import contextlib, io, os
        from unittest import mock
        tool = load_tool()
        err = io.StringIO()
        with mock.patch.object(tool, "MERMAID_HINTS", []), \
             mock.patch.dict(os.environ, {"WEFT_MERMAID_JS": ""}), \
             contextlib.redirect_stderr(err), \
             self.assertRaises(SystemExit) as cm:
            tool.find_mermaid(None)
        self.assertIn("could not find mermaid.min.js", str(cm.exception.code))

    def test_inlined_page_blames_the_bundle_not_the_network(self):
        """A --mermaid-js file that defines no global is the wrong file; telling
        its user to rebuild with --self-contained sends them in a circle."""
        page = render([node("x")])
        self.assertIn('const MERMAID_SRC = "";', page)
        self.assertIn("defines no <code>mermaid</code> global", page)

    def test_a_page_that_cannot_draw_still_records_its_view(self):
        body = page_fn_decl(render([node("x")]), "draw")
        self.assertLess(body.index("syncURL();"), body.index("if (!rendererLoaded()) return;"))

    def test_the_pin_is_exact(self):
        """A tag or a bare major would let the drawing change under a page
        nobody rebuilt."""
        url = load_tool().MERMAID_CDN_URL
        self.assertRegex(url, r"@\d+\.\d+\.\d+/")
        self.assertNotIn("latest", url)

    def test_page_says_why_when_the_renderer_did_not_load(self):
        page = render([node("x")])
        self.assertIn('if (typeof mermaid !== "undefined") return true;', page)
        self.assertIn("--self-contained", page_fn_decl(page, "rendererLoaded"))
        self.assertIn("if (!rendererLoaded()) return;", page)

    def test_pinned_hash_matches_the_cdn_file(self):
        """The hash and the URL move together; a bump that changes one makes
        every default page refuse to draw. Needs the network, so it skips
        rather than fails when there is none."""
        import base64, hashlib, urllib.error, urllib.request
        tool = load_tool()
        try:
            with urllib.request.urlopen(tool.MERMAID_CDN_URL, timeout=20) as resp:
                body = resp.read()
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            self.skipTest(f"no network: {e}")
        digest = base64.b64encode(hashlib.sha384(body).digest()).decode()
        self.assertEqual(tool.MERMAID_CDN_SRI, "sha384-" + digest)


class TestStructure(unittest.TestCase):

    def test_edges_and_nodes_both_survive(self):
        a, b = "a" * 64, "b" * 64
        page = render([node("A", id=a), node("B", id=b)],
                      [{"kind": "depends-on", "from": a, "to": b}])
        data = json.loads(page.split("const DATA = ", 1)[1].split(";\n", 1)[0])
        self.assertEqual(len(data["nodes"]), 2)
        self.assertEqual(data["edges"][0]["kind"], "depends-on")

    def test_timestamps_survive_into_the_page(self):
        """Recency is only as good as the data reaching the browser, and the
        export omits both fields when a node has none — so the page has to carry
        what is there and tolerate what is not."""
        a, b = "a" * 64, "b" * 64
        page = render([node("dated", id=a, created="2026-07-30T09:15:00Z",
                            touched="2026-08-04T17:40:30Z"),
                       node("undated", id=b)])
        nodes = json.loads(page.split("const DATA = ", 1)[1].split(";\n", 1)[0])["nodes"]
        self.assertEqual(nodes[0]["created"], "2026-07-30T09:15:00Z")
        self.assertEqual(nodes[0]["touched"], "2026-08-04T17:40:30Z")
        self.assertNotIn("created", nodes[1])

    def test_ages_are_compared_as_instants_not_strings(self):
        """Go's RFC3339Nano omits the fraction entirely when it is zero, so a
        whole-second `created` and a fractional `touched` one moment later
        compare 'Z' (0x5A) against '.' (0x2E) and order backwards. A string
        compare drops the touched clause for a node that did move — and the
        export fixture happens to use whole seconds for both fields, which is
        the one shape where the bug is invisible."""
        page = render([node("skewed", created="2026-08-06T07:50:41Z",
                            touched="2026-08-06T07:50:41.5Z")])
        self.assertNotIn("n.touched > n.created", page)
        self.assertIn("Date.parse(n.touched) > Date.parse(n.created)", page)

    def test_rejects_ls_json_with_a_usable_message(self):
        with tempfile.TemporaryDirectory() as d:
            js = Path(d) / "m.js"
            js.write_text("x")
            r = subprocess.run([sys.executable, str(TOOL), "--mermaid-js", str(js), "-o", "-"],
                               input='{"wrong": []}', capture_output=True, text=True)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("export --json", r.stderr)


class TestOnlyNS(unittest.TestCase):
    """`--only-ns` filters the data, not the opening view.

    The distinction is the whole feature: `--ns` leaves every namespace in the
    file, so a page built to send someone carries every title on the machine.
    What is not in the file cannot be read out of it.
    """

    A, B, C = "a" * 64, "b" * 64, "c" * 64

    def graph(self):
        return ([self.node_in("keep me"), self.node_in("keep me too", id=self.B),
                 node("secret work", id=self.C, ns="other/repo")],
                [{"kind": "depends-on", "from": self.A, "to": self.C},
                 {"kind": "part-of", "from": self.B, "to": self.A}])

    def node_in(self, title, **kw):
        return node(title, **kw)

    def test_other_namespaces_leave_the_file(self):
        nodes, edges = self.graph()
        page = render(nodes, edges, args=["--only-ns", "acme/api"])
        # The strong condition: not merely filtered from the view, absent from
        # the bytes. A title still in the page is a title that leaves with it.
        self.assertNotIn("secret work", page)
        kept = {n["title"] for n in page_data(page)["nodes"]}
        self.assertEqual(kept, {"keep me", "keep me too", ""})

    def test_a_crossing_edge_keeps_a_stub_rather_than_vanishing(self):
        """Dropping the edge would make the page assert that a node has no
        blockers when it has one in another project — the failure SPEC §5
        exists to prevent, and the reason to stub rather than filter."""
        nodes, edges = self.graph()
        data = page_data(render(nodes, edges, args=["--only-ns", "acme/api"]))
        kinds = sorted(e["kind"] for e in data["edges"])
        self.assertEqual(kinds, ["depends-on", "part-of"])
        stub = next(n for n in data["nodes"] if n.get("external"))
        self.assertEqual(stub["id"], self.C)
        self.assertEqual(stub["ns"], "other/repo")
        self.assertEqual(stub["title"], "")

    def test_a_stub_discloses_only_the_approved_boundary_fields(self):
        """The shareable page's boundary is id plus namespace. State, refs and
        timestamps are still work data even when the title has been removed."""
        nodes, edges = self.graph()
        nodes[2]["state"] = "done"
        nodes[2]["refs"] = ["gh:secret/repo#42"]
        nodes[2]["created"] = "2026-08-11T01:02:03Z"
        data = page_data(render(nodes, edges, args=["--only-ns", "acme/api"]))
        stub = next(n for n in data["nodes"] if n.get("external"))
        self.assertEqual(set(stub), {"id", "ns", "title", "external"})

    def test_unknown_top_level_data_cannot_cross_the_scope_boundary(self):
        nodes, edges = self.graph()
        page = render_data({"nodes": nodes, "edges": edges,
                            "future_index": {"secret work": self.C}},
                           args=["--only-ns", "acme/api"])
        self.assertNotIn("future_index", page_data(page))
        self.assertNotIn("secret work", page)

    def test_an_edge_wholly_outside_the_scope_is_dropped(self):
        """Context stops at one hop. Keeping the far side's own edges would
        walk the whole graph back into a page built to exclude it."""
        d = "d" * 64
        nodes, edges = self.graph()
        nodes.append(node("also outside", id=d, ns="other/repo"))
        edges.append({"kind": "depends-on", "from": self.C, "to": d})
        data = page_data(render(nodes, edges, args=["--only-ns", "acme/api"]))
        self.assertNotIn(d, [n["id"] for n in data["nodes"]])
        self.assertEqual(len(data["edges"]), 2)

    def test_the_page_says_it_is_partial(self):
        """An absence in a scoped page is not evidence of absence in the graph,
        and nothing else on screen tells the two apart."""
        nodes, edges = self.graph()
        page = render(nodes, edges, args=["--only-ns", "acme/api"])
        self.assertIn('const SCOPE = "acme/api"', page)
        # The banner is static markup, so its presence proves nothing; what has
        # to hold is that it stays hidden unless SCOPE says otherwise.
        self.assertIn('<span class="scope" id="scope" hidden>', page)
        self.assertIn("if (!SCOPE) return;", page)
        # And an unscoped page does not claim a scope it does not have.
        self.assertIn('const SCOPE = ""', render(*self.graph()))

    def test_scoping_sets_the_opening_view_too(self):
        nodes, edges = self.graph()
        page = render(nodes, edges, args=["--only-ns", "acme/api"])
        self.assertIn('const INITIAL_NS = "acme/api"', page)
        # An explicit --ns still wins: the flags answer different questions.
        page = render(nodes, edges, args=["--only-ns", "acme/api", "--ns", "other/repo"])
        self.assertIn('const INITIAL_NS = "other/repo"', page)

    def test_an_empty_scope_fails_loudly(self):
        """The usual cause is a typo, and a page with nothing in it looks like
        a graph with nothing in it."""
        nodes, edges = self.graph()
        with tempfile.TemporaryDirectory() as d:
            js = Path(d) / "m.js"
            js.write_text("x")
            r = subprocess.run(
                [sys.executable, str(TOOL), "--mermaid-js", str(js), "-o", "-",
                 "--only-ns", "acme/apo"],
                input=json.dumps({"nodes": nodes, "edges": edges}),
                capture_output=True, text=True)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("acme/api", r.stderr)   # names what is actually there
        self.assertIn("other/repo", r.stderr)

    def test_a_stub_is_never_a_fold_container(self):
        """The fold rule rests on the parent's label saying what the group is.
        A stub has no title, so folding into one hides N real nodes behind a box
        that cannot name them — five had collapsed into one on a real scoped
        page before this."""
        page = render(*self.graph(), args=["--only-ns", "acme/api"])
        fold = page.split("const FOLD = ", 1)[1].split("})();", 1)[0]
        # The whole guard, not just the call: `if (!isExternal(ps[0]))` is the
        # exact inversion this test exists to catch, and it contains the same
        # tokens. There is no JS engine here, so polarity has to be asserted.
        self.assertIn("if (isExternal(ps[0])) continue;", fold)

    def test_a_stub_renders_as_external_not_as_dropped(self):
        """`dropped` and `outside this page` mean opposite things about whether
        the reader should care, so they must not look the same."""
        page = render(*self.graph(), args=["--only-ns", "acme/api"])
        self.assertIn("classDef external", page)
        # External has to be tested *before* the search-context branch, or a
        # stub picked up as context loses the one thing worth saying about it.
        # Asserting the order is the point; either half alone passes on code
        # that puts them the wrong way round.
        external_first = page.index('const variant = n.external ? "external"')
        context_second = page.index(': narrowing && !matched.has(n.id) ? "context"')
        self.assertLess(external_first, context_second)

    def test_a_stub_only_namespace_is_marked_in_the_dropdown(self):
        """Otherwise it reads as a namespace this page holds, and selecting it
        yields titleless boxes — which looks like data loss, not a boundary."""
        page = render(*self.graph(), args=["--only-ns", "acme/api"])
        self.assertIn('const mark = real[ns] ? "" : "\\u2197 ";', page)


class TestProvenanceShapes(unittest.TestCase):
    """`weft export --include` makes this page the first consumer of a
    non-default export shape, so it has to read both — a page generated without
    the flag must not break (SPEC §9.3).

    These assert what reached the browser, via `page_data`, rather than that a
    line of JS is present in the template. The template is emitted whole on
    every render, so a string check over it passes for a page built from *no*
    data at all and cannot fail. Where a source check is the only thing
    available without a JS engine, it pins an exact expression so the inverted
    form fails, and says so.
    """

    A, B = "a" * 64, "b" * 64

    def edge(self, **kw):
        return dict({"kind": "depends-on", "from": self.A, "to": self.B}, **kw)

    def test_bare_string_refs_survive_unchanged(self):
        data = page_data(render([node("plain", refs=["gh:acme/api#12", "file:docs/x.md"])]))
        self.assertEqual(data["nodes"][0]["refs"], ["gh:acme/api#12", "file:docs/x.md"])

    def test_object_refs_carry_upstream_and_age(self):
        """The age is the load-bearing half: an observation shown as current
        invites a decision on data nobody re-checked."""
        data = page_data(render([node("rich", refs=[{
            "ref": "gh:acme/api#12",
            "observed_at": "2026-06-01T00:00:00Z",
            "upstream_state": "closed",
            "upstream_title": "fixed upstream",
        }])]))
        ref = data["nodes"][0]["refs"][0]
        self.assertEqual(ref["ref"], "gh:acme/api#12")
        self.assertEqual(ref["upstream_state"], "closed")
        self.assertEqual(ref["observed_at"], "2026-06-01T00:00:00Z")

    def test_every_ref_read_goes_through_the_normalizer(self):
        """Source-level, because there is no JS engine here — but pinned to the
        exact expressions, so removing the normalizer or reverting either read
        site fails. `haystack` is the one that bit: joining object refs directly
        matches the literal "[object Object]" and never the ref's own name."""
        page = render([node("x", refs=["gh:acme/api#12"])])
        self.assertIn('const refName = r => typeof r === "string" ? r : (r && r.ref) || "";', page)
        self.assertIn("(n.refs || []).map(refName)", page)
        self.assertIn("const name = refName(r), meta = refObj(r);", page)
        # The pre-normalizer spread is gone rather than merely supplemented.
        self.assertNotIn("...(n.refs || [])]", page)

    def test_claims_reach_the_page(self):
        """"Who is already on this" was invisible in the view that would make
        it obvious at a glance."""
        data = page_data(render([node("claimed", claims=[
            {"holder": "user:colleague", "by": "agent:me", "source": "inferred",
             "note": "per issue comment", "at": "2026-06-01T00:00:00Z"},
        ])]))
        claim = data["nodes"][0]["claims"][0]
        self.assertEqual(claim["holder"], "user:colleague")
        # by and holder differ on exactly the claim worth discounting.
        self.assertEqual(claim["by"], "agent:me")
        self.assertEqual(claim["source"], "inferred")

    def test_an_expired_claim_is_marked_not_hidden(self):
        """An expired claim is what abandoned work looks like, so hiding it
        makes abandoned indistinguishable from unclaimed (SPEC §4.3) — and
        showing it unmarked is the same failure the other way, reading as
        somebody currently on the job. `weft show` marks it; so must this."""
        data = page_data(render([node("stale", claims=[
            {"holder": "user:gone", "at": "2026-01-01T00:00:00Z",
             "expires": "2026-01-08T00:00:00Z"},
        ])]))
        # The field has to survive the boundary before anything can render it.
        self.assertEqual(data["nodes"][0]["claims"][0]["expires"], "2026-01-08T00:00:00Z")
        page = render([node("stale")])
        # Exact, so the inverted comparison — which has the same tokens — fails.
        self.assertIn("const expiredClaim = stamp => !!stamp && Date.parse(stamp) < Date.now();", page)
        self.assertIn("const gone = expiredClaim(c.expires);", page)
        # A claim with no expiry never ages out, so absence must read as live.
        self.assertIn("!!stamp &&", page)

    def test_an_inferred_edge_reaches_the_page_marked(self):
        """Two inferred edges drew identically to thirteen asserted ones. The
        README promises inferences are marked so you can tell."""
        data = page_data(render(
            [node("a", id=self.A), node("b", id=self.B)],
            [self.edge(source="inferred")],
        ))
        self.assertEqual(data["edges"][0]["source"], "inferred")
        page = render([node("a", id=self.A), node("b", id=self.B)],
                      [self.edge(source="inferred")])
        # Exact, so the inverted form fails: a marker on *asserted* edges would
        # satisfy any looser check for the same tokens.
        self.assertIn('e.source === "inferred" ? e.kind + "?" : e.kind', page)
        # The stroke is restated rather than merged, because a later linkStyle
        # replaces the whole declaration; omitting the colour would un-style
        # the edge kind instead of qualifying it.
        self.assertIn('lines.push(`  linkStyle ${i} ${s ? `stroke:${s},` : ""}', page)

    def test_a_default_page_carries_no_provenance_at_all(self):
        """Nothing may announce provenance the data does not carry, and the
        default export is the shape most pages are built from."""
        data = page_data(render(
            [node("plain", id=self.A, refs=["gh:acme/api#12"]), node("b", id=self.B)],
            [self.edge()],
        ))
        self.assertNotIn("source", data["edges"][0])
        self.assertNotIn("claims", data["nodes"][0])
        self.assertIsInstance(data["nodes"][0]["refs"][0], str)
        # The legend entry is gated on the data rather than always shown.
        page = render([node("a", id=self.A)], [])
        self.assertIn('if (!DATA.edges.some(e => e.source === "inferred")) return;', page)


class TestRedrawCost(unittest.TestCase):
    """mermaid's layout is the expensive part of a redraw and it is not
    cancellable: `renderSeq` discards a superseded result, but only after it has
    already blocked the main thread. Measured on a 137-node graph, typing an
    eight-character word into an undebounced box ran eight layouts, threw away
    seven, and delayed each keystroke by 424 ms. Nothing looks wrong on the page
    when this regresses — the drawing is still correct, just late — so it is
    pinned here rather than left to be noticed.
    """

    def q_handler(self, page):
        return page.split('getElementById("q").addEventListener', 1)[1].split("\n", 1)[0]

    def test_the_search_box_is_debounced(self):
        page = render([node("x")])
        self.assertIn("const drawSoon = debounce(draw, 150);", page)
        self.assertIn("drawSoon()", self.q_handler(page))

    def test_typing_answers_before_the_graph_redraws(self):
        """A debounce with no immediate feedback reads as a dropped keystroke.
        Selection is cheap enough to run on every one, so the counts line stays
        live while the drawing waits."""
        page = render([node("x")])
        self.assertIn("select()", self.q_handler(page))
        select_body = page.split("function select(", 1)[1].split("function buildDef", 1)[0]
        self.assertIn('getElementById("counts")', select_body)

    def test_discrete_controls_redraw_immediately(self):
        """These fire once per interaction, so a delay would only feel broken."""
        page = render([node("x")])
        self.assertIn('["showDropped", "showIsolated", "foldGroups", "nsFilter"].forEach', page)
        self.assertNotIn('"q", "showDropped"', page)

    def test_an_unchanged_definition_does_not_relayout(self):
        """`unconnected` is consulted only when nothing is narrowing, so toggling
        it during a search cannot change the drawing — and a relayout to produce
        the picture already on screen is the most expensive kind of no-op."""
        page = render([node("x")])
        self.assertIn("if (def === lastDef) return;", page)
        # A failed render leaves something else on screen, so the next attempt at
        # the same definition must not be skipped.
        self.assertIn("lastDef = null;", page)


class TestNegationFilter(unittest.TestCase):
    """On a large graph the useful question is usually "this, but not that", and
    a substring box cannot ask it. `!term` excludes; terms are space-separated
    and all must hold.
    """

    def test_a_negated_term_is_a_hard_exclusion(self):
        """It has to shrink the pool, not the match. Applied to the match, the
        context expansion would hand back the excluded nodes drawn faded — the
        one result someone typing `!test` cannot have meant."""
        page = render([node("x")])
        pool = page.split("const pool = DATA.nodes.filter", 1)[1].split(";", 1)[0]
        # Excluded alongside dropped, which is the other hard exclusion.
        self.assertIn("reject.some(", pool)
        self.assertIn('n.state !== "dropped"', pool)

    def test_a_lone_bang_does_nothing(self):
        """It is a prefix mid-typing. A box that blanks the graph on the
        keystroke before the useful one reads as broken."""
        page = render([node("x")])
        self.assertIn("if (!body) continue;", page_fn_decl(page, "parseQuery"))

    def test_terms_are_split_and_all_must_hold(self):
        page = render([node("x")])
        self.assertIn("want.every(", page)
        self.assertIn("reject.some(", page)

    def test_an_exclusion_alone_does_not_narrow(self):
        """Narrowing drives the faded-context expansion, which is for choosing a
        subject set. An exclusion does not choose one, so `!vm` shows the whole
        remaining graph rather than a slice of it."""
        page = render([node("x")])
        self.assertIn("const searching = want.length > 0;", page)

    def test_the_placeholder_advertises_it(self):
        """An operator nobody can discover is an operator nobody uses."""
        page = render([node("x")])
        self.assertIn("!term excludes", page)
        self.assertIn("touched<7d", page.split('id="q"', 1)[1].split(">", 1)[0])


NODE = shutil.which("node")


@unittest.skipUnless(NODE, "node not on PATH")
class TestTimeWords(unittest.TestCase):
    """`touched<7d`, `created>2026-10-01` and friends, run for real.

    String assertions cannot tell an inverted comparison from a correct one, and
    the two forms deliberately point opposite ways (an age `<` is newer, a date
    `<` is earlier), so the page's own functions are lifted out and executed in
    node against a fixed clock and fixed nodes. TZ is pinned because a date
    bound is local midnight.
    """

    NOW = "2026-11-02T12:00:00Z"
    NODES = {
        "fresh":   {"created": "2026-10-20T09:00:00Z", "touched": "2026-11-01T09:00:00Z"},
        "old":     {"created": "2026-06-01T09:00:00Z"},  # never touched since
        "sept":    {"created": "2026-09-15T09:00:00Z", "touched": "2026-09-30T23:59:00Z"},
        "oct1":    {"created": "2026-10-01T00:00:00Z"},
        "stub":    {"external": True},
    }

    def matches(self, query, now=None, nodes=None):
        now = now or self.NOW
        nodes = nodes if nodes is not None else self.NODES
        page = render([node("x")])
        src = "\n".join([
            "const haystack = n => n.title;",
            next(l for l in page.splitlines() if l.startswith("const TIME_WORD")),
            next(l for l in page.splitlines() if l.startswith("const UNIT_MS")),
            page_fn_decl(page, "timeBound") + "\n}",
            "const nodeTime = " + page.split("const nodeTime = ", 1)[1].split(";\n", 1)[0] + ";",
            page_fn_decl(page, "termMatcher") + "\n}",
            page_fn_decl(page, "parseQuery") + "\n}",
            f"Date.now = () => Date.parse({json.dumps(now)});",
            f"const nodes = {json.dumps([dict(v, title=k) for k, v in nodes.items()])};",
            "const DATA = { nodes };",
            f"const {{ want, reject, ignored }} = parseQuery({json.dumps(query)});",
            "console.log(JSON.stringify({",
            "  hit: nodes.filter(n => want.every(m => m(n)) && !reject.some(m => m(n))).map(n => n.title),",
            "  ignored }));",
        ])
        r = subprocess.run([NODE, "-e", src], capture_output=True, text=True,
                           env={"TZ": "UTC", "PATH": "/usr/bin:/bin"})
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)

    def hits(self, query, **kw):
        return sorted(self.matches(query, **kw)["hit"])

    def test_a_month_back_from_the_31st_is_late_february(self):
        """setMonth on 31 March into February overflows to 3 March, which would
        drop everything touched on 1-2 March."""
        nodes = {"mar1": {"created": "2026-03-01T00:00:00Z"},
                 "feb27": {"created": "2026-02-27T00:00:00Z"}}
        self.assertEqual(self.hits("touched<1m", now="2026-03-31T12:00:00Z", nodes=nodes), ["mar1"])

    def test_a_year_back_from_a_leap_day_is_28_february(self):
        nodes = {"mar1": {"created": "2027-03-01T00:00:00Z"}}
        self.assertEqual(self.hits("created<1y", now="2028-02-29T12:00:00Z", nodes=nodes), ["mar1"])

    def test_a_bound_outside_dates_range_is_ignored_not_empty(self):
        got = self.matches("touched<99999999y")
        self.assertEqual(got["ignored"], ["touched<99999999y"])
        self.assertEqual(sorted(got["hit"]), sorted(self.NODES))

    def test_text_that_only_looks_like_a_time_word_is_still_searchable(self):
        nodes = dict(self.NODES, **{"retry when created>limit": {"created": "2026-10-01T00:00:00Z"}})
        got = self.matches("created>limit", nodes=nodes)
        self.assertEqual(got, {"hit": ["retry when created>limit"], "ignored": []})

    def test_an_age_less_than_is_newer(self):
        self.assertEqual(self.hits("touched<7d"), ["fresh"])

    def test_an_age_greater_than_is_older(self):
        self.assertEqual(self.hits("touched>30d"), ["oct1", "old", "sept"])
        self.assertEqual(self.hits("touched>40d"), ["old"])

    def test_untouched_falls_back_to_created(self):
        """`old` has no touched field; its creation is its last activity."""
        self.assertIn("old", self.hits("touched>3m"))

    def test_a_date_less_than_is_before_the_day(self):
        self.assertEqual(self.hits("touched<2026-10-01"), ["old", "sept"])

    def test_a_date_greater_than_includes_the_day(self):
        """Half-open: midnight on the day itself is in `>`, not in `<`."""
        self.assertEqual(self.hits("created>2026-10-01"), ["fresh", "oct1"])

    def test_created_and_touched_are_different_clocks(self):
        self.assertEqual(self.hits("created<7d"), [])
        self.assertEqual(self.hits("touched<7d"), ["fresh"])

    def test_months_are_calendar_months(self):
        self.assertEqual(self.hits("created<2m"), ["fresh", "oct1", "sept"])

    def test_a_negated_time_word_excludes(self):
        self.assertEqual(self.hits("!touched<7d"), ["oct1", "old", "sept", "stub"])

    def test_a_stub_matches_no_time_word_either_way(self):
        for q in ("touched<100y", "touched>1h", "created<100y", "created>1h"):
            self.assertNotIn("stub", self.hits(q), q)

    def test_time_words_compose_with_text(self):
        self.assertEqual(self.hits("touched>30d sep"), ["sept"])

    def test_an_unreadable_word_contributes_nothing_and_is_reported(self):
        """Mid-typing `touched<` must not blank the graph, and a bad unit must
        not look as though it applied."""
        for q in ("touched<", "touched<7", "touched<7x", "created>2026-02-31", "touched>2026-10"):
            got = self.matches(q)
            self.assertEqual(sorted(got["hit"]), sorted(self.NODES), q)
            self.assertEqual(got["ignored"], [q], q)

    def test_the_counts_line_names_ignored_words(self):
        page = render([node("x")])
        self.assertIn("ignoring ${ignored.join(\" \")}", page_fn_decl(page, "select"))


class TestFoldIntoParent(unittest.TestCase):
    """A node whose only `part-of` out-edge is P and which nothing is `part-of`
    folds into P. Measured over the real store it removes 46% of the boxes, and
    the parent it folds into already exists, so this is rendering over exported
    data rather than a change to the model.
    """

    def test_the_rule_is_stated_in_one_line_and_needs_no_part_of_in_edges(self):
        """A grouping the tool cannot justify in one line is one nobody trusts —
        and the same clause is what makes nesting impossible, since a parent
        carries in-edges from the very children folding into it."""
        page = render([node("x")])
        rule = page.split("const FOLD = (() =>", 1)[1].split("})();", 1)[0]
        self.assertIn('e.kind !== "part-of"', rule)
        self.assertIn("ps.length !== 1 || contains.has(id)", rule)

    def test_candidacy_is_computed_over_the_whole_graph(self):
        """Derived from the visible set, a `part-of` edge a search filtered out
        would make the same node collapse or not depending on the search box."""
        page = render([node("x")])
        rule = page.split("const FOLD = (() =>", 1)[1].split("})();", 1)[0]
        self.assertIn("for (const e of DATA.edges)", rule)

    def test_a_match_inside_a_fold_cannot_vanish(self):
        """The failure is silent: the box count drops, the graph looks tidier,
        and results are missing. The parent counts as matched and carries how
        many of its folded children matched, rather than auto-unfolding — which
        would reshuffle the drawing on every keystroke."""
        page = render([node("x")])
        body = page.split("function applyFold(", 1)[1].split("\n}", 1)[0]
        self.assertIn("matched.add(p)", body)
        self.assertIn("hits.set(p", body)
        self.assertIn("matching", page)

    def test_matching_is_counted_only_for_a_positive_term(self):
        """A namespace filter narrows too, and under one every visible node
        matches by definition — so counting there put "1 matching" on every
        folded parent, which says nothing and teaches the reader to skip the
        annotation in the one case it matters."""
        page = render([node("x")])
        body = page.split("function applyFold(", 1)[1].split("\n}", 1)[0]
        self.assertIn("if (searching) hits.set(p", body)
        # The parent still counts as matched under any narrowing, or a match
        # inside a fold would fade out under a namespace filter.
        self.assertIn("if (narrowing && matched.has(c))", body)

    def test_the_fold_runs_after_the_isolated_node_filter(self):
        """A parent whose children carry no edges but the `part-of` ones they
        fold along has none left afterwards. Filtering second would drop it, and
        with it every child it stands for, while the reader had only asked to
        hide nodes with nothing to show."""
        page = render([node("x")])
        body = page.split("function select(", 1)[1].split("\nfunction ", 1)[0]
        self.assertLess(body.index('getElementById("showIsolated")'),
                        body.index("applyFold("),
                        "the fold must run after the isolated-node filter, not before")

    def test_repointing_drops_self_edges_and_dedupes(self):
        """Both artifacts a merge raises: an edge whose ends now both land on the
        parent says only that the parent contains itself, and two re-pointed
        edges can collapse onto one (kind, from, to)."""
        page = render([node("x")])
        body = page.split("function applyFold(", 1)[1].split("\n}", 1)[0]
        self.assertIn("if (from === to) continue;", body)
        self.assertIn("if (seen.has(key)) continue;", body)

    def test_a_child_never_folds_into_a_parent_the_filters_removed(self):
        """That would delete the child rather than group it."""
        page = render([node("x")])
        body = page.split("function applyFold(", 1)[1].split("\n}", 1)[0]
        self.assertIn("drawn.has(p)", body)

    def test_folded_nodes_get_their_own_word_in_the_counts(self):
        """They are neither shown nor hidden, so without one the arithmetic stops
        adding up and people stop trusting the line."""
        page = render([node("x")])
        line = page.split('getElementById("counts").textContent', 1)[1].split(";", 1)[0]
        self.assertIn("folded", line)
        self.assertIn("DATA.nodes.length - nodes.length - folds.folded", page)

    def test_a_folded_parent_says_so_on_its_label(self):
        """A fold that does not announce itself is a lie about how many boxes
        there are."""
        page = render([node("x")])
        self.assertIn("+${f} folded", page)

    def test_fold_state_and_its_exceptions_ride_in_the_fragment(self):
        """Fold-by-default with the exceptions in the URL keeps a link short and
        a collapsed view shareable."""
        page = render([node("x")])
        self.assertIn('p.set("fold", "0")', page)
        self.assertIn('p.set("unfold"', page)
        self.assertIn('p.get("fold") !== "0"', page)
        self.assertIn('p.get("unfold")', page)

    def test_an_unfold_exception_that_no_longer_resolves_is_dropped(self):
        """A stale link should degrade, not break (SPEC.md §9.3)."""
        page = render([node("x")])
        self.assertIn("if (hit.length === 1) UNFOLDED.add(hit[0].id);", page)

    def test_the_same_view_always_produces_the_same_link(self):
        page = render([node("x")])
        self.assertIn("[...UNFOLDED].map(short).sort().join(\",\")", page)

    def test_unfolding_is_a_button_not_a_click_on_the_box(self):
        """Clicking a box already means "show me this one", and one gesture
        cannot mean both."""
        page = render([node("x")])
        self.assertIn("button.fold-toggle", page)

    def test_the_detail_panel_lists_only_children_in_the_current_selection(self):
        """Global candidacy still names every child. Listing it under a filter
        would disagree with the "+N folded" label and offer an unfold that
        changes nothing when every child is already filtered out."""
        page = render([node("x")])
        body = page.split("function showDetail(", 1)[1].split("\nfunction ", 1)[0]
        self.assertIn("lastFolds.group.get", body)
        self.assertNotIn("FOLD.children", body)
        fold = page.split("function applyFold(", 1)[1].split("\nfunction ", 1)[0]
        self.assertIn("group.get(p).push", fold)
        # Eligible before the unfold exception, or an opened parent would leave
        # the panel empty while its children are on screen.
        self.assertLess(fold.index("group.get(p).push"),
                        fold.index("UNFOLDED.has(p)"),
                        "group must include children of an unfolded parent")

    def test_the_control_is_discoverable_and_on_by_default(self):
        page = render([node("x")])
        self.assertIn('id="foldGroups" checked', page)
        self.assertIn("grouped", page)


class TestPanAndWheelZoom(unittest.TestCase):
    """The pane is a canvas: drag or scroll to pan, pinch to zoom. Drag is
    mouse-only — touch already pans natively with momentum this cannot
    reproduce. Zoom is the modal one, and only ctrlKey qualifies.
    """

    def test_both_gestures_are_mouse_only(self):
        """Capturing the pointer on touch would replace native momentum
        scrolling with something worse."""
        page = render([node("x")])
        self.assertIn('e.pointerType !== "mouse"', page)

    def test_a_drag_needs_slop_before_it_becomes_a_pan(self):
        """Without it an unsteady hand turns selecting a node into a two-pixel
        pan, and the node never opens."""
        page = render([node("x")])
        self.assertIn("Math.hypot(dx, dy) < 4", page)

    def test_a_drag_that_ends_on_a_node_does_not_select_it(self):
        page = render([node("x")])
        self.assertIn("swallowClick = true", page)
        # Capture phase, or the per-node handlers draw() attaches run first.
        click = page.split('pane.addEventListener("click"', 1)[1].split("}, true);", 1)[0]
        self.assertIn("stopPropagation", click)

    def test_a_stranded_suppression_cannot_eat_a_later_click(self):
        """A drag ending off the pane never gets its click, so the flag is
        cleared when the next gesture starts rather than after it is used."""
        page = render([node("x")])
        down = page.split('pane.addEventListener("pointerdown"', 1)[1].split("});", 1)[0]
        self.assertIn("swallowClick = false", down)

    def test_a_gesture_with_no_button_down_is_over(self):
        """`drag` is otherwise cleared on pointerup, which only reliably arrives
        once the pane has captured the pointer — and capture is taken after the
        slop threshold. A press that never clears the threshold and lifts
        elsewhere therefore stranded `drag`, and the next move over the pane
        panned with nothing held."""
        page = render([node("x")])
        move = page.split('pane.addEventListener("pointermove"', 1)[1].split("});", 1)[0]
        self.assertIn("if (!e.buttons)", move)
        # And it has to come before the slop test, or the stranded press still
        # clears the threshold in one jump on its way out.
        self.assertLess(move.index("e.buttons"), move.index("Math.hypot"),
                        "the button check must precede the slop threshold")

    def test_the_fit_measurement_is_not_clamped(self):
        """It measures what is on screen rather than naming a level to move to.
        A big graph can fit below the slider's floor, and reporting the floor
        would make zoomAt compute its ratio against a scale nobody is looking
        at, slipping the anchor on the first notch out of fit."""
        page = render([node("x")])
        body = page.split("function currentZoomPct(", 1)[1].split("\n}", 1)[0]
        self.assertNotIn("clampZoom(", body)
        self.assertIn("Math.max(1,", body)
        # The target still is clamped — that is the value the controls must show.
        self.assertIn("const to = clampZoom(", page)

    def test_only_a_pinch_zooms_and_a_plain_wheel_is_left_alone(self):
        """A WheelEvent carries no device information — no `pointerType`, unlike
        the pointer events the drag handler uses — so a mouse and a trackpad are
        indistinguishable. Zooming on any wheel meant a two-finger scroll zoomed
        while its sideways component fell through and panned: on a tall graph,
        the gesture for scrolling down zoomed out. ctrlKey is the gesture the
        browser *does* report, and it serves both hardware types."""
        page = render([node("x")])
        body = page.split('pane.addEventListener("wheel"', 1)[1].split("}, { passive", 1)[0]
        self.assertIn("if ((!e.ctrlKey && !e.metaKey) || !e.deltaY) return;", body)
        # The early return must precede preventDefault, or a plain scroll is
        # swallowed and the pane stops panning natively.
        self.assertLess(body.index("e.ctrlKey"), body.index("preventDefault"))

    def test_the_modal_gesture_is_discoverable(self):
        """A row of percentages does not tell you that scroll pans and pinch
        zooms. The legend carries it as visible text rather than relying on the
        title attribute alone: a tooltip on a non-focusable element reaches
        neither a keyboard nor a screen reader."""
        page = render([node("x")])
        legend = page.split('<div class="legend">', 1)[1].split("</div>", 1)[0]
        self.assertIn("pan", legend)
        # The modifier is the part no gesture reveals — a pinch you may stumble
        # into, ctrl+wheel you will not.
        self.assertIn("ctrl/cmd+wheel to zoom", legend)
        # The hover hint stays too, on the control it describes.
        self.assertIn("Pinch, or ctrl/cmd+wheel, to zoom. Scroll or drag to pan.", page)


    def test_command_zooms_too_but_shift_is_left_to_the_browser(self):
        """Command is the modifier a mac reader reaches for. Shift is not here on
        purpose: browsers already turn shift+wheel into horizontal scroll, which
        is the sideways pan, so claiming it for zoom would remove it."""
        page = render([node("x")])
        body = page.split('pane.addEventListener("wheel"', 1)[1].split("}, { passive", 1)[0]
        self.assertIn("e.metaKey", body)
        self.assertNotIn("shiftKey", body)


class TestPinchToZoom(unittest.TestCase):
    """Two fingers zoom, one finger is left entirely alone. The split is what
    keeps native scrolling — and its momentum — intact on a phone."""

    def test_the_browser_is_stopped_from_claiming_the_pinch(self):
        """Without touch-action the handlers still fire and the page zooms as
        well, which reads as the graph fighting the gesture. pan-x pan-y claims
        only the pinch and leaves one-finger scrolling native."""
        page = render([node("x")])
        base = page.split("#graph{flex:1", 1)[1].split("}", 1)[0]
        self.assertIn("touch-action:pan-x pan-y", base)

    def test_one_finger_is_never_intercepted(self):
        """Native scrolling has momentum this cannot reproduce, so every touch
        handler bails unless exactly two fingers are down."""
        page = render([node("x")])
        for ev in ("touchstart", "touchmove"):
            body = page.split(f'pane.addEventListener("{ev}"', 1)[1].split("}, { passive", 1)[0]
            self.assertIn("twoFingers(e.touches)", body)
        # And preventDefault is reached only after that guard, or a one-finger
        # scroll would be swallowed.
        move = page.split('pane.addEventListener("touchmove"', 1)[1].split("}, { passive", 1)[0]
        self.assertLess(move.index("twoFingers"), move.index("preventDefault"))

    def test_scale_is_measured_from_the_start_of_the_gesture(self):
        """Accumulating frame to frame compounds its own rounding, so fingers
        returned to where they began leave the graph at a different zoom than it
        started. A pinch that will not undo itself is the thing that feels
        broken."""
        page = render([node("x")])
        move = page.split('pane.addEventListener("touchmove"', 1)[1].split("}, { passive", 1)[0]
        self.assertIn("pinch.from * gap / pinch.gap", move)

    def test_a_zero_gap_cannot_divide(self):
        """Two touches at one point have no gap to scale against, and dividing
        by it sends the zoom to infinity on the first move."""
        page = render([node("x")])
        start = page.split('pane.addEventListener("touchstart"', 1)[1].split("}, { passive", 1)[0]
        self.assertIn("if (!gap) return;", start)

    def test_the_pinch_is_anchored_between_the_fingers(self):
        """The same contract the wheel has with the cursor: what you pinch about
        stays where it was."""
        page = render([node("x")])
        move = page.split('pane.addEventListener("touchmove"', 1)[1].split("}, { passive", 1)[0]
        self.assertIn("fingerMid(e.touches)", move)
        self.assertIn("zoomAt(to, mx, my)", move)

    def test_lifting_a_finger_ends_the_gesture(self):
        """Rather than reinterpreting one remaining finger as a new pinch, which
        would fight the native scroll it should be handing back to."""
        page = render([node("x")])
        self.assertIn('for (const ev of ["touchend", "touchcancel"])', page)
        self.assertIn("if (!twoFingers(e.touches)) pinch = null;", page)

    def test_the_wheel_zooms_multiplicatively_and_within_the_slider(self):
        """Additive steps crawl when zoomed out and lurch when zoomed in. The
        bounds are the slider's own, so the wheel cannot reach a level the
        controls cannot express."""
        page = render([node("x")])
        self.assertIn("Math.exp(", page)
        self.assertIn("const ZOOM_MIN = +zoomRange.min, ZOOM_MAX = +zoomRange.max;", page)

    def test_the_wheel_normalises_delta_mode(self):
        """deltaY is pixels, lines or pages depending on the device."""
        page = render([node("x")])
        self.assertIn("WHEEL_UNIT[e.deltaMode]", page)

    def test_zoom_is_anchored_at_the_cursor(self):
        """About the pane's origin instead, the label you leaned in to read
        slides off the edge."""
        page = render([node("x")])
        body = page.split("function zoomAt(", 1)[1].split("\n}", 1)[0]
        self.assertIn("g.scrollLeft = cx * ratio", body)
        self.assertIn("g.scrollTop = cy * ratio", body)

    def test_leaving_fit_starts_from_what_fit_produced(self):
        """fit is a bound, not a multiple, so the slider's value is not the
        scale on screen — starting from it jumps to 100% on the first notch."""
        page = render([node("x")])
        body = page.split("function currentZoomPct(", 1)[1].split("\n}", 1)[0]
        self.assertIn('classList.contains("fit")', body)
        self.assertIn("getBoundingClientRect().width / inner", body)

    def test_the_scale_is_read_from_the_render_not_from_the_slider(self):
        """zoomRange has step="5", so assigning it snaps: the wheel asks for
        133%, the control records 135%, the drawing renders 133%. Reading the
        control back made every following ratio wrong by the snap, and since
        zoomAt turns that ratio into a scroll offset, the anchor slid further off
        on each event of a burst — felt as the graph scrolling vertically while
        it zoomed. Measured on the rendered page: 3.11% horizontal and 1.50%
        vertical drift over one six-event burst, against 0.11% and 0.03% once
        --zoom became the source."""
        page = render([node("x")])
        body = page.split("function currentZoomPct(", 1)[1].split("\n}", 1)[0]
        self.assertIn('getPropertyValue("--zoom")', body)
        # The slider survives only as the fallback for "nothing has zoomed yet",
        # never as the record of what is on screen.
        self.assertNotIn('if (!g.classList.contains("fit")) return +zoomRange.value;', body)

    def test_the_zoom_slider_still_snaps_because_it_is_a_control(self):
        """The snap is not the bug and is not being removed — a dragged slider
        wants detents. The bug was reading it back as the truth."""
        page = render([node("x")])
        self.assertIn('id="zoomRange"', page)
        self.assertIn('step="5"', page)

    def test_a_continuous_gesture_writes_the_url_once_it_settles(self):
        """On file:// syncURL assigns location.hash, which makes a history entry
        each time — an undeferred wheel buries the back button."""
        page = render([node("x")])
        self.assertIn("const syncURLSoon = debounce(syncURL, 250);", page)
        self.assertIn("if (quiet) syncURLSoon(); else syncURL();", page)

    def test_the_cursor_says_the_pane_is_draggable(self):
        page = render([node("x")])
        self.assertIn("cursor:grab", page)
        self.assertIn("#graph.panning{cursor:grabbing;user-select:none;}", page)


@unittest.skipUnless(NODE, "node not on PATH")
class TestNamespaceColour(unittest.TestCase):
    """Colour by namespace, run for real: the depth adapts to what is drawn, and
    the colour follows the group rather than its rank in the current view."""

    STORE = (["nooga/let-go/frontier"] * 6 + ["nooga/let-go/mining/prs"] * 4 +
             ["nooga/let-go/ir"] * 3 + ["nooga/let-go/vm"] * 2 + ["nooga/let-go/aot"] * 1 +
             ["nooga/let-go"] * 1 + ["lanternfish"] * 5 + ["mparrett/weft"] * 2 + [""] * 1)

    def groups(self, drawn_ns):
        page = render([node("x")])
        src = "\n".join([
            next(l for l in page.splitlines() if l.startswith("const NS_SLOTS")),
            next(l for l in page.splitlines() if l.startswith("const nsSegs")),
            page_fn_decl(page, "nsGroups") + "\n}",
            page_fn_decl(page, "mixHex") + "\n}",
            f"const DATA = {{ nodes: {json.dumps([{'ns': ns} for ns in self.STORE])} }};",
            f"const drawn = {json.dumps([{'ns': ns} for ns in drawn_ns])};",
            "const g = nsGroups(drawn);",
            "console.log(JSON.stringify({ base: g.base, rows: drawn.map(n => {",
            "  const k = g.key(n); return [n.ns, g.label(k), g.slot(k)]; }),",
            "  mix: mixHex('#ff0000', '#0000ff', .25) }));",
        ])
        r = subprocess.run([NODE, "-e", src], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)

    def test_the_whole_store_colours_by_owner(self):
        got = self.groups(self.STORE)
        self.assertEqual(got["base"], "")
        rows = {ns: (label, slot) for ns, label, slot in got["rows"]}
        self.assertEqual(rows["nooga/let-go/ir"], ("nooga", 0))      # 17 nodes: largest
        self.assertEqual(rows["lanternfish"], ("lanternfish", 1))
        self.assertEqual(rows["mparrett/weft"], ("mparrett", 2))
        self.assertEqual(rows[""], ("(no namespace)", 3))

    def test_one_projects_nodes_colour_by_subspace(self):
        drawn = [ns for ns in self.STORE if ns.startswith("nooga/let-go")]
        got = self.groups(drawn)
        self.assertEqual(got["base"], "nooga/let-go")
        rows = {ns: (label, slot) for ns, label, slot in got["rows"]}
        self.assertEqual(rows["nooga/let-go/frontier"], ("frontier", 0))
        self.assertEqual(rows["nooga/let-go/mining/prs"], ("mining", 1))
        # Past four, grey: aot (1) and the prefix's own node (1) rank last.
        self.assertEqual(rows["nooga/let-go/aot"][1], "x")
        self.assertEqual(rows["nooga/let-go"][0], "let-go")

    def test_a_filter_at_the_same_depth_does_not_repaint(self):
        """Ranks come from the whole page under the prefix, so drawing only the
        smaller groups leaves their colours where they were."""
        drawn = [ns for ns in self.STORE if ns.startswith("nooga/let-go")]
        full = {ns: slot for ns, _, slot in self.groups(drawn)["rows"]}
        some = ["nooga/let-go/ir", "nooga/let-go/vm", "nooga/let-go/frontier"]
        part = {ns: slot for ns, _, slot in self.groups(some)["rows"]}
        for ns in some:
            self.assertEqual(part[ns], full[ns], ns)

    def test_small_groups_borrow_hues_nothing_drawn_is_using(self):
        """Drawing only groups ranked past four would otherwise be all grey.
        ir keeps its own slot; the two small groups take free ones in rank
        order (ties by name), and nothing stays grey while a hue is unused."""
        got = {ns: slot for ns, _, slot in
               self.groups(["nooga/let-go/ir", "nooga/let-go/aot", "nooga/let-go"])["rows"]}
        self.assertEqual(got, {"nooga/let-go/ir": 2, "nooga/let-go": 0, "nooga/let-go/aot": 1})

    def test_one_namespace_is_one_group(self):
        got = self.groups(["mparrett/weft", "mparrett/weft"])
        self.assertEqual(got["rows"][0][1:], ["weft", 0])

    def test_mix_blends_by_share(self):
        self.assertEqual(self.groups(["x"])["mix"], "#4000bf")


class TestNamespaceColourWiring(unittest.TestCase):
    def test_the_mode_rides_in_the_link_only_when_chosen(self):
        page = render([node("x")])
        self.assertIn('if (seg("colour") !== "state") p.set("colour", seg("colour"));',
                      page_fn_decl(page, "syncURL"))
        restore = page_fn_decl(page, "applyURLState")
        self.assertIn('const colour = p.get("colour") || p.get("color");', restore)
        self.assertIn('["ns", "age", "touched"].includes(colour) ? colour : "state"', restore)

    def test_the_group_key_survives_the_phone_layout(self):
        self.assertIn(".legend > .ns-groups{display:inline-flex;}", render([node("x")]))

    def test_identity_is_never_colour_alone(self):
        """The group is named on every box in namespace mode."""
        self.assertIn('byNs.label(k), 24) + " \\u00b7 " + short(n.id)', page_fn_decl(render([node("x")]), "buildDef"))


@unittest.skipUnless(NODE, "node not on PATH")
class TestTimeColour(unittest.TestCase):
    """Colour by age and by touched, run for real against a fixed clock: the
    bucket edges, the per-box time, and touched falling back to created."""

    NOW = "2026-11-02T12:00:00Z"
    NODES = {
        "hours":   {"created": "2026-11-02T10:00:00Z"},
        "days":    {"created": "2026-10-30T12:00:00Z"},
        "week":    {"created": "2026-10-26T12:00:00Z"},  # exactly 7d: the next bucket
        "months":  {"created": "2026-09-01T12:00:00Z", "touched": "2026-11-02T11:00:00Z"},
        "ancient": {"created": "2026-01-01T12:00:00Z"},
        "stub":    {"external": True},
    }

    def rows(self, clock):
        page = render([node("x")])
        src = "\n".join([
            page_fn_decl(page, "age") + "\n}",
            "const nodeTime = " + page.split("const nodeTime = ", 1)[1].split(";\n", 1)[0] + ";",
            next(l for l in page.splitlines() if l.startswith("const TIME_BUCKETS")),
            page_fn_decl(page, "timeGroups") + "\n}",
            f"Date.now = () => Date.parse({json.dumps(self.NOW)});",
            f"const nodes = {json.dumps([dict(v, title=k) for k, v in self.NODES.items()])};",
            f"const g = timeGroups({json.dumps(clock)});",
            "console.log(JSON.stringify(Object.fromEntries(nodes.map(n => {",
            "  const k = g.key(n); return [n.title, [g.label(k), g.slot(k), g.boxLabel(n)]]; }))));",
        ])
        r = subprocess.run([NODE, "-e", src], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)

    def test_age_buckets_by_creation(self):
        got = self.rows("created")
        self.assertEqual(got["hours"], ["<1d", 0, "2h"])
        self.assertEqual(got["days"], ["1\u20137d", 1, "3d"])
        self.assertEqual(got["week"], ["7\u201330d", 2, "7d"])
        self.assertEqual(got["months"], ["30\u201390d", 3, "62d"])
        self.assertEqual(got["ancient"], ["90d+", 4, "305d"])

    def test_touched_falls_back_to_created(self):
        got = self.rows("touched")
        self.assertEqual(got["months"], ["<1d", 0, "1h"])
        self.assertEqual(got["days"], ["1\u20137d", 1, "3d"])  # never touched since

    def test_a_stub_has_no_date(self):
        for clock in ("created", "touched"):
            self.assertEqual(self.rows(clock)["stub"], ["no date", "x", "no date"])


class TestLegendHelp(unittest.TestCase):
    """The ? panel is built from the legend's own spans, so the one way it can
    rot is an entry added without its description. That is what these pin."""

    def legend(self, page):
        return page.split('<div class="legend">', 1)[1].split("\n</div>", 1)[0]

    def test_every_mark_in_the_legend_carries_its_description(self):
        legend = self.legend(render([node("x")]))
        spans = re.findall(r"<span[^>]*>.*?</span>", legend, re.S)
        marks = [s for s in spans if 'class="swatch' in s or 'class="rule' in s]
        self.assertGreaterEqual(len(marks), 8, "the legend parse found too few entries")
        for s in marks:
            self.assertRegex(s, r'data-help="[^"]{20,}"', f"legend entry without help: {s[:80]}")

    def test_the_toggle_is_a_button_the_phone_rule_does_not_hide(self):
        """The phone layout hides `.legend > span`; the panel is how a phone
        reader learns the marks at all, so its toggle must not be a span."""
        page = render([node("x")])
        self.assertIn('<button type="button" class="help-toggle" id="helpToggle" aria-expanded="false"',
                      self.legend(page))
        self.assertIn(".legend > span{display:none;}", page)

    def test_closed_by_default_and_carried_in_the_link(self):
        page = render([node("x")])
        self.assertIn('<div class="legend-help" id="legendHelp" hidden></div>', page)
        sync = page.split("function syncURL()", 1)[1].split("\n}", 1)[0]
        self.assertIn('p.set("help", "1")', sync)
        apply = page.split("function applyURLState()", 1)[1].split("\n}", 1)[0]
        self.assertIn('setHelp(p.has("help")', apply)

    def test_one_column_fits_a_phone(self):
        """A bare 380px column minimum is wider than a 390px screen less its
        padding, which scrolled the page sideways."""
        self.assertIn("minmax(min(380px,100%),1fr)", render([node("x")]))

    def test_the_pages_own_fragment_write_is_not_reapplied(self):
        """On file:// syncURL assigns location.hash, which fires hashchange;
        re-applying a state the page is already in re-initialised mermaid on
        every control change. The skip is one-shot, so back-then-forward to
        that fragment still applies."""
        page = render([node("x")])
        handler = page.split('addEventListener("hashchange", () => {', 1)[1].split("\n});", 1)[0]
        self.assertIn("location.hash === selfWrittenHash", handler)
        self.assertIn("selfWrittenHash = null;", handler)
        sync = page_fn_decl(page, "syncURL")
        self.assertIn("selfWrittenHash = location.hash;", sync)

    def test_hidden_entries_are_not_explained(self):
        """An unscoped page has no stubs; explaining them is noise."""
        self.assertIn(".filter(s => !s.hidden)", page_fn_decl(render([node("x")]), "renderHelp"))


class TestURLState(unittest.TestCase):
    """One artifact serves every scope. The export is whole-store regardless — an
    edge may cross namespaces — so a second page could only ever differ by which
    control it opened on, and would then drift out of date on its own schedule.
    """

    def test_state_rides_in_the_fragment_not_the_query(self):
        """These pages are opened over file:// as often as over http, and
        history.replaceState throws on a null origin."""
        page = render([node("x")])
        self.assertIn("location.hash", page)
        self.assertIn("history.replaceState", page)
        # The throw is caught, or the whole handler dies on file://.
        sync = page.split("function syncURL()", 1)[1].split("\n}", 1)[0]
        self.assertIn("catch", sync)

    def test_a_hand_written_query_string_still_works(self):
        page = render([node("x")])
        self.assertIn("location.search", page)

    def test_display_mode_is_written_only_when_chosen(self):
        """Writing it unconditionally puts theme=light in every link and
        overrides the recipient's own OS setting, which is the reader's call."""
        page = render([node("x")])
        sync = page.split("function syncURL()", 1)[1].split("\n}", 1)[0]
        self.assertIn('seg("theme") !== "auto"', sync)
        self.assertIn('z !== "100"', sync)

    def test_every_control_is_derived_from_the_fragment(self):
        """Applying only the keys that are present looks equivalent and is not:
        navigating away from a link that forced a theme found nothing to apply,
        so the forced theme stayed and the reader never got their own back."""
        page = render([node("x")])
        body = page.split("function applyURLState()", 1)[1].split("\n}", 1)[0]
        self.assertNotIn("return;", body)  # no "leave it alone" early exit
        self.assertIn('removeAttribute("data-theme")', body)
        self.assertIn('press("dir", p.get("dir") || "LR")', body)

    def test_a_stale_scope_degrades_rather_than_breaking(self):
        """A link outliving the namespace it names should land on "all", not
        error — tolerance over validation (SPEC.md §9.3)."""
        page = render([node("x")])
        self.assertIn("function press(", page)
        press = page.split("function press(", 1)[1].split("\n}", 1)[0]
        self.assertIn("some(b => b.dataset.v === value)", press)

    def test_the_url_records_views_the_drawing_cannot_show(self):
        """syncURL runs before draw's unchanged-definition check: toggling a
        control that leaves the picture alone still changed the view."""
        page = render([node("x")])
        body = page.split("async function draw()", 1)[1]
        self.assertLess(body.index("syncURL()"), body.index("if (def === lastDef)"))

    def test_the_tab_title_names_the_active_scope(self):
        """A heading naming one workstream while showing another is the exact
        confusion this addresses."""
        page = render([node("x")])
        self.assertIn("BASE_TITLE", page)
        self.assertIn("document.title = ns ?", page)


class TestNarrowScreen(unittest.TestCase):
    """Measured on a 390x664 portrait viewport, the desktop layout gave the
    chrome 350px — 53% of the screen — left the graph 231px, and put the detail
    panel below the fold so tapping a node looked like it had done nothing.
    """

    def narrow_css(self, page):
        return page.split("@media (max-width: 900px){", 1)[1].split("\n  }", 1)[0]

    def test_the_gesture_hint_survives_where_the_rest_of_the_legend_does_not(self):
        """The legend is hidden here because it is explanatory and cost two rows.
        The gesture line is the exception, and became one when pinch shipped:
        there is no hover and no title attribute on a phone, and two-finger zoom
        is not guessable from a row of percentages. The touch wording must also
        not name ctrl or the wheel, neither of which a phone reader has."""
        page = render([node("x")])
        legend = page.split('<div class="legend">', 1)[1].split("</div>", 1)[0]
        self.assertIn("two fingers to zoom", legend)
        touch = legend.split('class="touch-gestures"', 1)[1]
        self.assertNotIn("ctrl", touch)
        self.assertNotIn("wheel", touch)
        # Hidden by default; the narrow rule swaps which of the two is shown.
        self.assertIn(".legend .touch-gestures{display:none;}", page)
        narrow = self.narrow_css(page)
        self.assertIn(".legend > span{display:none;}", narrow)
        self.assertIn(".legend > .touch-gestures{display:inline-flex;}", narrow)

    def test_secondary_controls_collapse_but_search_scope_and_zoom_do_not(self):
        """Zoom stays out because on a phone it stands in for a pinch."""
        page = render([node("x")])
        more = page.split('<div class="more" id="moreControls">', 1)[1].split("</header>", 1)[0]
        for folded in ['id="dir"', 'id="theme"', 'id="showDropped"', 'id="showIsolated"']:
            self.assertIn(folded, more)
        for kept in ['id="q"', 'id="nsFilter"', 'id="zoom"']:
            self.assertNotIn(kept, more)

    def test_the_detail_panel_becomes_a_bottom_sheet(self):
        css = self.narrow_css(render([node("x")]))
        self.assertIn("position:fixed", css)
        # The desktop rule anchors the panel with top:0. Left standing, bottom:0
        # stretches the sheet and it covers the graph from the wrong edge.
        self.assertIn("top:auto", css)
        self.assertIn("body.has-detail aside{transform:translateY(0);}", css)

    def test_selecting_a_node_summons_the_sheet(self):
        page = render([node("x")])
        self.assertIn('classList.toggle("has-detail", !!n)', page)

    def test_the_sheet_can_be_dismissed(self):
        """It covers the graph it describes, so there has to be a way back."""
        page = render([node("x")])
        self.assertIn('id="detailClose"', page)
        self.assertIn('classList.remove("has-detail")', page)

    def test_a_phone_opens_zoomed_enough_to_read(self):
        """Fitting the graph to a 390px column makes every label too small to be
        worth drawing. It is the fallback arm, so an explicit zoom in a link
        still wins."""
        page = render([node("x")])
        self.assertIn("const fallback = narrow() ? 200 : 100;", page)

    def test_the_graph_pane_is_bounded_so_it_can_pan(self):
        """Unbounded, the container's height always equalled its content, so it
        scrolled only sideways and a drag went to the page instead — the graph
        looked cut off at the right with no way to reach it. flex:none matters
        as much as the height: main is a column here, so flex-basis governs the
        vertical axis and height is ignored while flex:1 stands."""
        page = render([node("x")])
        # The bound is on the base rule now, so it holds at every width — the
        # wheel means zoom, and a wheel over a pane that cannot scroll itself
        # goes to the page instead.
        base = page.split("#graph{flex:1", 1)[1].split("}", 1)[0]
        self.assertIn("height:var(--pane-h)", base)
        self.assertIn("overscroll-behavior:contain", base)
        # flex:none is still phone-specific: main is a column there, so
        # flex-basis governs the vertical axis and height alone is ignored.
        self.assertIn("flex:none", self.narrow_css(page))

    def test_turning_the_column_also_turns_align_items(self):
        """The desktop rule sets align-items:flex-start so the sticky detail
        panel does not stretch to the graph's height. Flip flex-direction and
        the cross axis becomes width, so the same declaration shrink-wraps the
        pane to its content — 320px of a 390px phone, the rest dead space to the
        right of the scrollbar. Invisible in review because it is a rule about
        the axis nobody was thinking about."""
        css = self.narrow_css(render([node("x")]))
        self.assertIn("main{flex-direction:column;align-items:stretch;}", css)

    def test_the_pane_is_measured_from_its_own_top_not_by_summing_the_chrome(self):
        """Summing the parts needs a constant for whatever is left over, and a
        constant is right on exactly one screen: the 36 that used to stand here
        was the desktop pane's padding, so at narrow widths — padding 10 — it
        left 36px of screen unused below the graph."""
        page = render([node("x")])
        body = page.split("function measureChrome(", 1)[1].split("\n}", 1)[0]
        self.assertIn("getBoundingClientRect().top", body)
        self.assertNotIn("+ 36", body)
        # Two quantities, not one asked to be both: the pane is border-box, so
        # its height includes the padding that the drawing inside it does not get.
        self.assertIn("--pane-h", body)
        self.assertIn("--fit-h", body)
        self.assertIn("paddingTop", body)

    def test_unfolding_the_options_remeasures_the_pane(self):
        """The pane is sized from what the chrome leaves, so a taller header has
        to shrink it or the page starts scrolling behind it."""
        page = render([node("x")])
        body = page.split("function setMore(", 1)[1].split("\n}", 1)[0]
        self.assertIn("measureChrome()", body)

    def test_the_legend_and_slider_give_up_their_space(self):
        """The legend gave up all of it until pinch shipped. It now keeps one
        line — the gesture hint — because that is the only thing here a phone
        cannot get another way; the swatches and the edge-kind rules are all
        repeated in the detail sheet. Everything else still goes."""
        css = self.narrow_css(render([node("x")]))
        self.assertIn(".legend > span{display:none;}", css)
        self.assertIn(".legend > .touch-gestures{display:inline-flex;}", css)
        self.assertIn("input[type=range]", css)

    def test_the_phone_affordances_are_absent_on_a_wide_screen(self):
        page = render([node("x")])
        self.assertIn(".more-toggle,.sheet-close{display:none;}", page)
        self.assertIn(".more{display:contents;}", page)


def page_fn(page, name):
    """The source text of a top-level `const <name> = ...` arrow function."""
    start = page.index("const " + name + " = ")
    return page[start:page.index(";\n", start)]


if __name__ == "__main__":
    unittest.main(verbosity=2)
