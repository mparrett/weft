package cli

import (
	"flag"
	"reflect"
	"testing"
)

func newFS() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("kind", "", "")
	fs.String("ns", "", "")
	fs.Bool("json", false, "")
	fs.Bool("all-ns", false, "")
	return fs
}

func TestPermute(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"already ordered", []string{"--kind", "bug", "title"}, []string{"--kind", "bug", "title"}},
		// The case that motivated the whole pass.
		{"flag after positional", []string{"title", "--kind", "bug"}, []string{"--kind", "bug", "title"}},
		{"bool flag does not eat the positional", []string{"--json", "title"}, []string{"--json", "title"}},
		{"bool flag after positional", []string{"title", "--json"}, []string{"--json", "title"}},
		{"interleaved", []string{"title", "--kind", "bug", "--json"}, []string{"--kind", "bug", "--json", "title"}},
		{"equals form", []string{"title", "--kind=bug"}, []string{"--kind=bug", "title"}},
		{"single dash", []string{"title", "-kind", "bug"}, []string{"-kind", "bug", "title"}},
		{"two positionals", []string{"a", "b", "--json"}, []string{"--json", "a", "b"}},
		// The separator survives. Dropping it leaves the protected positional
		// bare, so FlagSet.Parse reads it as a flag again and the escape hatch
		// protects nothing — `weft add -- "--json is broken"` used to be a usage
		// error, which meant no node could be named with a leading dash at all.
		{"double dash is kept", []string{"--json", "--", "--kind"}, []string{"--json", "--", "--kind"}},
		{"double dash after a positional", []string{"a", "--", "-b"}, []string{"--", "a", "-b"}},
		{"bare dash is positional", []string{"-", "--json"}, []string{"--json", "-"}},
		{"empty", []string{}, []string{}},
		{"positional only", []string{"a", "b"}, []string{"a", "b"}},
		// A value that looks like a flag still belongs to its flag.
		{"flag-like value", []string{"title", "--kind", "--weird"}, []string{"--kind", "--weird", "title"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Permute(newFS(), tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The point of permuting is that parsing then sees the flags.
func TestPermuteThenParse(t *testing.T) {
	fs := newFS()
	if err := fs.Parse(Permute(fs, []string{"my title", "--kind", "bug", "--json"})); err != nil {
		t.Fatal(err)
	}
	if got := fs.Lookup("kind").Value.String(); got != "bug" {
		t.Errorf("kind = %q, want bug", got)
	}
	if got := fs.Lookup("json").Value.String(); got != "true" {
		t.Errorf("json = %q, want true", got)
	}
	if got := fs.Args(); !reflect.DeepEqual(got, []string{"my title"}) {
		t.Errorf("positionals = %q", got)
	}
}

// Without permuting, the stdlib silently drops the trailing flag. This test
// documents the failure the pass exists to prevent.
func TestUnpermutedParseLosesFlags(t *testing.T) {
	fs := newFS()
	if err := fs.Parse([]string{"my title", "--kind", "bug"}); err != nil {
		t.Fatal(err)
	}
	if got := fs.Lookup("kind").Value.String(); got != "" {
		t.Fatalf("expected the stdlib to drop --kind, got %q; if this now parses, the permute pass may be removable", got)
	}
}

func TestUnknownFlagStillErrors(t *testing.T) {
	fs := newFS()
	fs.SetOutput(discard{})
	if err := fs.Parse(Permute(fs, []string{"title", "--nope"})); err == nil {
		t.Error("expected an unknown flag to be rejected")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
