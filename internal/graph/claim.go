package graph

import (
	"sort"
	"time"

	"github.com/mparrett/weft/internal/event"
)

// Claim is an ownership assertion on a node.
//
// Claims do not lock. They are advisory, and two live claims on one node is a
// reportable condition rather than an error (SPEC.md §4.3): the graph records
// what people say about who is working on what, and arbitrating that is not its
// job.
type Claim struct {
	ID      string // the claim.add event id, which claim.release targets
	Node    string
	Holder  string // the subject: who is said to be doing the work
	Note    string
	By      string // the actor who asserted it, often not the holder
	Source  string
	At      time.Time
	Expires time.Time // zero means the claim never ages
	Seq     int       // position in file order, which merge has to preserve

	// The claim.release that let it go, zero until then. A released claim
	// leaves ClaimsFor but stays readable through ReleasedClaimsFor, because
	// "who held this, from when to when" is history a reader can rebuild only
	// if the fold keeps both ends (EVENTS.md §3.6).
	ReleasedAt     time.Time
	ReleasedBy     string
	ReleasedSource string
}

// Live reports whether the claim still stands as of now.
//
// A released claim is out of ClaimsFor and LiveClaims, so liveness here is only
// about expiry.
func (c *Claim) Live(now time.Time) bool {
	return c.Expires.IsZero() || c.Expires.After(now)
}

// Expired reports a claim that was never released and has aged out. This is
// what abandonment looks like: an agent took the work and never came back.
func (c *Claim) Expired(now time.Time) bool {
	return !c.Live(now)
}

// ClaimsFor returns every unreleased claim on a node, in file order, expired
// ones included. Callers decide what to do with expiry; hiding it here would
// make abandoned work indistinguishable from unclaimed work.
func (g *Graph) ClaimsFor(node string) []*Claim {
	return g.claimsByNode[node]
}

// ReleasedClaimsFor returns every released claim on a node, in the file order
// of the claims themselves, so one holder who claimed, released and claimed
// again appears as two entries in the order they took the work.
func (g *Graph) ReleasedClaimsFor(node string) []*Claim {
	out := g.releasedByNode[node]
	if g.releasedUnsorted[node] {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
		delete(g.releasedUnsorted, node)
	}
	return out
}

// LiveClaims returns only the claims still standing as of now.
func (g *Graph) LiveClaims(node string, now time.Time) []*Claim {
	var out []*Claim
	for _, c := range g.claimsByNode[node] {
		if c.Live(now) {
			out = append(out, c)
		}
	}
	return out
}

// ClaimBy returns the live claim held by a given holder, or nil.
func (g *Graph) ClaimBy(node, holder string, now time.Time) *Claim {
	for _, c := range g.LiveClaims(node, now) {
		if c.Holder == holder {
			return c
		}
	}
	return nil
}

// applyClaimAdd folds a claim.add. A claim on an unknown node is retained with
// the reference dangling, like any other assertion about work we have not seen
// asserted yet (SPEC.md §9.3).
func (g *Graph) applyClaimAdd(e event.Event) {
	c := &Claim{
		ID:     e.ID(),
		Node:   g.Canonical(e.Str("node")),
		Holder: e.Str("holder"),
		Note:   e.Str("note"),
		By:     e.Str("by"),
		Source: e.Str("source"),
		Seq:    g.seq,
	}
	c.At = eventAt(e)
	if s := e.Str("expires"); s != "" {
		c.Expires, _ = time.Parse(time.RFC3339, s)
	}
	g.Claims[c.ID] = c
	g.claimsByNode[c.Node] = append(g.claimsByNode[c.Node], c)
	// Somebody taking the work is movement on the node, so it counts toward
	// recency even though nothing about the work itself changed.
	g.touch(c.Node, c.At)
}

// applyClaimRelease folds a claim.release. Releasing an unknown or
// already-released claim is a no-op, not an error (EVENTS.md §3.3).
func (g *Graph) applyClaimRelease(e event.Event) {
	c, ok := g.Claims[e.Str("target")]
	if !ok {
		return
	}
	delete(g.Claims, c.ID)
	g.claimsByNode[c.Node] = removeClaim(g.claimsByNode[c.Node], c.ID)
	c.ReleasedAt = eventAt(e)
	c.ReleasedBy = e.Str("by")
	c.ReleasedSource = e.Str("source")
	// Appended in release order and sorted on read. Keeping the bucket sorted
	// here would cost a copy per release, and a node released once per session
	// would make the fold quadratic, which is a defect at any constant.
	g.releasedByNode[c.Node] = append(g.releasedByNode[c.Node], c)
	g.releasedUnsorted[c.Node] = true
	g.touch(c.Node, eventAt(e))
}

func removeClaim(claims []*Claim, id string) []*Claim {
	for i, c := range claims {
		if c.ID == id {
			return append(claims[:i:i], claims[i+1:]...)
		}
	}
	return claims
}
