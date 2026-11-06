package graph

import (
	"testing"

	"github.com/mparrett/weft/internal/event"
)

// FindEdge is what `weft unlink` turns a triple back into the assertion that
// made it, so its keying has to agree with edge.add's in every case where the
// two could disagree. Each test below is a way they could drift apart while
// every command still exits 0.

func TestFindEdgeMatchesTheAssertedTriple(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("a"), f.node("b")
	id := f.link(EdgeDependsOn, a, b)

	got := f.g.FindEdge(EdgeDependsOn, a, b)
	if got == nil {
		t.Fatal("FindEdge did not find the edge that link just asserted")
	}
	if got.ID != id {
		t.Errorf("FindEdge returned edge %s, want the edge.add id %s", got.ID, id)
	}
	// The id is what edge.retract targets, so returning the wrong one would
	// retract a different assertion than the caller asked about.
	if f.g.FindEdge(EdgePartOf, a, b) != nil {
		t.Error("FindEdge ignored the kind")
	}
	if f.g.FindEdge(EdgeDependsOn, b, a) != nil {
		t.Error("FindEdge ignored direction on a directed kind")
	}
}

// Symmetric kinds normalize endpoint order in tripleKey, so a lookup that did
// not would report no edge between B and A while holding the one asserted from
// A to B — telling a caller their edge is gone while leaving it in the graph.
func TestFindEdgeIgnoresOrderForSymmetricKinds(t *testing.T) {
	for _, kind := range []string{EdgeConflictsWith, EdgeSameAs} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			a, b := f.node("a"), f.node("b")
			id := f.link(kind, a, b)

			for _, order := range [][2]string{{a, b}, {b, a}} {
				got := f.g.FindEdge(kind, order[0], order[1])
				if got == nil || got.ID != id {
					t.Errorf("FindEdge(%s, %.4s, %.4s) did not find the edge",
						kind, order[0], order[1])
				}
			}
		})
	}
}

// edge.add canonicalizes its endpoints before keying, so FindEdge has to as
// well. A caller holding an id from before a merge — the id someone pasted out
// of a commit message a month ago — would otherwise be told no such edge exists
// while the re-pointed edge sits in the graph.
func TestFindEdgeCanonicalizesThroughTheAliasMap(t *testing.T) {
	f := newFixture(t)
	a, survivor, merged := f.node("a"), f.node("survivor"), f.node("merged")
	id := f.link(EdgeDependsOn, a, merged)
	f.merge(survivor, merged)

	if got := f.g.FindEdge(EdgeDependsOn, a, survivor); got == nil || got.ID != id {
		t.Fatal("FindEdge did not find the re-pointed edge by the survivor's id")
	}
	if got := f.g.FindEdge(EdgeDependsOn, a, merged); got == nil || got.ID != id {
		t.Error("FindEdge did not canonicalize the merged id onto the survivor")
	}
}

func TestFindEdgeReportsNothingForAnAbsentEdge(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("a"), f.node("b")

	if f.g.FindEdge(EdgeDependsOn, a, b) != nil {
		t.Error("FindEdge invented an edge that was never asserted")
	}

	// And after a retraction: dropEdge has to release the triple key, or the
	// edge stays findable under a key whose edge is gone from g.Edges — which
	// would make FindEdge return nil after a lookup that claimed a hit.
	f.link(EdgeDependsOn, a, b)
	e := f.g.FindEdge(EdgeDependsOn, a, b)
	if e == nil {
		t.Fatal("FindEdge lost the edge it just found")
	}
	f.apply(event.EdgeRetract(e.ID, "user:test", event.SourceAsserted, f.tick()))
	if f.g.FindEdge(EdgeDependsOn, a, b) != nil {
		t.Error("FindEdge still reports a retracted edge")
	}
}
