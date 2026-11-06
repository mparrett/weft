package graph

import (
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
)

// CheckMerge must cover every acyclic kind, not just depends-on. Restricting it
// to the first kind left part-of, derived-from, and supersedes able to close a
// cycle permanently, and the whole suite stayed green.
func TestCheckMergeCoversEveryAcyclicKind(t *testing.T) {
	for _, kind := range acyclicKinds {
		f := newFixture(t)
		a, b, x := f.node("survivor"), f.node("duplicate"), f.node("middle")
		f.link(kind, a, x)
		f.link(kind, x, b)

		err := f.g.CheckMerge(a, b)
		if err == nil {
			t.Errorf("%s: a merge closing a cycle was permitted", kind)
			continue
		}
		if cycle, ok := err.(*ErrCycle); !ok || cycle.Kind != kind {
			t.Errorf("%s: reported %v", kind, err)
		}
	}
}

// Merging two nodes that already have an edge between them is the ordinary
// case — you linked them before noticing they were duplicates. The edge becomes
// a self-edge and is dropped, so it is not a cycle to refuse.
func TestCheckMergeAllowsAnEdgeBetweenTheMergedPair(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	f.link(EdgeDependsOn, a, b)

	if err := f.g.CheckMerge(a, b); err != nil {
		t.Fatalf("merging a pair that already had an edge was rejected: %v", err)
	}
	f.merge(a, b)
	if got := f.g.OutEdges(a, EdgeDependsOn); len(got) != 0 {
		t.Errorf("the folded self-edge was stored: %v", got)
	}
}

// EVENTS.md §3.7: a rewritten edge collapsing into an existing triple keeps the
// EARLIEST by file order. Keeping whichever already held the key reattributes
// the relation to whoever asserted it later, which is the provenance loss merge
// is specifically not allowed to cause.
func TestMergeTripleCollapseKeepsEarliest(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("survivor"), f.node("duplicate"), f.node("other")
	early := f.link(EdgeDependsOn, b, c) // asserted first; becomes a -> c
	f.link(EdgeDependsOn, a, c)          // asserted second, same triple after the fold
	f.merge(a, b)

	deps := f.g.OutEdges(a, EdgeDependsOn)
	if len(deps) != 1 {
		t.Fatalf("got %d edges, want them collapsed to 1", len(deps))
	}
	if deps[0].ID != early {
		t.Errorf("kept the later assertion; provenance now names the wrong asserter")
	}
	// And the indexes agree with the choice.
	if _, ok := f.g.Edges[early]; !ok {
		t.Error("the retained edge is missing from Edges")
	}
}

// An edge asserted as a self-edge is a fact the log states, and the fold reports
// what the log says. Only a self-edge *produced* by folding is an artifact.
func TestMergePreservesAnAssertedSelfEdge(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	self := f.link(EdgeConflictsWith, b, b)
	f.merge(a, b)

	if _, ok := f.g.Edges[self]; !ok {
		t.Error("a self-edge the log asserted was dropped by the merge")
	}
}

// Alias chains grow by one every time a survivor is itself merged. Past the hop
// cap, Canonical used to return a merged node — so later writes landed on
// something Ordered() hides and were silently lost.
func TestLongAliasChainStillResolves(t *testing.T) {
	f := newFixture(t)
	ids := []string{f.node("n0")}
	for i := 1; i <= maxAliasHops+20; i++ {
		next := f.node("n" + string(rune('a'+i%26)))
		f.merge(next, ids[len(ids)-1]) // previous survivor folds into the new node
		ids = append(ids, next)
	}
	final := ids[len(ids)-1]

	for _, id := range ids {
		if got := f.g.Canonical(id); got != final {
			t.Fatalf("%s resolved to a node that is itself merged (want %s)", got[:7], final[:7])
		}
	}
	// A write naming the oldest id must reach the survivor, not vanish.
	f.apply(event.NodeState(ids[0], event.StateDone, "user:test", event.SourceAsserted, f.tick()))
	if f.g.Nodes[final].State != event.StateDone {
		t.Error("a state assertion on a deeply-merged id did not reach the survivor")
	}
	if live := f.g.Ordered(); len(live) != 1 {
		t.Errorf("Ordered listed %d nodes, want 1", len(live))
	}
}

// The fold is a pure function of the bytes, so a node.merge that arrives before
// its endpoints must not leave alias and MergedInto disagreeing — that made
// Ordered() list two nodes for one piece of work while node.state on one still
// landed on the other.
func TestMergeBeforeNodeAddStaysConsistent(t *testing.T) {
	f := newFixture(t)
	// Build the ids without applying, so the merge can be folded first.
	a, errA := event.NodeAdd("ns", "task", "survivor", "user:test", event.SourceAsserted, f.tick())
	b, errB := event.NodeAdd("ns", "task", "duplicate", "user:test", event.SourceAsserted, f.tick())
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	f.apply(event.NodeMerge(a.ID(), b.ID(), "user:test", event.SourceAsserted, f.tick()))
	f.apply(a, nil)
	f.apply(b, nil)

	if f.g.Nodes[b.ID()].MergedInto != a.ID() {
		t.Error("alias and MergedInto disagree after an out-of-order merge")
	}
	if live := f.g.Ordered(); len(live) != 1 {
		t.Errorf("Ordered listed %d nodes for one piece of work", len(live))
	}
	if got, err := f.g.Resolve(b.ID()); err != nil || got.ID != a.ID() {
		t.Errorf("the merged id did not resolve to the survivor")
	}
}

// A merge copies the ref onto the survivor, so retracting it from only the node
// that asserted it leaves the survivor carrying a ref nobody asserts — still
// resolvable, and still handed to import as an existing binding.
func TestRefRetractAfterMergeClearsBoth(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	e := event.New(event.KindRefAdd, "user:test", event.SourceObserved, f.tick())
	e["node"], e["ref"] = b, "gh:acme/api#412"
	f.apply(e, e.Seal())
	f.merge(a, b)

	r := event.New(event.KindRefRetract, "user:test", event.SourceAsserted, f.tick())
	r["target"] = e.ID()
	f.apply(r, r.Seal())

	if len(f.g.Nodes[a].Refs) != 0 {
		t.Errorf("the survivor still carries a retracted ref: %v", f.g.Nodes[a].Refs)
	}
	if _, err := f.g.Resolve("gh:acme/api#412"); err == nil {
		t.Error("a retracted ref still resolves")
	}
}

// ClaimsFor promises file order, and `show` prints in the order it returns.
func TestClaimsKeepFileOrderAcrossMerge(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	f.apply(event.ClaimAdd(b, "user:early", "", "user:test", event.SourceAsserted, f.tick(), time.Time{}))
	f.apply(event.ClaimAdd(a, "user:late", "", "user:test", event.SourceAsserted, f.tick(), time.Time{}))
	f.merge(a, b)

	claims := f.g.ClaimsFor(a)
	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2", len(claims))
	}
	if claims[0].Holder != "user:early" || claims[1].Holder != "user:late" {
		t.Errorf("claims out of file order: %s then %s", claims[0].Holder, claims[1].Holder)
	}
}

// The survivor takes the ref, so it must take what upstream said about it.
// Losing the observation drops the age annotation from `show` and makes the
// next import write a spurious "changed upstream" against unchanged upstream.
func TestObservationsMoveWithTheMerge(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("survivor"), f.node("duplicate")
	ref := "gh:acme/api#412"
	f.ref(b, ref)
	f.apply(event.RefObserve(b, ref, event.ObservePayload{State: "open", Title: "upstream title"}, "user:test", event.SourceObserved, f.tick()))
	f.merge(a, b)

	o := f.g.Observed(a, ref)
	if o == nil {
		t.Fatal("the survivor lost the observation")
	}
	if o.State != "open" || o.At.IsZero() {
		t.Errorf("observation arrived incomplete: %+v", o)
	}
	// Looking it up by the merged id lands on the survivor's copy too.
	if f.g.Observed(b, ref) == nil {
		t.Error("the merged id no longer finds the observation")
	}
	// And it is reachable through the accessor `show` actually uses.
	if _, ok := f.g.ObservationsFor(a)[ref]; !ok {
		t.Error("ObservationsFor does not surface the moved observation")
	}
}

// Changed drives whether anything is written at all, so each field has to
// discriminate on its own — a field the comparison misses is a change the log
// silently never records.
func TestObservationChangedPerField(t *testing.T) {
	base := &Observation{
		State: "open", Title: "a title",
		Created: "2026-01-01T00:00:00Z", Modified: "2026-02-01T00:00:00Z",
		Hash: "sha256:aa",
	}
	same := event.ObservePayload{
		State: "open", Title: "a title",
		Created: "2026-01-01T00:00:00Z", Modified: "2026-02-01T00:00:00Z",
		Hash: "sha256:aa",
	}
	if base.Changed(same) {
		t.Error("an identical payload must not count as a change")
	}
	mutate := []struct {
		name string
		with func(p *event.ObservePayload)
	}{
		{"state", func(p *event.ObservePayload) { p.State = "closed" }},
		{"title", func(p *event.ObservePayload) { p.Title = "renamed" }},
		{"created", func(p *event.ObservePayload) { p.Created = "2025-12-31T00:00:00Z" }},
		{"modified", func(p *event.ObservePayload) { p.Modified = "2026-03-01T00:00:00Z" }},
		{"hash", func(p *event.ObservePayload) { p.Hash = "sha256:bb" }},
	}
	for _, tc := range mutate {
		p := same
		tc.with(&p)
		if !base.Changed(p) {
			t.Errorf("a %s change alone must count as a change", tc.name)
		}
	}
	// A touch — modified moved, hash did not — is still a change: the payload
	// records what upstream says, and judging it noise is the reader's call.
	touched := same
	touched.Modified = "2026-03-01T00:00:00Z"
	if !base.Changed(touched) {
		t.Error("modified moving with an unchanged hash must still count")
	}
	var absent *Observation
	if !absent.Changed(same) {
		t.Error("a first observation must count as new")
	}
}
