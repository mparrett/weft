# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Current state

Milestones 0–11 done. Phase 1 is complete. add, ls, link, unlink, ref, show, claim, release, done, drop, why,
rdeps, path, merge, import, view, and `ls --ready|--blocked|--mine|--stale` all work end to end.
`weft view` embeds only the page template (`cmd/weft/view.html`), never
mermaid; `tools/weft-html.py` fills the same template, and
`TestViewMatchesWeftHTML` is what keeps the two fillers from drifting — a
change to one filler's substitution or scoping has to land in both.
Observations carry the artifact's own clock (`created`/`modified`) and a
content hash; `weft ref observe` stats and hashes `file:` refs, and
`tools/weft-timeline.py` lanes an export by artifact time. The two-clock rule
(SPEC §6.3): the artifact clock is for chronology, `observed at` is for trust,
and the artifact clock never enters `ls --since`/`--recent`.
Namespaces resolve from `$WEFT_NS`, a config path mapping, or the git remote.
SPEC §7 runs as an executable transcript in `cmd/weft/worked_example_test.go`;
only the `gh` shell-out is stubbed. The transcript and the test are kept in
step by hand, so changing one means changing the other — a review caught SPEC
advertising a `--by` flag that never existed.

`smoke.sh` reports **0 pending**, because every command is built. That is
correct, not a bug in the queue.

The agent onboarding snippet is **`cmd/weft/agent.md`** — embedded with
`go:embed`, printed by `weft help agent`, and symlinked as `docs/AGENT.md`. It
lives under `cmd/weft/` because `go:embed` cannot reach out of its package
directory and refuses symlinks, so the symlink has to point the other way.
Editing it is checked against the binary by `cmd/weft/agentdoc_test.go`: every
command, exit code, kind, and flag it names must exist, and every command line
it shows is executed.

`merge --undo` does **not** exist and its absence is deliberate — `EVENTS.md`
§3.7 records the cost. Undoing a merge needs replay, not bookkeeping, because
events that landed on the survivor after the merge have to go back to the node
they named.

Tests pin `WEFT_NS` in `scratch()`. Without it the suite depends on the git
remote of whatever directory it runs in, which passes in a bare checkout and
fails once an origin exists.

Two things here are easy to break by accident:

- **`smoke.sh` treats exit 3 as pending, not failure.** The pending count is the
  build queue and main stays green. Do not "fix" it by making unbuilt stages
  fail.
- **Flags may follow positionals** (`BUILD.md` §1.2). Go's `flag` stops at the
  first positional, so every command must parse via `cli.Permute` first.
  Skipping it makes `--kind` silently ignored — the command succeeds and does
  the wrong thing.
- **Timestamps carry sub-second precision** (`EVENTS.md` §2.5). Ids hash the
  record including `at`, and the fold treats a repeated id as a crash-retry, so
  second granularity silently discards two identical assertions made in the same
  second. Do not "tidy" `at` back to `time.RFC3339`.
- **Namespace filters select result roots; traversal never stops at a namespace
  boundary** (`SPEC.md` §5). Filtering before the walk is what would let
  `ls --ready` hide a cross-namespace blocker and call a node ready when it is
  not. `cmd/weft/ls.go` computes readiness over the whole graph *then* filters.
- **A batch of writes needs per-item timestamps.** A node id hashes the whole
  record and carries nothing unique to the item, so one `time.Now()` for a batch
  makes two same-titled items hash identically and the fold discards the second
  as a crash-retry (`EVENTS.md` §2.5). Two unrelated issues silently become one
  node, and the log carries a duplicate id. `weft add` never hit this because it
  writes once per process; anything bulk does.
- **A merge has to move everything the survivor now owns** — refs, edges,
  claims, *and* observations (`internal/graph/merge.go`). Each one missed fails
  silently and differently: a stranded observation makes `show` drop the age
  annotation and makes the next import report a change that never happened.
- **Import must diff against the fold, never lean on it to absorb repeats**
  (`EVENTS.md` §3.9). Re-importing unchanged upstream has to append *nothing* —
  byte-identical log, not just "no duplicate node". The fold tolerates a
  redundant `ref.add` by leaving the ref list unchanged, so a writer that
  re-emits blindly looks correct in every query while growing the log on every
  scheduled run. Tolerance on the read side is not idempotence on the write side.
- **Upstream is cached, never authoritative.** `ref.observe` records what
  GitHub said and when; the node's own `title` and `state` are weft's. `ready`,
  `blocked`, and `why` all key off `state`, so a "helpful" sync from upstream
  silently rewrites the frontier. Render observations with their age — stale
  data shown as current is worse than none (`SPEC.md` §6.3).
- **Every node reference in the fold canonicalizes through the alias map**
  (`internal/graph/merge.go`). After a merge, an event naming the merged node
  has to land on the survivor. Miss one case in `Graph.Apply` and that kind of
  assertion silently vanishes into a node nothing reads — the command still
  exits 0. `Ordered()` omits merged nodes; `IDs()` must keep them, or a prefix
  renders unambiguously and then fails to resolve.
- **Resolve *inside* `store.Mutate`, never before it.** Any command that
  validates against current state must resolve and check within the same fold
  the append lands on, or it reopens the race the transaction exists to close
  (`EVENTS.md` §6). `cmd/weft/link.go` is the reference shape.
- **After the transaction commits, nothing may change the exit code**
  (`BUILD.md` §5). Every mutating command re-folds afterwards for presentation,
  and that fold is a second read outside the lock. `add` is the one that could
  reach it on a corrupt log, because plain `add` resolves nothing and so its
  `Mutate` callback never folds. It exited 6 from a fold whose only job was
  picking the id display width. The caller reads 6 as "nothing landed" and
  retries, and plain `add` asserts a *new* node: one silent duplicate, found later
  by whoever has to `merge` it. Failing a write in the renderer is invisible in
  every query. Only `cmd/weft/add.go` enforces this so far; `BUILD.md` §5
  records why the same window is bounded, not closed, everywhere else.
- **Dropping an edge must release its triple key, not just the indexes**
  (`internal/graph/graph.go`, `dropEdge`). The fold collapses identical
  `(kind, from, to)` triples, so a key left behind makes the *next* `link` of
  that triple look like a duplicate and get ignored — `weft link` exits 0,
  prints the edge, and the graph holds nothing. Nothing else in the suite
  catches it; `TestAnEdgeCanBeRelinkedAfterUnlink` is what pins it. Edge lookup
  goes through `Graph.FindEdge` for the same reason: it keys the way `edge.add`
  does, including the endpoint-order normalization symmetric kinds rely on.
- **Ref ambiguity counts live nodes only, and the rule lives in one place.**
  `graph.PreferLive` is shared by `Resolve`'s ref arm and `import`'s collision
  check (`internal/graph/resolve.go`); a liveness check hand-rolled anywhere
  else is the copy that drifts. Two *dropped* holders of one ref are still
  ambiguous — deliberately, there is no live node to prefer — and id-prefix
  resolution never discounts dropped nodes. `merge` is the cure, not a bug.
- **Duplicate-ref notes are post-commit presentation, off the write lock.**
  `printRefNotes` runs after `store.Mutate` from a lock-free fold, on every
  path that writes a ref (`add`, the `--even-dropped` reuse, `ref add`,
  `import`'s twin note). Moving the check into the transaction reopens the
  in-lock fold cost the exit-6 fix removed; a fold error may suppress a note
  but must never block the write; and equivalence (`gh:`/`pr:` sibling) must never
  enter `Resolve` — the moment it does, refs have become identity, which
  SPEC §4.1 refuses.
- **`--even-dropped` binds missing refs on reuse; plain `--if-absent` must
  not.** The flag exists for re-runnable scripts that address by ref, and
  without the binding a same-titled node silently absorbs the assertion and
  the script dies later at the addressing layer — the dropped-node-resurrection
  shape. Plain
  `--if-absent`'s "refs are ignored, nothing appended" is the documented
  contract; the divergence is stated in SPEC §6.
- **Golden samples must carry `By`/`Source` on at least one node.** Every node
  folded from a valid log has both (`event.Parse` rejects their absence), so a
  sample without them pins a shape no real node has and leaves the provenance
  rendering — and export's `--include source` gate — invisible to every
  golden. That is how the by/source fold fix once shipped with goldens that
  proved nothing.
- **`rdeps` annotations key on kinds *crossed*, never requested**, and a row
  deeper than one hop names its parent — level-order output plus depth-indent
  means the row above it is not its parent, so a bare relation there has no
  valid referent. A row carries *every* walked kind linking it to its parent,
  sorted; reporting the traversal kind alone made row content depend on the
  order `--via` was typed.
- **The doc pages' pasted transcripts are compared by nothing.**
  `gettingstarted_test.go` / `advanced_test.go` execute every command and
  check exit codes only, so output drift is invisible to `make check` — one
  `show` block was wrong from the day its page landed. Any change that touches
  rendered output means re-verifying the pasted blocks by hand until
  replay-and-diff transcripts (as a `tools/test_*.py`) are built.

## Normative docs — read before writing code

1. **`docs/SPEC.md`** — what `weft` is. Data model, edge kinds, storage,
   CLI surface, phase 2 VM, design constraints, open questions. Ends with a
   lineage appendix recording the prior art each decision came from.
2. **`docs/EVENTS.md`** — **normative.** The byte format, event identity and
   hashing, provenance fields, corrections, fold semantics, and the write
   protocol. SPEC describes intent; this describes bytes and **wins on any
   conflict.**
3. **`docs/BUILD.md`** — stack, platform, module layout, line budget, milestone
   order, and the verification ladder ("the oracle").

`AGENTS.md` is a symlink to this file, so they cannot diverge.

`SPEC.md` stays implementation-agnostic; anything about Go belongs in
`BUILD.md`.

## What weft is

A durable, inspectable graph of **claims about work** — shared by humans and
many concurrent agents — driven by a CLI with no daemon and no server. Nodes
assert that a unit of work exists; most of them have no GitHub issue and never
will. That is the point: issue trackers can only name what already exists in
the tracker.

Phase 1 is the claims graph with zero compute. Phase 2 makes node bodies
suspendable computations. Phase 3 (resident process, IPC) is deferred and
nothing in phases 1–2 may assume it.

## Settled decisions — do not relitigate

These are decided and recorded with reasoning. Changing one means editing the
spec and saying why, not working around it.

- **The JSONL log is the contract, not the binary.** Other tools and agents
  consume it. This is why the language choice is reversible and why phase 1
  optimizes for time-to-useful (`BUILD.md` §1).
- **Acyclicity is per edge-kind, not global** (`SPEC.md` §4.2). `depends-on`
  must be a DAG; `conflicts-with` is symmetric and cyclic by nature. Do not add
  a global DAG check — it would reject valid data.
- **Namespace is a field on the node, never part of its identity**
  (`SPEC.md` §5). IDs are event-derived hashes and already globally unique, so
  cross-namespace edges need no special case. Note they are **event** identity,
  not content identity — two agents asserting the same title get two nodes, and
  deciding they are the same is `weft merge`, not a hash collision
  (`EVENTS.md` §2.2).
- **One global store**, not one per repo. Multi-project use is a requirement.
- **No snapshot and no index** until the `BUILD.md` §1.4 budget is exceeded.
  Measured at milestone 1: ~50 ms to fold 10k events, so the crossover is ~50k
  events or ~250 ms. The real constraint is that the fold stays **linear** — an
  O(n²) fold is a defect at any constant. Any cache added later must be
  deletable, never a second source of truth.
- **Provenance (`by`/`at`/`source`) on every node, edge, and claim**
  (`SPEC.md` §4.4). Roughly half a real graph is inference; a graph that cannot
  separate observed from guessed goes unread.
- **Claims are advisory and do not lock.** Two live claims on one node is a
  *reported condition*, not an error (`SPEC.md` §4.3).
- **Tolerance over validation** (`SPEC.md` §9.3). Unknown nodes, dangling refs,
  stale claims, and unverifiable third-party claims are normal states. Do not
  turn them into errors.
- **Near-zero dependencies** (`BUILD.md` §1.1). Every dependency is a cold-start
  tax paid on every invocation. `gh` is shelled out to, not wrapped.

## Two constraints that rot silently

Both are easy to break without anything failing, so check them deliberately:

- **Canonical printing** (`SPEC.md` §9.5). Identical graphs must print
  byte-identically — the text form is what humans review and what lands in git.
  Nothing breaks when node ordering drifts; the diffs just go noisy months
  later. Pinned by golden files over both the log and rendered output.
- **One-line writes and one-call reads** (`SPEC.md` §9.1, §9.4). If recording a
  blocker takes three commands, or orienting takes five, agents stop using the
  tool and the graph dies. Adoption is a design constraint, not a docs task —
  it is milestone 10 with an exit condition.

## Build order

`BUILD.md` §3. Milestones 1–4 get `add` / `link` / `show` working end to end
before any query command exists. Milestone 1 is concurrent append, not "write a
file" — N processes × M events must produce N·M valid lines, since that is the
one bug class that corrupts the durable artifact rather than returning a wrong
answer.

## Commands

```
make                # rung 1 — stamped build to dist/weft
make vet            # go vet plus a gofmt check
make smoke          # rung 2 — pending stages are fine, failures are not
make test           # rungs 3-5
make test-tools     # rung 6 — every tools/test_*.py; CI ran none of these until 2026-08-10
make check          # all of the above, in ladder order
```

**Build through the Makefile, not with a bare `go build`.** The version comes
from `-ldflags`, so a hand-rolled build reports `0.0.0-dev` and a modified tree
is stamped `-dirty` instead of claiming a commit it does not match. A dogfooding
run installed an unstamped binary this way and had to stamp it by hand.

`smoke.sh` treats exit 3 as *pending*, not failure. That is deliberate: the
pending count is the build queue, and main stays green. Do not "fix" it by
making unbuilt stages fail.

Exit codes are a stable contract (`BUILD.md` §5) — agents branch on them, and 3
in particular is load-bearing for smoke.

## Cursor Cloud specific instructions

The whole product is a single CLI binary — there is **no daemon, server, or
long-running service to start**. "Running the app" means building `dist/weft`
(via `make`) and driving it against a store. Verification is the ladder in the
Commands section (`make check` runs `vet`, `smoke`, `test`, `test-tools`);
prefer those over ad-hoc `go build`/`go test`.

- The toolchain (Go 1.25, `python3`, `make`, `git`, `gh`) is preinstalled by
  the environment; the startup update script only warms the Go module cache
  (`go mod download`). There is nothing else to install.
- For an isolated, reproducible run, point `WEFT_HOME` at a fresh tempdir and
  pin `WEFT_NS` and `WEFT_ACTOR` — the same thing the test `scratch()` helper
  does so results don't depend on the checkout's git remote.
- **`weft add` prints a formatted line** (`<id>  <title>  [ns]`), not a bare id.
  To script against the new node, take the first whitespace field for the id
  (this is what `smoke.sh` does); passing the whole line to `link`/`show`/etc.
  fails to resolve.
- **When a task references an issue, open the issue on GitHub and read the
  body — do not work from a title or a relayed list.** `weft`'s issues carry the
  repro, the scope, and the *decision* in the body: often a prescribed fix and
  the obvious one it rejects. Verify the issue *number* too: a pasted
  list once arrived shifted by one, so the titles named real bugs under the
  wrong numbers.
- **If issue reads fail, it is a token-scope problem, not a missing issue.**
  The cloud `gh` credential is a GitHub App token; if it lacks Issues access,
  `gh issue view` / `gh api repos/.../issues/...` return `403 Resource not
  accessible by integration`, while `gh pr view` still works. The repo is
  public, so an unauthenticated read works as a fallback
  (`curl -s https://api.github.com/repos/mparrett/weft/issues/N`). The lasting
  fix is granting the Cursor GitHub App **Issues: Read**, or using a token with
  issues read from a secret (`GH_TOKEN=$SECRET gh api ...`).
