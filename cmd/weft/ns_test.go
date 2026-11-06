package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mparrett/weft/internal/ns"
)

// The exit condition: a cross-namespace depends-on edge resolves both ends.
func TestCrossNamespaceEdge(t *testing.T) {
	scratch(t)
	local := addNodeFull(t, "local work", "task", "--ns", "acme/api")
	upstream := addNodeFull(t, "upstream fix", "task", "--ns", "other/fork")

	if code, _, stderr := session(t, "link", local, "depends-on", upstream); code != exitOK {
		t.Fatalf("cross-namespace link: exit %d, %s", code, stderr)
	}

	// Both ends resolve, from either side, regardless of the caller's namespace.
	t.Setenv("WEFT_NS", "acme/api")
	_, shown, _ := session(t, "show", local)
	if !strings.Contains(shown, "upstream fix") {
		t.Errorf("edge to another namespace not shown:\n%s", shown)
	}
	t.Setenv("WEFT_NS", "other/fork")
	_, reverse, _ := session(t, "show", upstream)
	if !strings.Contains(reverse, "local work") {
		t.Errorf("inbound edge from another namespace not shown:\n%s", reverse)
	}
	// And the dependent is found wherever it lives.
	_, deps, _ := session(t, "rdeps", upstream)
	if !strings.Contains(deps, "local work") {
		t.Errorf("rdeps did not cross the namespace boundary:\n%s", deps)
	}
}

// WEFT_NS is how an agent pins its context, and it must reach `add`.
func TestNamespaceFromEnvironment(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "pinned/context")
	a := addNode(t, "in the pinned namespace", "task")

	_, listing, _ := session(t, "ls")
	if !strings.Contains(listing, "in the pinned namespace") {
		t.Errorf("node not listed in its own namespace:\n%s", listing)
	}
	t.Setenv("WEFT_NS", "somewhere/else")
	if _, other, _ := session(t, "ls"); strings.Contains(other, "in the pinned namespace") {
		t.Errorf("node leaked into another namespace:\n%s", other)
	}
	// But it is still addressable by id from anywhere: namespace is a field,
	// never part of identity.
	if code, _, _ := session(t, "show", a); code != exitOK {
		t.Error("a node became unaddressable from another namespace")
	}
}

// An explicit --ns beats the resolved default.
func TestExplicitNamespaceFlagWins(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "resolved/default")
	addNodeFull(t, "explicitly placed", "task", "--ns", "chosen/place")

	if _, here, _ := session(t, "ls"); strings.Contains(here, "explicitly placed") {
		t.Errorf("--ns was ignored on add:\n%s", here)
	}
	if _, there, _ := session(t, "ls", "--ns", "chosen/place"); !strings.Contains(there, "explicitly placed") {
		t.Errorf("node not in the namespace it was placed in:\n%s", there)
	}
}

func TestAllNamespacesWidensListing(t *testing.T) {
	scratch(t)
	addNodeFull(t, "first", "task", "--ns", "one/place")
	addNodeFull(t, "second", "task", "--ns", "another/place")

	t.Setenv("WEFT_NS", "one/place")
	_, scoped, _ := session(t, "ls")
	if strings.Contains(scoped, "second") {
		t.Errorf("scoped listing leaked:\n%s", scoped)
	}
	_, all, _ := session(t, "ls", "--all-ns")
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(all, want) {
			t.Errorf("--all-ns missing %q:\n%s", want, all)
		}
	}
	// It widens the filtered listings too, not just the bare one.
	_, allReady, _ := session(t, "ls", "--ready", "--all-ns")
	if !strings.Contains(allReady, "second") {
		t.Errorf("--all-ns did not widen --ready:\n%s", allReady)
	}
}

// An empty listing beside a non-empty store almost always means the namespace
// resolved somewhere unexpected, which is otherwise invisible.
func TestEmptyNamespaceHintsAtAllNS(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "where/things/are")
	addNode(t, "somewhere", "task")

	t.Setenv("WEFT_NS", "where/nothing/is")
	code, stdout, stderr := session(t, "ls")
	if code != exitOK {
		t.Fatalf("an empty namespace is not an error, got exit %d", code)
	}
	if stdout != "" {
		t.Errorf("expected no nodes listed, got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "--all-ns") {
		t.Errorf("expected a hint on stderr, got:\n%s", stderr)
	}
	// No hint when the store really is empty — there would be nothing to find.
	scratch(t)
	t.Setenv("WEFT_NS", "anything")
	if _, _, stderr := session(t, "ls"); stderr != "" {
		t.Errorf("empty store should not hint:\n%s", stderr)
	}
}

// Node-rooted queries never take a namespace filter. Filtering them would
// reproduce exactly the failure the filter-vs-traversal rule prevents.
func TestNodeRootedQueriesAreAlwaysGlobal(t *testing.T) {
	scratch(t)
	local := addNodeFull(t, "local work", "task", "--ns", "acme/api")
	upstream := addNodeFull(t, "upstream blocker", "task", "--ns", "other/fork")
	session(t, "link", local, "depends-on", upstream)

	t.Setenv("WEFT_NS", "acme/api")
	_, why, _ := session(t, "why", local)
	if !strings.Contains(why, "upstream blocker") {
		t.Errorf("why hid a cross-namespace blocker:\n%s", why)
	}
	_, route, _ := session(t, "path", local, upstream)
	if !strings.Contains(route, "upstream blocker") {
		t.Errorf("path stopped at a namespace boundary:\n%s", route)
	}
}

// `weft ns` exists so the namespace question can be asked without answering it
// by writing a node and seeing where it lands. A read-only question that
// mutates the store is one people stop asking.
func TestNSReportsTheEnvironment(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "pinned/by/env")

	code, stdout, _ := session(t, "ns")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "pinned/by/env") {
		t.Errorf("namespace missing from output:\n%s", stdout)
	}
	// The rule matters as much as the name: the same namespace from a config
	// mapping and from a stray $WEFT_NS call for different fixes.
	if !strings.Contains(stdout, string(ns.SourceEnv)) {
		t.Errorf("expected the source to be named:\n%s", stdout)
	}
}

// A config mapping must report the prefix that matched, not just the value it
// produced, so a wrong namespace points at the line responsible.
func TestNSReportsTheConfigLineThatMatched(t *testing.T) {
	dir := scratch(t)
	os.Unsetenv("WEFT_NS")
	t.Setenv("WEFT_NS", "")

	work := t.TempDir()
	conf := "ns:" + work + " = mapped/here\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	code, stdout, _ := session(t, "ns")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "mapped/here") {
		t.Errorf("config mapping did not take effect:\n%s", stdout)
	}
	if !strings.Contains(stdout, work) {
		t.Errorf("expected the matched prefix in the output:\n%s", stdout)
	}
}

// The reported prefix must be the line as *written*, not its symlink target.
// The detail exists so a wrong namespace points at the line responsible, and
// naming a path that appears nowhere in the file sends the reader hunting for
// text that is not there. This went unnoticed because the sibling test uses a
// plain TempDir, where macOS's /var -> /private/var passes by substring luck.
func TestNSReportsTheConfigLineAsWritten(t *testing.T) {
	dir := scratch(t)
	t.Setenv("WEFT_NS", "")

	root := t.TempDir()
	real := filepath.Join(root, "real-repo")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "via-symlink")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	conf := "ns:" + link + " = mapped/here\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(real) // arrive by the physical path; the mapping names the link

	_, stdout, _ := session(t, "ns")
	if !strings.Contains(stdout, "mapped/here") {
		t.Fatalf("mapping did not take effect through the symlink:\n%s", stdout)
	}
	if !strings.Contains(stdout, link) {
		t.Errorf("reported a path that is not in the config file; want %q:\n%s", link, stdout)
	}
}

func TestNSJSON(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "some/ns")

	code, stdout, _ := session(t, "ns", "--json")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		NS     string `json:"ns"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if got.NS != "some/ns" || got.Source != string(ns.SourceEnv) {
		t.Errorf("got ns=%q source=%q", got.NS, got.Source)
	}
}

// Across namespaces the rows are otherwise indistinguishable — two nodes with
// the same title in different projects render identically without the column.
func TestLsAllNSShowsANamespaceColumn(t *testing.T) {
	scratch(t)
	addNodeFull(t, "same title", "task", "--ns", "acme/api")
	addNodeFull(t, "same title", "task", "--ns", "other/fork")

	_, stdout, _ := session(t, "ls", "--all-ns")
	for _, want := range []string{"acme/api", "other/fork"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("namespace %q missing from --all-ns listing:\n%s", want, stdout)
		}
	}
	// A single-namespace listing repeats one value on every row, so it stays off.
	_, plain, _ := session(t, "ls", "--ns", "acme/api")
	if strings.Contains(plain, "acme/api") {
		t.Errorf("namespace column should be off without --all-ns:\n%s", plain)
	}
}

// The two empty-listing states get opposite advice. A filter that
// empties a namespace which holds other work must not blame the namespace or
// send the reader to --all-ns; it points back into the graph. A namespace that
// is genuinely empty here, with matches elsewhere, is the wrong-namespace case,
// and its count must be of what the namespace filter excluded.
func TestEmptyListingHintIsAccurate(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "acme/api")
	addNode(t, "one", "task")
	addNode(t, "two", "task")
	addNodeFull(t, "three", "task", "--ns", "other/lib")

	// Emptied by --kind in a namespace that holds work: name the filter, point
	// into the graph, and do not blame the namespace or suggest --all-ns.
	_, _, stderr := session(t, "ls", "--kind", "bug")
	if !strings.Contains(stderr, "it holds other work") {
		t.Errorf("a filter-caused empty listing should say the namespace has work:\n%s", stderr)
	}
	if strings.Contains(stderr, "--all-ns") {
		t.Errorf("a filter-caused empty listing must not blame the namespace:\n%s", stderr)
	}

	// Emptied by the namespace itself: hint, counting only what that filter
	// excluded, and steering to the namespace remedies.
	t.Setenv("WEFT_NS", "empty/ns")
	_, _, stderr = session(t, "ls")
	if !strings.Contains(stderr, `no nodes in namespace "empty/ns"`) {
		t.Errorf("an empty namespace should say so:\n%s", stderr)
	}
	if !strings.Contains(stderr, "3 elsewhere") {
		t.Errorf("expected the count of nodes excluded by the namespace filter:\n%s", stderr)
	}
}

// A namespace with nodes but none ready is not an empty namespace, and it calls
// for the opposite advice. The hint used to say "no nodes in
// namespace X (… try --all-ns …)", which is both a false statement about a
// namespace that has work and a steer toward namespace remedies for a problem
// that is not about the namespace. It should name the predicate and point back
// into the graph.
func TestReadyEmptyHintPointsIntoTheGraph(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "acme/api")
	// acme/api has one node, and it is blocked by upstream work — so the
	// namespace has a node, but nothing in it is ready.
	local := addNode(t, "ship the API", "task")
	upstream := addNodeFull(t, "upstream lib fix", "task", "--ns", "other/lib")
	if code, _, stderr := session(t, "link", local, "depends-on", upstream); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}

	code, stdout, stderr := session(t, "ls", "--ready")
	if code != exitOK {
		t.Fatalf("ls --ready: exit %d, %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("nothing in acme/api is ready, so stdout should be empty:\n%s", stdout)
	}
	if strings.Contains(stderr, `no nodes in namespace "acme/api"`) {
		t.Errorf("hint claims the namespace is empty when it has a blocked node:\n%s", stderr)
	}
	if !strings.Contains(stderr, `no ready nodes in namespace "acme/api"`) {
		t.Errorf("hint should name the predicate that emptied the listing:\n%s", stderr)
	}
	// The namespace is fine, so the advice is graph-shaped, not --all-ns.
	if !strings.Contains(stderr, "weft why") {
		t.Errorf("hint should point back into the graph (`weft why`):\n%s", stderr)
	}
	if strings.Contains(stderr, "--all-ns") {
		t.Errorf("a namespace that holds work must not be sent to --all-ns:\n%s", stderr)
	}
}

// The blocker lives in another namespace, which is the whole point of the
// answer — "you are blocked by work in a repo you were not thinking about".
func TestCrossNamespaceNeighboursAreMarked(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_NS", "acme/api")
	local := addNode(t, "ship the API", "task")
	upstream := addNodeFull(t, "upstream lib fix", "task", "--ns", "other/lib")
	if code, _, stderr := session(t, "link", local, "depends-on", upstream); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}

	// The marker names where the *neighbour* lives, so it differs by command:
	// rdeps is rooted at the blocker and looks back the other way.
	for _, tc := range []struct{ cmd, root, want string }{
		{"show", local, "[other/lib]"},
		{"why", local, "[other/lib]"},
		{"rdeps", upstream, "[acme/api]"},
	} {
		_, stdout, _ := session(t, tc.cmd, tc.root)
		if !strings.Contains(stdout, tc.want) {
			t.Errorf("%s did not mark the neighbour %s:\n%s", tc.cmd, tc.want, stdout)
		}
	}
	// A same-namespace neighbour must stay unannotated, or the marker is noise.
	sibling := addNode(t, "sibling work", "task")
	session(t, "link", local, "depends-on", sibling)
	if _, stdout, _ := session(t, "show", local); strings.Contains(stdout, "[acme/api]") {
		t.Errorf("annotated a neighbour in the same namespace:\n%s", stdout)
	}
}
