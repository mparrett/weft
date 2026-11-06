package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// cmdMerge folds one node into another: refs, edges, and claims union onto the
// survivor, and the merged node stays addressable as an alias of it
// (SPEC.md §6.2).
//
// The survivor's own fields win outright, including state. "Take the more
// resolved state" was rejected: closing a duplicate is the ordinary way people
// discover two nodes are the same, and letting that close the surviving node
// would mark real work done. Nothing is lost either way, because the merged
// node keeps its own ns, kind, title, and state.
func cmdMerge(e *env, args []string) int {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	source := fs.String("source", event.SourceAsserted, "observed, asserted, or inferred")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(e.stderr, "weft merge: want <into> <from> — the first survives, e.g. weft merge a3f 9b0")
		return exitUsage
	}
	if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, *source) {
		fmt.Fprintf(e.stderr, "weft merge: unknown source %q\n", *source)
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)

	// Resolution, the same-node check, and the cycle check all happen inside
	// the transaction, against the fold the append lands on. Merge is the shape
	// this matters most for: two processes merging into each other from
	// opposite directions would otherwise both pass (EVENTS.md §6).
	var into, from *graph.Node
	var resolveErr error
	var alreadyOne bool
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		if into, err = g.Resolve(fs.Arg(0)); err != nil {
			resolveErr = err
			return nil, err
		}
		if from, err = g.Resolve(fs.Arg(1)); err != nil {
			resolveErr = err
			return nil, err
		}
		// Resolve returns survivors, so this covers merging a node into itself
		// and re-merging a pair that is already folded. Neither is a failure:
		// the desired state holds (SPEC.md §9.3).
		if into.ID == from.ID {
			alreadyOne = true
			return nil, nil
		}
		if err := g.CheckMerge(into.ID, from.ID); err != nil {
			return nil, err
		}
		ev, err := event.NodeMerge(into.ID, from.ID, by, *source, time.Now())
		if err != nil {
			return nil, err
		}
		return []event.Event{ev}, nil
	})
	if err != nil {
		if resolveErr != nil {
			return resolveOnStore(e, s, *asJSON, resolveErr)
		}
		return cycleFailure(e, s, *asJSON, err, "this merge")
	}

	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}
	if *asJSON {
		if err := render.NodesJSON(e.stdout, []*graph.Node{g.Nodes[into.ID]}); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	width := render.Width(g.IDs())
	if alreadyOne {
		fmt.Fprintf(e.stdout, "%s already names one node\n", render.Short(into.ID, width))
		return exitOK
	}
	fmt.Fprintf(e.stdout, "%s merged %s\n", render.Short(into.ID, width), render.Short(from.ID, width))
	return exitOK
}
