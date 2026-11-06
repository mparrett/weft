package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The milestone 8 exit condition: refs, edges, and claims union, and the merge
// is log-only — nothing but the log changes, so the result survives a re-fold
// in a separate invocation.
func TestMergeUnionsAcrossSessions(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "N+1 in report generation", "bug", "--ref", "gh:acme/api#412")
	b := addNodeFull(t, "upstream: slow report query", "bug", "--ref", "gh:acme/api#508")
	blocker := addNodeFull(t, "add query index", "task")

	for _, args := range [][]string{
		{"link", b, "depends-on", blocker},
		{"claim", b, "--holder", "user:colleague", "--source", "inferred"},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}

	if code, stdout, stderr := session(t, "merge", a, b); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	} else if !strings.Contains(stdout, "merged") {
		t.Errorf("merge said nothing useful: %q", stdout)
	}

	code, shown, stderr := session(t, "show", a)
	if code != exitOK {
		t.Fatalf("show: exit %d, %s", code, stderr)
	}
	for _, want := range []string{
		"gh:acme/api#412",             // its own ref
		"gh:acme/api#508",             // the merged node's ref
		"user:colleague",              // the merged node's claim
		"add query index",             // the merged node's dependency
		"upstream: slow report query", // named as merged, so the redirect is visible
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("show missing %q after merge:\n%s", want, shown)
		}
	}

	// One piece of work, listed once.
	_, listed, _ := session(t, "ls")
	if strings.Contains(listed, "upstream: slow report query") {
		t.Errorf("merged node still listed:\n%s", listed)
	}
	if n := strings.Count(strings.TrimSpace(listed), "\n") + 1; n != 2 {
		t.Errorf("ls listed %d nodes, want 2:\n%s", n, listed)
	}
}

// The id someone pasted from a commit message a month ago has to keep working.
func TestMergedIDStillResolves(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "survivor", "task")
	b := addNodeFull(t, "duplicate", "task")
	session(t, "merge", a, b)

	for _, query := range []string{b, b[:7]} {
		code, stdout, stderr := session(t, "show", query)
		if code != exitOK {
			t.Fatalf("show %s: exit %d, %s", query, code, stderr)
		}
		if !strings.Contains(stdout, "survivor") {
			t.Errorf("show %s did not land on the survivor:\n%s", query, stdout)
		}
	}

	// Queries rooted at the merged node reach the survivor's graph too.
	if code, _, stderr := session(t, "why", b); code != exitOK {
		t.Errorf("why on a merged id: exit %d, %s", code, stderr)
	}
}

// A merge that would close a depends-on cycle is rejected with the path, and
// nothing is written (BUILD.md §3, milestone 8).
func TestMergeRejectsCycleAndWritesNothing(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "survivor", "task")
	b := addNodeFull(t, "duplicate", "task")
	mid := addNodeFull(t, "middle", "task")

	for _, args := range [][]string{
		{"link", a, "depends-on", mid},
		{"link", mid, "depends-on", b},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}

	code, _, stderr := session(t, "merge", a, b)
	if code != exitRejected {
		t.Fatalf("merge closing a cycle: exit %d, want %d", code, exitRejected)
	}
	if !strings.Contains(stderr, "cycle") || !strings.Contains(stderr, "middle") {
		t.Errorf("rejection did not print the offending path:\n%s", stderr)
	}

	// Nothing written: the merged node is still its own node.
	_, listed, _ := session(t, "ls")
	if !strings.Contains(listed, "duplicate") {
		t.Errorf("a rejected merge still folded the node:\n%s", listed)
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "survivor", "task")
	b := addNodeFull(t, "duplicate", "task")

	if code, _, stderr := session(t, "merge", a, b); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}
	// Re-merging an already-folded pair is the desired state, not a failure
	// (SPEC.md §9.3).
	code, stdout, stderr := session(t, "merge", a, b)
	if code != exitOK {
		t.Fatalf("re-merge: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "already") {
		t.Errorf("re-merge did not say the state already held: %q", stdout)
	}
	if code, _, _ := session(t, "merge", a, a); code != exitOK {
		t.Errorf("merging a node into itself: exit %d, want %d", code, exitOK)
	}
}

func TestMergeJSONReportsSurvivor(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "survivor", "task", "--ref", "gh:acme/api#1")
	b := addNodeFull(t, "duplicate", "task", "--ref", "gh:acme/api#2")

	code, stdout, stderr := session(t, "merge", a, b, "--json")
	if code != exitOK {
		t.Fatalf("merge --json: exit %d, %s", code, stderr)
	}
	var out struct {
		Nodes []struct {
			ID    string   `json:"id"`
			Title string   `json:"title"`
			Refs  []string `json:"refs"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("merge --json emitted invalid JSON: %v\n%s", err, stdout)
	}
	// add echoes the display prefix, and --json carries the full id.
	if len(out.Nodes) != 1 || !strings.HasPrefix(out.Nodes[0].ID, a) {
		t.Fatalf("want the survivor %s, got %+v", a, out.Nodes)
	}
	if len(out.Nodes[0].Refs) != 2 {
		t.Errorf("survivor reports %v, want both refs", out.Nodes[0].Refs)
	}
}

// Flags after positionals, per BUILD.md §1.2 — Go's flag package stops at the
// first positional, so without cli.Permute this silently succeeds while doing
// the wrong thing.
func TestMergeParsesFlagsAfterPositionals(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "survivor", "task")
	b := addNodeFull(t, "duplicate", "task")

	code, stdout, stderr := session(t, "merge", a, b, "--json")
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("--json after positionals was ignored:\n%s", stdout)
	}
}

func TestMergeUnknownNodeExitsNotFound(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "survivor", "task")
	if code, _, _ := session(t, "merge", a, "ffffffff"); code != exitNotFound {
		t.Errorf("exit %d, want %d", code, exitNotFound)
	}
}
