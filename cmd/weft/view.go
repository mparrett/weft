package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/mparrett/weft/internal/graph"
	"github.com/mparrett/weft/internal/store"
)

// viewPage is the interactive page, shared with tools/weft-html.py. The
// template is ~74 KB and the renderer is not in it: mermaid comes from a
// pinned CDN build or, with --self-contained, from a local file read at run
// time. Embedding mermaid itself would cost every command's cold start for the
// one that draws (BUILD.md §1.1); embedding the template costs nothing
// until `view` runs.
//
//go:embed view.html
var viewPage string

// The CDN build the default page loads, pinned to an exact version with its
// integrity hash so a page's drawing cannot change under it. These duplicate
// tools/weft-html.py's MERMAID_CDN_*; TestViewMatchesWeftHTML fails if the two
// drift, and the tool's network test checks the hash against the file.
const (
	mermaidCDNURL = "https://cdn.jsdelivr.net/npm/@mermaid-js/tiny@11.17.2/dist/mermaid.tiny.js"
	mermaidCDNSRI = "sha384-10s6o+N3M2s7dB3bn+t7QT0KKY4aJUPH3JgMK/M0DCc/C23fI5mm0h2dPQcRtVdd"
)

// mermaidHints is where a local mermaid tends to live, for --self-contained
// when neither --mermaid-js nor $WEFT_MERMAID_JS names one. A var so tests can
// empty it: some entries are global-install paths, and a test that a miss is an
// error must not depend on what the host has installed.
var mermaidHints = []string{
	"./mermaid.min.js",
	"./node_modules/mermaid/dist/mermaid.min.js",
	"~/.npm/_npx/*/node_modules/mermaid/dist/mermaid.min.js",
	"~/.cache/npm/_npx/*/node_modules/mermaid/dist/mermaid.min.js",
	"/usr/local/lib/node_modules/mermaid/dist/mermaid.min.js",
	"/opt/homebrew/lib/node_modules/mermaid/dist/mermaid.min.js",
}

// openPage hands the written page to the desktop. Start, not Run: the browser
// outlives the command, and waiting on it would hang an agent's shell.
var openPage = func(path string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, path).Start()
}

var errNoMermaid = errors.New("--self-contained could not find mermaid.min.js.\n" +
	"  Pass --mermaid-js <path>, or set $WEFT_MERMAID_JS, or run:\n" +
	"    npm install mermaid   # then it is found automatically\n" +
	"  Or drop --self-contained to load mermaid from the CDN.")

// cmdView writes the graph as one interactive HTML page: search, namespace
// scope, fold, layout, and view state in the URL fragment.
//
// It is a snapshot, not a server. The page is built from one export fold and
// never watches the log; a live view is phase 3's resident process (SPEC §10),
// and nothing here may assume one.
func cmdView(e *env, args []string) int {
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	out := fs.String("o", "", "output file, or - for stdout (default: $XDG_CACHE_HOME/weft/<title>.html)")
	fs.StringVar(out, "out", "", "same as -o")
	title := fs.String("title", "weft", "page title")
	initialNS := fs.String("ns", "", "namespace the page opens scoped to; it still holds every namespace")
	onlyNS := fs.String("only-ns", "", "build the page from this namespace alone, so the others are not in the file")
	selfContained := fs.Bool("self-contained", false, "inline a local mermaid.min.js so the page draws offline (default: load a pinned build from the CDN)")
	mermaidJS := fs.String("mermaid-js", "", "path to mermaid.min.js to inline; implies --self-contained")
	openIt := fs.Bool("open", false, "open the page in the default browser")
	asJSON := fs.Bool("json", false, "report the written page as JSON: path, bytes, self_contained")

	if code, ok := parseFlags(e, fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "weft view: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *openIt && *out == "-" {
		fmt.Fprintln(e.stderr, "weft view: --open needs a file; it cannot open stdout")
		return exitUsage
	}
	if *asJSON && *out == "-" {
		fmt.Fprintln(e.stderr, "weft view: --json reports a written file; with -o - the page itself is the output")
		return exitUsage
	}

	// An explicit bundle path is a request to inline it. $WEFT_MERMAID_JS is
	// not: it says where a bundle is, and an environment variable silently
	// making the default page offline-only is the quiet mode switch the
	// CDN-by-default design rules out. Resolved before the fold so a miss costs
	// nothing.
	inline := *selfContained || *mermaidJS != ""
	var mermaidTag, mermaidFrom string
	if inline {
		path, err := findMermaid(*mermaidJS)
		if err != nil {
			return fail(e, exitExternal, *asJSON, err)
		}
		bundle, err := os.ReadFile(path)
		if err != nil {
			return fail(e, exitExternal, *asJSON, err)
		}
		mermaidTag = "<script>" + string(bundle) + "</script>"
	} else {
		mermaidTag = `<script src="` + mermaidCDNURL + `" integrity="` + mermaidCDNSRI +
			`" crossorigin="anonymous"></script>`
		mermaidFrom = mermaidCDNURL
	}

	st, err := store.Open()
	if err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	g, err := graph.Fold(st.Fold)
	if err != nil {
		return fail(e, storeExitCode(err), *asJSON, err)
	}

	// Always every section: the detail panel shows provenance, claims and
	// observation ages, and the default export's silence about them is exactly
	// what a picture made for somebody else should not inherit.
	var exported bytes.Buffer
	nodes := g.Ordered()
	if err := exportJSON(&env{stdout: &exported, stderr: e.stderr}, g, nodes, sortedEdges(g, nodes), includeAll); err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	data := exported.Bytes()
	if *onlyNS != "" {
		scoped, err := scopeToNS(data, *onlyNS)
		if err != nil {
			return fail(e, exitNotFound, *asJSON, err)
		}
		data = scoped
		// A scoped page that opened on "all" would show the stubs as peers of
		// real work on first paint. The reader can still widen it.
		if *initialNS == "" {
			*initialNS = *onlyNS
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return fail(e, exitStore, *asJSON, err)
	}

	page := fillView(map[string]string{
		"__TITLE__":       htmlEscape(*title),
		"__DATA__":        scriptJSON(compact.String()),
		"__INITIAL_NS__":  scriptJSONString(*initialNS),
		"__SCOPE__":       scriptJSONString(*onlyNS),
		"__MERMAID_SRC__": scriptJSONString(mermaidFrom),
		"__MERMAID__":     mermaidTag,
	})

	if *out == "-" {
		_, err := fmt.Fprint(e.stdout, page)
		return renderOr(e, err)
	}
	if *out == "" {
		*out = defaultViewPath(*title)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	if err := os.WriteFile(*out, []byte(page), 0o644); err != nil {
		return fail(e, exitStore, *asJSON, err)
	}
	// Opened before reporting, so a --json caller gets one document either
	// way: the report on success, the error object on failure.
	if *openIt {
		if err := openPage(*out); err != nil {
			return fail(e, exitExternal, *asJSON, fmt.Errorf("wrote %s but could not open it: %w", *out, err))
		}
	}
	if *asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return renderOr(e, enc.Encode(struct {
			Path          string `json:"path"`
			Bytes         int    `json:"bytes"`
			SelfContained bool   `json:"self_contained"`
		}{*out, len(page), inline}))
	}
	// The path alone on stdout, so `open "$(weft view)"` and an agent reading
	// the first field both work; the description goes to stderr.
	fmt.Fprintln(e.stdout, *out)
	how := "loads mermaid from the CDN; --self-contained to inline"
	if inline {
		how = "self-contained"
	}
	fmt.Fprintf(e.stderr, "(%.1f MB, %s)\n", float64(len(page))/1_048_576, how)
	return exitOK
}

// findMermaid resolves the bundle to inline. A miss is an error, never a
// quiet fetch: the caller asked for a page that does not reach the network.
func findMermaid(explicit string) (string, error) {
	for _, c := range []string{explicit, os.Getenv("WEFT_MERMAID_JS")} {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err != nil || fi.IsDir() {
			return "", fmt.Errorf("no such file: %s", c)
		}
		return c, nil
	}
	home, _ := os.UserHomeDir()
	for _, pattern := range mermaidHints {
		if strings.HasPrefix(pattern, "~/") {
			if home == "" {
				continue
			}
			pattern = filepath.Join(home, pattern[2:])
		}
		hits, _ := filepath.Glob(pattern)
		sort.Strings(hits)
		for _, h := range hits {
			if fi, err := os.Stat(h); err == nil && !fi.IsDir() {
				return h, nil
			}
		}
	}
	return "", errNoMermaid
}

// scopeToNS keeps only ns, stubbing the far end of every edge that leaves it.
//
// A cross-boundary edge is kept and its outside endpoint reduced to id and
// namespace. Dropping those edges would make the page say a node has no
// blockers when it has one in another project — the failure SPEC §5 exists to
// prevent — and the reason to scope a page is to send it to somebody.
//
// This is a disclosure boundary, so only the two sections the viewer reads are
// carried; a future export section cannot silently ride along into a page
// promised to hold one namespace.
func scopeToNS(data []byte, ns string) ([]byte, error) {
	var in struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }

	kept := []map[string]any{}
	inside := map[string]bool{}
	byID := map[string]map[string]any{}
	present := map[string]bool{}
	for _, n := range in.Nodes {
		byID[str(n, "id")] = n
		present[str(n, "ns")] = true
		if str(n, "ns") == ns {
			kept = append(kept, n)
			inside[str(n, "id")] = true
		}
	}
	if len(kept) == 0 {
		// Loud beats an empty page: the usual cause is a typo, and an empty
		// artifact looks like an empty graph.
		have := make([]string, 0, len(present))
		for p := range present {
			have = append(have, p)
		}
		sort.Strings(have)
		list := strings.Join(have, ", ")
		if list == "" {
			list = "nothing"
		}
		return nil, fmt.Errorf("no nodes in namespace %q. This store holds: %s", ns, list)
	}

	edges := []map[string]any{}
	stubbed := map[string]bool{}
	var stubs []map[string]any
	for _, ed := range in.Edges {
		from, to := str(ed, "from"), str(ed, "to")
		if !inside[from] && !inside[to] {
			continue
		}
		edges = append(edges, ed)
		for _, end := range []string{from, to} {
			if end == "" || inside[end] || stubbed[end] {
				continue
			}
			stubbed[end] = true
			// A dangling endpoint is a normal state (SPEC §9.3); it stubs the
			// same way, with an unknown namespace.
			stubs = append(stubs, map[string]any{
				"id": end, "ns": str(byID[end], "ns"), "title": "", "external": true,
			})
		}
	}
	return json.Marshal(map[string]any{"nodes": append(kept, stubs...), "edges": edges})
}

var viewPlaceholder = regexp.MustCompile(`__(?:TITLE|DATA|INITIAL_NS|SCOPE|MERMAID_SRC|MERMAID)__`)

// fillView substitutes in one pass. Chained replaces would expose each
// payload to the next: a node titled "__MERMAID__" would splice the bundle
// into the data, and a --title of "__DATA__" would dump the graph into <title>.
func fillView(fill map[string]string) string {
	return viewPlaceholder.ReplaceAllStringFunc(viewPage, func(m string) string { return fill[m] })
}

// scriptJSON makes JSON safe inside an inline <script>: `</script>` in any
// value ends the element early however valid the JSON is. encoding/json
// already escapes `<`, so this is a backstop for input that did not come
// through it.
func scriptJSON(s string) string { return strings.ReplaceAll(s, "</", `<\/`) }

func scriptJSONString(s string) string {
	b, _ := json.Marshal(s)
	return scriptJSON(string(b))
}

// htmlEscape matches Python's html.escape, so the page and weft-html.py's
// agree byte for byte; html.EscapeString spells the apostrophe differently.
var htmlEscape = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;",
).Replace

// defaultViewPath is a cache path, not the working directory: the obvious
// default drops a derived artifact into whatever repo you are standing in.
// Cache rather than data, because regenerating it is one command.
func defaultViewPath(title string) string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(title), "-"), "-")
	if slug == "" {
		slug = "graph"
	}
	return filepath.Join(base, "weft", slug+".html")
}
