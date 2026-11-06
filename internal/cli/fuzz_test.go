package cli

import (
	"flag"
	"sort"
	"strings"
	"testing"
)

// Permute rewrites argv before every command parses it, and its failures are
// quiet by construction: the command still runs, just on arguments the user did
// not write. BUILD.md §1.2 records the original form of that — a dropped --kind
// that made `add` succeed with the wrong node kind.

// splitArgs turns fuzzer bytes into an argv. NUL is the separator because it is
// the one byte a shell cannot put inside an argument, so every other byte stays
// available for the fuzzer to explore inside one.
func splitArgs(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

func fuzzFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("fuzz", flag.ContinueOnError)
	fs.String("kind", "", "takes a value")
	fs.String("ns", "", "takes a value")
	fs.Bool("json", false, "takes none")
	fs.Bool("ready", false, "takes none")
	return fs
}

// FuzzPermuteKeepsEveryArgument pins the property the whole rewrite rests on:
// reordering must not be losing or duplicating.
//
// This is what a dropped "--" was — the separator went in and did not come out,
// leaving the positional it protected to be read as a flag again.
func FuzzPermuteKeepsEveryArgument(f *testing.F) {
	f.Add([]byte("title\x00--kind\x00bug"))
	f.Add([]byte("--json\x00--\x00--kind"))
	f.Add([]byte("a\x00--\x00-b\x00--ns\x00x"))
	f.Add([]byte("-\x00--json"))
	f.Add([]byte("--kind=bug\x00title"))
	f.Add([]byte("--ns"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		args := splitArgs(raw)
		got := Permute(fuzzFlagSet(), args)

		if len(got) != len(args) {
			t.Fatalf("Permute changed the argument count: %d in, %d out\n%q\n%q",
				len(args), len(got), args, got)
		}
		in, out := append([]string(nil), args...), append([]string(nil), got...)
		sort.Strings(in)
		sort.Strings(out)
		for i := range in {
			if in[i] != out[i] {
				t.Fatalf("Permute changed the arguments themselves:\n in  %q\n out %q", args, got)
			}
		}
	})
}

// FuzzPermuteIsStable pins that permuting an already-permuted argv is a no-op.
// If it were not, the order a command sees would depend on how many times the
// rewrite had run, which is the kind of thing that only shows up once something
// else starts calling it twice.
func FuzzPermuteIsStable(f *testing.F) {
	f.Add([]byte("title\x00--kind\x00bug"))
	f.Add([]byte("--json\x00--\x00--kind"))
	f.Add([]byte("--kind\x00--json\x00title"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		args := splitArgs(raw)
		once := Permute(fuzzFlagSet(), args)
		twice := Permute(fuzzFlagSet(), once)
		if strings.Join(once, "\x00") != strings.Join(twice, "\x00") {
			t.Fatalf("Permute is not stable:\n in    %q\n once  %q\n twice %q", args, once, twice)
		}
	})
}
