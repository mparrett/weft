// Package query answers the questions the CLI exists to answer: what can I
// start, what is stopping me, who is waiting on me, and how are these related.
//
// Every traversal here is **global**. Namespace selects which nodes appear as
// results; it never limits the walk. Filtering before traversal is what would
// let `ls --ready` hide a cross-namespace blocker and declare a node ready when
// it is not (SPEC.md §5) — and the motivating story crosses repos, so that is
// the common case rather than an edge case.
package query

import (
	"slices"
	"sort"
	"time"

	"github.com/mparrett/weft/internal/graph"
)

// Blocks reports whether a dependency in this state stops dependents.
//
// Only `open` blocks. `done` is resolved; `dropped` is resolved too — it means
// someone decided against the work, and treating that as a permanent block
// would leave dependents stuck with no way forward but a fake completion.
func Blocks(state string) bool {
	return state == "" || state == "open"
}

// Container is the node kind that groups work rather than being work. The
// frontier queries skip it: a workstream has no depends-on edges by design, so
// it would always look ready, and an agent orienting from ls --ready would be
// offered a container to claim or close while its children are still open.
const Container = "workstream"

// Blocker is one reason a node cannot start, with the route to it.
//
// Gates is filled in by Rank, not by Why: it is a property of the answer as a
// whole rather than of the blocker, and the callers that only ask whether the
// set is empty should not pay for it.
type Blocker struct {
	Path   []string // node ids from the queried node to the blocker
	Node   *graph.Node
	ID     string // set even when Node is nil, for a dangling dependency
	Claims []*graph.Claim
	Gates  int // how many of the other blockers in this answer wait on this one
}

// Why returns the nearest unresolved dependencies of a node, each with the path
// that reaches it and whoever holds a live claim on it.
//
// Nearest, not all: the transitive set is noise once you know the first thing
// to go fix. The claim holder is included because "blocked" without a name is
// half an answer — the next action is usually to go talk to whoever holds it.
func Why(g *graph.Graph, id string, now time.Time) []Blocker {
	var out []Blocker
	seen := map[string]bool{id: true}
	type step struct {
		id   string
		path []string
	}
	queue := []step{{id: id, path: []string{id}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, e := range g.OutEdges(cur.id, graph.EdgeDependsOn) {
			if seen[e.To] {
				continue
			}
			seen[e.To] = true
			path := append(append([]string(nil), cur.path...), e.To)

			dep, known := g.Nodes[e.To]
			// A dependency we cannot resolve blocks. The edge is a known
			// assertion; that its target is absent is exactly why it is not
			// safe to call the node ready.
			if !known {
				out = append(out, Blocker{Path: path, ID: e.To})
				continue
			}
			if Blocks(dep.State) {
				out = append(out, Blocker{
					Path: path, Node: dep, ID: dep.ID,
					Claims: g.LiveClaims(dep.ID, now),
				})
				continue
			}
			// Resolved, so keep walking through it.
			queue = append(queue, step{id: e.To, path: path})
		}
	}
	return out
}

// Rank fills in Gates and puts the blockers that gate other listed blockers
// first, most first.
//
// Why returns the *nearest* open blockers, which is a set and not an order, so
// they print as peers. When one of them waits on another, a reader who starts on
// the wrong one does work the other will invalidate — and nothing in a flat list
// says which. That is not a rare shape: on a real graph one item held nine of
// them where the next-highest anywhere had one, so it was a single chokepoint
// rather than a gradient. `rdeps` finds it from the other end, but only if you
// already suspect the node; `why` is the command you reach for when you do not.
//
// Gates counts only the blockers in this answer, not every dependent in the
// graph. The question is which of the things in front of you to start, so a
// count over nodes you were not shown would rank on evidence the output does not
// carry.
//
// Kept out of Why because Ready and Blocked call Why once per node and only ask
// whether the result is empty; this walk is per blocker, and folding it in would
// multiply the cost of `ls --ready` for an answer neither one reads.
func Rank(g *graph.Graph, blockers []Blocker) []Blocker {
	if len(blockers) < 2 {
		return blockers
	}
	at := make(map[string]int, len(blockers))
	for i, b := range blockers {
		at[b.ID] = i
	}
	for _, b := range blockers {
		// Every blocker this one waits on, resolved states included: a done node
		// between them does not stop the one behind it from gating this one.
		for id := range reachable(g, b.ID) {
			if j, ok := at[id]; ok && id != b.ID {
				blockers[j].Gates++
			}
		}
	}
	// Stable, so equal counts keep the file order the walk produced. Identical
	// graphs must print byte-identically (SPEC.md §9.5).
	sort.SliceStable(blockers, func(i, j int) bool {
		return blockers[i].Gates > blockers[j].Gates
	})
	return blockers
}

// reachable returns everything a node transitively depends on.
func reachable(g *graph.Graph, id string) map[string]bool {
	seen := map[string]bool{}
	frontier := []string{id}
	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		for _, e := range g.OutEdges(cur, graph.EdgeDependsOn) {
			if seen[e.To] {
				continue
			}
			seen[e.To] = true
			frontier = append(frontier, e.To)
		}
	}
	return seen
}

// Ready returns nodes with no known blocker, in file order.
//
// **Optimistic by definition.** A deliberately incomplete graph cannot
// establish that work is ready, only that nothing recorded says otherwise
// (SPEC.md §6.1). An agent that reads this as a guarantee will start blocked
// work and blame the graph.
func Ready(g *graph.Graph, me string, now time.Time) []*graph.Node {
	var out []*graph.Node
	for _, n := range g.Ordered() {
		if !Blocks(n.State) || n.Kind == Container {
			continue
		}
		if len(Why(g, n.ID, now)) > 0 {
			continue
		}
		// Someone else's live claim means the work is taken, not that it is
		// blocked — but it should not show up as something to pick up.
		taken := false
		for _, c := range g.LiveClaims(n.ID, now) {
			if c.Holder != me {
				taken = true
				break
			}
		}
		if taken {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Blocked returns open nodes with at least one unresolved dependency. Like
// Ready, it skips containers.
func Blocked(g *graph.Graph, now time.Time) []*graph.Node {
	var out []*graph.Node
	for _, n := range g.Ordered() {
		if Blocks(n.State) && n.Kind != Container && len(Why(g, n.ID, now)) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// Dependent is a node that transitively depends on the queried one.
//
// Parent is the node this row was reached from — the queried node at depth 1.
// Vias is every walked kind with a live edge between the row and Parent, in
// sorted order. Both exist because a widened walk (--via depends-on,part-of)
// otherwise merges "waits on this" with "is part of a thing that does" into
// one indented tree — asserting a dependency the edges do not carry.
// One kind per row is not enough: when two kinds both connect
// the pair, reporting whichever the walk crossed first asserts it as *the*
// relation and lets the answer vary with the order the caller typed --via.
type Dependent struct {
	Node   *graph.Node
	ID     string
	Parent string
	Depth  int
	Vias   []string
}

// RDeps returns the transitive reverse closure over the given edge kinds:
// everything waiting on this node, nearest first. depth <= 0 means unlimited.
//
// Kinds defaults to depends-on alone, which is what the name means and the
// narrow answer worth having by default. On a real map most edges are part-of
// and derived-from, though, so the default silence reads as "nothing depends on
// this" when it means "nothing depends on this *through depends-on*" — which is
// why callers are expected to say which kinds they walked, exactly as Path does.
func RDeps(g *graph.Graph, id string, depth int, kinds []string) []Dependent {
	if len(kinds) == 0 {
		kinds = []string{graph.EdgeDependsOn}
	}
	var out []Dependent
	seen := map[string]bool{id: true}
	frontier := []string{id}

	for level := 1; len(frontier) > 0 && (depth <= 0 || level <= depth); level++ {
		var next []string
		for _, cur := range frontier {
			for _, k := range kinds {
				for _, e := range g.InEdges(cur, k) {
					// The far endpoint, not e.From. A symmetric edge is reported
					// from either side, so its From can be the node we are
					// standing on — taking it blind would walk to ourselves.
					other := e.From
					if other == cur {
						other = e.To
					}
					if seen[other] {
						continue
					}
					seen[other] = true
					out = append(out, Dependent{
						Node: g.Nodes[other], ID: other, Parent: cur, Depth: level,
						Vias: viasBetween(g, other, cur, kinds),
					})
					// Report it, but do not walk on through it unless the kind
					// is transitive. A conflicts-with B and B conflicts-with C
					// says nothing about A and C, so extending the frontier over
					// that kind reports a relationship nobody asserted — and
					// Dependents indents by depth, so it does not merely fail to
					// rule the chain out, it draws it.
					if graph.Transitive(k) {
						next = append(next, other)
					}
				}
			}
		}
		frontier = next
	}
	return out
}

// viasBetween reports every walked kind with a live edge from a row to the
// node it was reached from, in the row's own outbound direction — the reading
// the renderer prints. Sorted, so the row's content never depends on the
// order the caller typed --via: discovery order decides which kind found the
// row first, and reporting that one alone would assert it as *the* relation
// while silently dropping the rest.
func viasBetween(g *graph.Graph, row, parent string, kinds []string) []string {
	var out []string
	for _, k := range kinds {
		// FindEdge keys the way edge.add does, so a symmetric kind matches
		// from either end and an asymmetric one only in the row→parent
		// direction, which is what the outbound reading claims.
		if g.FindEdge(k, row, parent) != nil && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// crumb records how a node was reached, for rebuilding the route.
type crumb struct {
	prev string
	hop  Hop
}

// Hop is one step along a route between two nodes.
type Hop struct {
	Edge     *graph.Edge
	Node     *graph.Node
	ID       string
	Outbound bool // whether the edge was followed in its stored direction
}

// Direction restricts which way a walk may cross an edge.
//
// Either keeps Path a connectivity query, which is the default because the usual
// question is how two pieces of work are related. It is not the question that
// `--via depends-on` invites, though: a route that reaches the target through a
// *shared* dependency reads as a dependency, and the reversed hop label is the
// only thing saying otherwise. Forward and Backward turn the same command into a
// reachability query, so "does A depend on B" has an answer that cannot be
// misread.
type Direction int

const (
	Either   Direction = iota // cross an edge from either endpoint
	Forward                   // only from -> to, as the edge was asserted
	Backward                  // only to -> from
)

// Path returns the shortest route between two nodes, or nil if none exists.
//
// Kinds restricts which edge kinds the walk may cross; empty means every kind.
// Dir restricts which way it may cross them.
//
// All kinds are traversed by default rather than the link family alone. The
// motivating example requires it: a task depends on a review point that is
// part-of a PR, so restricting to link-family edges would report no route
// between the task and the PR — which is plainly wrong to anyone looking at it.
func Path(g *graph.Graph, from, to string, kinds []string, dir Direction) []Hop {
	if from == to {
		return []Hop{}
	}
	allowed := map[string]bool{}
	for _, k := range kinds {
		allowed[k] = true
	}
	crossable := func(n graph.Neighbor) bool {
		if len(allowed) > 0 && !allowed[n.Edge.Kind] {
			return false
		}
		// A symmetric kind stays crossable both ways under a directional walk:
		// its stored orientation is the fold's normalization, not an assertion,
		// so honouring it would answer a direction question with a coin flip.
		if dir == Either || graph.Symmetric(n.Edge.Kind) {
			return true
		}
		return n.Outbound == (dir == Forward)
	}

	trail := map[string]crumb{}
	seen := map[string]bool{from: true}
	frontier := []string{from}

	for len(frontier) > 0 {
		var next []string
		for _, cur := range frontier {
			for _, n := range g.Neighbors(cur) {
				if !crossable(n) || seen[n.ID] {
					continue
				}
				seen[n.ID] = true
				trail[n.ID] = crumb{prev: cur, hop: Hop{
					Edge: n.Edge, Node: n.Node, ID: n.ID, Outbound: n.Outbound,
				}}
				if n.ID == to {
					return rebuild(trail, from, to)
				}
				next = append(next, n.ID)
			}
		}
		frontier = next
	}
	return nil
}

func rebuild(trail map[string]crumb, from, to string) []Hop {
	var hops []Hop
	for cur := to; cur != from; {
		c, ok := trail[cur]
		if !ok {
			break
		}
		hops = append(hops, c.hop)
		cur = c.prev
	}
	// Built backwards from the destination.
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	return hops
}
