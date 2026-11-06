package main

import (
	_ "embed"
	"fmt"
	"io"
)

// agentDoc is the onboarding snippet (BUILD.md §3, milestone 10).
//
// Embedded rather than left as a file in the repo because the store is global
// and agents mostly run somewhere else entirely. `weft help agent` is how the
// snippet reaches a project's CLAUDE.md; the snippet itself is what has to be
// sufficient, not this command.
//
//go:embed agent.md
var agentDoc string

func printAgentDoc(w io.Writer) int {
	fmt.Fprint(w, agentDoc)
	return exitOK
}
