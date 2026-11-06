package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
	"regexp"
	"sort"
	"strings"
)

// cmdExport writes the whole graph in one fold.
//
// Rendering a graph from `ls` plus one `show` per node is not merely slower —
// it is N folds at N points in time, so a write landing mid-loop yields a
// picture that never existed. One fold is one consistent snapshot, and the cost
// stops growing with the node count (BUILD.md §1.4).
//
// It is deliberately whole-store, with no namespace filter. An edge may cross
// namespaces (SPEC.md §5), so a scoped export would emit edges pointing at
// nodes it had excluded — reproducing exactly the filter-versus-traversal
// failure the namespace rules exist to prevent. Scoping this needs a
// neighbour-closure design, not a filter.
func cmdExport(e *env, args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	asJSON := fs.Bool("json", false, "emit the graph as JSON instead of DOT")
	asD2 := fs.Bool("d2", false, "emit the graph as d2 instead of DOT, part-of drawn as containers")
	flat := fs.Bool("flat", false, "with --d2: draw part-of as arrows instead of containers")
	includeSpec := fs.String("include", "", "extra JSON sections, comma-separated: "+
		strings.Join(includeSections, ", ")+", or all")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "weft export: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	in, err := parseInclude(*includeSpec)
	if err != nil {
		fmt.Fprintf(e.stderr, "weft export: %v\n", err)
		return exitUsage
	}
	// DOT is a layout format for graphviz; provenance has nowhere to go in it,
	// and silently accepting the flag would report a richer export than was
	// written.
	if !*asJSON && *includeSpec != "" {
		fmt.Fprintln(e.stderr, "weft export: --include applies to --json only")
		return exitUsage
	}
	if *asJSON && *asD2 {
		fmt.Fprintln(e.stderr, "weft export: --json and --d2 are different formats; pick one")
		return exitUsage
	}
	if *flat && !*asD2 {
		fmt.Fprintln(e.stderr, "weft export: --flat applies to --d2 only")
		return exitUsage
	}

	st, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	g, err := graph.Fold(st.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	nodes := g.Ordered()
	edges := sortedEdges(g, nodes)

	if *asJSON {
		return renderOr(e, exportJSON(e, g, nodes, edges, in))
	}
	if *asD2 {
		return renderOr(e, exportD2(e, nodes, edges, render.Width(g.IDs()), !*flat))
	}
	return renderOr(e, exportDOT(e, nodes, edges, render.Width(g.IDs())))
}

// sortedEdges returns every edge exactly once, in an order derived from the
// node order rather than from map iteration.
//
// g.Edges is a map, and OutEdges(id, "") reports symmetric edges from both
// ends, so neither is usable directly: one is non-deterministic and the other
// double-counts. Ordering by the endpoints' position in the canonical node
// order keeps identical graphs printing identically (SPEC.md §9.5).
func sortedEdges(g *graph.Graph, nodes []*graph.Node) []*graph.Edge {
	pos := make(map[string]int, len(nodes))
	for i, n := range nodes {
		pos[n.ID] = i
	}
	// A dangling endpoint sorts last rather than at position zero, so unknown
	// nodes do not shuffle the known ones (SPEC.md §9.3).
	rank := func(id string) int {
		if i, ok := pos[id]; ok {
			return i
		}
		return len(nodes)
	}

	out := make([]*graph.Edge, 0, len(g.Edges))
	for _, ed := range g.Edges {
		out = append(out, ed)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := rank(a.From), rank(b.From); ra != rb {
			return ra < rb
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if ra, rb := rank(a.To), rank(b.To); ra != rb {
			return ra < rb
		}
		return a.ID < b.ID // total order, so ties never depend on map iteration
	})
	return out
}

// Timestamps ride along so a consumer can render recency — the viewer's whole
// reason to care about time. Absolute RFC3339, never a relative age: the export
// is a file that outlives the moment it was written.
//
// DOT gets no equivalent. It is a layout format read by graphviz, and a date on
// every node is noise in a picture whose job is structure.
//
// Refs is `any` so one field can hold either shape: bare strings by default,
// objects under --include refs. A second field would have made every consumer
// check two places and would put the default's own name in the opt-in output.
type exportNodeJSON struct {
	ID      string            `json:"id"`
	NS      string            `json:"ns"`
	Kind    string            `json:"kind"`
	Title   string            `json:"title"`
	State   string            `json:"state"`
	By      string            `json:"by,omitempty"`
	Source  string            `json:"source,omitempty"`
	Refs    []any             `json:"refs,omitempty"`
	Created string            `json:"created,omitempty"`
	Touched string            `json:"touched,omitempty"`
	Claims  []exportClaimJSON `json:"claims,omitempty"`
	// ReleasedClaims is a separate array, not released entries mixed into
	// Claims: every reader written before it existed treats a `claims` entry
	// as current ownership, and would show a released holder as still on the
	// work.
	ReleasedClaims []exportClaimJSON `json:"released_claims,omitempty"`
}

// exportRefJSON is a ref together with what upstream last said about it and
// when. observed_at is what lets a reader judge the rest: an observation shown
// as though it were current invites a decision on data nobody re-checked
// (EVENTS.md §2.5, SPEC.md §6.3).
// upstream_created and upstream_modified are the artifact's own clock — what a
// layout or timeline consumer orders by; observed_at says how much to trust
// them, never where to place the node (SPEC.md §6.4).
type exportRefJSON struct {
	Ref              string `json:"ref"`
	ObservedAt       string `json:"observed_at,omitempty"`
	UpstreamState    string `json:"upstream_state,omitempty"`
	UpstreamTitle    string `json:"upstream_title,omitempty"`
	UpstreamCreated  string `json:"upstream_created,omitempty"`
	UpstreamModified string `json:"upstream_modified,omitempty"`
	UpstreamHash     string `json:"upstream_hash,omitempty"`
}

// exportClaimJSON is one unreleased claim. Expired ones are included and dated
// rather than filtered: an expired claim is what abandoned work looks like, and
// hiding it makes abandoned indistinguishable from unclaimed (SPEC.md §4.3).
//
// `by` and `holder` are separate because they routinely differ — an agent
// recording that a colleague seems to be on something is the motivating case,
// and it is exactly the claim a reader must be able to discount.
type exportClaimJSON struct {
	Holder  string `json:"holder"`
	By      string `json:"by,omitempty"`
	Source  string `json:"source,omitempty"`
	Note    string `json:"note,omitempty"`
	At      string `json:"at,omitempty"`
	Expires string `json:"expires,omitempty"`
	// Set only on released_claims entries. `at` stays the time the claim was
	// taken, so [at, released_at) is the interval the holder had the work.
	ReleasedAt     string `json:"released_at,omitempty"`
	ReleasedBy     string `json:"released_by,omitempty"`
	ReleasedSource string `json:"released_source,omitempty"`
}

// claimJSON is the one mapping both arrays use, so claims and released_claims
// entries cannot drift apart in shape. A live claim's Released* fields are
// zero and drop out through omitempty.
func claimJSON(c *graph.Claim) exportClaimJSON {
	return exportClaimJSON{
		Holder: c.Holder, By: c.By, Source: c.Source, Note: c.Note,
		At: render.Stamp(c.At), Expires: render.Stamp(c.Expires),
		ReleasedAt: render.Stamp(c.ReleasedAt), ReleasedBy: c.ReleasedBy,
		ReleasedSource: c.ReleasedSource,
	}
}

type exportEdgeJSON struct {
	Kind   string `json:"kind"`
	From   string `json:"from"`
	To     string `json:"to"`
	By     string `json:"by,omitempty"`
	Source string `json:"source,omitempty"`
	At     string `json:"at,omitempty"`
}

// include is which optional sections the export carries. The zero value is the
// default shape, which stays byte-identical — golden files rest on it, and
// canonical printing is one of the two constraints recorded as rotting
// silently (SPEC.md §9.5). Changing the default would mean regenerating those
// goldens, which is the move that makes the next real drift invisible.
type include struct {
	claims bool
	source bool
	refs   bool
}

var includeSections = []string{"claims", "source", "refs"}

// includeAll is every section. Shared with `weft view`, whose page promises the
// full export: a section added here reaches both, or neither.
var includeAll = include{claims: true, source: true, refs: true}

func parseInclude(spec string) (include, error) {
	var in include
	if strings.TrimSpace(spec) == "" {
		return in, nil
	}
	for _, name := range strings.Split(spec, ",") {
		switch strings.TrimSpace(name) {
		case "claims":
			in.claims = true
		case "source":
			in.source = true
		case "refs":
			in.refs = true
		case "all":
			in = includeAll
		default:
			return include{}, fmt.Errorf("unknown --include section %q (want one of: %s, or all)",
				name, strings.Join(includeSections, ", "))
		}
	}
	return in, nil
}

func exportJSON(e *env, g *graph.Graph, nodes []*graph.Node, edges []*graph.Edge, in include) error {
	out := struct {
		Nodes []exportNodeJSON `json:"nodes"`
		Edges []exportEdgeJSON `json:"edges"`
	}{
		Nodes: make([]exportNodeJSON, 0, len(nodes)),
		Edges: make([]exportEdgeJSON, 0, len(edges)),
	}
	for _, n := range nodes {
		refs := append([]string(nil), n.Refs...)
		sort.Strings(refs)
		node := exportNodeJSON{
			ID: n.ID, NS: n.NS, Kind: n.Kind, Title: n.Title, State: n.State,
			Created: render.Stamp(n.Created), Touched: render.Stamp(n.Touched),
		}
		if in.source {
			node.By, node.Source = n.By, n.Source
		}
		for _, r := range refs {
			if !in.refs {
				node.Refs = append(node.Refs, r)
				continue
			}
			obj := exportRefJSON{Ref: r}
			// A ref nobody has ever looked at carries no observation, and says
			// so by omission rather than by an empty state that would read as
			// "upstream reported nothing".
			if o := g.Observed(n.ID, r); o != nil {
				obj.ObservedAt = render.Stamp(o.At)
				obj.UpstreamState = o.State
				obj.UpstreamTitle = o.Title
				obj.UpstreamCreated = o.Created
				obj.UpstreamModified = o.Modified
				obj.UpstreamHash = o.Hash
			}
			node.Refs = append(node.Refs, obj)
		}
		if in.claims {
			for _, c := range g.ClaimsFor(n.ID) {
				node.Claims = append(node.Claims, claimJSON(c))
			}
			for _, c := range g.ReleasedClaimsFor(n.ID) {
				node.ReleasedClaims = append(node.ReleasedClaims, claimJSON(c))
			}
		}
		out.Nodes = append(out.Nodes, node)
	}
	for _, ed := range edges {
		edge := exportEdgeJSON{Kind: ed.Kind, From: ed.From, To: ed.To}
		if in.source {
			edge.By, edge.Source, edge.At = ed.By, ed.Source, render.Stamp(ed.At)
		}
		out.Edges = append(out.Edges, edge)
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// dotQuote escapes a label for DOT. Titles are free text and routinely contain
// quotes and backslashes; an unescaped one produces a file graphviz rejects,
// which reads as "weft is broken" rather than "that title had a quote in it".
func dotQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// refURL turns a ref into a browsable URL, or "" when it cannot.
//
// Only `gh:owner/repo#12` and `pr:owner/repo#45` carry enough to build one. A
// ref is free text and any scheme is accepted (EVENTS.md), so this matches
// exactly and gives up otherwise: `branch:` and `file:` name no host, and a
// guessed link that 404s is worse than no link, because it looks authoritative.
var refURLPattern = regexp.MustCompile(`^(gh|pr):([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)#([0-9]+)$`)

func refURL(ref string) string {
	m := refURLPattern.FindStringSubmatch(ref)
	if m == nil {
		return ""
	}
	// GitHub redirects /issues/N to /pull/N when N turns out to be a PR, so
	// `gh:` is correct for both without a lookup to disambiguate.
	path := "issues"
	if m[1] == "pr" {
		path = "pull"
	}
	return "https://github.com/" + m[2] + "/" + path + "/" + m[3]
}

// nodeURL returns the first linkable ref in sorted order. A node may carry
// several, and ref order comes from the log, so sorting is what makes the
// choice stable across stores that acquired the same refs in a different
// sequence (SPEC.md §9.5).
func nodeURL(n *graph.Node) string {
	refs := append([]string(nil), n.Refs...)
	sort.Strings(refs)
	for _, r := range refs {
		if u := refURL(r); u != "" {
			return u
		}
	}
	return ""
}

func exportDOT(e *env, nodes []*graph.Node, edges []*graph.Edge, width int) error {
	var b strings.Builder
	b.WriteString("digraph weft {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [shape=box, style=\"rounded,filled\", fillcolor=white, fontname=\"Helvetica\"];\n")
	b.WriteString("  edge [fontname=\"Helvetica\", fontsize=10];\n\n")

	for _, n := range nodes {
		// The short id is what a human pastes back into weft, so it leads the
		// label even though the node name is the full id.
		label := fmt.Sprintf("%s\\n%s", render.Short(n.ID, width), dotQuote(n.Title))
		attrs := fmt.Sprintf("label=\"%s\", tooltip=\"%s\"", label, dotQuote(n.Kind+" · "+n.NS))
		// graphviz turns URL into a hyperlink in SVG output, so the rendered
		// graph is navigable rather than only readable.
		if u := nodeURL(n); u != "" {
			attrs += fmt.Sprintf(", URL=%q, target=\"_blank\"", u)
		}
		switch n.State {
		case "done":
			attrs += ", fillcolor=\"#e8f5e9\", color=\"#66bb6a\""
		case "dropped":
			attrs += ", style=\"rounded,dashed\", fontcolor=\"#999999\", color=\"#cccccc\""
		}
		fmt.Fprintf(&b, "  %q [%s];\n", n.ID, attrs)
	}
	b.WriteString("\n")

	for _, ed := range edges {
		style := ""
		switch ed.Kind {
		case "depends-on":
			style = ", color=\"#c62828\""
		case "conflicts-with":
			// Symmetric, so it carries no direction to draw (SPEC.md §4.2).
			style = ", dir=none, style=dashed, color=\"#ef6c00\""
		case "part-of":
			style = ", style=dotted, color=\"#5c6bc0\""
		}
		fmt.Fprintf(&b, "  %q -> %q [label=%q%s];\n", ed.From, ed.To, ed.Kind, style)
	}
	b.WriteString("}\n")

	_, err := e.stdout.Write([]byte(b.String()))
	return err
}

// ancestorOf reports whether a is node or one of node's containers in parent.
// parent is kept acyclic by its only writer, so the walk terminates.
func ancestorOf(parent map[string]string, a, node string) bool {
	for cur, ok := node, true; ok; cur, ok = parent[cur] {
		if cur == a {
			return true
		}
	}
	return false
}

// d2Quote escapes a label for a d2 double-quoted string. `$` matters as much as
// the quote: d2 substitutes `${var}` inside double quotes and rejects a bare
// `$` outright, so a title mentioning a shell variable or a price would make the
// whole file fail to compile.
var d2Escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", "", "\t", " ")

func d2Quote(s string) string {
	return d2Escaper.Replace(s)
}

// exportD2 writes the graph as d2 (https://d2lang.com). It exists for the one
// thing DOT cannot say: part-of is containment (SPEC.md §4.2, the place
// family), and d2 draws containment as boxes inside boxes instead of as one more
// arrow that reads like a dependency.
//
// The node key is the full id, as in DOT. A display prefix would be shorter, but
// a dangling endpoint is not among the ids the prefix width was computed over,
// so its prefix could collide with a real node and merge two shapes.
//
// part-of is meant to be a forest, but only acyclicity is enforced, so a node
// can have two parents. The first in canonical edge order becomes the
// container; any other part-of edge is drawn as an arrow, so nothing in the
// graph goes missing from the picture. --flat (containers=false) draws them all
// as arrows.
func exportD2(e *env, nodes []*graph.Node, edges []*graph.Edge, width int, containers bool) error {
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
	}
	parent := map[string]string{}
	contained := map[*graph.Edge]bool{}
	if containers {
		for _, ed := range edges {
			if ed.Kind != "part-of" || !known[ed.From] || !known[ed.To] {
				continue
			}
			if _, has := parent[ed.From]; has {
				continue
			}
			// The write path rejects part-of cycles, but tolerance over
			// validation (SPEC.md §9.3) means a log may carry one anyway. An
			// edge that would nest a node inside its own descendant stays an
			// arrow, so the containers remain a forest and every node has one
			// consistent path.
			if ancestorOf(parent, ed.From, ed.To) {
				continue
			}
			parent[ed.From] = ed.To
			contained[ed] = true
		}
	}
	// A node's key is its container chain, each segment quoted. Quoting is not
	// cosmetic: the fold never checks that an edge endpoint is a hex id, so a
	// dangling endpoint from a hand-written log can hold `.` (d2 nesting), `#`
	// (a comment) or `"`. Unquoted, it silently draws an edge to the wrong
	// shape or breaks the file — the d2 form of DOT's %q.
	paths := make(map[string]string, len(nodes))
	path := func(id string) string {
		if p, ok := paths[id]; ok {
			return p
		}
		chain := []string{`"` + d2Quote(id) + `"`}
		for p, ok := parent[id]; ok; p, ok = parent[p] {
			chain = append(chain, `"`+d2Quote(p)+`"`)
		}
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		paths[id] = strings.Join(chain, ".")
		return paths[id]
	}

	var b strings.Builder
	// ELK routes a dense graph more cleanly than d2's default dagre. The file
	// says so itself, so a plain `d2 in.d2 out.svg` uses it and --layout still
	// overrides.
	b.WriteString("vars: {\n  d2-config: {\n    layout-engine: elk\n  }\n}\n")
	b.WriteString("direction: right\n\n")

	for _, n := range nodes {
		fmt.Fprintf(&b, "%s: \"%s\\n%s\" {\n", path(n.ID), render.Short(n.ID, width), d2Quote(n.Title))
		fmt.Fprintf(&b, "  tooltip: \"%s\"\n", d2Quote(n.Kind+" · "+n.NS))
		if u := nodeURL(n); u != "" {
			fmt.Fprintf(&b, "  link: \"%s\"\n", d2Quote(u))
		}
		switch n.State {
		case "done":
			b.WriteString("  style.fill: \"#e8f5e9\"\n  style.stroke: \"#66bb6a\"\n")
		case "dropped":
			b.WriteString("  style.stroke-dash: 3\n  style.stroke: \"#cccccc\"\n  style.font-color: \"#999999\"\n")
		}
		b.WriteString("}\n")
	}
	b.WriteString("\n")

	for _, ed := range edges {
		if contained[ed] {
			continue
		}
		arrow, style := "->", ""
		switch ed.Kind {
		case "depends-on":
			style = "style.stroke: \"#c62828\""
		case "conflicts-with":
			// Symmetric, so it carries no direction to draw (SPEC.md §4.2).
			arrow, style = "--", "style.stroke: \"#ef6c00\"; style.stroke-dash: 3"
		case "part-of":
			style = "style.stroke: \"#5c6bc0\"; style.stroke-dash: 2"
		}
		// The kind is escaped too: only `weft link` validates kinds, and the
		// fold tolerates any string another writer put in the log.
		fmt.Fprintf(&b, "%s %s %s: \"%s\"", path(ed.From), arrow, path(ed.To), d2Quote(ed.Kind))
		if style != "" {
			fmt.Fprintf(&b, " {%s}", style)
		}
		b.WriteString("\n")
	}

	_, err := e.stdout.Write([]byte(b.String()))
	return err
}
