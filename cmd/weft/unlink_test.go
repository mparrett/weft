package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// edgeCount reports how many edges the export holds, which is the fold's own
// answer rather than a rendering of it.
func edgeCount(t *testing.T) int {
	t.Helper()
	code, stdout, stderr := session(t, "export")
	if code != exitOK {
		t.Fatalf("export: exit %d, %s", code, stderr)
	}
	return strings.Count(stdout, "->")
}

func logText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func logLines(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(logText(t, dir), "\n")
}

func TestUnlinkRetractsTheEdge(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: ship the exporter", "task")
	b := addNode(t, "spec: export format", "spec")

	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	if got := edgeCount(t); got != 1 {
		t.Fatalf("after link: %d edges, want 1", got)
	}

	code, stdout, stderr := session(t, "unlink", a, "depends-on", b)
	if code != exitOK {
		t.Fatalf("unlink: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "unlinked") {
		t.Errorf("unlink said %q, which does not report what it did", stdout)
	}
	if got := edgeCount(t); got != 0 {
		t.Errorf("after unlink: %d edges, want 0", got)
	}

	// The neighbourhood is the read an agent actually orients with, so the
	// retracted edge has to be gone from there too, not merely from the export.
	if _, stdout, _ := session(t, "show", a); strings.Contains(stdout, "export format") {
		t.Errorf("show still reports the retracted neighbour:\n%s", stdout)
	}
}

// Retracting an edge that is not there is the desired state already holding, so
// it is exit 0 (EVENTS.md §3.3, SPEC.md §9.3) — and it must write nothing. A
// command that appends a retract naming no edge folds to the same graph while
// growing the log on every run, which is the write-side idempotence EVENTS.md
// §3.9 requires and which no query would ever reveal.
func TestUnlinkAnAbsentEdgeIsANoOpThatWritesNothing(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "task: unrelated", "task")
	b := addNode(t, "note: also unrelated", "note")

	before := logLines(t, dir)
	code, stdout, stderr := session(t, "unlink", a, "depends-on", b)
	if code != exitOK {
		t.Fatalf("unlink of an absent edge: exit %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "no depends-on edge") {
		t.Errorf("unlink said %q, which does not say nothing matched", stdout)
	}
	if after := logLines(t, dir); after != before {
		t.Errorf("unlink of an absent edge appended %d lines; want 0", after-before)
	}

	// Twice over: the second unlink of a genuinely retracted edge is the same
	// no-op, and is the case an agent retrying a failed script actually hits.
	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	if code, _, _ := session(t, "unlink", a, "depends-on", b); code != exitOK {
		t.Fatalf("first unlink: exit %d", code)
	}
	mid := logLines(t, dir)
	if code, _, stderr := session(t, "unlink", a, "depends-on", b); code != exitOK {
		t.Fatalf("second unlink: exit %d, %s", code, stderr)
	}
	if after := logLines(t, dir); after != mid {
		t.Errorf("re-unlinking a retracted edge appended %d lines; want 0", after-mid)
	}
}

// The bug this rules out is silent and total: dropEdge has to release the
// triple key as well as the indexes, or the fold treats the re-link as a
// duplicate of the edge it just dropped and ignores it. `link` would exit 0 and
// print the edge while the graph held nothing.
func TestAnEdgeCanBeRelinkedAfterUnlink(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: reversible", "task")
	b := addNode(t, "spec: reversible", "spec")

	for _, args := range [][]string{
		{"link", a, "depends-on", b},
		{"unlink", a, "depends-on", b},
		{"link", a, "depends-on", b},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}
	if got := edgeCount(t); got != 1 {
		t.Errorf("after link/unlink/link: %d edges, want 1", got)
	}
}

// Endpoint order is not information for a symmetric kind, and the fold
// normalizes it when collapsing triples. If unlink used a different rule it
// would report no edge while holding the one link created.
func TestUnlinkFindsASymmetricEdgeFromEitherEnd(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: option A", "task")
	b := addNode(t, "task: option B", "task")

	if code, _, stderr := session(t, "link", a, "conflicts-with", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	code, stdout, stderr := session(t, "unlink", b, "conflicts-with", a)
	if code != exitOK {
		t.Fatalf("reversed unlink: exit %d, %s", code, stderr)
	}
	if strings.Contains(stdout, "no conflicts-with edge") {
		t.Fatalf("reversed unlink found nothing; the edge is still there:\n%s", stdout)
	}
	if got := edgeCount(t); got != 0 {
		t.Errorf("after reversed unlink: %d edges, want 0", got)
	}
}

// Retracting the blocker is one of the two ways a node becomes ready, and the
// readiness query has to see it. This is the behaviour the command exists for.
func TestUnlinkUnblocksTheDependent(t *testing.T) {
	scratch(t)
	blocked := addNode(t, "task: waiting on nothing much", "task")
	blocker := addNode(t, "task: turned out to be unrelated", "task")

	if code, _, stderr := session(t, "link", blocked, "depends-on", blocker); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	if _, stdout, _ := session(t, "ls", "--ready"); strings.Contains(stdout, "waiting on nothing much") {
		t.Fatalf("node is ready while still blocked:\n%s", stdout)
	}
	if code, _, stderr := session(t, "unlink", blocked, "depends-on", blocker); code != exitOK {
		t.Fatalf("unlink: exit %d, %s", code, stderr)
	}
	if _, stdout, _ := session(t, "ls", "--ready"); !strings.Contains(stdout, "waiting on nothing much") {
		t.Errorf("node is still not ready after its only blocker was retracted:\n%s", stdout)
	}
	if _, stdout, _ := session(t, "why", blocked); !strings.Contains(stdout, "no known blocker") {
		t.Errorf("why still reports a blocker after unlink:\n%s", stdout)
	}
}

// Retracting the edge that closed off a direction has to free that direction,
// or the acyclicity check is reading state the retraction was supposed to clear.
func TestUnlinkAllowsThePreviouslyCyclicEdge(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: first", "task")
	b := addNode(t, "task: second", "task")

	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	if code, _, _ := session(t, "link", b, "depends-on", a); code != exitRejected {
		t.Fatalf("the reverse edge: exit %d, want %d (cycle)", code, exitRejected)
	}
	if code, _, stderr := session(t, "unlink", a, "depends-on", b); code != exitOK {
		t.Fatalf("unlink: exit %d, %s", code, stderr)
	}
	if code, _, stderr := session(t, "link", b, "depends-on", a); code != exitOK {
		t.Errorf("the reverse edge is still refused after the first was retracted: exit %d, %s", code, stderr)
	}
}

// A merge re-points an edge onto the survivor while keeping the edge.add id it
// was asserted under, so a retract still finds it. Both spellings of the far
// endpoint have to work: the survivor's id, and the merged id someone pasted
// from a commit message before the merge happened (SPEC.md §6.2).
func TestUnlinkAfterAMerge(t *testing.T) {
	for _, tc := range []struct{ name, endpoint string }{
		{"by the survivor's id", "survivor"},
		{"by the merged id", "merged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch(t)
			a := addNode(t, "task: depends on the duplicate", "task")
			survivor := addNode(t, "spec: the real one", "spec")
			merged := addNode(t, "spec: the duplicate", "spec")

			for _, args := range [][]string{
				{"link", a, "depends-on", merged},
				{"merge", survivor, merged},
			} {
				if code, _, stderr := session(t, args...); code != exitOK {
					t.Fatalf("%v: exit %d, %s", args, code, stderr)
				}
			}
			if got := edgeCount(t); got != 1 {
				t.Fatalf("after merge: %d edges, want the re-pointed 1", got)
			}

			target := survivor
			if tc.endpoint == "merged" {
				target = merged
			}
			code, stdout, stderr := session(t, "unlink", a, "depends-on", target)
			if code != exitOK {
				t.Fatalf("unlink: exit %d, %s", code, stderr)
			}
			if strings.Contains(stdout, "no depends-on edge") {
				t.Fatalf("unlink found nothing; the re-pointed edge is stranded:\n%s", stdout)
			}
			if got := edgeCount(t); got != 0 {
				t.Errorf("after unlink: %d edges, want 0", got)
			}
		})
	}
}

// Exit 0 covers both a retraction and a no-op, deliberately, so under --json the
// flag is the only thing that distinguishes them. A caller reconciling a graph
// against a source of truth needs to know whether it converged or was already
// there — the same argument that put the walked kinds in a route's JSON.
func TestUnlinkJSONReportsWhetherItRetracted(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "beta", "task")
	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}

	for _, want := range []bool{true, false} {
		code, out, _ := session(t, "unlink", a, "depends-on", b, "--json")
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not valid JSON: %v\n%s", err, out)
		}
		if got["retracted"] != want {
			t.Errorf("retracted = %v, want %v\n%s", got["retracted"], want, out)
		}
		if _, ok := got["nodes"]; !ok {
			t.Errorf("the endpoints are missing from the payload:\n%s", out)
		}
	}
}

func TestUnlinkUsageErrors(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: usage", "task")
	b := addNode(t, "task: usage two", "task")

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"too few arguments", []string{"unlink", a, "depends-on"}, exitUsage},
		{"unknown edge kind", []string{"unlink", a, "relates-to", b}, exitUsage},
		{"unknown source", []string{"unlink", a, "depends-on", b, "--source", "vibes"}, exitUsage},
		{"unknown node", []string{"unlink", "ffffffffff", "depends-on", b}, exitNotFound},
	} {
		if code, _, _ := session(t, tc.args...); code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.want)
		}
	}
}

// BUILD.md §1.2: flags may follow positionals, and Go's flag package stops at
// the first one. Without cli.Permute the --source here is silently ignored, the
// command still exits 0, and the retraction is recorded with the wrong
// provenance.
func TestUnlinkParsesAFlagAfterPositionals(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "task: permuted", "task")
	b := addNode(t, "spec: permuted", "spec")

	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	if code, _, stderr := session(t, "unlink", a, "depends-on", b, "--source", "inferred"); code != exitOK {
		t.Fatalf("unlink with a trailing flag: exit %d, %s", code, stderr)
	}
	if got := edgeCount(t); got != 0 {
		t.Fatalf("after unlink: %d edges, want 0", got)
	}
	log := logText(t, dir)
	if !strings.Contains(log, `"ev":"edge.retract"`) {
		t.Error("no edge.retract in the log")
	}
	if !strings.Contains(log, `"source":"inferred"`) {
		t.Error("--source after the positionals was ignored; provenance is wrong")
	}
}
