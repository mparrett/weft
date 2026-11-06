package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
)

var update = flag.Bool("update", false, "rewrite golden files")

// golden pins rendered output byte for byte (SPEC.md §9.5). Nothing else fails
// when attribute order or column alignment drifts — the git diffs just go noisy
// months later.
//
// TestExportIsDeterministic is not a substitute: it compares four runs of the
// same binary against each other, so a refactor that reorders every line
// consistently passes it and changes the artifact.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden (run: go test ./cmd/weft -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// exportSample is a hand-built graph rather than a real store, because node ids
// are content hashes over an event that includes its timestamp — a store built
// during the test would produce different ids on every run and no golden could
// ever match. Fixed ids move the thing under test from "does hashing work" to
// "does the format hold still", which is what §9.5 is about.
//
// It deliberately covers every branch the renderers have: all three states, all
// three styled edge kinds, a linkable ref and an unlinkable one, a title
// carrying characters that must be escaped, and an edge whose target is not in
// the store.
func exportSample() ([]*graph.Node, *graph.Graph) {
	id := func(c byte) string { return string(bytes.Repeat([]byte{c}, 64)) }

	// Fixed timestamps, for the same reason the ids are fixed. The third node
	// deliberately carries none: a graph assembled in memory rather than folded
	// from a log has no timestamps, and the omitempty branch is what keeps that
	// from exporting as year 1.
	created := time.Date(2026, 7, 30, 9, 15, 0, 0, time.UTC)
	touched := time.Date(2026, 8, 4, 17, 40, 30, 0, time.UTC)

	// The first two nodes carry provenance, which every log-folded node has —
	// without it the default-export golden proves nothing about the --include
	// gate, since dropping the gate would change nothing either way. With it,
	// deleting the gate moves the default bytes and the golden fails.
	nodes := []*graph.Node{
		{ID: id('a'), NS: "acme/api", Kind: "bug", Title: "N+1 in report generation", State: "open",
			By: "agent:scanner", Source: "inferred",
			Refs: []string{"gh:acme/api#412"}, Created: created, Touched: touched},
		{ID: id('b'), NS: "acme/api", Kind: "spec", Title: `the "batch export" spec\draft`, State: "done",
			By: "user:matt", Source: "asserted",
			Refs: []string{"file:docs/export.md", "pr:acme/api#77"}, Created: created, Touched: created},
		{ID: id('c'), NS: "other/fork", Kind: "finding", Title: "review point Z", State: "dropped"},
	}

	edge := func(k, from, to string) *graph.Edge {
		return &graph.Edge{ID: from[:8] + to[:8] + k, Kind: k, From: from, To: to}
	}
	// graph.New rather than a literal: the observation store is unexported and
	// only reachable by folding events, and a literal graph's nil maps would
	// panic under Apply.
	g := graph.New()
	for _, n := range nodes {
		g.Nodes[n.ID] = n
	}
	// Observations for the --include refs golden: a gh: ref carrying the
	// upstream clock, and a file: ref carrying stamps and a hash but no state —
	// the two honest shapes EVENTS.md §3.8 defines. Without them the refs
	// golden would pin only the never-observed branch and prove nothing about
	// the payload fields, the shape the by/source fold fix once shipped with.
	for _, o := range [][2]string{
		{id('a'), "gh:acme/api#412"},
		{id('b'), "file:docs/export.md"},
	} {
		p := event.ObservePayload{
			Created:  "2026-05-01T09:00:00Z",
			Modified: "2026-08-01T12:30:00Z",
		}
		if o[1] == "gh:acme/api#412" {
			p.State, p.Title = "closed", "N+1 in report generation"
		} else {
			p.Hash = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
		}
		ev, err := event.RefObserve(o[0], o[1], p, "agent:import", event.SourceObserved, touched)
		if err != nil {
			panic(err)
		}
		if err := g.Apply(ev); err != nil {
			panic(err)
		}
	}
	for _, e := range []*graph.Edge{
		edge("depends-on", id('a'), id('b')),
		edge("part-of", id('c'), id('a')),
		edge("conflicts-with", id('a'), id('c')),
		// A dangling endpoint is a normal state (SPEC.md §9.3) and is the one
		// case sortedEdges' rank fallback exists for; nothing else in the suite
		// can produce it, since `weft link` resolves both ends.
		edge("depends-on", id('b'), id('f')),
	} {
		g.Edges[e.ID] = e
	}
	return nodes, g
}

func TestExportDOTGolden(t *testing.T) {
	nodes, g := exportSample()
	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := exportDOT(e, nodes, sortedEdges(g, nodes), render.MinWidth); err != nil {
		t.Fatal(err)
	}
	golden(t, "export.dot", e.stdout.(*bytes.Buffer).Bytes())
}

// The sample's part-of edge (c inside a) and its conflicts-with edge between the
// same pair pin the hard case: an arrow between a container and its own child.
func TestExportD2Golden(t *testing.T) {
	for _, tc := range []struct {
		name       string
		containers bool
	}{{"export.d2", true}, {"export_flat.d2", false}} {
		nodes, g := exportSample()
		e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
		if err := exportD2(e, nodes, sortedEdges(g, nodes), render.MinWidth, tc.containers); err != nil {
			t.Fatal(err)
		}
		golden(t, tc.name, e.stdout.(*bytes.Buffer).Bytes())
	}
}

// The golden is only useful if d2 accepts it: a format pinned byte for byte can
// still be one d2 refuses to compile. d2 is not a build dependency, so the
// check runs where it is installed and skips elsewhere.
func TestExportD2GoldensCompile(t *testing.T) {
	d2, err := exec.LookPath("d2")
	if err != nil {
		t.Skip("d2 not installed")
	}
	for _, name := range []string{"export.d2", "export_flat.d2"} {
		out := filepath.Join(t.TempDir(), "out.svg")
		if b, err := exec.Command(d2, filepath.Join("testdata", name), out).CombinedOutput(); err != nil {
			t.Errorf("d2 rejects %s: %v\n%s", name, err, b)
		}
	}
}

// A second part-of parent must still reach the picture, as an arrow; a `$` in a
// title must not make the file uncompilable; and a part-of cycle in the log,
// which the write path forbids but the fold tolerates, must not hang the
// renderer.
func TestExportD2EdgeCases(t *testing.T) {
	id := func(c byte) string { return string(bytes.Repeat([]byte{c}, 64)) }
	nodes := []*graph.Node{
		{ID: id('a'), NS: "x", Kind: "task", Title: "costs $5, see ${HOME}", State: "open"},
		{ID: id('b'), NS: "x", Kind: "workstream", Title: "first parent", State: "open"},
		{ID: id('c'), NS: "x", Kind: "workstream", Title: "second parent", State: "open"},
	}
	g := graph.New()
	for _, n := range nodes {
		g.Nodes[n.ID] = n
	}
	for _, ed := range []*graph.Edge{
		{ID: "1", Kind: "part-of", From: id('a'), To: id('b')},
		{ID: "2", Kind: "part-of", From: id('a'), To: id('c')},
		{ID: "3", Kind: "part-of", From: id('b'), To: id('c')},
		{ID: "4", Kind: "part-of", From: id('c'), To: id('b')},
		// Neither endpoints nor kinds are validated by the fold, so a log from
		// another writer can carry d2 syntax in both. Unquoted, `#` starts a
		// comment, `.` nests, and `$` or `"` breaks the file.
		{ID: "5", Kind: "depends-on", From: id('a'), To: "gh:acme/api#12"},
		{ID: "6", Kind: `odd$"kind`, From: id('b'), To: `foo.bar{"x"}`},
	} {
		g.Edges[ed.ID] = ed
	}
	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := exportD2(e, nodes, sortedEdges(g, nodes), render.MinWidth, true); err != nil {
		t.Fatal(err)
	}
	out := e.stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, `costs \$5, see \${HOME}`) {
		t.Errorf("$ must be escaped for d2:\n%s", out)
	}
	if strings.Count(out, `: "part-of"`) != 2 {
		t.Errorf("the second parent and the cycle-closing edge must be drawn as arrows:\n%s", out)
	}
	for _, want := range []string{`-> "gh:acme/api#12": "depends-on"`, `-> "foo.bar{\"x\"}": "odd\$\"kind"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s:\n%s", want, out)
		}
	}

	d2, err := exec.LookPath("d2")
	if err != nil {
		return
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "edge.d2")
	if err := os.WriteFile(in, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	svg := filepath.Join(dir, "edge.svg")
	if b, err := exec.Command(d2, in, svg).CombinedOutput(); err != nil {
		t.Fatalf("d2 rejects the edge cases: %v\n%s\n%s", err, b, out)
	}
	drawn, _ := os.ReadFile(svg)
	for _, label := range []string{"gh:acme/api#12", "foo.bar{&#34;x&#34;}"} {
		if !strings.Contains(string(drawn), label) {
			t.Errorf("d2 did not draw the dangling endpoint %s as one shape", label)
		}
	}
}

func TestExportJSONGolden(t *testing.T) {
	nodes, g := exportSample()
	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := exportJSON(e, g, nodes, sortedEdges(g, nodes), include{}); err != nil {
		t.Fatal(err)
	}
	golden(t, "export.json", e.stdout.(*bytes.Buffer).Bytes())
}

// The refs shape is pinned with observations behind it: one gh: ref carrying
// the upstream clock and state, one file: ref carrying stamps and a hash, and
// one never-observed ref that must stay a bare object. A sample with no
// observations would pin only the omission branch and prove nothing about the
// payload fields (SPEC §6.4).
func TestExportJSONIncludeRefsGolden(t *testing.T) {
	nodes, g := exportSample()
	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := exportJSON(e, g, nodes, sortedEdges(g, nodes), include{refs: true}); err != nil {
		t.Fatal(err)
	}
	golden(t, "export_refs.json", e.stdout.(*bytes.Buffer).Bytes())
}

// The opt-in shape is pinned too: --include source is the documented home of
// node provenance (SPEC §6.4), and a golden is what notices the field name or
// placement drifting — the CLI tests only substring-match it.
// Claims folded from real events, with fixed times, so the ids hold still. Two
// claims are released in the reverse of the order they were taken, and the
// first holder takes the work again, so the golden pins claim-order output,
// the released fields, and that a live claim never carries them.
func TestExportJSONIncludeClaimsGolden(t *testing.T) {
	nodes, g := exportSample()
	a := nodes[0].ID
	at := func(min int) time.Time { return time.Date(2026, 8, 1, 9, min, 0, 0, time.UTC) }
	apply := func(ev event.Event, err error) event.Event {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Apply(ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	one := apply(event.ClaimAdd(a, "claude:one", "canonical", "agent:ws", event.SourceAsserted, at(0), at(600)))
	two := apply(event.ClaimAdd(a, "claude:two", "ancillary", "agent:ws", event.SourceAsserted, at(5), at(600)))
	apply(event.ClaimRelease(two.ID(), "agent:ws", event.SourceObserved, at(20)))
	apply(event.ClaimRelease(one.ID(), "agent:ws", event.SourceObserved, at(30)))
	apply(event.ClaimAdd(a, "claude:one", "canonical", "agent:ws", event.SourceAsserted, at(40), at(600)))

	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := exportJSON(e, g, nodes, sortedEdges(g, nodes), include{claims: true}); err != nil {
		t.Fatal(err)
	}
	golden(t, "export_claims.json", e.stdout.(*bytes.Buffer).Bytes())
}

func TestExportJSONIncludeSourceGolden(t *testing.T) {
	nodes, g := exportSample()
	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := exportJSON(e, g, nodes, sortedEdges(g, nodes), include{source: true}); err != nil {
		t.Fatal(err)
	}
	golden(t, "export_source.json", e.stdout.(*bytes.Buffer).Bytes())
}

// The id tiebreak in sortedEdges cannot be reached through the goldens, or
// through any real store: the fold's triple index rejects a second edge with
// the same (kind, from, to), so nothing that survives a fold can tie. It exists
// so the comparison is a *total* order regardless — without it, two tied edges
// fall back to map iteration and the output stops being canonical (SPEC.md
// §9.5). Reversing it must therefore change something, or it is not doing its
// job.
func TestSortedEdgesTotalOrderOnTiedTriples(t *testing.T) {
	a, b := string(bytes.Repeat([]byte{'a'}, 64)), string(bytes.Repeat([]byte{'b'}, 64))
	nodes := []*graph.Node{{ID: a}, {ID: b}}
	g := &graph.Graph{
		Nodes: map[string]*graph.Node{a: nodes[0], b: nodes[1]},
		Edges: map[string]*graph.Edge{
			"zzz": {ID: "zzz", Kind: "depends-on", From: a, To: b},
			"aaa": {ID: "aaa", Kind: "depends-on", From: a, To: b},
			"mmm": {ID: "mmm", Kind: "depends-on", From: a, To: b},
		},
	}
	// Repeated because the defect being ruled out is map iteration order, which
	// is randomised per run and per range: one pass could agree by luck.
	for i := 0; i < 20; i++ {
		got := sortedEdges(g, nodes)
		ids := []string{got[0].ID, got[1].ID, got[2].ID}
		if ids[0] != "aaa" || ids[1] != "mmm" || ids[2] != "zzz" {
			t.Fatalf("tied edges are not in a stable order on pass %d: %v", i, ids)
		}
	}
}
