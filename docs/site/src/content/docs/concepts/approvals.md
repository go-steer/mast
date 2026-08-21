---
title: Approvals and the write gate
description: The four boundaries between an autonomous agent and a change to your cluster — capability split, mutation classification, the per-call gate, and RBAC — and what each one does not cover.
sidebar:
  order: 3
---

The question an SRE team asks about an autonomous agent is not "how good is
its diagnosis". It is **"what can it do to my cluster, and who said it
could?"**

mast answers that with four boundaries, deliberately layered so that no
single one has to be perfect. Three are enforced by the runtime; the fourth
is your cluster's, and mast is built to compose with it rather than replace
it.

## 1. Capability — declared per specialist, checked at startup

Every specialist declares what class of thing it is:

```yaml
capability: read_only        # the default
capability: change_executor  # may reach mutating tools
```

At construction — before the daemon serves a single request — mast walks
the roster and **refuses to start** if a `read_only` specialist can reach a
mutating tool. Three shapes are refused, each with the offending file and
tool named in the error:

- it names a mutating tool in its allowlist,
- it grants a whole MCP server (under
  [default-deny-unknown](/concepts/tools-and-mcp/), that is a grant of
  every mutating tool on it),
- it declares no `tools.mcp` at all while a tool catalog exists — an
  unrestricted specialist is not a read-only one.

`SingleTurn` specialists are exempt: the mode carries no tools, so there is
nothing to reach.

The point of this layer is that the read/write split is **structural, not
prompt-held**. A prompt saying "you are a read-only diagnoser" is a
suggestion to a language model. A construction-time refusal is not. Startup
also logs every `change_executor` in the roster, so the answer to "what can
change my cluster" is a log line, not an intersection of three files.

## 2. Mutation classification — declared, never guessed

The gate can only stop calls it knows are mutating, so *how* a tool gets
classified is load-bearing.

MCP publishes a `readOnlyHint` annotation, which sounds like the answer —
but the substrate's MCP toolset drops annotations at conversion, so the hint
never reaches mast. Rather than guess from the verb in a tool's name, mast
takes the honest path: **unclassified means mutating**. A tool is treated as
read-only only when the workload's `tool_catalog` says so explicitly, and
every such un-gating is audit-logged at startup.

The failure mode this chooses is the safe one — a misconfigured read-only
tool parks for an approval that was not strictly needed, rather than a
misconfigured mutating tool firing unseen. Details, and the reason a
name-based heuristic was rejected: [tools and MCP](/concepts/tools-and-mcp/).

## 3. The write gate — one call, one operator, one answer

With the classification in hand, the gate is simple to state: **a call that
would change something stops before it fires.**

```yaml
hitl:
  on_mutation: require_approval   # default. also: apply | dry_run
```

The session parks with a durable interrupt carrying the specialist, the
tool, and the exact arguments. The agent is not spinning; the process can
die and the pause survives it ([durability](/concepts/durability/)). An
operator answers with one of three verdicts:

| Verdict | Effect |
|---|---|
| `approve` | the call fires, exactly as shown |
| `reject` | the call does not fire, and the agent stops rather than looking for another way |
| `edit` | the operator supplies corrected arguments, and *those* run |

Three properties are worth calling out, because each closes a hole that
looks small on paper:

- **Scope is `once`, unless the operator is looking at the whole set.** A
  verdict approves the single call in front of them. The one exception is
  `change_set`, below: it authorizes the *specific other calls quoted in the
  same question*, and nothing else. Anything wider — "approve all scale
  operations for this session" — is refused and audited as
  `approval_scope_refused`, because that is the blanket consent the gate
  exists to prevent.
- **The approver is the authenticated caller, not the payload.** Whatever
  name arrives in the request body is overwritten by the identity that
  actually authenticated. An audit trail an agent could write into is not
  an audit trail. The same rule governs who a resumed pause names — see
  [who a resume names](/reference/cli/#who-a-resume-names).
- **An edit is recorded durably.** The substrate re-fires the *original*
  call on resume, so the edited arguments are persisted as their own record
  and re-applied — the thing that ran and the thing the operator authorized
  are the same thing, and the log shows both.

Every outcome lands in the audit log under a fixed vocabulary:
`awaiting_approval`, `denied_by_policy`, `denied_by_operator`,
`approval_scope_refused`, `edit_unattributed`, `edit_refused`,
`edit_applied`, `apply`, `dry_run`, and the change-set outcomes
`change_set_approved`, `approved_by_change_set`, `change_set_scope_refused`,
`change_set_refused`.

The other two modes exist for real situations, not as escape hatches:
`dry_run` runs the whole loop and reports what *would* have happened, which
is how you evaluate a roster before trusting it; `apply` is for
already-bounded automation where the RBAC layer below is the real
constraint. Both are opt-in, in the bundle, reviewable in a diff.

Field-level detail, request and response shapes:
[write gate reference](/reference/write-gate/).

## 4. RBAC — the boundary mast does not own

The first three layers are mast's, which means they are only as good as
mast's own correctness. The fourth is not: the shipped Kubernetes base
mirrors the capability split into two service accounts, so a compromised or
buggy read-only path has no credential to change anything with, whatever it
believes about itself.

That is the layer to lean on hardest, because it holds when the others are
wrong. See [cluster permissions](/reference/cluster-permissions/) for the
manifests and the current caveat on narrowing the GKE IAM binding.

## The change set — approving the call, not the prose

The three layers above stop a call. This one decides *which call* the
operator is being shown.

A finding that says "scale the api Deployment back to 2 replicas" is prose,
and prose cannot be approved, only agreed with. If the change executor
composes the actual tool call afterwards, from that sentence, on its own
turn, then the thing that reached your cluster is not the thing anybody read.

So a report can carry the call. A workload's report schema declares
`proposed_change`, and a finding fills it with entries naming a tool from
the workload's own catalog:

```json
"proposed_change": [
  {"tool": "apply_change", "arguments": "{\"namespace\":\"prod\",\"replicas\":2}"}
]
```

Two checks run the moment the report is returned, before it becomes the
specialist's answer: the tool has to be one this workload declares, and the
arguments have to fit **that tool's own** input schema. A report that fails
either comes back to the specialist as an error it can correct, rather than
travelling on to an operator who has no way to tell. It is the same code
that validates an operator's `edit` at the gate, so the two cannot drift
apart on what "valid arguments" means.

**An empty list is a right answer.** "Raise the memory limit, but I don't
know to what" is an honest diagnosis; a specialist that cannot name an exact
call returns nothing and escalates, instead of inventing a plausible number
to fill a field.

Under `graph` dispatch this closes the loop the first three layers left
open. A finding with a non-empty change set that the operator approved is
routed to the roster's change executor, which is handed those calls
verbatim — the edge is a structural rule about the finding, not an
instruction in a prompt. Everything else about the gate is unchanged: each
call still parks, still names its arguments, still needs one authenticated
answer. A roster that declares no `proposed_change`, or a finding that
proposes nothing, behaves exactly as it did before; and a roster carrying
two change executors logs an error and leaves the edge unwired rather than
guessing which was meant.

Field reference: [the report
contract](/reference/workload-bundle/#the-one-property-mast-reads-proposed_change).

## One answer for a set of calls — and what makes it go stale

A remediation is often several calls: scale two Deployments, or patch a
ConfigMap and restart what reads it. Parking each one separately means the
operator who has already read the plan answers the same question three
times, and the third answer is not a decision, it is a reflex.

So when a parked call belongs to a change set, the question says so and
quotes the rest of the set. Answering with `scope: change_set` mints a
**grant for each remaining call**:

```
Message:   Approve mutating call scale_deployment(deployment=api, replicas=2)?
           It is one of 2 calls in the change set ScaleUp proposed;
           answer with scope=change_set to authorize all 2.
  Change set: 2 call(s) proposed by ScaleUp
    1. scale_deployment(deployment=api, replicas=2)
    2. scale_deployment(deployment=worker, replicas=2)
    freshness of scale_deployment: re-checked against get_deployment before the call fires
    Approve all with: --response='{"verdict":"approve","scope":"change_set"}' (valid for 600s)
```

A grant is deliberately narrow:

- it authorizes **one exact `(tool, arguments)` pair** — not the tool, not
  the verb, not "scaling" — so a call the model composes afterwards, however
  similar, still parks;
- it is **single-use** and **durable**, so a restart between the answer and
  the call changes nothing, and a second identical call needs its own
  approval;
- the **deny policy still runs first**, and spending a grant is still
  recorded as an allow-once decision in the audit log. A grant removes the
  question, never the accounting;
- if any call in the set cannot be granted — an argument too large to render
  in the question, a precondition mast cannot evaluate — the whole verdict is
  **refused with a reason** rather than covering the part that fits. A
  partial "approve all of this" is the one outcome nobody chose.

**What makes an approval stale is the cluster, not the clock.** A set
approved twenty seconds ago against a Deployment somebody has since scaled
by hand is stale. One approved an hour ago against an untouched object is
not. mast cannot work out which read tells it that — it does not know which
tool reads the object a write is about, or which of the write's arguments
names it — so the bundle declares it, per tool, and mast runs it twice: once
when the operator answers, once just before each granted call fires. If a
watched field moved in between, the grant is voided and the call goes back
to the operator with a question naming the field and both values.

Both of those reads run the tool itself, underneath the [digest
wrap](/reference/mcp-servers/#digesting-large-tool-responses). Digesting
exists to shrink what a *model* reads; this read is mast's own, and a
digest of it would drop the very field the comparison is about.

`hitl.change_set_ttl` (default 10 minutes) is the backstop for everything a
precondition cannot see. A tool that declares no precondition gets the TTL
and nothing else — and its question **says so**, in those words, rather than
implying a check mast is not making.

How to declare one, and the mistake to avoid on your first try (a
precondition watching the field the set itself rewrites invalidates its own
set): [`precondition:` in the workload
bundle](/reference/workload-bundle/#precondition--what-makes-an-approval-stale).

## What the operator decided, after the incident is over

A verdict is expensive. Somebody stopped what they were doing, read a call,
and made a judgement about a production system. Spending that once and
keeping only a log line wastes the most informative thing a gated fleet
produces: a case where the model's proposal and a human's answer are known
to differ.

So every adjudication is a durable record beside the call it decided, and
`mast sessions export-decisions` writes them out as JSON Lines — one row per
decision, with what the model proposed, what actually executed, which of
approve / reject / edit the operator chose, and whether the call ended up
authorized, refused by the operator, or refused by mast. Rejections are in
there for the same reason edits are: an export of nothing but approvals is a
dataset that never shows the model being wrong.

Two things to know before pointing it at a production store. **Approver
identities are digested by default** — a stable hash, so you can still group
by approver or count how many people approved a class of change, without
naming anyone; `--include-approver` opts out and the file records which mode
produced it. **Tool arguments are exported verbatim**, because the
proposed→executed diff is the whole label — which makes an export as
sensitive as the arguments your tools take.

Mast captures and exports; it does not score, retrain, or read the file
back. What it is for is the evaluation you run yourself. Field detail and
the full record shape: [exporting
decisions](/reference/write-gate/#exporting-what-was-decided).

## Composing the four

| Layer | Stops | Does not stop |
|---|---|---|
| Capability split | a read-only specialist ever reaching a mutating tool | a `change_executor` doing something wrong |
| Mutation predicate | an unclassified tool sliding through as safe | a tool wrongly declared `mutating: false` |
| Write gate | any classified mutation firing unseen | an operator approving a bad call |
| RBAC | any credential-less change, whatever mast believes | a change inside the granted scope |

Nothing here removes the operator from the loop. That is the design: mast's
job is to make sure the operator is *asked*, with the actual call in front
of them, and that the record of what they said survives the process that
asked.
