# weft

A durable graph of work, and the log underneath it.

Light inspiration from sibling projects:

- agent-tools / claude-graph

## 0. The name

On a loom, the **warp** is the fixed set of threads held under tension; the
**weft** is what you thread across them. That is exactly the two-layer structure
here: the append-only log is the warp, and the graph of work is the weft woven
through it.

The project began as *DAG-Pipe-VM*, and the rename records a real correction.
"DAG" overclaimed: acyclicity holds for `depends-on` and not for
`conflicts-with` (§4.2), so the system is a graph containing a DAG, not a DAG.
The invariant actually worth naming is §9.6 — **the graph may have cycles; the
log never does.** The acyclic thing is the history.

> *Original objective, preserved:* To create a DAG-based pipeline to support
> processing tasks in a structured manner, allowing for efficient execution and
> management of dependencies among multiple collaborative agents and humans.
> Idiosyncracies: the entire DAG is driven by a bytecode VM.

---

## 1. Thesis

Coordination between many agents and several humans fails at the *unwritten*
layer. Issue trackers can only name things that exist in the tracker; the
expensive facts — a latent bug nobody filed, a colleague's unpublished branch, a
dependency on one sub-point of a code review — have no address anywhere.

`weft` is a durable, inspectable graph of **claims about work**, driven by a
CLI usable by humans and agents alike, with no daemon and no server. Phase 2
makes the nodes computational: a call-by-need heap that survives process death.

Single binary. Many short invocations: load, look, mutate, save, exit.

## 2. Scope

**Phase 1 — the claims graph.** Nodes, typed edges, claims, five queries. Zero
compute. This is the MVP and is independently useful.

**Phase 2 — the VM.** Node bodies become suspendable computations with durable
execution. Phase 1 stores bodies as a structured-but-inert field so phase 2
requires no migration.

**Phase 3 — long-running / IPC.** Explicitly deferred. Nothing in phases 1–2 may
assume a resident process.

Stack, module layout, build order, and the verification ladder live in
`BUILD.md`. This document stays implementation-agnostic.

### 2.1 v0 is host-local

**Coordination is between processes and agents sharing one filesystem.
Cross-machine sharing is a non-goal for v0.**

This has to be stated because the rest of the document implied more than it
delivered. "Shared by humans and agents" is true of ten agents and one person on
one laptop; it is not a claim about replication. Distributed collaboration would
require a transport, a merge order, event identity, and a causal model, and a
single append-only file with wall-clock timestamps supplies none of those.

Consequences:

- No transport, no replication, no sync. Not deferred — out of scope.
- Wall-clock `at` is adequate, because one host has one clock. Any move to
  multi-host invalidates that and needs revisiting first.
- §9.5's "what goes in git" is therefore about an **explicit export**, not about
  the live store. `weft export` (§6) provides one; what remains open is whether
  the canonical *text* form is the thing worth committing — see open question 5.

## 3. Glossary

- **Node** — an assertion that some unit of work exists. May or may not
  correspond to anything in an external system.
- **Ref** — a binding from a node to external reality (`gh:o/r#12`,
  `pr:o/r#45`, `branch:x`, `file:p#L10`, `session:<uuid>`). A node has zero or
  more.
- **Observation** — cached facts about the artifact behind a ref: what it said
  about itself, and when weft looked. A cache with provenance, never authority
  (§6.3). The node is what you say, the ref is where it lives, the observation
  is what you saw.
- **Edge** — a typed, directed relation between two nodes.
- **Claim** (formerly *Lease*) — an ownership assertion on a node by an agent or
  human. Not authoritative: a claim may be *inferred* about a third party.
- **Provenance** — who asserted a fact, when, and how they came to know it.
- **Frontier** — the set of open nodes with no unmet dependencies.

## 4. Data model

### 4.1 Node

| Field | Notes |
|---|---|
| `id` | The id of the node's `node.add` event; full 64-hex stored, display prefix expands dynamically. Globally unique across namespaces. Normative definition in `EVENTS.md` §2. |
| `ns` | Namespace. Free string, not necessarily a repo (`acme/api`, `research/weft`, `personal/infra`). |
| `kind` | `bug` \| `task` \| `spec` \| `pr` \| `issue` \| `finding` \| `note` \| `workstream` |
| `title` | Free text, required. |
| `body` | Optional structured field. Inert in phase 1; the VM's node body in phase 2. |
| `refs` | List of external bindings. Added over time as reality catches up. |
| `state` | `open` \| `done` \| `dropped`. Derived from events, not stored. |

A `workstream` is the durable identity of a piece of work that spans sessions
and repos. It is a container: the sessions and anchors that make it up hang off
it as refs or as child nodes linked with `part-of`. Refs are free-form, so a
caller's schemes such as `claude:<sid>` or `wt:<path>` are its own convention:
weft gives them no special handling and does not normalize them. Containment does not imply `depends-on` — a workstream is not
blocked by its children unless someone asserts that edge.

IDs are hash-derived rather than sequential so that concurrent writers never
need to coordinate on allocation. Human ergonomics come from prefix resolution,
not from short counters.

### 4.2 Edge kinds

Acyclicity is a property **per kind**, not global. A global DAG constraint would
reject `conflicts-with`, which is inherently symmetric.

| Kind | Family | Direction | Acyclic? |
|---|---|---|---|
| `depends-on` | link | A needs B | **Required.** Enforced on write. |
| `conflicts-with` | link | symmetric | No — cycles are meaningful. |
| `derived-from` | link | A was discovered via B | Yes, by construction (append-only provenance). |
| `part-of` | place | A decomposes out of B | Required — forest. |
| `same-as` | place | symmetric | No. |
| `supersedes` | place | A replaces B | Required. |

The link/place split is Milner's (bigraphs): *connectivity* and *containment*
are orthogonal structures over the same node set, and conflating them into one
edge type with a discriminator field loses the distinction that makes each
queryable.

`depends-on` is the spine. All readiness, path, and blocking queries run over it
alone.

### 4.3 Claim

| Field | Notes |
|---|---|
| `node` | Target. |
| `holder` | `me` \| `agent:<id>` \| `user:<handle>` |
| `note` | Optional free text. |
| `provenance` | See below. |

Claims do not lock. They are advisory statements that inform `ls --ready` and
surface collisions; two live claims on one node is a reportable condition, not
an error.

### 4.4 Provenance

Every node, edge, and claim carries:

- `by` — human or agent identity
- `at` — wall-clock timestamp at append
- `source` — `observed` (read from an API) | `asserted` (a human said so) |
  `inferred` (an agent concluded it)

This is not optional metadata. Roughly half of a real graph will be inference,
and a graph that cannot distinguish "I read this in the API thirty seconds ago"
from "an agent guessed this on Tuesday" becomes untrustworthy within a week and
then goes unread. `source` is what makes `ls --stale` possible.

## 5. Storage

A single append-only event log (path in **Scope** below). One JSON object per
line. The graph is a **fold over the log** — there is no separate mutable state
file.

```
{"v":1,"ev":"node.add","id":"<64hex>","ns":"acme/api","kind":"bug","title":"...","body":null,"at":"2026-07-31T09:14:22-07:00","by":"agent:scanner","source":"inferred"}
```

Illustrative only — `EVENTS.md` §3 is normative for every field and kind.

Consequences, genuinely free: full history and audit ("which agent added this
dependency"), plus a natural multi-writer story.

Undo is **not** free, despite the obvious appeal of saying so. Append-only
storage makes correction *possible*, not automatic: it requires explicit
compensating events (`EVENTS.md` §3.3), because truncating a shared log would
discard unrelated later events from other writers.

**The byte-level format, event identity, fold semantics, and the write protocol
are normative in `EVENTS.md`.** This section describes intent; that document
describes bytes, and wins on any conflict.

**Concurrency.** Locking for the duration of the *append* is not sufficient — a
read-validate-write sequence must be atomic, or two processes can each validate
opposite `depends-on` edges and then both commit the cycle they had just
rejected. Validating mutations hold an exclusive lock across
fold → validate → append → fsync (`EVENTS.md` §6). Readers take no lock and
tolerate a partial final record.

No snapshot or index is materialized in phase 1. But note that the transaction
above folds the whole log under an exclusive lock on every validating write, so
write throughput degrades with log length and concurrent agents serialize —
which is the likeliest first breach of the `BUILD.md` §1.4 budget, and is
measured at milestone 1 (`EVENTS.md` §6.1).

**Scope.** *One* store, globally, at `$XDG_DATA_HOME/weft/log.jsonl`
(falling back to `~/.weft/`). Not one per repo.

Namespaces are a **field on the node, never part of its identity**. IDs are
event-derived hashes and therefore already globally unique, so cross-namespace edges
cost nothing and need no special case — which is the point, since the motivating
story crosses repos (an upstream issue, a colleague's fork, a review comment).
Namespaces are also not repo-bound: multi-project and task-agnostic use is a
requirement, not a stretch goal.

`--ns` resolves in this order, first match winning:

1. `$WEFT_NS` — the mechanism agents use to pin a context
2. the longest matching `ns:<path>` prefix in the store config
3. the git remote of the working directory, as `owner/repo` (host dropped, so a
   mirror shares a namespace with its origin)
4. `default`

Config beats the remote deliberately: it exists to override a remote you
disagree with, and an override that lost to the thing it overrides would be
useless.

The store config is `$WEFT_HOME/config` — `~/.weft/config` by default — read as
`ns:<path> = <namespace>` lines, `#` starting a comment:

```
ns:/w/acme            = acme
ns:/w/acme/vendored   = upstream/lib
```

Longest prefix wins, so a mapping on a parent directory covers everything
beneath it while a deeper entry carves out an exception. Two consequences worth
stating, because both are silent: since config outranks the remote, a mapping on
a parent also captures repos below it that would otherwise resolve from their
own remotes, and those must be restated to keep them; and a directory that is
not itself a repo resolves from the *enclosing* repo's remote, so sibling
directories in one checkout collapse into a single namespace.

**Both the working directory and each mapping's path are resolved through
symlinks before matching**, so one directory resolves to one namespace however
it was reached. Without this the answer depends on `$PWD` — a shell that `cd`ed
through a symlink and a tool that resolved the path first would match different
lines and file work under different projects, decided by shell history rather
than by anything about the work. A mapping may therefore be written against
either the link or its target. A path that cannot be resolved, because it names
a directory that does not exist yet, falls back to matching literally; nothing
lives under a path that does not exist, so the fallback cannot over-match.

A consequence worth planning for: a workspace assembled from symlinks does not
contain its repositories in the filesystem sense, so a catch-all mapping on the
workspace directory will not capture them. Give each target its own line.

**Only `ls` takes a namespace filter.** `why`, `path`, `rdeps`, and `show` are
rooted at a node and are *always* global — filtering them would reproduce
exactly the failure the filter-vs-traversal rule prevents, and a node addressed
by id should answer the same way from anywhere.

A single global log keeps cross-namespace queries free and the fold trivial at
expected scale. Sharding per namespace is a later optimization and would make
cross-namespace reverse queries read every shard — do not do it preemptively.

## 6. CLI surface

Nineteen commands. Every one accepts `--json` and emits stable IDs. `--ns`
selects where new nodes land and which nodes a listing shows, so it appears on
`add`, `ls`, and `import`; `--all-ns` widens `ls`. Node-rooted queries take
neither — they are global by construction (§6.1), which is stronger than a flag
that would only ever be passed.

```
weft add "title" --kind bug|task|spec|pr|issue|finding|note|workstream [--ref REF]... [--if-absent [--even-dropped]]
weft link A depends-on B [--if-absent]   # also: conflicts-with, part-of, supersedes, derived-from
weft unlink A depends-on B        # retract the assertion; absent is success (§9.3)
weft ref add|rm A REF             # bind an existing node to external reality, or unbind it
weft ref observe A [REF]          # refresh the cached facts behind A's refs (§6.3)
weft claim A [--note "..."] [--holder WHO] [--source observed|asserted|inferred]
                                # --holder is the subject; the asserting actor
                                # is always resolved, never passed (EVENTS.md §4)
weft release A
weft show A                       # full local neighborhood in one call
weft why A                        # the blocking path(s), not just the fact
weft path A B [--via K[,K...]] [--dir either|out|in]   # cleanest route; reports how it walked
weft rdeps A [--depth N] [--via K[,K...]]   # what depends on me, and through which kinds
weft ls [--ready|--blocked|--mine|--stale|--kind K] [--since D] [--recent] [--all-ns]
weft ns                           # which namespace this directory resolves to, and why (§5)
weft env                          # the store, actor and namespace in use, and why
weft export [--json [--include claims,source,refs|all] | --d2 [--flat]]   # the whole graph in one fold (§6.4)
weft view [-o FILE|-] [--only-ns NS] [--self-contained] [--open]   # the graph as one interactive page (§6.4)
weft merge A B                    # we learned they were always the same node (§6.2)
weft import gh o/r [--merged-since D] | o/r#12   # bind reality; one-way, on demand (§6.3)
weft done A | weft drop A [--if-present]   # --undo returns the node to open
```

`add --if-absent` and `link --if-absent` are the retry-safe forms, for a writer
that re-runs rather than one asserting a second time.

`add --if-absent` is the retry-safe form for a writer that asserts its title
means the same work. Inside the write transaction, it reuses the earliest live,
unmerged node with the same exact title and namespace, prints that node, and
appends nothing. Kind, source, and refs do not participate in the match and are
not changed on a match. A dropped node is absent and may be recreated. Plain
`add` remains an assertion of a new node even when every field is identical
(§4.1).

`--even-dropped` widens `--if-absent`'s notion of present to include dropped
nodes, for the writer whose script asserts a claim and then retracts it — the
natural record of "considered and rejected". Without it, the drop makes the
node absent again and every re-run mints a fresh one. A live match still wins
over a dropped one: when both exist, the live node is the work and the dropped
one is history. Unlike plain `--if-absent`, an `--even-dropped` reuse **binds
any `--ref` the reused node lacks** (already-carried refs append nothing): its
audience is re-runnable scripts that address by ref, and a reuse that silently
absorbed the assertion without the binding would make every later line naming
the ref fail at the addressing layer — the failure shape this flag exists to
close.

Because a dropped node keeps its refs, one ref can come to be carried by both
a dropped node and its live re-assertion. **Ambiguity in a ref counts live
nodes only**: a ref resolving among several candidates discards the dropped
ones, and reports ambiguity only when two or more live nodes carry it. A ref
carried by a *single* dropped node still resolves — retraction is a state, not
an erasure. Two or more dropped holders with no live one stay ambiguous: there
is no live node to prefer, and choosing among retracted claims is `weft
merge`'s judgment, not resolution's. Id-prefix resolution does *not* discount
dropped nodes: a
prefix names identity rather than meaning, and a dropped node must stay
addressable by its id.

Every command that binds a ref prints a note when another live node already
carries it, or its `gh:`/`pr:` sibling for the same `owner/repo#N` — both
schemes are first-class names for a pull request, so one PR can otherwise
occupy two nodes with no warning and no ambiguity error. `add --ref` and
`ref add` note at the moment of binding; `import` notes each node it creates
whose ref's sibling a live node carries, since the bulk path is where the
duplicates multiply. The write still happens and the exit code stays 0: refs
are not identity (§4.1), the equivalence never enters resolution, and
deciding the two are the same work remains `weft merge`. The note exists so
the duplicate is visible at the moment it is created rather than several
edges later.

`done --if-present` and `drop --if-present` are the retry-safe forms of the
state commands: a node already in the target state, or a query matching no
node at all, appends nothing and succeeds — the flag reads as "act only if
present", so under `--undo` it means reopen-if-present and a missing node is
likewise a no-op. Ambiguity stays an error even under the flag: the writer
named something, and retracting one of several matches on its behalf is not
tolerance. An empty query stays a usage error too — that is the unset shell
variable, a caller bug, not a node that happens to be absent.

`link --if-absent` reports success and appends nothing when the edge is already
there, matching on the same `(kind, from, to)` key the fold collapses on — so a
symmetric kind asserted the other way round is found, and both endpoints
canonicalize through the alias map. A retracted edge is absent, so re-linking
after `unlink` writes. Plain `link` remains an assertion, which is not
redundant: the fold keeps the earliest triple, but the log keeps every asserter,
and the log is the contract (§9.1).

### 6.1 Query semantics

- **`ls --ready`** — open nodes whose every *known* `depends-on` target is
  `done`, and which carry no live claim by another holder.

  **Readiness is optimistic: it means "no known blocker," never "ready."** A
  graph that is deliberately incomplete (§9.2) cannot establish the second. The
  distinction is not pedantry — an agent that reads `ready` as a guarantee will
  start work that is in fact blocked, and the graph gets blamed. Output and
  documentation say *no known blocker*.

  **`ls --ready` and `ls --blocked` skip `workstream` nodes.** A workstream is
  a container, not work, so it is never something to pick up or to unblock.
  It usually carries no `depends-on` edges, so it would otherwise read as
  ready, and an agent orienting from the frontier would be offered a container
  to claim or close while its children are still open. Plain `ls` and
  `ls --kind workstream` still list them. The exclusion is from the listings
  only: `T depends-on W` on a workstream `W` is valid, makes `W` a blocker that
  `why T` names, and resolves when `W` is marked `done` — `why` does not look
  through a container to its `part-of` children.
- **`why A`** — walks `depends-on` from A to the nearest open blockers and
  prints the *paths*, annotated with each hop's claim holder. "Blocked" without
  the path is not actionable.
- **`path A B`** — shortest route between two nodes, each hop annotated with the
  relation followed and the direction it was read in. Edges are traversed in
  **either direction**, because the question is how two pieces of work relate,
  not which formally precedes.

  **Every kind is traversed, not just the link family.** §7's own example
  requires it: a task depends on a review point that is `part-of` a PR, so
  restricting to link-family edges reports no route between the task and the PR.
  `--via <kind>[,<kind>...]` narrows the walk when that is what you want. It
  takes a list because one kind at a time cannot express the restriction a
  reader usually wants — the *directed* family, excluding the symmetric kinds.
  A symmetric edge is a free bidirectional shortcut between two nodes that exist
  to say they are alternatives, and an index node with many `part-of` children
  is a hub that puts everything within two hops of everything else; a structural
  graph has far more of both than a work graph does.

  **`--dir` makes it a reachability query instead.** Kind and direction are
  separate filters, and narrowing only the kind leaves a trap: `--via depends-on`
  between two nodes that share one dependency finds a route *through* it, which
  reads as a dependency. The reversed hop is labelled honestly, but both labels
  contain the substring "depends on", so neither skimming nor scripting recovers
  the direction. `--dir out` restricts the walk to edges crossed forwards — does
  A lead to B — and `--dir in` to the other way. Symmetric kinds are exempt: they
  have no asserted direction to restrict, only the fold's normalized endpoint
  order, so pinning one would answer a direction question with an artifact.

  **`path` reports how it walked** — which kinds, which direction, and which
  kinds ignored the direction — restricted or not. The route alone cannot
  distinguish an absent one from one the filter excluded, and a route that
  crosses a symmetric edge looks wrong when it is merely permitted. Naming the
  rule is the same move as `weft ns` naming the rule and not just the answer.
- **`why A`** — the *nearest* unresolved dependencies, each with the route that
  reaches it and whoever holds a live claim. Nearest rather than transitive: the
  full set is noise once you know the first thing to go fix. Only `open`
  dependencies block — `done` and `dropped` are both resolved, and treating
  `dropped` as a permanent block would leave dependents with no way forward but
  a fake completion.

  **Blockers that gate other blockers are marked and sorted first.** Nearest is
  a set and not an order, so a flat list reads as equally available work; when
  one of the answers waits on another, a reader who starts on the wrong one has
  it re-done. The count is over the blockers in the same answer rather than
  every dependent in the graph — the question is which of the things in front of
  you to start, and ranking on nodes the output never showed would rank on
  evidence the reader cannot check. `rdeps` answers it from the other end, but
  only for a node you already suspect.
- **`ls --mine`** — nodes you hold a **live** claim on.
- **`ls --stale`** — nodes whose claims have *all* expired: work someone took
  and abandoned. A node nobody ever claimed is *unclaimed*, not stale, and the
  two call for different responses. Claim lifecycle and the default TTL are
  normative in `EVENTS.md` §3.6.
- **`ls --since D`** / **`ls --recent`** — filter to, and order by, what moved
  most recently. `D` accepts `h`, `d`, and `w`; days and weeks are fixed
  multiples, not calendar units.

  **"Moved" means weft asserted something about the node** — state, edge, ref,
  claim, or merge. Claiming counts: someone taking the work is a fact about the
  node, and the claim is stamped when it was made, so work taken and abandoned
  three months ago still reads three months old. It deliberately excludes
  `ref.observe`: an observation records that upstream was *looked at*, and
  counting it would let a scheduled import float every node it polled to the top
  of a recency listing, making polling indistinguishable from progress (§10).

  **`--stale` and `--since` do not compose**, and the reason is worth stating
  because the combination reads as though it should. A node goes stale when its
  claim's TTL passes, and expiry is computed at read time from a field on the
  claim — nothing is written, so no timestamp can record it. `ls --stale
  --since 7d` therefore answers "what was *claimed* this week and has since
  expired," not "what was abandoned this week." Those sets are disjoint whenever
  the TTL exceeds the window, which is the default case.

  The timestamps behind this are a **hint, not an order**. They come from event
  `at`, which is a wall clock — it skews between machines, and a backfilled
  import writes an old date today. File order remains the log's total order
  (`EVENTS.md` §5.1) and is what breaks ties, so canonical printing (§9.5)
  survives. For the same reason recency accumulates as a maximum rather than a
  last write: one backdated event must not make a node that just moved report as
  untouched.
- **`rdeps A`** — transitive reverse closure over `depends-on`, and it reports
  which kinds it walked. The name is the narrow one and the default matches it,
  but on a real map most edges are `part-of` and `derived-from`, so an empty
  answer reads as "nothing depends on this" when it means "nothing depends on
  this *through `depends-on`*" — the same class of silent miscommunication the
  `via:` line on `path` exists to close, so it gets the same treatment.
  `--via K[,K...]` walks other kinds instead, and the walk stops extending at a
  kind that is not transitive. `conflicts-with` is the only such kind: A
  conflicts with B and B conflicts with C says nothing about A and C, so
  following the chain manufactures a relationship nobody asserted — and since
  the output indents by depth, it would draw it rather than merely fail to rule
  it out. `same-as` is symmetric too but genuinely transitive, so it walks. Note the polarity is the opposite
  of `path`'s: `path` starts at every kind and `--via` narrows, `rdeps` starts at
  one and `--via` widens.

  A widened walk changes what the output *is*: a pure `depends-on` closure
  lists dependents, but a `part-of` child or a `derived-from` derivation waits
  on nothing, and an indented tree under a command named for dependencies would
  assert otherwise. So once the walk actually **crosses** any kind beyond
  `depends-on` — crossed, not merely requested: a wide `--via` over a store
  whose edges are all `depends-on` still yields the pure closure and stays
  bare — every row names its relations to the node it was reached from, in
  the row's own reading: `(part of)`, `(depends on, part of)` when several
  kinds connect the pair, since naming one would assert it as *the* relation.
  A depth-1 row relates to the queried node; a deeper row relates to its
  parent in the walk, which level-order output does not place above it, so
  the deeper annotation names that parent — `(depends on c269c4b)` — making
  the claim checkable rather than a relation to a node the reader cannot
  identify. An empty answer crossed nothing and always reads `no known
  dependent`; the `via:` line says which kinds were open to it. Every `--json`
  row carries `via` (the list of kinds linking it to its parent) and `parent`,
  on every walk, pure ones included — a consumer should not have to know
  which flags produced the output.
- **`show A`** — one-shot neighborhood dump: node, refs, claims, all in/out
  edges with neighbour titles. Designed so an agent needs exactly one call to
  orient, not five.

### 6.2 Merge

`weft merge A B` folds B into A: refs union, edges re-point to A, claims union,
observations move, and A's recency advances to whichever node moved last. B stays
a node — it keeps its own `ns`, `kind`, `title`, and `state` in the
fold, and every way of addressing it resolves to A. So an id pasted from a
month-old commit message keeps working, and A's fields winning any conflict
discards nothing: B's values are still in the log, and `show A` names B as
merged. No command prints B's own fields, though, so "not lost" means
recoverable from the log rather than one query away.

Reversed by a compensating event, **never by truncating the log** — truncation
in a shared store discards unrelated later events from other writers, and is a
recovery operation only (`EVENTS.md` §3.3, §5.3). No such event exists yet;
`EVENTS.md` §3.7 records why it is deferred rather than shipped.

Merge can introduce a `depends-on` cycle that neither graph had. It is checked
at merge time and rejected with the offending path printed.

An earlier draft said B was "recorded as `same-as A`". That cannot work: §4.2
makes `same-as` symmetric, so the fold normalizes its dedup key and the record
of *which node survived* is precisely what it cannot hold. The direction lives
on the merge event instead, and `same-as` keeps its own meaning — "these look
like the same work" — asserted without folding anything.

### 6.3 Import

`weft import gh <owner/repo>` binds nodes to GitHub issues and pull requests;
`<owner/repo#123>` does one. One-way and explicit — nothing here writes to
GitHub (§10).

Imported nodes land in the **repo's** namespace, not the working directory's.
Importing `acme/api` while sitting in some other checkout should not file the
work under that checkout.

What upstream currently says is recorded as an observation, with the time it was
taken, and is rendered with its age:

```
  refs   gh:acme/api#412    closed  (observed 3d ago)
```

**It is a cache, never authority.** The node's own `title` and `state` are set
once, at creation, and are weft's thereafter. The whole query surface — `ready`,
`blocked`, `why` — keys off `state`, so letting upstream write it would let a
GitHub close silently change what your frontier says. The single-item form can
name a closed one, and it arrives `done` rather than open — state records what
was observed at creation, and after that weft owns the field.

The whole-repo form fetches open items, and `--merged-since D` widens it to the
pull requests merged inside that window. A newly discovered merged PR arrives
`done`, which is what makes the rest of the query surface mean anything: on a
graph where nothing is done, `ls --ready` returns everything and one real
blocker is invisible in the noise. An existing node still keeps its own state,
with the merge recorded only as an observation; changing that would make the
cache authoritative in the exact way this section forbids. So the window is
closer to step 0 of importing a repo than to an edge case — and a repo with no
open PRs, which is the quiet common case, is exactly the one an open-only import
leaves unable to explain itself.

The window is sent to the server as a search qualifier rather than applied to a
plain listing. `gh pr list` orders by creation date, so a long-lived branch
merged yesterday sorts behind newer pull requests and falls off the end of
`--limit` — silently, and on exactly the repos where the window matters most.
One consequence is worth stating: because search does not order by merge time
either, a merged window cut short by `--limit` cannot say *which* pull requests
it missed, only that it missed some.

The divergence showing is the point. "Upstream closed this, we have not" is a
fact worth seeing, and `at` is what makes it judgeable — an observation
presented as current invites a decision on data nobody re-checked.

**An observation records two clocks**, and the distinction carries most of this
section's weight:

- **The artifact's own clock** — `created` and `modified`, what upstream says
  about itself. `import` records them for `gh:` and `pr:` from the item's own
  stamps. This clock is what chronology reads: ordering a lineage, laying out
  a corpus, grouping by cohort. It is what makes weaving over *historical*
  artifacts work at all — an issue three years old imported today is three
  years old, not new.
- **Weft's clock** — when we looked. This clock is what trust reads, and it is
  already the rule above: render the age.

The artifact clock never enters `ls --since` or `--recent` (§6.1). Those
answer "what has weft asserted lately", and letting an import of an old corpus
flood the listing with ancient news is the same defect as letting polling
float nodes to the top — a clock doing the other clock's job. Chronology over
artifact time belongs to export consumers (§6.4), which is where layout lives.

**Observation is broader than import.** `weft ref observe A [REF]` refreshes
the cached facts behind A's refs — all of them, or the one named — for schemes
observable without a network: `file:` today, by stat and content hash. `gh:`
and `pr:` observations arrive through `import`, which stays the one place weft
shells out. A ref of a scheme the command cannot observe is reported and
skipped, never an error (§9.3): a node holding `branch:` and `file:` refs
should not fail because only one of them is observable. Re-observing an
unchanged artifact appends nothing — `EVENTS.md` §3.9's byte-idempotence binds
`ref observe` exactly as it binds import, and §3.8 is normative for the
payload fields and for when an observation is written at all.

A relative `file:` path is relative to the root of the node's own project
checkout. The store is global, so `ref observe` reads a relative path only
when the working directory resolves to the node's namespace (§5), and then
against the enclosing checkout's root rather than the working directory, so a
run from a subdirectory reads the same file. From anywhere else the ref is
skipped with the reason, never observed: hashing another project's `README.md`
and recording it as observed is a false fact in the log. Absolute paths are
read as written.

The content hash rides along on `file:` observations because modification
times lie — a `touch`, a checkout, a copy all move `mtime` without changing a
byte — and because the hash is what makes renames detectable. **A rename is a
change of address, not a change of artifact**, and address is assertion, not
observation: the cure is `ref rm` + `ref add` on the same node, recorded with
provenance like any assertion. Tooling may notice a known hash at a new path
and *suggest* the rebind; it must never perform it, because "same bytes" does
not prove "same artifact" — the same reason node identity is `merge` and not a
hash collision (`EVENTS.md` §2.2).

Rendering shows both clocks as ages, never absolutes:

```
  refs   file:docs/handoff.md   sha256:ab12…  (modified 2d ago, observed 5m ago)
```

### 6.4 Export schema

`weft export --json` emits `{nodes, edges}`. Written down because inspecting
`keys` on one element reads like a schema and is not: a node that happens to
carry no refs led to a documented-sounding "refs are omitted entirely", which
was true of that node and false of the format.

Every field below is present unless marked optional; optional fields are
omitted rather than emitted empty, so absence means "nothing recorded" and
never "recorded as blank".

```
node   id, ns, kind, title, state
       by?, source?   only under --include source; no at — created carries it
       refs?      by default a list of strings
       created?   RFC3339, absolute — the file outlives the moment it was written
       touched?
       claims?    only under --include claims
       released_claims?   only under --include claims
edge   kind, from, to
       by?, source?, at?   only under --include source
```

**The default shape is frozen.** Golden files rest on its exact bytes, and
canonical printing is one of the two constraints that rot silently (§9.5), so
anything richer is opt-in through `--include`:

- **`claims`** adds a `claims` array to each node: `holder`, and optional `by`,
  `source`, `note`, `at`, `expires`. Unreleased claims only, expired ones
  included and dated — an expired claim is what abandoned work looks like, and
  filtering it makes abandoned indistinguishable from unclaimed (§4.3). `holder`
  and `by` are separate because they routinely differ.

  It also adds a `released_claims` array: every claim a `claim.release` let
  go, in the order the claims were taken. Each entry has the same fields plus
  `released_at` and optional `released_by` and `released_source`. `at` stays
  the time the claim was taken, so `[at, released_at)` is the interval the
  holder had the work, and a holder who claimed, released and claimed again
  shows as two entries. `note` survives the release. Released claims are a
  separate array, never entries in `claims`, because every reader written
  before it treats a `claims` entry as current ownership. `weft merge` moves
  them to the survivor like everything else.
- **`source`** adds `by`, `source` and `at` to each edge, and `by` and `source`
  to each node (a node's `at` is its `created`, already present). Without it an
  inferred edge and an asserted one are the same three fields, which defeats
  the reason provenance is recorded at all (§4.4).
- **`refs`** turns each ref from a string into `{ref, observed_at?,
  upstream_state?, upstream_title?, upstream_created?, upstream_modified?,
  upstream_hash?}`. A ref nobody has looked at carries only `ref`; every other
  field appears only when the scheme reports it (§6.3). The age is the
  load-bearing part: an observation rendered as though it were current invites
  a decision on data nobody re-checked (§6.3). `upstream_created` and
  `upstream_modified` are the artifact's own clock and are what a layout or
  timeline consumer orders by; `observed_at` says how much to trust them,
  never where to place the node.
- **`all`** is every section.

`--include` applies to `--json` only. DOT and d2 are layout formats and have
nowhere to put provenance, so asking for it there is a usage error rather than
a silent no-op.

**`--d2` draws `part-of` as containment.** DOT can only draw it as one more
arrow, which reads like a dependency. In d2 (https://d2lang.com) a child sits
inside its parent's box, which is what the place family means (§4.2). The rest
follows DOT: the same label (short id and title), tooltip, link, state styling,
and edge colors, in the same canonical order. Three rules make it total:

- **A node has at most one container.** `part-of` is meant to be a forest, but
  only acyclicity is enforced, so a second parent is possible. The first in
  canonical edge order becomes the container. Any other `part-of` edge,
  including one that would nest a node inside its own descendant in a log that
  carries a cycle (§9.3), is drawn as an arrow. Nothing in the graph is missing
  from the picture.
- **The file names its layout engine, ELK.** A plain `d2 graph.d2 graph.svg`
  uses it, and `--layout` still overrides. d2's default engine, dagre, cannot
  route an edge between a container and its own descendant, which containment
  produces whenever a child also depends on or conflicts with its parent. Use
  `--flat` for dagre: it draws every `part-of` edge as an arrow and nests
  nothing.
- **`--flat` applies to `--d2` only**, and `--d2` and `--json` are exclusive.
  Both mistakes are usage errors.

Like every format, it is the whole store. A one-namespace picture needs the
neighbour closure §11 question 5 describes, not a filter, so for now that
scoping is the reader's job.

A consumer must tolerate both ref shapes (§9.3). `tools/weft-html.py` does, and
is the first consumer of a non-default export.

`weft view` is that consumer made a command: it renders one `--include all`
export fold as an interactive HTML page, a snapshot that never watches the log
(a live view is §10's resident process). `--only-ns` is the one place a
namespace scope is drawn, and it keeps every edge that touches the scope, its
far endpoint reduced to a stub of id and namespace, so the page cannot claim a
node is unblocked when its blocker lives elsewhere (§5). The renderer is never
part of the tool: the page loads a pinned, integrity-checked mermaid build, or
inlines a local one under `--self-contained`, and neither mode falls back to
the other.

## 7. Worked example

The motivating session, transcribed:

```
weft add "N+1 in report generation" --kind bug --ref gh:acme/api#412 --source observed
weft add "cache invalidation race" --kind bug --source inferred     # latent, unfiled
weft add "batch export spec" --kind spec --ref file:docs/export.md
weft claim <2>                                                      # start work

weft import gh acme/api#508                                         # upstream issue found
weft merge <2> <5>                                                  # they were the same thing
weft claim <2> --holder user:colleague --source inferred             # per issue comment

weft add "PR: fix pagination" --kind pr --ref pr:acme/api#77 --source observed
weft link <2> conflicts-with <7>

weft add "review point Z" --kind finding --source observed
weft add "task 7: batch export" --kind task
weft link <9> part-of <7>
weft link <10> depends-on <9>

weft why <10>
weft ls --ready
```

Note what phase 1 must tolerate to make this work: a node with no external ref
(`cache invalidation race`), a claim about a third party we cannot verify, a
`conflicts-with` cycle, and a finding that exists only inside a review comment.

## 8. Phase 2 — the VM

> **Non-normative.** This section is a research direction, not a contract. It
> must not constrain phase 1 — no phase-1 decision may be justified by "phase 2
> will need it," and the `body` field is reserved space, not a compatibility
> guarantee. Two known problems are unresolved below: §8.3's determinacy claim
> and §8.4's scheduling requirement are different models, and an edge is *not*
> a defunctionalized continuation (it names what a computation waits for, but
> holds none of its program counter, locals, or pending expression).
>
> This becomes normative only after a replay-and-effects spike validates it.

### 8.1 Why a VM at all

Not "so nodes can compute" — a shell-out per node gives that. The load-bearing
reason is that work spans invocations: waiting on a claim, a human review, a CI
run, another agent. Shell-out can only checkpoint at node *boundaries*. A VM
checkpoints *inside* a node body.

The target property: **node bodies are suspendable computations, durable across
process death.**

### 8.2 Durability model

Three candidates:

- **A. Node-atomic.** Bodies run to completion within one invocation. Simple;
  could embed an existing Lisp today. But "wait for approval" cannot live inside
  a node, which pushes all the interesting control flow back into edges.
- **B. Serializable continuations.** Suspend anywhere, snapshot the frame stack.
  Maximally expressive; requires an explicit frame stack, heap closures, and a
  fully serializable value domain.
- **C. Replay.** The body re-executes from the top each invocation; every effect
  consults the event log first (already performed → return recorded result; not
  yet → perform and append; blocking → park). Temporal's model, and DBOS's.

**Decision: build for C, with an explicit frame stack anyway.** C is shippable
early and gets B's programming model with A's VM simplicity; the price is that
bodies must be deterministic and get re-run, which for workflow glue is free.
The explicit frame stack costs nothing up front and keeps B available.

This is a recorded lesson from a sibling project: `let-go` nests the host Go
stack per call (`Frame.Run → Func.Invoke → Frame.Run`), so there is no stack to
snapshot. `apolisp` uses explicit frames. Whichever substrate is
chosen, explicit frames are non-negotiable.

### 8.3 A node is a process; its edges are its IO

The intended shape: **each node is an independently executable program whose
only interface to the world is its incoming and outgoing edges.** Ports, not
globals. This is Kahn Process Networks (1974) — independent sequential
processes joined by channels, with the determinacy result that output does not
depend on scheduling — and it is the same shape as dataflow architectures and
interaction nets.

Three things fall out rather than having to be imposed:

- **Suspension is a blocking read.** A node awaiting a dependency is parked on
  an input port. There is no separate suspension mechanism to design, and no
  need for a rule forbidding suspension mid-expression.
- **Replay is deterministic by construction.** A body is a function of its port
  values, and those values are already in the log. Determinism stops being a
  discipline the author must maintain.
- **The graph is a defunctionalized continuation.** When A awaits B, A's
  continuation *is* the edge B→A — already serializable, already inspectable,
  because defunctionalization is exactly what turns an unprintable closure into
  a printable record.

Open: whether side effects are also edges (to nodes that own them), which would
be elegant and would largely dissolve §11.1, or whether that purity is more
ceremony than a workflow tool can carry.

### 8.4 Scheduling

Replay demands deterministic scheduling. Green threads must therefore be a
single-threaded, round-robin fiber scheduler owned by the VM — *not* goroutines
or OS threads. This is the strongest argument for writing the VM rather than
embedding one.

### 8.5 Effects

Time, randomness, subprocess execution, and network calls are effects, injected
at the invocation boundary and recorded in the log. Open question: whether the
memoized value is the effect's *result* or whether the effect itself replays
(§11.1).

## 9. Design constraints

These are load-bearing, not aspirations.

1. **Every write is one line.** Recording a blocker mid-task must not take three
   commands, or it will not happen.
2. **Reads must pay off at low write density.** The graph will never be more
   than ~40% complete. `weft why` must be useful anyway.
3. **Tolerance over validation.** Unknown nodes, dangling refs, stale claims,
   and unverifiable third-party claims are normal states, not errors.
4. **One call to orient.** `weft show` gives an agent everything it needs about a
   neighbourhood without a second invocation.
5. **Canonical printing.** The s-expression / text form of a graph is what
   humans review and what goes in git. Identical graphs must print byte-
   identically, which makes node ordering a real design decision, not an
   afterthought. The tree in that output is a spanning tree plus labels; the
   labels are the edges the tree could not hold.
6. **The graph may contain cycles; the log never does.** Retries and generative
   nodes are loops in the graph and chains in the history. Time unrolls cycles,
   which is the same move as loop unrolling and the same move Temporal's history
   makes.

## 10. Non-goals

- Bidirectional GitHub sync. `import` is one-way and explicit.
- A daemon, server, or resident process (phase 3 at the earliest).
- Being a build system. Nodes model *work*, not compilation targets.
- Authority. The graph records claims; it does not arbitrate them.

## 11. Open questions

1. **Effect boundary (phase 2).** If `shell` is an effect, is its *output*
   memoized, or is the side effect itself replayed? Getting this wrong
   double-pushes to git. See §8.3 — modelling effects as edges may resolve it.
2. **Blocking granularity (phase 2).** If node X waits three days on a human, do
   dependents see X as running or blocked? Determines whether claims are
   per-node or per-fiber.
3. **Forced cycles.** Should `--force` exist on a rejected `depends-on` link,
   degrading `ready`/`path` to best-effort for that component? Phase 1 says no.
4. **Hash-consing bodies.** Hashing node bodies with references-as-hashes
   (Unison's discipline) makes structurally identical subgraphs unify
   automatically. Worth it in phase 1, or does it only pay off once bodies are
   code?
5. **What of an export belongs in git.** `weft export` settles the mechanical
   half: whole store, one fold, DOT, d2 or JSON, byte-identical across runs. It is
   whole-store deliberately, since edges cross namespaces and a scoped export
   would name nodes it had excluded — scoping needs a neighbour closure, not a
   filter. What stays open is §9.5's actual claim: committing DOT or JSON tracks
   a *rendering*, and diffs of it are noisy in the way §9.5 warns about, so the
   s-expression text form may still be the thing worth versioning. Nobody has
   yet committed an export to a repo and lived with the diffs, which is the only
   way to answer it.
6. **Observation history.** Only the latest observation renders, but the
   sequence stays in the log, and "what did this corpus look like in March" is
   a real lineage question. Whether as-of queries earn a flag, an export mode,
   or stay a log-reading tool's job is undecided; the first consumer that
   needs one decides it.

## Appendix — lineage

Recorded so the design decisions above can be checked against prior art.

- **Bigraphs** (Milner, 2009) — place/link as orthogonal structures. §4.2.
- **Double-pushout graph rewriting** (Ehrig et al., 1970s) — the formal shape of
  "a node emits nodes," including what happens to edges pointing at removed
  nodes. Relevant to phase 2 generative nodes and to §6.2.
- **Term graph rewriting / Clean** (Barendregt et al., 1987) — graph rewriting
  as a language's semantic foundation rather than its implementation.
- **Call-by-need calculi** (Wadsworth 1971; Ariola–Felleisen 1997) — a DAG of
  named, evaluated-once nodes *is* call-by-need with sharing made syntactic.
  Phase 2's memoization is not an optimization; it is what the model is.
- **Build Systems à la Carte** (Mokhov, Mitchell, Peyton Jones, 2018) — the
  scheduler taxonomy. Suspending + traces is the cell that supports dynamic
  dependencies, i.e. generative nodes.
- **Kahn Process Networks** (1974) — independent sequential processes joined by
  channels; output determinacy under arbitrary scheduling. The direct model for
  §8.3.
- **Unison** — content-addressed code as a DAG; the mature form of §11.4.
- **Sea of Nodes** (Click, 1995) — a production compiler's position that
  computation is not a tree and scheduling should be a late decision.
- **Dataflow architectures** (MIT TTDA, Manchester) — cautionary: the cost of a
  graph primitive is matching and locality, never compute. Expect the
  implementation to be flat arrays with an index.
