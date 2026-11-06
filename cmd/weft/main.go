// Command weft is the CLI for the weft claims graph.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"

	"github.com/mparrett/weft/internal/cli"
	"github.com/mparrett/weft/internal/event"
	"github.com/mparrett/weft/internal/ns"
	"io"
	"os"
	"sort"
)

// Exit codes are a stable part of the CLI contract — agents branch on them, and
// smoke.sh depends on exitNotImplemented specifically. See BUILD.md §5.
const (
	exitOK             = 0
	exitUsage          = 1
	exitNotFound       = 2
	exitNotImplemented = 3
	exitAmbiguous      = 4
	exitRejected       = 5
	exitStore          = 6
	// exitExternal is a shell-out failure, not a weft failure: gh missing, not
	// authenticated, or the API refusing us. Folded into 6 it would read as a
	// corrupt log, which sends you to entirely the wrong place.
	exitExternal = 7
)

// version is overridden at build time via -ldflags.
var version = "0.0.0-dev"

type command struct {
	summary string
	run     func(*env, []string) int
}

// env carries everything a command needs, so commands touch no globals and
// tests can drive them without a subprocess.
type env struct {
	stdout io.Writer
	stderr io.Writer
}

var commands map[string]command

func init() {
	// Populated in init to avoid an initialization cycle: usage ranges over
	// commands, and help is itself a command.
	commands = map[string]command{
		"add":     {"register a node", cmdAdd},
		"ls":      {"list nodes", cmdLs},
		"ns":      {"print the namespace this directory resolves to", cmdNS},
		"env":     {"print the store, actor and namespace in use, and why", cmdEnv},
		"export":  {"write the whole graph as DOT or JSON", cmdExport},
		"view":    {"write the graph as an interactive HTML page", cmdView},
		"link":    {"connect two nodes with a typed edge", cmdLink},
		"unlink":  {"retract an edge between two nodes", cmdUnlink},
		"ref":     {"bind an external object to an existing node, or unbind it", cmdRef},
		"claim":   {"record that someone is working on a node", cmdClaim},
		"release": {"drop a claim", cmdRelease},
		"show":    {"print a node's full neighborhood", cmdShow},
		"why":     {"print the path(s) blocking a node", cmdWhy},
		"path":    {"print the route between two nodes", cmdPath},
		"rdeps":   {"print what depends on a node", cmdRdeps},
		"merge":   {"fold one node into another", cmdMerge},
		"import":  {"bind nodes to GitHub issues and PRs", cmdImport},
		"done":    {"mark a node done", cmdState(event.StateDone)},
		"drop":    {"mark a node dropped", cmdState(event.StateDropped)},
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	e := &env{stdout: stdout, stderr: stderr}

	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "-h", "--help", "help":
		// `weft help agent` prints the onboarding snippet (docs/AGENT.md), which
		// is the artifact milestone 10 is actually about — usage text is for
		// humans who already know the tool exists.
		if len(args) > 1 && args[1] == "agent" {
			return printAgentDoc(stdout)
		}
		usage(stdout)
		return exitOK
	case "--version":
		fmt.Fprintln(stdout, version)
		return exitOK
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "weft: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
	return cmd.run(e, args[1:])
}

func notImplemented(e *env, _ []string) int {
	fmt.Fprintln(e.stderr, "weft: not implemented yet")
	return exitNotImplemented
}

// parseFlags runs the flag dance every subcommand shares and reports whether to
// proceed. A deliberate help flag is a success — usage on stdout, exit 0 — not
// the exit 1 on stderr a typo earns (help used to exit 1 from every
// subcommand). BUILD.md §5 defines exit 1 as
// unknown command, bad flag, or missing argument; a correct help flag is none
// of the three.
//
// The destination is chosen *before* Parse rather than after, because flag
// writes usage to the FlagSet's output the moment it sees -h and returns
// ErrHelp only afterwards. Capturing it first is what lets the same text go to
// stdout for a help request and to stderr for a mistake.
//
// Deciding this from flag's own result, rather than by scanning the arguments
// beforehand, is what keeps `--note -h` a note whose text is "-h": only the
// parser knows which dashed words are flags and which are values, and a
// pre-scan that guesses turns a legitimate command into a silent no-op that
// still exits 0.
//
// ok is false for both a help request and a parse error; the caller returns
// code in either case and only proceeds when ok.
func parseFlags(e *env, fs *flag.FlagSet, args []string) (code int, ok bool) {
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := fs.Parse(cli.Permute(fs, args))
	switch {
	case errors.Is(err, flag.ErrHelp):
		io.Copy(e.stdout, &out)
		return exitOK, false
	case err != nil:
		io.Copy(e.stderr, &out)
		return exitUsage, false
	}
	return exitOK, true
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "weft %s — a durable graph of work\n\nusage: weft <command> [args]\n\ncommands:\n", version)
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-8s %s\n", name, commands[name].summary)
	}
	fmt.Fprintf(w, "\nweft help agent   orientation for an agent, ready to paste into CLAUDE.md\n")
	fmt.Fprintf(w, "docs: SPEC.md (behavior), EVENTS.md (log format), BUILD.md (build)\n")
}

// currentNS resolves the namespace for the working directory. A failure to read
// the cwd is not worth failing a command over — the fallback is a namespace,
// not an error.
func currentNS(storeDir string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ns.Default
	}
	return ns.Resolve(cwd, storeDir)
}
