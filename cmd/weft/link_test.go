package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// addNode returns the displayed short id of a freshly added node.
func addNode(t *testing.T, title, kind string) string {
	t.Helper()
	code, stdout, stderr := session(t, "add", title, "--kind", kind)
	if code != exitOK {
		t.Fatalf("add %q: exit %d, %s", title, code, stderr)
	}
	return strings.Fields(stdout)[0]
}

func TestLinkAndShowNeighborhood(t *testing.T) {
	scratch(t)
	task := addNode(t, "task 7: batch export", "task")
	finding := addNode(t, "review point Z", "finding")
	pr := addNode(t, "PR: fix pagination", "pr")

	for _, args := range [][]string{
		{"link", task, "depends-on", finding},
		{"link", finding, "part-of", pr},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}

	// One invocation has to answer both "what does this need" and "what needs
	// this" (SPEC.md §9.4).
	code, stdout, stderr := session(t, "show", finding)
	if code != exitOK {
		t.Fatalf("show: exit %d, %s", code, stderr)
	}
	for _, want := range []string{
		"review point Z",       // the node itself
		"part of",              // outbound reading
		"PR: fix pagination",   // neighbour title, not just an id
		"blocks",               // inbound reading, inverted
		"task 7: batch export", // the other neighbour's title
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show output missing %q:\n%s", want, stdout)
		}
	}
}

// The milestone's headline: a cycle is rejected *and* the offending path is
// printed. Naming the constraint without showing what violates it leaves the
// caller unable to act.
func TestLinkCycleRejectedWithPath(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "bravo", "task")
	c := addNode(t, "charlie", "task")

	session(t, "link", a, "depends-on", b)
	session(t, "link", b, "depends-on", c)

	code, _, stderr := session(t, "link", c, "depends-on", a)
	if code != exitRejected {
		t.Fatalf("exit %d, want %d", code, exitRejected)
	}
	for _, want := range []string{"cycle", "alpha", "bravo", "charlie"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("cycle diagnostic missing %q:\n%s", want, stderr)
		}
	}

	// And nothing was written.
	_, shown, _ := session(t, "show", c)
	if strings.Contains(shown, "alpha") {
		t.Errorf("rejected edge was committed anyway:\n%s", shown)
	}
}

func TestLinkSelfEdgeRejected(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	if code, _, _ := session(t, "link", a, "depends-on", a); code != exitRejected {
		t.Errorf("self depends-on: exit %d, want %d", code, exitRejected)
	}
	// conflicts-with is not acyclic, so a self-edge is merely odd, not invalid.
	if code, _, stderr := session(t, "link", a, "conflicts-with", a); code != exitOK {
		t.Errorf("self conflicts-with: exit %d, %s", code, stderr)
	}
}

// A symmetric edge must read the same from either endpoint, and must never
// report its own node as the neighbour.
func TestSymmetricEdgeReadsFromBothEnds(t *testing.T) {
	scratch(t)
	mine := addNode(t, "my fix branch", "task")
	theirs := addNode(t, "PR: fix pagination", "pr")

	if code, _, stderr := session(t, "link", mine, "conflicts-with", theirs); code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}

	for _, pair := range [][2]string{{mine, "PR: fix pagination"}, {theirs, "my fix branch"}} {
		_, stdout, _ := session(t, "show", pair[0])
		if !strings.Contains(stdout, "conflicts with") {
			t.Errorf("show %s missing the conflict:\n%s", pair[0], stdout)
		}
		if !strings.Contains(stdout, pair[1]) {
			t.Errorf("show %s should name the other node %q:\n%s", pair[0], pair[1], stdout)
		}
	}

	// Regression: the neighbour is the *other* endpoint. Deriving it from the
	// edge's stored direction made a node conflict with itself.
	_, stdout, _ := session(t, "show", theirs)
	inConflict := stdout[strings.Index(stdout, "conflicts with"):]
	if strings.Contains(inConflict, "PR: fix pagination") {
		t.Errorf("node reported as its own neighbour:\n%s", stdout)
	}
}

// Asserting the same symmetric relation from both directions is one fact.
func TestSymmetricEdgeDeduplicates(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "bravo", "task")

	session(t, "link", a, "conflicts-with", b)
	session(t, "link", b, "conflicts-with", a)

	_, stdout, _ := session(t, "show", a)
	if n := strings.Count(stdout, "bravo"); n != 1 {
		t.Errorf("edge asserted from both directions appears %d times, want 1:\n%s", n, stdout)
	}
}

func TestLinkResolvesPrefixesAndRefs(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	session(t, "add", "bravo", "--kind", "issue", "--ref", "gh:acme/api#9")

	if code, _, stderr := session(t, "link", a[:4], "depends-on", "gh:acme/api#9"); code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	_, stdout, _ := session(t, "show", a)
	if !strings.Contains(stdout, "bravo") {
		t.Errorf("edge not created:\n%s", stdout)
	}
}

func TestLinkFailureExitCodes(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"unknown target", []string{"link", a, "depends-on", "ffffff"}, exitNotFound},
		{"unknown source node", []string{"link", "ffffff", "depends-on", a}, exitNotFound},
		{"unknown edge kind", []string{"link", a, "relates-to", a}, exitUsage},
		{"too few args", []string{"link", a, "depends-on"}, exitUsage},
		{"too many args", []string{"link", a, "depends-on", a, a}, exitUsage},
		{"bad source flag", []string{"link", a, "depends-on", a, "--source", "vibes"}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := session(t, tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

func TestLinkCycleJSONCarriesPath(t *testing.T) {
	scratch(t)
	a := addNode(t, "alpha", "task")
	b := addNode(t, "bravo", "task")
	session(t, "link", a, "depends-on", b)

	code, stdout, _ := session(t, "link", b, "depends-on", a, "--json")
	if code != exitRejected {
		t.Fatalf("exit %d", code)
	}
	var out struct {
		Code       int      `json:"code"`
		Candidates []string `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if out.Code != exitRejected {
		t.Errorf("code = %d", out.Code)
	}
	if len(out.Candidates) < 2 {
		t.Errorf("expected the cycle path, got %v", out.Candidates)
	}
}

// A dangling edge is a normal state, and show must not hide or crash on it.
func TestShowToleratesDanglingEdge(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "alpha", "task")

	appendRaw(t, dir, `{"at":"2026-07-31T09:14:22Z","by":"user:t","ev":"edge.add","from":"`+
		fullID(t, a)+`","id":"`+strings.Repeat("e", 64)+`","kind":"depends-on","source":"asserted","to":"`+
		strings.Repeat("f", 64)+`","v":1}`)

	code, stdout, _ := session(t, "show", a)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "not in this store") {
		t.Errorf("dangling neighbour should be shown as such:\n%s", stdout)
	}

	// And a --json consumer has to be able to tell the difference, or it
	// renders an edge to a node with no title as though the title were empty.
	_, asJSON, _ := session(t, "show", a, "--json")
	var out struct {
		Edges []struct {
			ID    string `json:"id"`
			Known bool   `json:"known"`
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(asJSON), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, asJSON)
	}
	if len(out.Edges) != 1 {
		t.Fatalf("got %d edges, want the dangling one", len(out.Edges))
	}
	if out.Edges[0].Known {
		t.Error("a dangling edge is reported as known")
	}
}

// The write half of EVENTS.md §3.9. `add --if-absent` made node writes
// retry-safe and `link` had no equivalent, so any writer that re-ran — a
// scheduled import, an agent resuming after a crash, a derivation that
// recomputes rather than remembers — appended an edge.add per run that the fold
// then discarded. Nothing failed and no query changed, which is what made it
// invisible: measured on a 29-PR derivation, a second identical run added 55
// events and 19 KB.
//
// Byte-identical, not "no new edge": the log is the contract, and tolerance on
// the read side is not idempotence on the write side.
func TestLinkIfAbsentAppendsNothingWhenTheEdgeIsAlreadyThere(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "task: derive the graph", "task")
	b := addNode(t, "spec: derivation rules", "spec")

	if code, _, stderr := session(t, "link", a, "depends-on", b, "--if-absent"); code != exitOK {
		t.Fatalf("first link --if-absent: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)

	for i := range 3 {
		code, stdout, stderr := session(t, "link", a, "depends-on", b, "--if-absent")
		if code != exitOK {
			t.Fatalf("re-link %d: exit %d, %s", i, code, stderr)
		}
		// The retry reports the edge it found, so a caller cannot tell a
		// no-op from a write and does not need to.
		if want := a + " depends-on " + b + "\n"; stdout != want {
			t.Errorf("re-link %d printed %q, want %q", i, stdout, want)
		}
	}
	if after := logText(t, dir); after != before {
		t.Errorf("re-linking with --if-absent grew the log by %d bytes; want a byte-identical log",
			len(after)-len(before))
	}
	if got := edgeCount(t); got != 1 {
		t.Errorf("%d edges after four links of one triple, want 1", got)
	}
}

// The default stays an assertion, deliberately. The fold keeps the earliest
// triple, so a second asserter never shows up in a query — but the event is in
// the log, and the log is what other tools read (SPEC.md §9.1). Making dedupe
// the default would erase a second, independent witness from the durable
// artifact to save bytes nobody is short of; --if-absent is the writer saying "I
// am re-running", which is a different statement from "I also believe this".
func TestLinkWithoutIfAbsentStillRecordsTheSecondAssertion(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "task: shared ground", "task")
	b := addNode(t, "spec: shared ground", "spec")

	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	before := logLines(t, dir)
	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("second link: exit %d, %s", code, stderr)
	}
	if after := logLines(t, dir); after != before+1 {
		t.Errorf("a plain re-link appended %d lines; want 1", after-before)
	}
	if got := edgeCount(t); got != 1 {
		t.Errorf("%d edges, want 1 — the fold still collapses the triple", got)
	}
}

// Endpoint order is not information for a symmetric kind, and the fold
// normalizes it when collapsing triples. An --if-absent check with its own
// notion of sameness would miss the reversed assertion and append the duplicate
// it exists to prevent — the same trap FindEdge was written for on the unlink
// side.
func TestLinkIfAbsentFindsASymmetricEdgeAssertedTheOtherWay(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "pr: rate-limit middleware", "pr")
	b := addNode(t, "pr: progress panel", "pr")

	if code, _, stderr := session(t, "link", a, "conflicts-with", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)

	code, stdout, stderr := session(t, "link", b, "conflicts-with", a, "--if-absent")
	if code != exitOK {
		t.Fatalf("reversed link --if-absent: exit %d, %s", code, stderr)
	}
	if after := logText(t, dir); after != before {
		t.Errorf("the reversed assertion appended %d bytes; want 0", len(after)-len(before))
	}
	// Reported as the graph holds it, not as the request phrased it: the answer
	// to "is this already there" is the edge that is already there.
	if want := a + " conflicts-with " + b + "\n"; stdout != want {
		t.Errorf("printed %q, want the stored orientation %q", stdout, want)
	}
}

// --if-absent must key off the edge, never off "have I seen this triple". The
// triple key is released when an edge is retracted (dropEdge), so a re-link
// after an unlink is a genuinely new assertion and has to be written. A check
// that ignored retraction would exit 0, print the edge, and leave the graph
// holding nothing — the failure TestAnEdgeCanBeRelinkedAfterUnlink pins for the
// fold, reappearing one layer up.
func TestLinkIfAbsentCreatesTheEdgeAgainAfterUnlink(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: reversible with if-absent", "task")
	b := addNode(t, "spec: reversible with if-absent", "spec")

	for _, args := range [][]string{
		{"link", a, "depends-on", b, "--if-absent"},
		{"unlink", a, "depends-on", b},
		{"link", a, "depends-on", b, "--if-absent"},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}
	if got := edgeCount(t); got != 1 {
		t.Errorf("after link/unlink/link --if-absent: %d edges, want 1", got)
	}
}

// Every node reference canonicalizes through the alias map, so an edge asserted
// against a node that was later merged away is the survivor's edge. A writer
// re-running against the pre-merge id must see it, or the merge turns a
// no-op back into a duplicate append.
func TestLinkIfAbsentSeesThroughAMerge(t *testing.T) {
	dir := scratch(t)
	survivor := addNode(t, "task: extract report query", "task")
	dup := addNode(t, "task: extract the report query", "task")
	spec := addNode(t, "spec: report query", "spec")

	for _, args := range [][]string{
		{"link", dup, "depends-on", spec},
		{"merge", survivor, dup},
	} {
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}
	before := logText(t, dir)

	// Both spellings name the same edge now.
	for _, from := range []string{dup, survivor} {
		if code, _, stderr := session(t, "link", from, "depends-on", spec, "--if-absent"); code != exitOK {
			t.Fatalf("link --if-absent via %s: exit %d, %s", from, code, stderr)
		}
	}
	if after := logText(t, dir); after != before {
		t.Errorf("--if-absent across a merge appended %d bytes; want 0", len(after)-len(before))
	}
	if got := edgeCount(t); got != 1 {
		t.Errorf("%d edges, want 1", got)
	}
}

// Go's flag package stops at the first positional, so every command permutes
// first (BUILD.md §1.2). Skipping it here would leave --if-absent silently
// ignored: the command exits 0, prints the edge, and appends the duplicate it
// was told not to.
func TestLinkIfAbsentIsParsedAfterPositionals(t *testing.T) {
	dir := scratch(t)
	a := addNode(t, "task: flag after positionals", "task")
	b := addNode(t, "spec: flag after positionals", "spec")

	if code, _, stderr := session(t, "link", a, "depends-on", b, "--if-absent"); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	before := logText(t, dir)
	if code, _, stderr := session(t, "link", a, "depends-on", b, "--if-absent"); code != exitOK {
		t.Fatalf("re-link: exit %d, %s", code, stderr)
	}
	if after := logText(t, dir); after != before {
		t.Fatal("--if-absent after the positionals was ignored; the duplicate landed")
	}
}

// A retry must return the same endpoints in the same order, so a caller can
// treat the two runs alike.
//
// Byte-identity is deliberately *not* claimed. link --json renders each endpoint
// as the fold sees it, and the first link touches both nodes, so the retry
// reports a later `touched` — true of plain `link --json` run twice as well, and
// not something --if-absent introduces or should quietly paper over. What has to
// be stable is identity, which is what a writer keys on.
func TestLinkIfAbsentJSONNamesTheSameEndpointsOnRetry(t *testing.T) {
	scratch(t)
	a := addNode(t, "task: json retry", "task")
	b := addNode(t, "spec: json retry", "spec")

	ids := func(out string) []string {
		t.Helper()
		var payload struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("link --json does not parse: %v\n%s", err, out)
		}
		var got []string
		for _, n := range payload.Nodes {
			got = append(got, n.ID)
		}
		return got
	}

	code, first, stderr := session(t, "link", a, "depends-on", b, "--if-absent", "--json")
	if code != exitOK {
		t.Fatalf("first: exit %d, %s", code, stderr)
	}
	code, second, stderr := session(t, "link", a, "depends-on", b, "--if-absent", "--json")
	if code != exitOK {
		t.Fatalf("retry: exit %d, %s", code, stderr)
	}
	if got, want := ids(second), ids(first); !slices.Equal(got, want) {
		t.Errorf("retry named %v, want the same endpoints in the same order %v", got, want)
	}
	if got := ids(second); len(got) != 2 {
		t.Errorf("want both endpoints in the JSON, got %d nodes", len(got))
	}
}
