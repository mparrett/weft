package graph

import (
	"strings"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

func build(t *testing.T, events ...event.Event) *Graph {
	t.Helper()
	g := New()
	for _, e := range events {
		if err := g.Apply(e); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	return g
}

func node(t *testing.T, title string) event.Event {
	t.Helper()
	e, err := event.NodeAdd("test", "bug", title, "user:test", event.SourceAsserted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func edge(t *testing.T, kind, from, to string) event.Event {
	t.Helper()
	e, err := event.EdgeAdd(kind, from, to, "user:test", event.SourceAsserted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Acyclicity is a property per edge kind. A global constraint would reject
// conflicts-with, which is symmetric by nature (SPEC.md §4.2).
func TestAcyclicityIsPerKind(t *testing.T) {
	a, b := node(t, "a"), node(t, "b")

	for _, kind := range []string{EdgeDependsOn, EdgePartOf, EdgeSupersedes, EdgeDerivedFrom} {
		t.Run(kind+"/rejects cycle", func(t *testing.T) {
			g := build(t, a, b, edge(t, kind, a.ID(), b.ID()))
			if err := g.CheckEdge(kind, b.ID(), a.ID()); err == nil {
				t.Errorf("%s should reject a cycle", kind)
			}
		})
	}

	for _, kind := range []string{EdgeConflictsWith, EdgeSameAs} {
		t.Run(kind+"/permits cycle", func(t *testing.T) {
			g := build(t, a, b, edge(t, kind, a.ID(), b.ID()))
			if err := g.CheckEdge(kind, b.ID(), a.ID()); err != nil {
				t.Errorf("%s must permit the reverse edge, got: %v", kind, err)
			}
		})
	}
}

func TestSelfEdgeRejected(t *testing.T) {
	a := node(t, "a")
	g := build(t, a)
	if err := g.CheckEdge(EdgeDependsOn, a.ID(), a.ID()); err == nil {
		t.Error("a node cannot depend on itself")
	}
	if err := g.CheckEdge(EdgeConflictsWith, a.ID(), a.ID()); err != nil {
		t.Errorf("conflicts-with is not acyclic, self-edge should be allowed: %v", err)
	}
}

func TestTransitiveCycleRejected(t *testing.T) {
	a, b, c := node(t, "a"), node(t, "b"), node(t, "c")
	g := build(t, a, b, c,
		edge(t, EdgeDependsOn, a.ID(), b.ID()),
		edge(t, EdgeDependsOn, b.ID(), c.ID()),
	)
	err := g.CheckEdge(EdgeDependsOn, c.ID(), a.ID())
	if err == nil {
		t.Fatal("expected the transitive cycle to be rejected")
	}
	// "Blocked" without the path is not actionable — the same principle the
	// why query is built on (SPEC.md §6.1).
	if !strings.Contains(err.Error(), "->") {
		t.Errorf("error should show the offending path, got: %v", err)
	}
}

func TestIndependentEdgesAllowed(t *testing.T) {
	a, b, c := node(t, "a"), node(t, "b"), node(t, "c")
	g := build(t, a, b, c, edge(t, EdgeDependsOn, a.ID(), b.ID()))
	if err := g.CheckEdge(EdgeDependsOn, a.ID(), c.ID()); err != nil {
		t.Errorf("unrelated edge rejected: %v", err)
	}
	// A diamond is not a cycle.
	if err := g.CheckEdge(EdgeDependsOn, c.ID(), b.ID()); err != nil {
		t.Errorf("diamond rejected as a cycle: %v", err)
	}
}

// A cycle in one kind must not constrain another.
func TestKindsDoNotInterfere(t *testing.T) {
	a, b := node(t, "a"), node(t, "b")
	g := build(t, a, b, edge(t, EdgeConflictsWith, a.ID(), b.ID()), edge(t, EdgeConflictsWith, b.ID(), a.ID()))
	if err := g.CheckEdge(EdgeDependsOn, a.ID(), b.ID()); err != nil {
		t.Errorf("conflicts-with cycle blocked a depends-on edge: %v", err)
	}
}

func TestRetractionUnblocksEdge(t *testing.T) {
	a, b := node(t, "a"), node(t, "b")
	e := edge(t, EdgeDependsOn, a.ID(), b.ID())
	retract, err := event.EdgeRetract(e.ID(), "user:test", event.SourceAsserted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	g := build(t, a, b, e, retract)
	if len(g.Edges) != 0 {
		t.Fatalf("retracted edge survived: %d edges", len(g.Edges))
	}
	if err := g.CheckEdge(EdgeDependsOn, b.ID(), a.ID()); err != nil {
		t.Errorf("reverse edge still rejected after retraction: %v", err)
	}
}

// An uninterpretable fact is not corruption (EVENTS.md §1.3).
func TestUnknownKindIgnored(t *testing.T) {
	a := node(t, "a")
	unknown := event.New("quantum.entangle", "user:test", event.SourceAsserted, time.Now())
	if err := unknown.Seal(); err != nil {
		t.Fatal(err)
	}
	g := build(t, a, unknown)
	if len(g.Nodes) != 1 {
		t.Errorf("unknown kind perturbed the fold: %d nodes", len(g.Nodes))
	}
}

// Deliberately incomplete graphs are the normal state (SPEC.md §9.3).
func TestDanglingEdgeRetained(t *testing.T) {
	a := node(t, "a")
	g := build(t, a, edge(t, EdgeDependsOn, a.ID(), strings.Repeat("f", 64)))
	if len(g.Edges) != 1 {
		t.Errorf("dangling edge dropped: %d edges", len(g.Edges))
	}
}

// fixedTime keeps generated ids stable across runs where the test does not care
// about the timestamp.
func fixedTime() time.Time {
	return time.Date(2026, 7, 31, 9, 14, 22, 0, time.UTC)
}

// Regression: the neighbour is whichever endpoint is not the node we centred
// on. Deriving it from the edge's stored direction made a symmetric edge report
// its own node as its neighbour.
func TestNeighborsPicksTheOtherEndpoint(t *testing.T) {
	a, b := node(t, "a"), node(t, "b")
	g := build(t, a, b, edge(t, EdgeConflictsWith, a.ID(), b.ID()))

	for _, tc := range []struct{ centre, want string }{
		{a.ID(), b.ID()},
		{b.ID(), a.ID()},
	} {
		ns := g.Neighbors(tc.centre)
		if len(ns) != 1 {
			t.Fatalf("centre %s: got %d neighbours, want 1", tc.centre[:7], len(ns))
		}
		if ns[0].ID != tc.want {
			t.Errorf("centre %s: neighbour = %s, want %s", tc.centre[:7], ns[0].ID[:7], tc.want[:7])
		}
	}
}

// Both directions in one pass, so orienting on a node is one call.
func TestNeighborsIncludesBothDirections(t *testing.T) {
	a, b, c := node(t, "a"), node(t, "b"), node(t, "c")
	g := build(t, a, b, c,
		edge(t, EdgeDependsOn, a.ID(), b.ID()),
		edge(t, EdgeDependsOn, c.ID(), a.ID()),
	)
	ns := g.Neighbors(a.ID())
	if len(ns) != 2 {
		t.Fatalf("got %d neighbours, want 2", len(ns))
	}
	var outbound, inbound int
	for _, n := range ns {
		if n.Outbound {
			outbound++
		} else {
			inbound++
		}
	}
	if outbound != 1 || inbound != 1 {
		t.Errorf("got %d outbound and %d inbound, want 1 each", outbound, inbound)
	}
}

// A symmetric relation asserted from both directions is one fact, not two.
func TestSymmetricEdgeDeduplicatesInFold(t *testing.T) {
	a, b := node(t, "a"), node(t, "b")
	g := build(t, a, b,
		edge(t, EdgeConflictsWith, a.ID(), b.ID()),
		edge(t, EdgeConflictsWith, b.ID(), a.ID()),
	)
	if len(g.Edges) != 1 {
		t.Errorf("got %d edges, want 1", len(g.Edges))
	}
	// A directed kind keeps both, because there they are different facts.
	g2 := build(t, a, b,
		edge(t, EdgeConflictsWith, a.ID(), b.ID()),
		edge(t, EdgeDependsOn, a.ID(), b.ID()),
	)
	if len(g2.Edges) != 2 {
		t.Errorf("distinct kinds collapsed: got %d edges, want 2", len(g2.Edges))
	}
}

// A dangling neighbour is reported with its id and a nil Node, never dropped.
func TestNeighborsReportsDangling(t *testing.T) {
	a := node(t, "a")
	missing := strings.Repeat("f", 64)
	g := build(t, a, edge(t, EdgeDependsOn, a.ID(), missing))

	ns := g.Neighbors(a.ID())
	if len(ns) != 1 {
		t.Fatalf("got %d neighbours, want 1", len(ns))
	}
	if ns[0].Node != nil {
		t.Error("expected a nil Node for a dangling edge")
	}
	if ns[0].ID != missing {
		t.Errorf("dangling id = %s", ns[0].ID[:7])
	}
}

func TestRetractionRemovesFromBothIndexes(t *testing.T) {
	a, b := node(t, "a"), node(t, "b")
	e := edge(t, EdgeDependsOn, a.ID(), b.ID())
	retract, err := event.EdgeRetract(e.ID(), "user:test", event.SourceAsserted, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	g := build(t, a, b, e, retract)
	if n := len(g.Neighbors(a.ID())); n != 0 {
		t.Errorf("outbound index still holds %d edges", n)
	}
	if n := len(g.Neighbors(b.ID())); n != 0 {
		t.Errorf("inbound index still holds %d edges", n)
	}
}

// An edge is indexed once, under out[from] and in[to], and the fold normalizes
// endpoint order for symmetric kinds so that A-B and B-A collapse. Scanning in[]
// alone therefore found a conflicts-with edge from whichever endpoint
// normalization made `to` and not from the other — resurrecting on the read side
// exactly the asymmetry the normalization exists to remove. OutEdges already
// compensated; InEdges did not, and nothing walked *in* over a symmetric kind
// until rdeps gained --via.
func TestInEdgesFindsASymmetricEdgeFromEitherEnd(t *testing.T) {
	// Both assertion orders, since the bug was invisible from one of them.
	for _, swap := range []bool{false, true} {
		a, b := node(t, "a"), node(t, "b")
		from, to := a.ID(), b.ID()
		if swap {
			from, to = to, from
		}
		g := build(t, a, b, edge(t, EdgeConflictsWith, from, to))

		for _, end := range []struct {
			name string
			id   string
		}{{"a", a.ID()}, {"b", b.ID()}} {
			if got := g.InEdges(end.id, EdgeConflictsWith); len(got) != 1 {
				t.Errorf("swap=%v: InEdges from %s returned %d edges, want 1",
					swap, end.name, len(got))
			}
		}
	}
}

// Symmetry and transitivity are independent properties, and every valid kind
// needs an answer for both — a kind missing from the map reads as false, which
// for `transitive` silently truncates any walk that crosses it.
func TestTransitiveCoversEveryKind(t *testing.T) {
	all := []string{EdgeDependsOn, EdgeConflictsWith, EdgeDerivedFrom,
		EdgePartOf, EdgeSameAs, EdgeSupersedes}
	for _, k := range all {
		if _, ok := transitive[k]; !ok {
			t.Errorf("kind %q has no transitivity answer", k)
		}
	}
	if len(transitive) != len(all) {
		t.Errorf("transitive names %d kinds, there are %d", len(transitive), len(all))
	}
	// conflicts-with is the singular one: symmetric and *not* transitive.
	// same-as is symmetric and transitive, which is why it walks fine.
	if Transitive(EdgeConflictsWith) {
		t.Error("conflicts-with is not transitive: A-B and B-C say nothing about A-C")
	}
	if !Transitive(EdgeSameAs) {
		t.Error("same-as is transitive")
	}
}
