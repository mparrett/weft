#!/usr/bin/env bash
# Rung 2 of the oracle (BUILD.md §4): drive the pipeline end to end.
#
# Stages that exit 3 (not implemented) are reported PENDING and do not fail the
# run — that is what makes this a build queue that is also green in CI from the
# first commit. Any other nonzero exit is a real failure.
set -uo pipefail

BIN=${BIN:-./dist/weft}
WEFT_HOME=$(mktemp -d)
export WEFT_HOME
export XDG_DATA_HOME="$WEFT_HOME"
trap 'rm -rf "$WEFT_HOME"' EXIT

failed=0
pending=0

# stage NAME -- CMD...
stage() {
  local name=$1; shift
  [ "${1:-}" = "--" ] && shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  case $rc in
    0) printf '  ok      %s\n' "$name" ;;
    3) printf '  pending %s\n' "$name"; pending=$((pending + 1)) ;;
    *) printf '  FAIL    %s (exit %d)\n%s\n' "$name" "$rc" "$out"; failed=$((failed + 1)) ;;
  esac
}

echo "smoke: $BIN"

export WEFT_ACTOR=user:smoke
# Pinned so smoke does not depend on the git remote of the checkout it runs in.
export WEFT_NS=smoke

stage "binary runs"   -- "$BIN" --version
stage "agent snippet" -- "$BIN" help agent
stage "usage"         -- "$BIN" --help
stage "add node"      -- "$BIN" add "smoke test node" --kind task
# Two refs on one node: the code anchor a reader wants and a stable key that
# survives it moving. Both have to resolve, or the second was not really bound.
stage "add with flags after positional" -- "$BIN" add "second node" --kind bug --source inferred --ref smoke:ref --ref smoke:alt
stage "ls nodes"      -- "$BIN" ls
stage "ls json"       -- "$BIN" ls --json
# Resolve a real id rather than guessing a prefix: whether a literal "a" happens
# to match depends on hashes, which would make this stage flaky. Reading the
# first column back is safe precisely because §9.5 makes the output canonical.
NODE=$("$BIN" ls 2>/dev/null | head -1 | awk '{print $1}')
stage "show node"     -- "$BIN" show "$NODE"
stage "show by ref"   -- "$BIN" show "smoke:ref"
stage "show by 2nd ref" -- "$BIN" show "smoke:alt"
# Binding after the fact is the half of the ref story `add` cannot cover.
stage "ref add"       -- "$BIN" ref add "$NODE" smoke:late
stage "show by late ref" -- "$BIN" show "smoke:late"
stage "ref add again" -- "$BIN" ref add "$NODE" smoke:late
stage "ref rm"        -- "$BIN" ref rm "$NODE" smoke:late
stage "ref rm again"  -- "$BIN" ref rm "$NODE" smoke:late

OTHER=$("$BIN" ls 2>/dev/null | tail -1 | awk '{print $1}')
stage "link edge"     -- "$BIN" link "$NODE" depends-on "$OTHER"
stage "show neighborhood" -- "$BIN" show "$NODE"

stage "claim node"    -- "$BIN" claim "$NODE"
stage "ls mine"       -- "$BIN" ls --mine
stage "release node"  -- "$BIN" release "$NODE"
stage "done node"     -- "$BIN" done "$OTHER"
stage "why"           -- "$BIN" why "$NODE"
stage "ls ready"      -- "$BIN" ls --ready
stage "ls blocked"    -- "$BIN" ls --blocked
stage "rdeps"         -- "$BIN" rdeps "$OTHER"
stage "path"          -- "$BIN" path "$NODE" "$OTHER"
stage "path via"      -- "$BIN" path "$NODE" "$OTHER" --via depends-on,part-of

# A cycle must be refused. Exit 5 is success here, so it is checked inline
# rather than through stage(), which treats any nonzero as failure.
if "$BIN" link "$OTHER" depends-on "$NODE" >/dev/null 2>&1; then
  printf '  FAIL    cycle rejection: the reverse edge was accepted\n'
  failed=$((failed + 1))
else
  printf '  ok      cycle rejected\n'
fi

stage "unlink edge"   -- "$BIN" unlink "$NODE" depends-on "$OTHER"
# Retracting what is not there is the desired state already holding, so it is
# exit 0 and writes nothing (EVENTS.md §3.3).
stage "unlink absent" -- "$BIN" unlink "$NODE" depends-on "$OTHER"
# The direction refused above must now be accepted. An unlink that dropped the
# edge from the indexes but left its triple key would pass every stage above
# this one and fail here, which is the whole reason to re-link rather than to
# re-read.
stage "relink freed"  -- "$BIN" link "$OTHER" depends-on "$NODE"

# Beyond exit status: the round trip has to actually round-trip, since every
# stage above would pass on a binary that silently wrote nothing.
if [ "$("$BIN" ls 2>/dev/null | wc -l | tr -d ' ')" != "2" ]; then
  printf '  FAIL    round trip: expected 2 nodes from ls\n'
  failed=$((failed + 1))
else
  printf '  ok      round trip (2 nodes persisted across invocations)\n'
fi

# Merge runs after the round-trip assertion, on its own pair: folding a node
# leaves one node where there were two, which is the point.
M1=$("$BIN" add "merge survivor" --kind task 2>/dev/null | awk '{print $1}')
M2=$("$BIN" add "merge duplicate" --kind task --ref smoke:dup 2>/dev/null | awk '{print $1}')
stage "merge"         -- "$BIN" merge "$M1" "$M2"
# The merged id stays addressable, and its ref reaches the survivor.
stage "show merged id"  -- "$BIN" show "$M2"
stage "show merged ref" -- "$BIN" show "smoke:dup"
# import needs gh and a network, so only its hermetic half runs here. Rung 2 is
# meant to be fast and deterministic, and a live API call is a flake source; the
# fetch path is covered against a stubbed gh in cmd/weft/import_test.go.
if "$BIN" import gh not-a-repo >/dev/null 2>&1; then
  printf '  FAIL    import: a malformed target was accepted\n'
  failed=$((failed + 1))
else
  printf '  ok      import rejects a malformed target\n'
fi

if [ "$("$BIN" ls 2>/dev/null | wc -l | tr -d ' ')" != "3" ]; then
  printf '  FAIL    merge: expected 3 nodes listed after folding one of four\n'
  failed=$((failed + 1))
else
  printf '  ok      merge (one node where there were two)\n'
fi

echo
if [ "$failed" -gt 0 ]; then
  echo "smoke: $failed failed, $pending pending"
  exit 1
fi
echo "smoke: all implemented stages pass, $pending pending"
