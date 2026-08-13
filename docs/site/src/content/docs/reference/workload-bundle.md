---
title: Workload bundle schema
description: The v0.1 workload.yaml fields mast actually parses, including the budget block and its env-var overrides.
sidebar:
  order: 1
---

A **workload bundle** is the declarative operational profile for a mast
deployment: one YAML file naming the specialists, tool catalog, budget,
HITL policy, and edge trigger for one named workload. Bundles live at
`.agents/workloads/<name>.yaml` (see [.agents/ discovery](/reference/agents-discovery/))
or in a workload directory passed to `--workload=<path>`.

This page documents the **v0.1 subset** — exactly the fields the shipped
loader parses. The canonical full schema lives in
[`docs/orchestration-design.md`](https://github.com/go-steer/mast/blob/main/docs/orchestration-design.md);
fields beyond this subset (planner review knobs, isolation scope, bundle
learning) land with their subsystems.

## Example

```yaml
name: gke-triage
description: Autonomous triage of GKE cluster incidents.
mode: single_session

tool_catalog:
  mcp:
    - server: gke
  tools:
    - name: list_clusters
      mutating: false

specialists:
  - triage-classifier
  - CrashLoopBackOff
  - ImagePullBackOff
  - _fallback

budget:
  max_wallclock_seconds: 300
  max_turns: 20
  max_cost_usd: 5.00

hitl:
  require_approval: true

planner:
  enabled: false

edge_trigger:
  http:
    path: /inject
    auth: bearer

a2a:
  expose: true
  skill_name: incident-triage
  skill_description: Investigate GKE pod-failure incidents.
  auth:
    scopes: [incident-triage.invoke]

agui:
  expose: true
  endpoint_path: /agui/incident-triage
  description: Investigate GKE pod-failure incidents.
  session_model: per_thread
  auth:
    scopes: [incident-triage.run]
```

## Fields

| Field | Type | Notes |
|---|---|---|
| `name` | string | Required. Unique per deployment; used as the `workload` metric label. |
| `description` | string | Human-readable; used in operator UIs and logs. |
| `mode` | string | `single_session` (default) or `multi_session`. Multi-session is vocabulary-only in v0.1 — declared intent, honored when the multi-session substrate lands (v0.2). |
| `tool_catalog.mcp[].server` | string | MCP server references, by name from the deployment's [`mcp.json`](/reference/mcp-servers/) (HTTP or local stdio). Intersected against per-specialist tool allowlists at dispatch time. |
| `tool_catalog.tools[].name` | string | Tool name a per-tool policy override applies to. Names must be unique. |
| `tool_catalog.tools[].mutating` | bool | Overrides the tool's mutation classification for the recorded-effect outbox. Unknown tools — MCP tools included — default to **mutating** (annotations are advisory, and ADK drops MCP `readOnlyHint` before mast can see it); `mutating: false` un-gates a known-read-only tool. Omitted means no override. Every applied override is audit-logged at startup. |
| `specialists[]` | list of strings | Specialist names; resolve against the config root's `specialists/*.tmpl`. A roster with a SingleTurn classifier plus a `_fallback` Task specialist enables graph dispatch. |
| `budget` | block | See below. |
| `hitl.require_approval` | bool | When true, every specialist result pauses on a durable RequestInput interrupt until an operator resumes with a verdict. |
| `planner.enabled` | bool | v0.1 scaffold: switches the root agent to the supervisor-body planner with the bundle's specialists as its `invoke_specialist` roster (`--dispatch` is then ignored). The planner's `run_shape_*` vocabulary tools return `not_implemented` until v0.2. |
| `edge_trigger.http.path`, `.auth` | strings | Informational in v0.1 — the inject server declares its routes globally; per-workload path prefixes come later. |
| `a2a.expose` | bool | Opt this workload into the [A2A server](/mast/reference/cli/#a2a-server) surface (`--a2a-listen`). Default false — A2A exposure is an external contract, so it is never automatic. |
| `a2a.skill_name`, `a2a.skill_description` | strings | The skill id/name and human-readable summary published on the agent card. `skill_name` defaults to the workload name; `skill_description` to the workload description. |
| `a2a.input_schema`, `a2a.output_schema` | maps | A **mast-side** convention only: mast may validate inbound task inputs against them and fold them into the skill description. Spec `AgentSkill` has no schema fields, so they do **not** round-trip through the agent card as machine-readable schema. |
| `a2a.auth.required`, `a2a.auth.scopes` | bool, list of strings | Per-skill auth policy. `scopes` are enforced per call when a token validator is configured (`MAST_A2A_TOKEN`): a caller whose token lacks a scope is refused `403`. |
| `agui.expose` | bool | Opt this workload into the [AG-UI server](/mast/reference/cli/#ag-ui-server) surface (`--agui-listen`). Default false — like A2A, AG-UI exposure is a public turn-driving endpoint, so it is never automatic. |
| `agui.endpoint_path` | string | The HTTP path the workload's run endpoint is served at. Must start with `/`. Defaults to `/agui/<name>`. |
| `agui.description` | string | Surfaced in the `/agui/agents.json` discovery descriptor; defaults to the workload description. |
| `agui.session_model` | string | How a run maps to a mast session: `per_thread` (default — one continuing session per AG-UI `threadId`, matching chat UX) or `per_run` (a fresh session per `runId`, for stateless one-shots). The daemon always derives and namespaces the session id; a client never supplies a raw one. |
| `agui.input_schema` | map | A **mast-side** convention only: an optional JSON-Schema-shaped hint surfaced in the discovery descriptor so a client can render an input form. AG-UI's `RunAgentInput` has no schema field, so it does **not** constrain the wire input. |
| `agui.auth.required`, `agui.auth.scopes` | bool, list of strings | Per-endpoint auth policy. `scopes` are enforced per run when a token validator is configured (`MAST_AGUI_TOKEN`): a caller whose token lacks a scope is refused `403`. |

## Per-specialist model override

A specialist `.tmpl` may declare its own model in frontmatter, so a roster
can run cheap analysts under a frontier synthesizer instead of billing
every specialist at one tier:

```yaml
---
description: Inspects pod state and returns a 3-bullet digest.
model: claude-haiku-4-5
---
```

Specialists that declare no `model:` inherit the process model
(`--model`). An override is dispatched by model id, exactly like
`--model` — the `--provider` alias only disambiguates the Anthropic
backend — so an override **may name a different provider** than the
parent. Resolution is memoized per model id, so eight specialists on one
tier share one client.

Two behaviors worth knowing before you tier a roster:

- **A declared override that cannot be resolved fails startup.** It is
  never quietly downgraded to the parent's model, because a bundle that
  reads as tiered while running everything on one tier is worse than one
  that refuses to start. Credentials for every provider named in the
  roster must resolve at construction.
- **Offline fakes collapse overrides.** Under `--model=echo`,
  `scripted`, or `toolactor`, every override resolves back to that fake,
  so a tiered bundle still runs credential-free in smoke and acceptance
  runs.

Naming a concrete model id binds the bundle to that provider — worth
weighing before putting an override in a bundle other deployments fork.

## Per-specialist report contract

A specialist `.tmpl` may declare the shape of what it returns:

```yaml
---
description: Diagnoses OOMKilled container terminations.
output_schema: ../schemas/finding.json
---
```

The value is a path to a JSON-Schema document (`.json`, `.yaml` or
`.yml`), resolved **relative to the `.tmpl` file's own directory** — so
`../schemas/finding.json` means the same thing whether the roster sits in
a workload bundle or a shared config root.

It is a file reference rather than an inline block on purpose. A report
shape is a contract between the specialist that produces it and every
consumer that reads it; inlining it in one specialist's frontmatter makes
that contract private to that specialist, and a roster of a dozen
diagnosers ends up with a dozen copies that drift. The shipped
`gke-triage` bundle points all twelve diagnosers at one
`schemas/finding.json`.

**A violation is a refusal, not a warning**, and the shape is the same in
both agent modes:

- **`Task` mode** — the schema becomes the auto-injected `finish_task`
  declaration. A non-conforming call comes back to the model as a
  validation error naming the offending key, and the model can correct
  itself within its turn budget.
- **`SingleTurn` mode** — the reply is validated on the way out. A
  violation refuses the delegation; the caller sees an error naming the
  key, with no result attached.

Either way non-conforming output never becomes the specialist's answer,
and the caller — not just a log line — learns that it was refused.

The document is read and checked when the roster loads, not when a
specialist first runs, so a broken contract fails startup with the file
named. Refused at load time: unknown keys (a misspelled `propertys:`
would otherwise produce a schema that constrains nothing), a node with no
`type`, an array with no `items`, an object with no `properties`, a
`required` name that is not a declared property, and a top level that is
not an object.

That last one is a mast restriction rather than an ADK one, because ADK's
two modes disagree about non-object schemas: `Task` wraps a scalar under
a `result` key, while `SingleTurn` unmarshals the reply into an object
before it consults the schema at all, so a scalar contract can never
validate there. Rather than let one document mean two things, mast
requires an object. A report contract is an object anyway.

Mast does not interpret the schema. There is no built-in report type; the
shape belongs to the workload.

## Budget fields

| Field | Meaning |
|---|---|
| `budget.max_turns` | Cap on **model calls** per session. One "turn" = one model call — a Task specialist looping through five model calls before `finish_task` has spent five turns, not one. Absent/0 = unlimited. |
| `budget.max_wallclock_seconds` | Bounds each whole turn with a context timeout. |
| `budget.max_cost_usd` | Session-cumulative cost ceiling, derived by the budget meter from streamed usage metadata (flat per-1K-token spike pricing in v0.1). |

Crossing a ceiling cancels the run context mid-turn, aborts in-flight
model/tool work, and increments `mast_budget_trips_total`. Budgets compose:
workload-level and per-specialist budgets both apply, tightest cap wins.

### Per-specialist ceilings

A specialist `.tmpl` may declare its own budget in frontmatter, alongside
the model override and report contract above:

```yaml
---
description: Diagnoses OOMKilled container terminations.
budget:
  max_turns: 6
  max_wallclock_seconds: 60
  max_cost_usd: 0.25
---
```

`max_turns` and `max_cost_usd` are ceilings on *that specialist's* spend,
checked on every model call it authors; `max_wallclock_seconds` bounds
each activation of its node under graph dispatch. Composition with the
workload's ceilings is by construction rather than arithmetic: each call
is measured against the specialist's ceiling and the workload's, and
whichever is crossed first stops the run. When one call crosses both, the
error names the specialist — the more specific fact, and the one an
operator acts on. A specialist that declares a `model:` override is priced
at that model's rate, so a cheap analyst's tokens are not billed at the
synthesizer's.

Two limits are worth knowing. Metering reads the event stream, so a
ceiling is crossed *by* the call that reports it: the cap bounds total
spend within one call's overshoot, it does not pre-authorize a call.
And a crossed specialist ceiling stops the session rather than just that
specialist — handing the coordinator a refusal it could route around
needs a pre-call seam mast does not have yet.

### Env-var overrides

Scalar budget values can be overridden process-wide (applies to every
bundle loaded in the invocation, overrides file values unconditionally;
set-but-unparseable is a fatal load error):

```
budget.max_cost_usd          → MAST_BUDGET_MAX_COST_USD
budget.max_wallclock_seconds → MAST_BUDGET_MAX_WALLCLOCK_SECONDS
```
