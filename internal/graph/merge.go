package graph

import (
	"slices"

	"github.com/mparrett/weft/internal/event"
)

// maxAliasHops is how far the cheap untracked alias walk goes before switching
// to the cycle-guarded one. It bounds allocation, never correctness.
const maxAliasHops = 64

// Canonical follows the alias chain to the node that survives. An id that was
// never merged is returned unchanged, so this is safe to call on anything.
//
// The walk compresses as it goes. Without that, chains grow by one every time a
// survivor is itself later merged — an ordinary dedupe pattern — and two things
// break: the hop cap starts returning a *merged* node, so later assertions land
// somewhere `Ordered()` hides and are silently lost; and every node reference in
// every event pays O(depth). Compression keeps the map flat, which is also what
// makes the cap unreachable by any chain merge can build.
func (g *Graph) Canonical(id string) string {
	// Overwhelmingly the common case: no merge has ever been folded, so there
	// is no chain to walk and nothing to compress.
	if len(g.alias) == 0 {
		return id
	}

	root, hops := id, 0
	for {
		next, ok := g.alias[root]
		if !ok {
			break
		}
		root = next
		hops++
		// Past the cheap bound the walk switches to tracking visited nodes.
		// The bound is NOT a correctness limit — stopping early would return a
		// node that is itself merged, and every later assertion naming it would
		// land somewhere Ordered() hides, exiting 0 while losing the write.
		if hops > maxAliasHops {
			return g.canonicalTracked(root)
		}
	}
	for id != root {
		next := g.alias[id]
		g.alias[id] = root
		id = next
		if id == "" {
			break
		}
	}
	return root
}

// canonicalTracked finishes a walk long enough to be worth guarding against a
// cycle. Merge cannot build one — it only ever aliases two nodes that are
// already canonical and already distinct — but a hand-edited or third-party log
// can, and tolerance means terminating rather than hanging (SPEC.md §9.3).
func (g *Graph) canonicalTracked(id string) string {
	seen := map[string]bool{id: true}
	for {
		next, ok := g.alias[id]
		if !ok || seen[next] {
			return id
		}
		seen[next] = true
		id = next
	}
}

// MergedFrom returns the nodes that were folded into this one, in file order.
//
// Computed rather than indexed: it is read once by `show` and never inside the
// fold, and a second index is a second thing that can disagree with the log.
func (g *Graph) MergedFrom(id string) []*Node {
	var out []*Node
	for _, nid := range g.order {
		n, ok := g.Nodes[nid]
		if ok && n.MergedInto != "" && g.Canonical(nid) == id {
			out = append(out, n)
		}
	}
	return out
}

// pendingMerge describes a merge that has not been applied, so a traversal can
// answer questions about the graph the merge would produce. The zero value
// means no merge is pending.
type pendingMerge struct{ from, into string }

func (p pendingMerge) view(id string) string {
	if p.from != "" && id == p.from {
		return p.into
	}
	return id
}

// outMerged returns the edges leaving a node in the pending merge's view: the
// survivor inherits the merged node's outbound edges.
func (g *Graph) outMerged(id string, pending pendingMerge) []*Edge {
	if pending.from == "" || id != pending.into {
		return g.out[id]
	}
	out := make([]*Edge, 0, len(g.out[pending.into])+len(g.out[pending.from]))
	return append(append(out, g.out[pending.into]...), g.out[pending.from]...)
}

// CheckMerge reports whether folding from into into is permitted.
//
// A merge can close a cycle that neither node had. Every path the merge creates
// runs through the survivor, because it is the only node whose adjacency
// changes — so it is enough to ask whether the survivor can reach itself once
// the fold is applied.
//
// Like CheckEdge, this must run inside the write transaction, against the same
// fold the append lands on (EVENTS.md §6).
func (g *Graph) CheckMerge(into, from string) error {
	pending := pendingMerge{from: from, into: into}
	for _, kind := range acyclicKinds {
		for _, e := range g.outMerged(into, pending) {
			if e.Kind != kind {
				continue
			}
			next := pending.view(e.To)
			// A self-edge is dropped by the fold rather than stored, so it is
			// not a cycle to report.
			if next == into {
				continue
			}
			if path := g.findPath(kind, next, into, pending); path != nil {
				return &ErrCycle{Kind: kind, Path: append([]string{into}, path...)}
			}
		}
	}
	return nil
}

// applyMerge folds one node into another (EVENTS.md §3.7).
//
// Nothing in the log is rewritten and nothing is lost: the merged node keeps
// every field it was asserted with, and only the index moves. That is what
// makes "the surviving node's fields win" a safe rule rather than a lossy one —
// the values that lost are still readable on a node that still resolves.
func (g *Graph) applyMerge(e event.Event) {
	from, into := g.Canonical(e.Str("from")), g.Canonical(e.Str("into"))
	// Both empty-id and already-merged land here. Neither is an error: the
	// desired state already holds (SPEC.md §9.3).
	if from == "" || into == "" || from == into {
		return
	}
	g.alias[from] = into

	src, dst := g.Nodes[from], g.Nodes[into]
	if src != nil {
		src.MergedInto = into
	}
	// Refs union. A node is usually merged *because* of its refs, so this is
	// the part that makes the survivor findable by what the duplicate knew.
	if src != nil && dst != nil {
		for _, r := range src.Refs {
			if !slices.Contains(dst.Refs, r) {
				dst.Refs = append(dst.Refs, r)
			}
		}
	}
	for _, edge := range g.edgesTouching(from) {
		g.repoint(edge, from, into)
	}
	// Claim.Node moves with the claim, or a later claim.release would look for
	// it in the bucket it just left. Merged by Seq rather than appended, so the
	// result stays in file order — ClaimsFor promises that, and `show` prints
	// in the order it returns.
	for _, c := range g.claimsByNode[from] {
		c.Node = into
	}
	g.claimsByNode[into] = mergeBySeq(g.claimsByNode[into], g.claimsByNode[from])
	delete(g.claimsByNode, from)
	// Released claims are history the survivor now owns too. Missed, the
	// duplicate's attach intervals vanish from export with nothing failing.
	for _, c := range g.releasedByNode[from] {
		c.Node = into
	}
	if len(g.releasedByNode[from]) > 0 {
		g.releasedByNode[into] = append(g.releasedByNode[into], g.releasedByNode[from]...)
		g.releasedUnsorted[into] = true
	}
	delete(g.releasedByNode, from)
	delete(g.releasedUnsorted, from)

	g.moveObservations(from, into)

	// Recency moves with everything else. The survivor now owns the merged
	// node's history, so work done under the duplicate's id has to keep counting
	// as work on the survivor — miss this and merging a node that moved this
	// morning into one that has sat still for a month makes the morning's work
	// vanish from every recency listing, with nothing failing.
	//
	// Created is not merged: it is when *this* node was asserted, and the
	// duplicate's own Created stays readable on the duplicate, which still
	// resolves. Taking the earlier of the two would claim the survivor existed
	// before anyone asserted it.
	if src != nil && dst != nil && src.Touched.After(dst.Touched) {
		dst.Touched = src.Touched
	}
	g.touch(into, eventAt(e))
}

// mergeBySeq interleaves two file-ordered claim slices back into file order.
func mergeBySeq(a, b []*Claim) []*Claim {
	out := make([]*Claim, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Seq <= b[j].Seq {
			out = append(out, a[i])
			i++
			continue
		}
		out = append(out, b[j])
		j++
	}
	return append(append(out, a[i:]...), b[j:]...)
}

// edgesTouching snapshots every edge with an endpoint at id, deduplicated.
// A snapshot rather than a live view because repoint mutates the very
// adjacency slices this walks.
func (g *Graph) edgesTouching(id string) []*Edge {
	out := make([]*Edge, 0, len(g.out[id])+len(g.in[id]))
	seen := make(map[string]bool, cap(out))
	for _, e := range g.out[id] {
		seen[e.ID] = true
		out = append(out, e)
	}
	for _, e := range g.in[id] {
		if !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out
}

// repoint moves an edge's endpoint from a merged node to its survivor, applying
// the same rules edge.add applies to a freshly asserted edge.
func (g *Graph) repoint(e *Edge, from, into string) {
	// Whether this was already a self-edge before the merge touched it. An edge
	// asserted as A->A is a fact the log states and the fold reports it
	// (EVENTS.md §3.7); only a self-edge *produced* by folding is an artifact.
	wasSelf := e.From == e.To

	g.dropEdge(e)
	if e.From == from {
		e.From = into
	}
	if e.To == from {
		e.To = into
	}
	if e.From == e.To && !wasSelf {
		return
	}
	key := tripleKey(e.Kind, e.From, e.To)
	// Collapsing into an edge that already asserts this triple is the same rule
	// edge.add uses, and keeps the earliest by file order (EVENTS.md §3.2).
	// Comparing Seq is what makes that true: the surviving edge has to be the
	// one asserted first, not whichever happened to already hold the key, or
	// the relation gets reattributed to whoever asserted it *later* — the exact
	// provenance loss §3.7 says merge must not cause.
	if held, dup := g.triples[key]; dup {
		other := g.Edges[held]
		if other != nil && other.Seq <= e.Seq {
			return
		}
		if other != nil {
			g.dropEdge(other)
		}
	}
	g.Edges[e.ID] = e
	g.triples[key] = e.ID
	g.out[e.From] = append(g.out[e.From], e)
	g.in[e.To] = append(g.in[e.To], e)
}
