package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mparrett/weft/internal/importer"
)

// fakeGH stands in for the gh CLI. The shell-out is the only part of import
// that cannot run hermetically, so it is the only part stubbed — everything
// below this seam is the real command, the real transaction, and the real fold.
type fakeGH struct {
	issues []importer.Item
	prs    []importer.Item
	merged []importer.Item // answers the --merged-since search only
	err    error
	calls  int
	args   [][]string
}

func (f *fakeGH) run(args ...string) ([]byte, error) {
	f.calls++
	f.args = append(f.args, args)
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case args[0] == "issue" && args[1] == "list":
		return encodeItems(f.issues), nil
	case args[0] == "pr" && args[1] == "list":
		// The open list and the merged search are the same subcommand, told
		// apart by --state the way gh itself tells them apart.
		for _, a := range args {
			if a == "merged" {
				return encodeItems(f.merged), nil
			}
		}
		return encodeItems(f.prs), nil
	case args[0] == "api":
		// `gh api repos/OWNER/REPO/issues/N` — the REST issues endpoint covers
		// both issues and PRs, and marks the latter with a pull_request key.
		// Modelled faithfully because the real distinction is drawn from the
		// payload's shape, not from whether a second call failed.
		n := args[1][strings.LastIndex(args[1], "/")+1:]
		for _, it := range f.issues {
			if fmt.Sprint(it.Number) == n {
				return encodeAPIItem(it), nil
			}
		}
		for _, it := range f.prs {
			if fmt.Sprint(it.Number) == n {
				return encodeAPIItem(it), nil
			}
		}
		return nil, &importer.ErrUnavailable{Err: errors.New("not found"), Detail: "no such object"}
	}
	return nil, fmt.Errorf("fakeGH: unexpected args %v", args)
}

// encodeAPIItem renders the REST shape, including the pull_request marker.
func encodeAPIItem(it importer.Item) []byte {
	out := map[string]any{"number": it.Number, "title": it.Title, "state": it.State}
	if it.Created != "" {
		out["created_at"] = it.Created
	}
	if it.Updated != "" {
		out["updated_at"] = it.Updated
	}
	if it.IsPR {
		pr := map[string]any{}
		if it.State == "merged" {
			pr["merged_at"] = "2026-07-31T09:00:00Z"
			out["state"] = "closed"
		}
		out["pull_request"] = pr
	}
	b, _ := json.Marshal(out)
	return b
}

func encodeItems(items []importer.Item) []byte {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m := map[string]any{"number": it.Number, "title": it.Title, "state": it.State}
		if it.Created != "" {
			m["createdAt"] = it.Created
		}
		if it.Updated != "" {
			m["updatedAt"] = it.Updated
		}
		out = append(out, m)
	}
	b, _ := json.Marshal(out)
	return b
}

// installGH swaps the runner for one test and restores it afterwards.
func installGH(t *testing.T, f *fakeGH) {
	t.Helper()
	prev := ghRunner
	ghRunner = f.run
	t.Cleanup(func() { ghRunner = prev })
}

func logBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	return b
}

// The milestone 9 exit condition, in its strong form: re-importing unchanged
// upstream leaves the log *byte-identical*, not merely free of duplicate nodes.
// That is what makes running this on a schedule safe.
func TestImportIsIdempotent(t *testing.T) {
	dir := scratch(t)
	installGH(t, &fakeGH{
		issues: []importer.Item{
			{Number: 412, Title: "N+1 in report generation", State: "open"},
			{Number: 508, Title: "upstream: slow report query", State: "open"},
		},
		prs: []importer.Item{{Number: 77, Title: "fix pagination", State: "open"}},
	})

	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	first := logBytes(t, dir)

	code, stdout, stderr := session(t, "import", "gh", "acme/api")
	if code != exitOK {
		t.Fatalf("re-import: exit %d, %s", code, stderr)
	}
	if got := logBytes(t, dir); string(got) != string(first) {
		t.Errorf("re-import appended %d bytes; unchanged upstream must write nothing",
			len(got)-len(first))
	}
	if !strings.Contains(stdout, "up to date") {
		t.Errorf("re-import did not report that nothing changed: %q", stdout)
	}

	// A first observation is not a change. Reporting one on the initial import
	// makes every item look like it moved.
	scratch(t)
	_, initial, initialErr := session(t, "import", "gh", "acme/api")
	if strings.Contains(initial, "3 with upstream changes") || strings.Contains(initialErr, "changed upstream") {
		t.Errorf("initial import reported first sightings as changes:\n%s%s", initial, initialErr)
	}

	// And no duplicate node, which is the weaker condition the same rule buys.
	_, listed, _ := session(t, "ls", "--all-ns")
	if n := strings.Count(strings.TrimSpace(listed), "\n") + 1; n != 3 {
		t.Errorf("ls listed %d nodes, want 3:\n%s", n, listed)
	}
}

func TestImportBindsRefsAndNamespace(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{
		issues: []importer.Item{{Number: 412, Title: "N+1 in report generation", State: "open"}},
		prs:    []importer.Item{{Number: 77, Title: "fix pagination", State: "open"}},
	})
	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}

	// The namespace comes from the imported repo, not the working directory —
	// scratch() pins WEFT_NS to "default", so this would land there if import
	// used the cwd.
	_, listed, _ := session(t, "ls", "--ns", "acme/api")
	if !strings.Contains(listed, "N+1 in report generation") {
		t.Errorf("imported node did not land in the repo's namespace:\n%s", listed)
	}

	// Refs are bound, so the issue resolves without anyone knowing the node id.
	_, shown, _ := session(t, "show", "gh:acme/api#412")
	if !strings.Contains(shown, "N+1 in report generation") {
		t.Errorf("ref did not resolve to the imported node:\n%s", shown)
	}
	if !strings.Contains(shown, "issue") {
		t.Errorf("imported issue did not get kind issue:\n%s", shown)
	}

	// A PR gets the pr: scheme and kind pr, not the --kind default.
	_, pr, _ := session(t, "show", "pr:acme/api#77")
	if !strings.Contains(pr, "fix pagination") || !strings.Contains(pr, "pr") {
		t.Errorf("PR imported wrong:\n%s", pr)
	}
}

// State is set once, at creation, from what was observed then. A closed issue
// imports as done so importing a repo's history does not dump its backlog into
// the frontier.
func TestImportClosedIssueLandsDone(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 1, Title: "already fixed", State: "closed"},
		{Number: 2, Title: "still open", State: "open"},
	}})
	session(t, "import", "gh", "acme/api")

	_, shown, _ := session(t, "show", "gh:acme/api#1")
	if !strings.Contains(shown, "done") {
		t.Errorf("a closed issue did not import as done:\n%s", shown)
	}
	_, ready, _ := session(t, "ls", "--ready", "--ns", "acme/api")
	if strings.Contains(ready, "already fixed") {
		t.Errorf("a closed issue landed in the frontier:\n%s", ready)
	}
	if !strings.Contains(ready, "still open") {
		t.Errorf("an open issue is missing from the frontier:\n%s", ready)
	}
}

// Upstream is cached with provenance, never authority. A change upstream is
// recorded and shown with its age; the node's own fields do not move.
func TestImportObservesWithoutOverwriting(t *testing.T) {
	dir := scratch(t)
	gh := &fakeGH{issues: []importer.Item{{Number: 412, Title: "original title", State: "open"}}}
	installGH(t, gh)
	session(t, "import", "gh", "acme/api")
	before := len(logBytes(t, dir))

	// The maintainer renames it and closes it.
	gh.issues[0].Title = "renamed upstream"
	gh.issues[0].State = "closed"
	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("re-import: exit %d, %s", code, stderr)
	}
	if len(logBytes(t, dir)) == before {
		t.Error("an upstream change wrote nothing")
	}

	_, shown, _ := session(t, "show", "gh:acme/api#412")
	// weft's own fields are untouched: GitHub does not get to retitle your node
	// or close your work.
	if !strings.Contains(shown, "original title") {
		t.Errorf("upstream overwrote the node's title:\n%s", shown)
	}
	if strings.Contains(shown, "state  done") {
		t.Errorf("upstream closed the node:\n%s", shown)
	}
	// But the divergence is visible, and dated.
	if !strings.Contains(shown, "closed") || !strings.Contains(shown, "observed") {
		t.Errorf("the upstream observation is not shown with its age:\n%s", shown)
	}
}

// Import carries the artifact's own clock — the issue's createdAt/updatedAt —
// into the observation (EVENTS.md §3.8). Idempotence must hold with the
// payload present, and a bare upstream touch (updatedAt moved, nothing else)
// is a real change: the payload records what upstream says, and judging a
// touch to be noise is the reader's call, not the writer's.
func TestImportRecordsTheArtifactClock(t *testing.T) {
	dir := scratch(t)
	gh := &fakeGH{issues: []importer.Item{{
		Number: 412, Title: "N+1 in report generation", State: "open",
		Created: "2024-03-01T10:00:00Z", Updated: "2026-08-01T09:30:00Z",
	}}}
	installGH(t, gh)

	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	first := logBytes(t, dir)
	for _, stamp := range []string{`"created":"2024-03-01T10:00:00Z"`, `"modified":"2026-08-01T09:30:00Z"`} {
		if !strings.Contains(string(first), stamp) {
			t.Errorf("the observation does not carry %s:\n%s", stamp, first)
		}
	}

	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("re-import: exit %d, %s", code, stderr)
	}
	if got := logBytes(t, dir); string(got) != string(first) {
		t.Errorf("re-import with an unchanged payload appended %d bytes", len(got)-len(first))
	}

	gh.issues[0].Updated = "2026-08-19T14:00:00Z"
	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("re-import after touch: exit %d, %s", code, stderr)
	}
	after := logBytes(t, dir)
	if len(after) == len(first) {
		t.Error("an upstream touch wrote nothing; modified moving is a change")
	}
	if n := strings.Count(string(after), `"ev":"ref.observe"`); n != 2 {
		t.Errorf("expected exactly 2 observations after one touch, found %d", n)
	}
}

// One ref on two nodes is a normal state, not corruption — but import cannot
// pick a winner, because that judgment is `weft merge` (EVENTS.md §2.2).
func TestImportRefusesAmbiguousRef(t *testing.T) {
	scratch(t)
	for _, title := range []string{"first sighting", "second sighting"} {
		if code, _, stderr := session(t, "add", title, "--ref", "gh:acme/api#412"); code != exitOK {
			t.Fatalf("add %q: exit %d, %s", title, code, stderr)
		}
	}
	installGH(t, &fakeGH{issues: []importer.Item{{Number: 412, Title: "upstream", State: "open"}}})

	code, _, stderr := session(t, "import", "gh", "acme/api")
	if code != exitAmbiguous {
		t.Fatalf("exit %d, want %d", code, exitAmbiguous)
	}
	for _, want := range []string{"first sighting", "second sighting"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("ambiguity did not name candidate %q:\n%s", want, stderr)
		}
	}
}

// A missing or unauthenticated gh is an environment failure, not a corrupt log.
func TestImportGHUnavailableExitsExternal(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{err: &importer.ErrUnavailable{
		Err:    errors.New("exit status 4"),
		Detail: "gh: To get started with GitHub CLI, please run: gh auth login",
	}})

	code, _, stderr := session(t, "import", "gh", "acme/api")
	if code != exitExternal {
		t.Fatalf("exit %d, want %d", code, exitExternal)
	}
	if !strings.Contains(stderr, "auth login") {
		t.Errorf("gh's own diagnostic was swallowed:\n%s", stderr)
	}
}

// Bulk is the design, not an add-on: the whole run is one gh fetch pair and one
// append, not one transaction per item (BUILD.md §1.4).
func TestImportBatchesTheWholeRun(t *testing.T) {
	dir := scratch(t)
	var issues []importer.Item
	for i := 1; i <= 50; i++ {
		issues = append(issues, importer.Item{Number: i, Title: fmt.Sprintf("issue %d", i), State: "open"})
	}
	gh := &fakeGH{issues: issues}
	installGH(t, gh)

	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	// Two calls total — `issue list` and `pr list` — regardless of item count.
	if gh.calls != 2 {
		t.Errorf("gh invoked %d times for 50 items, want 2", gh.calls)
	}
	// node.add + ref.add + ref.observe per item, and nothing else.
	if lines := strings.Count(string(logBytes(t, dir)), "\n"); lines != 150 {
		t.Errorf("log has %d events for 50 items, want 150", lines)
	}
}

func TestImportSingleIssue(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 508, Title: "upstream: slow report query", State: "open"},
		{Number: 999, Title: "unrelated", State: "open"},
	}})

	if code, _, stderr := session(t, "import", "gh", "acme/api#508"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	_, listed, _ := session(t, "ls", "--all-ns")
	if strings.Contains(listed, "unrelated") {
		t.Errorf("importing one issue pulled in the whole repo:\n%s", listed)
	}
	if !strings.Contains(listed, "slow report query") {
		t.Errorf("the named issue was not imported:\n%s", listed)
	}
}

// The motivating case for --merged-since: a repo with nothing open. Without the
// window it imports a graph that cannot explain itself, and with every node open
// `ls --ready` returns the whole listing, which is the same as returning nothing.
func TestImportMergedSinceBringsPRsInAsDone(t *testing.T) {
	scratch(t)
	gh := &fakeGH{
		issues: []importer.Item{{Number: 412, Title: "N+1 in report generation", State: "open"}},
		merged: []importer.Item{
			{Number: 77, Title: "fix pagination", State: "merged", IsPR: true},
			{Number: 78, Title: "cache the query", State: "merged", IsPR: true},
		},
	}
	installGH(t, gh)

	if code, _, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "7d"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	// One extra fetch for the window, not one per pull request.
	if gh.calls != 3 {
		t.Errorf("gh invoked %d times, want 3", gh.calls)
	}
	// The window has to reach gh as a search qualifier. Parsing it and then
	// fetching everything would pass every assertion below on this small fake
	// and drop merges silently on a real repo.
	var search string
	for _, args := range gh.args {
		for i, a := range args {
			if a == "--search" && i+1 < len(args) {
				search = args[i+1]
			}
		}
	}
	if !strings.HasPrefix(search, "merged:>=") {
		t.Errorf("--merged-since did not reach gh as a search qualifier: %q", search)
	}

	// A merged PR carries the pr: scheme and imports done, so it leaves the
	// frontier instead of swamping it.
	_, shown, _ := session(t, "show", "pr:acme/api#77")
	if !strings.Contains(shown, "fix pagination") || !strings.Contains(shown, "done") {
		t.Errorf("a merged PR did not import as a done pr node:\n%s", shown)
	}
	_, ready, _ := session(t, "ls", "--ready", "--ns", "acme/api")
	if strings.Contains(ready, "fix pagination") {
		t.Errorf("a merged PR landed in the frontier:\n%s", ready)
	}
	if !strings.Contains(ready, "N+1") {
		t.Errorf("the open issue is missing from the frontier:\n%s", ready)
	}
}

// The open and merged lists are separate snapshots. A PR can merge between
// them and appear in both; choosing the open copy would create a node that
// stays open forever, because later imports correctly treat upstream as a cache
// rather than authority over the node's own state.
func TestImportPrefersMergedWhenTheWindowsOverlap(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{
		prs:    []importer.Item{{Number: 77, Title: "fix pagination", State: "open", IsPR: true}},
		merged: []importer.Item{{Number: 77, Title: "fix pagination", State: "merged", IsPR: true}},
	})

	if code, _, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "7d"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	_, shown, _ := session(t, "show", "pr:acme/api#77")
	if !strings.Contains(shown, "state  done") {
		t.Errorf("the stale open copy won over the merged copy:\n%s", shown)
	}
}

// Idempotence is the property that makes a scheduled import safe, and the
// merged window is the form most likely to be scheduled — so it is pinned in
// its own right rather than assumed from the open-only case.
func TestImportMergedSinceIsIdempotent(t *testing.T) {
	dir := scratch(t)
	installGH(t, &fakeGH{
		issues: []importer.Item{{Number: 412, Title: "still open", State: "open"}},
		merged: []importer.Item{{Number: 77, Title: "fix pagination", State: "merged", IsPR: true}},
	})
	if code, _, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "7d"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}
	first := logBytes(t, dir)

	if code, _, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "7d"); code != exitOK {
		t.Fatalf("re-import: exit %d, %s", code, stderr)
	}
	if got := logBytes(t, dir); string(got) != string(first) {
		t.Errorf("re-import appended %d bytes; unchanged upstream must write nothing",
			len(got)-len(first))
	}
}

// A truncated merged window has to say so, and must not borrow the open list's
// "newest first" claim — GitHub's search does not order by merge time, so the
// items that fell off the end are not the oldest ones.
func TestImportWarnsWhenTheMergedWindowIsTruncated(t *testing.T) {
	scratch(t)
	var merged []importer.Item
	for i := 1; i <= 4; i++ {
		merged = append(merged, importer.Item{
			Number: i, Title: fmt.Sprintf("pr %d", i), State: "merged", IsPR: true,
		})
	}
	installGH(t, &fakeGH{merged: merged})

	_, _, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "30d", "--limit", "3")
	if !strings.Contains(stderr, "--merged-since") || !strings.Contains(stderr, "--limit") {
		t.Errorf("a truncated merged window did not name both remedies:\n%s", stderr)
	}
	if strings.Contains(stderr, "newest first") {
		t.Errorf("the merged warning claims an ordering search does not have:\n%s", stderr)
	}

	// And it reaches a --json caller, who otherwise reads a clean exit and a
	// short node list as a complete import.
	scratch(t)
	_, stdout, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "30d", "--limit", "3", "--json")
	if !strings.Contains(stderr, "were not seen") {
		t.Errorf("--json swallowed the truncation warning:\n%s", stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("the warning contaminated the JSON payload:\n%s", stdout)
	}
}

func TestImportDoesNotCallAnExactlyFullWindowTruncated(t *testing.T) {
	scratch(t)
	var merged []importer.Item
	for i := 1; i <= 3; i++ {
		merged = append(merged, importer.Item{
			Number: i, Title: fmt.Sprintf("pr %d", i), State: "merged", IsPR: true,
		})
	}
	installGH(t, &fakeGH{merged: merged})

	_, _, stderr := session(t, "import", "gh", "acme/api", "--merged-since", "30d", "--limit", "3")
	if strings.Contains(stderr, "were not seen") {
		t.Errorf("exactly --limit items were reported as an omission:\n%s", stderr)
	}
}

func TestImportUsageErrors(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{})
	cases := [][]string{
		{"import"},
		{"import", "gh"},
		{"import", "gitlab", "acme/api"},
		{"import", "gh", "not-a-repo"},
		{"import", "gh", "acme/api#abc"},
		{"import", "gh", "acme/api", "--kind", "nonsense"},
		{"import", "gh", "acme/api", "--limit", "0"},
		{"import", "gh", "acme/api", "--limit", "-1"},
		{"import", "gh", "acme/api", "--merged-since", "yesterday"},
		// One item has no window to widen. Accepting this would report a
		// complete import of a window that was never fetched.
		{"import", "gh", "acme/api#508", "--merged-since", "7d"},
	}
	for _, args := range cases {
		if code, _, _ := session(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// Flags after positionals (BUILD.md §1.2).
func TestImportParsesFlagsAfterPositionals(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{{Number: 1, Title: "one", State: "open"}}})

	code, stdout, stderr := session(t, "import", "gh", "acme/api", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("--json after positionals was ignored:\n%s", stdout)
	}

	scratch(t)
	if code, _, _ := session(t, "import", "gh", "acme/api", "--kind", "bug"); code != exitOK {
		t.Fatalf("--kind after positionals: exit %d", code)
	}
	_, shown, _ := session(t, "show", "gh:acme/api#1")
	if !strings.Contains(shown, "bug") {
		t.Errorf("--kind after positionals was ignored:\n%s", shown)
	}
}

// Import's collision check holds the same notion of present that resolution
// does: a live holder wins over dropped ones. Without this, a
// drop-and-recreate leaves the dropped predecessor carrying the ref, and every
// scheduled import skips the item forever — and exits 4 on every run.
func TestImportPrefersTheLiveHolderOverADroppedOne(t *testing.T) {
	scratch(t)
	dropped := addNodeFull(t, "first sighting", "issue", "--ref", "gh:acme/api#412")
	if code, _, _ := session(t, "drop", dropped); code != exitOK {
		t.Fatal("drop failed")
	}
	live := addNodeFull(t, "second sighting", "issue", "--ref", "gh:acme/api#412")
	installGH(t, &fakeGH{issues: []importer.Item{{Number: 412, Title: "upstream", State: "open"}}})

	code, _, stderr := session(t, "import", "gh", "acme/api")
	if code != exitOK {
		t.Fatalf("exit %d, want %d: %s", code, exitOK, stderr)
	}
	// The observation landed on the live node, not the dropped predecessor.
	_, shown, _ := session(t, "show", live)
	if !strings.Contains(shown, "observed") {
		t.Errorf("the live holder did not receive the upstream observation:\n%s", shown)
	}
}

// Import mints pr: for PRs and gh: for issues, and its collision check keys
// on the exact ref string — so a hand-added node under the other scheme is
// invisible to it and the duplicate is created regardless (making the schemes
// visible there would make them identity, which SPEC §4.1 refuses). What
// import must do is say so: the same warn-only note add prints, on the bulk
// path where the duplicates multiply.
func TestImportNamesTheTwinSchemeHolder(t *testing.T) {
	scratch(t)
	addNodeFull(t, "708 hand-added", "pr", "--ref", "gh:nooga/let-go#708")
	installGH(t, &fakeGH{prs: []importer.Item{{Number: 708, Title: "upstream PR", State: "open", IsPR: true}}})

	code, _, stderr := session(t, "import", "gh", "nooga/let-go")
	if code != exitOK {
		t.Fatalf("exit %d, want %d: %s", code, exitOK, stderr)
	}
	for _, want := range []string{
		"pr:nooga/let-go#708 names the same thing as gh:nooga/let-go#708",
		"708 hand-added",
		"weft merge",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("twin note missing %q:\n%s", want, stderr)
		}
	}
	// And nothing on a re-run: the node now exists under pr:, so the exact-ref
	// match binds and no new node — and no note — is minted.
	_, _, stderr = session(t, "import", "gh", "nooga/let-go")
	if strings.Contains(stderr, "names the same thing") {
		t.Errorf("re-import repeated the twin note:\n%s", stderr)
	}
}
