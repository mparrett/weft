package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The milestone 10 artifact has to stay in step with the tool by construction,
// not by anyone remembering. Every drift here is silent: the snippet keeps
// looking authoritative while telling an agent to run something that no longer
// works, or omitting something it now needs.

func TestHelpAgentPrintsTheSnippet(t *testing.T) {
	code, stdout, _ := session(t, "help", "agent")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(stdout, "# weft, for agents") {
		t.Errorf("help agent did not print the snippet:\n%.120s", stdout)
	}
	// It is the thing you paste, so it has to arrive whole.
	if len(stdout) < 2000 {
		t.Errorf("snippet is %d bytes; suspiciously short", len(stdout))
	}
}

// Every command must appear, or an agent given only the snippet cannot reach it.
func TestSnippetCoversEveryCommand(t *testing.T) {
	for name := range commands {
		if !strings.Contains(agentDoc, "weft "+name) {
			t.Errorf("command %q is not in the agent snippet", name)
		}
	}
}

// Every exit code must appear with its meaning. Agents branch on these, so a
// code the snippet omits is one an agent will treat as a generic failure.
func TestSnippetCoversEveryExitCode(t *testing.T) {
	codes := map[string]int{
		"success": exitOK, "usage": exitUsage, "not found": exitNotFound,
		"ambiguous": exitAmbiguous, "rejected": exitRejected,
		"store": exitStore, "external": exitExternal,
	}
	table := agentDoc[strings.Index(agentDoc, "| Code |"):]
	for label, code := range codes {
		if !strings.Contains(table, "| "+strconv.Itoa(code)+" |") {
			t.Errorf("exit code %d (%s) is missing from the snippet's table", code, label)
		}
	}
	// exitNotImplemented is deliberately absent: no command returns it now
	// (main_test.go pins the helper), and documenting a code an agent will
	// never see is noise in the artifact that most needs to stay short.
}

// Every node kind and edge kind the CLI accepts must be listed, since the
// snippet is the only place an agent learns what the vocabulary is.
func TestSnippetCoversEveryKind(t *testing.T) {
	for _, k := range validKinds {
		if !strings.Contains(agentDoc, k) {
			t.Errorf("node kind %q is missing from the snippet", k)
		}
	}
	for _, k := range validEdgeKinds {
		if !strings.Contains(agentDoc, k) {
			t.Errorf("edge kind %q is missing from the snippet", k)
		}
	}
}

// Every flag the snippet advertises must actually parse. A snippet that teaches
// a flag the binary rejects is worse than one that omits it: the agent follows
// it, gets exit 1, and concludes the tool is broken.
func TestSnippetFlagsAllExist(t *testing.T) {
	scratch(t)
	// Flags as written in the snippet, paired with a command that should take
	// them. Parsed out of the prose rather than hardcoded, so adding a flag to
	// the snippet without adding it to the tool fails here.
	// Preceded by a space or backtick, so the markdown table rules (|---|) are
	// not mistaken for flags.
	// Digits are allowed after the first letter, or --d2 reads as --d.
	flagRE := regexp.MustCompile("[ `](--[a-z][a-z0-9-]*)")
	// A flag can belong to more than one command; every named command is
	// checked, or a flag added to a second command goes unverified while the
	// snippet teaches it.
	known := map[string]string{
		"--kind": "ls", "--ns": "ls", "--all-ns": "ls", "--ready": "ls",
		"--blocked": "ls", "--mine": "ls", "--stale": "ls", "--json": "ls",
		"--since": "ls", "--recent": "ls",
		"--ref": "add", "--source": "add", "--if-absent": "add,link", "--holder": "claim", "--note": "claim",
		"--even-dropped": "add", "--if-present": "done,drop",
		"--ttl": "claim", "--undo": "done", "--limit": "import", "--help": "",
		"--via": "path", "--dir": "path", "--merged-since": "import",
		"--include": "export", "--d2": "export",
		"--mermaid-js": "view", "--self-contained": "view", "--only-ns": "view", "--open": "view",
	}
	for _, m := range flagRE.FindAllStringSubmatch(agentDoc, -1) {
		f := m[1]
		cmd, ok := known[f]
		if !ok {
			t.Errorf("snippet advertises %s, which this test does not know how to check", f)
			continue
		}
		if cmd == "" {
			continue
		}
		for _, name := range strings.Split(cmd, ",") {
			// -h on the subcommand's FlagSet lists its flags; an unknown flag
			// there is a usage error, which is what we are ruling out.
			if code, _, stderr := session(t, name, f, "-h"); code == exitUsage &&
				strings.Contains(stderr, "not defined") {
				t.Errorf("snippet advertises %s for `weft %s`, but it does not exist:\n%s", f, name, stderr)
			}
		}
	}
}

// Every command line the snippet shows must be accepted by the binary.
//
// Placeholders like <id> resolve to nothing, so exit 2 is fine — what is being
// ruled out is exit 1, which means the snippet is teaching a grammar the tool
// rejects. An agent that follows the snippet and gets a usage error concludes
// the tool is broken and stops using it, which is the failure milestone 10
// exists to prevent.
func TestEverySnippetCommandParses(t *testing.T) {
	scratch(t)
	installGH(t, &fakeGH{}) // import must not reach the network

	fence := regexp.MustCompile("(?s)```\n(.*?)```")
	var ran int
	for _, block := range fence.FindAllStringSubmatch(agentDoc, -1) {
		for _, line := range strings.Split(block[1], "\n") {
			// Drop trailing "# commentary" and skip blanks.
			if i := strings.Index(line, "#"); i >= 0 && !strings.Contains(line[:i], "gh:") {
				line = line[:i]
			}
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "weft ") {
				continue
			}
			args := snippetArgs(line)
			if args == nil {
				continue
			}
			ran++
			code, _, stderr := session(t, args...)
			if code == exitUsage {
				t.Errorf("snippet line %q is a usage error:\n%s", line, stderr)
			}
		}
	}
	if ran < 15 {
		t.Errorf("only %d snippet commands were exercised; the parser probably missed a block", ran)
	}
}

// snippetArgs turns a documented line into argv, expanding the alternation the
// snippet uses for readability. Returns nil for a line that is illustrative
// rather than runnable.
func snippetArgs(line string) []string {
	fields := strings.Fields(strings.TrimPrefix(line, "weft "))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "[") || strings.HasSuffix(f, "]"):
			// Optional-argument notation; the required form is what gets run.
			return out
		case strings.Contains(f, "|") && strings.HasPrefix(f, "--"):
			out = append(out, strings.SplitN(f, "|", 2)[0])
		case strings.Contains(f, "|"):
			out = append(out, strings.SplitN(f, "|", 2)[0])
		case strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">"):
			// A placeholder resolves to nothing, which is exit 2, not exit 1.
			out = append(out, "ffffffffff")
		case strings.HasPrefix(f, `"`):
			out = append(out, strings.Trim(f, `"`))
		default:
			out = append(out, f)
		}
	}
	return out
}

// Everything above checks what the snippet *says*. Nothing checks what it
// leaves out, which is how the viewer stayed invisible while an agent read this
// file, concluded that drawing the graph was its own job, and wrote a worse
// renderer against the same JSON. Pinning one mention is narrow on purpose —
// this is the omission that actually cost a round trip, not a general claim
// that the snippet is complete.
func TestSnippetNamesTheViewer(t *testing.T) {
	if !strings.Contains(agentDoc, "`weft view`") {
		t.Error("the snippet does not mention `weft view`, so an agent reading only the snippet writes its own renderer")
	}
	// The mention must carry its precondition. The default page needs the
	// network, and --self-contained refuses to run without a local
	// mermaid.min.js (deliberately unvendored — BUILD.md §2's layout note,
	// backed by §1.1's dependency policy), and the snippet's reader is
	// by construction somewhere without the weft README — a firm "do not write
	// your own renderer" followed by a hard stop reads as "the tool is
	// broken", and the likely recovery is exactly the hand-rolled renderer the
	// paragraph exists to prevent (agent.md once sent agents to weft-html.py without
	// mentioning it needs a local mermaid.min.js).
	if !strings.Contains(agentDoc, "mermaid.min.js") {
		t.Error("the snippet does not name the local mermaid.min.js requirement, so the tool's stop on a fresh machine reads as breakage")
	}
	// The requirement without the way out is that agent.md omission restated,
	// so the recovery flag is pinned too — a rewrite that keeps the hard stop
	// and drops the escape hatch must fail here, not pass.
	if !strings.Contains(agentDoc, "--mermaid-js") {
		t.Error("the snippet names the mermaid requirement but not the --mermaid-js recovery")
	}
	// The requirement belongs to the offline mode, so the flag that selects it
	// is pinned as well; without it the snippet explains a stop nobody can
	// reach on purpose.
	if !strings.Contains(agentDoc, "--self-contained") {
		t.Error("the snippet does not name --self-contained, the offline mode")
	}
}
