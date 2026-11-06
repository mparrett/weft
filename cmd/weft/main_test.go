package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// session runs one command the way a fresh process would, and returns its exit
// code and streams. Commands take their environment as a parameter precisely so
// this needs no subprocess.
func session(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func scratch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WEFT_HOME", dir)
	t.Setenv("WEFT_ACTOR", "agent:test")
	// Pinned so tests do not depend on the git remote of whatever directory
	// they happen to run in. Without this, namespace resolution makes the suite
	// pass in a bare checkout and fail once the repo has an origin.
	t.Setenv("WEFT_NS", "default")
	// `weft view` writes under the cache dir by default, and the snippet test
	// runs every documented command; without this a test run drops pages into
	// the developer's real ~/.cache.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	return dir
}

// The milestone 2 exit condition: three nodes added in one session are listed
// in the next, and the log is valid JSONL to a parser that knows nothing about
// weft's envelope.
func TestAddThenListAcrossSessions(t *testing.T) {
	dir := scratch(t)

	titles := []string{"N+1 in report generation", "cache invalidation race", "batch export spec"}
	for _, title := range titles {
		if code, _, stderr := session(t, "add", title, "--kind", "bug"); code != exitOK {
			t.Fatalf("add %q: exit %d, stderr %s", title, code, stderr)
		}
	}

	code, stdout, stderr := session(t, "ls")
	if code != exitOK {
		t.Fatalf("ls: exit %d, stderr %s", code, stderr)
	}
	for _, title := range titles {
		if !strings.Contains(stdout, title) {
			t.Errorf("ls output missing %q:\n%s", title, stdout)
		}
	}
	if got := strings.Count(strings.TrimSpace(stdout), "\n") + 1; got != 3 {
		t.Errorf("ls listed %d nodes, want 3:\n%s", got, stdout)
	}

	// Validated with encoding/json directly rather than event.Parse, so a bug
	// in our own envelope logic cannot mask a malformed log.
	data, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		var generic map[string]any
		if err := json.Unmarshal([]byte(line), &generic); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i+1, err)
		}
	}
}

// Assertion order is file order, and it must be stable across reads.
func TestListOrderIsDeterministic(t *testing.T) {
	scratch(t)
	for _, title := range []string{"zebra", "apple", "mango"} {
		if code, _, _ := session(t, "add", title); code != exitOK {
			t.Fatal("add failed")
		}
	}
	_, first, _ := session(t, "ls")
	for i := 0; i < 5; i++ {
		if _, again, _ := session(t, "ls"); again != first {
			t.Fatalf("ls output changed between runs:\n%s\n---\n%s", first, again)
		}
	}
	// Assertion order, not alphabetical.
	if !strings.HasPrefix(strings.Split(first, "\n")[0][9:], "task  zebra") {
		t.Errorf("expected assertion order, got:\n%s", first)
	}
}

// The permute pass, end to end: without it the stdlib would drop --kind and the
// node would be silently created as the default kind.
func TestFlagsAfterPositional(t *testing.T) {
	scratch(t)
	if code, _, stderr := session(t, "add", "a bug", "--kind", "bug"); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	_, stdout, _ := session(t, "ls", "--json")
	var out struct {
		Nodes []struct{ Kind string } `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 1 {
		t.Fatalf("got %d nodes", len(out.Nodes))
	}
	if out.Nodes[0].Kind != "bug" {
		t.Errorf("kind = %q, want bug — the trailing flag was dropped", out.Nodes[0].Kind)
	}
}

func TestAddWithRefIsOneTransaction(t *testing.T) {
	dir := scratch(t)
	if code, _, stderr := session(t, "add", "spec", "--kind", "spec", "--ref", "file:docs/x.md"); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	_, stdout, _ := session(t, "ls", "--json")
	if !strings.Contains(stdout, "file:docs/x.md") {
		t.Errorf("ref not attached:\n%s", stdout)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Errorf("expected node.add and ref.add, got %d events", n)
	}
}

func TestExitCodes(t *testing.T) {
	scratch(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"add without title", []string{"add"}, exitUsage},
		{"add bad kind", []string{"add", "x", "--kind", "nonsense"}, exitUsage},
		{"add bad source", []string{"add", "x", "--source", "vibes"}, exitUsage},
		{"ls bad kind", []string{"ls", "--kind", "nonsense"}, exitUsage},
		{"ls stray argument", []string{"ls", "extra"}, exitUsage},
		{"unknown flag", []string{"add", "x", "--nope"}, exitUsage},
		{"help", []string{"--help"}, exitOK},
		{"version", []string{"--version"}, exitOK},
		{"merge without both nodes", []string{"merge", "a"}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := session(t, tc.args...); code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

// Asking a subcommand for help is a success, not a usage error (help used to exit 1):
// exit 0 and on stdout, distinct from a typo, which is exit 1 on stderr.
// Returning flag.ErrHelp as exitUsage made `weft ls --help` exit 1 (on stderr),
// so a caller branching on the exit code could not tell a help request from a
// mistake, and the text landed on the error stream either way.
func TestSubcommandHelpExitsZero(t *testing.T) {
	scratch(t)
	helped := []string{
		"add", "ls", "link", "unlink", "show", "why", "rdeps", "path",
		"claim", "release", "merge", "import", "export", "ns", "done", "drop",
	}
	for _, name := range helped {
		for _, arg := range []string{"--help", "-h"} {
			code, stdout, stderr := session(t, name, arg)
			if code != exitOK {
				t.Errorf("weft %s %s: exit %d, want %d", name, arg, code, exitOK)
			}
			// Help is the artifact that was asked for, so it goes to stdout,
			// not the error stream.
			if !strings.Contains(stdout, "Usage of") {
				t.Errorf("weft %s %s: usage not on stdout:\n%s", name, arg, stdout)
			}
			if stderr != "" {
				t.Errorf("weft %s %s: help wrote to stderr:\n%s", name, arg, stderr)
			}
		}
	}

	// A real unknown flag is still a usage error, distinct from a help request,
	// and its diagnostic stays on stderr.
	for _, args := range [][]string{{"ls", "--nope"}, {"add", "x", "--nope"}} {
		code, _, stderr := session(t, args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
		if stderr == "" {
			t.Errorf("%v: a usage error writes a diagnostic to stderr", args)
		}
	}
}

// A flag whose *value* is "-h" is a value, not a help request.
//
// Deciding what counts as help by scanning the arguments before parsing cannot
// tell the two apart — only the parser knows which dashed words are flags and
// which belong to the flag in front of them. Getting it wrong is silent and
// exits 0: the command prints usage, records nothing, and a caller branching on
// the exit code reads it as done.
func TestAFlagValueThatLooksLikeHelpIsNotHelp(t *testing.T) {
	scratch(t)
	id := addNode(t, "a node to claim", "task")

	code, _, stderr := session(t, "claim", id, "--note", "-h")
	if code != exitOK {
		t.Fatalf("claim with a -h note: exit %d, %s", code, stderr)
	}
	if _, out, _ := session(t, "ls", "--mine"); !strings.Contains(out, "a node to claim") {
		t.Errorf("the claim was silently skipped as a help request:\n%s", out)
	}

	// And the terminator still lets a title be literally "--help".
	if code, _, stderr := session(t, "add", "--", "--help"); code != exitOK {
		t.Errorf("add -- --help: exit %d, %s", code, stderr)
	}
}

// Exit 3 is still part of the contract (BUILD.md §5) and smoke.sh still treats
// it as pending, but every command is built now, so nothing emits it. Pinned
// directly rather than through a command, so the code does not rot before the
// next unbuilt thing needs it.
func TestNotImplementedStillExitsThree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &env{stdout: &stdout, stderr: &stderr}
	if code := notImplemented(e, nil); code != exitNotImplemented {
		t.Errorf("exit %d, want %d", code, exitNotImplemented)
	}
	if stderr.Len() == 0 {
		t.Error("every nonzero exit writes a diagnostic to stderr (BUILD.md §5)")
	}
}

func TestNamespaceFiltersListing(t *testing.T) {
	scratch(t)
	session(t, "add", "in default")
	session(t, "add", "elsewhere", "--ns", "other/repo")

	_, stdout, _ := session(t, "ls")
	if strings.Contains(stdout, "elsewhere") {
		t.Error("default listing leaked another namespace")
	}
	_, all, _ := session(t, "ls", "--all-ns")
	if !strings.Contains(all, "elsewhere") || !strings.Contains(all, "in default") {
		t.Errorf("--all-ns should show both:\n%s", all)
	}
	_, other, _ := session(t, "ls", "--ns", "other/repo")
	if !strings.Contains(other, "elsewhere") || strings.Contains(other, "in default") {
		t.Errorf("--ns should select one namespace:\n%s", other)
	}
}

func TestEmptyStoreListsNothing(t *testing.T) {
	scratch(t)
	code, stdout, _ := session(t, "ls")
	if code != exitOK {
		t.Errorf("empty store should not be an error, got exit %d", code)
	}
	if stdout != "" {
		t.Errorf("expected no output, got %q", stdout)
	}
}

// A corrupt log is a store error, not a usage error (BUILD.md §5).
func TestCorruptLogExitsStore(t *testing.T) {
	dir := scratch(t)
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte("garbage\n{\"v\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := session(t, "ls"); code != exitStore {
		t.Errorf("exit %d, want %d", code, exitStore)
	}
}

func TestJSONErrorShape(t *testing.T) {
	dir := scratch(t)
	os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte("garbage\n{\"v\":1}\n"), 0o644)
	code, stdout, _ := session(t, "ls", "--json")
	if code != exitStore {
		t.Fatalf("exit %d", code)
	}
	var out struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json error was not JSON: %v\n%s", err, stdout)
	}
	if out.Code != exitStore || out.Error == "" {
		t.Errorf("unexpected error object: %+v", out)
	}
}

// appendRaw writes a hand-built event line, for states the CLI cannot produce.
func appendRaw(t *testing.T, dir, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// fullID expands a displayed short id back to its full 64-hex form.
func fullID(t *testing.T, short string) string {
	t.Helper()
	_, stdout, _ := session(t, "show", short, "--json")
	var out struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.Node.ID == "" {
		t.Fatalf("could not expand %q: %v\n%s", short, err, stdout)
	}
	return out.Node.ID
}
