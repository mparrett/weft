package ns

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFromRemote(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/acme/api.git", "acme/api"},
		{"https://github.com/acme/api", "acme/api"},
		{"git@github.com:acme/api.git", "acme/api"},
		{"git@github.com:acme/api", "acme/api"},
		{"ssh://git@github.com/acme/api.git", "acme/api"},
		{"ssh://git@ssh.github.com:443/acme/api.git", "acme/api"},
		// Nested groups survive, since the whole path is the identity.
		{"https://gitlab.com/group/sub/repo.git", "group/sub/repo"},
		{"https://github.com/acme/api/", "acme/api"},
		// A local path is not a namespace.
		{"/srv/git/repo.git", ""},
		{"", ""},
		{"nonsense", ""},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := FromRemote(tc.url); got != tc.want {
				t.Errorf("FromRemote(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	storeDir := t.TempDir()
	work := t.TempDir()

	// Nothing configured, no git remote.
	t.Setenv("WEFT_NS", "")
	if got := Resolve(work, storeDir); got != Default {
		t.Errorf("bare directory = %q, want %q", got, Default)
	}

	// Config mapping applies.
	writeConfig(t, storeDir, "ns:"+work+" = from/config\n")
	if got := Resolve(work, storeDir); got != "from/config" {
		t.Errorf("config mapping = %q", got)
	}

	// The environment wins over everything, which is how agents pin a context.
	t.Setenv("WEFT_NS", "from/env")
	if got := Resolve(work, storeDir); got != "from/env" {
		t.Errorf("env override = %q", got)
	}
}

// A mapping on a parent directory covers what is beneath it, and a deeper entry
// carves out an exception.
func TestConfigLongestPrefixWins(t *testing.T) {
	t.Setenv("WEFT_NS", "")
	storeDir := t.TempDir()
	parent := t.TempDir()
	child := filepath.Join(parent, "sub", "deep")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, storeDir,
		"ns:"+parent+" = broad/mapping\n"+
			"ns:"+filepath.Join(parent, "sub")+" = narrow/mapping\n")

	if got := Resolve(parent, storeDir); got != "broad/mapping" {
		t.Errorf("parent = %q, want broad/mapping", got)
	}
	if got := Resolve(child, storeDir); got != "narrow/mapping" {
		t.Errorf("child = %q, want narrow/mapping", got)
	}
}

// A string prefix test would match /foo/barbecue against /foo/bar.
func TestConfigRequiresPathBoundary(t *testing.T) {
	t.Setenv("WEFT_NS", "")
	storeDir := t.TempDir()
	base := t.TempDir()
	bar := filepath.Join(base, "bar")
	barbecue := filepath.Join(base, "barbecue")
	for _, d := range []string{bar, barbecue} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(t, storeDir, "ns:"+bar+" = the/mapping\n")

	if got := Resolve(bar, storeDir); got != "the/mapping" {
		t.Errorf("exact directory = %q", got)
	}
	if got := Resolve(barbecue, storeDir); got == "the/mapping" {
		t.Error("a sibling sharing a name prefix must not match")
	}
}

// The exit condition: --ns defaults correctly from a git remote.
func TestResolveFromGitRemote(t *testing.T) {
	t.Setenv("WEFT_NS", "")
	storeDir := t.TempDir()
	repo := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "https://github.com/acme/api.git")

	if got := Resolve(repo, storeDir); got != "acme/api" {
		t.Errorf("git remote = %q, want acme/api", got)
	}

	// A config mapping overrides the remote — an override that lost to the
	// thing it overrides would be useless.
	writeConfig(t, storeDir, "ns:"+repo+" = my/override\n")
	if got := Resolve(repo, storeDir); got != "my/override" {
		t.Errorf("config should beat the remote, got %q", got)
	}
}

// A git repo with no origin falls through rather than erroring.
func TestGitRepoWithoutOrigin(t *testing.T) {
	t.Setenv("WEFT_NS", "")
	storeDir := t.TempDir()
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if got := Resolve(repo, storeDir); got != Default {
		t.Errorf("repo without origin = %q, want %q", got, Default)
	}
}

// A malformed config is not an error; it just does not match.
func TestMalformedConfigIsTolerated(t *testing.T) {
	t.Setenv("WEFT_NS", "")
	storeDir := t.TempDir()
	work := t.TempDir()
	writeConfig(t, storeDir, "# a comment\ngarbage without an equals\nns: = empty value\nactor = user:matt\n")

	if got := Resolve(work, storeDir); got != Default {
		t.Errorf("malformed config = %q, want %q", got, Default)
	}
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One directory, one namespace — however you reached it. os.Getwd returns the
// logical path when $PWD is set, so a shell that cd'd through a symlink and a
// tool that resolved the path first used to match different config lines: the
// same directory getting two namespaces, decided by shell history.
func TestSymlinkedPathResolvesToOneNamespace(t *testing.T) {
	store := t.TempDir()
	root := t.TempDir()

	real := filepath.Join(root, "real-repo")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "via-symlink")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Mapping written against the physical path; the logical path must match it.
	conf := "ns:" + real + " = the/answer\n"
	if err := os.WriteFile(filepath.Join(store, "config"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEFT_NS", "")

	for _, cwd := range []string{real, link} {
		if got := Resolve(cwd, store); got != "the/answer" {
			t.Errorf("Resolve(%q) = %q, want the/answer", cwd, got)
		}
	}

	// And the other way round: a mapping written against the symlink must still
	// match someone standing on the physical path.
	conf = "ns:" + link + " = the/answer\n"
	if err := os.WriteFile(filepath.Join(store, "config"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{real, link} {
		if got := Resolve(cwd, store); got != "the/answer" {
			t.Errorf("symlink-keyed mapping: Resolve(%q) = %q, want the/answer", cwd, got)
		}
	}
}

// A mapping may name a directory that does not exist yet — a repo not cloned.
// Resolution falls back to the literal path rather than treating it as an error
// (SPEC §9.3).
func TestConfigToleratesAPathThatDoesNotExist(t *testing.T) {
	store := t.TempDir()
	work := t.TempDir()
	conf := "ns:/nowhere/at/all = ghost\nns:" + work + " = real\n"
	if err := os.WriteFile(filepath.Join(store, "config"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEFT_NS", "")

	if got := Resolve(work, store); got != "real" {
		t.Errorf("got %q, want real", got)
	}
}
