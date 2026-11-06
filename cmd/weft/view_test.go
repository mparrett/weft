package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// viewConsts pulls the page's data literals out, so tests can read what reached
// the browser rather than grepping a 74 KB document.
var viewConstRE = regexp.MustCompile(`(?m)^const (DATA|INITIAL_NS|SCOPE|MERMAID_SRC) = (.*);$`)

func viewConsts(t *testing.T, page string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, m := range viewConstRE.FindAllStringSubmatch(page, -1) {
		var v any
		if err := json.Unmarshal([]byte(m[2]), &v); err != nil {
			t.Fatalf("const %s is not JSON: %v\n%s", m[1], err, m[2])
		}
		out[m[1]] = v
	}
	if len(out) != 4 {
		t.Fatalf("found %d of the page's 4 data literals; the template moved", len(out))
	}
	return out
}

func viewNodes(t *testing.T, page string) []map[string]any {
	t.Helper()
	var nodes []map[string]any
	for _, n := range viewConsts(t, page)["DATA"].(map[string]any)["nodes"].([]any) {
		nodes = append(nodes, n.(map[string]any))
	}
	return nodes
}

func noMermaidHints(t *testing.T) {
	t.Helper()
	saved := mermaidHints
	mermaidHints = nil
	t.Cleanup(func() { mermaidHints = saved })
}

func writeBundle(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mermaid.min.js")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The default is the CDN build, pinned with its integrity hash, and the
// data is the full export: the detail panel shows provenance and claims, and a
// page drawn for someone should not inherit the default export's silence.
func TestViewDefaultsToThePinnedCDNAndTheFullExport(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_MERMAID_JS", "")
	addNode(t, "the work", "task")

	code, page, stderr := session(t, "view", "-o", "-")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	tag := `<script src="` + mermaidCDNURL + `" integrity="` + mermaidCDNSRI + `" crossorigin="anonymous"></script>`
	if !strings.Contains(page, tag) {
		t.Errorf("page does not load the pinned CDN build")
	}
	if strings.Contains(page, "__MERMAID") || strings.Contains(page, "__DATA__") {
		t.Errorf("a placeholder survived into the page")
	}
	n := viewNodes(t, page)[0]
	if n["title"] != "the work" || n["by"] != "agent:test" || n["source"] == nil {
		t.Errorf("node lacks the --include all fields: %v", n)
	}
	if got := viewConsts(t, page)["MERMAID_SRC"]; got != mermaidCDNURL {
		t.Errorf("MERMAID_SRC = %v, want the CDN URL for the failure message", got)
	}
}

func TestViewWritesUnderTheCacheAndPrintsThePath(t *testing.T) {
	dir := scratch(t)
	addNode(t, "x", "task")

	code, stdout, stderr := session(t, "view", "--title", "Acme API!")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := filepath.Join(dir, "cache", "weft", "acme-api.html")
	if strings.TrimSpace(stdout) != want {
		t.Errorf("stdout = %q, want just the path %q", stdout, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("page not written: %v", err)
	}
	if !strings.Contains(stderr, "CDN") {
		t.Errorf("stderr does not say which mode the page is in: %q", stderr)
	}
}

func TestViewSelfContainedInlinesAndFetchesNothing(t *testing.T) {
	scratch(t)
	addNode(t, "x", "task")
	t.Setenv("WEFT_MERMAID_JS", writeBundle(t, "ENV_BUNDLE"))

	code, page, stderr := session(t, "view", "--self-contained", "-o", "-")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(page, "<script>ENV_BUNDLE</script>") {
		t.Error("bundle not inlined")
	}
	for _, remote := range []string{"https://cdn", "unpkg.com", "jsdelivr", "<script src="} {
		if strings.Contains(page, remote) {
			t.Errorf("self-contained page references %q", remote)
		}
	}
	if got := viewConsts(t, page)["MERMAID_SRC"]; got != "" {
		t.Errorf("MERMAID_SRC = %v, want empty so the page blames the bundle, not the network", got)
	}
}

func TestViewMermaidJSImpliesSelfContained(t *testing.T) {
	scratch(t)
	addNode(t, "x", "task")
	code, page, _ := session(t, "view", "--mermaid-js", writeBundle(t, "FLAG_BUNDLE"), "-o", "-")
	if code != exitOK || !strings.Contains(page, "<script>FLAG_BUNDLE</script>") {
		t.Errorf("exit %d; --mermaid-js did not inline", code)
	}
}

// $WEFT_MERMAID_JS says where a bundle is, not which mode to use.
func TestViewEnvBundleDoesNotSwitchTheDefault(t *testing.T) {
	scratch(t)
	addNode(t, "x", "task")
	t.Setenv("WEFT_MERMAID_JS", writeBundle(t, "ENV_BUNDLE"))
	_, page, _ := session(t, "view", "-o", "-")
	if strings.Contains(page, "ENV_BUNDLE") || !strings.Contains(page, mermaidCDNURL) {
		t.Error("an environment variable switched the default page to inline")
	}
}

// No silent fallback: asking for an offline page and not finding a bundle is
// an error, not a page that fetches. Exit 7 because it is a missing external
// dependency, like gh, not a weft failure.
func TestViewSelfContainedMissIsAnErrorNotAFetch(t *testing.T) {
	dir := scratch(t)
	addNode(t, "x", "task")
	noMermaidHints(t)
	t.Setenv("WEFT_MERMAID_JS", "")

	code, stdout, stderr := session(t, "view", "--self-contained")
	if code != exitExternal {
		t.Errorf("exit %d, want %d", code, exitExternal)
	}
	if stdout != "" || !strings.Contains(stderr, "could not find mermaid.min.js") {
		t.Errorf("stdout %q, stderr %q", stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache", "weft", "weft.html")); err == nil {
		t.Error("a page was written anyway")
	}
}

func TestViewOnlyNSStubsTheFarEndAndRejectsAnEmptyScope(t *testing.T) {
	scratch(t)
	local := addNodeFull(t, "local work", "task", "--ns", "acme/api")
	far := addNodeFull(t, "secret upstream", "task", "--ns", "other/fork")
	addNodeFull(t, "unrelated", "task", "--ns", "other/fork")
	session(t, "link", local, "depends-on", far)

	code, page, stderr := session(t, "view", "--only-ns", "acme/api", "-o", "-")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(page, "secret upstream") || strings.Contains(page, "unrelated") {
		t.Error("another namespace's titles are in a page scoped to acme/api")
	}
	nodes := viewNodes(t, page)
	if len(nodes) != 2 || nodes[1]["external"] != true || nodes[1]["ns"] != "other/fork" {
		t.Errorf("want the local node plus a stub of the blocker, got %v", nodes)
	}
	c := viewConsts(t, page)
	if c["SCOPE"] != "acme/api" || c["INITIAL_NS"] != "acme/api" {
		t.Errorf("SCOPE %v, INITIAL_NS %v", c["SCOPE"], c["INITIAL_NS"])
	}

	code, _, stderr = session(t, "view", "--only-ns", "acme/apj", "-o", "-")
	if code != exitNotFound || !strings.Contains(stderr, "acme/api, other/fork") {
		t.Errorf("typo'd scope: exit %d, stderr %q", code, stderr)
	}
}

// One pass over the template: a title that spells a placeholder must arrive
// as text, and a title that closes the script element must not close it.
func TestViewPayloadsCannotReachOtherPlaceholders(t *testing.T) {
	scratch(t)
	addNode(t, "__MERMAID__ </script><b>x", "task")
	_, page, _ := session(t, "view", "--title", "__DATA__ & 'q'", "-o", "-")
	if !strings.Contains(page, "<title>__DATA__ &amp; &#x27;q&#x27;</title>") {
		t.Error("title not escaped as text")
	}
	if strings.Count(page, "</script>") != 2 {
		t.Errorf("a payload closed a script element early: %d closers", strings.Count(page, "</script>"))
	}
	if viewNodes(t, page)[0]["title"] != "__MERMAID__ </script><b>x" {
		t.Error("title did not survive the round trip")
	}
}

func TestViewOpenHandsThePathToTheDesktop(t *testing.T) {
	scratch(t)
	addNode(t, "x", "task")
	var opened string
	saved := openPage
	openPage = func(p string) error { opened = p; return nil }
	t.Cleanup(func() { openPage = saved })

	code, stdout, _ := session(t, "view", "--open")
	if code != exitOK || opened == "" || opened != strings.TrimSpace(stdout) {
		t.Errorf("exit %d, opened %q, printed %q", code, opened, stdout)
	}
	if code, _, _ := session(t, "view", "--open", "-o", "-"); code != exitUsage {
		t.Errorf("--open with stdout: exit %d, want usage", code)
	}
}

// weft-html.py fills the same template from an export it is handed, and the
// two must not drift: same page, data literals compared as values (the JSON
// encoders differ in spacing and in which characters they escape).
func TestViewMatchesWeftHTML(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	scratch(t)
	t.Setenv("WEFT_MERMAID_JS", "")
	a := addNodeFull(t, "naïve </script> & \"quotes\"", "task", "--ns", "acme/api")
	b := addNodeFull(t, "blocker", "bug", "--ns", "other/fork")
	session(t, "link", a, "depends-on", b)
	session(t, "claim", a)
	bundle := writeBundle(t, "/*bundle*/")

	_, exported, _ := session(t, "export", "--json", "--include", "all")
	for _, args := range [][]string{
		{"--title", "it's <weft>"},
		{"--ns", "acme/api"},
		{"--only-ns", "acme/api"},
		{"--mermaid-js", bundle},
	} {
		_, goPage, stderr := session(t, append([]string{"view", "-o", "-"}, args...)...)
		cmd := exec.Command(py, append([]string{"../../tools/weft-html.py", "-o", "-"}, args...)...)
		cmd.Stdin = strings.NewReader(exported)
		cmd.Env = append(os.Environ(), "WEFT_MERMAID_JS=")
		pyPage, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: weft-html.py: %v (go stderr %s)", args, err, stderr)
		}
		if g, p := viewConsts(t, goPage), viewConsts(t, string(pyPage)); !reflect.DeepEqual(g, p) {
			t.Errorf("%v: data literals differ\n go: %v\n py: %v", args, g, p)
		}
		strip := func(s string) string { return viewConstRE.ReplaceAllString(s, "const $1 = …;") }
		if strip(goPage) != strip(string(pyPage)) {
			t.Errorf("%v: pages differ outside the data literals", args)
		}
	}
}

// Every command takes --json (SPEC §6). For view the report is the file it
// wrote; failures are the usual error object, on stdout.
func TestViewJSONReportsTheFileOrTheError(t *testing.T) {
	scratch(t)
	addNode(t, "x", "task")
	code, stdout, _ := session(t, "view", "--json")
	var got struct {
		Path          string
		Bytes         int
		SelfContained bool `json:"self_contained"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || code != exitOK {
		t.Fatalf("exit %d, not JSON: %v\n%s", code, err, stdout)
	}
	if fi, err := os.Stat(got.Path); err != nil || int(fi.Size()) != got.Bytes || got.SelfContained {
		t.Errorf("report %+v does not describe the written file (%v)", got, err)
	}

	noMermaidHints(t)
	t.Setenv("WEFT_MERMAID_JS", "")
	code, stdout, _ = session(t, "view", "--json", "--self-contained")
	if code != exitExternal || !json.Valid([]byte(stdout)) || !strings.Contains(stdout, "mermaid.min.js") {
		t.Errorf("exit %d, want a JSON error naming the miss:\n%s", code, stdout)
	}
	if code, _, _ := session(t, "view", "--json", "-o", "-"); code != exitUsage {
		t.Errorf("--json with -o -: exit %d, want usage", code)
	}
}
