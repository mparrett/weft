package query

import (
	"testing"
	"time"

	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
)

var now = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

type fixture struct {
	g  *graph.Graph
	t  *testing.T
	id map[string]string
}

func newFixture(t *testing.T) *fixture {
	return &fixture{g: graph.New(), t: t, id: map[string]string{}}
}

func (f *fixture) node(title string) string {
	f.t.Helper()
	e, err := event.NodeAdd("test", "task", title, "user:t", event.SourceAsserted, now)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.Apply(e); err != nil {
		f.t.Fatal(err)
	}
	f.id[title] = e.ID()
	return e.ID()
}

func (f *fixture) nodeIn(title, ns string) string {
	f.t.Helper()
	e, err := event.NodeAdd(ns, "task", title, "user:t", event.SourceAsserted, now)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.Apply(e); err != nil {
		f.t.Fatal(err)
	}
	f.id[title] = e.ID()
	return e.ID()
}

func (f *fixture) link(kind, from, to string) {
	f.t.Helper()
	e, err := event.EdgeAdd(kind, from, to, "user:t", event.SourceAsserted, now)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.Apply(e); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) state(id, state string) {
	f.t.Helper()
	e, err := event.NodeState(id, state, "user:t", event.SourceAsserted, now.Add(time.Second))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.Apply(e); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) claim(id, holder string, expires time.Time) {
	f.t.Helper()
	e, err := event.ClaimAdd(id, holder, "", "user:t", event.SourceAsserted, now, expires)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.g.Apply(e); err != nil {
		f.t.Fatal(err)
	}
}

func titles(nodes []*graph.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Title
	}
	return out
}

func has(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Only open dependencies block. Treating dropped as a permanent block would
// leave dependents stuck with no way forward but a fake completion.
func TestBlocksByState(t *testing.T) {
	cases := map[string]bool{"open": true, "": true, "done": false, "dropped": false}
	for state, want := range cases {
		if got := Blocks(state); got != want {
			t.Errorf("Blocks(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestWhyNamesTheNearestBlocker(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.link(graph.EdgeDependsOn, a, b)
	f.link(graph.EdgeDependsOn, b, c)

	blockers := Why(f.g, a, now)
	if len(blockers) != 1 {
		t.Fatalf("got %d blockers, want 1 (nearest only)", len(blockers))
	}
	if blockers[0].Node.Title != "b" {
		t.Errorf("nearest blocker = %q, want b", blockers[0].Node.Title)
	}

	// Once b is resolved, the walk continues through it to c.
	f.state(b, event.StateDone)
	blockers = Why(f.g, a, now)
	if len(blockers) != 1 || blockers[0].Node.Title != "c" {
		t.Fatalf("expected c to surface once b was done, got %v", blockers)
	}
	// And the path records how we got there.
	if len(blockers[0].Path) != 3 {
		t.Errorf("path = %v, want three hops", blockers[0].Path)
	}
}

// The why-prints-blockers-as-peers repro: two nearest blockers where one waits on the other.
// Both are genuinely nearest, so Why is right to return both; the flat order is
// what makes them read as equally available work.
func TestRankPutsTheChokepointFirst(t *testing.T) {
	f := newFixture(t)
	a, c, b := f.node("a"), f.node("c"), f.node("b")
	f.link(graph.EdgeDependsOn, a, b)
	f.link(graph.EdgeDependsOn, a, c)
	f.link(graph.EdgeDependsOn, c, b)

	// c is created before b, so file order alone would list the gated one first.
	ranked := Rank(f.g, Why(f.g, a, now))
	if len(ranked) != 2 {
		t.Fatalf("got %d blockers, want 2", len(ranked))
	}
	if ranked[0].Node.Title != "b" || ranked[0].Gates != 1 {
		t.Errorf("first blocker = %q gating %d, want b gating 1", ranked[0].Node.Title, ranked[0].Gates)
	}
	if ranked[1].Node.Title != "c" || ranked[1].Gates != 0 {
		t.Errorf("second blocker = %q gating %d, want c gating 0", ranked[1].Node.Title, ranked[1].Gates)
	}
}

// Gates counts the blockers in this answer, not every dependent in the graph:
// ranking on nodes the output does not show would be evidence the reader cannot
// check. Here d waits on b too, but d is not blocking a.
func TestRankCountsOnlyTheBlockersItWasGiven(t *testing.T) {
	f := newFixture(t)
	a, b, c, d := f.node("a"), f.node("b"), f.node("c"), f.node("d")
	f.link(graph.EdgeDependsOn, a, b)
	f.link(graph.EdgeDependsOn, a, c)
	f.link(graph.EdgeDependsOn, c, b)
	f.link(graph.EdgeDependsOn, d, b)

	ranked := Rank(f.g, Why(f.g, a, now))
	if ranked[0].Gates != 1 {
		t.Errorf("gates = %d, want 1 — d waits on b but is not in this answer", ranked[0].Gates)
	}
}

// A resolved node between two blockers does not stop the far one gating the
// near one: the work still has to happen in that order.
func TestRankSeesThroughAResolvedHop(t *testing.T) {
	f := newFixture(t)
	a, b, c, mid := f.node("a"), f.node("b"), f.node("c"), f.node("mid")
	f.link(graph.EdgeDependsOn, a, b)
	f.link(graph.EdgeDependsOn, a, c)
	f.link(graph.EdgeDependsOn, c, mid)
	f.link(graph.EdgeDependsOn, mid, b)
	f.state(mid, event.StateDone)

	ranked := Rank(f.g, Why(f.g, a, now))
	if len(ranked) != 2 {
		t.Fatalf("got %d blockers, want 2", len(ranked))
	}
	if ranked[0].Node.Title != "b" || ranked[0].Gates != 1 {
		t.Errorf("first = %q gating %d, want b gating 1", ranked[0].Node.Title, ranked[0].Gates)
	}
}

// Equal counts keep the order the walk produced, or identical graphs stop
// printing identically (SPEC.md §9.5).
func TestRankIsStableWhenNothingGates(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.link(graph.EdgeDependsOn, a, b)
	f.link(graph.EdgeDependsOn, a, c)

	ranked := Rank(f.g, Why(f.g, a, now))
	if len(ranked) != 2 || ranked[0].Node.Title != "b" || ranked[1].Node.Title != "c" {
		t.Errorf("unranked order changed: %q, %q", ranked[0].Node.Title, ranked[1].Node.Title)
	}
	for _, r := range ranked {
		if r.Gates != 0 {
			t.Errorf("%q gates %d, want 0", r.Node.Title, r.Gates)
		}
	}
}

func TestWhyCarriesClaimHolders(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("a"), f.node("b")
	f.link(graph.EdgeDependsOn, a, b)
	f.claim(b, "user:colleague", now.Add(time.Hour))

	blockers := Why(f.g, a, now)
	if len(blockers) != 1 {
		t.Fatalf("got %d blockers", len(blockers))
	}
	if len(blockers[0].Claims) != 1 || blockers[0].Claims[0].Holder != "user:colleague" {
		t.Errorf("blocker did not carry the claim holder: %+v", blockers[0].Claims)
	}
	// An expired claim is not a current answer to "who has this".
	f2 := newFixture(t)
	x, y := f2.node("x"), f2.node("y")
	f2.link(graph.EdgeDependsOn, x, y)
	f2.claim(y, "agent:gone", now.Add(-time.Hour))
	if got := Why(f2.g, x, now)[0].Claims; len(got) != 0 {
		t.Errorf("expired claim reported as a holder: %+v", got)
	}
}

// A dependency we cannot resolve blocks: the edge is a known assertion, and its
// target being absent is exactly why the node is not safe to call ready.
func TestDanglingDependencyBlocks(t *testing.T) {
	f := newFixture(t)
	a := f.node("a")
	f.link(graph.EdgeDependsOn, a, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")

	blockers := Why(f.g, a, now)
	if len(blockers) != 1 {
		t.Fatalf("got %d blockers, want 1", len(blockers))
	}
	if blockers[0].Node != nil {
		t.Error("expected a nil Node for an unresolvable dependency")
	}
	if len(Ready(f.g, "user:t", now)) != 0 {
		t.Error("node with an unresolvable dependency reported as ready")
	}
}

// The rule review finding 6 named: namespace never limits traversal, so a
// cross-namespace blocker cannot be filtered into invisibility.
func TestReadinessCrossesNamespaces(t *testing.T) {
	f := newFixture(t)
	local := f.nodeIn("local work", "acme/api")
	upstream := f.nodeIn("upstream blocker", "other/fork")
	f.link(graph.EdgeDependsOn, local, upstream)

	if has(titles(Ready(f.g, "user:t", now)), "local work") {
		t.Error("a blocker in another namespace was ignored")
	}
	f.state(upstream, event.StateDone)
	if !has(titles(Ready(f.g, "user:t", now)), "local work") {
		t.Error("node still blocked after its cross-namespace dependency was done")
	}
}

func TestReadyExcludesTakenAndClosed(t *testing.T) {
	f := newFixture(t)
	f.node("free")
	taken := f.node("taken")
	mine := f.node("mine")
	finished := f.node("finished")

	f.claim(taken, "user:someone-else", now.Add(time.Hour))
	f.claim(mine, "user:t", now.Add(time.Hour))
	f.state(finished, event.StateDone)

	got := titles(Ready(f.g, "user:t", now))
	if !has(got, "free") {
		t.Error("unclaimed node missing from the frontier")
	}
	if has(got, "taken") {
		t.Error("node claimed by someone else listed as ready")
	}
	// Your own claim does not hide your work from you.
	if !has(got, "mine") {
		t.Error("your own claim removed the node from your frontier")
	}
	if has(got, "finished") {
		t.Error("closed node listed as ready")
	}
	// An expired claim frees the work again.
	f2 := newFixture(t)
	abandoned := f2.node("abandoned")
	f2.claim(abandoned, "agent:gone", now.Add(-time.Hour))
	if !has(titles(Ready(f2.g, "user:t", now)), "abandoned") {
		t.Error("expired claim still holding the node")
	}
}

func TestBlockedIsTheComplementOfReady(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("a"), f.node("b")
	f.link(graph.EdgeDependsOn, a, b)

	if got := titles(Blocked(f.g, now)); len(got) != 1 || got[0] != "a" {
		t.Errorf("blocked = %v, want [a]", got)
	}
	if has(titles(Ready(f.g, "user:t", now)), "a") {
		t.Error("a node cannot be both ready and blocked")
	}
}

func TestRDepsWalksTransitively(t *testing.T) {
	f := newFixture(t)
	base := f.node("base")
	mid := f.node("mid")
	top := f.node("top")
	f.link(graph.EdgeDependsOn, mid, base)
	f.link(graph.EdgeDependsOn, top, mid)

	deps := RDeps(f.g, base, 0, nil)
	if len(deps) != 2 {
		t.Fatalf("got %d dependents, want 2", len(deps))
	}
	if deps[0].Node.Title != "mid" || deps[0].Depth != 1 {
		t.Errorf("nearest dependent = %+v", deps[0])
	}
	if deps[1].Node.Title != "top" || deps[1].Depth != 2 {
		t.Errorf("transitive dependent = %+v", deps[1])
	}
	if got := RDeps(f.g, base, 1, nil); len(got) != 1 {
		t.Errorf("--depth 1 returned %d, want 1", len(got))
	}
}

// The route crosses kinds, because the motivating example needs it: a task
// depends on a review point that is part-of a PR.
func TestPathCrossesEdgeKinds(t *testing.T) {
	f := newFixture(t)
	task := f.node("task 7")
	z := f.node("review point Z")
	pr := f.node("PR 77")
	f.link(graph.EdgeDependsOn, task, z)
	f.link(graph.EdgePartOf, z, pr)

	hops := Path(f.g, task, pr, nil, Either)
	if len(hops) != 2 {
		t.Fatalf("got %d hops, want 2", len(hops))
	}
	if hops[0].Node.Title != "review point Z" || hops[1].Node.Title != "PR 77" {
		t.Errorf("unexpected route: %+v", hops)
	}
	// Restricting to one kind must break that route.
	if got := Path(f.g, task, pr, []string{graph.EdgeDependsOn}, Either); got != nil {
		t.Errorf("--via depends-on should find no route, got %d hops", len(got))
	}
}

func TestPathIsUndirectedAndShortest(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.link(graph.EdgeDependsOn, a, b)
	f.link(graph.EdgeDependsOn, c, b)

	// c → b is stored in the other direction, but the question is how a and c
	// relate, not which formally precedes.
	hops := Path(f.g, a, c, nil, Either)
	if len(hops) != 2 {
		t.Fatalf("got %d hops, want 2: %+v", len(hops), hops)
	}
	if !hops[0].Outbound || hops[1].Outbound {
		t.Errorf("direction not recorded per hop: %+v", hops)
	}
}

func TestPathEdgeCases(t *testing.T) {
	f := newFixture(t)
	a, b := f.node("a"), f.node("b")

	if got := Path(f.g, a, a, nil, Either); len(got) != 0 {
		t.Errorf("path to self should be empty, got %d hops", len(got))
	}
	if got := Path(f.g, a, b, nil, Either); got != nil {
		t.Errorf("unconnected nodes should have no route, got %+v", got)
	}
}

// A cycle in a non-acyclic kind must not hang the walk.
func TestPathTerminatesOnCycles(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.link(graph.EdgeConflictsWith, a, b)
	f.link(graph.EdgeConflictsWith, b, c)
	f.link(graph.EdgeConflictsWith, c, a)

	done := make(chan []Hop, 1)
	go func() { done <- Path(f.g, a, c, nil, Either) }()
	select {
	case hops := <-done:
		if len(hops) == 0 {
			t.Error("expected a route around the cycle")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Path did not terminate on a cyclic graph")
	}
}

// Reported when rdeps gained --via: RDeps extended its frontier over whatever kinds it was
// handed, so `--via conflicts-with` walked A-B-C and reported C as waiting on A.
// Nothing asserted that, and Dependents indents by depth — so it did not merely
// fail to rule the chain out, it drew it.
func TestRDepsStopsAtANonTransitiveKind(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.link(graph.EdgeConflictsWith, a, b)
	f.link(graph.EdgeConflictsWith, b, c)

	got := RDeps(f.g, a, 0, []string{graph.EdgeConflictsWith})
	if len(got) != 1 || got[0].Node.Title != "b" {
		t.Fatalf("got %v, want b alone — c has no edge to a", titlesOf(got))
	}
	if got[0].Depth != 1 {
		t.Errorf("depth = %d, want 1", got[0].Depth)
	}
}

// same-as is symmetric too, but genuinely transitive, so the chain is real.
func TestRDepsWalksATransitiveSymmetricKind(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.node("a"), f.node("b"), f.node("c")
	f.link(graph.EdgeSameAs, a, b)
	f.link(graph.EdgeSameAs, b, c)

	if got := RDeps(f.g, a, 0, []string{graph.EdgeSameAs}); len(got) != 2 {
		t.Errorf("got %v, want both — same-as is transitive", titlesOf(got))
	}
}

func titlesOf(deps []Dependent) []string {
	out := make([]string, len(deps))
	for i, d := range deps {
		if d.Node != nil {
			out[i] = d.Node.Title
		}
	}
	return out
}
