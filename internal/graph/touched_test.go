package graph

import (
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

// at is the fixture's clock value for a given tick, so a test can assert an
// exact timestamp rather than a relative one.
func (f *mergeFixture) at() time.Time { return f.clock }

func (f *mergeFixture) state(node, s string) {
	f.t.Helper()
	f.apply(event.NodeState(node, s, "user:test", event.SourceAsserted, f.tick()))
}

func (f *mergeFixture) observe(node, ref, state, title string) {
	f.t.Helper()
	f.apply(event.RefObserve(node, ref, event.ObservePayload{State: state, Title: title}, "user:test", event.SourceObserved, f.tick()))
}

func (f *mergeFixture) claim(node, holder string) string {
	f.t.Helper()
	return f.apply(event.ClaimAdd(node, holder, "", "user:test", event.SourceAsserted, f.tick(), time.Time{})).ID()
}

func TestNodeAddStampsCreatedAndTouched(t *testing.T) {
	f := newFixture(t)
	id := f.node("a task")
	n := f.g.Nodes[id]

	if !n.Created.Equal(f.at()) {
		t.Errorf("Created = %v, want the node.add timestamp %v", n.Created, f.at())
	}
	if !n.Touched.Equal(n.Created) {
		t.Errorf("Touched = %v, want it to start at Created %v", n.Touched, n.Created)
	}
}

// Every kind of assertion weft makes about a node counts as movement. A kind
// that silently failed to touch would leave the node looking abandoned in every
// recency listing while work was visibly happening on it.
func TestEveryAssertionTouches(t *testing.T) {
	cases := []struct {
		name string
		do   func(f *mergeFixture, id string)
	}{
		{"state", func(f *mergeFixture, id string) { f.state(id, event.StateDone) }},
		{"edge.add", func(f *mergeFixture, id string) { f.link(EdgeDependsOn, id, f.node("blocker")) }},
		{"edge.retract", func(f *mergeFixture, id string) {
			e := f.link(EdgeDependsOn, id, f.node("blocker"))
			f.apply(event.EdgeRetract(e, "user:test", event.SourceAsserted, f.tick()))
		}},
		{"ref.add", func(f *mergeFixture, id string) { f.ref(id, "gh:acme/api#1") }},
		{"ref.retract", func(f *mergeFixture, id string) {
			e := event.New(event.KindRefAdd, "user:test", event.SourceObserved, f.tick())
			e["node"], e["ref"] = id, "gh:acme/api#1"
			f.apply(e, e.Seal())
			r := event.New(event.KindRefRetract, "user:test", event.SourceAsserted, f.tick())
			r["target"] = e.ID()
			f.apply(r, r.Seal())
		}},
		{"claim.add", func(f *mergeFixture, id string) { f.claim(id, "user:test") }},
		{"claim.release", func(f *mergeFixture, id string) {
			c := f.claim(id, "user:test")
			f.apply(event.ClaimRelease(c, "user:test", event.SourceAsserted, f.tick()))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.node("the work")
			created := f.g.Nodes[id].Created

			tc.do(f, id)

			if got := f.g.Nodes[id].Touched; !got.After(created) {
				t.Errorf("%s left Touched at %v, want later than Created %v", tc.name, got, created)
			}
			if got := f.g.Nodes[id].Created; !got.Equal(created) {
				t.Errorf("%s moved Created to %v, want it fixed at %v", tc.name, got, created)
			}
		})
	}
}

// An observation is evidence about upstream, not about this node. Counting it
// would let a scheduled import float every node it polled to the top of a
// recency listing, making polling indistinguishable from progress.
func TestObserveDoesNotTouch(t *testing.T) {
	f := newFixture(t)
	id := f.node("bound to upstream")
	f.ref(id, "gh:acme/api#412")
	touched := f.g.Nodes[id].Touched

	f.observe(id, "gh:acme/api#412", "closed", "upstream title")

	if got := f.g.Nodes[id].Touched; !got.Equal(touched) {
		t.Errorf("ref.observe advanced Touched to %v, want it left at %v", got, touched)
	}
}

// A re-emitted assertion that changes nothing is not movement. Import leans on
// this: it must be able to re-read upstream without making every node look
// freshly worked on (EVENTS.md §3.9).
func TestRedundantRefAddDoesNotTouch(t *testing.T) {
	f := newFixture(t)
	id := f.node("bound twice")
	f.ref(id, "gh:acme/api#412")
	touched := f.g.Nodes[id].Touched

	f.ref(id, "gh:acme/api#412")

	if got := f.g.Nodes[id].Touched; !got.Equal(touched) {
		t.Errorf("redundant ref.add advanced Touched to %v, want it left at %v", got, touched)
	}
}

// Touched takes the max, never the last write. `at` is a wall clock: it skews
// between machines and a concatenated log can present events in any order
// (EVENTS.md §5.2). Last-write-wins would let one backdated event make a node
// that just moved report as untouched for as long as the skew lasted.
func TestTouchedNeverGoesBackwards(t *testing.T) {
	f := newFixture(t)
	id := f.node("the work")
	f.state(id, event.StateDone)
	forward := f.g.Nodes[id].Touched

	// An event stamped a week before the node was even asserted, which is what
	// a badly-skewed clock or a backfilled import produces.
	backdated := f.at().Add(-7 * 24 * time.Hour)
	f.apply(event.NodeState(id, event.StateOpen, "user:other", event.SourceAsserted, backdated))

	if got := f.g.Nodes[id].Touched; !got.Equal(forward) {
		t.Errorf("backdated event moved Touched to %v, want it held at %v", got, forward)
	}
	// The assertion itself still applies — only the timestamp is disregarded.
	if got := f.g.Nodes[id].State; got != event.StateOpen {
		t.Errorf("backdated event was ignored entirely: state = %q", got)
	}
}

// The other two "discarded assertion does not touch" cases §5.4 claims. Both
// are correct as written; without a test, a future reorder of those branches
// could move a touch to the wrong side of its check and nothing would fail.
func TestDiscardedAssertionsDoNotTouch(t *testing.T) {
	t.Run("redundant edge.add", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.node("blocked"), f.node("blocker")
		f.link(EdgeDependsOn, a, b)
		touched := f.g.Nodes[a].Touched

		f.link(EdgeDependsOn, a, b) // same triple, collapses

		if got := f.g.Nodes[a].Touched; !got.Equal(touched) {
			t.Errorf("re-asserting an existing edge advanced Touched to %v, want %v", got, touched)
		}
	})

	t.Run("merge artifact self-edge", func(t *testing.T) {
		f := newFixture(t)
		a, b := f.node("one"), f.node("other")
		f.merge(a, b)
		touched := f.g.Nodes[a].Touched

		// Folding two nodes into one turns an edge between them into a
		// self-edge, which the fold drops as an artifact nobody asserted.
		f.link(EdgeDependsOn, a, b)

		if got := f.g.Nodes[a].Touched; !got.Equal(touched) {
			t.Errorf("a dropped self-edge advanced Touched to %v, want %v", got, touched)
		}
	})
}

// A future-dated event is the one input that breaks recency: max never
// decreases, so nothing can walk it back, and it is contagious through merge.
// This pins current behaviour rather than endorsing it — if the fold ever grows
// a clamp, this test is where the change becomes visible.
func TestFutureTimestampIsRecordedAsIs(t *testing.T) {
	f := newFixture(t)
	id := f.node("the work")
	future := f.at().Add(365 * 24 * time.Hour)
	f.apply(event.NodeState(id, event.StateDone, "user:skewed", event.SourceAsserted, future))

	if got := f.g.Nodes[id].Touched; !got.Equal(future) {
		t.Errorf("Touched = %v, want the future stamp %v recorded unchanged", got, future)
	}
	// Contagion: the survivor inherits it and no later event can lower it.
	other := f.node("the survivor")
	f.merge(other, id)
	if got := f.g.Nodes[other].Touched; !got.Equal(future) {
		t.Errorf("survivor Touched = %v, want the inherited future stamp %v", got, future)
	}
}

// A merge moves everything the survivor now owns. Recency is one more thing to
// move, and missing it is silent: work done under the duplicate's id vanishes
// from every recency listing while both nodes still resolve.
func TestMergeCarriesTouched(t *testing.T) {
	f := newFixture(t)
	survivor := f.node("the survivor")
	dup := f.node("the duplicate")

	// The duplicate is where the recent work happened.
	f.state(dup, event.StateDone)
	dupTouched := f.g.Nodes[dup].Touched
	survivorCreated := f.g.Nodes[survivor].Created

	f.merge(survivor, dup)

	n := f.g.Nodes[survivor]
	if n.Touched.Before(dupTouched) {
		t.Errorf("survivor Touched = %v, want at least the duplicate's %v", n.Touched, dupTouched)
	}
	// Created is not merged: it says when *this* node was asserted, and the
	// duplicate's own stays readable on the duplicate, which still resolves.
	if !n.Created.Equal(survivorCreated) {
		t.Errorf("merge moved survivor Created to %v, want it fixed at %v", n.Created, survivorCreated)
	}
	if f.g.Nodes[dup].Created.IsZero() {
		t.Error("merged node lost its own Created; merging moves the index, not the assertions")
	}
}

// Recency must not depend on arrival order, for the events that apply at all.
//
// The qualifier is the whole subtlety. Whether an event applies is governed by
// §5.2, not by this rule: a node.state naming a node the fold has not seen is
// dropped, so reordering it past its node.add legitimately changes the graph.
// What must never vary is the *timestamp accumulation* over the events that do
// apply — and node.add and node.merge apply in any order, which makes them the
// case worth pinning.
//
// That case is also the one that broke. A node.merge landing before the
// node.add it names left the merged node stamping only its own struct, and the
// survivor never heard about the work. Every other test here builds fixtures in
// causal order and therefore cannot see it.
func TestRecencyIsIndependentOfArrivalOrder(t *testing.T) {
	base := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)

	// A lagging clock on the merging machine stamps the merge before either
	// node was asserted, so no order here is the "obviously right" one.
	build := func() []event.Event {
		survivor, err := event.NodeAdd("ns", "task", "survivor", "user:test", event.SourceAsserted, base)
		if err != nil {
			t.Fatal(err)
		}
		dup, err := event.NodeAdd("ns", "task", "duplicate", "user:test", event.SourceAsserted, base.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		merge, err := event.NodeMerge(survivor.ID(), dup.ID(), "user:test", event.SourceAsserted, base.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return []event.Event{survivor, dup, merge}
	}

	fold := func(order []int) time.Time {
		evs := build()
		g := New()
		for _, i := range order {
			if err := g.Apply(evs[i]); err != nil {
				t.Fatal(err)
			}
		}
		return g.Nodes[evs[0].ID()].Touched
	}

	// Every permutation in which the survivor is known by the time the merge
	// lands. Enumerated and filtered rather than hand-picked: which orders are
	// reachable depends on how logs were concatenated, and guessing wrong is
	// how this went unnoticed the first time.
	//
	// The excluded orders are not a second bug. A merge naming a survivor the
	// fold has not seen is a reference to an unknown node, and §5.2 makes that
	// dangle — the same rule that drops a node.state arriving before its
	// node.add. Recency inherits the limit rather than escaping it.
	want := fold([]int{0, 1, 2})
	var checked int
	for _, order := range permutations([]int{0, 1, 2}) {
		if indexIn(order, 0) > indexIn(order, 2) {
			continue // survivor asserted after the merge that names it
		}
		checked++
		if got := fold(order); !got.Equal(want) {
			t.Errorf("order %v folded Touched to %v, want %v", order, got, want)
		}
	}
	if checked < 3 {
		t.Fatalf("only %d orders exercised; the filter is too aggressive to prove anything", checked)
	}
}

func indexIn(order []int, v int) int {
	for i, x := range order {
		if x == v {
			return i
		}
	}
	return -1
}

func permutations(in []int) [][]int {
	if len(in) <= 1 {
		return [][]int{append([]int(nil), in...)}
	}
	var out [][]int
	for i := range in {
		rest := make([]int, 0, len(in)-1)
		rest = append(rest, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]int{in[i]}, p...))
		}
	}
	return out
}

// After a merge, an assertion naming the merged node has to land on the
// survivor — the same canonicalization every other field gets. Miss it and the
// survivor silently stops aging while work continues under the old id.
func TestTouchCanonicalizesThroughMerge(t *testing.T) {
	f := newFixture(t)
	survivor := f.node("the survivor")
	dup := f.node("the duplicate")
	f.merge(survivor, dup)
	before := f.g.Nodes[survivor].Touched

	f.state(dup, event.StateDone)

	if got := f.g.Nodes[survivor].Touched; !got.After(before) {
		t.Errorf("assertion on the merged id left survivor Touched at %v, want later than %v", got, before)
	}
}
