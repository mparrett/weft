// Package render produces weft's human and machine output.
//
// Identical graphs must render byte-identically (SPEC.md §9.5): the text form
// is what humans review and what lands in git, and nothing fails when ordering
// drifts — the diffs just go noisy months later. Every ordering here is
// therefore explicit, never map iteration.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/query"
)

// MinWidth is the shortest id prefix ever displayed (EVENTS.md §2.4).
const MinWidth = 7

// FullWidth prints an id whole. It is what to use when the set of ids to stay
// distinct from is unknown — an untruncated id always resolves, where a prefix
// chosen against a guess may not.
const FullWidth = 64

// Width returns the display width for ids: the shortest length at or above
// MinWidth at which every id in the set stays distinct.
//
// A fixed 7 is unsafe — a uniform 28-bit prefix over 10,000 ids collides with
// probability ~17% — so the width expands, git-style, rather than assuming.
func Width(ids []string) int {
	for w := MinWidth; w < 64; w++ {
		seen := make(map[string]struct{}, len(ids))
		collision := false
		for _, id := range ids {
			p := id
			if len(p) > w {
				p = p[:w]
			}
			if _, dup := seen[p]; dup {
				collision = true
				break
			}
			seen[p] = struct{}{}
		}
		if !collision {
			return w
		}
	}
	return 64
}

// Short abbreviates an id to the given width.
func Short(id string, width int) string {
	if len(id) <= width {
		return id
	}
	return id[:width]
}

// NodeList renders nodes as aligned text, in the given order.
//
// width is passed in rather than derived from these nodes, because a filtered
// listing must still print ids at the width that is unambiguous across the
// whole store — a user pastes what they see, and a prefix that is unique within
// one namespace can be ambiguous outside it.
// showNS adds a namespace column. It is for listings that span namespaces,
// where the rows are otherwise indistinguishable — the namespace is the only
// thing separating two nodes with the same title in different projects. A
// single-namespace listing repeats one value on every row, so it stays off.
func NodeList(w io.Writer, nodes []*graph.Node, width int, showNS bool) error {
	if len(nodes) == 0 {
		return nil
	}

	kindCol, nsCol := 0, 0
	for _, n := range nodes {
		if len(n.Kind) > kindCol {
			kindCol = len(n.Kind)
		}
		if len(n.NS) > nsCol {
			nsCol = len(n.NS)
		}
	}

	for _, n := range nodes {
		// State is shown only when it is not the default, so a routine listing
		// stays scannable.
		suffix := ""
		if n.State != "" && n.State != "open" {
			suffix = "  (" + n.State + ")"
		}
		nsField := ""
		if showNS {
			nsField = fmt.Sprintf("%-*s  ", nsCol, n.NS)
		}
		if _, err := fmt.Fprintf(w, "%s  %s%-*s  %s%s\n",
			Short(n.ID, width), nsField, kindCol, n.Kind, n.Title, suffix); err != nil {
			return err
		}
	}
	return nil
}

// NodeJSON is the machine-readable form. Ids are always full: abbreviation is
// a display concern, and a consumer that stores a short id has stored something
// that can later become ambiguous.
//
// Timestamps are absolute RFC3339, never the relative form the human output
// uses: a consumer stores what it reads, and "3d ago" is only true at the moment
// it was printed. Omitted when zero rather than rendered as year 1.
//
// by and source ride unconditionally here while export gates them behind
// --include source, and the divergence is deliberate: export's default is the
// frozen contract other tools consume, pinned byte-for-byte by goldens
// (SPEC.md §6.4), so anything richer there is opt-in. This shape is the
// per-command convenience answer — it carries what the fold knows, and hiding
// a field the caller must then re-derive from the log would repeat the written-but-unreadable
// `source` bug.
type NodeJSON struct {
	ID      string   `json:"id"`
	NS      string   `json:"ns"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	State   string   `json:"state"`
	By      string   `json:"by,omitempty"`
	Source  string   `json:"source,omitempty"`
	Refs    []string `json:"refs,omitempty"`
	Created string   `json:"created,omitempty"`
	Touched string   `json:"touched,omitempty"`
}

// nodeJSONOf builds the machine-readable form of a node, so every producer of a
// NodeJSON formats its timestamps the same way.
func nodeJSONOf(n *graph.Node) NodeJSON {
	refs := append([]string(nil), n.Refs...)
	sort.Strings(refs)
	return NodeJSON{
		ID: n.ID, NS: n.NS, Kind: n.Kind, Title: n.Title, State: n.State,
		By: n.By, Source: n.Source, Refs: refs,
		Created: Stamp(n.Created), Touched: Stamp(n.Touched),
	}
}

// Stamp formats a timestamp for machine output, or "" if it was never set.
func Stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// NodesJSON renders nodes as a JSON object with a "nodes" array.
func NodesJSON(w io.Writer, nodes []*graph.Node) error {
	out := struct {
		Nodes []NodeJSON `json:"nodes"`
	}{Nodes: make([]NodeJSON, 0, len(nodes))}

	for _, n := range nodes {
		out.Nodes = append(out.Nodes, nodeJSONOf(n))
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ChangedJSON renders nodes alongside whether the command actually wrote
// anything, for the mutations whose no-op is a success rather than an error.
//
// Exit code cannot carry this: a retraction that found nothing and one that
// removed an edge are both 0, deliberately, because the desired state holds
// either way (SPEC.md §9.3). Without the flag a caller reconciling a graph
// against a source of truth cannot tell whether it converged or was already
// there — the same reason a route carries the kinds it walked, since an empty
// answer with a filter is a different fact from an empty answer without one.
func ChangedJSON(w io.Writer, nodes []*graph.Node, field string, changed bool) error {
	out := map[string]any{field: changed}
	rows := make([]NodeJSON, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, nodeJSONOf(n))
	}
	out["nodes"] = rows

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// Error renders a failure as JSON for --json consumers (BUILD.md §5).
func Error(w io.Writer, code int, msg string, candidates []string) error {
	out := struct {
		Error      string   `json:"error"`
		Code       int      `json:"code"`
		Candidates []string `json:"candidates,omitempty"`
	}{Error: msg, Code: code, Candidates: candidates}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// Added renders the confirmation for a newly created node. The id is echoed at
// display width so it can be pasted straight into the next command.
//
// The namespace is echoed too, because it is the one field `add` decides on the
// caller's behalf and the only one that can be silently wrong. Resolution reads
// $WEFT_NS, a config mapping, then the git remote (SPEC.md §5), so a shell in an
// unexpected directory files work under an unexpected project and every command
// still reports success. Nothing else in the output distinguishes the right
// answer from the wrong one, and the namespace is fixed at creation — there is
// no move — so a run of wrong ones costs an add and a merge each to correct.
// It takes a width rather than the id set, like Node does, because the caller
// may not have one: `add` prints after the write has committed, and if the log
// cannot be read back there is no set to be distinct from (BUILD.md §5).
func Added(w io.Writer, n *graph.Node, width int) error {
	// A dropped reuse says so, in the listing style. Only dropped: a done node
	// handed back by --if-absent keeps the retry byte-identical (that is the
	// pinned contract), and done work is still the work — but a dropped node
	// is one every readiness query discounts, reachable only through the
	// explicit --even-dropped, and handing it back in the same shape as a
	// fresh write tells the writer nothing.
	suffix := ""
	if n.State == "dropped" {
		suffix = "  (" + n.State + ")"
	}
	_, err := fmt.Fprintf(w, "%s  %s  [%s]%s\n", Short(n.ID, width), n.Title, n.NS, suffix)
	return err
}

// Node renders one node in detail. Milestone 4 extends this with the
// neighborhood; the header line is stable so goldens survive that.
//
// No age line: ages are relative to now, and this is the form with no now to be
// relative to. NodeObserved is the dated one.
func Node(w io.Writer, n *graph.Node, width int) error {
	return node(w, n, width, nil, time.Time{})
}

func node(w io.Writer, n *graph.Node, width int, annotations map[string]string, now time.Time) error {
	if _, err := fmt.Fprintf(w, "%s  %s  %s\n", Short(n.ID, width), n.Kind, n.Title); err != nil {
		return err
	}
	fields := [][2]string{{"ns", n.NS}, {"state", n.State}, {"source", nodeProvenance(n)}, {"age", nodeAge(n, now)}}
	for _, f := range fields {
		if f[1] == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %-6s %s\n", f[0], f[1]); err != nil {
			return err
		}
	}
	refs := append([]string(nil), n.Refs...)
	sort.Strings(refs)
	for i, r := range refs {
		label := ""
		if i == 0 {
			label = "refs"
		}
		if _, err := fmt.Fprintf(w, "  %-6s %s%s\n", label, r, annotations[r]); err != nil {
			return err
		}
	}
	return nil
}

// NodeObserved renders one node, annotating each ref with what upstream last said.
//
// The annotation always carries its age. An observation shown as though it were
// current is worse than none: it invites a decision on data nobody re-checked
// (SPEC.md §10 — import is a cache with provenance, never authority).
//
// Both clocks render as ages, never absolutes (SPEC.md §6.3): "modified" is
// the artifact's own clock, "observed" is weft's, and either alone answers
// only half of "how current is this".
func NodeObserved(w io.Writer, n *graph.Node, width int, obs map[string]*graph.Observation, now time.Time) error {
	annotations := make(map[string]string, len(obs))
	for ref, o := range obs {
		var facts []string
		if o.State != "" {
			facts = append(facts, o.State)
		}
		if o.Hash != "" {
			facts = append(facts, shortHash(o.Hash))
		}
		ages := "observed " + Age(o.At, now)
		if m, err := time.Parse(time.RFC3339, o.Modified); err == nil {
			ages = "modified " + Age(m, now) + ", " + ages
		}
		ann := "    "
		if len(facts) > 0 {
			ann += strings.Join(facts, "  ") + "  "
		}
		annotations[ref] = ann + "(" + ages + ")"
	}
	return node(w, n, width, annotations, now)
}

// shortHash abbreviates a self-describing hash for display: the algorithm
// stays, the digest is cut to a glanceable prefix. Display only — the full
// digest is in the log and the export, and nothing resolves by hash.
func shortHash(h string) string {
	algo, digest, ok := strings.Cut(h, ":")
	if !ok || len(digest) <= 8 {
		return h
	}
	return algo + ":" + digest[:8] + "…"
}

// nodeProvenance renders who asserted the node and how they knew, in the same
// vocabulary claims use ("inferred, per agent:x"), or "" to omit the line — a
// hand-built node has no provenance, and rendering an empty one would assert
// that nothing is known rather than that nothing was recorded.
func nodeProvenance(n *graph.Node) string {
	if n.Source == "" {
		return ""
	}
	if n.By == "" {
		return n.Source
	}
	return n.Source + ", per " + n.By
}

// nodeAge renders the node's own timestamps, or "" to omit the line entirely.
//
// Empty for a node with no Created, which means a graph built in memory rather
// than folded from a log — the renderers are used that way by tests and by
// anything holding a hand-built node, and a line reading "created 56 years ago"
// is worse than no line.
//
// Touched is dropped when it adds nothing: a node nobody has revisited since
// asserting it would otherwise print the same age twice, and the second copy
// reads as a second fact.
func nodeAge(n *graph.Node, now time.Time) string {
	if n.Created.IsZero() || now.IsZero() {
		return ""
	}
	out := "created " + Age(n.Created, now)
	if n.Touched.After(n.Created) {
		out += ", touched " + Age(n.Touched, now)
	}
	return out
}

// Age renders how long ago something was observed, coarsely. Coarse on purpose:
// the question an observation answers is "should I re-check", and that turns on
// days, not seconds.
func Age(at, now time.Time) string {
	d := now.Sub(at)
	switch {
	// A timestamp ahead of now is a clock, not a fact: `at` comes from whichever
	// machine wrote the event, and one running fast dates its writes in the
	// future. Saying so is the point — the negative duration otherwise falls
	// into the "just now" arm below and renders as permanently fresh, which is
	// worse than visibly broken because nothing about it looks wrong.
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// MergedFrom lists the nodes folded into the one being shown.
//
// Without this, `show <old id>` prints a different node's title with no
// explanation of why — the redirect has to be visible or the tool looks like it
// lost something (SPEC.md §6.2).
func MergedFrom(w io.Writer, merged []*graph.Node, width int) error {
	for i, n := range merged {
		label := ""
		if i == 0 {
			label = "merged"
		}
		if _, err := fmt.Fprintf(w, "  %-6s %s  %s\n", label, Short(n.ID, width), n.Title); err != nil {
			return err
		}
	}
	return nil
}

// SortNodes orders nodes for display. Insertion order is file order, which is
// the log's own total order (EVENTS.md §5.1) and therefore both deterministic
// and meaningful — newest last, like a log.
func SortNodes(nodes []*graph.Node, byTitle bool) {
	if !byTitle {
		return
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Title != nodes[j].Title {
			return nodes[i].Title < nodes[j].Title
		}
		return nodes[i].ID < nodes[j].ID
	})
}

// SortRecent orders nodes by what moved last, newest first.
//
// Stable, and given a file-ordered input that matters: `at` is a wall clock, so
// two nodes can carry the same timestamp or a skewed one, and file order is what
// breaks the tie. That keeps identical graphs printing identically (SPEC.md
// §9.5) even though the sort key itself is only a hint.
//
// A node with no Touched sorts last rather than first, which is where the zero
// time would otherwise put it.
func SortRecent(nodes []*graph.Node) {
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].Touched.After(nodes[j].Touched)
	})
}

// Plural is a small helper for diagnostics.
func Plural(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, strings.TrimSuffix(singular, "s"))
}

// edgeLabels give each edge kind a reading in both directions. The inverse
// reading is what makes a neighborhood scannable: "A depends-on B" is the same
// fact as "B blocks A", but only the second answers "who is waiting on me".
var edgeLabels = map[string][2]string{
	graph.EdgeDependsOn:     {"depends on", "blocks"},
	graph.EdgePartOf:        {"part of", "contains"},
	graph.EdgeSupersedes:    {"supersedes", "superseded by"},
	graph.EdgeDerivedFrom:   {"derived from", "led to"},
	graph.EdgeConflictsWith: {"conflicts with", "conflicts with"},
	graph.EdgeSameAs:        {"same as", "same as"},
}

func edgeLabel(kind string, outbound bool) string {
	pair, ok := edgeLabels[kind]
	if !ok {
		return kind
	}
	if outbound {
		return pair[0]
	}
	return pair[1]
}

// Neighborhood renders every edge touching a node, grouped by reading.
//
// Everything needed to orient is in this one block, because making an agent
// issue a second call to learn what blocks a node is how the tool stops getting
// used (SPEC.md §9.4).
// elsewhere annotates a node living in a different namespace than the query was
// rooted at. Node-rooted queries are always global (SPEC §5), so a neighbour in
// another project is the ordinary case — and that it *is* elsewhere is often the
// whole answer: "you are blocked by work in a repo you were not thinking about"
// is what weft exists to say, and it is unsayable if the namespace is invisible.
//
// Shown only on difference, like state, so a single-namespace neighbourhood
// stays scannable and its output stays byte-identical.
func elsewhere(n *graph.Node, homeNS string) string {
	if n == nil || n.NS == "" || n.NS == homeNS {
		return ""
	}
	return "  [" + n.NS + "]"
}

func Neighborhood(w io.Writer, neighbors []graph.Neighbor, width int, homeNS string) error {
	if len(neighbors) == 0 {
		return nil
	}

	// Group by reading, preserving first-appearance order so output stays
	// canonical without an arbitrary alphabetical sort.
	var order []string
	groups := map[string][]graph.Neighbor{}
	for _, n := range neighbors {
		label := edgeLabel(n.Edge.Kind, n.Outbound)
		if _, seen := groups[label]; !seen {
			order = append(order, label)
		}
		groups[label] = append(groups[label], n)
	}

	for _, label := range order {
		if _, err := fmt.Fprintf(w, "\n  %s\n", label); err != nil {
			return err
		}
		for _, n := range groups[label] {
			// A dangling edge is a normal state, not corruption: the other end
			// may simply not have been asserted yet (SPEC.md §9.3).
			if n.Node == nil {
				if _, err := fmt.Fprintf(w, "    %s  (not in this store)\n", Short(n.ID, width)); err != nil {
					return err
				}
				continue
			}
			state := ""
			if n.Node.State != "" && n.Node.State != "open" {
				state = "  (" + n.Node.State + ")"
			}
			if _, err := fmt.Fprintf(w, "    %s  %s  %s%s%s\n",
				Short(n.ID, width), n.Node.Kind, n.Node.Title, state, elsewhere(n.Node, homeNS)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Path renders a chain of node ids with titles, used for cycle diagnostics.
func Path(w io.Writer, ids []string, g *graph.Graph, width int) error {
	for i, id := range ids {
		marker := "  "
		if i > 0 {
			marker = "→ "
		}
		title := "(not in this store)"
		if n, ok := g.Nodes[id]; ok {
			title = n.Title
		}
		if _, err := fmt.Fprintf(w, "  %s%s  %s\n", marker, Short(id, width), title); err != nil {
			return err
		}
	}
	return nil
}

// Claims renders a node's claims. Every claim is shown, expired ones included
// and marked: hiding expiry would make abandoned work indistinguishable from
// unclaimed work, and two live claims is a condition to surface rather than
// resolve (SPEC.md §4.3).
func Claims(w io.Writer, claims []*graph.Claim, now time.Time) error {
	if len(claims) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n  claimed by\n"); err != nil {
		return err
	}
	for _, c := range claims {
		marks := []string{c.Source}
		if c.Expired(now) {
			marks = append(marks, "expired")
		}
		// An inferred claim is a guess about someone else; saying who guessed
		// is what keeps it from being read as fact.
		if c.By != c.Holder {
			marks = append(marks, "per "+c.By)
		}
		line := fmt.Sprintf("    %s  (%s)", c.Holder, strings.Join(marks, ", "))
		if c.Note != "" {
			line += "  " + c.Note
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// Blockers renders why a node cannot start.
//
// Each blocker shows the route that reaches it and whoever holds a live claim,
// because "blocked" without a name and a path is half an answer — the next
// action is usually to go talk to whoever holds the thing in the way.
func Blockers(w io.Writer, blockers []query.Blocker, g *graph.Graph, width int, homeNS string) error {
	if _, err := fmt.Fprintf(w, "\nblocked by\n"); err != nil {
		return err
	}
	for _, b := range blockers {
		title := "(not in this store)"
		kind := ""
		if b.Node != nil {
			title = b.Node.Title
			kind = b.Node.Kind + "  "
		}
		if _, err := fmt.Fprintf(w, "\n  %s  %s%s%s\n",
			Short(b.ID, width), kind, title, elsewhere(b.Node, homeNS)); err != nil {
			return err
		}
		// Which of these to start first. Without it the list reads as equally
		// available work, and starting on a gated one gets it re-done.
		if b.Gates > 0 {
			waits := "blockers here wait"
			if b.Gates == 1 {
				waits = "blocker here waits"
			}
			if _, err := fmt.Fprintf(w, "    %d other %s on this\n", b.Gates, waits); err != nil {
				return err
			}
		}
		for _, c := range b.Claims {
			marks := c.Source
			if c.By != c.Holder {
				marks += ", per " + c.By
			}
			if _, err := fmt.Fprintf(w, "    held by %s  (%s)\n", c.Holder, marks); err != nil {
				return err
			}
		}
		// The route matters once the blocker is more than one hop away.
		if len(b.Path) > 2 {
			if _, err := fmt.Fprintf(w, "    via %s\n", viaPath(b.Path, g, width)); err != nil {
				return err
			}
		}
	}
	return nil
}

func viaPath(ids []string, g *graph.Graph, width int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		label := Short(id, width)
		if n, ok := g.Nodes[id]; ok {
			label = n.Title
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " → ")
}

// Dependents renders the reverse closure: everything waiting on a node.
//
// annotate adds the relations each row carries to the node it was reached
// from, in the row's own reading — "part of", "derived from", "depends on" —
// every one of them when several kinds connect the pair, since naming one
// would assert it as *the* relation. It is on whenever the walk actually
// crossed anything beyond depends-on: an indented tree under a command named
// for dependencies otherwise asserts that every row *waits*, which a part-of
// child does not. The pure depends-on walk stays bare, because
// there the tree really is the wait and the label on every row would say the
// command's own name.
//
// A depth-1 row relates to the queried node; deeper rows relate to their
// parent in the walk, which level-order output does not place above them —
// with two depth-1 rows, the indentation points at the wrong one. So a deeper
// annotation names its parent, making the claim checkable instead of a
// relation to a node the reader cannot identify.
func Dependents(w io.Writer, deps []query.Dependent, width int, homeNS string, annotate bool) error {
	for _, d := range deps {
		title := "(not in this store)"
		kind := ""
		if d.Node != nil {
			title = d.Node.Title
			kind = d.Node.Kind + "  "
		}
		relation := ""
		if annotate && len(d.Vias) > 0 {
			labels := make([]string, 0, len(d.Vias))
			for _, k := range d.Vias {
				labels = append(labels, edgeLabel(k, true))
			}
			rel := strings.Join(labels, ", ")
			if d.Depth > 1 {
				rel += " " + Short(d.Parent, width)
			}
			relation = "  (" + rel + ")"
		}
		// Indentation is depth, so the shape of the walk is visible at a glance.
		if _, err := fmt.Fprintf(w, "%s%s  %s%s%s%s\n",
			strings.Repeat("  ", d.Depth-1), Short(d.ID, width), kind, title,
			relation, elsewhere(d.Node, homeNS)); err != nil {
			return err
		}
	}
	return nil
}

// Route renders a path between two nodes, annotating each hop with the relation
// that was followed and the direction it was read in.
func Route(w io.Writer, from *graph.Node, hops []query.Hop, width int) error {
	if _, err := fmt.Fprintf(w, "%s  %s\n", Short(from.ID, width), from.Title); err != nil {
		return err
	}
	// The route is rooted at `from`, so its namespace is the one to read hops
	// against; no caller needs to supply it.
	for _, h := range hops {
		title := "(not in this store)"
		if h.Node != nil {
			title = h.Node.Title
		}
		if _, err := fmt.Fprintf(w, "  %s %s  %s%s\n",
			edgeLabel(h.Edge.Kind, h.Outbound), Short(h.ID, width), title,
			elsewhere(h.Node, from.NS)); err != nil {
			return err
		}
	}
	return nil
}

// NeighborhoodJSON is what `show --json` emits: everything the text form
// shows, in one object.
//
// It deliberately does not reuse the {"nodes":[...]} shape. `show` is the
// one-call-to-orient command (SPEC.md §9.4), and answering it with a bare node
// forces a consumer into the follow-up calls that constraint exists to prevent —
// which would fall hardest on agents, the audience --json is *for*.
type NeighborhoodJSON struct {
	Node       NodeJSON         `json:"node"`
	Observed   []observedJSON   `json:"observed,omitempty"`
	Claims     []claimJSON      `json:"claims,omitempty"`
	MergedFrom []mergedFromJSON `json:"merged_from,omitempty"`
	Edges      []edgeJSON       `json:"edges,omitempty"`
}

type observedJSON struct {
	Ref      string `json:"ref"`
	State    string `json:"state,omitempty"`
	Title    string `json:"title,omitempty"`
	Created  string `json:"created,omitempty"`
	Modified string `json:"modified,omitempty"`
	Hash     string `json:"hash,omitempty"`
	At       string `json:"at,omitempty"`
	By       string `json:"by,omitempty"`
}

type claimJSON struct {
	Holder  string `json:"holder"`
	By      string `json:"by,omitempty"`
	Source  string `json:"source,omitempty"`
	Note    string `json:"note,omitempty"`
	At      string `json:"at,omitempty"`
	Expires string `json:"expires,omitempty"`
	Live    bool   `json:"live"`
}

// mergedFromJSON carries the merged node's own provenance, because nothing
// else can: show redirects the merged id to its survivor, and Ordered() omits
// merged nodes from every listing and export. Without these two fields, who
// asserted the losing node and how they knew is written to the log and
// readable through no interface — the same written-but-unreadable shape the
// by/source fold fix closed, one level down.
type mergedFromJSON struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	By     string `json:"by,omitempty"`
	Source string `json:"source,omitempty"`
}

// edgeJSON carries both the raw kind and the reading, so a consumer can branch
// on `kind` without having to know that an inbound depends-on reads "blocks".
type edgeJSON struct {
	Kind      string `json:"kind"`
	Direction string `json:"direction"`
	Reading   string `json:"reading"`
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Known     bool   `json:"known"`
}

// Neighborhood renders a node and everything touching it for --json.
func NeighborhoodJSONOf(n *graph.Node, obs map[string]*graph.Observation, claims []*graph.Claim,
	mergedFrom []*graph.Node, neighbors []graph.Neighbor, now time.Time) NeighborhoodJSON {

	out := NeighborhoodJSON{Node: nodeJSONOf(n)}
	refs := out.Node.Refs
	for _, ref := range refs {
		o, ok := obs[ref]
		if !ok {
			continue
		}
		out.Observed = append(out.Observed, observedJSON{
			Ref: ref, State: o.State, Title: o.Title, By: o.By,
			Created: o.Created, Modified: o.Modified, Hash: o.Hash,
			At: o.At.Format(time.RFC3339Nano),
		})
	}
	for _, c := range claims {
		row := claimJSON{
			Holder: c.Holder, By: c.By, Source: c.Source, Note: c.Note, Live: c.Live(now),
			At: c.At.Format(time.RFC3339Nano),
		}
		if !c.Expires.IsZero() {
			row.Expires = c.Expires.Format(time.RFC3339Nano)
		}
		out.Claims = append(out.Claims, row)
	}
	for _, m := range mergedFrom {
		out.MergedFrom = append(out.MergedFrom, mergedFromJSON{ID: m.ID, Title: m.Title, By: m.By, Source: m.Source})
	}
	for _, nb := range neighbors {
		row := edgeJSON{
			Kind: nb.Edge.Kind, ID: nb.ID, Known: nb.Node != nil,
			Direction: "in", Reading: edgeLabel(nb.Edge.Kind, nb.Outbound),
		}
		if nb.Outbound {
			row.Direction = "out"
		}
		if nb.Node != nil {
			row.Title = nb.Node.Title
		}
		out.Edges = append(out.Edges, row)
	}
	return out
}

// blockerJSON and friends keep full ids, like every other machine-readable
// output: an abbreviation a consumer stores can become ambiguous later.
type blockerJSON struct {
	ID     string   `json:"id"`
	Title  string   `json:"title,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Known  bool     `json:"known"`
	Path   []string `json:"path"`
	Claims []string `json:"claims,omitempty"`
	// Never omitempty: zero is the answer to "does anything here wait on this",
	// and an absent field would make an agent choosing what to start guess.
	Gates int `json:"gates"`
}

// BlockersJSON shapes blockers for --json.
func BlockersJSON(blockers []query.Blocker) any {
	out := struct {
		Blockers []blockerJSON `json:"blockers"`
	}{Blockers: make([]blockerJSON, 0, len(blockers))}
	for _, b := range blockers {
		row := blockerJSON{ID: b.ID, Path: b.Path, Known: b.Node != nil, Gates: b.Gates}
		if b.Node != nil {
			row.Title, row.Kind = b.Node.Title, b.Node.Kind
		}
		for _, c := range b.Claims {
			row.Claims = append(row.Claims, c.Holder)
		}
		out.Blockers = append(out.Blockers, row)
	}
	return out
}

// DependentsJSON shapes the reverse closure for --json. via names the kinds the
// walk crossed, which an empty list otherwise leaves ambiguous: no dependents at
// all reads the same as none through the kinds that were actually walked.
func DependentsJSON(deps []query.Dependent, via []string) any {
	type row struct {
		ID    string `json:"id"`
		Title string `json:"title,omitempty"`
		Kind  string `json:"kind,omitempty"`
		Depth int    `json:"depth"`
		// Every walked kind linking this row to parent, and the parent it was
		// reached from (the queried node at depth 1). Per row, not per
		// invocation: a widened walk mixes kinds, and without these a consumer
		// reads a part-of child as a dependent. A list, never one
		// kind: when two kinds both connect the pair, one alone would be
		// asserted as *the* relation. Emitted on every walk, pure ones
		// included, and never omitted — a consumer should not have to know
		// which flags produced the output.
		Via    []string `json:"via"`
		Parent string   `json:"parent"`
	}
	out := struct {
		Dependents []row    `json:"dependents"`
		Via        []string `json:"via"`
	}{Dependents: make([]row, 0, len(deps)), Via: via}
	for _, d := range deps {
		r := row{ID: d.ID, Depth: d.Depth, Via: d.Vias, Parent: d.Parent}
		if d.Node != nil {
			r.Title, r.Kind = d.Node.Title, d.Node.Kind
		}
		out.Dependents = append(out.Dependents, r)
	}
	return out
}

// HopsJSON shapes a route for --json. via names the edge kinds the walk was
// allowed to cross and dir which way it could cross them, neither of which the
// route alone reveals: a caller cannot tell an absent route from one excluded by
// the filter without them, and under the default dir a route says the two nodes
// are connected rather than that one reaches the other.
func HopsJSON(hops []query.Hop, via []string, dir string) any {
	type row struct {
		ID       string `json:"id"`
		Title    string `json:"title,omitempty"`
		Kind     string `json:"kind"`
		Outbound bool   `json:"outbound"`
	}
	out := struct {
		Hops []row    `json:"hops"`
		Via  []string `json:"via"`
		Dir  string   `json:"dir"`
	}{Hops: make([]row, 0, len(hops)), Via: via, Dir: dir}
	for _, h := range hops {
		r := row{ID: h.ID, Kind: h.Edge.Kind, Outbound: h.Outbound}
		if h.Node != nil {
			r.Title = h.Node.Title
		}
		out.Hops = append(out.Hops, r)
	}
	return out
}
