// Package actor resolves the identity that asserts an event (docs/EVENTS.md §4).
package actor

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolve returns the asserting actor, first match wins:
//
//  1. $WEFT_ACTOR — the mechanism agents use
//  2. actor= in the store config file
//  3. user: + git config user.email
//  4. user: + $USER
//
// The literal "me" is CLI sugar and is resolved here; it never reaches the log.
func Resolve(storeDir string) string {
	name, _ := ResolveFrom(storeDir)
	return name
}

// ResolveFrom is Resolve plus the rule that chose the identity. Every event
// carries `by`, so an actor resolved from an unexpected rule mislabels the
// whole session's provenance — and the four rules are indistinguishable from
// the name alone.
func ResolveFrom(storeDir string) (name, source string) {
	if a := strings.TrimSpace(os.Getenv("WEFT_ACTOR")); a != "" {
		return a, "WEFT_ACTOR"
	}
	if a := fromConfig(storeDir); a != "" {
		return a, "config"
	}
	if a := gitEmail(); a != "" {
		return "user:" + a, "git config user.email"
	}
	if u := os.Getenv("USER"); u != "" {
		return "user:" + u, "$USER"
	}
	return "user:unknown", "fallback"
}

// fromConfig reads actor= from <storeDir>/config, a minimal key=value file.
// A missing or malformed config is not an error — it just does not match.
func fromConfig(storeDir string) string {
	f, err := os.Open(filepath.Join(storeDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == "actor" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func gitEmail() string {
	out, err := exec.Command("git", "config", "user.email").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
