# Stack, layout, build order, and the oracle

Companion to `SPEC.md`. The spec says what `weft` is; this says how it gets
built and how you know it works.

## 1. Stack: Go

**Decision: Go for v0.**

The argument that makes this low-stakes: **the JSONL log is the contract, not
the binary.** Other tools consume it, agents read it, it is the durable
artifact. So the implementation language is reversible in a way it usually is
not, and phase 1 should optimize for time-to-useful rather than for phase 2.

Reasons in order of weight:

1. Phase 1 is fold-a-log-and-walk-a-graph. Go's stdlib covers all of it.
2. Single static binary, no runtime, ~2–5 ms cold start. This matters more here
   than usual — every `weft show` is a process spawn, and at 10+ agents making
   several calls per turn you pay it constantly.
3. `let-go` is prior art in the same house for a bytecode VM in Go, so phase 2
   is not a leap into unfamiliar terrain.

**The flip condition, recorded so it can be checked later:** if phase 2 is
expected within roughly two months, start in Rust instead and accept a slower
phase 1. The phase-2 VM wants enum-tagged values and instructions and flat
arrays with an index rather than a pointer graph (SPEC appendix, dataflow
entry), which is Rust's home turf, and `apolisp` is live Rust VM experience. If
phase 1 is the whole story for six months, that is paying now for a maybe.

C is ruled out: `wallisp` needed freestanding wasm, and none of that applies.

### 1.1 Dependency policy

Near-zero. Every dependency is a cold-start tax paid on every invocation.

| Need | Choice | Why not otherwise |
|---|---|---|
| CLI parsing | stdlib `flag.FlagSet` per subcommand + a permute pass (§1.2) | cobra's init is a real fraction of a 5 ms budget, for a surface this small |
| JSON | stdlib `encoding/json` | — |
| File locking | `golang.org/x/sys/unix` | the one non-stdlib dependency worth taking |
| GitHub | `exec` out to `gh` | auth, pagination, and enterprise hosts already solved; ~20 lines instead of a dependency tree |
| Graph algorithms | hand-rolled, ~120 lines | BFS, reverse closure, cycle check, topological frontier — a library costs more in impedance than it saves |

### 1.2 CLI parsing: flags may follow positionals

Every example in `SPEC.md` is written as `weft add "title" --kind bug`, with
flags *after* the positional argument. Go's `flag` package stops parsing at the
first non-flag argument, so a naive implementation would silently ignore
`--kind` and every other trailing flag.

Silently, which is the problem — the command succeeds and does the wrong thing.

**Decision: a `flag.FlagSet` per subcommand, preceded by a permute pass** that
partitions `os.Args` into flags and positionals before parsing (roughly 30
lines, GNU `getopt`-style). Flags may appear anywhere.

Rejected: requiring flags first, which contradicts every documented example and
the one-line-write constraint (`SPEC.md` §9.1); and taking `spf13/pflag`, which
buys permutation at the cost of the dependency policy above.

The permute pass must exist before milestone 2, because the golden files from
that point on encode the accepted grammar.

### 1.3 Platform

**v0 targets macOS** (arm64 and amd64). Windows is out of scope.

**macOS is verified locally; CI runs Linux only.** Development happens on the
target platform, so every local `go test` and `smoke.sh` *is* the macOS check.
CI exists to catch what did not get run locally, and a Linux runner is
enough for that.

Linux is a **portability canary, not a support commitment** — a green CI run
promises nothing to anyone until Linux becomes a stated target.

The one thing this trades away, recorded so it is a decision and not an
oversight: a macOS-specific regression can only be caught before push. That
matters most at milestone 1, since `flock` semantics differ between Darwin and
Linux. Run the oracle locally before pushing; CI is not a substitute for it
here.

macOS-first plus the host-local scope (SPEC §2.1) is what makes the following
safe to assume, so they are assumptions with an expiry rather than facts:

- POSIX advisory locking via `golang.org/x/sys/unix`. No Windows path.
- One filesystem, one clock. See SPEC §2.1.
- Pure Go, `CGO_ENABLED=0`, so the Linux image is a cross-compile rather than a
  port.

Store location resolves `$XDG_DATA_HOME/weft/` first even on macOS — the
platform convention would be `~/Library/Application Support/`, but agents and
scripts expect XDG, and a path that differs per platform is a path that gets
hardcoded wrong. Falls back to `~/.weft/`.

### 1.4 Performance budget

The budget's original form — "`weft show` under 15 ms cold at 10,000 events" —
was **invented, not measured**. Milestone 1 measured it, and it was wrong.

Measured on an M2, 10,000 events (`BenchmarkFold10k`, `BenchmarkMutate10k`):

| Path | Cost |
|---|---:|
| Fold (read) | **~50 ms** |
| Mutate (fold + validate + append + fsync) | **~48 ms** |

Two defects surfaced getting there, both found by the benchmark rather than by
reading the code:

- Duplicate-triple detection scanned every edge on every `edge.add`, making the
  fold **O(n²)**: 847 ms at 10k events. Fixed with a triple index (`graph.go`).
- Envelope validation used a regexp for the id, at 10% of fold time. Replaced
  with a hand-rolled hex check.

847 ms → 50 ms. But 50 ms is still **3× the invented budget**, so the budget is
restated as measured rather than aspirational:

> `weft show` completes in **under 75 ms** against a 10,000-event log, cold,
> and the fold stays **linear** in log length.

Linearity is the real constraint; the constant is negotiable. An O(n²) fold is
a design defect at any constant, and it is what the benchmark exists to catch.

**This does not yet force an index.** At 10k events a command costs ~55 ms
including process start, which is imperceptible to a human and acceptable to an
agent. SPEC §5's "no snapshot, no index" therefore stands — but on measured
grounds now, with a stated crossover: revisit at **~50k events or ~250 ms**,
whichever comes first.

The write path is the one to watch. Every validating mutation folds the whole
log under an exclusive lock (`EVENTS.md` §6.1), so ~48 ms per write is also a
**ceiling of roughly 20 writes/second across all agents combined**, falling as
the log grows. That is fine for agents recording blockers; it would not be fine
for anything bulk, and `weft import` is the first command that might be.

Module path: `github.com/mparrett/weft`. Go 1.25+ — raised from 1.22 by
`golang.org/x/sys`, which sets that floor.

## 2. Layout

```
cmd/weft/          dispatch, flag parsing, exit codes
internal/event/    envelope, canonical JSON (RFC 8785 subset), event hashing
internal/store/    log path resolution, flock, append, fold
internal/graph/    materialized graph: nodes, edges, claims, prefix resolution
internal/query/    ready, blocked, why, rdeps, path
internal/render/   canonical text and --json output
internal/cli/      flag permutation shared by every subcommand
internal/actor/    asserting-actor resolution (EVENTS.md §4)
internal/ns/       namespace resolution: cwd -> git remote -> config -> default
internal/importer/ gh shell-out
cmd/weft/agent.md  the onboarding snippet, go:embed-ed (milestone 10)
cmd/weft/view.html the `weft view` page template, go:embed-ed; mermaid is not in it
tools/             consumers of `weft export --json`; not part of the binary
```

`tools/` is deliberately outside the build. Anything that only needs the graph
belongs there, reading `weft export --json` like any other consumer (§1).

`weft view` is inside it, but its renderer is not. mermaid is ~3.2 MB, and
embedding it would tax every invocation of every command to serve the one that
draws a picture, against a 2-5 ms cold-start budget (§1.1). The binary embeds
only `cmd/weft/view.html`, the ~74 KB page template; the page loads a pinned
CDN build of mermaid by default, and `--self-contained` reads a local copy at
run time and inlines it. mermaid is not vendored here either. `tools/weft-html.py`
fills the same template from an export on stdin, and `TestViewMatchesWeftHTML`
pins the two fillers to the same page.

`agent.md` lives beside the code that embeds it because `go:embed` cannot reach
out of its package directory and refuses symlinks. `docs/AGENT.md` is the
symlink, in the direction that keeps the build working.

The fold lives in `store`/`graph` as a **library**, not inside `cmd`. Phase 2's
scheduler imports it rather than reimplementing it; that boundary is the whole
reason the split exists.

### 2.1 Line budget

Orders of magnitude, not targets. Only the total is asserted; the rows exist so
you notice when a subsystem doubles.

| Layer | Target |
|---|---:|
| Events: envelope, canonical JSON (RFC 8785 subset), hashing | 400 |
| Store: paths, flock, append, fold | 350 |
| Graph: materialization, indexes, prefix resolution | 400 |
| Queries | 450 |
| Rendering: canonical text + JSON | 350 |
| Namespace resolution | 200 |
| Flag permutation, actor resolution | 150 |
| `gh` import | 150 |
| CLI dispatch | 400 |
| **Total phase 1** | **~2,800** |

Tests and tooling are outside the budget.

## 3. Build order

Each milestone is a runnable artifact with a falsifiable exit condition.
Milestones 1–4 get `add` / `link` / `show` working end to end before any query
command exists.

| # | Milestone | Exit condition |
|---:|---|---|
| 0 | Scaffold: module, dispatch, exit codes, smoke, CI | **Done** — the oracle passes locally on macOS and CI is green on Linux from the first push; `smoke.sh` reports every unbuilt stage as pending and still exits 0 |
| 1 | Events + store: envelope, hashing, transaction, fold | **Done** — 14 `EVENTS.md` §7 conformance fixtures pass; racing opposite `depends-on` edges commit exactly one; a mid-append fragment is truncated by the next writer and discarded by readers; fold measured at ~50 ms/10k events (§1.4) |
| 2 | `weft add`, `weft ls` | **Done** — three nodes added in one session list in the next; the log validates as JSONL under a parser that does not know weft's envelope; flags parse after positionals (§1.2); rendering pinned by golden files |
| 3 | ID prefix resolution | **Done** — any unique prefix resolves, as does an exact ref; an ambiguous query exits 4 listing every candidate with id and title; display width expands past 7 on a corpus seeded with a 7-char collision, and what is displayed always resolves |
| 4 | `weft link`, cycle check, `weft show` | **Done** — a `depends-on` cycle exits 5 with the offending path printed as titles, and nothing is written; `show` renders the full neighborhood in one invocation, both directions, with neighbour titles and inverse readings |
| 5 | `claim` / `release` / `done` / `drop` | **Done** — state derives purely by folding, with nothing in the store but the log; two live claims on one node is reported, not refused; claims carry an absolute expiry so abandonment surfaces as `ls --stale` |
| 6 | Queries: `ready`, `why`, `rdeps`, `path` | **Done** — SPEC §7 runs as an executable transcript (rung 5), minus the two lines needing milestones 8-9; `why` prints the route and the claim holder, never a bare status; namespace filters select result roots while traversal crosses, so a cross-namespace blocker cannot be filtered into invisibility |
| 7 | Namespaces | **Done** — a cross-namespace `depends-on` edge resolves from either end; `--ns` defaults from `$WEFT_NS`, then a config path mapping, then the git remote; `--all-ns` widens every listing, and node-rooted queries are global by construction rather than by flag |
| 8 | `weft merge` | **Done** — refs, edges, and claims union onto the survivor and the merged id still resolves to it; a merge closing a `depends-on` cycle exits 5 with the path and writes nothing; the merge is one event and rewrites no others, so edges keep their original provenance. Reversal is deferred rather than shipped: undoing a merge needs replay, and `EVENTS.md` §3.7 records the cost |
| 9 | `weft import gh` | **Done** — binds refs by shelling out to `gh`, and re-importing unchanged upstream appends *nothing*, leaving the log byte-identical rather than merely free of duplicate nodes. One `gh` fetch pair and one transaction for the whole batch, however many items. Upstream state is cached as `ref.observe` with its age and is never authoritative over the node's own fields |
| 10 | Agent onboarding | **Done** — `--json` on every command, and `show --json` carries the whole neighborhood rather than a bare node, so the one-call-to-orient command never forces a follow-up call. The snippet is `cmd/weft/agent.md`, embedded in the binary, printed by `weft help agent`, symlinked as `docs/AGENT.md`. Drift is caught by construction: every command, exit code, node kind, edge kind, and flag it names is checked against the binary, and every command line it shows is executed. The condition was run rather than asserted: a cold agent given only the snippet and the binary — no `--help`, no source, no docs — completed SPEC §7 end to end, and the four points where it had to guess became edits to the snippet |
| 11 | Artifact observation: the payload clock + `weft ref observe` | **Done** — `import` records upstream `created`/`modified` and `ref observe` stats and hashes `file:` refs, both diffing inside the transaction so an unchanged artifact appends nothing, verified on the bytes; unobservable schemes are reported and skipped, with `gh:`/`pr:` naming import as the remedy; `show` renders both clocks as ages and the machine forms carry the payload verbatim; `tools/weft-timeline.py` lanes a real corpus (this repo's own issues, imported) by the artifact clock, with weft-created fallbacks marked `~` (SPEC §6.3, `EVENTS.md` §3.8–3.9) |

Milestone 10 is not documentation. SPEC §9 makes adoption a design constraint:
if an agent has to discover the tool by reading help text, it mostly will not.
Treat it as a feature with an exit condition, because it is the one most likely
to be skipped.

## 4. The oracle

Climb in this order. Each rung is cheap and always-on once it exists. `make
check` is rungs 1, 2, 3-5 and 6 in that order; the individual targets are `make
build`, `make vet`, `make smoke`, `make test`, and `make test-tools`.

**Rung 1 — it builds.** `make build`, which is `go build` plus `go vet` and a
`gofmt` check. Build through the Makefile rather than by hand: the version is
applied with `-ldflags`, so a bare `go build` yields a binary calling itself
`0.0.0-dev`, and a build from a modified tree is stamped `-dirty` rather than
claiming the commit it is not.

**Rung 2 — it runs.** `smoke.sh`: add, link, show, end to end against a temp
`WEFT_HOME`. Written before milestone 1, so it is a build queue from the start.

Unbuilt stages exit `3` (§5) and are reported `pending`; **any other nonzero
exit fails the run.** So the queue is visible without main ever being red — a
permanently-failing smoke test would just teach everyone to ignore CI, which is
worse than having none. The queue property lives in the *pending count*, not in
the exit status.

**Rung 3 — printing is pinned.** Golden files over *both* the raw JSONL log and
the rendered output. This is the direct test of SPEC §9.5: identical graphs must
print byte-identically. It is the constraint most likely to rot silently,
because nothing else breaks when node ordering drifts — only your git diffs get
noisy, six months later, in someone else's review.

**Rung 6 — the tools are tested too.** `make test-tools` runs every
`tools/test_*.py`. They sit outside the Go build (§1.1) and therefore outside
`go test`, so for a while CI ran none of them and a green check on a viewer
change said nothing about the viewer — a gap a reviewer found, because nothing
failed to reveal it. **Coverage that no job runs looks exactly like coverage
that passes**, which makes this the rung most worth stating rather than
assuming.

The suites are globbed, not listed: a suite named in no build file is the same
failure again, and a hand-maintained list is a second place to forget. A missing
`python3` is a hard failure rather than a skip, for the same reason — a check
that quietly covers less than it appears to is worse than one that is honestly
absent. Override the interpreter with `make test-tools PYTHON=...`.

**Rung 4 — properties.**

- *Fold determinism.* A fixed event set folds to the same graph regardless of
  read chunking or buffer boundaries.
- *Concurrent append.* N processes × M events produces exactly N·M valid lines,
  every one parseable. This is the real risk at 10+ agents, and it is the one
  bug class that corrupts the durable artifact rather than just returning a
  wrong answer.
- *Prefix resolution totality.* Every node in the corpus is reachable by some
  prefix, and no prefix resolves to the wrong node.
- *Fuzzing*, on the three surfaces where a foreign input reaches us: the event
  envelope, the fold, and argv. Go's native fuzzing, so no dependency
  (§1.1). `make fuzz`, seeded from the conformance fixtures — mutation from a
  real event reaches far more of the parser than mutation from a literal.

  The properties are chosen for failures that do not announce themselves.
  Anything `Parse` accepts, `Marshal` must write back, or the event carries an
  id nobody can recompute. Folding is deterministic and leaves every edge
  pointing at a survivor. `Permute` neither drops nor invents an argument. What
  is deliberately *not* asserted is acyclicity: the fold reports what the log
  says (SPEC §9.3), so a log holding a cycle must fold without complaint.

  Kept out of CI — a time-boxed search fails nondeterministically, and a job
  that goes red for reasons unrelated to the change teaches everyone to ignore
  CI, which is the argument `smoke.sh` already makes about pending stages. Seed
  corpora still run under every `go test`, so a crash found once stays fixed
  whether or not anyone fuzzes again.

**Rung 5 — the worked example is a test.** SPEC §7 committed as an executable
transcript. It is the spec's own claim about what the tool does; it should fail
the build when it stops being true.

## 5. Exit codes

Part of the CLI contract, because agents branch on them and `smoke.sh` depends
on `3` specifically. Stable from milestone 0.

| Code | Meaning |
|---:|---|
| 0 | Success |
| 1 | Usage error — unknown command, bad flag, missing argument |
| 2 | Not found — no node matches the given id or ref |
| 3 | Not implemented — the milestone has not landed |
| 4 | Ambiguous — a prefix matched more than one node (`EVENTS.md` §2.4) |
| 5 | Rejected — validation failed, e.g. the edge would create a cycle |
| 6 | Store error — corrupt log, unsupported schema `v`, lock failure |
| 7 | External tool failed — `gh` missing, not authenticated, or refused; `view --self-contained` found no local mermaid, or `--open` could not start the opener |

A correct `--help` (or `-h`) is `0`, on stdout — it is a success, not one of the
three usage errors `1` names. Help printed *because* a flag or argument was
wrong stays `1`, on stderr; so does a subcommand named wrong (`weft ref` with no
verb) or a typo (`weft ref bogus`). Asking a command what it does and getting
told you made a mistake is the confusion this distinction removes.

`2` and `4` are distinct because they call for different responses: one means
the thing does not exist, the other means say more.

`7` is distinct from `6` for the same reason: a shell-out that failed is not a
weft failure, and reporting `gh auth login` as a store error sends you to
entirely the wrong place. Output from a tool that *ran* but returned something
unreadable is also `7` — a changed JSON shape is still not a corrupt log. Adding a code is additive — nothing branching on `0`–`6`
breaks — which is why the contract can grow without a `v` bump anywhere.

Every nonzero exit writes a diagnostic. Under `--json` it goes to stdout as an
object with `error`, `code`, and optionally `candidates`; otherwise to stderr.
`candidates` carries node ids for `4` and the offending path for `5`, so a
consumer must read it by exit code rather than assume it is a disambiguation
list.

**A nonzero exit means nothing was written.** Callers branch on the code and
retry, and a retried `weft add` asserts a *new* node rather than finding the one
that already landed. A committed write reported as a failure therefore costs a
silent duplicate that someone has to `merge`.

Presentation runs after the transaction commits, so it must not be able to
decide the code. `cmd/weft/add.go` is the reference shape: past the commit,
both a failed read (the fold that picks the id display width) and a failed
write to stdout degrade to a warning on stderr and exit `0`. The node prints at
full id width in that case. With no readable log there is no id set to be
distinct from, and an untruncated id always resolves.

That is enforced in `add` and nowhere else yet. The other mutating commands
resolve their arguments inside `Mutate`, so their fold runs *before* the append
and a corrupt log aborts the write for real, which is why `add` was the only one
reproducibly wrong. Their post-commit fold is still a lockless read and can
still return `6` for a write that landed, through a narrower window: a
concurrent writer on a newer binary appending a bumped `v` between the commit
and the fold. The cost is bounded where it is not in `add`, because `link`,
`done`, `claim`, and `ref add` are idempotent in effect. A repeated assertion
folds to the same graph, where a repeated `add` folds to a second node. Bounded
is not correct; it is the reason this is a known gap rather than an open bug.

## 6. Deferred

Nothing here is phase 1.

- Snapshots or indexes (see §1.4 — only when the budget is exceeded, measured)
- Per-namespace log sharding (SPEC §5)
- The VM, fibers, and node bodies (SPEC §8)
- Any resident process (SPEC §2, phase 3)
