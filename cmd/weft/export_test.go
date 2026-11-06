package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mparrett/weft/internal/importer"
)

// One fold, not one per node. Rendering a graph from a `show` per node is N
// folds at N instants, so a write landing mid-loop yields a picture that never
// existed — and the cost grows with the node count.
func TestExportCarriesNodesAndEdgesTogether(t *testing.T) {
	scratch(t)
	a := addNode(t, "the work", "task")
	b := addNode(t, "the blocker", "bug")
	if code, _, stderr := session(t, "link", a, "depends-on", b); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}

	code, stdout, _ := session(t, "export", "--json")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		Nodes []struct{ ID, Title string }
		Edges []struct{ Kind, From, To string }
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(got.Nodes) != 2 {
		t.Errorf("want 2 nodes, got %d", len(got.Nodes))
	}
	if len(got.Edges) != 1 || got.Edges[0].Kind != "depends-on" {
		t.Fatalf("want one depends-on edge, got %+v", got.Edges)
	}
	// Full ids, never the display prefix: a consumer that stores a short id has
	// stored something that can later become ambiguous.
	if len(got.Edges[0].From) != 64 {
		t.Errorf("edge endpoints must be full ids, got %q", got.Edges[0].From)
	}
}

// Export is whole-store. An edge may cross namespaces, so a scoped export would
// emit edges pointing at nodes it excluded — the filter-versus-traversal
// failure the namespace rules exist to prevent.
func TestExportIsNeverNamespaceScoped(t *testing.T) {
	scratch(t)
	local := addNodeFull(t, "local", "task", "--ns", "acme/api")
	upstream := addNodeFull(t, "upstream", "task", "--ns", "other/fork")
	session(t, "link", local, "depends-on", upstream)

	t.Setenv("WEFT_NS", "acme/api")
	_, stdout, _ := session(t, "export", "--json")
	for _, want := range []string{"local", "upstream"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("export dropped %q while scoped to acme/api:\n%s", want, stdout)
		}
	}
}

// Identical graphs must print byte-identically (SPEC §9.5). Edges live in a map
// and symmetric edges are reachable from both ends, so ordering them is a real
// decision rather than an incidental one.
func TestExportIsDeterministic(t *testing.T) {
	scratch(t)
	a := addNode(t, "a", "task")
	b := addNode(t, "b", "task")
	c := addNode(t, "c", "task")
	session(t, "link", a, "depends-on", b)
	session(t, "link", c, "conflicts-with", a)
	session(t, "link", b, "part-of", c)

	_, first, _ := session(t, "export")
	for i := 0; i < 3; i++ {
		if _, again, _ := session(t, "export"); again != first {
			t.Fatalf("export is not deterministic on run %d", i+2)
		}
	}
	if !strings.HasPrefix(first, "digraph weft {") {
		t.Errorf("not DOT:\n%.80s", first)
	}
}

// Titles are free text. An unescaped quote produces a file graphviz rejects,
// which reads as "weft is broken" rather than "that title had a quote in it".
func TestExportEscapesTitlesForDOT(t *testing.T) {
	scratch(t)
	addNode(t, `a "quoted" title with a \ backslash`, "task")

	_, stdout, _ := session(t, "export")
	if strings.Contains(stdout, `label="`+"`") {
		t.Error("label quoting looks wrong")
	}
	if !strings.Contains(stdout, `\"quoted\"`) {
		t.Errorf("quotes were not escaped:\n%s", stdout)
	}
	if !strings.Contains(stdout, `\\ backslash`) {
		t.Errorf("backslash was not escaped:\n%s", stdout)
	}
}

// A ref is free text and any scheme is accepted, so link construction must
// match exactly. A guessed link that 404s is worse than no link: it looks
// authoritative, and the reader blames the target rather than the guess.
func TestRefURL(t *testing.T) {
	cases := map[string]string{
		"gh:mparrett/weft#12":   "https://github.com/mparrett/weft/issues/12",
		"pr:mparrett/weft#45":   "https://github.com/mparrett/weft/pull/45",
		"gh:acme/api.v2#1":      "https://github.com/acme/api.v2/issues/1",
		"branch:fix/n-plus-1":   "",
		"file:docs/export.md":   "",
		"note:head-to-head":     "",
		"jira:AB-12":            "",
		"gh:not-a-repo#12":      "",
		"gh:acme/api#notanum":   "",
		"gh:acme/api#12 trail":  "",
		"gh:acme/api/extra#12":  "",
		"https://example.com/x": "",
		"":                      "",
	}
	for ref, want := range cases {
		if got := refURL(ref); got != want {
			t.Errorf("refURL(%q) = %q, want %q", ref, got, want)
		}
	}
}

// The DOT gains a hyperlink only where a ref supports one; every other node
// must stay link-free rather than pointing somewhere invented.
func TestExportDOTLinksOnlyWhatItCan(t *testing.T) {
	scratch(t)
	addNodeFull(t, "upstream issue", "issue", "--ref", "gh:mparrett/weft#12")
	addNodeFull(t, "local note", "finding", "--ref", "note:local-only")

	_, dot, _ := session(t, "export")
	if !strings.Contains(dot, `URL="https://github.com/mparrett/weft/issues/12"`) {
		t.Errorf("no link for a gh: ref:\n%s", dot)
	}
	if strings.Count(dot, "URL=") != 1 {
		t.Errorf("want exactly one linked node, got %d:\n%s", strings.Count(dot, "URL="), dot)
	}
}

// The default shape is the contract that golden files rest on, so --include
// must be the only thing that changes it. Asserted by comparing the two runs
// rather than by eyeballing a golden: a section that leaked into the default
// would still match a golden someone regenerated.
func TestExportDefaultIsUnchangedByTheFlagExisting(t *testing.T) {
	scratch(t)
	id := addNodeFull(t, "a task", "task", "--ref", "gh:acme/api#1")
	session(t, "claim", id, "--note", "on it")

	_, plain, _ := session(t, "export", "--json")
	for _, absent := range []string{"claims", "source", "observed_at", "upstream_state"} {
		if strings.Contains(plain, absent) {
			t.Errorf("the default export carries %q; it must stay byte-identical:\n%s", absent, plain)
		}
	}
	// Refs stay bare strings by default, which is the shape every existing
	// consumer parses.
	var out struct {
		Nodes []struct {
			Refs []any `json:"refs"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes[0].Refs) != 1 {
		t.Fatalf("got %d refs", len(out.Nodes[0].Refs))
	}
	if _, ok := out.Nodes[0].Refs[0].(string); !ok {
		t.Errorf("default refs are not bare strings: %T", out.Nodes[0].Refs[0])
	}
}

// Every judgment-quality signal the graph records has to survive the boundary
// where it becomes something a person looks at (SPEC.md §4.4). Before this, an
// inferred edge exported identically to an asserted one.
func TestExportIncludeCarriesProvenance(t *testing.T) {
	scratch(t)
	a := addNodeFull(t, "the task", "task", "--ref", "gh:acme/api#1")
	b := addNodeFull(t, "the blocker", "bug")
	if code, _, stderr := session(t, "link", a, "depends-on", b, "--source", "inferred"); code != exitOK {
		t.Fatalf("link: exit %d, %s", code, stderr)
	}
	session(t, "claim", a, "--holder", "user:colleague", "--source", "inferred", "--note", "per issue comment")

	_, out, _ := session(t, "export", "--json", "--include", "all")
	var got struct {
		Nodes []struct {
			ID   string `json:"id"`
			Refs []struct {
				Ref           string `json:"ref"`
				ObservedAt    string `json:"observed_at"`
				UpstreamState string `json:"upstream_state"`
			} `json:"refs"`
			Claims []struct {
				Holder, By, Source, Note string
			} `json:"claims"`
		} `json:"nodes"`
		Edges []struct {
			Kind, From, To, By, Source, At string
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	// An inferred edge is distinguishable from an asserted one, which is the
	// README's promise and the reason provenance exists at all.
	if len(got.Edges) != 1 || got.Edges[0].Source != "inferred" {
		t.Errorf("edge provenance missing: %+v", got.Edges)
	}
	if got.Edges[0].By == "" || got.Edges[0].At == "" {
		t.Errorf("edge carries source but not who or when: %+v", got.Edges[0])
	}

	var node *struct {
		ID   string `json:"id"`
		Refs []struct {
			Ref           string `json:"ref"`
			ObservedAt    string `json:"observed_at"`
			UpstreamState string `json:"upstream_state"`
		} `json:"refs"`
		Claims []struct {
			Holder, By, Source, Note string
		} `json:"claims"`
	}
	for i := range got.Nodes {
		if strings.HasPrefix(got.Nodes[i].ID, a) {
			node = &got.Nodes[i]
		}
	}
	if node == nil {
		t.Fatalf("node %s missing from the export:\n%s", a, out)
	}
	// "Who is already on this" is the question the detail panel could not
	// answer, and an inferred claim about someone else is exactly the one a
	// reader has to be able to discount.
	if len(node.Claims) != 1 || node.Claims[0].Holder != "user:colleague" ||
		node.Claims[0].Source != "inferred" {
		t.Errorf("claim provenance missing: %+v", node.Claims)
	}
	if node.Claims[0].By == node.Claims[0].Holder {
		t.Errorf("by and holder collapsed; they routinely differ: %+v", node.Claims[0])
	}
	// A ref nobody looked at says so by omission rather than by an empty state
	// that would read as "upstream reported nothing".
	if len(node.Refs) != 1 || node.Refs[0].Ref != "gh:acme/api#1" {
		t.Fatalf("refs did not become objects: %+v", node.Refs)
	}
	if node.Refs[0].ObservedAt != "" || node.Refs[0].UpstreamState != "" {
		t.Errorf("an unobserved ref invented an observation: %+v", node.Refs[0])
	}
}

// An observation is only judgeable with its age attached (EVENTS.md §2.5).
func TestExportIncludeRefsCarriesTheObservationAge(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{issues: []importer.Item{
		{Number: 412, Title: "upstream title", State: "closed"},
	}})
	if code, _, stderr := session(t, "import", "gh", "acme/api"); code != exitOK {
		t.Fatalf("import: exit %d, %s", code, stderr)
	}

	_, out, _ := session(t, "export", "--json", "--include", "refs")
	var got struct {
		Nodes []struct {
			Refs []struct {
				Ref           string `json:"ref"`
				ObservedAt    string `json:"observed_at"`
				UpstreamState string `json:"upstream_state"`
				UpstreamTitle string `json:"upstream_title"`
			} `json:"refs"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	r := got.Nodes[0].Refs[0]
	if r.ObservedAt == "" {
		t.Errorf("an observation exported without its age: %+v", r)
	}
	if r.UpstreamState != "closed" || r.UpstreamTitle != "upstream title" {
		t.Errorf("upstream cache not carried: %+v", r)
	}
}

type releasedExport struct {
	Nodes []struct {
		ID     string `json:"id"`
		Claims []struct {
			Holder, Note string
		} `json:"claims"`
		Released []struct {
			Holder         string `json:"holder"`
			Note           string `json:"note"`
			At             string `json:"at"`
			ReleasedAt     string `json:"released_at"`
			ReleasedBy     string `json:"released_by"`
			ReleasedSource string `json:"released_source"`
		} `json:"released_claims"`
	} `json:"nodes"`
}

func exportReleased(t *testing.T) releasedExport {
	t.Helper()
	code, out, stderr := session(t, "export", "--json", "--include", "claims")
	if code != exitOK {
		t.Fatalf("export: exit %d, %s", code, stderr)
	}
	var got releasedExport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return got
}

// A session that attached, released and re-attached must read as two
// intervals, [at, released_at) and a live claim, so a reader can rebuild who
// had the work when. The note carries the attach role, so it has to survive
// the release; and `claims` keeps meaning "current", or every existing reader
// shows a released holder as still on the work.
func TestExportKeepsReleasedClaimsApart(t *testing.T) {
	scratch(t)
	_, out, _ := session(t, "add", "continuity", "--kind", "workstream")
	id := strings.Fields(out)[0]
	mustRun := func(args ...string) {
		t.Helper()
		if code, _, stderr := session(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}
	mustRun("claim", id, "--holder", "claude:one", "--note", "canonical")
	mustRun("claim", id, "--holder", "claude:two", "--note", "ancillary")
	// Released out of the order they were taken: the export must still list
	// them in claim order.
	mustRun("release", id, "--holder", "claude:two")
	mustRun("release", id, "--holder", "claude:one")
	mustRun("claim", id, "--holder", "claude:one", "--note", "canonical")

	got := exportReleased(t)
	if len(got.Nodes) != 1 {
		t.Fatalf("want one node, got %d", len(got.Nodes))
	}
	n := got.Nodes[0]
	if len(n.Claims) != 1 || n.Claims[0].Holder != "claude:one" {
		t.Errorf("claims must hold only the live re-attach: %+v", n.Claims)
	}
	if len(n.Released) != 2 || n.Released[0].Holder != "claude:one" || n.Released[1].Holder != "claude:two" {
		t.Fatalf("released claims missing or out of claim order: %+v", n.Released)
	}
	for _, r := range n.Released {
		if r.At == "" || r.ReleasedAt == "" || r.ReleasedAt < r.At {
			t.Errorf("an interval needs both ends, in order: %+v", r)
		}
		if r.ReleasedBy == "" || r.ReleasedSource == "" {
			t.Errorf("the release's own provenance is missing: %+v", r)
		}
	}
	if n.Released[0].Note != "canonical" || n.Released[1].Note != "ancillary" {
		t.Errorf("the note (attach role) must survive the release: %+v", n.Released)
	}
}

// A merge moves everything the survivor now owns; released claims missed here
// would drop the duplicate's intervals from export with nothing failing.
func TestMergeCarriesReleasedClaims(t *testing.T) {
	scratch(t)
	_, out, _ := session(t, "add", "survivor", "--kind", "workstream")
	into := strings.Fields(out)[0]
	_, out, _ = session(t, "add", "duplicate", "--kind", "workstream")
	from := strings.Fields(out)[0]
	session(t, "claim", from, "--holder", "claude:old")
	session(t, "release", from, "--holder", "claude:old")
	if code, _, stderr := session(t, "merge", into, from); code != exitOK {
		t.Fatalf("merge: exit %d, %s", code, stderr)
	}
	for _, n := range exportReleased(t).Nodes {
		if !strings.HasPrefix(n.ID, into) {
			continue
		}
		if len(n.Released) != 1 || n.Released[0].Holder != "claude:old" {
			t.Errorf("the survivor lost the duplicate's released claim: %+v", n.Released)
		}
		return
	}
	t.Fatal("survivor missing from export")
}

func TestExportIncludeUsageErrors(t *testing.T) {
	scratch(t)
	for _, args := range [][]string{
		{"export", "--json", "--include", "nonsense"},
		{"export", "--json", "--include", "claims,nonsense"},
		// DOT has nowhere to put provenance; accepting it would report a richer
		// export than was written.
		{"export", "--include", "claims"},
		{"export", "--d2", "--include", "claims"},
		{"export", "--json", "--d2"},
		// --flat only changes how d2 draws part-of; elsewhere it would be
		// silently ignored.
		{"export", "--flat"},
		{"export", "--json", "--flat"},
	} {
		if code, _, _ := session(t, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
