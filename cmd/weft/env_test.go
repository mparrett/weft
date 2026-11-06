package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The store path had no command at all, so the only way to find out where
// writes were landing was to make one and look. That is a question people stop
// asking, and getting it wrong is silent and durable.
func TestEnvReportsTheStoreAndWhy(t *testing.T) {
	dir := scratch(t)
	session(t, "add", "something")

	code, out, stderr := session(t, "env")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("env did not name the store in use:\n%s", out)
	}
	// The rule matters as much as the path: the same path means different
	// things depending on whether WEFT_HOME is following you between projects.
	if !strings.Contains(out, "WEFT_HOME") {
		t.Errorf("env did not say which rule chose the store:\n%s", out)
	}
	if !strings.Contains(out, "1 event") {
		t.Errorf("env did not report the log size:\n%s", out)
	}
	for _, want := range []string{"actor", "ns", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("env omitted %q:\n%s", want, out)
		}
	}
}

// A variable that looks plausible and is read by nothing is indistinguishable
// from one that works. This is the whole reason the command exists: WEFT_STORE
// was set for a whole session, silently ignored, and the writes went to the
// real store.
func TestEnvNamesVariablesItDoesNotRead(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_STORE", "/tmp/nowhere")

	_, _, stderr := session(t, "env")
	if !strings.Contains(stderr, "WEFT_STORE") {
		t.Errorf("an unread WEFT_ variable went unreported:\n%s", stderr)
	}
	// Naming the mistake without naming the alternative leaves the reader
	// exactly where they started.
	for _, want := range []string{"WEFT_HOME", "WEFT_NS", "WEFT_ACTOR"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the warning did not say weft reads %s:\n%s", want, stderr)
		}
	}
}

// The tools are part of weft even though they are outside the build, so a
// variable one of them reads must not be reported as a mistake.
func TestEnvDoesNotWarnAboutVariablesItKnows(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_MERMAID_JS", "/tmp/mermaid.min.js")

	if _, _, stderr := session(t, "env"); strings.Contains(stderr, "not read by weft") {
		t.Errorf("a known variable was reported as unread:\n%s", stderr)
	}
}

// env answers a question about the store, so it must not change the answer by
// being asked. Nothing here may create the directory or the log.
func TestEnvWritesNothing(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "not-created-yet")
	t.Setenv("WEFT_HOME", missing)
	t.Setenv("WEFT_ACTOR", "agent:test")
	t.Setenv("WEFT_NS", "default")

	code, out, stderr := session(t, "env")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("env created the store directory; it must only report")
	}
	// And it says so, rather than claiming an empty log that is not there.
	if !strings.Contains(out, "not written yet") {
		t.Errorf("an absent log was not reported as absent:\n%s", out)
	}
}

// A crash mid-append leaves a fragment with no newline. The fold drops it
// (store.foldBytes discards the last element, EVENTS.md §5.3), so counting it
// would make env report more events than any other command can see — and it
// would do so precisely when something has gone wrong and somebody is checking.
func TestEnvCountsOnlyCommittedEvents(t *testing.T) {
	dir := scratch(t)
	session(t, "add", "one")
	session(t, "add", "two")

	logPath := filepath.Join(dir, "log.jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// A torn write: valid JSON so far, no terminating newline.
	if _, err := f.WriteString(`{"v":1,"kind":"node.add","ns":"default"`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, out, _ := session(t, "env")
	if !strings.Contains(out, "2 events") {
		t.Errorf("env counted an uncommitted fragment; the fold does not:\n%s", out)
	}
	// And the rest of weft still reads the store, so this is a live condition
	// rather than a corrupt one.
	if code, _, _ := session(t, "ls"); code != exitOK {
		t.Errorf("a trailing fragment should not break ls: exit %d", code)
	}
}

// The tools are part of weft, so a variable one of them reads must not be
// reported as a mistake — a false alarm is the same failure the warning exists
// to prevent, pointed the other way.
func TestEnvKnowsTheToolsVariables(t *testing.T) {
	scratch(t)
	t.Setenv("WEFT_BIN", "/usr/local/bin/weft")
	t.Setenv("WEFT_MERMAID_JS", "/tmp/mermaid.min.js")

	if _, _, stderr := session(t, "env"); strings.Contains(stderr, "not read by weft") {
		t.Errorf("a variable the tools read was reported as unread:\n%s", stderr)
	}
}

func TestEnvJSONKeepsStdoutParseable(t *testing.T) {
	dir := scratch(t)
	t.Setenv("WEFT_STORE", "/tmp/nowhere")

	code, out, stderr := session(t, "env", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got struct {
		Version   string   `json:"version"`
		Store     string   `json:"store"`
		StoreFrom string   `json:"store_from"`
		Actor     string   `json:"actor"`
		NS        string   `json:"ns"`
		Unread    []string `json:"unread_env"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, out)
	}
	if got.Store != dir || got.StoreFrom != "WEFT_HOME" {
		t.Errorf("store not reported: %+v", got)
	}
	// The warning has to reach a --json caller too, and on stderr so it cannot
	// contaminate the payload.
	if len(got.Unread) != 1 || got.Unread[0] != "WEFT_STORE" {
		t.Errorf("unread variables missing from the payload: %+v", got)
	}
	if !strings.Contains(stderr, "WEFT_STORE") {
		t.Errorf("--json swallowed the warning:\n%s", stderr)
	}
}

func TestEnvUsageErrors(t *testing.T) {
	scratch(t)
	if code, _, _ := session(t, "env", "extra"); code != exitUsage {
		t.Errorf("a stray argument was accepted")
	}
}
