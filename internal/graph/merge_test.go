package graph

import (
	"slices"
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

// mergeFixture builds a graph by applying events directly, so the fold can be
// tested without going through the store.
type mergeFixture struct {
	t     *testing.T
	g     *Graph
	clock time.Time
}

func newFixture(t *testing.T) *mergeFixture {
	t.Helper()
	return &mergeFixture{t: t, g: New(), clock: time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)}
}

// tick advances the clock so that two otherwise identical events do not hash to
// the same id and get folded as a crash-retry (EVENTS.md §2.5).
func (f *mergeFixture) tick() time.Time {
	f.clock = f.clock.Add(time.Millisecond)
	return f.clock
}

func (f *mergeFixture) apply(e event.Event, err error) event.Event {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("building event: %v", err)
	}
	if err := f.g.Apply(e); err != nil {
		f.t.Fatalf("applying %s: %v", e.Kind(), err)
	}
	return e
}

func (f *mergeFixture) node(title string) string {
	f.t.Helper()
	return f.apply(event.NodeAdd("ns", "task", title, "user:test", event.SourceAsserted, f.tick())).ID()
}

func (f *mergeFixture) link(kind, from, to string) string {
	f.t.Helper()
	return f.apply(event.EdgeAdd(kind, from, to, "user:test", event.SourceAsserted, f.tick())).ID()
}

// ref builds a ref.add inline, as cmd/weft/add.go does — the event package has
// no constructor for it.
func (f *mergeFixture) ref(node, r string) {
	f.t.Helper()
	e := event.New(event.KindRefAdd, "user:test", event.SourceObserved, f.tick())
	e["node"] = node
	e["ref"] = r
	f.apply(e, e.Seal())
}

func (f *mergeFixture) merge(into, from string) {
	f.t.Helper()
	f.apply(event.NodeMerge(into, from, "user:test", event.SourceAsserted, f.tick()))
}

func TestAcyclicKindsAgree(t *testing.T) {
	for kind, isAcyclic := range acyclic {
		if isAcyclic != slices.Contains(acyclicKinds, kind) {
			t.Errorf("%s: acyclic=%v but acyclicKinds membership=%v",
				kind, isAcyclic, slices.Contains(acyclicKinds, kind))
		}
	}
	if len(acyclicKinds) > len(acyclic) {
		t.Errorf("acyclicKinds names %d kinds, acyclic knows %d", len(acyclicKinds), len(acyclic))
	}
}

func TestMergeRedirectsResolution(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	f.merge(a, b)

	// The whole point: an id pasted from a month-old commit message still works.
	got, err := f.g.Resolve(b)
	if err != nil {
		t.Fatalf("resolving a merged node: %v", err)
	}
	if got.ID != a {
		t.Errorf("resolved to %s, want the survivor %s", got.ID[:7], a[:7])
	}
	if f.g.Nodes[b].MergedInto != a {
		t.Errorf("merged node does not record its survivor")
	}
	// Nothing is lost: the merged node keeps the title it was asserted with.
	if f.g.Nodes[b].Title != "duplicate" {
		t.Errorf("merged node lost its title: %q", f.g.Nodes[b].Title)
	}
	if merged := f.g.MergedFrom(a); len(merged) != 1 || merged[0].ID != b {
		t.Errorf("MergedFrom did not report the merged node")
	}
	// Listings show one piece of work, not two.
	if live := f.g.Ordered(); len(live) != 1 || live[0].ID != a {
		t.Errorf("Ordered listed %d nodes, want only the survivor", len(live))
	}
	// Display width still spans the merged id, so a pasted prefix stays valid.
	if !slices.Contains(f.g.IDs(), b) {
		t.Errorf("IDs dropped the merged node, making its prefix unrenderable")
	}
}

func TestMergeMovesEdgesAndDropsSelfEdges(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("survivor"), f.node("duplicate"), f.node("other")
	f.link(EdgeDependsOn, b, c) // moves: b -> c becomes a -> c
	f.link(EdgeDependsOn, a, b) // becomes a -> a, so it is dropped
	f.merge(a, b)

	deps := f.g.OutEdges(a, EdgeDependsOn)
	if len(deps) != 1 {
		t.Fatalf("survivor has %d depends-on edges, want 1", len(deps))
	}
	if deps[0].To != c {
		t.Errorf("edge points at %s, want %s", deps[0].To[:7], c[:7])
	}
	if got := f.g.InEdges(c, EdgeDependsOn); len(got) != 1 || got[0].From != a {
		t.Errorf("reverse index not repointed at the survivor")
	}
	if len(f.g.OutEdges(b, "")) != 0 || len(f.g.InEdges(b, "")) != 0 {
		t.Errorf("merged node still carries edges")
	}
}

func TestMergeCollapsesDuplicateTriples(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("survivor"), f.node("duplicate"), f.node("other")
	first := f.link(EdgeDependsOn, a, c)
	f.link(EdgeDependsOn, b, c) // same triple once b becomes a
	f.merge(a, b)

	deps := f.g.OutEdges(a, EdgeDependsOn)
	if len(deps) != 1 {
		t.Fatalf("survivor has %d depends-on edges, want them collapsed to 1", len(deps))
	}
	// Earliest by file order is retained, so provenance points at whoever said
	// it first (EVENTS.md §3.2).
	if deps[0].ID != first {
		t.Errorf("collapsed to the later assertion, losing the original provenance")
	}
}

func TestMergeUnionsRefsAndClaims(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	f.ref(a, "gh:acme/api#1")
	f.ref(b, "gh:acme/api#508")
	claim := f.apply(event.ClaimAdd(b, "user:colleague", "", "user:test", event.SourceInferred, f.tick(), time.Time{}))
	f.merge(a, b)

	if refs := f.g.Nodes[a].Refs; len(refs) != 2 || !slices.Contains(refs, "gh:acme/api#508") {
		t.Errorf("survivor did not take the merged node's refs: %v", refs)
	}
	// The merged node's ref now resolves to the survivor, which is usually why
	// the merge happened at all.
	if got, err := f.g.Resolve("gh:acme/api#508"); err != nil || got.ID != a {
		t.Errorf("ref did not follow the merge: %v %v", got, err)
	}
	if claims := f.g.ClaimsFor(a); len(claims) != 1 || claims[0].Holder != "user:colleague" {
		t.Fatalf("claim did not move to the survivor: %v", claims)
	}
	// Claim.Node has to move too, or release looks in the bucket it left.
	f.apply(event.ClaimRelease(claim.ID(), "user:test", event.SourceAsserted, f.tick()))
	if claims := f.g.ClaimsFor(a); len(claims) != 0 {
		t.Errorf("releasing a moved claim left it in place: %v", claims)
	}
}

func TestMergeRedirectsLaterEvents(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("survivor"), f.node("duplicate"), f.node("other")
	f.merge(a, b)

	// Everything asserted about the merged node afterwards lands on the survivor.
	f.apply(event.NodeState(b, event.StateDone, "user:test", event.SourceAsserted, f.tick()))
	f.ref(b, "gh:acme/api#9")
	f.link(EdgeDependsOn, b, c)

	if f.g.Nodes[a].State != event.StateDone {
		t.Errorf("state asserted on the merged node did not reach the survivor")
	}
	if !slices.Contains(f.g.Nodes[a].Refs, "gh:acme/api#9") {
		t.Errorf("ref asserted on the merged node did not reach the survivor")
	}
	if deps := f.g.OutEdges(a, EdgeDependsOn); len(deps) != 1 || deps[0].To != c {
		t.Errorf("edge asserted from the merged node did not reach the survivor")
	}
	// An edge between the two is a self-edge once folded, so it never lands.
	f.link(EdgeDependsOn, b, a)
	if len(f.g.OutEdges(a, EdgeDependsOn)) != 1 {
		t.Errorf("a self-edge produced by the merge was stored")
	}
}

func TestMergeChainsCanonicalize(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("first"), f.node("second"), f.node("third")
	f.merge(b, c) // c -> b
	f.merge(a, b) // b -> a, so c -> a

	if got := f.g.Canonical(c); got != a {
		t.Errorf("chain resolved to %s, want %s", got[:7], a[:7])
	}
	if merged := f.g.MergedFrom(a); len(merged) != 2 {
		t.Errorf("MergedFrom found %d nodes, want both links of the chain", len(merged))
	}
}

func TestCheckMergeRejectsIntroducedCycle(t *testing.T) {
	f := newFixture(t)
	// a depends on x, and x depends on b. Neither graph has a cycle; folding b
	// into a produces a -> x -> a.
	a, b, x := f.node("survivor"), f.node("duplicate"), f.node("middle")
	f.link(EdgeDependsOn, a, x)
	f.link(EdgeDependsOn, x, b)

	err := f.g.CheckMerge(a, b)
	if err == nil {
		t.Fatal("merge closing a depends-on cycle was permitted")
	}
	cycle, ok := err.(*ErrCycle)
	if !ok {
		t.Fatalf("want *ErrCycle, got %T", err)
	}
	if cycle.Kind != EdgeDependsOn {
		t.Errorf("cycle reported for %s, want depends-on", cycle.Kind)
	}
	// The path has to name the route, not just the fact (SPEC.md §6.1).
	if len(cycle.Path) < 3 || cycle.Path[0] != a || cycle.Path[len(cycle.Path)-1] != a {
		t.Errorf("path does not run from the survivor back to itself: %v", cycle.Path)
	}
	if !slices.Contains(cycle.Path, x) {
		t.Errorf("path omits the node the cycle runs through: %v", cycle.Path)
	}
}

func TestCheckMergeAllowsCycleFreeAndSymmetricKinds(t *testing.T) {
	f := newFixture(t)
	a, b, x := f.node("survivor"), f.node("duplicate"), f.node("other")
	f.link(EdgeDependsOn, a, x)
	f.link(EdgeDependsOn, b, x)
	if err := f.g.CheckMerge(a, b); err != nil {
		t.Errorf("merge that closes nothing was rejected: %v", err)
	}

	// conflicts-with is symmetric and cyclic by nature; merging into a cycle
	// there is not a violation (SPEC.md §4.2).
	c, d := f.node("c"), f.node("d")
	f.link(EdgeConflictsWith, c, d)
	if err := f.g.CheckMerge(c, d); err != nil {
		t.Errorf("merge across a conflicts-with edge was rejected: %v", err)
	}
}

func TestMergeIntoSelfIsNoOp(t *testing.T) {
	f := newFixture(t)
	a := f.node("only")
	f.merge(a, a)

	if f.g.Nodes[a].MergedInto != "" {
		t.Errorf("a node was merged into itself")
	}
	if len(f.g.Ordered()) != 1 {
		t.Errorf("self-merge changed the listing")
	}
}
