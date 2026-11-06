package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mparrett/weft/internal/importer"
)

// nodesJSON parses `ls --all-ns --json` so assertions read the actual field
// rather than a substring of the rendered line. `Contains(out, "pr")` matched
// the ref `pr:acme/api#77` no matter what kind the node had, so making every
// imported PR an issue used to pass.
func nodesJSON(t *testing.T, args ...string) []struct {
	ID    string   `json:"id"`
	NS    string   `json:"ns"`
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	State string   `json:"state"`
	Refs  []string `json:"refs"`
} {
	t.Helper()
	code, stdout, stderr := session(t, append(args, "--json")...)
	if code != exitOK {
		t.Fatalf("%v: exit %d, %s", args, code, stderr)
	}
	var out struct {
		Nodes []struct {
			ID    string   `json:"id"`
			NS    string   `json:"ns"`
			Kind  string   `json:"kind"`
			Title string   `json:"title"`
			State string   `json:"state"`
			Refs  []string `json:"refs"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	return out.Nodes
}

// Two issues with the same title must stay two nodes.
//
// A node id hashes the whole record and carries no issue number, so one
// timestamp for the batch made them hash identically and the fold discarded the
// second as a crash-retry — collapsing two unrelated issues into one node, with
// a duplicate id in the durable log. Titles like "flaky test" repeat constantly
// in real repos.
func TestImportKeepsSameTitledIssuesDistinct(t *testing.T) {
	dir := scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 1, Title: "flaky test", State: "open"},
		{Number: 2, Title: "flaky test", State: "open"},
		{Number: 3, Title: "flaky test", State: "open"},
	}})
	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}

	nodes := nodesJSON(t, "ls", "--all-ns")
	if len(nodes) != 3 {
		t.Fatalf("got %d nodes for 3 same-titled issues, want 3", len(nodes))
	}
	// Each keeps its own ref rather than one node accumulating all three.
	for _, n := range nodes {
		if len(n.Refs) != 1 {
			t.Errorf("node %s carries %v; refs from distinct issues were merged", n.ID[:7], n.Refs)
		}
	}
	// The durable artifact must not contain a repeated id — other tools read it.
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(logBytes(t, dir))), "\n") {
		var e struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if seen[e.ID] {
			t.Errorf("duplicate event id %s in the log", e.ID[:7])
		}
		seen[e.ID] = true
	}
}

// A merge must carry the observation onto the survivor, or the next import
// reports a change against upstream that did not change — breaking the
// byte-idempotence of EVENTS.md §3.9 — and `show` drops the age annotation
// SPEC.md §6.3 makes load-bearing.
func TestImportStaysIdempotentAcrossAMerge(t *testing.T) {
	dir := scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 412, Title: "upstream: slow report query", State: "open"},
	}})
	session(t, "import", "gh", "acme/api")
	local := addNodeFull(t, "the same bug, found locally", "bug")

	if code, _, stderr := session(t, "merge", local, "gh:acme/api#412"); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}
	// The age annotation survives the merge.
	_, shown, _ := session(t, "show", local)
	if !strings.Contains(shown, "observed") {
		t.Errorf("the merge dropped the upstream observation:\n%s", shown)
	}

	before := logBytes(t, dir)
	code, stdout, stderr := session(t, "import", "gh", "acme/api")
	if code != exitOK {
		t.Fatalf("re-import: exit %d, %s", code, stderr)
	}
	if got := logBytes(t, dir); string(got) != string(before) {
		t.Errorf("re-import after a merge appended %d bytes against unchanged upstream",
			len(got)-len(before))
	}
	if strings.Contains(stderr, "changed upstream") {
		t.Errorf("reported a change that did not happen:\n%s", stderr)
	}
	if !strings.Contains(stdout, "up to date") {
		t.Errorf("did not report that nothing changed: %q", stdout)
	}
}

// State is set once and never revisited, so an unrecognised value must fail
// towards open. Landing done drops real work out of the frontier permanently.
func TestImportStateMapping(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"open", "open"},
		{"closed", "done"},
		{"merged", "done"},
		{"", "open"},        // gh omitted the field
		{"Draft", "open"},   // a value a newer gh reports
		{"unknown", "open"}, // anything else
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			scratch(t)
			installGH(t, &fakeGH{issues: []importer.Item{
				{Number: 1, Title: "an issue", State: tc.state},
			}})
			session(t, "import", "gh", "acme/api")
			nodes := nodesJSON(t, "ls", "--all-ns")
			if len(nodes) != 1 {
				t.Fatalf("got %d nodes", len(nodes))
			}
			if nodes[0].State != tc.want {
				t.Errorf("upstream %q landed state %q, want %q", tc.state, nodes[0].State, tc.want)
			}
		})
	}
}

// A single-field upstream change has to be detected on its own, or a rename
// with a stable state (or a close with a stable title) is never recorded.
func TestImportDetectsSingleFieldChanges(t *testing.T) {
	for _, tc := range []struct {
		name         string
		title, state string
	}{
		{"title only", "renamed upstream", "open"},
		{"state only", "original", "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := scratch(t)
			gh := &fakeGH{issues: []importer.Item{{Number: 1, Title: "original", State: "open"}}}
			installGH(t, gh)
			session(t, "import", "gh", "acme/api")
			before := len(logBytes(t, dir))

			gh.issues[0].Title, gh.issues[0].State = tc.title, tc.state
			_, stdout, stderr := session(t, "import", "gh", "acme/api")
			if len(logBytes(t, dir)) == before {
				t.Error("the change was not recorded")
			}
			if !strings.Contains(stdout, "1 with upstream changes") {
				t.Errorf("summary did not report the change: %q", stdout)
			}
			if !strings.Contains(stderr, "changed upstream") {
				t.Errorf("no note for the change:\n%s", stderr)
			}
		})
	}
}

// A PR gets kind pr, asserted on the field rather than on a substring that the
// ref itself satisfies.
func TestImportPRGetsPRKind(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{
		issues: []importer.Item{{Number: 1, Title: "an issue", State: "open"}},
		prs:    []importer.Item{{Number: 77, Title: "fix pagination", State: "open"}},
	})
	// --kind applies to issues only; a PR is a PR whatever the flag says.
	session(t, "import", "gh", "acme/api", "--kind", "bug")

	byTitle := map[string]string{}
	for _, n := range nodesJSON(t, "ls", "--all-ns") {
		byTitle[n.Title] = n.Kind
	}
	if byTitle["fix pagination"] != "pr" {
		t.Errorf("PR imported as kind %q, want pr", byTitle["fix pagination"])
	}
	if byTitle["an issue"] != "bug" {
		t.Errorf("--kind was ignored for the issue: got %q", byTitle["an issue"])
	}
}

// A truncated import must not look like a complete one. gh lists newest first,
// so the window is not even stable across runs.
func TestImportWarnsWhenTruncated(t *testing.T) {
	scratch(t)
	var issues []importer.Item
	for i := 1; i <= 6; i++ {
		issues = append(issues, importer.Item{Number: i, Title: fmt.Sprintf("issue %d", i), State: "open"})
	}
	gh := &fakeGH{issues: issues}
	installGH(t, gh)

	// The public limit plus one sentinel must reach gh, not merely parse. The
	// extra result is how import knows an exactly full window omitted nothing.
	var sawLimit string
	prev := ghRunner
	ghRunner = func(args ...string) ([]byte, error) {
		for i, a := range args {
			if a == "--limit" && i+1 < len(args) {
				sawLimit = args[i+1]
			}
		}
		return gh.run(args...)
	}
	t.Cleanup(func() { ghRunner = prev })

	_, _, stderr := session(t, "import", "gh", "acme/api", "--limit", "5")
	if sawLimit != "6" {
		t.Errorf("--limit reached gh as %q, want 6 (5 plus the sentinel)", sawLimit)
	}
	if !strings.Contains(stderr, "--limit") || !strings.Contains(stderr, "newest first") {
		t.Errorf("a truncated import did not say so:\n%s", stderr)
	}
}

// One collided ref must not block a scheduled import of everything else, and
// the message has to name the remedy.
func TestImportSkipsCollisionAndContinues(t *testing.T) {
	scratch(t)
	for _, title := range []string{"first sighting", "second sighting"} {
		session(t, "add", title, "--ref", "gh:acme/api#1")
	}
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 1, Title: "collides", State: "open"},
		{Number: 2, Title: "imports fine", State: "open"},
	}})

	code, _, stderr := session(t, "import", "gh", "acme/api")
	if code != exitAmbiguous {
		t.Fatalf("exit %d, want %d", code, exitAmbiguous)
	}
	if !strings.Contains(stderr, "weft merge") {
		t.Errorf("the collision did not name the remedy:\n%s", stderr)
	}
	for _, want := range []string{"first sighting", "second sighting"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("collision did not name candidate %q:\n%s", want, stderr)
		}
	}
	// The unambiguous item still landed.
	titles := map[string]bool{}
	for _, n := range nodesJSON(t, "ls", "--all-ns") {
		titles[n.Title] = true
	}
	if !titles["imports fine"] {
		t.Error("a collision on one item blocked the rest of the batch")
	}
}

// The same item listed twice in one response is one node, not two. byRef is a
// snapshot taken before the loop, so the batch has to track what it planned.
func TestImportDedupesWithinOneBatch(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 1, Title: "listed twice", State: "open"},
		{Number: 1, Title: "listed twice", State: "open"},
	}})
	session(t, "import", "gh", "acme/api")

	if nodes := nodesJSON(t, "ls", "--all-ns"); len(nodes) != 1 {
		t.Errorf("got %d nodes for one issue listed twice", len(nodes))
	}
}

// Unreadable output from a gh that ran is a gh problem. Reporting it as a store
// error sends you to inspect a log that is perfectly fine.
func TestImportMalformedGHOutputExitsExternal(t *testing.T) {
	scratch(t)
	prev := ghRunner
	ghRunner = func(args ...string) ([]byte, error) { return []byte(`{"not":"an array"}`), nil }
	t.Cleanup(func() { ghRunner = prev })

	if code, _, _ := session(t, "import", "gh", "acme/api"); code != exitExternal {
		t.Errorf("exit %d, want %d", code, exitExternal)
	}
}

// The full summary line, so a miscount cannot hide behind a substring match.
func TestImportSummaryCounts(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 1, Title: "one", State: "open"},
		{Number: 2, Title: "two", State: "open"},
	}})
	_, stdout, stderr := session(t, "import", "gh", "acme/api")
	if !strings.Contains(stdout, "2 items: 2 added, 0 with upstream changes") {
		t.Errorf("summary wrong: %q", stdout)
	}
	if strings.Contains(stderr, "changed upstream") {
		t.Errorf("first sightings reported as changes:\n%s", stderr)
	}
}
