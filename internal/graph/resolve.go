package graph

import (
	"fmt"
	"strings"

	"github.com/mparrett/weft/internal/event"
)

// ErrNotFound reports a query that matched no node. Distinct from ErrAmbiguous
// because the two call for different responses: this one means the thing does
// not exist, the other means say more (BUILD.md §5).
type ErrNotFound struct {
	Query string
}

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("no node matches %q", e.Query)
}

// ErrAmbiguous reports a query that matched more than one node.
type ErrAmbiguous struct {
	Query      string
	Candidates []*Node
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("%q is ambiguous: %d nodes match", e.Query, len(e.Candidates))
}

// Resolve maps a user-supplied string to exactly one node.
//
// Tried in order, most specific first:
//
//  1. a full 64-character id
//  2. an exact ref, so `weft show gh:acme/api#412` works without a lookup step;
//     when live and dropped nodes both carry it, the live ones win (PreferLive)
//  3. an id prefix of any length
//
// Resolution is deliberately **global, never namespace-scoped**. An id names one
// node in the whole store, and scoping resolution would make cross-namespace
// references unaddressable — which is the point of namespaces being a field
// rather than part of identity (SPEC.md §5).
// A merged node still resolves, and resolves to its survivor. The id someone
// pasted from a commit message or a chat log a month ago has to keep working;
// silently reporting "no such node" after a merge would make the tool feel
// lossy when it is not (SPEC.md §6.2).
func (g *Graph) Resolve(query string) (*Node, error) {
	if query == "" {
		return nil, &ErrNotFound{Query: query}
	}

	if n, ok := g.Nodes[query]; ok {
		return g.survivor(n), nil
	}

	if matches := g.byRef(query); len(matches) > 0 {
		return g.one(query, PreferLive(g.distinct(matches)))
	}

	return g.one(query, g.distinct(g.byPrefix(query)))
}

// survivor maps a node to whatever it was merged into, or itself.
func (g *Graph) survivor(n *Node) *Node {
	if n.MergedInto == "" {
		return n
	}
	if into, ok := g.Nodes[g.Canonical(n.ID)]; ok {
		return into
	}
	return n
}

// distinct collapses candidates that merged into each other.
//
// The collapse matters because both a merged node and its survivor stay
// addressable: a prefix matching both names one piece of work, and reporting
// that as ambiguous would ask the user to choose between a node and itself.
func (g *Graph) distinct(matches []*Node) []*Node {
	var out []*Node
	seen := make(map[string]bool, len(matches))
	for _, n := range matches {
		s := g.survivor(n)
		if !seen[s.ID] {
			seen[s.ID] = true
			out = append(out, s)
		}
	}
	return out
}

// PreferLive narrows a candidate set to its live members, when it has any.
// Resolve applies it to ref matches only, never to prefixes: a ref is a
// *name*, and ambiguity in a name counts live nodes only, so that dropping a
// claim and re-asserting it — the documented retry pattern, since `add
// --if-absent` treats dropped as absent — does not poison the ref forever.
// A prefix is an *id*, and a dropped node must stay addressable
// by it, so a prefix collision keeps reporting every node it matches.
// Import's ref-collision check applies it for the same reason: a scheduled
// import must not skip an item forever because a dropped predecessor still
// carries the ref.
//
// A ref carried by a *single* dropped node still resolves — retraction is a
// state, not an erasure. Two or more dropped holders with no live one stay
// ambiguous: there is no live node to prefer, and choosing among retracted
// claims is `weft merge`'s judgment, not resolution's.
//
// The state check runs after the survivor collapse in Resolve, so a node
// merged into a live survivor counts as live — it is the survivor that would
// be returned.
func PreferLive(candidates []*Node) []*Node {
	if len(candidates) < 2 {
		return candidates
	}
	var live []*Node
	for _, n := range candidates {
		if n.State != event.StateDropped {
			live = append(live, n)
		}
	}
	if len(live) == 0 {
		return candidates
	}
	return live
}

// one picks the single node a set of distinct candidates names.
func (g *Graph) one(query string, distinct []*Node) (*Node, error) {
	switch len(distinct) {
	case 0:
		return nil, &ErrNotFound{Query: query}
	case 1:
		return distinct[0], nil
	default:
		return nil, &ErrAmbiguous{Query: query, Candidates: distinct}
	}
}

// byRef returns every node carrying the given ref, dropped or not. More than
// one is possible and is not corruption: two agents can bind the same issue to
// different nodes before anyone notices they are the same work (that is what
// `weft merge` is for). Between *live* holders this reports the collision
// rather than picking a winner; PreferLive is what discounts dropped ones.
func (g *Graph) byRef(ref string) []*Node {
	var out []*Node
	for _, id := range g.order {
		n, ok := g.Nodes[id]
		if !ok {
			continue
		}
		for _, r := range n.Refs {
			if r == ref {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// byPrefix returns every node whose id starts with the query, in file order so
// candidate lists are deterministic.
func (g *Graph) byPrefix(prefix string) []*Node {
	if !isHexPrefix(prefix) {
		return nil
	}
	var out []*Node
	for _, id := range g.order {
		if n, ok := g.Nodes[id]; ok && strings.HasPrefix(id, prefix) {
			out = append(out, n)
		}
	}
	return out
}

// isHexPrefix reports whether s could be the leading part of an id. Anything
// else is a ref that did not match, not a prefix, and saying so keeps the
// diagnostic honest.
func isHexPrefix(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
