package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/mparrett/weft/internal/ns"
	"github.com/mparrett/weft/internal/store"
	"os"
)

// cmdNS answers "which namespace am I in, and why". Before it existed the only
// way to find out was to write a node and look at where it landed — a question
// that mutates the store is one people stop asking, and namespace mistakes are
// silent: work files itself under the wrong project and nothing complains.
//
// The rule that chose the namespace matters as much as the name. "lanternfish"
// alone does not tell you whether a config mapping took effect or whether
// $WEFT_NS is set in this shell and following you between repos.
func cmdNS(e *env, args []string) int {
	fs := flag.NewFlagSet("ns", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "weft ns: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	cwd, err := os.Getwd()
	if err != nil {
		// Same reasoning as currentNS: an unreadable cwd still has an answer.
		cwd = "."
	}
	dir, _ := store.Dir()
	name, src, detail := ns.ResolveFrom(cwd, dir)

	if *asJSON {
		out := struct {
			NS     string `json:"ns"`
			Source string `json:"source"`
			From   string `json:"from,omitempty"`
		}{name, string(src), detail}
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fail(e, exitStore, true, err)
		}
		return exitOK
	}

	if detail != "" {
		fmt.Fprintf(e.stdout, "%s\t(%s: %s)\n", name, src, detail)
	} else {
		fmt.Fprintf(e.stdout, "%s\t(%s)\n", name, src)
	}
	return exitOK
}
