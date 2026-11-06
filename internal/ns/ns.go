// Package ns resolves which namespace a command operates in.
//
// Namespace is a field on the node, never part of its identity (SPEC.md §5), so
// resolution only ever decides where *new* nodes land and which nodes a listing
// shows. It never affects traversal: a query that stopped at a namespace
// boundary would hide cross-repo blockers, which is the failure the whole
// filter-vs-traversal rule exists to prevent.
package ns

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Default applies when nothing else identifies the working context.
const Default = "default"

// Source names the rule that supplied a namespace. Reported so a surprising
// namespace can be traced to the rule that chose it: the mechanisms are
// invisible from the outside, and picking the wrong one is silent — work lands
// in the wrong project and nothing complains until someone goes looking.
type Source string

const (
	// There is no SourceFlag: --ns short-circuits resolution in the caller, so
	// this function is never reached with one and could not honestly report it.
	SourceEnv     Source = "WEFT_NS"    // the environment
	SourceConfig  Source = "config"     // a mapping in the store config
	SourceGit     Source = "git remote" // the working directory's origin
	SourceDefault Source = "default"    // nothing identified the context
)

// Resolve returns the namespace for a working directory. See ResolveFrom for
// the order; this drops the provenance for callers that only need the name.
func Resolve(cwd, storeDir string) string {
	n, _, _ := ResolveFrom(cwd, storeDir)
	return n
}

// ResolveFrom returns the namespace, the rule that supplied it, and that
// rule's detail — the matched prefix for a config mapping, the remote URL for
// git, empty otherwise. First match wins:
//
//  1. $WEFT_NS — the mechanism agents use
//  2. the longest matching path prefix in the store config
//  3. the git remote, as owner/repo
//  4. Default
//
// The config mapping comes before git deliberately: it exists to override a
// remote you disagree with, and an override that loses to the thing it is
// overriding would be useless.
func ResolveFrom(cwd, storeDir string) (string, Source, string) {
	if v := strings.TrimSpace(os.Getenv("WEFT_NS")); v != "" {
		return v, SourceEnv, ""
	}
	if v, path := fromConfig(cwd, storeDir); v != "" {
		return v, SourceConfig, path
	}
	if url := gitRemote(cwd); url != "" {
		if v := FromRemote(url); v != "" {
			return v, SourceGit, url
		}
	}
	return Default, SourceDefault, ""
}

// fromConfig reads `ns:<path> = <namespace>` lines, longest prefix winning, so
// a mapping on a parent directory covers everything beneath it while a deeper
// entry can still carve out an exception.
// It returns the matched prefix alongside the namespace, so a caller can say
// which line was responsible rather than only what it produced.
func fromConfig(cwd, storeDir string) (string, string) {
	f, err := os.Open(filepath.Join(storeDir, "config"))
	if err != nil {
		return "", ""
	}
	defer f.Close()

	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	abs = canonical(abs)

	best, bestPath, bestLen := "", "", -1
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		path, isNS := strings.CutPrefix(key, "ns:")
		if !isNS || value == "" {
			continue
		}
		// Matching uses the canonical form; reporting uses the line as written.
		// The detail exists so a wrong namespace points at the line responsible,
		// and naming a symlink target that appears nowhere in the file sends the
		// reader looking for text that is not there — in exactly the symlinked
		// workspace this canonicalization was added for.
		written := strings.TrimSuffix(filepath.Clean(strings.TrimSpace(path)), string(filepath.Separator))
		resolved := canonical(written)
		if under(abs, resolved) && len(resolved) > bestLen {
			best, bestPath, bestLen = value, written, len(resolved)
		}
	}
	return best, bestPath
}

// canonical resolves symlinks so that one directory resolves to one namespace
// however you reached it. os.Getwd returns the *logical* path when $PWD is set,
// so a shell that cd'd through a symlink and a tool that resolved the path
// first would otherwise match different config lines — the same directory
// getting two namespaces, decided by shell history. Both sides go through here
// so a mapping may be written either way.
//
// An unresolvable path falls back to itself: a mapping may name a directory
// that does not exist yet, and that is a normal state rather than an error
// (SPEC §9.3). Nothing is under a path that does not exist, so a literal
// leftover cannot match anything it should not.
func canonical(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// under reports whether path is dir or lives beneath it. A string prefix test
// would match /foo/barbecue against /foo/bar, so the separator is required.
func under(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// gitRemote returns the raw origin URL, left unparsed so a caller can report
// the remote it resolved from and not just the namespace it became.
func gitRemote(cwd string) string {
	cmd := exec.Command("git", "-C", cwd, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// FromRemote turns a git remote URL into a namespace: everything after the
// host, without a .git suffix.
//
// The host is dropped, so github.com/acme/api and an internal mirror of the
// same project share a namespace. That is usually what you want — it is one
// project — and `--ns` or a config mapping settles it when it is not.
func FromRemote(url string) string {
	if url == "" {
		return ""
	}
	path := url

	switch {
	case strings.Contains(path, "://"):
		// scheme://[user@]host/path
		if _, rest, ok := strings.Cut(path, "://"); ok {
			if _, after, hasSlash := strings.Cut(rest, "/"); hasSlash {
				path = after
			} else {
				return ""
			}
		}
	case strings.Contains(path, ":"):
		// scp-style: [user@]host:path
		if _, after, ok := strings.Cut(path, ":"); ok {
			path = after
		}
	default:
		return ""
	}

	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || strings.Contains(path, " ") {
		return ""
	}
	return path
}
