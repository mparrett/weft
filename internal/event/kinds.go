package event

import "time"

// Event kinds. Milestone 1 emits NodeAdd and EdgeAdd; the correction kinds are
// defined because a log with no correction path is one that eventually gets
// deleted (EVENTS.md §3).
const (
	KindNodeAdd     = "node.add"
	KindEdgeAdd     = "edge.add"
	KindEdgeRetract = "edge.retract"
	KindRefAdd      = "ref.add"
	KindRefRetract  = "ref.retract"
	KindNodeState   = "node.state"
)

// KindNodeMerge folds one node into another (EVENTS.md §3.7).
const KindNodeMerge = "node.merge"

// Node states (EVENTS.md §3.5).
const (
	StateOpen    = "open"
	StateDone    = "done"
	StateDropped = "dropped"
)

// NodeAdd builds a sealed node.add event. The returned id is the node's id
// (EVENTS.md §2.3).
func NodeAdd(ns, kind, title, actor, source string, at time.Time) (Event, error) {
	e := New(KindNodeAdd, actor, source, at)
	e["ns"] = ns
	e["kind"] = kind
	e["title"] = title
	e["body"] = nil
	return e, e.Seal()
}

// EdgeAdd builds a sealed edge.add event. from and to are node ids.
func EdgeAdd(kind, from, to, actor, source string, at time.Time) (Event, error) {
	e := New(KindEdgeAdd, actor, source, at)
	e["kind"] = kind
	e["from"] = from
	e["to"] = to
	return e, e.Seal()
}

// EdgeRetract builds a sealed edge.retract event. target is the id of the
// edge.add being retracted (EVENTS.md §3.3).
func EdgeRetract(target, actor, source string, at time.Time) (Event, error) {
	e := New(KindEdgeRetract, actor, source, at)
	e["target"] = target
	return e, e.Seal()
}

// KindRefObserve records what an external object said at a moment (EVENTS.md §3.8).
const KindRefObserve = "ref.observe"

// RefAdd builds a sealed ref.add event, binding a node to external reality.
func RefAdd(node, ref, actor, source string, at time.Time) (Event, error) {
	e := New(KindRefAdd, actor, source, at)
	e["node"] = node
	e["ref"] = ref
	return e, e.Seal()
}

// ObservePayload is what the artifact said about itself (EVENTS.md §3.8).
//
// Each scheme carries the subset it can honestly report — gh:/pr: have state
// and title and their own stamps, file: has stamps and a content hash. An
// empty field is omitted from the event entirely: absence means "this scheme
// does not report it", never "reported as blank".
//
// Created and Modified are the artifact's own clock, verbatim from upstream
// (normalized to RFC3339); the event's "at" is ours. Only "at" participates in
// identity, so only "at" carries §2.5's precision requirement.
type ObservePayload struct {
	State    string
	Title    string
	Created  string
	Modified string
	Hash     string // self-describing, e.g. "sha256:<64 lowercase hex>"
}

// RefObserve builds a sealed ref.observe event: what the external object said,
// and when we looked.
//
// This is a *cache with provenance*, never authority. The node's own title and
// state stay weft's (SPEC.md §10); this records what upstream said so the
// divergence is visible and datable. "at" is what makes an observation
// judgeable — stale data presented as current is worse than no data.
func RefObserve(node, ref string, p ObservePayload, actor, source string, at time.Time) (Event, error) {
	e := New(KindRefObserve, actor, source, at)
	e["node"] = node
	e["ref"] = ref
	for k, v := range map[string]string{
		"state": p.State, "title": p.Title,
		"created": p.Created, "modified": p.Modified, "hash": p.Hash,
	} {
		if v != "" {
			e[k] = v
		}
	}
	return e, e.Seal()
}

// NodeState builds a sealed node.state event.
func NodeState(node, state, actor, source string, at time.Time) (Event, error) {
	e := New(KindNodeState, actor, source, at)
	e["node"] = node
	e["state"] = state
	return e, e.Seal()
}

// NodeMerge builds a sealed node.merge event: into survives, from becomes an
// alias of it (EVENTS.md §3.7).
//
// The direction lives on this event rather than on a same-as edge because
// same-as is symmetric in the fold and therefore cannot say which node
// survived — see SPEC.md §6.2.
func NodeMerge(into, from, actor, source string, at time.Time) (Event, error) {
	e := New(KindNodeMerge, actor, source, at)
	e["into"] = into
	e["from"] = from
	return e, e.Seal()
}

// Claim event kinds (EVENTS.md §8.1).
const (
	KindClaimAdd     = "claim.add"
	KindClaimRelease = "claim.release"
)

// DefaultTTL is how long a claim stays live when the caller does not say.
//
// An unbounded default is the failure that matters here: an agent that dies
// mid-task leaves a claim nobody will ever release, and every later reader
// treats abandoned work as taken. A week is long enough not to interrupt real
// work and short enough that abandonment surfaces.
const DefaultTTL = 7 * 24 * time.Hour

// ClaimAdd builds a sealed claim.add event.
//
// holder is the *subject* — who is said to be doing the work — and is distinct
// from the actor in "by", which is whoever asserted it. An agent recording that
// a colleague appears to be working on something sets holder to the colleague,
// by to itself, and source to inferred (EVENTS.md §4).
//
// expires is written as an absolute timestamp rather than a duration so that
// staleness is a property of the record, not of when someone happens to read
// it. A zero expires means the claim never ages.
func ClaimAdd(node, holder, note, actor, source string, at, expires time.Time) (Event, error) {
	e := New(KindClaimAdd, actor, source, at)
	e["node"] = node
	e["holder"] = holder
	if note == "" {
		e["note"] = nil
	} else {
		e["note"] = note
	}
	if expires.IsZero() {
		e["expires"] = nil
	} else {
		e["expires"] = expires.Format(time.RFC3339Nano)
	}
	return e, e.Seal()
}

// ClaimRelease builds a sealed claim.release event. target is the id of the
// claim.add being released, so releasing is a compensating event rather than a
// deletion — who claimed what, and when they let it go, both stay in the log.
func ClaimRelease(target, actor, source string, at time.Time) (Event, error) {
	e := New(KindClaimRelease, actor, source, at)
	e["target"] = target
	return e, e.Seal()
}
