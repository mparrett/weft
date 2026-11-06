// Package graph materializes the log into nodes and edges (docs/SPEC.md §4).
//
// Milestone 1 implements only what the write transaction needs to validate:
// node and edge existence, retraction, state, and depends-on acyclicity. The
// query surface arrives with milestones 4 and 6.
package graph

import (
	"fmt"
	"slices"
	"time"

	"github.com/mparrett/weft/internal/event"
)

// Edge kinds (SPEC.md §4.2). Acyclicity is a property per kind, not global — a
// global constraint would reject conflicts-with, which is symmetric by nature.
const (
	EdgeDependsOn     = "depends-on"
	EdgeConflictsWith = "conflicts-with"
	EdgeDerivedFrom   = "derived-from"
	EdgePartOf        = "part-of"
	EdgeSameAs        = "same-as"
	EdgeSupersedes    = "supersedes"
)

// symmetric edge kinds have no meaningful direction. The log still records the
// assertion exactly as it was made — it is a record of what someone said — but
// the fold normalizes for deduplication and indexes both endpoints so traversal
// finds the edge from either side. This settles the storage and traversal
// question the design review left open for conflicts-with.
var symmetric = map[string]bool{
	EdgeConflictsWith: true,
	EdgeSameAs:        true,
}

// Symmetric reports whether an edge kind carries no direction. Callers that
// restrict a walk to one direction need this: there is no direction to restrict
// on a symmetric kind, so pinning one would pick the fold's normalized endpoint
// order, which is an artifact of deduplication rather than something anyone
// asserted.
func Symmetric(kind string) bool { return symmetric[kind] }

// transitive edge kinds are the ones a multi-hop walk says something true
// about. Every kind here is transitive except `conflicts-with`: A conflicts with
// B and B conflicts with C implies nothing whatever about A and C, so following
// the chain manufactures a relationship nobody asserted.
//
// Symmetry and transitivity are independent properties and the pair is what
// makes `conflicts-with` singular — `same-as` is symmetric *and* transitive, so
// it walks fine. Keeping this beside `symmetric` rather than folding the two
// together is the point: a walker needs to ask them separately.
var transitive = map[string]bool{
	EdgeDependsOn:     true,
	EdgeConflictsWith: false,
	EdgeDerivedFrom:   true,
	EdgePartOf:        true,
	EdgeSameAs:        true,
	EdgeSupersedes:    true,
}

// Transitive reports whether following two edges of this kind in sequence
// asserts anything about the endpoints. A traversal that extends its frontier
// over a non-transitive kind reports relationships that were never written down.
func Transitive(kind string) bool { return transitive[kind] }

// Acyclic reports whether a cycle in the given edge kind is forbidden.
var acyclic = map[string]bool{
	EdgeDependsOn:     true,
	EdgeConflictsWith: false,
	EdgeDerivedFrom:   true,
	EdgePartOf:        true,
	EdgeSameAs:        false,
	EdgeSupersedes:    true,
}

// acyclicKinds is the same set in a fixed order, for checks that scan every
// kind. Ranging over the map would report a different kind on each run when a
// merge closes cycles in more than one, which is exactly the kind of drift
// canonical printing exists to prevent (SPEC.md §9.5). TestAcyclicKindsAgree
// keeps the two in step.
var acyclicKinds = []string{EdgeDependsOn, EdgeDerivedFrom, EdgePartOf, EdgeSupersedes}

// Node is a materialized assertion that a unit of work exists.
//
// MergedInto is set when this node was folded into another (SPEC.md §6.2). Such
// a node keeps every field it was asserted with — merging moves the *index*,
// never the assertions — so the values that lost the merge stay readable on a
// node that is still addressable.
// Created is when node.add was asserted. Touched is the latest assertion weft
// itself made about the node — state, edges, refs, claims, merges — and is what
// "what moved this week" means.
//
// Observations are deliberately excluded from Touched. A ref.observe records
// that we *looked* at upstream, not that the work moved, and letting a scheduled
// import float a node to the top of a recency listing would make polling look
// like progress (SPEC.md §10 — upstream is cached, never authoritative).
//
// Both are derived from event `at`, which is a wall clock and therefore only a
// hint: file order remains the log's total order (EVENTS.md §5.1). Anything that
// must be deterministic orders by file order and uses these to break ties, never
// the reverse.
// By and Source are the provenance of the node.add itself (SPEC.md §4.4) and
// are never revised by later events, the same way Created is not: a node.state
// or ref.add says something *about* the node, each carrying its own
// provenance, while By/Source record who asserted the node into existence and
// how they knew. The log has carried both fields all along; the fold dropped
// them, which left `--source inferred` written but unreadable through every
// interface.
type Node struct {
	ID         string
	NS         string
	Kind       string
	Title      string
	State      string
	By         string
	Source     string
	Refs       []string
	MergedInto string

	Created time.Time
	Touched time.Time
}

// Edge is a materialized typed relation. ID is the id of the edge.add event,
// which is what edge.retract targets.
//
// Seq is the edge's position in file order, which is the log's total order
// (EVENTS.md §5.1). It exists because "earliest by file order wins" is a
// normative tie-break (§3.2, §3.7) and neither the event id nor `at` can stand
// in for it — ids are hashes and wall clocks move backwards.
// By, Source and At are the edge's provenance, carried for the same reason
// nodes and claims carry theirs (SPEC.md §4.4): roughly half a real graph is
// inference, and one that cannot separate observed from guessed goes unread.
// The fold held them nowhere until now, so every consumer downstream — the
// export, and the viewer through it — drew an inferred edge exactly like an
// asserted one.
type Edge struct {
	ID   string
	Kind string
	From string
	To   string
	Seq  int

	By     string
	Source string
	At     time.Time
}

// Graph is the fold of a log.
type Graph struct {
	Nodes map[string]*Node
	Edges map[string]*Edge

	// triples indexes (kind, from, to) -> edge id for duplicate detection, and
	// out indexes from -> edges for traversal. Both exist because scanning
	// Edges linearly makes the fold O(n^2); at 10k events that measured 56x
	// over the BUILD.md §1.4 budget.
	triples map[string]string
	out     map[string][]*Edge
	in      map[string][]*Edge

	// order preserves file order, which is the log's total order (EVENTS.md
	// §5.1). Map iteration would make output non-deterministic and silently
	// break the canonical-printing constraint (SPEC.md §9.5).
	order []string

	// refs maps a ref.add event id to what it attached, so ref.retract can
	// undo exactly that assertion.
	refs map[string]refEntry

	// Claims is keyed by claim.add event id; claimsByNode indexes the same
	// claims for lookup, in file order.
	Claims       map[string]*Claim
	claimsByNode map[string][]*Claim
	// releasedByNode holds claims a claim.release let go, per node. Kept out of
	// Claims so nothing that reads current ownership can see them by accident.
	// Buckets are filled in release order and put into claim file order on
	// read (ReleasedClaimsFor), which releasedUnsorted marks as pending.
	releasedByNode   map[string][]*Claim
	releasedUnsorted map[string]bool

	// alias maps a merged node to the one it was folded into (merge.go).
	alias map[string]string

	// observations holds the latest ref.observe per node, then per ref
	// (observe.go). Nested rather than flat-keyed so a merge can move a node's
	// whole set in one step, the way claims move.
	observations map[string]map[string]*Observation

	// seq counts applied events, stamping file order onto edges and claims.
	seq int
}

type refEntry struct {
	node string
	ref  string
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{
		Nodes:   make(map[string]*Node),
		Edges:   make(map[string]*Edge),
		triples: make(map[string]string),
		out:     make(map[string][]*Edge),
		in:      make(map[string][]*Edge),
		refs:    make(map[string]refEntry),

		Claims:       make(map[string]*Claim),
		claimsByNode: make(map[string][]*Claim),
		alias:        make(map[string]string),

		releasedByNode:   make(map[string][]*Claim),
		releasedUnsorted: make(map[string]bool),
		observations:     make(map[string]map[string]*Observation),
	}
}

func tripleKey(kind, from, to string) string {
	// Symmetric kinds normalize endpoint order so that asserting A-B and then
	// B-A collapses to one edge instead of two records of the same fact.
	if symmetric[kind] && from > to {
		from, to = to, from
	}
	return kind + "\x00" + from + "\x00" + to
}

// Apply folds one event in. Unknown event kinds are ignored rather than
// rejected (EVENTS.md §1.3): an uninterpretable fact is not corruption.
//
// Events referencing unknown nodes are retained with the reference dangling,
// because a deliberately incomplete graph is the normal state (SPEC.md §9.3).
func (g *Graph) Apply(e event.Event) error {
	g.seq++
	at := eventAt(e)
	switch e.Kind() {
	case event.KindNodeAdd:
		id := e.ID()
		if _, ok := g.Nodes[id]; ok {
			return nil
		}
		n := &Node{
			ID:      id,
			NS:      e.Str("ns"),
			Kind:    e.Str("kind"),
			Title:   e.Str("title"),
			State:   event.StateOpen,
			By:      e.Str("by"),
			Source:  e.Str("source"),
			Created: at,
			Touched: at,
		}
		// A node.merge can arrive before the node it merges — a concatenated or
		// third-party log is under no obligation to order them (EVENTS.md §5.2).
		// Without this the alias map and MergedInto disagree, and the two
		// disagree *visibly*: Ordered() would list both nodes while node.state
		// on one still lands on the other.
		if into := g.Canonical(id); into != id {
			n.MergedInto = into
		}
		g.Nodes[id] = n
		g.order = append(g.order, id)
		// Also touch through the alias map, which matters only when the merge
		// arrived first — the case handled two lines up. Setting Touched on this
		// struct alone would leave the survivor unaware of work asserted under an
		// id it had already absorbed, and the fold would then depend on arrival
		// order, which is precisely what taking the maximum exists to prevent
		// (EVENTS.md §5.4). A no-op for an unmerged node.
		g.touch(id, at)

	case event.KindEdgeAdd:
		kind := e.Str("kind")
		rawFrom, rawTo := e.Str("from"), e.Str("to")
		from, to := g.Canonical(rawFrom), g.Canonical(rawTo)
		// A merge can leave an edge pointing at itself. That is an artifact of
		// folding two nodes into one, not something anyone asserted, so it is
		// dropped. An edge asserted as a self-edge in the first place is kept:
		// the log said it, and the fold reports what the log says (SPEC.md §9.3).
		if from == to && rawFrom != rawTo {
			return nil
		}
		// Identical triples collapse, earliest by file order retained for
		// provenance (EVENTS.md §3.2).
		key := tripleKey(kind, from, to)
		if _, dup := g.triples[key]; dup {
			return nil
		}
		// The retained triple is the earliest by file order, so the provenance
		// kept is the first assertion's — the one the tie-break says wins.
		edge := &Edge{
			ID: e.ID(), Kind: kind, From: from, To: to, Seq: g.seq,
			By: e.Str("by"), Source: e.Str("source"), At: at,
		}
		g.Edges[e.ID()] = edge
		g.triples[key] = e.ID()
		g.out[from] = append(g.out[from], edge)
		g.in[to] = append(g.in[to], edge)
		// Both ends: an edge is an assertion about the pair, and "what is
		// blocking me now" is a change to the blocked node as much as the
		// blocker. Touching happens only past the dedupe checks above, so a
		// redundant re-emit of an existing triple leaves recency alone — the
		// same reason import must not rewrite an unchanged ref (EVENTS.md §3.9).
		g.touch(from, at)
		g.touch(to, at)

	case event.KindEdgeRetract:
		// Retracting an unknown or already-retracted target is a no-op, not an
		// error (EVENTS.md §3.3).
		if edge, ok := g.Edges[e.Str("target")]; ok {
			g.dropEdge(edge)
			g.touch(edge.From, at)
			g.touch(edge.To, at)
		}

	case event.KindRefAdd:
		node, ref := g.Canonical(e.Str("node")), e.Str("ref")
		g.refs[e.ID()] = refEntry{node: node, ref: ref}
		if n, ok := g.Nodes[node]; ok && !slices.Contains(n.Refs, ref) {
			n.Refs = append(n.Refs, ref)
			g.touch(node, at)
		}

	case event.KindRefRetract:
		if entry, ok := g.refs[e.Str("target")]; ok {
			delete(g.refs, e.Str("target"))
			// Both the asserting node and its survivor: a merge *copies* the
			// ref onto the survivor (merge.go), so retracting it from only the
			// node that asserted it leaves the survivor still carrying a ref
			// nobody asserts, still resolvable and still handed to import as an
			// existing binding.
			dropRef(g.Nodes[entry.node], entry.ref)
			if into := g.Canonical(entry.node); into != entry.node {
				dropRef(g.Nodes[into], entry.ref)
			}
			g.touch(entry.node, at)
		}

	// No touch: an observation says upstream was looked at, not that this node
	// moved. See the Node doc comment.
	case event.KindRefObserve:
		g.applyRefObserve(e)

	case event.KindClaimAdd:
		g.applyClaimAdd(e)

	case event.KindClaimRelease:
		g.applyClaimRelease(e)

	case event.KindNodeState:
		if n, ok := g.Nodes[g.Canonical(e.Str("node"))]; ok {
			n.State = e.Str("state")
			g.touch(n.ID, at)
		}

	case event.KindNodeMerge:
		g.applyMerge(e)
	}
	return nil
}

// eventAt reads the event's wall-clock timestamp. A malformed one yields the
// zero time rather than an error: event.Parse already rejects an unparseable
// `at`, so this only fires on a hand-built event, and a missing timestamp is
// absence of information, not corruption (SPEC.md §9.3).
func eventAt(e event.Event) time.Time {
	at, _ := time.Parse(time.RFC3339, e.Str("at"))
	return at
}

// touch advances a node's Touched time, canonicalizing first so an assertion
// naming a merged node lands on its survivor.
//
// Max rather than last-write, and that is the whole point. `at` is a wall clock:
// it moves backwards across machines, and a concatenated or third-party log is
// under no obligation to present a node's events in order (EVENTS.md §5.2).
// Last-write would let one skewed or back-dated event make a node that just
// moved report as untouched for as long as the skew lasts.
//
// An unknown node is a no-op — a dangling reference is a normal state.
func (g *Graph) touch(id string, at time.Time) {
	if at.IsZero() {
		return
	}
	if n, ok := g.Nodes[g.Canonical(id)]; ok && at.After(n.Touched) {
		n.Touched = at
	}
}

// dropEdge removes an edge from every index it sits in. Shared by edge.retract
// and by merge, which has to unhook an edge before re-pointing it.
func (g *Graph) dropEdge(e *Edge) {
	delete(g.Edges, e.ID)
	delete(g.triples, tripleKey(e.Kind, e.From, e.To))
	g.out[e.From] = removeEdge(g.out[e.From], e.ID)
	g.in[e.To] = removeEdge(g.in[e.To], e.ID)
}

// Fold builds a graph from a fold function, as returned by store.Mutate or
// wrapping store.Fold.
func Fold(fold func(func(event.Event) error) error) (*Graph, error) {
	g := New()
	if err := fold(g.Apply); err != nil {
		return nil, err
	}
	return g, nil
}

// CheckEdge reports whether adding this edge is permitted.
//
// This is the validation that must run inside the write transaction. Run
// outside it, two processes can each independently approve opposite edges and
// then both commit, producing the cycle both had just rejected (EVENTS.md §6).
func (g *Graph) CheckEdge(kind, from, to string) error {
	if !acyclic[kind] {
		return nil
	}
	if from == to {
		return &ErrCycle{Kind: kind, Path: []string{from, from}}
	}
	if path := g.findPath(kind, to, from, pendingMerge{}); path != nil {
		return &ErrCycle{Kind: kind, Path: append(path, to)}
	}
	return nil
}

// ErrCycle reports an edge that would close a cycle in an acyclic kind. Path
// holds full node ids; rendering them with titles is the caller's job, because
// "blocked" without the path is not actionable and only the CLI knows the
// display width (SPEC.md §6.1).
type ErrCycle struct {
	Kind string
	Path []string
}

func (e *ErrCycle) Error() string {
	return fmt.Sprintf("would create a %s cycle: %s", e.Kind, formatPath(e.Path))
}

// findPath returns a path from src to dst following edges of the given kind, or
// nil if none exists.
//
// pending, when set, makes the walk see the graph a merge *would* produce. That
// is what lets CheckMerge answer the question without cloning the graph first —
// and a clone is the version that rots, because every field added later has to
// be remembered in it.
func (g *Graph) findPath(kind, src, dst string, pending pendingMerge) []string {
	type step struct {
		node string
		path []string
	}
	src, dst = pending.view(src), pending.view(dst)
	seen := map[string]bool{src: true}
	queue := []step{{node: src, path: []string{src}}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.node == dst {
			return cur.path
		}
		for _, e := range g.outMerged(cur.node, pending) {
			to := pending.view(e.To)
			if e.Kind != kind || seen[to] {
				continue
			}
			seen[to] = true
			next := make([]string, len(cur.path), len(cur.path)+1)
			copy(next, cur.path)
			queue = append(queue, step{node: to, path: append(next, to)})
		}
	}
	return nil
}

func formatPath(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += " -> "
		}
		if len(id) > 7 {
			id = id[:7]
		}
		out += id
	}
	return out
}

// dropRef removes a ref from a node, tolerating a nil node or an absent ref.
func dropRef(n *Node, ref string) {
	if n == nil {
		return
	}
	if i := slices.Index(n.Refs, ref); i >= 0 {
		n.Refs = append(n.Refs[:i:i], n.Refs[i+1:]...)
	}
}

// removeEdge drops an edge from an adjacency slice, preserving order.
func removeEdge(edges []*Edge, id string) []*Edge {
	for i, e := range edges {
		if e.ID == id {
			return append(edges[:i:i], edges[i+1:]...)
		}
	}
	return edges
}

// Ordered returns the live nodes in file order — deterministic, and meaningful
// because it is the order they were asserted in (SPEC.md §9.5).
//
// Merged nodes are omitted: after a merge they name the same work as their
// survivor, and listing both would double-count it.
func (g *Graph) Ordered() []*Node {
	out := make([]*Node, 0, len(g.order))
	for _, id := range g.order {
		if n, ok := g.Nodes[id]; ok && n.MergedInto == "" {
			out = append(out, n)
		}
	}
	return out
}

// IDs returns every node id in file order, merged nodes included.
//
// Unlike Ordered, this must stay complete: it feeds display width, and a merged
// id is still something a user can paste. Narrowing it would let a prefix render
// unambiguously and then fail to resolve.
func (g *Graph) IDs() []string {
	return append([]string(nil), g.order...)
}

// Neighbor is one edge as seen from a particular node, with direction resolved.
type Neighbor struct {
	Edge     *Edge
	Node     *Node  // nil when the edge dangles (SPEC.md §9.3)
	ID       string // the other endpoint, even when Node is nil
	Outbound bool
}

// Neighbors returns every edge touching a node, in file order.
//
// Both directions in one pass: orienting on a node means knowing what it needs
// and what needs it, and making that two calls would break the one-call-to-
// orient constraint (SPEC.md §9.4).
func (g *Graph) Neighbors(id string) []Neighbor {
	var out []Neighbor
	seen := make(map[string]bool)

	add := func(e *Edge) {
		if seen[e.ID] {
			return
		}
		seen[e.ID] = true

		// The other endpoint is whichever one is not the node we centred on.
		// Deriving it from the edge's direction instead would report a
		// symmetric edge's own node as its neighbour.
		other := e.To
		if e.To == id {
			other = e.From
		}
		out = append(out, Neighbor{
			Edge:     e,
			Node:     g.Nodes[other],
			ID:       other,
			Outbound: e.From == id,
		})
	}

	for _, e := range g.out[id] {
		add(e)
	}
	for _, e := range g.in[id] {
		add(e)
	}
	return out
}

// OutEdges returns edges leaving a node, optionally filtered to one kind.
// Symmetric edges appear from either endpoint, since direction is not
// information for those kinds.
func (g *Graph) OutEdges(id, kind string) []*Edge {
	var out []*Edge
	for _, e := range g.out[id] {
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	if symmetric[kind] || kind == "" {
		for _, e := range g.in[id] {
			if symmetric[e.Kind] && (kind == "" || e.Kind == kind) {
				out = append(out, e)
			}
		}
	}
	return out
}

// FindEdge returns the edge asserting this triple, or nil if none does.
//
// The lookup goes through the same key the fold dedupes on rather than scanning
// the adjacency lists, because that key is where symmetric kinds normalize
// endpoint order. A second rule here could disagree with the collapse rule and
// report no edge between B and A while holding the one asserted from A to B —
// which, for a caller about to retract it, is the answer that leaves a wrong
// edge in the graph and tells them it is gone.
func (g *Graph) FindEdge(kind, from, to string) *Edge {
	id, ok := g.triples[tripleKey(kind, g.Canonical(from), g.Canonical(to))]
	if !ok {
		return nil
	}
	return g.Edges[id]
}

// FindRefs returns the ids of every ref.add asserting this binding, sorted.
//
// Every one of them, not the first found, because the fold records an entry per
// ref.add even when the ref is already on the node — unlike edges, which
// collapse on the triple — so two writers binding the same ref leave two live
// assertions. Retracting one would drop the ref from the node while another
// still asserts it, and a later re-add would then behave differently depending
// on which was retracted. Sorted because map iteration is randomized, and a
// command that emits events in a random order is not canonical (SPEC.md §9.5).
//
// Entries hold the node id as it was canonical when asserted, so a merge that
// happened afterwards has to be resolved on both sides of the comparison.
func (g *Graph) FindRefs(node, ref string) []string {
	want := g.Canonical(node)
	var ids []string
	for id, entry := range g.refs {
		if entry.ref == ref && g.Canonical(entry.node) == want {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// InEdges returns edges arriving at a node, optionally filtered to one kind.
// Symmetric edges appear from either endpoint, matching OutEdges: direction is
// not information for those kinds.
//
// The symmetric clause is not decoration. An edge is indexed once, under
// g.out[from] and g.in[to], and the fold normalizes endpoint order for symmetric
// kinds so that asserting A-B and B-A collapse (see addEdge). Scanning g.in
// alone would therefore find a conflicts-with edge from whichever endpoint
// normalization happened to make `to` and not from the other — resurrecting, on
// the read side, exactly the asymmetry the normalization exists to remove.
func (g *Graph) InEdges(id, kind string) []*Edge {
	var out []*Edge
	for _, e := range g.in[id] {
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	if symmetric[kind] || kind == "" {
		for _, e := range g.out[id] {
			if symmetric[e.Kind] && (kind == "" || e.Kind == kind) {
				out = append(out, e)
			}
		}
	}
	return out
}
