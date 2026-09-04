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
│   ├── triage-classifier.tmpl
│   ├── OOMKilled.tmpl
│   ├── ...
│   └── change-executor.tmpl
└── schemas/
    ├── finding.json         the diagnosers' report contract
    └── change-report.json   the executor's
```

The point of that shape is that the operational profile is reviewable. You
can answer "what can this thing do to my cluster" by reading two files, and
mast checks the answer at startup rather than at incident time.

## A specialist

A specialist is a `.tmpl` file: YAML frontmatter, then the prompt body. It
runs as a sub-agent, invoked as a tool by whatever root shape the roster is
built for.

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

### Braces in the body are not punctuation

The prompt body is a template, and `{...}` is its one piece of syntax. A
bare identifier in braces is a **session-state lookup**, and an
`artifact.`-prefixed one is an artifact load — both resolved before the
prompt is sent. So a body that says "investigate `{project}`" is asking for
a state key named `project`, and if nothing set one the run ends with
`state key does not exist`.

mast refuses those at load instead, naming the file, the line and the key,
because a prompt full of Kubernetes and GCP examples is exactly where
braces live and the runtime error names none of it.

Almost nothing you write is affected. Only a valid state name resolves, so
manifests and jsonpath are literal and always were:

| In the body | What happens |
| --- | --- |
| `{"spec":{"replicas":1}}` | literal — not an identifier |
| `{.status.phase}` | literal — not an identifier |
| `{app: web}` | literal — the space makes it invalid |
| `{app:web}` | **refused** — `app:` is a state scope, `web` a key |
| `{project}` | **refused** — a state lookup |
| `{artifact.report}` | **refused** — mast runs no artifact service |

Doubling the braces is not an escape: `{{project}}` is trimmed to the same
key. To write a literal, use another bracket — `<project>`. To genuinely
ask for session state, mark it optional — `{project?}` renders as nothing
when the key is unset instead of ending the run. An optional marker does
*not* rescue an artifact placeholder, which fails for want of a service
before the marker is consulted.

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
