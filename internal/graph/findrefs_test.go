package graph

import (
	"slices"
	"testing"

	"github.com/mparrett/weft/internal/event"
)

// refRetract builds a ref.retract inline, as cmd/weft/ref.go does — the event
// package has no constructor for it, matching the fixture's ref helper.
func (f *mergeFixture) refRetract(target string) {
	f.t.Helper()
	e := event.New(event.KindRefRetract, "user:test", event.SourceAsserted, f.tick())
	e["target"] = target
	f.apply(e, e.Seal())
}

func TestFindRefsReturnsTheAssertingEvent(t *testing.T) {
	f := newFixture(t)
	a := f.node("a")
	f.ref(a, "gh:acme/api#412")

	ids := f.g.FindRefs(a, "gh:acme/api#412")
	if len(ids) != 1 {
		t.Fatalf("FindRefs returned %d ids, want 1", len(ids))
	}
	if f.g.FindRefs(a, "gh:acme/api#999") != nil {
		t.Error("FindRefs matched a ref that was never asserted")
	}
	if f.g.FindRefs(f.node("other"), "gh:acme/api#412") != nil {
		t.Error("FindRefs matched a ref asserted about a different node")
	}
}

// Unlike edges, which collapse on the triple, the fold records an entry for
// every ref.add even when the node already carries the ref. Two writers binding
// the same thing therefore leave two live assertions, and retracting one drops
// the ref from the node while the log still says it holds.
func TestFindRefsReturnsEveryAssertionOfTheSameBinding(t *testing.T) {
	f := newFixture(t)
	a := f.node("a")
	f.ref(a, "gh:acme/api#412")
	f.ref(a, "gh:acme/api#412") // a second writer, same binding

	ids := f.g.FindRefs(a, "gh:acme/api#412")
	if len(ids) != 2 {
		t.Fatalf("FindRefs returned %d ids, want both assertions", len(ids))
	}
	if !slices.IsSorted(ids) {
		t.Errorf("FindRefs returned an unsorted set %v; map order is randomized, so a "+
			"command built on this would emit events in a different order each run", ids)
	}

	// Retracting only the first leaves the node without the ref while the second
	// assertion is still live — which is why the command retracts all of them.
	f.refRetract(ids[0])
	if got := f.g.FindRefs(a, "gh:acme/api#412"); len(got) != 1 {
		t.Errorf("after one retraction FindRefs returned %d, want the remaining 1", len(got))
	}
}

// Entries hold the node id as it was canonical when asserted, so a merge that
// happens afterwards has to be resolved on both sides of the comparison — or
// the binding becomes unfindable under the survivor's id, which is the only id
// a caller has after the merge.
func TestFindRefsCanonicalizesThroughTheAliasMap(t *testing.T) {
	f := newFixture(t)
	survivor, merged := f.node("survivor"), f.node("merged")
	f.ref(merged, "gh:acme/api#412")
	f.merge(survivor, merged)

	if got := f.g.FindRefs(survivor, "gh:acme/api#412"); len(got) != 1 {
		t.Errorf("FindRefs by the survivor's id returned %d, want 1", len(got))
	}
	if got := f.g.FindRefs(merged, "gh:acme/api#412"); len(got) != 1 {
		t.Errorf("FindRefs by the merged id returned %d, want 1", len(got))
	}
}
