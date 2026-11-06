package graph

import (
	"time"

	"github.com/mparrett/weft/internal/event"
)

// Observation is what an external object said, and when we looked.
//
// It is never authoritative. The node's own State and Title are weft's
// assertions; this is a dated cache of somebody else's (SPEC.md §10). The two
// diverging is *information* — "upstream closed this, we have not" is the thing
// you want surfaced, not an inconsistency to reconcile away.
type Observation struct {
	Ref   string
	State string
	Title string
	// The artifact's own clock and content hash (EVENTS.md §3.8), kept as the
	// event's exact strings rather than parsed: the write-side diff below
	// compares them byte-for-byte, and a parse/format round trip that
	// normalized precision would report a change upstream never made. Parsing
	// is the renderer's job, at the moment an age is displayed.
	Created  string
	Modified string
	Hash     string
	At       time.Time
	By       string
}

// Changed reports whether an observation says something new.
//
// Every payload field is compared; At is deliberately excluded: it differs on
// every look, and comparing it would make every import write an event and grow
// the log for nothing. Import stays idempotent precisely because "we checked
// again and nothing had changed" is not a fact worth a record.
func (o *Observation) Changed(p event.ObservePayload) bool {
	return o == nil || o.State != p.State || o.Title != p.Title ||
		o.Created != p.Created || o.Modified != p.Modified || o.Hash != p.Hash
}

// Observed returns the latest observation of a ref on a node, or nil.
//
// Canonicalized on the way in as well as at fold time: a merge moves the whole
// set onto the survivor, and this makes a lookup by the merged id land there
// too rather than silently reporting "never observed".
func (g *Graph) Observed(node, ref string) *Observation {
	return g.observations[g.Canonical(node)][ref]
}

// ObservationsFor returns every observation on a node, keyed by ref.
func (g *Graph) ObservationsFor(node string) map[string]*Observation {
	out := make(map[string]*Observation)
	if n, ok := g.Nodes[node]; ok {
		for _, r := range n.Refs {
			if o := g.Observed(node, r); o != nil {
				out[r] = o
			}
		}
	}
	return out
}

// ByRef indexes every ref to the nodes carrying it, in file order.
//
// Built in one pass so a bulk import can resolve N refs with N map lookups
// instead of N scans. Calling Resolve per item would be O(N*V) and would put
// the quadratic back that BUILD.md §1.4 makes the real constraint.
//
// The value is a slice because one ref on two nodes is a normal state, not
// corruption: two agents can bind the same issue before anyone notices they
// mean the same work. Deciding that is `weft merge`.
func (g *Graph) ByRef() map[string][]*Node {
	out := make(map[string][]*Node, len(g.Nodes))
	for _, id := range g.order {
		n, ok := g.Nodes[id]
		if !ok || n.MergedInto != "" {
			continue
		}
		for _, r := range n.Refs {
			out[r] = append(out[r], n)
		}
	}
	return out
}

// applyRefObserve folds a ref.observe. Last by file order wins: an observation
// supersedes the previous one rather than accumulating, because what is wanted
// is "what does upstream say now, and how old is that".
//
// The history is not lost — every observation stays in the log, which is the
// artifact other tools read (SPEC.md §2.1).
func (g *Graph) applyRefObserve(e event.Event) {
	node := g.Canonical(e.Str("node"))
	o := &Observation{
		Ref:      e.Str("ref"),
		State:    e.Str("state"),
		Title:    e.Str("title"),
		Created:  e.Str("created"),
		Modified: e.Str("modified"),
		Hash:     e.Str("hash"),
		By:       e.Str("by"),
	}
	o.At, _ = time.Parse(time.RFC3339, e.Str("at"))
	if g.observations[node] == nil {
		g.observations[node] = make(map[string]*Observation)
	}
	g.observations[node][o.Ref] = o
}

// moveObservations folds one node's observations onto another, called by
// applyMerge alongside the refs, edges, and claims.
//
// Missing this is not a cosmetic loss. The survivor takes the ref but not what
// upstream said about it, so `show` drops the age annotation SPEC.md §6.3 makes
// load-bearing, and the next import sees no prior observation and writes a
// spurious one — reporting a change against upstream that did not change, and
// breaking the byte-idempotence of EVENTS.md §3.9.
func (g *Graph) moveObservations(from, into string) {
	src := g.observations[from]
	if len(src) == 0 {
		return
	}
	if g.observations[into] == nil {
		g.observations[into] = make(map[string]*Observation)
	}
	for ref, o := range src {
		// The survivor's own observation of a ref is the more recent statement
		// about it, so it stands.
		if _, held := g.observations[into][ref]; !held {
			g.observations[into][ref] = o
		}
	}
	delete(g.observations, from)
}
