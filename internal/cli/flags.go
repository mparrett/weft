// Package cli holds argument handling shared by every subcommand.
package cli

import (
	"flag"
	"strings"
)

// Permute reorders args so every flag precedes every positional.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `weft add "title" --kind bug` would silently ignore --kind and succeed while
// doing the wrong thing. Every documented example puts flags after the
// positional (SPEC.md §6), and the one-line-write constraint (SPEC.md §9.1)
// depends on that being natural to type, so the parser adapts rather than the
// documentation (BUILD.md §1.2).
//
// Knowing whether a flag consumes the next argument requires the FlagSet:
// --kind takes a value, --json does not.
func Permute(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	separated := false

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after "--" is positional by convention, including things
		// that look like flags.
		if arg == "--" {
			// The separator is re-emitted below rather than consumed here.
			// Dropping it hands FlagSet.Parse a protected positional with nothing
			// left to protect it, so `weft add -- "--json is broken"` came back as
			// "flag provided but not defined" — no way at all to name a node
			// whose title starts with a dash, which is the one thing the
			// separator exists to make possible.
			separated = true
			positional = append(positional, args[i+1:]...)
			break
		}

		// A bare "-" is stdin by convention, not a flag.
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		name := arg[1:]
		if name[0] == '-' {
			name = name[1:]
		}

		// --flag=value is self-contained.
		if strings.IndexByte(name, '=') >= 0 {
			flags = append(flags, arg)
			continue
		}

		flags = append(flags, arg)
		// An unknown flag is left for FlagSet.Parse to reject, so error
		// reporting stays in one place. Assume it takes no value, which keeps a
		// following positional from being swallowed into a bad diagnostic.
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	if separated {
		flags = append(flags, "--")
	}
	return append(flags, positional...)
}

// isBoolFlag reports whether a flag is satisfied by its presence alone. The
// stdlib signals this through an unexported interface that Value may implement.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
