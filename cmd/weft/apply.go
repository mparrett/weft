package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// cmdApply commits a batch of existing mutations in one transaction, guarded by
// preconditions on the state the caller read (SPEC.md §6, EVENTS.md §5.5).
//
// It exists because a caller that sequences single commands has a transaction
// per command and none across them: two writers each reading "nobody holds this"
// before either writes both act on it. Here the expectations are checked against
// the same in-lock fold the batch lands on, so a stale read fails instead of
// committing.
//
// The mechanism is generic on purpose. weft checks "this holder's live claims
// are exactly these nodes"; what a claim *means* — one canonical attachment, a
// task assignee — belongs to the caller.
func cmdApply(e *env, args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dryRun := fs.Bool("dry-run", false, "check and plan everything, append nothing; same reply and exit code as a real apply")
	// Accepted for uniformity with every other command (SPEC.md §6); apply
	// speaks only JSON, so it changes nothing.
	fs.Bool("json", true, "always on: the request and the reply are JSON")
	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		return applyFail(e, &applyErr{code: exitUsage, msg: "weft apply: reads one JSON request on stdin and takes no arguments"})
	}

	raw, err := io.ReadAll(e.stdin)
	if err != nil {
		return applyFail(e, &applyErr{code: exitUsage, msg: "reading stdin: " + err.Error()})
	}
	req, aerr := parseApplyRequest(raw)
	if aerr != nil {
		return applyFail(e, aerr)
	}
	if req.Key == "" {
		req.Key = newTxKey()
	}

	s, err := store.Open()
	if err != nil {
		return applyFail(e, &applyErr{code: exitStore, msg: err.Error()})
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)
	defaultNS := ""
	if req.needsNS {
		defaultNS = currentNS(dir)
	}

	var reply *applyReply
	var failure error
	after, err := s.MutateRev(func(fold func(func(event.Event) error) error, rev string) ([]event.Event, error) {
		events, r, err := planApply(fold, rev, req, by, defaultNS, *dryRun)
		reply = r
		if err != nil {
			failure = err
			return nil, err
		}
		if *dryRun {
			return nil, nil
		}
		return events, nil
	})
	if err != nil {
		var conflict *applyConflict
		var ae *applyErr
		switch {
		case errors.As(failure, &conflict):
			return writeApplyJSON(e, exitConflict, conflict)
		case errors.As(failure, &ae):
			return applyFail(e, ae)
		default:
			return applyFail(e, &applyErr{code: storeExitCode(err), msg: err.Error()})
		}
	}
	// Past the commit, nothing may change the exit code (BUILD.md §5): the reply
	// was built inside the transaction and the rev from the bytes written, so
	// the only thing left to fail is the write to stdout, which degrades.
	reply.Rev = after
	if code := writeApplyJSON(e, exitOK, reply); code != exitOK {
		return degraded(e, "the batch was committed", "printing the reply failed", errors.New("stdout"))
	}
	return exitOK
}

// --- the request ------------------------------------------------------------

type applyRequest struct {
	Key    string            `json:"key"`
	Rev    string            `json:"rev"`
	Expect []json.RawMessage `json:"expect"`
	Ops    []json.RawMessage `json:"ops"`

	ops     []applyOp
	expects []applyExpect
	needsNS bool
}

type applyOp struct {
	Op string `json:"op"`

	Title string   `json:"title"`
	Kind  string   `json:"kind"`
	NS    string   `json:"ns"`
	Refs  []string `json:"refs"`
	As    string   `json:"as"`

	Node string `json:"node"`
	From string `json:"from"`
	To   string `json:"to"`
	Ref  string `json:"ref"`

	Source string `json:"source"`

	Holder     string `json:"holder"`
	Note       string `json:"note"`
	TTL        string `json:"ttl"`
	NoExpiry   bool   `json:"no_expiry"`
	UnlessHeld bool   `json:"unless_held"`

	Claim string `json:"claim"`
	All   bool   `json:"all"`

	ttl time.Duration
}

// opFields is every field each op accepts. A field another op uses is still an
// error here: `{"op": "done", "holder": …}` is a request that means something
// other than what weft would do with it, and doing the rest silently is the
// failure mode a strict parse exists to prevent.
var opFields = map[string][]string{
	"add":     {"title", "kind", "ns", "refs", "source", "as"},
	"ref.add": {"node", "ref", "source"},
	"ref.rm":  {"node", "ref", "source"},
	"claim":   {"node", "holder", "note", "ttl", "no_expiry", "source", "unless_held"},
	"release": {"node", "holder", "claim", "all"},
	"link":    {"from", "kind", "to", "source"},
	"done":    {"node"},
}

// opEvents is the event kinds each op can write, which is how a replay checks
// that the logged batch came from this request.
var opEvents = map[string][]string{
	"add":     {event.KindNodeAdd, event.KindRefAdd},
	"ref.add": {event.KindRefAdd},
	"ref.rm":  {event.KindRefRetract},
	"claim":   {event.KindClaimAdd},
	"release": {event.KindClaimRelease},
	"link":    {event.KindEdgeAdd},
	"done":    {event.KindNodeState},
}

type applyExpect struct {
	Node    *string   `json:"node"`
	State   *string   `json:"state"`
	Claims  *[]string `json:"claims"`
	Holder  *string   `json:"holder"`
	LiveOn  *[]string `json:"live_on"`
	NS      *string   `json:"ns"`
	Kind    *string   `json:"kind"`
	Ref     *string   `json:"ref"`
	BoundTo *[]string `json:"bound_to"`

	raw json.RawMessage
}

func usageAt(field string, i int, format string, a ...any) *applyErr {
	idx := i
	ae := &applyErr{code: exitUsage, msg: fmt.Sprintf(format, a...)}
	if field == "expect" {
		ae.expect = &idx
	} else {
		ae.i = &idx
	}
	return ae
}

func strictDecode(raw []byte, v any) (map[string]bool, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(keys))
	for k := range keys {
		present[k] = true
	}
	return present, nil
}

func parseApplyRequest(raw []byte) (*applyRequest, *applyErr) {
	var req applyRequest
	if _, err := strictDecode(raw, &req); err != nil {
		return nil, &applyErr{code: exitUsage, msg: "malformed request: " + err.Error()}
	}
	if len(req.Ops) == 0 {
		return nil, &applyErr{code: exitUsage, msg: "a request needs at least one op"}
	}
	if len(req.Key) > 128 {
		return nil, &applyErr{code: exitUsage, msg: "key is longer than 128 characters"}
	}

	for i, r := range req.Expect {
		var x applyExpect
		present, err := strictDecode(r, &x)
		if err != nil {
			return nil, usageAt("expect", i, "expectation %d: %v", i, err)
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, r)
		x.raw = compact.Bytes()
		shape := ""
		switch {
		case present["node"] && present["state"] && len(present) == 2:
			shape = "state"
		case present["node"] && present["claims"] && len(present) == 2:
			shape = "claims"
		case present["holder"] && present["live_on"]:
			shape = "live_on"
			for k := range present {
				if !slices.Contains([]string{"holder", "live_on", "ns", "kind"}, k) {
					shape = ""
				}
			}
		case present["ref"] && present["bound_to"] && len(present) == 2:
			shape = "bound_to"
		}
		if shape == "" {
			return nil, usageAt("expect", i, "expectation %d: want one of {node, state}, {node, claims}, {holder, live_on, ns?, kind?}, {ref, bound_to}", i)
		}
		for _, q := range append(append([]string{deref(x.Node)}, derefs(x.LiveOn)...), derefs(x.BoundTo)...) {
			if strings.HasPrefix(q, "$") {
				return nil, usageAt("expect", i, "expectation %d: %q names a node this batch creates; expectations are checked before any op runs", i, q)
			}
		}
		req.expects = append(req.expects, x)
	}

	defined := map[string]bool{}
	for i, r := range req.Ops {
		var op applyOp
		present, err := strictDecode(r, &op)
		if err != nil {
			return nil, usageAt("op", i, "op %d: %v", i, err)
		}
		allowed, ok := opFields[op.Op]
		if !ok {
			return nil, usageAt("op", i, "op %d: unknown op %q (want one of: add, ref.add, ref.rm, claim, release, link, done)", i, op.Op)
		}
		for k := range present {
			if k != "op" && !slices.Contains(allowed, k) {
				return nil, usageAt("op", i, "op %d: %s takes no %q", i, op.Op, k)
			}
		}
		need := func(field, value string) *applyErr {
			if value == "" {
				return usageAt("op", i, "op %d: %s needs %q", i, op.Op, field)
			}
			return nil
		}
		if op.Source == "" {
			op.Source = event.SourceAsserted
		} else if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, op.Source) {
			return nil, usageAt("op", i, "op %d: unknown source %q", i, op.Source)
		}
		var nodeFields []string
		switch op.Op {
		case "add":
			if ae := need("title", strings.TrimSpace(op.Title)); ae != nil {
				return nil, ae
			}
			op.Title = strings.TrimSpace(op.Title)
			if op.Kind == "" {
				op.Kind = "task"
			}
			if !contains(validKinds, op.Kind) {
				return nil, usageAt("op", i, "op %d: unknown kind %q (want one of: %s)", i, op.Kind, strings.Join(validKinds, ", "))
			}
			if op.NS == "" {
				req.needsNS = true
			}
			if present["as"] {
				if !strings.HasPrefix(op.As, "$") || len(op.As) < 2 {
					return nil, usageAt("op", i, "op %d: as must be a $name, got %q", i, op.As)
				}
				if defined[op.As] {
					return nil, usageAt("op", i, "op %d: %s is already defined in this batch", i, op.As)
				}
				defined[op.As] = true
			}
			for _, ref := range op.Refs {
				if ref == "" {
					return nil, usageAt("op", i, "op %d: a ref cannot be empty", i)
				}
			}
		case "ref.add", "ref.rm":
			nodeFields = []string{op.Node}
			if ae := firstErr(need("node", op.Node), need("ref", op.Ref)); ae != nil {
				return nil, ae
			}
		case "claim":
			nodeFields = []string{op.Node}
			if ae := need("node", op.Node); ae != nil {
				return nil, ae
			}
			op.ttl = event.DefaultTTL
			if op.TTL != "" {
				if op.NoExpiry {
					return nil, usageAt("op", i, "op %d: ttl and no_expiry are exclusive", i)
				}
				d, err := time.ParseDuration(op.TTL)
				if err != nil || d <= 0 {
					return nil, usageAt("op", i, "op %d: ttl must be a positive duration like 168h", i)
				}
				op.ttl = d
			}
		case "release":
			forms := 0
			if op.Claim != "" {
				forms++
				if present["node"] || present["holder"] {
					return nil, usageAt("op", i, "op %d: release by claim takes the claim id alone", i)
				}
			}
			if op.Holder != "" {
				forms++
			}
			if op.All {
				forms++
			}
			if forms != 1 {
				return nil, usageAt("op", i, "op %d: release wants exactly one of {node, holder}, {claim}, {node, all: true}", i)
			}
			if op.Claim == "" {
				nodeFields = []string{op.Node}
				if ae := need("node", op.Node); ae != nil {
					return nil, ae
				}
			}
		case "link":
			nodeFields = []string{op.From, op.To}
			if ae := firstErr(need("from", op.From), need("to", op.To), need("kind", op.Kind)); ae != nil {
				return nil, ae
			}
			if !contains(validEdgeKinds, op.Kind) {
				return nil, usageAt("op", i, "op %d: unknown edge kind %q (want one of: %s)", i, op.Kind, strings.Join(validEdgeKinds, ", "))
			}
		case "done":
			nodeFields = []string{op.Node}
			if ae := need("node", op.Node); ae != nil {
				return nil, ae
			}
		}
		for _, q := range nodeFields {
			if strings.HasPrefix(q, "$") && !defined[q] {
				return nil, usageAt("op", i, "op %d: %s is not defined by an earlier add in this batch", i, q)
			}
		}
		req.ops = append(req.ops, op)
	}
	return &req, nil
}

func firstErr(errs ...*applyErr) *applyErr {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefs(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// newTxKey names a batch whose caller sent no key. Random rather than derived:
// a derived key would make two identical requests one batch, and the second is
// a separate assertion unless the caller says otherwise by reusing a key.
func newTxKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- the reply --------------------------------------------------------------

type applyResult struct {
	I       int       `json:"i"`
	Op      string    `json:"op"`
	Changed bool      `json:"changed"`
	Node    string    `json:"node,omitempty"`
	Claims  *[]string `json:"claims,omitempty"` // release only, and always there for it
	Claim   string    `json:"claim,omitempty"`
	Edge    string    `json:"edge,omitempty"`
}

type applyReply struct {
	Committed bool              `json:"committed"`
	Replayed  bool              `json:"replayed,omitempty"`
	Key       string            `json:"key"`
	Rev       string            `json:"rev"`
	Tmp       map[string]string `json:"tmp,omitempty"`
	Results   []applyResult     `json:"results"`
}

type applyFailure struct {
	I      *int            `json:"i,omitempty"`
	Expect json.RawMessage `json:"expect"`
	Actual any             `json:"actual"`
}

// applyConflict is exit 8: the state the caller read no longer holds. Every
// failed expectation is reported, so one reread answers all of them.
type applyConflict struct {
	Error_    string         `json:"error"`
	Code      int            `json:"code"`
	Committed bool           `json:"committed"`
	Rev       string         `json:"rev"`
	Failed    []applyFailure `json:"failed"`
}

func (c *applyConflict) Error() string { return c.Error_ }

// applyErr is every other refusal. i names the op and expect names the
// expectation it came from, so a caller can point at the line it got wrong.
type applyErr struct {
	code       int
	msg        string
	i          *int
	expect     *int
	candidates []string
}

func (e *applyErr) Error() string { return e.msg }

func applyFail(e *env, ae *applyErr) int {
	return writeApplyJSON(e, ae.code, struct {
		Error      string   `json:"error"`
		Code       int      `json:"code"`
		I          *int     `json:"i,omitempty"`
		Expect     *int     `json:"expect,omitempty"`
		Candidates []string `json:"candidates,omitempty"`
	}{ae.msg, ae.code, ae.i, ae.expect, ae.candidates})
}

func writeApplyJSON(e *env, code int, v any) int {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		if code == exitOK {
			return exitStore
		}
	}
	return code
}

// --- the transaction --------------------------------------------------------

// resolveErr turns a resolution failure into the exit code the single-node
// commands give it (BUILD.md §5).
func resolveErr(err error, ae *applyErr) *applyErr {
	var nf *graph.ErrNotFound
	var amb *graph.ErrAmbiguous
	switch {
	case errors.As(err, &nf):
		ae.code = exitNotFound
	case errors.As(err, &amb):
		ae.code = exitAmbiguous
		for _, c := range amb.Candidates {
			ae.candidates = append(ae.candidates, c.ID)
		}
	default:
		ae.code = exitStore
	}
	ae.msg = err.Error()
	return ae
}

// planApply runs inside the transaction. It folds once, answers a replay from
// the log, checks expectations against the fold, then plays the ops onto it in
// order, so each op resolves against the effects of the ones before it.
func planApply(fold func(func(event.Event) error) error, rev string, req *applyRequest, by, defaultNS string, dryRun bool) ([]event.Event, *applyReply, error) {
	var mine []event.Event
	committed := false
	g, err := graph.Fold(func(apply func(event.Event) error) error {
		return fold(func(ev event.Event) error {
			if ev.Str(event.FieldTx) == req.Key {
				if ev.Kind() == event.KindTxCommit {
					committed = true
				} else {
					mine = append(mine, ev)
				}
			}
			return apply(ev)
		})
	})
	if err != nil {
		return nil, nil, err
	}
	if committed {
		r, err := replayApply(g, req, mine)
		return nil, r, err
	}

	base := time.Now()
	if failed := checkExpectations(g, req, rev, base); failed != nil {
		return nil, nil, failed
	}

	// Strictly increasing per event: ids hash `at`, so a shared stamp would
	// make two same-titled adds one node (EVENTS.md §2.5).
	tick := 0
	at := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Microsecond)
	}
	reply := &applyReply{Committed: !dryRun, Key: req.Key, Tmp: map[string]string{}}
	created := map[string]bool{} // ids a dry run must not report: they would differ on a real apply
	var events []event.Event
	emit := func(i int, ev event.Event, err error) error {
		if err != nil {
			return err
		}
		if err := event.InTx(ev, req.Key, i); err != nil {
			return err
		}
		if err := g.Apply(ev); err != nil {
			return err
		}
		events = append(events, ev)
		return nil
	}
	resolve := func(i int, q string) (*graph.Node, error) {
		if strings.HasPrefix(q, "$") {
			return g.Nodes[reply.Tmp[q]], nil
		}
		n, err := g.Resolve(q)
		if err != nil {
			idx := i
			return nil, resolveErr(err, &applyErr{i: &idx})
		}
		return n, nil
	}

	for i, op := range req.ops {
		res := applyResult{I: i, Op: op.Op}
		before := len(events)
		switch op.Op {
		case "add":
			ns := op.NS
			if ns == "" {
				ns = defaultNS
			}
			node, err := event.NodeAdd(ns, op.Kind, op.Title, by, op.Source, at())
			if err := emit(i, node, err); err != nil {
				return nil, nil, err
			}
			id := node.ID()
			for _, ref := range op.Refs {
				if slices.Contains(g.Nodes[id].Refs, ref) {
					continue // a repeated ref in one add binds once
				}
				ev, err := event.RefAdd(id, ref, by, op.Source, at())
				if err := emit(i, ev, err); err != nil {
					return nil, nil, err
				}
			}
			res.Node = id
			created[id] = true
			if op.As != "" {
				reply.Tmp[op.As] = id
			}

		case "ref.add", "ref.rm":
			n, err := resolve(i, op.Node)
			if err != nil {
				return nil, nil, err
			}
			res.Node = n.ID
			if op.Op == "ref.add" {
				// Already bound is the desired state; re-emitting would grow the
				// log on every retry (EVENTS.md §3.9).
				if !slices.Contains(n.Refs, op.Ref) {
					ev, err := event.RefAdd(n.ID, op.Ref, by, op.Source, at())
					if err := emit(i, ev, err); err != nil {
						return nil, nil, err
					}
				}
				break
			}
			for _, target := range g.FindRefs(n.ID, op.Ref) {
				ev := event.New(event.KindRefRetract, by, op.Source, at())
				ev["target"] = target
				if err := emit(i, ev, ev.Seal()); err != nil {
					return nil, nil, err
				}
			}

		case "claim":
			n, err := resolve(i, op.Node)
			if err != nil {
				return nil, nil, err
			}
			res.Node = n.ID
			holder := op.Holder
			if holder == "" || holder == "me" {
				holder = by
			}
			if op.UnlessHeld && g.ClaimBy(n.ID, holder, base) != nil {
				break
			}
			stamp := at()
			var expires time.Time
			if !op.NoExpiry {
				expires = stamp.Add(op.ttl)
			}
			ev, err := event.ClaimAdd(n.ID, holder, op.Note, by, op.Source, stamp, expires)
			if err := emit(i, ev, err); err != nil {
				return nil, nil, err
			}
			res.Claim = ev.ID()
			created[ev.ID()] = true

		case "release":
			var targets []*graph.Claim
			if op.Claim != "" {
				// An unknown or already-released claim is the desired state
				// already, as with the CLI's release (SPEC.md §9.3).
				if c, ok := g.Claims[op.Claim]; ok {
					targets = append(targets, c)
					res.Node = g.Canonical(c.Node)
				}
			} else {
				n, err := resolve(i, op.Node)
				if err != nil {
					return nil, nil, err
				}
				res.Node = n.ID
				for _, c := range g.LiveClaims(n.ID, base) {
					if op.All || c.Holder == op.Holder {
						targets = append(targets, c)
					}
				}
			}
			released := []string{}
			for _, c := range targets {
				ev, err := event.ClaimRelease(c.ID, by, event.SourceAsserted, at())
				if err := emit(i, ev, err); err != nil {
					return nil, nil, err
				}
				released = append(released, c.ID)
			}
			res.Claims = &released

		case "link":
			from, err := resolve(i, op.From)
			if err != nil {
				return nil, nil, err
			}
			to, err := resolve(i, op.To)
			if err != nil {
				return nil, nil, err
			}
			// An existing triple is the desired state, as `link --if-absent`
			// treats it; a batch is retried, and retries must not grow the log.
			if found := g.FindEdge(op.Kind, from.ID, to.ID); found != nil {
				res.Edge = found.ID
				break
			}
			if err := g.CheckEdge(op.Kind, from.ID, to.ID); err != nil {
				idx := i
				var cycle *graph.ErrCycle
				if errors.As(err, &cycle) {
					return nil, nil, &applyErr{code: exitRejected, i: &idx, msg: err.Error(), candidates: cycle.Path}
				}
				return nil, nil, err
			}
			ev, err := event.EdgeAdd(op.Kind, from.ID, to.ID, by, op.Source, at())
			if err := emit(i, ev, err); err != nil {
				return nil, nil, err
			}
			res.Edge = ev.ID()
			created[ev.ID()] = true

		case "done":
			n, err := resolve(i, op.Node)
			if err != nil {
				return nil, nil, err
			}
			res.Node = n.ID
			if n.State != event.StateDone {
				ev, err := event.NodeState(n.ID, event.StateDone, by, event.SourceAsserted, at())
				if err := emit(i, ev, err); err != nil {
					return nil, nil, err
				}
			}
		}
		res.Changed = len(events) > before
		reply.Results = append(reply.Results, res)
	}

	if dryRun {
		reply.Tmp = nil
		for k := range reply.Results {
			r := &reply.Results[k]
			if created[r.Node] {
				r.Node = ""
			}
			if created[r.Claim] {
				r.Claim = ""
			}
			if created[r.Edge] {
				r.Edge = ""
			}
		}
	}
	if len(reply.Tmp) == 0 {
		reply.Tmp = nil
	}
	if len(events) == 0 {
		// Nothing to commit, so no marker either: an all-no-op batch records no
		// key, and its retry is evaluated afresh, which is safe because nothing
		// landed.
		return nil, reply, nil
	}
	commit, err := event.TxCommit(req.Key, len(events), by, at())
	if err != nil {
		return nil, nil, err
	}
	return append(events, commit), reply, nil
}

// checkExpectations evaluates every expectation against the pre-batch fold and
// returns a conflict naming all that failed, or nil.
func checkExpectations(g *graph.Graph, req *applyRequest, rev string, now time.Time) error {
	var failed []applyFailure
	if req.Rev != "" && req.Rev != rev {
		exp, _ := json.Marshal(map[string]string{"rev": req.Rev})
		failed = append(failed, applyFailure{Expect: exp, Actual: map[string]string{"rev": rev}})
	}
	type claimJSON struct {
		ID     string `json:"id"`
		Holder string `json:"holder"`
		At     string `json:"at"`
	}
	type liveJSON struct {
		Node  string `json:"node"`
		Claim string `json:"claim"`
		At    string `json:"at"`
	}
	// want resolves expected node names. An unresolvable one stays as typed:
	// it cannot match any real node, so the set comparison fails and reports
	// what is actually there.
	want := func(qs []string) (map[string]bool, *applyErr) {
		out := map[string]bool{}
		for _, q := range qs {
			n, err := g.Resolve(q)
			var amb *graph.ErrAmbiguous
			switch {
			case err == nil:
				out[n.ID] = true
			case errors.As(err, &amb):
				return nil, resolveErr(err, &applyErr{})
			default:
				out[q] = true
			}
		}
		return out, nil
	}
	sameSet := func(a map[string]bool, b []string) bool {
		seen := map[string]bool{}
		for _, x := range b {
			seen[x] = true
		}
		if len(seen) != len(a) {
			return false
		}
		for x := range a {
			if !seen[x] {
				return false
			}
		}
		return true
	}

	for i, x := range req.expects {
		idx := i
		fail := func(actual any) {
			failed = append(failed, applyFailure{I: &idx, Expect: x.raw, Actual: actual})
		}
		switch {
		case x.State != nil || x.Claims != nil:
			n, err := g.Resolve(*x.Node)
			if err != nil {
				var nf *graph.ErrNotFound
				if errors.As(err, &nf) {
					fail(map[string]bool{"exists": false})
					continue
				}
				ae := resolveErr(err, &applyErr{expect: &idx})
				return ae
			}
			if x.State != nil {
				if n.State != *x.State {
					fail(map[string]string{"state": n.State})
				}
				continue
			}
			live := g.LiveClaims(n.ID, now)
			ids := make([]string, 0, len(live))
			actual := make([]claimJSON, 0, len(live))
			for _, c := range live {
				ids = append(ids, c.ID)
				actual = append(actual, claimJSON{c.ID, c.Holder, render.Stamp(c.At)})
			}
			exp := map[string]bool{}
			for _, id := range *x.Claims {
				exp[id] = true
			}
			if !sameSet(exp, ids) {
				fail(map[string]any{"claims": actual})
			}

		case x.LiveOn != nil:
			exp, ae := want(*x.LiveOn)
			if ae != nil {
				ae.expect = &idx
				return ae
			}
			var nodes []string
			actual := []liveJSON{}
			for _, n := range g.Ordered() {
				if (x.NS != nil && n.NS != *x.NS) || (x.Kind != nil && n.Kind != *x.Kind) {
					continue
				}
				for _, c := range g.LiveClaims(n.ID, now) {
					if c.Holder == *x.Holder {
						nodes = append(nodes, n.ID)
						actual = append(actual, liveJSON{n.ID, c.ID, render.Stamp(c.At)})
					}
				}
			}
			if !sameSet(exp, nodes) {
				fail(map[string]any{"live_on": actual})
			}

		case x.BoundTo != nil:
			exp, ae := want(*x.BoundTo)
			if ae != nil {
				ae.expect = &idx
				return ae
			}
			actual := []string{}
			for _, n := range g.Ordered() {
				if n.State != event.StateDropped && slices.Contains(n.Refs, *x.Ref) {
					actual = append(actual, n.ID)
				}
			}
			if !sameSet(exp, actual) {
				fail(map[string]any{"bound_to": actual})
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &applyConflict{Error_: "precondition failed", Code: exitConflict, Rev: rev, Failed: failed}
}

// replayApply answers a key that is already committed: the caller lost the
// reply and retried. Nothing is applied again and expectations are not
// re-checked — they held when the batch committed. Results are rebuilt from
// the logged events, grouped by the op that wrote them.
func replayApply(g *graph.Graph, req *applyRequest, mine []event.Event) (*applyReply, error) {
	byOp := make([][]event.Event, len(req.ops))
	for _, ev := range mine {
		i, ok := ev.Int(event.FieldTxOp)
		if !ok || i < 0 || i >= len(req.ops) || !slices.Contains(opEvents[req.ops[i].Op], ev.Kind()) {
			return nil, &applyErr{code: exitUsage,
				msg: fmt.Sprintf("key %q was committed by a different request; use a new key per intent", req.Key)}
		}
		byOp[i] = append(byOp[i], ev)
	}
	reply := &applyReply{Committed: true, Replayed: true, Key: req.Key, Tmp: map[string]string{}}
	lookup := func(q string) string {
		if strings.HasPrefix(q, "$") {
			return reply.Tmp[q]
		}
		if n, err := g.Resolve(q); err == nil {
			return n.ID
		}
		return ""
	}
	for i, op := range req.ops {
		evs := byOp[i]
		res := applyResult{I: i, Op: op.Op, Changed: len(evs) > 0}
		switch op.Op {
		case "add":
			for _, ev := range evs {
				if ev.Kind() == event.KindNodeAdd {
					res.Node = ev.ID()
				}
			}
			if res.Node == "" {
				return nil, &applyErr{code: exitUsage,
					msg: fmt.Sprintf("key %q was committed by a different request; use a new key per intent", req.Key)}
			}
			if op.As != "" {
				reply.Tmp[op.As] = res.Node
			}
		case "claim":
			res.Node = lookup(op.Node)
			if len(evs) > 0 {
				res.Claim = evs[0].ID()
				res.Node = g.Canonical(evs[0].Str("node"))
			}
		case "release":
			released := []string{}
			for _, ev := range evs {
				released = append(released, ev.Str("target"))
			}
			res.Claims = &released
			if op.Claim != "" {
				if c := g.Claims[op.Claim]; c != nil {
					res.Node = g.Canonical(c.Node)
				} else if len(released) > 0 {
					for _, n := range g.Ordered() {
						for _, c := range g.ReleasedClaimsFor(n.ID) {
							if c.ID == op.Claim {
								res.Node = n.ID
							}
						}
					}
				}
			} else {
				res.Node = lookup(op.Node)
			}
		case "link":
			if len(evs) > 0 {
				res.Edge = evs[0].ID()
			} else if e := g.FindEdge(op.Kind, lookup(op.From), lookup(op.To)); e != nil {
				res.Edge = e.ID
			}
		default:
			res.Node = lookup(op.Node)
		}
		reply.Results = append(reply.Results, res)
	}
	if len(reply.Tmp) == 0 {
		reply.Tmp = nil
	}
	return reply, nil
}
