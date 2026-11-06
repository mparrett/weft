package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
	"time"
)

// cmdShow prints one node and its full neighborhood in a single invocation
// (SPEC.md §6.1, §9.4). Claims join it at milestone 5.
func cmdShow(e *env, args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "weft show: want exactly one node id, prefix, or ref")
		return exitUsage
	}

	s, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	g, err := graph.Fold(s.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	node, err := g.Resolve(fs.Arg(0))
	if err != nil {
		return resolveFailure(e, g, *asJSON, err)
	}

	now := time.Now()
	if *asJSON {
		out := render.NeighborhoodJSONOf(node, g.ObservationsFor(node.ID), g.ClaimsFor(node.ID),
			g.MergedFrom(node.ID), g.Neighbors(node.ID), now)
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fail(e, exitStore, false, err)
		}
		return exitOK
	}
	width := render.Width(g.IDs())
	if err := render.NodeObserved(e.stdout, node, width, g.ObservationsFor(node.ID), now); err != nil {
		return fail(e, exitStore, false, err)
	}
	if err := render.MergedFrom(e.stdout, g.MergedFrom(node.ID), width); err != nil {
		return fail(e, exitStore, false, err)
	}
	if err := render.Claims(e.stdout, g.ClaimsFor(node.ID), now); err != nil {
		return fail(e, exitStore, false, err)
	}
	if err := render.Neighborhood(e.stdout, g.Neighbors(node.ID), width, node.NS); err != nil {
		return fail(e, exitStore, false, err)
	}
	return exitOK
}

// resolveFailure maps a resolution error onto the exit contract. The two codes
// are distinct because they call for different responses: 2 means the thing
// does not exist, 4 means say more (BUILD.md §5).
func resolveFailure(e *env, g *graph.Graph, asJSON bool, err error) int {
	var ambiguous *graph.ErrAmbiguous
	if errors.As(err, &ambiguous) {
		width := render.Width(g.IDs())
		if asJSON {
			ids := make([]string, len(ambiguous.Candidates))
			for i, n := range ambiguous.Candidates {
				ids[i] = n.ID
			}
			_ = render.Error(e.stdout, exitAmbiguous, err.Error(), ids)
			return exitAmbiguous
		}
		fmt.Fprintf(e.stderr, "weft: %v\n\n", err)
		// Every candidate, with enough id to disambiguate and enough title to
		// choose. A bare count would leave the user no better off.
		_ = render.NodeList(e.stderr, ambiguous.Candidates, width, false)
		return exitAmbiguous
	}

	var notFound *graph.ErrNotFound
	if errors.As(err, &notFound) {
		return fail(e, exitNotFound, asJSON, err)
	}
	return fail(e, exitStore, asJSON, err)
}
