package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/query"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// loaded is the common preamble: open the store, fold, resolve the arguments.
type loaded struct {
	store *store.Store
	graph *graph.Graph
	width int
	me    string
	now   time.Time
}

func load(e *env, asJSON bool) (*loaded, int) {
	s, err := store.Open()
	if err != nil {
		return nil, fail(e, exitStore, asJSON, err)
	}
	g, err := graph.Fold(s.Fold)
	if err != nil {
		return nil, fail(e, storeExitCode(err), asJSON, err)
	}
	dir, _ := store.Dir()
	return &loaded{
		store: s, graph: g,
		width: render.Width(g.IDs()),
		me:    actor.Resolve(dir),
		now:   time.Now(),
	}, exitOK
}

func cmdWhy(e *env, args []string) int {
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "weft why: want exactly one node id, prefix, or ref")
		return exitUsage
	}

	l, code := load(e, *asJSON)
	if l == nil {
		return code
	}
	node, err := l.graph.Resolve(fs.Arg(0))
	if err != nil {
		return resolveFailure(e, l.graph, *asJSON, err)
	}

	blockers := query.Rank(l.graph, query.Why(l.graph, node.ID, l.now))
	if *asJSON {
		return renderJSON(e, render.BlockersJSON(blockers))
	}
	if len(blockers) == 0 {
		// Optimistic by definition: an incomplete graph can only say that
		// nothing recorded stands in the way (SPEC.md §6.1).
		fmt.Fprintf(e.stdout, "%s  %s\nno known blocker\n",
			render.Short(node.ID, l.width), node.Title)
		return exitOK
	}
	fmt.Fprintf(e.stdout, "%s  %s\n", render.Short(node.ID, l.width), node.Title)
	if err := render.Blockers(e.stdout, blockers, l.graph, l.width, node.NS); err != nil {
		return fail(e, exitStore, false, err)
	}
	return exitOK
}

func cmdRdeps(e *env, args []string) int {
	fs := flag.NewFlagSet("rdeps", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	depth := fs.Int("depth", 0, "how many levels to walk (0 = unlimited)")
	via := fs.String("via", "", "walk these edge kinds instead of depends-on, comma-separated")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "weft rdeps: want exactly one node id, prefix, or ref")
		return exitUsage
	}
	kinds, ok := parseVia(e, "rdeps", *via)
	if !ok {
		return exitUsage
	}

	l, code := load(e, *asJSON)
	if l == nil {
		return code
	}
	node, err := l.graph.Resolve(fs.Arg(0))
	if err != nil {
		return resolveFailure(e, l.graph, *asJSON, err)
	}

	// Which kinds the walk crossed. rdeps is named for dependencies and defaults
	// to them, but on a real map most edges are part-of and derived-from, so an
	// empty answer reads as "nothing depends on this" when it means "nothing
	// depends on this *through depends-on*". Saying so is the same move `path`
	// already makes, and for the same reason: a query that names its rule turns
	// a surprising result from a puzzle into a diagnosis.
	considered := kinds
	if len(considered) == 0 {
		considered = []string{graph.EdgeDependsOn}
	}

	deps := query.RDeps(l.graph, node.ID, *depth, kinds)

	// A pure depends-on walk lists dependents; anything wider lists what is
	// reachable, and calling that "dependent" asserts a wait the edges do not
	// carry, so each row then names the relations it carries.
	// Keyed on what the walk actually *crossed*, never on what --via asked
	// for: a wide --via over a store whose edges are all depends-on still
	// yields the pure closure, and labelling every row of it with the
	// command's own name is the noise the pure walk deliberately avoids. An
	// empty answer crossed nothing, so it always reads "no known dependent" —
	// the via: line is what says which kinds were open to it.
	widened := false
	for _, d := range deps {
		for _, k := range d.Vias {
			if k != graph.EdgeDependsOn {
				widened = true
			}
		}
	}

	if *asJSON {
		return renderJSON(e, render.DependentsJSON(deps, considered))
	}
	if len(deps) == 0 {
		fmt.Fprintln(e.stdout, "no known dependent")
	} else if code := renderOr(e, render.Dependents(e.stdout, deps, l.width, node.NS, widened)); code != exitOK {
		return code
	}
	fmt.Fprintf(e.stdout, "via: %s\n", rdepsViaLine(kinds, considered))
	return exitOK
}

// rdepsViaLine mirrors path's, with the polarity reversed: path defaults to every
// kind and --via narrows, rdeps defaults to one and --via widens.
func rdepsViaLine(requested, considered []string) string {
	s := strings.Join(considered, ", ")
	if len(requested) == 0 {
		return s + " (the default; --via walks other kinds)"
	}
	return s
}

func cmdPath(e *env, args []string) int {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	via := fs.String("via", "", "restrict to these edge kinds, comma-separated")
	dirArg := fs.String("dir", "either", "which way to cross edges: either, out (does a lead to b), in (does b lead to a)")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(e.stderr, "weft path: want two node ids, prefixes, or refs")
		return exitUsage
	}
	dir, ok := directions[*dirArg]
	if !ok {
		fmt.Fprintf(e.stderr, "weft path: unknown --dir %q (want one of: either, out, in)\n", *dirArg)
		return exitUsage
	}
	// A single kind is rarely the useful restriction. What a reader usually
	// wants is the *directed* family — the kinds that mean "this leads to that"
	// — and excluding one symmetric kind at a time cannot express it.
	kinds, ok := parseVia(e, "path", *via)
	if !ok {
		return exitUsage
	}

	l, code := load(e, *asJSON)
	if l == nil {
		return code
	}
	from, err := l.graph.Resolve(fs.Arg(0))
	if err != nil {
		return resolveFailure(e, l.graph, *asJSON, err)
	}
	to, err := l.graph.Resolve(fs.Arg(1))
	if err != nil {
		return resolveFailure(e, l.graph, *asJSON, err)
	}

	// What the walk was allowed to cross, which is not visible from the route
	// itself. A hub node with many part-of children puts everything within two
	// hops of everything else, and a symmetric kind is a free bidirectional
	// shortcut between nodes that exist to say they are alternatives — so a
	// surprising route is usually explained by the permitted set rather than by
	// the graph. `weft ns` names the rule and not just the answer for the same
	// reason, and that made a namespace trap a five-second diagnosis.
	considered := kinds
	if len(considered) == 0 {
		considered = validEdgeKinds
	}

	hops := query.Path(l.graph, from.ID, to.ID, kinds, dir)
	if *asJSON {
		// nil hops and an empty route are the same answer here — no route is an
		// answer, not a failure (SPEC.md §9.3) — so both carry the kinds walked
		// and the direction they were walked in.
		return renderJSON(e, render.HopsJSON(hops, considered, *dirArg))
	}
	if hops == nil {
		fmt.Fprintf(e.stdout, "no known route from %s to %s\n",
			render.Short(from.ID, l.width), render.Short(to.ID, l.width))
	} else if code := renderOr(e, render.Route(e.stdout, from, hops, l.width)); code != exitOK {
		return code
	}
	fmt.Fprintf(e.stdout, "via: %s\n", viaLine(kinds, considered, dir))
	return exitOK
}

// parseVia reads a comma-separated --via into a deduplicated kind list. Shared,
// so `path` and `rdeps` cannot come to disagree about what a valid kind is or
// how a typo is reported — they mean opposite things by the flag (path narrows
// from every kind, rdeps widens from one) but that is the caller's business, not
// the parser's.
//
// An empty result from a non-empty flag is a typo rather than a request to walk
// the default: silently falling back is how `--via ,,` would widen a walk the
// reader meant to restrict.
func parseVia(e *env, cmd, via string) ([]string, bool) {
	if via == "" {
		return nil, true
	}
	var kinds []string
	for _, k := range strings.Split(via, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !contains(validEdgeKinds, k) {
			fmt.Fprintf(e.stderr, "weft %s: unknown edge kind %q (want one of: %s)\n",
				cmd, k, strings.Join(validEdgeKinds, ", "))
			return nil, false
		}
		if !contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		fmt.Fprintf(e.stderr, "weft %s: --via named no edge kinds\n", cmd)
		return nil, false
	}
	return kinds, true
}

// directions maps the --dir surface onto the walk. "out" and "in" rather than
// forwards and backwards because the flag is read next to two positional
// arguments: out is "does a lead to b".
var directions = map[string]query.Direction{
	"either": query.Either,
	"out":    query.Forward,
	"in":     query.Backward,
}

// viaLine states which kinds the walk could cross, which way it could cross
// them, and where those choices came from — so an unrestricted walk reads as a
// choice rather than as the only behaviour.
//
// Direction belongs here rather than nowhere because `--via depends-on` invites
// a directional question and used to answer a connectivity one; the route alone
// cannot tell the two apart, since both a forward and a reversed hop print a
// label containing "depends on".
func viaLine(requested, considered []string, dir query.Direction) string {
	var notes []string
	if len(requested) == 0 {
		notes = append(notes, "all kinds; --via restricts")
	}
	switch dir {
	case query.Forward:
		notes = append(notes, "forwards only")
	case query.Backward:
		notes = append(notes, "backwards only")
	default:
		notes = append(notes, "either direction; --dir restricts")
	}
	// A directional walk still crosses symmetric kinds from both sides, so say
	// which ones — otherwise "forwards only" overstates what was excluded.
	if dir != query.Either {
		var sym []string
		for _, k := range considered {
			if graph.Symmetric(k) {
				sym = append(sym, k)
			}
		}
		if len(sym) > 0 {
			verb := " is symmetric, crossed either way"
			if len(sym) > 1 {
				verb = " are symmetric, crossed either way"
			}
			notes = append(notes, strings.Join(sym, " and ")+verb)
		}
	}
	return strings.Join(considered, ", ") + " (" + strings.Join(notes, "; ") + ")"
}

// renderJSON writes any value as indented JSON on stdout.
func renderJSON(e *env, v any) int {
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(e, exitStore, false, err)
	}
	return exitOK
}

func renderOr(e *env, err error) int {
	if err != nil {
		return fail(e, exitStore, false, err)
	}
	return exitOK
}
