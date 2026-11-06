# weft, for agents

Paste this into a project's `CLAUDE.md` / `AGENTS.md`, or load it as a skill.
`weft help agent` prints it, so it ships with the binary.

`weft` is a durable graph of **work and how it connects**, shared by everyone
and every agent on this machine. One global store, no daemon, no server.

**Reach for it when you learn something the tracker cannot hold**: a bug nobody
filed, a dependency between two things in different repos, who is already on a
task, why something is blocked. Most nodes have no GitHub issue and never will.
Recording a blocker is one command; if you skip it, the next agent rediscovers
it from scratch.

## Orient — one call each

```
weft ls --ready              # open, unblocked, not taken by someone else; skips workstreams
weft ls --blocked            # the opposite
weft ls --mine               # what you hold a live claim on
weft ls --stale              # taken and abandoned: every claim expired (default TTL 7d)
weft ls --since 7d           # only what moved in the last week (also 72h, 2w)
weft ls --recent             # most recently moved first
weft show <id>               # a node and everything touching it, in ONE call
weft why <id>                # what blocks it, the route there, and who holds those blockers
                             # chokepoints first: a blocker other listed blockers wait on says so
weft rdeps <id>              # what waits on it through depends-on; it says so, and --via widens
weft path <a> <b>            # how two things connect; every kind is walked, and it says which
weft path <a> <b> --dir out  # does a actually lead to b, rather than merely connect to it
```

`ls` shows the current repo's namespace; add `--all-ns` for everything.
`show`, `why`, `rdeps`, and `path` are always global — a blocker in another repo
is still your blocker.

**`why` and `ls --ready` are only as good as the `done` states in the graph.**
They walk to the nearest *open* blocker, so on a graph where nothing is marked
done yet — a fresh map of a codebase, say — `why` returns the immediate parent
of whatever you ask about, every time. That looks broken and is not: with every
node open, the nearest open blocker really is one hop away. Record state as you
go, and mark the components that already ship as `done`. That is the moment
`ls --ready` starts answering "what is actually unbuilt here".

**Importing a repo is the fast way to get those states.** `weft import gh
acme/api --merged-since 30d` pulls the pull requests merged in that window in as
`done`, alongside the open issues. That applies when the merged PR is first
discovered: if its node already exists, import records the upstream observation
but never overwrites weft's state, so mark it with `weft done` yourself. Without
the window the whole-repo form fetches open items only, so a quiet repo imports
a graph with nothing done in it — which is the state described above.

**A surprising `path` is usually the edge kinds, not the graph.** `conflicts-with`
and `same-as` are symmetric, so they are free shortcuts between nodes that exist
to say they are *alternatives*, and a node with many `part-of` children puts
everything within two hops. `path` prints the kinds it walked; narrow it with
`--via depends-on,part-of` when you want only the directed ones.

**`rdeps` walks `depends-on` alone by default**, which is what the name means —
but on a real map most edges are `part-of` and `derived-from`, so an empty answer
is easy to read as "nothing depends on this" when it means "nothing depends on
this *through `depends-on`*". It prints the kinds it walked for that reason;
widen with `--via derived-from,part-of` when the question is broader than
dependency. A widened walk lists what is *reachable*, not what waits: each row
then names its relations to the node it was reached from — `(part of)`,
`(depends on, part of)` when both connect the pair — and rows deeper than one
hop name the node they hang from, since the indent alone cannot say. `show`
is the other way to see it: it lists inbound edges of every kind under
"led to".

**`--via` filters kind, not direction.** By default `path` is a connectivity
query: `--via depends-on` between two siblings that share one dependency finds a
route *through* it, and the reversed hop reads `blocks` rather than `depends on`,
which is easy to skim past and impossible to grep for. Add `--dir out` to ask the
reachability question instead — does a lead to b — or `--dir in` for the other
way. Symmetric kinds are still crossed both ways under `--dir`, because they have
no direction to restrict, and the `via:` line names the ones that were.

"Moved" means weft recorded something about the node: a state change, an edge, a
ref, a claim. It does **not** include `import` checking upstream — polling is not
progress, so a nightly sync never makes stale work look fresh. `show` prints both
ages, created and last moved.

## Namespaces — where work lands

A namespace groups nodes by project. It decides where `add` puts a new node and
which nodes a bare `ls` shows, and it is resolved fresh each time, in this order:

1. `--ns` on the command
2. `$WEFT_NS`
3. the longest matching `ns:<path> = <name>` line in `~/.weft/config`
4. the git remote as `owner/repo` — failing that, `default`

```
weft env                     # store, actor and namespace in use, and which rule chose each
weft ns                      # which namespace you are in, and which rule chose it
weft ls --all-ns             # every namespace, with a namespace column
weft export                  # the whole graph as DOT, in one fold
weft export --json           # the whole graph as {nodes, edges}
weft export --json --include all   # plus claims, node and edge source, and ref observation ages
weft export --d2             # as d2, part-of drawn as containers; render with: d2 graph.d2 graph.svg
weft view                    # the graph as an interactive HTML page; prints its path
weft view --only-ns <ns>     # a page holding one namespace, for sending to someone
```

**The default export says what exists, not how well anyone knows it.** Node
and edge `source`, a node's `claims`, and the age of each ref's upstream
observation are all omitted unless you ask: `--include claims,source,refs`, or
`all`. Ask for
them whenever a human will read the result — an inferred edge and an asserted
one are otherwise the same picture, and a two-month-old reading of an upstream
issue looks exactly like one taken a minute ago. The default stays as it is
because golden files rest on its exact bytes.

`export` is the whole store, never namespace-scoped: edges cross namespaces, so
a scoped export would point at nodes it left out. Prefer it over a `show` per
node — that is one fold per node at N different instants, so a concurrent write
gives you a graph that never existed.

**Do not write your own renderer.** `weft view` writes the graph as one HTML
page — search, namespace scope, fold, layout, click a node for detail — under
`$XDG_CACHE_HOME/weft/` and prints the path (`-o` to choose, `--open` to open
it). Its view state lives in the URL fragment, so what you send someone is a
link to the view rather than a description of it. It always carries the full
export (`--include all`), because the default export's silence is the opposite
of why you are drawing a picture for somebody. `--only-ns` builds the page from
one namespace, so the rest of the store is not in the file to be read; `--ns`
only picks the opening view. By default the page loads mermaid from a pinned
CDN build, so it draws only with network access. `--self-contained` inlines a
local `mermaid.min.js` instead, for offline use or a single file to hand over.
If that exits 7 with "could not find mermaid.min.js", it is the expected state
on a fresh machine, not a broken tool: drop `--self-contained`, or point it at
a copy with `--mermaid-js` or `$WEFT_MERMAID_JS`, or `npm install mermaid` in
the directory you run it *from* (discovery checks `./node_modules`). This
paragraph exists because an agent with only this snippet learns weft emits DOT
and JSON, reasonably concludes drawing is its own job, and writes something
worse.

**Run `weft env` before any bulk write.** It is read-only, creates nothing, and
answers the one question that is otherwise unanswerable without writing a node
and looking at where it landed: which store am I about to change. It also names
any `WEFT_*` variable you have set that weft does not read — the failure mode
that costs the most, because a plausible-looking variable that does nothing is
indistinguishable from one that works until you read the log. weft reads exactly
`WEFT_HOME`, `WEFT_NS` and `WEFT_ACTOR`.

Check `weft ns` before filing work in an unfamiliar checkout. It is read-only,
and it names the rule, so a surprising namespace can be traced to the mapping or
the stray `$WEFT_NS` that caused it.

Namespace is a label, never part of a node's identity, so edges cross freely and
nothing needs to agree on names.

In a multi-repo workspace prefer the config file. `$WEFT_NS` is per-shell, so it
follows you into the next repo and quietly files work under the wrong project.
Two traps worth knowing: the config beats the git remote on purpose, so a
catch-all on a parent directory also captures the repos beneath it unless you
restate them; and directories that are not their own repo inherit the enclosing
repo's remote, which silently merges them into one namespace.

## Record

```
weft add "title" --kind bug|task|spec|pr|issue|finding|note|workstream [--ref gh:o/r#12]... [--source observed|asserted|inferred] [--if-absent]
weft link <a> depends-on <b>        # also: conflicts-with, part-of, supersedes, derived-from, same-as
weft unlink <a> depends-on <b>      # take back an edge; no such edge is exit 0, not an error
weft ref add <id> gh:o/r#12
weft ref rm <id> gh:o/r#12
weft ref observe <id>               # re-stat file: refs; unchanged files write nothing
weft claim <id> [--holder user:who] [--note "..."] [--ttl 168h]   # TTL defaults to 7d
weft release <id>
weft done <id>        # --undo reopens
weft drop <id>        # --undo reopens
weft merge <into> <from>            # they were always the same thing; the first survives
weft import gh acme/api             # or acme/api#508 for one
weft import gh acme/api --merged-since 30d   # merged PRs too, as done
```

**`--source` matters.** `observed` = you read it from an API or a file.
`asserted` = a human said so. `inferred` = you concluded it. Roughly half a real
graph is inference, and one that cannot tell them apart stops being trusted.
Default is `asserted`, so say `--source inferred` when you are guessing.

**`--holder` names who is doing the work**, which is often not you. An agent
recording that a colleague seems to be on something writes
`--holder user:colleague --source inferred`.

**Nodes are append-only — there is no edit.** Get `--kind` right at `add` time;
fixing it means `drop` and re-`add`, which loses the node's edges. Refs are not
in that trap: `weft ref add` and `weft ref rm` change them on a node that already
exists. Both are exit 0 when the binding is already there or already gone, so
they are safe to run when you are unsure. `--ref` takes any `scheme:value`
(`file:docs/export.md`, `jira:AB-12`, `branch:fix/n-plus-1`), and `gh:`/`pr:`
are the ones `import` binds to.

**`weft ref observe` refreshes what the world says.** It stats and hashes the
node's `file:` refs into dated observations — `show` then reads
`(modified 2d ago, observed 5m ago)` — and an unchanged file writes nothing, so
it is safe to run on a schedule. Run it from a checkout of the node's
project: a relative `file:` path is read from that checkout's root and is
skipped anywhere else. `gh:`/`pr:` refs are observed by `import`; other
schemes are reported and skipped. The stamps are the artifact's own
clock: they never touch the node's state, and never move it in `ls --recent`.

**Give a node every identity it has.** `--ref` repeats, and each one resolves,
so a component can carry both the code anchor a reader wants and a stable key
that survives the file moving:

```
weft add "Frame.Run: the VM entry point" --kind spec --ref file:pkg/vm/vm.go#L827 --ref lgx:vm.framerun
```

If another live node already carries a ref you bind — or its `gh:`/`pr:` twin
for the same PR — the binding commands say so: `add`, `ref add`, and `import`
all print a note naming that node. The write still happens and the exit code
is 0; the note is the moment to decide whether it is the same work
(`weft merge`) instead of finding the duplicate several edges later.

**Make retried scripts explicit.** `add --if-absent` reuses a live node with
the same exact title in the same namespace, printing its id and appending
nothing. It is a writer's assertion that the title means the same work, not a
change to node identity. Refs are ignored when matching, and a dropped node is
not present, so the command creates the work again.

**A script that asserts and then drops** — the shape of "considered and
rejected" — needs two more flags to stay quiet on re-run: `--even-dropped` on
the add, so the dropped node counts as present instead of being minted again,
and `--if-present` on the drop (works on `done` too), so a node already in
that state appends nothing. With both, a re-run appends a byte-identical log.
An `--even-dropped` reuse also binds any `--ref` the node lacks, so a script
line that grew a ref since the node was first written converges instead of
leaving the ref pointing at nothing.

`link --if-absent` is the same tool for edges: the edge already being there is
success, and nothing is appended. Use it in anything that can run twice — a
scheduled job, a retried script, a derivation that recomputes. Plain `link`
records the assertion every time, and while the graph is identical either way
(the fold keeps the first), the log is not: without the flag a job that re-runs
grows the log on every pass, and nothing fails to tell you.

Naming nodes by ref is what makes a mapping script readable: every `link` can
then name its endpoints by ref instead of by captured id.

**When `import` fails on one item** (exit 7), record it by hand —
`weft add "..." --kind pr --ref gh:o/r#77 --source observed`. The ref is what a
later successful import binds to, so it will attach rather than duplicate.

## Address things

Anywhere an id is wanted you may pass a full id, **any unique prefix**, or an
exact ref (`gh:acme/api#412`). Ids in output are already at a safe prefix length,
so paste them straight back. A ref carried by both a dropped node and a live
one resolves to the live one — ambiguity counts live nodes only, so dropping a
claim and re-asserting it does not poison its name. Two *dropped* holders of
one ref are still ambiguous; `weft merge` is the cure there.

## Exit codes — branch on these, not on stderr

| Code | Meaning | What to do |
|---:|---|---|
| 0 | success | — |
| 1 | usage error | fix the command |
| 2 | not found | the thing does not exist; `weft add` it |
| 4 | ambiguous | more than one match; candidates are listed, pick one |
| 5 | rejected | validation failed, e.g. the edge would create a `depends-on` cycle |
| 6 | store error | the log is corrupt or locked; stop and tell a human |
| 7 | external tool failed | `gh` is missing, unauthenticated, or refused; or `view` found no local mermaid or could not open the page |

`--json` works on every command. Under it, errors are a JSON object on stdout
with `error`, `code`, and sometimes `candidates`.

## Things that will otherwise surprise you

- **Claims do not lock.** Two live claims on one node is reported, not refused.
  It means two people think they own it, and that is worth surfacing to them.
- **Incomplete is normal.** Unknown nodes, dangling refs, and stale claims are
  ordinary states, not errors. Do not try to clean them up.
- **`weft merge` is a judgment, not a dedupe.** Two agents filing the same bug
  make two real assertions. Merge when you *know* they are one thing; the merged
  id keeps working afterwards and resolves to the survivor.
- **Imported state is a cache, never the truth.** `show` prints what GitHub said
  and when (`closed (observed 3d ago)`). The node's own state is weft's, and
  upstream never overwrites it. Divergence is information.
- **A cycle is refused with the path printed.** `depends-on` must stay acyclic;
  `conflicts-with` may cycle freely.

## Which way an edge points

Always subject first: `weft link A <kind> B` says something about A.

```
weft link A depends-on B        # A waits for B      — must stay acyclic
weft link A part-of B           # A is a piece of B  — does NOT block; add depends-on too if it does
weft link A conflicts-with B    # symmetric, may cycle freely
weft link A supersedes B        # A replaces B
weft link A derived-from B      # A was discovered via B
weft link A same-as B           # they look alike; use merge when you are sure
```

**Get it wrong and take it back.** Modeling is iterative — you link, you read
the source, and you find the lineage ran the other way. `weft unlink A
depends-on B` retracts the assertion, and both events stay in the log, so the
mistake and the correction are both still readable. Retracting an edge that is
not there is exit 0, so it is safe to run when you are unsure. Endpoint order
does not matter for `conflicts-with` and `same-as`, which are symmetric.
