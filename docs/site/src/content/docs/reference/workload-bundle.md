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

safety:
  watchdog: enforce

planner:
  enabled: false

edge_trigger:
  http:
    path: /inject
    auth: bearer
  scheduled:
    interval: 15m
    jitter: 45s
    prompt: Sweep every namespace for pods in CrashLoopBackOff and report what you find.

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
| `tool_catalog.tools[].precondition` | block | Optional. The re-read that decides whether an approval of this tool's call is still fresh when the call finally fires — see [`precondition:`](#precondition--what-makes-an-approval-stale) below. Only consulted for calls covered by a `change_set` grant; a call the operator is answering right now needs no freshness check. |
| `tool_catalog.tools[].precondition.read` | string | Required in the block. The tool to re-read with. It must be declared `mutating: false` in the same catalog — a freshness check that changes the cluster is not a check — and since an unlisted tool is mutating by default, an undeclared read fails at startup rather than at the incident. |
| `tool_catalog.tools[].precondition.args` | map | Fixed arguments for the read, e.g. `{namespace: prod}`. |
| `tool_catalog.tools[].precondition.args_from` | map | Arguments taken from the change itself: `{name: deployment}` reads *the read's* `name` from *this call's* `deployment`. This is what lets each call in a set be checked against its own object. |
| `tool_catalog.tools[].precondition.fields` | list of strings | Dotted paths into the read's result that must not have moved. Omitted means the whole result is digested. For an MCP tool every path starts `output.` — that is how a structured MCP result arrives. |
| `specialists[]` | list of strings | Specialist names; resolve against the config root's `specialists/*.tmpl`. A roster with a SingleTurn classifier plus a `_fallback` Task specialist enables graph dispatch. |
| `dispatch` | string | The root shape this roster is built for: `coordinator`, `graph`, `fanout`, `bounded`, or `auto`. Empty leaves the choice to the caller. `auto` never picks `bounded` — a cost ceiling is declared, never inferred. A shape is a property of the roster, not of how the daemon happened to be launched, so the bundle is where it belongs — `--dispatch` overrides it only when an operator actually typed the flag. |
| `fanout.max_concurrency` | int | Under `dispatch: fanout`, how many analyst branches run at once. `0` (omitted) means the default, **4**; a negative value means unbounded. Ignored under any other dispatch. |
| `budget` | block | See below. |
| `hitl.require_approval` | bool | When true, every specialist result pauses on a durable RequestInput interrupt until an operator resumes with a verdict. |
| `hitl.on_mutation` | string | What happens before a call that would change something: `require_approval` (**the default** — the call parks on a durable interrupt, with its arguments, and fires only once an operator approves it), `apply` (run it, unattended), or `dry_run` (never run it; report what would have happened). Because this defaults to gating, a bundle that says nothing about mutation does not get to write; unattended writes have to be asked for. The block may also be spelled `hitl_policy:`; setting both is an error. See [the write gate](/reference/write-gate/) for the verdict an operator sends back. |
| `hitl.change_set_ttl` | duration | How long an approval given with `scope: change_set` authorizes the set's remaining calls for. Default `10m` — far longer than an approve-then-execute round trip, far shorter than the span over which an operator forgets what they approved. It is the backstop, not the check: what an approval is really bounded by is the [`precondition:`](#precondition--what-makes-an-approval-stale) the tool declares. |
| `safety.watchdog` | string | The runaway-loop posture this workload ships with: `warn` (log only), `feedback` (**mast's default when nothing declares one** — tell the model on its next turn), or `enforce` (also cancel the turn in flight and refuse every later turn until an operator resets). Unset is not `warn`: it means *leave it to the host*, which is what keeps `--watchdog` able to override a bundle in both directions. Precedence is `--watchdog` > `safety.watchdog` > the default, and the daemon logs which won at startup. Declare `enforce` on a workload whose tool loop is bounded by construction; see [where the posture comes from](/concepts/interop/#where-the-posture-comes-from). |
| `planner.enabled` | bool | v0.1 scaffold: switches the root agent to the supervisor-body planner with the bundle's specialists as its `invoke_specialist` roster (`--dispatch` is then ignored). The planner's `run_shape_*` vocabulary tools return `not_implemented` until v0.2. |
| `edge_trigger.http.path`, `.auth` | strings | Informational in v0.1 — the inject server declares its routes globally; per-workload path prefixes come later. |
| `edge_trigger.scheduled.interval` | duration | Required in the block. How often the workload wakes itself, with nothing calling in — `15m`, `1h`, `24h`. Minimum `1s`. A malformed or missing value is a load error naming the file, because a cadence that fails to parse at runtime is a workload you believe is running and that never wakes up. See [`scheduled:`](#scheduled--a-workload-that-wakes-itself) below. |
| `edge_trigger.scheduled.jitter` | duration | Optional random offset added to each fire, drawn afresh every time. Defaults to a tenth of the interval, capped at `30s`; `0s` is honored as a declaration. Must be shorter than the interval. It is not decoration: N replicas started by one rollout otherwise wake on the same second. |
| `edge_trigger.scheduled.prompt` | string | What the workload is being woken up to do. Delivered as prose after the wake-up envelope, so it reaches the roster as you wrote it. A schedule with no prompt runs the roster against the envelope alone, which is a workload guessing at its own job. |
| `monitor.collect[].tool` | string | Required in each entry. A wired tool mast runs **on its own behalf** at the top of each scheduled cycle, before the model is woken; the results arrive in the wake-up envelope under `collected`. Requires an `edge_trigger.scheduled` block — a collection with no cadence never runs. No tool named here may be reachable from any specialist's allowlist, and the daemon refuses to start if one is. See [`monitor:`](#monitor--the-facts-a-cycle-gathers-for-itself) below. |
| `monitor.collect[].args` | map | Literal arguments for the call, e.g. `{transitions: "new,escalated,resolved"}`. There is no `args_from`: a collection call is not about any particular object, so there is nothing to map arguments from. |
| `monitor.collect[].as` | string | The key this call's result is filed under in the envelope. Defaults to the tool name; needed only when one workload collects from the same tool twice. Two entries filed under one key is a load error. |
| `monitor.transitions_from` | string | Optional. Names the `monitor.collect` key whose result carries the run-to-run classification — what is new, escalated, resolved since the last cycle. That result is parsed into the envelope's `transitions` block instead of being passed through raw. Must name a key some `collect` entry files a result under, or it is a load error. It names *where* the classification comes from, never what it may say: mast has no vocabulary of transition classes. See [`transitions_from:`](#transitions_from--the-classification-comes-from-the-tool) below. |
| `monitor.notify.conversation` | string | Required when a `notify:` block is present. Where a speaking cycle posts — the conversation id switchboard knows, e.g. `#sre-oncall` or a platform id. Requires an `edge_trigger.scheduled` block, for the same reason `collect` does: a cycle speaks at the end of a cycle, so with no cadence it never speaks. The ingress URL and its bearer token are the **daemon's** (`--notify-url`, `MAST_NOTIFY_TOKEN`), not the bundle's — deployment facts, so one bundle moves between staging and production unedited. See [`notify:`](#notify--speaking-only-when-something-changed) below. |
| `monitor.notify.digest_after` | duration | Optional deadman. After this long with nothing sent, the next cycle speaks whether or not anything changed. Omitted or `0s` = no deadman, and the workload may stay silent indefinitely. Wall-clock, not a count of quiet cycles: a monitor that has been quiet for a week is otherwise indistinguishable from one that died a week ago. |
| `monitor.ack.tool` | string | Optional. The wired tool an operator's acknowledgement is forwarded to, which arms `POST /monitor-ack` on the daemon; without the block that route answers `404`. Unlike `collect` and `notify` it needs **no** cadence — an ack arrives from an operator, not from a cycle. Fenced like a collect tool (reachable from no roster), and refused if it is also a `collect` tool: a cycle that acked what it just classified would suppress findings nobody asked to suppress. See [`ack:`](#ack--taking-an-acknowledgement-back) below. |
| `monitor.ack.args` | map | Literal arguments merged **under** the two mast supplies from the request itself, `subject_key` and `ack_by`. Deployment policy goes here — which cluster, how long a suppression lasts. Pinning either reserved name is a load error: a bundle that named the acker would name them for everyone. |
| `a2a.expose` | bool | Opt this workload into the [A2A server](/reference/cli/#a2a-server) surface (`--a2a-listen`). Default false — A2A exposure is an external contract, so it is never automatic. |
| `a2a.skill_name`, `a2a.skill_description` | strings | The skill id/name and human-readable summary published on the agent card. `skill_name` defaults to the workload name; `skill_description` to the workload description. |
| `a2a.input_schema`, `a2a.output_schema` | maps | A **mast-side** convention only: mast may validate inbound task inputs against them and fold them into the skill description. Spec `AgentSkill` has no schema fields, so they do **not** round-trip through the agent card as machine-readable schema. |
| `a2a.auth.required`, `a2a.auth.scopes` | bool, list of strings | Per-skill auth policy. `scopes` are enforced per call when a token validator is configured (`MAST_A2A_TOKEN`): a caller whose token lacks a scope is refused `403`. |
| `agui.expose` | bool | Opt this workload into the [AG-UI server](/reference/cli/#ag-ui-server) surface (`--agui-listen`). Default false — like A2A, AG-UI exposure is a public turn-driving endpoint, so it is never automatic. |
| `agui.endpoint_path` | string | The HTTP path the workload's run endpoint is served at. Must start with `/`. Defaults to `/agui/<name>`. |
| `agui.description` | string | Surfaced in the `/agui/agents.json` discovery descriptor; defaults to the workload description. |
| `agui.session_model` | string | How a run maps to a mast session: `per_thread` (default — one continuing session per AG-UI `threadId`, matching chat UX) or `per_run` (a fresh session per `runId`, for stateless one-shots). The daemon always derives and namespaces the session id; a client never supplies a raw one. |
| `agui.input_schema` | map | A **mast-side** convention only: an optional JSON-Schema-shaped hint surfaced in the discovery descriptor so a client can render an input form. AG-UI's `RunAgentInput` has no schema field, so it does **not** constrain the wire input. |
| `agui.auth.required`, `agui.auth.scopes` | bool, list of strings | Per-endpoint auth policy. `scopes` are enforced per run when a token validator is configured (`MAST_AGUI_TOKEN`): a caller whose token lacks a scope is refused `403`. |

## Fan-out rosters

`dispatch: fanout` runs the whole roster against one incident at the same
time and merges what comes back:

```yaml
dispatch: fanout
fanout:
  max_concurrency: 2
specialists:
  - dns
  - rbac
  - quota
  - _synthesis
```

Three constraints the loader enforces at startup, rather than letting a
run discover them:

- **`_synthesis` is a reserved specialist name and is required.** It is
  the one that receives every analyst's finding and writes the single
  report an operator approves. A roster without it is refused; every
  other Task specialist in the roster is an analyst.
- **Analysts must be read-only.** Every branch runs *before* the one
  approval gate, so a mutation in a branch is a mutation no operator was
  offered the chance to refuse. A roster whose analysts can reach a
  mutating tool is refused with the tool named — either drop it, or, if
  it really is read-only, classify it with `tool_catalog.tools[].mutating:
  false`. (`request_operator_input` passes the read-only check but still
  cannot be in a branch: a branch has no gate to pause on.)
- **Each analyst must name the tools it needs.** A branch with no tool
  allowlist is refused rather than silently inheriting the catalog.

An analyst that returns nothing is reported to `_synthesis` as silent, not
dropped. Approving the merged report finishes the run without re-running
any analyst — including after the daemon restarts, since branch events are
in the session log the run state is reconstructed from.

The shipped [GKE triage bundle](/quickstart/unattended-triage/) is *not* a fan-out
roster — it carries a change executor, and a branch check refuses even a
declared one, since a branch has no gate to pause on — so fan-out ships its
own read-only example at `examples/workloads/ns-audit`.

## Bounded rosters

`dispatch: bounded` is for the workload whose value is that it cannot get
expensive: one cheap model call, a report forced to a schema, and a step
count an operator can read off the meter rather than infer from how long
the run took.

```yaml
dispatch: bounded
specialists:
  - incident-report
```

```yaml
---
# specialists/incident-report.tmpl
description: Classifies one incident and returns the finding report.
mode: SingleTurn
tier: small
output_schema: ../schemas/finding.json
---
```

The roster is built as a single node with no orchestrator above it, so
there is nothing in the shape that can delegate, retry, or take a second
turn. The specialist's reply is validated against `output_schema:` before
the turn ends — a model that answers in prose fails the run instead of
returning an unparseable paragraph that reads like success.

Four things are refused at startup, each naming what it found, because
each one silently un-bounds the run:

- **A roster that is not exactly one specialist.** The error prints the
  count and the names.
- **A specialist not in `SingleTurn` mode.** A Task specialist runs a
  tool loop, which is however many calls it takes. A spec with no `mode:`
  at all is reported as `Task (the default for a spec with no mode:)` —
  the author wrote nothing and the default is what has to be named.
- **A specialist with no `output_schema:`.** Without it, one call buys a
  paragraph nothing downstream can read.
- **`planner.enabled: true`.** The planner is the orchestrator the shape
  is defined by not having.

`dispatch: auto` never infers this shape: a one-specialist roster is an
ordinary coordinator, and imposing a cost ceiling the bundle did not ask
for is not a favor. The worked example is
`examples/workloads/bounded-triage`, on the same `finding.json` report
contract the [GKE triage bundle](/quickstart/unattended-triage/) uses.

## Per-specialist capability

A specialist declares whether it may change anything:

```yaml
---
description: Applies one approved remediation and reports what it did.
capability: change_executor
---
```

The values are `read_only` and `change_executor`. **`read_only` is the
default**, so a specialist that says nothing about capability gets the safe
one.

Mast refuses to build a roster in which a read-only specialist can reach a
mutating tool, naming the specialist, the tools, and the fix. Three shapes
are refused:

- it names a mutating tool in `tools.mcp[].tools` or `tools.builtin`;
- it grants itself a whole MCP server (`- server: gke` with no `tools:`),
  since whatever that server adds later is unreviewed;
- it declares no `tools.mcp` at all while the bundle ships a
  `tool_catalog` — inheriting the catalog inherits everything mutating in
  it.

Fix any of them by enumerating the read-only tools the specialist needs, by
writing `mcp: []` if it needs none, or by declaring `capability:
change_executor` if it really is meant to change things. `SingleTurn`
specialists are exempt; the mode carries no tools.

"Mutating" is the same test the write gate (`hitl.on_mutation`, above) and
the effect log use — including its default-deny-unknown rule, so a tool your
`tool_catalog` has not classified counts as mutating and will fail the
roster rather than the incident. Classify the catalog with
`tool_catalog.tools[].mutating`.

Every `change_executor` in a roster is logged at startup, so the answer to
"which specialists here can change my cluster" is one log line rather than
an intersection of three files. The shipped GKE triage bundle logs exactly
one.

This is a stronger guarantee than prompt wording. A diagnoser told "do not
mutate anything" is one persuasive incident away from doing it; a diagnoser
that holds no mutating tool has nowhere to go.

## Per-specialist model: `model:` and `tier:`

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

Naming a concrete model id binds the bundle to that provider. When what
you mean is "this step is not worth the frontier model" rather than "this
step needs Haiku specifically", declare a tier instead.

### `tier:` — the portable spelling

A specialist may declare how much model it is worth and let the running
provider answer with a concrete id:

```yaml
---
description: Inspects pod state and returns a 3-bullet digest.
tier: small
---
```

The values are `small`, `mid`, and `frontier`. Mast resolves the tier
against the provider it is actually running (`pkg/taskclass.ModelForTier`),
so the same bundle reads the same and costs the right thing on either
backend:

| tier | Gemini | Anthropic |
|---|---|---|
| `small` | `gemini-3.5-flash-lite` | `claude-haiku-4-5` |
| `mid` | `gemini-3.5-flash` | `claude-sonnet-5` |
| `frontier` | `gemini-3.7-flash` | `claude-opus-5` |

Which provider a tier resolves against is the one mast dispatches on: the
`--provider` alias when you passed it, otherwise the root model id's own
prefix. A root model whose provider cannot be told from either — say a
custom id under no known prefix — fails startup asking for `--provider`,
rather than guessing a vendor and billing you for the guess.

Everything above holds for tiers too: an unresolvable tier fails startup
instead of falling back to the parent's model, resolution is memoized per
resolved id (twelve `tier: small` analysts share one client), and offline
fakes collapse every tier back to the fake so the bundle still runs
credential-free. Each resolution is logged at startup with the tier and
the id it became.

**A spec may declare `model:` or `tier:`, not both** — that is a load
error naming the file, not a precedence rule. The two say different
things, and a bundle that says both has not decided which it means.

The shipped `ns-audit` bundle is the worked example: four `tier: small`
namespace analysts under one `tier: mid` synthesis step.

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

Mast does not interpret the schema, with exactly one exception. There is no
built-in report type; the shape belongs to the workload.

### The one property mast reads: `proposed_change`

A report can carry the calls it recommends, not just prose describing them.
Declare a `proposed_change` array and mast will check it:

```json
"proposed_change": {
  "type": "array",
  "description": "The executable form of your recommendation …",
  "items": {
    "type": "object",
    "properties": {
      "tool":      {"type": "string"},
      "arguments": {"type": "string"}
    },
    "required": ["tool", "arguments"]
  }
}
```

Every entry has to name a tool the workload declares in its `tool_catalog`
and carry arguments that fit **that tool's own** input schema. Both are
checked at the moment the report is returned, before it becomes the
specialist's answer. A report naming an unknown tool, or arguments the tool
would reject, comes back to the specialist as an error it can correct —
the same refusal shape as a schema violation, not a failed run.

When the roster's change executors have enumerated their MCP allowlists,
the check narrows further: the entry has to name a tool one of them can
actually run. This is the difference between a call this daemon holds and a
call that will fire if an operator approves it, and the gap between the two
is where an approval ends with nothing happening. An executor that took an
un-enumerated grant has no finite surface to check against, so the narrower
check is skipped; an executor whose declared surface is only
`tools.builtin` has an *empty* one and every proposal is refused, which
mast warns about at startup — built-in tools are a declaration, not a grant
(see [specialists](/concepts/specialists-and-dispatch/)).

`arguments` is a JSON object **encoded in a string** —
`"{\"namespace\":\"prod\",\"replicas\":2}"`, or `"{}"` for a tool that takes
none. It has to be, because its keys are whichever tool the entry names and
no report schema can declare them in advance; an object property with no
declared properties is refused at load time (above) for exactly the reason
that it would accept anything. Mast parses the string and validates it
against the real tool's real schema, which is a stronger check than the
report schema could have made.

**An empty list is a complete report.** A specialist that cannot name an
exact call — the fix needs a human decision, or a tool the workload was not
given — returns `[]` and says why in its own escalation field. Nothing is
recorded and nothing is refused. A roster that does not declare the property
at all is unaffected.

What the property buys: under [`graph`
dispatch](/concepts/specialists-and-dispatch/#graph--a-classifier-routing-to-a-node)
a finding whose change set the operator approved is handed to the roster's
change executor verbatim, so the call that fires is the call that was on
screen. See [approvals](/concepts/approvals/#the-change-set--approving-the-call-not-the-prose).

## `precondition:` — what makes an approval stale

An operator answering `scope: change_set` authorizes calls that have not
fired yet. What should void that authorization is the cluster changing
underneath it — not a clock running out. But mast cannot work that out on
its own: a tool is opaque to it, so it does not know which tool reads the
object a write is about, or which of the write's arguments names that
object. The bundle knows both, so the bundle declares it:

```yaml
tool_catalog:
  tools:
    - name: get_deployment
      mutating: false            # required: the read must be read-only
    - name: scale_deployment
      mutating: true
      precondition:
        read: get_deployment
        args_from: {name: deployment}   # the read's "name" <- this call's "deployment"
        args: {namespace: prod}         # …plus anything fixed
        fields: [output.replicas]
```

The read runs twice: once when the operator answers, to snapshot the world
they answered about, and once immediately before each granted call fires. If
a declared field moved in between, the grant is voided and the call is
re-parked with a question that names the field and both values —
`output.replicas was 1 at approval and is 5 now`. Every field is reported,
not the first, because an operator re-reading the question needs the whole
delta.

Four things to know before writing one:

- **A precondition over the field the set itself rewrites invalidates its
  own set.** The snapshot is taken once, at approval. So a two-call set that
  scales the *same* object to 2 and then to 3 has call 1 move what call 2 was
  checked against, and call 2 goes back to the operator. The fix is
  `args_from`: check each call against **its own** object, and a set that
  touches two Deployments works while a set that touches one twice, by
  design, does not.
- **The read must be declared `mutating: false`.** An unlisted tool is
  mutating by default ([default-deny-unknown](/concepts/tools-and-mcp/)), so
  a catalog that simply forgot to list the read fails at startup with the
  declaration named — not at 3am with a "freshness check" that was writing
  to the cluster.
- **`fields:` paths start `output.` for MCP tools.** A structured MCP result
  arrives wrapped in an `output` property. A path that misses the wrapper
  matches nothing, every read digests identically, and the check silently
  compares nothing to nothing — which looks exactly like "the cluster never
  moves".
- **Omitting `fields:` digests the whole result.** That is right for a narrow
  read and wrong for a chatty one, where a timestamp or a `resourceVersion`
  in the payload makes every re-read look like drift. Narrow the read rather
  than filtering a wide one here.

A tool that declares no precondition is bounded by `hitl.change_set_ttl`
alone, and the parked question says so in those words. If mast cannot
evaluate a declared precondition at all, the set is not grantable: the
question says the calls must be approved one at a time, and `scope:
change_set` is refused rather than granted on a check that is not running.

## `scheduled:` — a workload that wakes itself

Every other way into a mast daemon needs something outside it: an inbound
POST, an operator, or a cron entry holding your schedule on mast's behalf.
`edge_trigger.scheduled` lets the bundle hold it:

```yaml
edge_trigger:
  scheduled:
    interval: 15m
    jitter: 45s
    prompt: Sweep every namespace for pods in CrashLoopBackOff and report what you find.
```

Four things to know before declaring one:

- **The cadence is anchored, not restarted.** Fires land on
  `anchor + k×interval`, and the anchor is written to the session store the
  first time the trigger comes up. A daemon that is redeployed twice a day
  resumes the phase it had rather than quietly moving a 02:00 sweep into the
  afternoon — the failure where a schedule keeps working and stops meaning
  what it said. (The anchor lives on a bookkeeping row, not a session; it
  never appears in `mast sessions list`.)
- **A tick the daemon was down for is skipped, not caught up.** Coming back
  from an outage that spanned three ticks fires none of them and logs one
  line naming how many were dropped and over what window. A periodic run
  samples the *current* state of the world, so a sample nobody took is owed
  to nobody — and catching up means a crash-looping daemon buys a fresh
  backlog of model runs at every restart, about the crash. If you need every
  occurrence to run, a schedule is the wrong shape: post them.
- **Every fire is a fresh session, and it runs as `mast:scheduler`.** The
  session is named for its tick and listed like any other, so a scheduled run
  is as readable after the fact as an injected one. The identity is
  namespaced, so no human login can produce it and no audit row can be
  misread as somebody's. A mutating call inside a scheduled run still parks
  for a real approver under `hitl.on_mutation` — unattended is not
  unsupervised — and the run is bounded by the same `budget:` block as
  everything else.
- **One daemon per session store.** As everywhere else in mast, there is no
  leader election: two replicas of a scheduled workload each keep their own
  cadence and both fire.

Fires are counted by `mast_scheduled_fires_total{workload,outcome}` —
`ran`, `skipped` (a tick that came due during a drain), `error`, and
`missed` (one per tick coalesced away after an outage). A run that fails is
not retried: the next tick is the retry, and it samples a fresher world.

## `monitor:` — the facts a cycle gathers for itself

A scheduled fire on its own wakes the model with a tick and a prompt, and
leaves it to work out what to look at. `monitor.collect` runs the looking
first, as mast rather than as the model:

```yaml
edge_trigger:
  scheduled:
    interval: 15m

monitor:
  collect:
    - tool: k8s_cluster_health
      as: health
    - tool: k8s_findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
  transitions_from: transitions
```

The calls run in declaration order, serially, before any model call. Their
results reach the roster inside the wake-up envelope:

```json
{
  "kind": "scheduled",
  "workload": "cluster-watch",
  "tick": "2026-08-21T10:00:00Z",
  "collected": {
    "health": { "…": "whatever the tool returned" }
  },
  "transitions": {
    "scanned": 412,
    "records": [
      {
        "subject_key": "prod/checkout-7d9f/OOMKilled",
        "transition": "escalated",
        "fields": { "severity": "critical", "prev_severity": "warning" }
      }
    ]
  }
}
```

Everything named in `collect` lands under `collected` as the tool returned
it. The one key named by `transitions_from` is the exception: it is parsed,
and it moves — it appears under `transitions` and **not** under `collected`,
so the model is never handed two spellings of the same fact.

### Why this is not just a tool the model holds

Because of what the interesting tools are. Knowing what *changed* since the
last run means asking something that keeps per-run state, and a tool that
advances persisted state as a side effect of answering is a mutating tool —
correctly so. Under the default `hitl.on_mutation: require_approval`, a
model holding that tool parks the cycle for an operator on **every fire**.
An unattended monitor that needs someone awake to authorize finding out
whether anything changed is not unattended. (And a tool nothing has
classified is mutating by default, so this is the common case, not a corner
of it.)

So the collection is mast's. Three things follow, all of them deliberate:

- **Nothing gates it**, because no model asked for anything. The write gate
  stays registered and stays in force for everything the model *does* call.
- **It costs zero model calls**, by construction rather than by tuning — a
  leg the model is not part of cannot spend a token. A monitoring cycle on a
  [bounded roster](#bounded-rosters) therefore costs exactly one model call,
  the same as an injected incident.
- **mast learns nothing about your domain.** It runs the calls you named and
  passes the results through unchanged. What a transition *means* is the
  collected tool's business.

### `transitions_from:` — the classification comes from the tool

A monitor's real question is not "what does the cluster look like" but "what
changed since last time", and answering that needs per-run state. The tool
that keeps it — [`k8s-lookout`](https://github.com/go-steer/k8s-lookout)'s
findings diff, or anything shaped like it — already does the classifying.
`transitions_from` points mast at that result so the transitions ride the
envelope as data rather than as a wall of text the model has to re-read:

```yaml
monitor:
  collect:
    - tool: k8s_findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
  transitions_from: transitions
```

**mast has no vocabulary of transition classes.** There is no enum, no
severity comparison, no fingerprinting, no "is this really new" second
opinion. A record's `transition` value is whatever string the classifier
wrote, carried to the model verbatim — if your tool emits `quiesced`, the
model sees `quiesced`. `transitions_from` names *where* the verdict comes
from; it never constrains what the verdict says. Two implementations of
"what changed" is one more than can be right.

#### The record stream

The named tool answers in text: one record per line, then a summary line.
Each record is logfmt or flat JSON (detected per line, so a stream may mix
them), and needs two keys — `subject_key` and `transition`. Everything else
becomes `fields`:

```
subject_key=prod/checkout-7d9f/OOMKilled transition=escalated severity=critical prev_severity=warning
{"subject_key":"prod/api-5c1/CrashLoopBackOff","transition":"new","severity":"warning"}
scanned=412 findings=2 elapsed=1.4s
```

The trailing `scanned=<n> findings=<n> elapsed=<d>` line is **mandatory**,
and `findings=` must equal the records above it. That is the only judgement
mast makes about the stream, and it is about completeness, not correctness:
a truncated answer is a *prefix* of a healthy one, and the notify half
deliberately says nothing when nothing changed. Without the summary line the
two are indistinguishable, so a stream that lacks it — or whose count
disagrees — is void, and voids the cycle. A record with a `transition` but
no `subject_key` is malformed on the same footing: nothing downstream can
acknowledge or de-duplicate a subject it cannot name.

#### A quiet cycle says so

`"records": []` means "we classified, and nothing changed". A workload with
no `transitions_from` ships no `transitions` key at all, which means "we do
not know". Those are different facts and the notify half acts on only one of
them, so the empty list is never elided.

A malformed stream fails the cycle exactly the way [a failed
collection](#when-collection-fails) does — before any model call, counted
`error`, cadence continuing — and the log line names both the
`transitions_from` key and what was wrong with the bytes.

### `notify:` — speaking only when something changed

`transitions_from` tells mast whether anything changed. `notify` is what it
does with that answer:

```yaml
monitor:
  collect:
    - tool: k8s_findings_diff
      args: {transitions: "new,escalated,resolved"}
      as: transitions
  transitions_from: transitions
  notify:
    conversation: "#sre-oncall"
    digest_after: 12h
```

The daemon supplies the other half — which ingress, and with what
credential:

```bash
MAST_NOTIFY_TOKEN=… mast --workload=cluster-watch \
  --notify-url=http://switchboard:8080 --listen=:7777 --session-db=…
```

That split is deliberate. *Where to speak* is a property of the workload;
*which switchboard, and as whom* is a property of the deployment. A bundle
that named its own ingress could not be promoted from staging to production
without editing the workload. The token is refused at startup if it equals
any of the daemon's own inbound tokens (`MAST_INJECT_TOKEN`,
`MAST_ATTACH_TOKEN`, `MAST_A2A_TOKEN`, `MAST_AGUI_TOKEN`): those point the
other way, and sharing one means anything that can read the chat bridge's
configuration can drive this daemon. It is an env var and never a flag, so
it stays out of `ps` and out of the container spec's args.

A bundle with a `notify:` block and a daemon with no `--notify-url` is a
**startup refusal**, not a warning. The workload's entire output is the
message it was going to send.

#### A quiet cycle does not wake the model

Not "wakes it and then declines to post" — the turn never runs. A
fifteen-minute cadence that spends a model call on every quiet cycle costs
more per month in nothing-happened than the incidents it exists to catch.

The skip is narrow: it applies only when the workload declared **both**
`transitions_from` (so "nothing changed" is the classifier's answer, not
mast's guess) and `notify` (so speaking is what the cycle is for). A
workload that names no classification source speaks on every cycle, and the
daemon says so at startup with `speaks_every_cycle=true`.

#### One incident is one message

The first speaking cycle posts; the next ones **append** to that message, so
an incident that takes six cycles to resolve reads as one growing story
instead of six notifications an operator has to reassemble. The first quiet
cycle **closes** the timeline — what happens after calm is a new incident,
and it gets a new message.

Switchboard can answer an append two ways that are not errors, and both are
handled rather than surfaced:

- **409, "send the full text"** — a restart, another replica, or a message
  posted from elsewhere means it no longer remembers the body. mast holds
  what it last sent and re-sends the whole story as an edit.
- **200 with a continuation ref** — the message is full, so the platform
  posted a threaded continuation. Every later cycle targets that one.

If neither works (the platform cannot edit, or the message is gone), the
whole story is posted fresh, so the thread that survives is the readable one.

#### A failed send is not replayed

There is no queue, no retry and no spool. A send that failed is an errored
fire, counted `mast_monitor_notifications_total{outcome="error"}`, and the
next cycle reports what is new **then**.

Holding the message and re-sending it next cycle is the tempting fix and it
is wrong: the classifier advanced its own state when it answered, so the
replay would describe a world that has already moved on. Advancing state
before speaking is the ordering that makes this safe.

#### A broken monitor says so

If a cycle cannot complete — collection failed, the classification was
truncated, the turn errored — mast posts a notice of its own into the same
conversation, and another when the cycle next succeeds. Both are
edge-triggered: **once** on the way down and once on the way back, never on
every failing cycle, because a channel that repeats itself every fifteen
minutes is a channel an operator mutes, and the mute costs them the incident
report too. A health notice that cannot be delivered is logged and never
fails the fire.

#### Silence has a deadline

With `digest_after`, a workload that has said nothing for that long speaks
on its next cycle whether or not it has news, and the deadman resets. The
clock starts at daemon startup, so a daemon booting into a quiet world does
not immediately announce the quiet. Those cycles are counted separately on
`mast_monitor_digest_wakes_total`, because "did anything change" and "did
the deadman have to fire" are different operator questions.

Every cycle's outcome is on
`mast_monitor_notifications_total{workload,outcome}` — `posted`, `appended`,
`replaced`, `rolled`, `quiet`, `health`, `error`. A healthy monitor is
mostly `quiet`.

### `ack:` — taking an acknowledgement back

Everything above runs outward. `ack` is the one leg that runs the other way:
an operator reads a finding in the chat, says "I have seen this one", and the
monitoring stops reporting it.

```yaml
monitor:
  ack:
    tool: k8s_findings_ack
    args: {window: 4h}
```

That arms `POST /monitor-ack` on the daemon. A workload with no `ack:` block
answers `404` there — nowhere to forward one.

```bash
curl -s -X POST http://localhost:7777/monitor-ack \
  -H 'Authorization: Bearer '"$MAST_INJECT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"subject":"ns/checkout/oom-restarts","reason":"known, fix rolling out"}'
```

```json
{
  "workload": "cluster-watch",
  "subject": "ns/checkout/oom-restarts",
  "ack_by": "alice@example.com",
  "previously_acked_by": "bob@example.com",
  "previously_acked_at": "2026-08-21T07:02:11Z"
}
```

The `subject` is the producer's subject key, verbatim from the classification
mast reported — mast does not parse it and has no vocabulary for what a
subject is.

#### Who owns what

The suppression lives with the producer of the transitions. It is the only
place the *next* cycle's classification can read it from, so mast forwarding
and then keeping its own copy of "how long this is muted" would be a second,
wrong answer to a question already answered. `monitor.ack.args` is where the
deployment's policy goes — which cluster, how long — and there is no
per-request window.

What the producer cannot do is say **who asked**. Its ack surface takes an
identity string from whoever calls it and has no way to check it. mast can,
because the ack came through an authenticated ingress, so the split is:

| | Store of record for |
|---|---|
| The producer | the suppression — whether a subject is muted, and until when |
| mast | the attribution — who acked, when, from what credential, with what note |

mast writes its record **before** forwarding. A forward that then fails
leaves an attributed ack the suppression never reached, which is visibly
different from nobody having acked at all; the operator is told, and acking
again is the recovery.

#### `ack_by` comes from the credential, never the body

The identity mast forwards and records is the one that authenticated the
request. A body carrying `ack_by` is **refused with a `400`** naming the
field — not ignored, because silently dropping a field a client believed in
produces an audit line naming the wrong person with no sign anything went
wrong.

A relay acking on a human's behalf names them the supported way, with the
`X-Asserted-Caller` header, which works only for a credential the users file
provisioned to assert. Both are kept: the human is the acker, the relay is
recorded alongside. This is the rule `/resume` settled for approvals, and it
matters at least as much here — see [Who a resume
names](/reference/cli/#who-a-resume-names).

#### An ack is not an approval

They share an operator, a chat window and a verb, and nothing else. An
approval mints a grant that licenses a write to the world and is consumed on
use. An ack asserts no diagnosis, authorizes no change, and carries no
verdict, no grant, no freshness window and no change-set signature. It does
not go through the write gate and **does not appear in the decision export** —
otherwise "who approved this change" would quietly start meaning "who muted
an alert".

The count is on `mast_monitor_acks_total{workload,outcome}`: `forwarded` or
`error`, and nothing between them.

### The fence

The exception is narrow because it is enforced, not because it is
documented. A tool mast runs on its own behalf — everything named in
`monitor.collect`, plus the one named in `monitor.ack` — must be reachable by
nothing else, and the daemon refuses to start otherwise, before it wires MCP,
so an operator without credentials reads the roster problem rather than a
`403` standing in for it. Three ways a roster reaches a tool, all three
refused:

1. a specialist names it outright, on an MCP server or among its built-ins;
2. a specialist allows an MCP server with no `tools:` list, which grants it
   every tool on that server (refused whether or not a collect tool is on
   that server today — mast cannot tell without connecting to it);
3. a specialist declares no `tools.mcp` key at all, which inherits the whole
   catalog. Write `mcp: []` if it needs no tools.

`SingleTurn` specialists are exempt: they are built with no toolsets at all,
so they reach nothing. That is what makes a bounded monitoring roster the
clearest form of the shape — its model holds no tools whatsoever and still
gets the transitions.

The fence matters most for the ack tool. A collect tool a model could also
reach would let it gather facts ungated; an ack tool it could reach would let
it **suppress its own alerts**, and the producer's triage-status write is
typically not permission-gated on its side, so the daemon's ingress
authentication is the only thing in front of it. "There is no ack tool the
model can call" is a property of the build — an agent that cannot see a tool
cannot be talked into calling it.

### When collection fails

The cycle fails, before any model call. Not "collect what you can and wake
the model with the rest": a cycle whose diff failed and whose scan succeeded
would hand the model a snapshot with no transitions attached, and the honest
reading of "no transitions" is "nothing changed". A monitor that reports calm
because its collection broke is worse than one that reports nothing, because
only the second is visibly broken. The fire is counted `error` on
`mast_scheduled_fires_total`, the reason is logged against the tick, and the
cadence continues to the next one.

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
whichever is reached first stops what it holds — the specialist, or the
whole run. When one call crosses both, the
error names the specialist — the more specific fact, and the one an
operator acts on. A specialist that declares a `model:` override or a
`tier:` is priced at the rate of the model it actually runs on — a tier is
priced off what it resolved to — so a cheap analyst's tokens are not billed
at the synthesizer's.

One limit is worth knowing. A ceiling is checked both before a call and
after it, but the pre-call check refuses only what it can prove — so
`max_turns` is exact, while a call that starts with dollars or tokens to
spare always finishes and is billed for what it turns out to cost. The cap
bounds total spend within one call's overshoot; it does not pre-authorize a
call.

A specialist's own ceiling stops that specialist and not the session: the
coordinator is handed the refusal and routes around it, so a workload with
budget elsewhere finishes with a path missing rather than failing. The loss
is reported on `mast_budget_trips_total`, in the daemon log, per specialist
under `cost_ceiling.scopes[]`, and in `mast.Result.Exhausted`
([budgets](/concepts/budgets/#a-spent-specialist-closes-one-path-not-the-session)).

### Env-var overrides

Scalar budget values can be overridden process-wide (applies to every
bundle loaded in the invocation, overrides file values unconditionally;
set-but-unparseable is a fatal load error):

```
budget.max_cost_usd          → MAST_BUDGET_MAX_COST_USD
budget.max_wallclock_seconds → MAST_BUDGET_MAX_WALLCLOCK_SECONDS
```
