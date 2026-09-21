---
title: Workloads and specialists
description: The workload bundle as the unit of deployment, what a specialist declares about itself, and the four dispatch shapes a roster can be built for.
sidebar:
  order: 1
---

A mast deployment is a **workload bundle**: one YAML file naming a roster
of specialists, the tools they may reach, the budget they run under, the
approval policy, and how an incident gets in. Everything else — which
model, which dispatch shape, which specialist handles what — is declared in
that bundle and the specialist files beside it, not compiled in.

```
examples/workloads/gke-triage/
├── workload.yaml            the bundle
├── specialists/
│   ├── triage-classifier.specialist.md
│   ├── OOMKilled.specialist.md
│   ├── ...
│   └── change-executor.specialist.md
└── schemas/
    ├── finding.json         the diagnosers' report contract
    └── change-report.json   the executor's
```

The point of that shape is that the operational profile is reviewable. You
can answer "what can this thing do to my cluster" by reading two files, and
mast checks the answer at startup rather than at incident time.

## A specialist

A specialist is a `<name>.specialist.md` file: YAML frontmatter, then the
prompt body. It is not a Go template and never was — nothing substitutes
into it, and a `{{ ... }}` in the body is refused at load. It runs as a
sub-agent, invoked as a tool by whatever root shape the roster is built
for.

```yaml
---
description: Diagnoses OOMKilled container terminations.
mode: Task
capability: read_only
model: gemini-2.5-flash
output_schema: ../schemas/finding.json
budget:
  max_turns: 6
  max_cost_usd: 0.25
tools:
  mcp:
    - server: gke
      tools: [get_k8s_resource, describe_k8s_resource, get_k8s_logs]
---

You diagnose OOMKilled terminations. Read the pod, its limits, and its
recent events, then report a finding...
```

Five of those lines are worth understanding as concepts rather than fields:

- **`mode`** — `Task` runs a full tool-calling loop until the specialist
  calls `finish_task`. `SingleTurn` answers in exactly one model call and
  carries no tools, which is what makes it the right shape for a
  classifier — and the only mode a `bounded` roster accepts. `Chat` is the
  conversational mode used by operator-facing
  surfaces.
- **`capability`** — `read_only` (the default) or `change_executor`. This
  is enforced at construction, not by the prompt; see
  [approvals](/concepts/approvals/).
- **`model`** — a per-specialist override, which may name a *different
  provider* than the rest of the roster. Its portable alternative is
  **`tier`** (`small` / `mid` / `frontier`), which says how much model the
  step is worth and lets the running provider name the id. A spec declares
  one or the other, never both. See [providers](/concepts/providers/).
- **`output_schema`** — a JSON-Schema file the specialist's report has to
  satisfy. A violation is refused and comes back to the model as a named
  error, so malformed output never becomes the answer.
- **`budget`** — ceilings on this specialist's own spend, composed under
  the workload's. See [budgets](/concepts/budgets/).

Exact semantics and every field: [workload bundle
reference](/reference/workload-bundle/).

### The extension changed in v0.8, and `.tmpl` stopped loading in v0.9

These files were called `<name>.tmpl` through v0.7.0. They were never Go
templates — `text/template` is imported by one package in mast, for the
planner's instruction, and it reads none of them — and the name invited
authors to write substitutions that would never fire.

:::caution[`.tmpl` no longer loads]
v0.8 accepted it with a startup warning. **v0.9 refuses it**
([#349](https://github.com/go-steer/mast/issues/349)): a directory holding
one fails to load, and mast names every file and the rename.

```
specialists: 2 file(s) in .agents/specialists use the .tmpl extension,
which loaded with a warning in v0.8 and was removed in v0.9:
Evicted.tmpl, OOMKilled.tmpl
	rename each to <name>.specialist.md — the stem is the specialist name
	your workload.yaml already lists, so renaming the files is the whole
	migration
	see https://github.com/go-steer/mast/issues/292
```

If you are coming from v0.7 you never saw the warning, because it only
ever existed in v0.8. This is the message instead.
:::

To migrate, rename; nothing inside the files changes, and the specialist
name is the stem either way:

```sh
cd .agents/specialists
for f in *.tmpl; do mv "$f" "${f%.tmpl}.specialist.md"; done
```

Do not leave the old copy behind. One specialist under both extensions
still fails the load — now because the `.tmpl` half is refused outright,
which is a better answer to the same mistake: through v0.8 both loaded and
picking one would have been an alphabetical accident, with the stale copy
usually winning.

mast refuses these files rather than ignoring them on purpose. An ignored
file is an *absent* specialist, so the error you would get instead is
`references specialist "OOMKilled" not found in .agents/specialists` — for
a file sitting in that directory, spelled correctly, with the wrong
suffix. And a specialist your `workload.yaml` does not name by hand would
simply disappear from the roster with nothing said at all.

### A key mast does not recognise is refused

Frontmatter is parsed strictly: misspell a key and the file fails to load,
naming the key. The reason is `tools:`. An absent `tools:` block means the
specialist inherits **every** MCP server the workload wires, so a
misspelled `toosl:` does not narrow it to the three tools the file lists —
it hands over the whole surface, and nothing downstream can tell that
apart from an author who meant to inherit. (`capability:` is the milder
case: absent resolves to `read_only`, so a typo there fails toward the
safe value.)

A specialist takes no `schema_version` of its own — it is only ever
reached through a bundle that names it, so [the bundle's
version](/reference/workload-bundle/#schema_version--and-why-an-unknown-key-is-refused)
governs the roster.

### Braces in the body are just braces

Write what you want the model to read. The body reaches it byte for byte —
a shell variable in `"${MAST_HOME}/bin/mast"`, a JSON shape you want
emitted, a `kubectl -o jsonpath={.status.phase}` — with no syntax of its
own and nothing substituted in at any layer.

:::note[Prompts were templates through v0.9, and are not any more]
This was not true before. mast handed prompts to a *template* field of the
underlying agent runtime, which resolved every `{...}` against session
state before each request: a bare identifier in braces was a state lookup
that ended the run with `state key does not exist` when nothing had set
the key, and injected the value into your prompt when something had. mast
worked around it by refusing such bodies at load. Both the substitution
and the refusal are gone — prompts are sent verbatim
([#464](https://github.com/go-steer/mast/issues/464)).
:::

The one thing that does not survive the change is the **optional marker**.
`{project?}` used to be the supported way to ask for session state, and
there is no longer any way to ask: session state is not reachable from a
prompt. Because it would otherwise go quiet — rendering as the literal
text `{project?}` rather than doing what the file says — a body that still
contains one is refused at load, naming the file, the line and the key:

| In the body | What happens |
| --- | --- |
| `{"spec":{"replicas":1}}` | literal |
| `{.status.phase}` | literal |
| `{app: web}` | literal |
| `{app:web}` | literal |
| `{project}` | literal |
| `{{project}}` | literal |
| `{artifact.report}` | literal |
| `{project?}` | **refused** — used to inject session state; nothing does now |
| `{artifact.report?}` | **refused** — same, and it never loaded anything either |

To carry a value into a specialist, put it in the request rather than the
prompt, or have the caller build the instruction with the value already in
it. If you were relying on `{project?}`, drop the marker and the braces
and write the text you meant.

## Four dispatch shapes

The same roster can be driven four ways. The shape belongs to the roster —
a bundle declares `dispatch:`, and `--dispatch` overrides it only when an
operator actually typed the flag — because whether a roster is safe to run
concurrently is a property of the roster, not of how the daemon happened to
be launched.

### `coordinator` — one agent delegating

The root is an LLM holding each specialist as a tool. It reads the
incident, picks a specialist, delegates, and summarizes what comes back.
It can consult more than one, and it can ask a follow-up.

Best when the routing decision benefits from judgment, or when one incident
may need two opinions. The cost is a model call spent on coordination, and
a root that can in principle wander.

### `graph` — a classifier routing to a node

A `SingleTurn` classifier reads the incident and names a specialist; the
workflow graph routes to that specialist's node. Roster shape:
`Start → classify → route → run_<specialist>`.

Best when the roster is a dispatch table — twelve failure modes, one
specialist each — and you want the routing to be cheap, legible, and
identical every time. Two properties follow from it being a graph rather
than a conversation:

- **Interrupt ids are deterministic per specialist** (`approve-OOMKilled`),
  so an operator tool can construct one without reading the session.
- **A resume re-enters at `Start`.** The graph re-runs from the classifier
  rather than picking up where it parked, so what a run knows across a
  pause is what it wrote down: the route it dispatched on, and the answer
  given at each gate. Both are recorded in session state for exactly that
  reason. An already-answered gate passes straight through on the later
  turn without re-running its specialist. See [two
  gates](/concepts/approvals/#two-gates-and-how-many-questions-they-add-up-to).
- **Specialist nodes are terminal, with one structural exception.** A node
  runs and the graph ends — that is what makes the shape predictable. The
  exception is the remediation edge: a finding that carried a
  `proposed_change` the operator approved is routed on to the roster's
  change executor, which receives those exact calls. The condition is a
  property of the finding, not an instruction in a prompt. See [the change
  set](/concepts/approvals/#the-change-set--approving-the-call-not-the-prose).

A roster needs a classifier and a `_fallback` specialist to be routable
this way; an incident the classifier cannot place goes to `_fallback`
rather than to nothing.

### `fanout` — the whole roster at once

Every specialist investigates the same incident concurrently, bounded by
`fanout.max_concurrency`, and one reserved `_synthesis` specialist merges
what comes back into the single report an operator approves. An analyst
that returns nothing is reported to synthesis as *silent*, not quietly
dropped.

Best for a standing audit — "tell me everything wrong with this namespace"
— where you want breadth rather than a routing decision.

Fan-out rosters are **read-only by construction**, checked at startup: every
branch runs before the one approval gate, so a mutation inside a branch is
one no operator was offered the chance to refuse. A roster whose analysts
can reach a mutating tool is refused with the tool named, and so is one that
grants a whole MCP server without enumerating its tools — under
[default-deny-unknown](/concepts/tools-and-mcp/) an un-enumerated grant *is*
a grant of mutating tools.

The shipped GKE triage roster is deliberately one of the refused ones: it
carries a change executor. Fan-out ships its own read-only example
(`examples/workloads/ns-audit`) rather than converting the anchor.

### `bounded` — one cheap call, one schema-forced report

A roster of exactly one `SingleTurn` specialist, built as a single node
with nothing above it. There is no orchestrator in the shape, so there is
nothing that can delegate, retry, or take a second turn: the run is one
model call, and `Result.Usage.ModelCalls`, the daemon's
`session_model_calls` log field, and `mast_model_calls_total` all say `1`.
The specialist declares an `output_schema:`, and its reply is validated
against that schema before the turn ends.

Best when a workload's value *is* that it cannot get expensive — a
standing classification that runs on a trigger and must cost a known,
small amount. Pair it with `tier: small` so the price is portable across
providers instead of pinned to one vendor's id.

Four things are refused at startup, because each one silently un-bounds
the run: a roster that is not exactly one specialist (the error prints the
count and the names), a specialist not in `SingleTurn` mode, a specialist
with no `output_schema:`, and `planner.enabled: true` — the planner being
the orchestrator this shape is defined by not having.

`dispatch: auto` **never infers this shape.** A one-specialist roster is an
ordinary coordinator, and a cost ceiling nobody declared is not a favor.
The example is `examples/workloads/bounded-triage`.

## Choosing

| You want… | Shape |
|---|---|
| One incident, one right specialist, cheap and repeatable routing | `graph` |
| Judgment in the routing, or several specialists on one incident | `coordinator` |
| Breadth over routing — audit everything, merge into one report | `fanout` |
| A provable ceiling — one cheap call, a report forced to a schema | `bounded` |

There is also a `planner` scaffold — a supervisor-body root whose
`run_shape_*` vocabulary returns `not_implemented` until the reference-graph
library lands. It is declared, not finished; the [roadmap](/roadmap/) says
where it sits. Its one working door, `invoke_specialist`, runs each
specialist on a runner of its own — which changes nothing about what a
dispatch costs you, and, since v0.6, nothing about what a spent cap does
either: it stops the specialist and hands whoever dispatched it the
reason, here as a `"status": "halted"` result the planner reads. See
[budgets](/concepts/budgets/#a-spent-specialist-closes-one-path-not-the-session).
The
watchdog watches those dispatches too, and a trip there does stop the
session; see
[it watches inside a planner dispatch too](/concepts/interop/#it-watches-inside-a-planner-dispatch-too).

## What is checked before the daemon serves

A roster is validated at construction, so a misconfiguration is a startup
error naming the file rather than an incident that behaves oddly:

- a `read_only` specialist that can reach a mutating tool → refused
- a fan-out roster with a mutating analyst, or an analyst with no tool
  allowlist, or no `_synthesis` → refused
- a `tools.mcp` entry naming a server the workload's `tool_catalog.mcp` does
  not declare → refused, naming the specialist and the servers that do
  exist. An allowlist is applied by dropping what does not match, so a
  mistyped server name grants nothing and used to say nothing; the *tool*
  half of the same mistake needs a `tools/list` and is warned about on first
  use instead. See [a name that matches
  nothing](/reference/workload-bundle/#a-name-that-matches-nothing)
- a `model:` override whose credentials do not resolve, or a `tier:` the
  running provider cannot answer → refused
- a spec declaring both `model:` and `tier:` → refused, with the file named
- a malformed `output_schema` document → refused, with the file named
- a bounded roster that is not exactly one `SingleTurn` specialist with an
  `output_schema:`, or one that also enables the planner → refused, naming
  what it found
- a graph roster with no classifier or no `_fallback` → not routable
- a planner roster holding a `change_executor` while `hitl.on_mutation`
  asks for the write to be gated → refused, because the gate cannot reach
  inside a dispatch. **This one is permanent**, not a gap waiting to be
  wired: an approval comes back through the session event log, and a
  dispatch's session is private and in-memory by design. The message
  names three ways forward — run the same roster under `coordinator` or
  `graph`, or set `on_mutation: apply` and accept that the writes fire
  (they are still recorded). [The write gate](/reference/write-gate/)
  has the full argument.

Startup also logs every `change_executor` in the roster, so "which
specialists here can change my cluster" is one log line instead of an
intersection of three files.

## A bundle built outside this repo

The examples in `examples/workloads/` are shaped to demonstrate one thing
each. For a bundle that was written to do a job rather than to illustrate a
feature, see
[**go-steer/mast-sre-agent**](https://github.com/go-steer/mast-sre-agent) —
a GKE incident-triage roster ported from a four-agent Python ADK service:
nine read-only diagnosers behind a `SingleTurn` classifier, one change
executor holding the only mutating tools, a tool catalog probed from a
live `tools/list` against Google's hosted MCP endpoints, and a report
schema for each of the two roles.

It is worth reading for what it measured as much as for its shape. It runs
against a real cluster and records what that cost — how far apart two
identical runs land, which reads are large enough to dominate a budget —
which is the evidence behind [sizing a
ceiling](/concepts/budgets/#sizing-one).
