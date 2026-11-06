package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// observeFixture makes a node bound to a real file and returns the store dir,
// the node id, and the file's path and ref.
func observeFixture(t *testing.T) (dir, id, path, ref string) {
	t.Helper()
	dir = scratch(t)
	path = filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(path, []byte("first draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref = "file:" + path
	code, stdout, stderr := session(t, "add", "handoff for the report work", "--kind", "note", "--ref", ref)
	if code != exitOK {
		t.Fatalf("add: exit %d, %s", code, stderr)
	}
	return dir, strings.Fields(stdout)[0], path, ref
}

// The milestone 11 exit condition, write side: an observation carries the
// artifact's own clock and hash, and re-observing an unchanged file leaves the
// log byte-identical — not merely free of duplicates (EVENTS.md §3.9).
func TestRefObserveIsIdempotentOnTheBytes(t *testing.T) {
	dir, id, path, ref := observeFixture(t)

	code, stdout, stderr := session(t, "ref", "observe", id)
	if code != exitOK {
		t.Fatalf("observe: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "observed   "+ref) {
		t.Errorf("first observation not reported:\n%s", stdout)
	}
	first := logBytes(t, dir)
	for _, field := range []string{`"ev":"ref.observe"`, `"modified":"`, `"hash":"sha256:`} {
		if !strings.Contains(string(first), field) {
			t.Errorf("the log does not carry %s:\n%s", field, first)
		}
	}
	if runtime.GOOS == "darwin" && !strings.Contains(string(first), `"created":"`) {
		t.Errorf("darwin reports a birth time, but the observation has no created:\n%s", first)
	}

	code, stdout, stderr = session(t, "ref", "observe", id)
	if code != exitOK {
		t.Fatalf("re-observe: exit %d, %s", code, stderr)
	}
	if got := logBytes(t, dir); string(got) != string(first) {
		t.Errorf("re-observing an unchanged file appended %d bytes", len(got)-len(first))
	}
	if !strings.Contains(stdout, "unchanged  "+ref) {
		t.Errorf("an unchanged file was not reported as such:\n%s", stdout)
	}

	// Content change → exactly one more observation, with a different hash.
	if err := os.WriteFile(path, []byte("second draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := session(t, "ref", "observe", id); code != exitOK {
		t.Fatalf("observe after edit: exit %d, %s", code, stderr)
	}
	after := logBytes(t, dir)
	if n := strings.Count(string(after), `"ev":"ref.observe"`); n != 2 {
		t.Errorf("expected exactly 2 observations after one edit, found %d", n)
	}
}

// Unobservable schemes are reported and skipped, never an error: a node
// holding branch: and file: refs must not fail because only one is observable
// (SPEC.md §9.3). gh:/pr: name their remedy, since one exists.
func TestRefObserveSkipsWhatItCannotObserve(t *testing.T) {
	_, id, _, ref := observeFixture(t)
	for _, extra := range []string{"gh:acme/api#412", "branch:fix/n-plus-1"} {
		if code, _, stderr := session(t, "ref", "add", id, extra); code != exitOK {
			t.Fatalf("ref add %s: exit %d, %s", extra, code, stderr)
		}
	}

	code, stdout, stderr := session(t, "ref", "observe", id)
	if code != exitOK {
		t.Fatalf("observe: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "observed   "+ref) {
		t.Errorf("the observable ref was not observed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "skipped    gh:acme/api#412  (weft import observes gh: and pr:)") {
		t.Errorf("the gh: skip does not name its remedy:\n%s", stdout)
	}
	if !strings.Contains(stdout, "skipped    branch:fix/n-plus-1") {
		t.Errorf("the branch: ref was not reported as skipped:\n%s", stdout)
	}
}

// A missing file is a normal state — the path may live on another machine or
// an unchecked-out branch — reported so silence cannot read as observed.
func TestRefObserveReportsAMissingFile(t *testing.T) {
	dir := scratch(t)
	code, stdout, _ := session(t, "add", "gone", "--kind", "note", "--ref", "file:/no/such/file.md")
	if code != exitOK {
		t.Fatal("add failed")
	}
	id := strings.Fields(stdout)[0]

	before := logBytes(t, dir)
	code, stdout, stderr := session(t, "ref", "observe", id)
	if code != exitOK {
		t.Fatalf("observe: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "skipped    file:/no/such/file.md  (no such file)") {
		t.Errorf("the missing file was not reported:\n%s", stdout)
	}
	if got := logBytes(t, dir); string(got) != string(before) {
		t.Error("a missing file wrote an observation")
	}
}

// Naming a ref narrows the run to it; naming one the node does not carry is
// refused, because the fold keys observations under the node's refs and an
// unbound observation lands in a slot no query reads — written and invisible.
func TestRefObserveNamedRef(t *testing.T) {
	_, id, _, ref := observeFixture(t)
	if code, _, stderr := session(t, "ref", "add", id, "branch:main"); code != exitOK {
		t.Fatalf("ref add: exit %d, %s", code, stderr)
	}

	code, stdout, _ := session(t, "ref", "observe", id, ref)
	if code != exitOK {
		t.Fatal("observe of a bound ref failed")
	}
	if strings.Contains(stdout, "branch:main") {
		t.Errorf("a named run reported refs it was not asked about:\n%s", stdout)
	}

	code, _, stderr := session(t, "ref", "observe", id, "file:elsewhere.md")
	if code != exitUsage {
		t.Errorf("observing an unbound ref exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "not bound") || !strings.Contains(stderr, "weft ref add") {
		t.Errorf("the refusal does not name the remedy:\n%s", stderr)
	}
}

// file:p#L10 names a place in a file; the artifact observed is the file.
func TestRefObserveStripsTheLineFragment(t *testing.T) {
	scratch(t)
	path := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(path, []byte("line one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := session(t, "add", "a line in a spec", "--kind", "note", "--ref", "file:"+path+"#L1")
	if code != exitOK {
		t.Fatal("add failed")
	}
	id := strings.Fields(stdout)[0]

	code, stdout, stderr := session(t, "ref", "observe", id)
	if code != exitOK {
		t.Fatalf("observe: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "observed   file:"+path+"#L1") {
		t.Errorf("the fragment ref was not observed:\n%s", stdout)
	}
}

// Both clocks render as ages, never absolutes (SPEC.md §6.3): "modified" is
// the artifact's own clock, "observed" is weft's, and the hash shows as a
// glanceable prefix rather than 64 hex characters.
func TestShowRendersBothClocks(t *testing.T) {
	_, id, _, _ := observeFixture(t)
	if code, _, stderr := session(t, "ref", "observe", id); code != exitOK {
		t.Fatalf("observe: exit %d, %s", code, stderr)
	}

	code, stdout, stderr := session(t, "show", id)
	if code != exitOK {
		t.Fatalf("show: exit %d, %s", code, stderr)
	}
	for _, want := range []string{"sha256:", "…", "(modified ", ", observed "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show is missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "sha256:"+strings.Repeat("0", 20)) || len(stdout) > 0 && strings.Count(stdout, "sha256:") != 1 {
		t.Errorf("hash rendering is off:\n%s", stdout)
	}

	// And the machine form carries the payload verbatim, absolutes included —
	// ages are for humans, and a consumer re-deriving a timestamp from "2d ago"
	// would be the written-but-unreadable `source` shape.
	code, js, _ := session(t, "show", id, "--json")
	if code != exitOK {
		t.Fatal("show --json failed")
	}
	for _, want := range []string{`"modified"`, `"hash"`, `"sha256:`} {
		if !strings.Contains(js, want) {
			t.Errorf("show --json is missing %s:\n%s", want, js)
		}
	}
}

func TestRefObserveJSON(t *testing.T) {
	_, id, _, ref := observeFixture(t)
	code, stdout, stderr := session(t, "ref", "observe", id, "--json")
	if code != exitOK {
		t.Fatalf("observe --json: exit %d, %s", code, stderr)
	}
	for _, want := range []string{`"id"`, `"observed"`, ref} {
		if !strings.Contains(stdout, want) {
			t.Errorf("json output missing %s:\n%s", want, stdout)
		}
	}
}

// A relative file: ref resolves against the checkout root, so a run from a
// subdirectory reads the same file as a run from the top.
func TestRefObserveResolvesRelativeToTheCheckoutRoot(t *testing.T) {
	scratch(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "plan.md"), []byte("plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ := session(t, "add", "the plan", "--kind", "note", "--ref", "file:docs/plan.md")
	id := strings.Fields(out)[0]

	t.Chdir(filepath.Join(root, "docs", "deep"))
	code, stdout, stderr := session(t, "ref", "observe", id)
	if code != exitOK || !strings.Contains(stdout, "observed   file:docs/plan.md") {
		t.Fatalf("observe from a subdirectory: exit %d\n%s%s", code, stdout, stderr)
	}
}

// The store is global: from another project's checkout, a relative path would
// name that project's file. It must be skipped, not observed.
func TestRefObserveSkipsARelativePathFromAnotherNamespace(t *testing.T) {
	scratch(t)
	_, out, _ := session(t, "add", "the readme", "--kind", "note", "--ref", "file:README.md")
	id := strings.Fields(out)[0]

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("someone else's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("WEFT_NS", "other/project")
	code, stdout, stderr := session(t, "ref", "observe", id)
	if code != exitOK {
		t.Fatalf("observe: exit %d, %s", code, stderr)
	}
	if strings.Contains(stdout, "observed") || !strings.Contains(stdout, "relative path; run from a checkout of") {
		t.Errorf("a relative path from another namespace must be skipped:\n%s", stdout)
	}
}

// A file that exists but cannot be read is not missing; saying "no such file"
// invites a rebind that records a false address change.
func TestRefObserveReportsUnreadableAsUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	scratch(t)
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locked, "plan.md")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	_, out, _ := session(t, "add", "locked plan", "--kind", "note", "--ref", "file:"+path)
	id := strings.Fields(out)[0]

	_, stdout, _ := session(t, "ref", "observe", id)
	if strings.Contains(stdout, "no such file") || !strings.Contains(stdout, "unreadable") {
		t.Errorf("an unreadable file must not read as missing:\n%s", stdout)
	}
}
