# The event log format

**Normative.** This is the contract — the artifact other tools read and agents
parse. `SPEC.md` describes behavior; this describes bytes.

Scope: the envelope, event identity, provenance, the correction model, fold
semantics, and the write protocol. Individual event kinds are specified as the
milestones that emit them land (§3, §9). That staging is safe because §1.3
defines extensibility first: **once the envelope and the unknown-kind policy are
fixed, every later event kind is an additive, non-breaking change.**

## 1. The envelope

### 1.1 Bytes

- One JSON object per line. UTF-8, no BOM.
- Lines terminated by `LF` (`0x0A`). Never `CRLF`.
- No insignificant whitespace within a line; no blank lines.
- A committed event is a complete line **including** its terminating `LF`. A
  trailing fragment with no `LF` is not an event (§5.3).
- **No floating-point numbers anywhere.** All numeric fields are integers; all
  other scalars are strings. This is what keeps canonicalization tractable
  (§2.1).

### 1.2 Required fields

Every event carries these, without exception:

| Field | Type | Notes |
|---|---|---|
| `v` | int | Schema version. Currently `1`. |
| `ev` | string | Event kind, `namespace.verb` (`node.add`, `edge.retract`). |
| `id` | string | 64 lowercase hex chars. See §2. |
| `at` | string | RFC 3339 with offset **and sub-second precision**, e.g. `2026-07-31T09:14:22.418573-07:00`. Host clock at append. See §2.5. |
| `by` | string | The actor that asserted this event. See §4. |
| `source` | string | `observed` \| `asserted` \| `inferred`. See §4. |

### 1.3 Extensibility policy

This section is the reason the rest can be staged. Readers **must**:

- **Ignore unknown fields.** Never error, never drop the event. A field you do
  not recognize belongs to a newer writer.
- **Ignore unknown `ev` kinds**, and continue the fold. Report them on stderr
  under `--verbose`; do not fail. An unknown kind is a fact you cannot
  interpret, not corruption.
- **Refuse a log whose `v` exceeds the reader's known version.** Exit nonzero
  with a clear message. Silently mis-folding a newer log is the one failure this
  policy will not tolerate.

`v` increments **only** for changes that would make an older reader compute a
wrong graph. Adding a field, or adding an event kind, is not such a change.

## 2. Event identity

### 2.1 The preimage

`id` is `sha256` over the **canonical serialization of the event with the `id`
field absent**, rendered as 64 lowercase hex characters.

Canonical serialization is RFC 8785 (JSON Canonicalization Scheme) restricted to
the no-floating-point subset permitted by §1.1:

- Object keys sorted ascending by UTF-8 code point
- No whitespace between tokens
- Integers in shortest form, no leading `+`, no leading zeros, no exponent
- Strings with the shortest legal escaping, `\uXXXX` lowercase hex only where
  escaping is mandatory

Excluding `id` from its own preimage is what makes the definition
non-circular — SPEC's earlier "hash of the creating event" was circular as
written.

### 2.2 This is event identity, not content identity

`at` and `by` are inside the preimage. Two agents adding a node with identical
titles therefore produce **different** ids.

That is deliberate. Two people independently noticing the same bug have made two
assertions, and deciding they are the same thing is a judgment — `weft merge`
(§9) — not an automatic hash collision. Content identity would silently
conflate independent observations, which is exactly the failure the provenance
model exists to prevent.

### 2.3 Node ids

**A node's id is the id of its `node.add` event.** Node ids and event ids share
one space, as commit ids and object ids do in git. `edge.add`'s `from`/`to`
therefore reference `node.add` event ids.

### 2.4 Display prefixes

Full 64-character ids are stored in the log, always. Prefixes exist only for
display and input.

- Minimum display width is 7 characters, **expanded as needed** until unique
  within the store — git-style, computed at render time, never stored.
- Input accepts any prefix that resolves uniquely; an ambiguous prefix is an
  error listing every candidate.

A *fixed* 7-character display is unsafe at the scales in `BUILD.md`: a uniform
28-bit prefix over 10,000 ids has roughly a 17% chance of at least one
collision.

### 2.5 Timestamp precision is load-bearing

`at` must carry sub-second precision. This is a correctness requirement, not a
matter of taste.

Ids hash the whole record including `at`, and the fold treats a repeated id as a
crashed-and-retried write (§5.2). At one-second granularity, two genuinely
distinct assertions made within the same second — a node reopened and reopened
again, a claim renewed, the same state re-asserted — produce byte-identical
records, hash to the same id, and the second is **silently discarded** as a
duplicate.

Found the way these things usually are: a test asserted a node was `open` after
`drop --undo` and got `dropped`, because the preceding `done --undo` in the same
second had already written that exact event.

## 3. Event kinds

Milestone 1 emits `node.add` and `edge.add`. The correction events (§3.3–3.5)
are specified now because a log with no correction path is a log that eventually
gets deleted.

### 3.1 `node.add`

```json
{"v":1,"ev":"node.add","id":"<64hex>","ns":"acme/api","kind":"bug",
 "title":"N+1 in report generation","body":null,
 "at":"2026-07-31T09:14:22-07:00","by":"agent:scanner","source":"inferred"}
```

`ns` and `kind` required; `title` required and non-empty; `body` optional and
inert in phase 1 (SPEC §2).

`by` and `source` here are the node's own provenance (SPEC §4.4), and the fold
carries them onto the node **unrevised for its lifetime**, the way `created`
never moves while `touched` does. A later `node.state` or `ref.add` carries
provenance for *its* assertion, never the node's: who asserted the work into
existence, and how they knew, is a fact about the `node.add` alone.

### 3.2 `edge.add`

```json
{"v":1,"ev":"edge.add","id":"<64hex>","kind":"depends-on",
 "from":"<64hex>","to":"<64hex>",
 "at":"...","by":"user:matt","source":"asserted"}
```

`from` and `to` are node ids. Duplicate triples are **always legal in the log**,
and the fold collapses identical `(kind, from, to)` triples, retaining the
earliest by file order for provenance. A reader must never assume a triple
appears once — that is the normative half, and it does not vary.

Whether a *writer* appends a duplicate is a choice, not a rule. `weft link`
asserts unconditionally; `weft link --if-absent` checks the triple inside the
write transaction and appends nothing when it is already there (§3.9). Neither
is deduplication of the log: the second is a writer declining to write, the
bytes it would have written are still valid, and an implementation may offer it
or not. Nothing downstream may behave differently for a log written one way or
the other.

### 3.3 `edge.retract`

```json
{"v":1,"ev":"edge.retract","id":"<64hex>","target":"<64hex>",
 "at":"...","by":"user:matt","source":"asserted"}
```

`target` is the id of the `edge.add` event being retracted. This is why every
assertion carries its own id.

Retracting an unknown or already-retracted target is a **no-op, not an error**
(SPEC §9.3). Retraction is not deletion: both events remain in the log, and the
history of the mistake is preserved along with who made it.

### 3.4 `ref.add` / `ref.retract`

```json
{"v":1,"ev":"ref.add","id":"<64hex>","node":"<64hex>","ref":"gh:acme/api#412",
 "at":"...","by":"agent:scanner","source":"observed"}
{"v":1,"ev":"ref.retract","id":"<64hex>","target":"<64hex>","at":"...","by":"...","source":"..."}
```

`ref.add` is also the general "attach a ref to an existing node" affordance the
CLI was missing.

### 3.5 `node.state`

```json
{"v":1,"ev":"node.state","id":"<64hex>","node":"<64hex>","state":"done",
 "at":"...","by":"user:matt","source":"asserted"}
```

`state` ∈ `open` | `done` | `dropped`. Reopening is a `node.state` back to
`open`, not a retraction. Last-by-file-order wins.

### 3.6 `claim.add` / `claim.release`

```json
{"v":1,"ev":"claim.add","id":"<64hex>","node":"<64hex>",
 "holder":"user:colleague","expires":"2026-08-07T09:14:22.418573-07:00","note":null,
 "at":"...","by":"agent:scanner","source":"inferred"}
{"v":1,"ev":"claim.release","id":"<64hex>","target":"<64hex>","at":"...","by":"...","source":"..."}
```

- `holder` — the subject (§4), distinct from `by`
- `expires` — absolute RFC 3339, or `null` for a claim that never ages
- `claim.release`'s `target` is a `claim.add` id, so letting go is a
  compensating event: who claimed what, and when they released it, both stay

**`expires` is absolute, never a duration.** Staleness must be a property of the
record rather than of when someone happens to read it, and a writer that stored
"7 days" would leave every reader to guess from when.

A claim written without an explicit expiry gets a default TTL applied **at write
time**, so the log is self-describing. An unbounded default was rejected: an
agent that dies mid-task would leave a claim nobody can release, and every later
reader would treat abandoned work as taken.

Settling what SPEC §6.1 deferred:

- **live** — not released, and `expires` is null or in the future
- **expired** — not released, `expires` has passed
- **stale** — a node whose claims all expired: work someone took and abandoned.
  A node nobody ever claimed is *unclaimed*, not stale, and the two want
  different responses

### 3.7 `node.merge`

```json
{"v":1,"ev":"node.merge","id":"<64hex>",
 "into":"<64hex>","from":"<64hex>",
 "at":"...","by":"user:matt","source":"asserted"}
```

`into` survives. `from` becomes an alias of it. Both are node ids.

**The direction lives on this event, not on a `same-as` edge.** SPEC §6.2 said B
was "recorded as `same-as` A", which is unimplementable: `same-as` is symmetric
(SPEC §4.2), so the fold normalizes its endpoints and the record of *which node
survived* is exactly the thing it cannot hold. `same-as` remains a user
assertion meaning "these look like the same work" without folding them.

Folding is an operation on the **index**, never on the log:

- **`from` keeps every field it was asserted with** — its own `ns`, `kind`,
  `title`, `state`, `by`, `source`, and its `node.add` event. It is still a
  node, still resolvable, and still carries what it said. The merge does not
  move `by`/`source` onto `into` — the survivor keeps its own, the same rule
  as `created` — so the merged node's provenance is read through the
  survivor's `merged_from` (in `show --json`), which carries it for exactly
  this reason: resolution redirects the merged id, and listings omit merged
  nodes, so nothing else can surface it.
- **Refs, edges, and claims union onto `into`.** An edge keeps its original
  `edge.add` id and provenance; only its endpoint moves, so the record of who
  asserted it and when survives the merge. Rewriting edges as new `edge.add`
  events would reattribute every one of them to whoever ran `merge`.
- **Every later event naming `from` applies to `into`.** `node.state`,
  `ref.add`, `claim.add`, and both endpoints of `edge.add` canonicalize through
  the alias chain before they are applied.
- **A self-edge produced by folding is dropped, not stored.** If `A depends-on
  B` and B folds into A, nobody asserted `A depends-on A`. An edge asserted as a
  self-edge in the first place is still retained, including across a merge — the
  log said it.
- **Refs, claims, and observations all move**, and claims keep file order. An
  observation left behind is not cosmetic: it breaks §3.9 and drops the age
  annotation that makes a cached upstream state judgeable (§3.8).
- **A merge folded before its endpoints still folds consistently.** §5.1 makes
  the fold a pure function of the bytes, so a `node.merge` preceding either
  `node.add` must leave the alias and the node's merged marker agreeing — not
  one view listing two nodes while another routes writes to one.
- **A rewritten edge that collides with an existing triple collapses into it**,
  earliest by file order retained, exactly as §3.2 specifies for `edge.add`.

**Conflicting fields resolve by "`into` wins", with no flag and no loss.**
Nothing is discarded, because `from` still carries its own values on a node that
still resolves. `state` is not special-cased to take the more resolved of the
two: closing a duplicate is the ordinary way people discover two nodes are the
same, so "more resolved wins" would let closing the duplicate mark real work
done.

Merging can close a cycle in an acyclic kind (§SPEC 4.2) that neither node had.
That is checked inside the write transaction (§6) and rejected with the path.
Every path a merge creates runs through the survivor, since it is the only node
whose adjacency changes.

`from == into`, and a pair already folded, are both **no-ops, not errors**
(SPEC §9.3). This is also what makes an alias cycle impossible: a merge is only
ever recorded between two nodes that are already canonical and already distinct.

**Reversal is not implemented.** SPEC §6.2's rule stands — a merge is reversed
by a compensating event, never by truncating the log — but no such event exists
yet, and `node.merge.retract` is reserved in §8 rather than shipped. The reason
is worth recording: undoing a merge cannot be done by bookkeeping, because
events that landed on the survivor *after* the merge have to go back to the node
they named. That needs replay — a two-pass fold — and the second pass is paid on
every read and every validating write, which is the cost §6.1 already names as
the first thing likely to break the `BUILD.md` §1.4 budget. It is additive under
§1.3 when someone needs it: a reader change, not a format change.

### 3.8 `ref.observe`

```json
{"v":1,"ev":"ref.observe","id":"<64hex>",
 "node":"<64hex>","ref":"gh:acme/api#412",
 "state":"closed","title":"N+1 in report generation",
 "created":"2025-11-03T18:20:11Z","modified":"2026-06-30T08:02:47Z",
 "at":"...","by":"agent:import","source":"observed"}
```

What an external object said, and when we looked. This is what §8 reserved as
"`import` provenance markers".

**A cache with provenance, never authority.** The node's own `title` and `state`
are weft's assertions; this records somebody else's. The two diverging is
*information* — "upstream closed this, we have not" is the thing worth
surfacing, not an inconsistency to reconcile away. SPEC §10 makes bidirectional
sync a non-goal, and a field that upstream can overwrite is that sync with extra
steps.

- **Last by file order wins.** An observation supersedes the previous one rather
  than accumulating. What is wanted is "what does upstream say now, and how old
  is that". The history is not lost: every observation stays in the log, which is
  the artifact other tools read.
- **`at` is load-bearing, not decoration.** An observation rendered as though it
  were current invites a decision on data nobody re-checked. Readers must show
  the age.
- **Written only when the payload changes.** Every payload field is compared
  against the last observation; `at` deliberately is not, since it differs on
  every look. A `modified` that moved while `hash` did not is still a change and
  still written: the payload records what upstream says, and judging a touch to
  be noise is the reader's call, not the writer's. This is what makes `weft
  import` and `weft ref observe` idempotent in the strong sense below.
  Two consequences are intended, not leaks. Observations written before
  `created`/`modified` existed lack them, so the first import after upgrading
  re-observes every bound `gh:`/`pr:` ref once. And `modified` is upstream's
  `updatedAt`, which moves on any upstream activity, so a comment or a label
  on an issue is a change and gets one new observation.

The payload vocabulary is shared across ref schemes: `state`, `title`,
`created`, `modified`, `hash`. An observation carries the subset its scheme can
honestly report — `gh:` and `pr:` yield `state`, `title`, `created`,
`modified`; `file:` yields `created`, `modified`, `hash`. Absence means "this
scheme does not report it", never "reported as blank". All of these are
additive under §1.3 — no `v` bump, and an older reader folds the observation
exactly as it always did.

- **`created` and `modified` are the artifact's own clock; `at` is ours.** The
  payload timestamps are what upstream says about itself, normalized to
  RFC3339 at whatever precision upstream reports; `at` records when we looked.
  Neither substitutes for the other: chronology over artifacts — layout,
  lineage, cohorts — reads the payload clock, trust reads `at`. Only `at`
  participates in event identity (§2.1), so only `at` carries §2.5's precision
  requirement.
- **`hash` is self-describing**: `sha256:` followed by 64 lowercase hex. The
  prefix names the algorithm, so a future algorithm is a new prefix rather
  than a guess about 64 hex characters.
- **A changed `created` is signal, not noise.** `created` should never move;
  re-observing an address and finding a different one — or a changed `hash`
  behind an unchanged `modified` — means the artifact at that address is not
  the artifact that was bound: replaced wholesale, not edited. The new
  observation still supersedes (last by file order wins, above) and the
  disagreement is reported, never refused — the log records what the world
  said, and SPEC §9.3 rules out turning the world's inconsistency into our
  error.

### 3.9 Import and observation are idempotent on the bytes

Re-running an import — or a `ref observe` — against an unchanged artifact must
append **nothing**, leaving the log byte-identical. That is stronger than
"creates no duplicate node", and it is the property that makes putting either
on a schedule safe.

It requires that the writer diff against the fold rather than lean on the fold
to absorb repeats. The fold *does* tolerate a redundant `ref.add` — §3.4 leaves
the node's ref list unchanged — but the event still lands in the log and the log
still grows. Tolerance on the read side is not idempotence on the write side.

The same rule binds edges, and it is the case the fold hides best. A repeated
`edge.add` collapses into the existing triple (§3.2), so every query keeps
returning the identical answer while the log grows on every run. `link
--if-absent` is the write-side form: it checks the triple inside the same
transaction the append would land in and writes nothing when the edge exists.
Any writer that can run twice needs it, and no query will tell you that you
forgot.

Two cases are legitimately not idempotent:

- Upstream changed. That is the point.
- The ref is on more than one node. A writer cannot pick a winner, because that
  judgment is `weft merge` (§2.2). It must report the collision, and it must
  skip that item rather than abandon the batch — one collided ref blocking a
  scheduled import of everything else is the intolerance §9.3 of SPEC rules out.

Idempotence has to survive a merge, which is a sharper constraint than it looks:
a merge moves the ref onto the survivor, so it must move the *observation* too
(§3.7). A survivor holding a ref with no observation behind it reads as
never-observed, and the next import writes a fresh observation and reports a
change that did not happen.

## 4. Provenance

Three distinct concepts, previously conflated:

- **`by` — the actor that asserted the event.** Always the resolving process's
  identity, never a third party.
- **`source` — how that actor came to believe it.** `observed` (read from an
  API or the filesystem), `asserted` (a human said so), `inferred` (concluded).
- **`holder` — the subject of a claim**, i.e. who is said to be doing the work.
  Appears only on claim events (§9).

An agent recording that a colleague appears to be working on something emits
`by: "agent:scanner"`, `holder: "user:colleague"`, `source: "inferred"`. Those
are three different facts and collapsing any two of them loses the ability to
distinguish "observed thirty seconds ago" from "guessed on Tuesday."

**Actor resolution**, first match wins:

1. `$WEFT_ACTOR` — the mechanism agents use
2. `actor` in the config file
3. `user:` + `git config user.email`
4. `user:` + `$USER`

The literal string `me` is CLI sugar. It is resolved to a concrete actor before
the event is written; **`me` never appears in the log.**

## 5. Fold semantics

### 5.1 Order

Events apply in **file order** — the byte order of lines in the log — not in
`at` order. Wall clocks move backwards; file position does not. `at` is
provenance, never sequencing.

The fold is a pure function of the log bytes: identical bytes must produce an
identical graph, on any implementation.

### 5.2 Duplicates and dangling references

- **Duplicate `id`:** the first occurrence in file order applies; later ones are
  ignored. This makes a crashed-and-retried write idempotent.
- **Reference to an unknown node id:** the event is retained and the reference
  dangles. Queries must tolerate dangling references (SPEC §9.3) — they are the
  normal state of a graph that is deliberately incomplete.
- **Retraction of an unknown target:** no-op (§3.3).

### 5.3 The partial final record

A writer killed mid-append leaves a trailing fragment.

- **Readers:** if the final line lacks a terminating `LF`, discard it. It was
  never a committed event.
- **Readers:** if any *non-final* line fails to parse, that is corruption. Exit
  nonzero. Do not silently skip it.
- **Writers:** while holding the lock, truncate the file back to the last `LF`
  before appending. Safe precisely because the fragment was never committed and
  the lock is held.

### 5.4 Derived timestamps

The fold derives two timestamps per node from `at`. Both are **derived state, not
sequencing** — §5.1 still holds, and nothing orders by them without breaking the
tie in file order.

- **`created`** — the `at` of the node's `node.add`.
- **`touched`** — the **maximum** `at` over every event that applies to the node
  after canonicalization (§3.7): `node.state`, `edge.add`, `edge.retract`,
  `ref.add`, `ref.retract`, `claim.add`, `claim.release`, `node.merge`.

Three rules make this well-defined:

- **`ref.observe` is excluded.** It records that an external object was looked
  at, not that the work moved (§3.8). Including it would make a scheduled import
  advance every node it polled, and §3.9's byte-idempotence guarantee would stop
  meaning what it says: the log would not grow, but every query keyed on recency
  would report activity that did not happen.
- **An assertion the fold discards does not touch.** A duplicate `id`, a
  redundant `ref.add` that changes no ref list, an `edge.add` collapsing into an
  existing triple: each leaves recency alone, for the same reason it leaves the
  graph alone.
- **Maximum, not last write.** `at` is a wall clock, and §5.1 already refuses to
  sequence by it. Taking the max means no single backdated event can lower a
  node's recency, and it makes `touched` independent of the order the events
  arrive in — **for the events that apply at all.**

  That qualifier is §5.2's, not a weakening of this rule. An event naming a node
  the fold has not yet seen dangles, and a dangling event contributes no
  timestamp; reordering it past the `node.add` it names therefore changes the
  result, exactly as it changes every other field. Recency inherits that limit
  rather than escaping it. Within it, any two orderings of the same events
  produce the same `touched`.

`node.merge` folds the merged node's `touched` onto the survivor, since the
survivor now owns that work. `created` is **not** merged: it states when that
node was asserted, and the merged node keeps its own.

## 6. The write protocol

The review's P0: locking only "for the duration of append" does not make a
read-validate-write sequence atomic. Two processes can each independently
validate opposite `depends-on` edges, then each append, producing the cycle both
had just rejected.

**Mutations that validate against current state** — every edge, merge, and
import — hold an exclusive lock across the whole sequence:

```
flock(LOCK_EX) → truncate any partial record → fold → validate → append → fsync → unlock
```

**Mutations that validate nothing** (`node.add`) hold the lock for
`append → fsync` but may skip the fold.

**Reads take no lock** and apply §5.3.

The lock is `flock(2)` `LOCK_EX` on the log file itself; no sidecar. `fsync`
happens before release — this is the durable artifact and the millisecond is
worth it.

### 6.1 The cost, stated up front

Every validating mutation folds the whole log under an exclusive lock. Write
throughput therefore degrades linearly with log length, and concurrent agents
serialize behind it.

This — not `weft show` — is what will break the `BUILD.md` §1.4 budget first,
and it means the validation index may be needed earlier than SPEC §5's
"no index until it hurts" implies. Measure it at milestone 1 rather than
discovering it at milestone 8.

## 7. Conformance fixtures

Committed under `testdata/conformance/`, each a log plus its expected folded
graph. Any implementation claiming to read weft logs must reproduce these.

Required from milestone 1:

- Minimal valid log; empty log; log of one event
- Unknown field present — must be ignored
- Unknown `ev` kind — must be ignored, fold continues
- `v` higher than the reader's — must refuse, nonzero
- Duplicate event id — second ignored
- Dangling `from`/`to` — retained, tolerated
- Retraction of unknown target — no-op
- Partial final record — discarded by the reader, truncated by the writer
- Corrupt non-final line — hard error
Canonicalization vectors — unicode escapes, key ordering, integer forms — are
pinned as unit tests in `internal/event`, not as fixtures: they are assertions
about the *preimage bytes* (§2.1), which a folded-graph expectation cannot
express.

From milestone 8:

- `node.merge`: the alias holds, a folded edge re-points, a self-edge produced
  by folding is dropped, and a later event naming the merged node lands on the
  survivor

From milestone 9:

- `ref.observe`: the latest observation wins, and a diverging observation leaves
  the node's own state untouched

From milestone 11:

- `ref.observe` carrying `created`, `modified`, and `hash`: the payload folds
  onto the ref, and a reader that predates the fields folds the observation as
  before (§1.3 exercised on a real kind, not just an invented field)
- A re-observation whose `created` disagrees with the last: latest wins, node
  fields untouched, nothing refused

## 8. Not yet specified

Deliberately deferred, each landing with its milestone. They are additive under
§1.3 and require no `v` bump.

| Reserved `ev` | Milestone |
|---|---|
| `node.merge.retract` | deferred — see §3.7 |

Merge conflict resolution is settled in §3.7 and no longer open.
