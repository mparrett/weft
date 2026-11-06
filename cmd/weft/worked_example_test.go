package main

import (
	"strings"
	"testing"

	"github.com/mparrett/weft/internal/importer"
)

// TestWorkedExample is rung 5 of the oracle (BUILD.md §4): SPEC.md §7 as an
// executable transcript. It is the spec's own claim about what the tool does,
// so it should fail the build when that stops being true.
//
// Every line of §7 now runs. Only the gh shell-out is stubbed, because it is
// the one step that cannot be hermetic; everything below that seam is real.
func TestWorkedExample(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_ACTOR", "user:matt")

	// An agent surveys the repo and records what it finds. Note that only the
	// first has an external identity: the latent bug is exactly the kind of
	// fact no tracker can hold.
	n1 := addNodeFull(t, "N+1 in report generation", "bug", "--ref", "gh:acme/api#412", "--source", "observed")
	n2 := addNodeFull(t, "cache invalidation race", "bug", "--source", "inferred")
	addNodeFull(t, "batch export spec", "spec", "--ref", "file:docs/export.md")

	// You start on the second.
	if code, _, stderr := session(t, "claim", n2); code != exitOK {
		t.Fatalf("claim: exit %d, %s", code, stderr)
	}

	// The upstream issue turns up. Only gh is stubbed; the transaction, the
	// fold, and the binding below it are all real.
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 508, Title: "upstream: slow report query", State: "open"},
	}})
	if code, _, stderr := session(t, "import", "gh", "acme/api#508"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}

	// It is the bug already found locally. The latent node survives: it was
	// asserted first and carries the work. Merging by ref rather than by id is
	// the ordinary way this goes — you have the issue, not the hash.
	if code, _, stderr := session(t, "merge", n2, "gh:acme/api#508"); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}
	// The upstream issue is now a way to reach the surviving node, which is the
	// point of the union.
	_, byRef, _ := session(t, "show", "gh:acme/api#508")
	if !strings.Contains(byRef, "cache invalidation race") {
		t.Errorf("the merged node's ref does not reach the survivor:\n%s", byRef)
	}

	// A colleague is working on it, per the upstream issue's comments — a claim
	// about a third party that nobody here can verify.
	if code, _, _ := session(t, "claim", n2, "--holder", "user:colleague", "--source", "inferred"); code != exitOK {
		t.Fatal("recording a third-party claim must not fail")
	}

	// An adjacent PR conflicts with the fix being built.
	pr := addNodeFull(t, "PR: fix pagination", "pr", "--ref", "pr:acme/api#77", "--source", "observed")
	if code, _, stderr := session(t, "link", n2, "conflicts-with", pr); code != exitOK {
		t.Fatalf("conflicts-with: exit %d, %s", code, stderr)
	}

	// Review on that PR is not green until point Z is addressed, and task 7
	// depends on it.
	z := addNodeFull(t, "review point Z", "finding", "--source", "observed")
	task7 := addNodeFull(t, "task 7: batch export", "task")
	for _, args := range [][]string{
		{"link", z, "part-of", pr},
		{"link", task7, "depends-on", z},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}

	// --- the questions the tool exists to answer ---

	// "What am I blocked on?" — with the path and the holder, never a bare
	// status (SPEC.md §6.1).
	code, why, stderr := session(t, "why", task7)
	if code != exitOK {
		t.Fatalf("why: exit %d, %s", code, stderr)
	}
	if !strings.Contains(why, "review point Z") {
		t.Errorf("why did not name the blocker:\n%s", why)
	}
	if strings.Contains(why, "no known blocker") {
		t.Errorf("why reported no blocker on a blocked node:\n%s", why)
	}

	// "What can I start?" — the frontier. task 7 is blocked; the claimed node
	// is taken; the spec and the PR are free.
	_, ready, _ := session(t, "ls", "--ready")
	if strings.Contains(ready, "task 7") {
		t.Errorf("blocked node listed as ready:\n%s", ready)
	}
	if !strings.Contains(ready, "batch export spec") {
		t.Errorf("unblocked node missing from the frontier:\n%s", ready)
	}
	if !strings.Contains(ready, "N+1 in report generation") {
		t.Errorf("node with no dependencies missing from the frontier:\n%s", ready)
	}

	// "Who is waiting on me?"
	_, rdeps, _ := session(t, "rdeps", z)
	if !strings.Contains(rdeps, "task 7") {
		t.Errorf("rdeps did not find the dependent:\n%s", rdeps)
	}

	// "How do these connect?" — the route crosses depends-on and then part-of,
	// which is why path traverses every kind rather than the link family alone.
	_, route, _ := session(t, "path", task7, pr)
	for _, want := range []string{"review point Z", "PR: fix pagination", "depends on", "part of"} {
		if !strings.Contains(route, want) {
			t.Errorf("route missing %q:\n%s", want, route)
		}
	}

	// The claim collision from earlier is still visible, with both holders and
	// the provenance that separates the observation from the guess.
	_, shown, _ := session(t, "show", n2)
	for _, want := range []string{"user:matt", "user:colleague", "inferred"} {
		if !strings.Contains(shown, want) {
			t.Errorf("show missing %q:\n%s", want, shown)
		}
	}

	// n1 has no dependencies, so it stays ready throughout.
	_, why1, _ := session(t, "why", n1)
	if !strings.Contains(why1, "no known blocker") {
		t.Errorf("expected n1 unblocked:\n%s", why1)
	}
}

func addNodeFull(t *testing.T, title, kind string, extra ...string) string {
	t.Helper()
	args := append([]string{"add", title, "--kind", kind}, extra...)
	code, stdout, stderr := session(t, args...)
	if code != exitOK {
		t.Fatalf("add %q: exit %d, %s", title, code, stderr)
	}
	return strings.Fields(stdout)[0]
}
