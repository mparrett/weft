package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

var validEdgeKinds = []string{
	graph.EdgeDependsOn, graph.EdgeConflictsWith, graph.EdgePartOf,
	graph.EdgeSupersedes, graph.EdgeDerivedFrom, graph.EdgeSameAs,
}

func cmdLink(e *env, args []string) int {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	source := fs.String("source", event.SourceAsserted, "observed, asserted, or inferred")
	ifAbsent := fs.Bool("if-absent", false, "append nothing if this edge already exists (a writer that re-runs, not a second assertion)")
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 3 {
		fmt.Fprintf(e.stderr, "weft link: want <from> <kind> <to>, e.g. weft link a3f depends-on 9b0\n"+
			"kinds: %s\n", strings.Join(validEdgeKinds, ", "))
		return exitUsage
	}
	fromQuery, kind, toQuery := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	if !contains(validEdgeKinds, kind) {
		fmt.Fprintf(e.stderr, "weft link: unknown edge kind %q (want one of: %s)\n",
			kind, strings.Join(validEdgeKinds, ", "))
		return exitUsage
	}
	if !contains([]string{event.SourceObserved, event.SourceAsserted, event.SourceInferred}, *source) {
		fmt.Fprintf(e.stderr, "weft link: unknown source %q\n", *source)
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	dir, _ := store.Dir()
	by := actor.Resolve(dir)

	// Resolution and validation both happen inside the transaction, against the
	// same fold that the append will land on. Resolving outside would reintroduce
	// exactly the race the transaction exists to close: two processes could each
	// approve opposite edges and then both commit (EVENTS.md §6).
	var from, to *graph.Node
	var existing *graph.Edge
	var resolveErr error
	err = s.Mutate(func(fold func(func(event.Event) error) error) ([]event.Event, error) {
		g, err := graph.Fold(fold)
		if err != nil {
			return nil, err
		}
		if from, err = g.Resolve(fromQuery); err != nil {
			resolveErr = err
			return nil, err
		}
		if to, err = g.Resolve(toQuery); err != nil {
			resolveErr = err
			return nil, err
		}
		// The retry-safe form. Without it a writer that re-runs — a scheduled
		// import, an agent resuming after a crash, any derivation that
		// recomputes rather than remembers — appends an edge.add the fold then
		// discards, so every query keeps returning the right answer while the
		// log grows on every run (EVENTS.md §3.9; a scheduled
		// writer leaked the log this way).
		//
		// The lookup goes through FindEdge rather than a scan, because that is
		// the key the fold dedupes on, including the endpoint-order
		// normalization symmetric kinds rely on. It runs *after* Resolve and
		// inside the transaction so it sees the same fold the append lands on;
		// checking outside would reopen the race the transaction closes
		// (EVENTS.md §6).
		if *ifAbsent {
			if found := g.FindEdge(kind, from.ID, to.ID); found != nil {
				existing = found
				return nil, nil
			}
		}
		if err := g.CheckEdge(kind, from.ID, to.ID); err != nil {
			return nil, err
		}
		edge, err := event.EdgeAdd(kind, from.ID, to.ID, by, *source, time.Now())
		if err != nil {
			return nil, err
		}
		return []event.Event{edge}, nil
	})

	if err != nil {
		if resolveErr != nil {
			return resolveOnStore(e, s, *asJSON, resolveErr)
		}
		return cycleFailure(e, s, *asJSON, err, "this edge")
	}

	// Report the edge as the graph holds it, not as the request phrased it. The
	// two differ only for a symmetric kind asserted the other way round, and
	// there the stored orientation is the useful answer: it says what is
	// already there, which is what --if-absent was asking.
	first, second := from, to
	if existing != nil && existing.From == to.ID && existing.To == from.ID {
		first, second = to, from
	}

	if *asJSON {
		if err := render.NodesJSON(e.stdout, []*graph.Node{first, second}); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), false, err)
	}
	width := render.Width(g.IDs())
	fmt.Fprintf(e.stdout, "%s %s %s\n",
		render.Short(first.ID, width), kind, render.Short(second.ID, width))
	return exitOK
}

// cycleFailure renders a mutation rejected for closing a cycle. The offending
// path prints with titles: naming the constraint without showing what violates
// it leaves the caller unable to act (SPEC.md §6.1). subject says which
// mutation was refused, since link and merge share this.
func cycleFailure(e *env, s *store.Store, asJSON bool, err error, subject string) int {
	var cycle *graph.ErrCycle
	if !errors.As(err, &cycle) {
		return fail(e, storeExitCode(err), asJSON, err)
	}

	g, foldErr := graph.Fold(s.Fold)
	if foldErr != nil {
		return fail(e, storeExitCode(foldErr), asJSON, foldErr)
	}
	if asJSON {
		_ = render.Error(e.stdout, exitRejected, err.Error(), cycle.Path)
		return exitRejected
	}
	fmt.Fprintf(e.stderr, "weft: %s would create a %s cycle:\n\n", subject, cycle.Kind)
	_ = render.Path(e.stderr, cycle.Path, g, render.Width(g.IDs()))
	return exitRejected
}
