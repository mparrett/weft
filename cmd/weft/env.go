package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mparrett/weft/internal/actor"
	"github.com/mparrett/weft/internal/ns"
	"github.com/mparrett/weft/internal/render"
	"github.com/mparrett/weft/internal/store"
)

// weftVars are the environment variables weft reads, with what each one does.
//
// This list is the command's whole reason to exist. A variable that looks
// plausible and is read by nothing is indistinguishable from one that works,
// and the failure is silent *and* durable: writes land in a store nobody meant
// to touch, and the only way to find out is to read the log. `WEFT_STORE` cost
// a session exactly that way.
var weftVars = []struct{ name, does string }{
	{"WEFT_HOME", "which store to read and write"},
	{"WEFT_NS", "which namespace new nodes land in"},
	{"WEFT_ACTOR", "who the log records as asserting"},
	// Where a local mermaid is, not whether to use one: only --self-contained
	// (or --mermaid-js) inlines, so setting this never changes the default page.
	{"WEFT_MERMAID_JS", "where `view --self-contained` and tools/weft-html.py find mermaid"},
	// Read by the tools rather than the binary. Listed so setting one is not
	// reported as a mistake — the tools are part of weft even though they are
	// outside the build (BUILD.md §1.1), and a false alarm here is the same
	// failure the warning exists to prevent, pointed the other way.
	{"WEFT_BIN", "which weft tools/weft-range-context.py shells out to (not read by this binary)"},
}

// cmdEnv answers "where am I operating, and why" in one call.
//
// Every fact here was already decided somewhere and reported nowhere: the store
// path had no command at all, and the actor and namespace rules were only
// visible by writing a node and looking at where it landed. A question that can
// only be answered by mutating the store is one people stop asking.
//
// It reports and never repairs. A suspicious variable is named, not unset —
// guessing what someone meant is how a diagnostic becomes a second bug.
func cmdEnv(e *env, args []string) int {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "weft env: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}

	dir, dirSrc, err := store.ResolveDir()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		// Same reasoning as cmdNS: an unreadable cwd still has an answer.
		cwd = "."
	}
	name, nsSrc, nsDetail := ns.ResolveFrom(cwd, dir)
	who, whoSrc := actor.ResolveFrom(dir)

	logPath := filepath.Join(dir, "log.jsonl")
	events, bytes := logSize(logPath)
	configPath := filepath.Join(dir, "config")
	_, configErr := os.Stat(configPath)

	if *asJSON {
		out := struct {
			Version   string   `json:"version"`
			Store     string   `json:"store"`
			StoreFrom string   `json:"store_from"`
			Log       string   `json:"log"`
			Events    int      `json:"events"`
			Bytes     int64    `json:"bytes"`
			Config    string   `json:"config,omitempty"`
			Actor     string   `json:"actor"`
			ActorFrom string   `json:"actor_from"`
			NS        string   `json:"ns"`
			NSFrom    string   `json:"ns_from"`
			Unread    []string `json:"unread_env,omitempty"`
		}{
			Version: version, Store: dir, StoreFrom: dirSrc,
			Log: logPath, Events: events, Bytes: bytes,
			Actor: who, ActorFrom: whoSrc, NS: name, NSFrom: string(nsSrc),
			Unread: unreadWeftVars(),
		}
		if configErr == nil {
			out.Config = configPath
		}
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fail(e, exitStore, true, err)
		}
		warnUnreadVars(e)
		return exitOK
	}

	row := func(k, v, why string) {
		if why != "" {
			fmt.Fprintf(e.stdout, "%-10s%s\t(%s)\n", k, v, why)
			return
		}
		fmt.Fprintf(e.stdout, "%-10s%s\n", k, v)
	}
	row("version", version, "")
	row("store", dir, dirSrc)
	// An absent log is the normal state of a fresh store, not a problem — it
	// folds to an empty graph. Saying so beats printing "0 events" against a
	// path that does not exist.
	if events < 0 {
		row("log", logPath, "not written yet")
	} else {
		row("log", logPath, fmt.Sprintf("%s, %s", render.Plural(events, "event"), humanBytes(bytes)))
	}
	if configErr == nil {
		row("config", configPath, "")
	}
	row("actor", who, whoSrc)
	if nsDetail != "" {
		row("ns", name, fmt.Sprintf("%s: %s", nsSrc, nsDetail))
	} else {
		row("ns", name, string(nsSrc))
	}
	warnUnreadVars(e)
	return exitOK
}

// logSize returns the committed event count and byte size of the log, or
// (-1, 0) when there is none. One event per line is the format (EVENTS.md §1),
// so counting is exact and costs no fold — which is what keeps `env` cheap
// enough to run reflexively before a bulk write.
//
// **Only newline-terminated lines count.** A crash mid-append leaves a fragment
// with no LF, and the fold drops it (`store.foldBytes` discards the last
// element for exactly this reason, EVENTS.md §5.3). Counting it here would make
// `env` report more events than any other command can see — the one number this
// command exists to be trusted on, disagreeing with the fold precisely when
// something has gone wrong and somebody is checking.
func logSize(path string) (events int, size int64) {
	info, err := os.Stat(path)
	if err != nil {
		return -1, 0
	}
	f, err := os.Open(path)
	if err != nil {
		return -1, 0
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		// A final chunk without the delimiter is the uncommitted fragment; it
		// ends the count rather than joining it.
		if err != nil {
			break
		}
		if strings.TrimSpace(line) != "" {
			events++
		}
	}
	return events, info.Size()
}

// unreadWeftVars returns any WEFT_* variable set in the environment that weft
// does not read, sorted.
func unreadWeftVars() []string {
	known := make(map[string]bool, len(weftVars))
	for _, v := range weftVars {
		known[v.name] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "WEFT_") || known[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// warnUnreadVars names the variable and lists what weft actually reads.
//
// Deliberately no "did you mean": a guess that lands wrong is worse than the
// list, and the list answers the question outright for any near miss, not just
// the ones someone thought to anticipate. It goes to stderr so a --json caller
// still gets a parseable payload on stdout.
func warnUnreadVars(e *env) {
	unread := unreadWeftVars()
	if len(unread) == 0 {
		return
	}
	fmt.Fprintf(e.stderr, "warning: set but not read by weft: %s\n", strings.Join(unread, ", "))
	for _, v := range weftVars {
		fmt.Fprintf(e.stderr, "  %-16s %s\n", v.name, v.does)
	}
}

// humanBytes keeps the size readable at every scale a log reaches. The point is
// "is this the store I think it is", and 393.7 KB answers that where 402,939
// makes you count digits.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
