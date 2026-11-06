# weft

A durable graph of work — what you're blocked on, what's blocked on you, and how
any of it connects — shared by humans and the agents working alongside them.

**Status: phase 1 is complete.** Nineteen commands — `add`, `ls`, `link`,
`unlink`, `ref`, `show`, `claim`, `release`, `done`, `drop`, `why`, `rdeps`,
`path`, `merge`, `import`, `ns`, `env`, `export`, `view` — against one append-only JSONL log,
no daemon and no server. Agents start from `weft help agent`, which prints the
onboarding snippet (`docs/AGENT.md`).

```
make            # build dist/weft, stamped with the version
make check      # the whole verification ladder: vet, smoke, test
make install    # to $PREFIX, default ~/.local/bin
```

Build with `make`, not with a bare `go build`: the version is applied through
`-ldflags`, so a plain build reports `0.0.0-dev` and `weft --version` stops
answering the first question you ask when something surprises you.

## The problem

You ask an agent for the top three tech-debt items in a repo. It finds a filed
bug, a latent one nobody has written down, and a half-finished spec. You start
on the second. Then:

- an upstream issue turns out to track it, and a colleague is apparently already
  on it — no PR, direction unclear
- an open PR on an adjacent bug conflicts with the fix you're building
- review on that PR says not-green until point Z is addressed
- your task #7 depends on point Z
- addressing it properly is a stack of three PRs and one more issue

Now multiply by ten agents per person and several contributors.

Every tracker can name the filed issue. None of them can name the latent bug,
the unpublished branch, or "point Z inside that review comment" — and those are
the ones that actually block you. The expensive coordination happens at the
unwritten layer, which is exactly the layer no tool addresses.

## What weft does

A node asserts that some unit of work exists. It is **not** a mirror of a GitHub
object — most nodes start with no external identity and some never get one. Refs
(`gh:o/r#412`, `pr:o/r#77`, `branch:x`, `file:p#L10`) bind to reality later, if
reality catches up.

Edges are typed and few: `depends-on`, `conflicts-with`, `part-of`,
`derived-from`, `supersedes`, `same-as`. Claims record who is working on what,
including inferences about other people that may be wrong — and are marked as
inferences so you can tell.

Everything is an append-only JSONL log. The graph is a fold over it, which is
where history, audit, undo, and multi-writer support come from for free. One
static binary, no daemon, no server.

```
weft add "cache invalidation race" --kind bug --source inferred   # unfiled, found by an agent
weft link task7 depends-on review-z
weft link my-fix conflicts-with pr77
weft claim task7

weft why task7        # the blocking path, with who holds each hop
weft ls --ready       # what can actually be started right now
weft rdeps task7      # who is waiting on me
```

`weft why` prints the path, not the word "blocked" — a bare status isn't
actionable. (Real IDs are content-hash prefixes resolved git-style; the readable
names above are for illustration.)

## Design

Two decisions shape everything else:

**The log is the contract, not the binary.** It's plain JSONL, so other tools
read it, agents parse it, and the implementation stays replaceable.

**Adoption is a design constraint, not documentation.** Nobody maintains graphs.
If recording a blocker mid-task takes three commands, it won't happen; if
orienting takes five calls, agents stop reaching for the tool. So writes are one
line, `weft show` returns a whole neighborhood in one invocation, and the read
queries have to pay off while the graph is still half-empty — because it will
never be more than half-empty.

## Phases

1. **Claims graph.** Nodes, typed edges, claims, five queries. Zero compute.
   Independently useful, and the current target.
2. **The VM.** Node bodies become suspendable computations, durable across
   process death — each node an independent program whose only IO is its edges.
   That shape is Kahn Process Networks, and it makes suspension a blocking port
   read rather than a mechanism to design.
3. **Resident process, IPC.** Deferred. Nothing in phases 1–2 may assume it.

## Docs

- [`docs/SPEC.md`](docs/SPEC.md) — data model, storage, CLI, phase 2, design
  constraints, open questions, and an appendix recording the prior art behind
  each decision
- [`docs/EVENTS.md`](docs/EVENTS.md) — the log format: envelope, event identity,
  provenance, corrections, fold semantics, write protocol. The actual contract
- [`docs/BUILD.md`](docs/BUILD.md) — stack, platform, layout, line budget,
  milestones, and the verification ladder
- [`docs/getting-started.html`](docs/getting-started.html) — ten minutes, start to
  finish, with real transcripts. Open it in a browser; it is self-contained and
  every command in it is re-run by the test suite
- [`docs/advanced-walkthrough.html`](docs/advanced-walkthrough.html) — the sequel:
  two repositories, duplicate assertions and `merge`, blast radius, routes,
  inferred and expired claims, retractions, and the exit codes weft refuses on.
  Also re-run by the suite, failing commands included
- [`docs/AGENT.md`](docs/AGENT.md) — the onboarding snippet to paste into a
  project's `CLAUDE.md` / `AGENTS.md`. `weft help agent` prints the same thing,
  so it ships with the binary

## Seeing the graph

To look at the graph, `weft view --open` writes it as one interactive HTML
page under `$XDG_CACHE_HOME/weft/` and opens it: search with exclusion,
namespace scope, fold, four layout directions, click a node for detail. View
state lives in the URL fragment, so a filtered view is a link rather than
instructions for which dropdowns to set. The page always carries the full
export, so inferred edges draw differently from asserted ones, claims appear,
and an observation shows its age.

By default the page loads a pinned, integrity-checked mermaid build from
jsdelivr, so it needs the network to draw. `--self-contained` inlines a local
`mermaid.min.js` instead, for offline use or a file you hand to someone; find
one with `--mermaid-js`, `$WEFT_MERMAID_JS`, or `npm install mermaid`. Neither
mode falls back to the other: a failed local find is an error (exit 7), never a
quiet fetch. mermaid stays out of the binary; only the page template is
embedded.

`--only-ns acme/api` is the one to reach for when the page is going to
somebody: it builds the file from that namespace alone, so the rest of the
store is not in it to be read. An edge that leaves the scope keeps a stub of
its far endpoint — id and namespace, no work data — because a page that
quietly dropped those would assert that a node has no blockers when it has one
next door.

For a static picture, `weft export --d2 | d2 - graph.svg` draws `part-of` as
boxes inside boxes. d2 is your own install, not a dependency (`SPEC.md` §6.4).

## Tools

Outside the build on purpose — the binary's premise is a 2–5 ms cold start
(`BUILD.md` §1.1), and these carry dependencies it will not pay for. They read
`weft export --json`, which is the seam the log-is-the-contract design exists to
provide. Their tests run in `make check` as rung 6.

- [`tools/weft-html.py`](tools/weft-html.py) — the same page built from an
  export you already have (`weft export --json --include all | tools/weft-html.py`),
  with the same flags. It fills the template `weft view` embeds, and a test
  pins the two to the same output.
- [`tools/weft-range-context.py`](tools/weft-range-context.py) — markdown
  describing what a commit range touches in the graph: which issues it closes,
  what still blocks the work, what was learned doing it. A diff cannot say what
  a change is connected to, and the join is the issue and PR numbers the range
  already mentions

### Spikes

Measuring instruments, not tools. Each answered one design question and stopped,
so they are kept for the reasoning rather than to be run — the numbers in their
docstrings are what a decision was made against, and re-running them says whether
that decision still holds.

- [`tools/weft-regions.py`](tools/weft-regions.py) — what a container/absorption
  feature could actually collapse. The answer was no to the shape proposed and
  yes to a different one: the container is the node the children are already
  `part-of`, which is what the viewer's `grouped` fold now does

## Name

On a loom the **warp** is the fixed threads held under tension; the **weft** is
what you thread across them. The log is the warp, the graph is the weft.

It began as *DAG-Pipe*, which overclaimed — `depends-on` is acyclic but
`conflicts-with` isn't, so this is a graph containing a DAG. The invariant worth
naming turned out to be a different one: the graph may have cycles, the log
never does.

## License

MIT. See [LICENSE](LICENSE).
