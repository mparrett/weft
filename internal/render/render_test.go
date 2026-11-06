package render

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/graph"
)

var update = flag.Bool("update", false, "rewrite golden files")

// golden pins rendered output byte for byte. Nothing else fails when node
// ordering or column alignment drifts — the git diffs just go noisy months
// later — so this is the only guard on SPEC.md §9.5.
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
		t.Fatalf("missing golden (run: go test ./internal/render -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func sample() []*graph.Node {
	// The first node carries provenance and the others do not. Every node
	// folded from a valid log has By and Source (event.Parse rejects their
	// absence), so a sample without them pins a shape no real node has and
	// leaves the source line invisible to every golden; the bare nodes keep
	// the omitted-when-empty branch covered for hand-built graphs.
	return []*graph.Node{
		{ID: strings.Repeat("a", 64), NS: "acme/api", Kind: "bug", Title: "N+1 in report generation", State: "open",
			By: "agent:scanner", Source: "inferred"},
		{ID: strings.Repeat("b", 64), NS: "acme/api", Kind: "spec", Title: "batch export spec", State: "open",
			Refs: []string{"file:docs/export.md", "gh:acme/api#412"}},
		{ID: strings.Repeat("c", 64), NS: "acme/api", Kind: "finding", Title: "review point Z", State: "done"},
	}
}

func TestNodeListGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := NodeList(&buf, sample(), MinWidth, false); err != nil {
		t.Fatal(err)
	}
	golden(t, "node_list.txt", buf.Bytes())
}

func TestNodeGolden(t *testing.T) {
	var buf bytes.Buffer
	for _, n := range sample() {
		if err := Node(&buf, n, MinWidth); err != nil {
			t.Fatal(err)
		}
	}
	golden(t, "node_detail.txt", buf.Bytes())
}

// A filtered listing must still print ids wide enough to stay unambiguous
// across the whole store, since a user pastes what they see.
func TestNodeListHonoursCallerWidth(t *testing.T) {
	var buf bytes.Buffer
	if err := NodeList(&buf, sample()[:1], 12, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), strings.Repeat("a", 12)+"  ") {
		t.Errorf("expected a 12-char id, got %q", buf.String())
	}
}

func TestNeighborhoodGolden(t *testing.T) {
	nodes := sample()
	mk := func(kind, from, to string) *graph.Edge {
		return &graph.Edge{ID: from[:8] + to[:8], Kind: kind, From: from, To: to}
	}
	a, b, c := nodes[0], nodes[1], nodes[2]
	neighbors := []graph.Neighbor{
		{Edge: mk(graph.EdgeDependsOn, a.ID, b.ID), Node: b, ID: b.ID, Outbound: true},
		{Edge: mk(graph.EdgeDependsOn, c.ID, a.ID), Node: c, ID: c.ID, Outbound: false},
		{Edge: mk(graph.EdgeConflictsWith, a.ID, b.ID), Node: b, ID: b.ID, Outbound: true},
		// A dangling neighbour is a normal state, not corruption.
		{Edge: mk(graph.EdgeDependsOn, a.ID, strings.Repeat("f", 64)),
			Node: nil, ID: strings.Repeat("f", 64), Outbound: true},
	}
	var buf bytes.Buffer
	if err := Neighborhood(&buf, neighbors, MinWidth, a.NS); err != nil {
		t.Fatal(err)
	}
	golden(t, "neighborhood.txt", buf.Bytes())
}

// The inverse reading is what answers "who is waiting on me".
func TestEdgeLabelInverts(t *testing.T) {
	cases := []struct {
		kind     string
		outbound bool
		want     string
	}{
		{graph.EdgeDependsOn, true, "depends on"},
		{graph.EdgeDependsOn, false, "blocks"},
		{graph.EdgePartOf, true, "part of"},
		{graph.EdgePartOf, false, "contains"},
		{graph.EdgeConflictsWith, true, "conflicts with"},
		{graph.EdgeConflictsWith, false, "conflicts with"},
		{"future-kind", true, "future-kind"},
	}
	for _, tc := range cases {
		if got := edgeLabel(tc.kind, tc.outbound); got != tc.want {
			t.Errorf("edgeLabel(%q, %v) = %q, want %q", tc.kind, tc.outbound, got, tc.want)
		}
	}
}

func TestClaimsGolden(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	claims := []*graph.Claim{
		{Holder: "user:matt", By: "user:matt", Source: "asserted", Note: "starting now",
			Expires: now.Add(time.Hour)},
		// An inferred claim about a third party: who guessed is what keeps it
		// from reading as fact.
		{Holder: "user:colleague", By: "agent:scanner", Source: "inferred",
			Expires: now.Add(time.Hour)},
		{Holder: "agent:gone", By: "agent:gone", Source: "asserted",
			Expires: now.Add(-time.Hour)},
		{Holder: "user:forever", By: "user:forever", Source: "asserted"},
	}
	var buf bytes.Buffer
	if err := Claims(&buf, claims, now); err != nil {
		t.Fatal(err)
	}
	golden(t, "claims.txt", buf.Bytes())
}

func TestNodesJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := NodesJSON(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	golden(t, "nodes.json", buf.Bytes())
}

func TestErrorGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := Error(&buf, 4, "prefix \"a3f\" is ambiguous", []string{"a3f91c2", "a3fbb01"}); err != nil {
		t.Fatal(err)
	}
	golden(t, "error_ambiguous.json", buf.Bytes())
}

// A fixed 7-character display is unsafe; the width expands until ids are
// distinct (EVENTS.md §2.4).
func TestWidthExpandsOnCollision(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		want int
	}{
		{"distinct early", []string{"abcdef01", "12345678"}, MinWidth},
		{"empty", nil, MinWidth},
		{"single", []string{strings.Repeat("a", 64)}, MinWidth},
		{"collide at 7", []string{"aaaaaaaX", "aaaaaaaY"}, 8},
		{"collide at 10", []string{"aaaaaaaaaaX", "aaaaaaaaaaY"}, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Width(tc.ids); got != tc.want {
				t.Errorf("Width = %d, want %d", got, tc.want)
			}
		})
	}
}

// Identical ids cannot be separated at any width; the function must terminate
// rather than loop.
func TestWidthTerminatesOnIdenticalIDs(t *testing.T) {
	id := strings.Repeat("a", 64)
	if got := Width([]string{id, id}); got != 64 {
		t.Errorf("Width = %d, want 64", got)
	}
}

func TestShort(t *testing.T) {
	if got := Short("abcdefghij", 7); got != "abcdefg" {
		t.Errorf("got %q", got)
	}
	if got := Short("abc", 7); got != "abc" {
		t.Errorf("short id should pass through, got %q", got)
	}
}

// Rendering must not depend on the caller's slice order for refs, or two
// identical graphs could print differently.
func TestRefsRenderSorted(t *testing.T) {
	n := &graph.Node{ID: strings.Repeat("a", 64), Kind: "bug", Title: "x",
		Refs: []string{"z:last", "a:first"}}
	var buf bytes.Buffer
	if err := NodesJSON(&buf, []*graph.Node{n}); err != nil {
		t.Fatal(err)
	}
	if strings.Index(buf.String(), "a:first") > strings.Index(buf.String(), "z:last") {
		t.Errorf("refs not sorted:\n%s", buf.String())
	}
}

// Age drives the "(observed Nd ago)" annotation, and every branch renders a
// different unit — a bug that labelled hours as days would be invisible in a
// substring assertion.
func TestAgeBranches(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		back time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
		{30 * 24 * time.Hour, "30d ago"},
	}
	for _, tc := range cases {
		if got := Age(now.Add(-tc.back), now); got != tc.want {
			t.Errorf("%v ago rendered %q, want %q", tc.back, got, tc.want)
		}
	}
}

// Node-rooted queries are always global, so a neighbour in another namespace is
// ordinary — and the fact that it is elsewhere is often the answer. It must be
// visible without a second command.
func TestNeighborhoodMarksOtherNamespaces(t *testing.T) {
	nodes := sample()
	a, b := nodes[0], nodes[1]
	b.NS = "other/lib"

	edge := &graph.Edge{ID: a.ID[:8] + b.ID[:8], Kind: graph.EdgeDependsOn, From: a.ID, To: b.ID}
	neighbors := []graph.Neighbor{
		{Edge: edge, Node: b, ID: b.ID, Outbound: true},
	}
	var buf bytes.Buffer
	if err := Neighborhood(&buf, neighbors, MinWidth, a.NS); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[other/lib]") {
		t.Errorf("cross-namespace neighbour not marked:\n%s", buf.String())
	}

	// Same namespace stays unannotated, so single-project output is unchanged.
	b.NS = a.NS
	buf.Reset()
	if err := Neighborhood(&buf, neighbors, MinWidth, a.NS); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "[") {
		t.Errorf("same-namespace neighbour should not be annotated:\n%s", buf.String())
	}
}
