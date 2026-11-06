#!/usr/bin/env python3
"""Tests for weft-range-context.py.

Covers the parts that fail silently: number extraction from squash-merge
subjects, remote parsing, and markdown escaping of free-text titles. The
git-and-graph join is exercised by running the tool against a scratch repo.

    python3 tools/test_weft_range_context.py
"""

import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("wrc", HERE / "weft-range-context.py")
wrc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wrc)


class TestNumberExtraction(unittest.TestCase):
    def test_squash_subject_yields_every_number(self):
        # `(#554) (#653)` is issues-then-PR; neither can be told apart without
        # asking GitHub, so both must survive to be resolved against the graph.
        found = wrc.REF_IN_MESSAGE.findall("fix(aot): boxed override (#554) (#653)")
        self.assertEqual(found, ["554", "653"])

    def test_ignores_things_that_are_not_refs(self):
        for s in ["no refs here", "colour #fff", "issue # 12", "v1.12.2"]:
            self.assertEqual(wrc.REF_IN_MESSAGE.findall(s), [], s)

    def test_hex_colour_does_not_yield_a_number(self):
        # `#` must be followed immediately by digits, so a hex colour is not a
        # ref. A number *after* letters is not one either.
        self.assertEqual(wrc.REF_IN_MESSAGE.findall("#abc123"), [])
        self.assertEqual(wrc.REF_IN_MESSAGE.findall("see #12 and #34"), ["12", "34"])


class TestRepoSlug(unittest.TestCase):
    def slug(self, url):
        with tempfile.TemporaryDirectory() as d:
            subprocess.run(["git", "init", "-q"], cwd=d, check=True)
            subprocess.run(["git", "remote", "add", "origin", url], cwd=d, check=True)
            return wrc.repo_slug(d)

    def test_https_and_ssh_and_bare_agree(self):
        for url in ["https://github.com/mparrett/weft.git",
                    "https://github.com/mparrett/weft",
                    "git@github.com:mparrett/weft.git"]:
            self.assertEqual(self.slug(url), "mparrett/weft", url)


class TestMarkdownEscaping(unittest.TestCase):
    def test_title_punctuation_cannot_become_markup(self):
        got = wrc.md_escape("use *args* and _kwargs_ with `code` and [link]")
        for ch in "*_`[]":
            self.assertNotIn(ch + ch, got)
        self.assertIn(r"\*args\*", got)

    def test_plain_title_is_unchanged(self):
        self.assertEqual(wrc.md_escape("a normal title"), "a normal title")


class TestEndToEnd(unittest.TestCase):
    def test_a_range_with_no_refs_says_so_and_exits_clean(self):
        with tempfile.TemporaryDirectory() as d:
            for cmd in (["git", "init", "-q"],
                        ["git", "remote", "add", "origin", "https://github.com/a/b.git"],
                        ["git", "-c", "user.email=t@t", "-c", "user.name=t",
                         "commit", "-q", "--allow-empty", "-m", "first"],
                        ["git", "-c", "user.email=t@t", "-c", "user.name=t",
                         "commit", "-q", "--allow-empty", "-m", "second, no refs"]):
                subprocess.run(cmd, cwd=d, check=True)
            r = subprocess.run([sys.executable, str(HERE / "weft-range-context.py"),
                                "-C", d, "HEAD~1", "HEAD"],
                               capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("No issue or PR numbers", r.stdout)


class TestBinaryResolution(unittest.TestCase):
    """The tool shells out to `weft export --json`, and naming only the PATH
    entry meant it worked on a machine that had run `make install` and nowhere
    else. CI is the first thing that ran it clean, and it did so by traceback.
    """

    def repo(self, d, subject):
        for cmd in (["git", "init", "-q"],
                    ["git", "remote", "add", "origin", "https://github.com/a/b.git"],
                    ["git", "-c", "user.email=t@t", "-c", "user.name=t",
                     "commit", "-q", "--allow-empty", "-m", "first"],
                    ["git", "-c", "user.email=t@t", "-c", "user.name=t",
                     "commit", "-q", "--allow-empty", "-m", subject]):
            subprocess.run(cmd, cwd=d, check=True)

    def run_tool(self, d, env_extra):
        env = dict(os.environ, **env_extra)
        return subprocess.run([sys.executable, str(HERE / "weft-range-context.py"),
                               "-C", d, "HEAD~1", "HEAD"],
                              capture_output=True, text=True, env=env)

    def test_a_range_with_no_refs_never_needs_a_binary(self):
        """Nothing to look up means nothing to load. Folding the store to answer
        a question with no subject was waste, and it made a range mentioning no
        issues depend on having weft installed at all."""
        with tempfile.TemporaryDirectory() as d:
            self.repo(d, "second, no refs")
            r = self.run_tool(d, {"WEFT_BIN": "/nonexistent/weft"})
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("No issue or PR numbers", r.stdout)

    def test_a_missing_binary_is_a_message_not_a_traceback(self):
        with tempfile.TemporaryDirectory() as d:
            self.repo(d, "fixes #42")
            r = self.run_tool(d, {"WEFT_BIN": "/nonexistent/weft"})
        self.assertEqual(r.returncode, 1)
        self.assertNotIn("Traceback", r.stderr)
        self.assertIn("cannot run", r.stderr)
        # The message has to say how to fix it, or it is just a nicer crash.
        self.assertIn("WEFT_BIN", r.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
