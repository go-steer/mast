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

- **Scope is `once`, and only `once`.** A verdict approves the single call
  in front of the operator. A request for anything wider is refused and
  audited as `approval_scope_refused` — "approve all scale operations for
  this session" is exactly the blanket consent the gate exists to prevent.
- **The approver is the authenticated caller, not the payload.** Whatever
  name arrives in the request body is overwritten by the identity that
  actually authenticated. An audit trail an agent could write into is not
  an audit trail.
- **An edit is recorded durably.** The substrate re-fires the *original*
  call on resume, so the edited arguments are persisted as their own record
  and re-applied — the thing that ran and the thing the operator authorized
  are the same thing, and the log shows both.

Every outcome lands in the audit log under a fixed vocabulary:
`awaiting_approval`, `denied_by_policy`, `denied_by_operator`,
`approval_scope_refused`, `edit_unattributed`, `edit_refused`,
`edit_applied`, `apply`, `dry_run`.

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

## Today: the executor is operator-invoked

An honest limitation, because you will hit it in the first ten minutes.

The diagnosers find the problem, and the change executor can fix it — but a
diagnosis does not currently hand itself to the executor. Under `graph`
dispatch, specialist nodes are terminal: the graph routes, the specialist
reports, the graph ends. An operator reads the finding and injects a
follow-up naming the change executor, which then parks for approval like
any other mutating call.

So the loop is *diagnose → operator → remediate-with-approval*, not
diagnose → remediate. Closing it needs the diagnosers to emit a typed,
machine-readable proposed change rather than prose an executor has to
re-derive; that work is scoped and blocking, and lands in v0.4. See the
[roadmap](/roadmap/).

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
