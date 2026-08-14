---
title: The write gate
description: How a mutating tool call parks for approval, the verdict an operator sends back, and what mast records about what actually ran.
sidebar:
  order: 6
---

A call that would change something stops and asks before it fires. This
page is the operator's side of that: what parks, what you send back, and
what mast writes down about it.

The policy that decides whether a call parks at all is
[`hitl.on_mutation`](/reference/workload-bundle/) in the workload bundle.
Its default is `require_approval`, so a bundle that says nothing about
mutation is gated — unattended writes have to be asked for.

## What a parked call looks like

`mast sessions show <session>` prints the pending question without a
running daemon:

```sh
mast sessions show incident-demo-1 --session-db=/tmp/mast-demo/sessions.db
```

```
State:     paused
Interrupt: adk-8f3c...-a41b
Message:   Approve mutating call scale_deployment(deployment=api, replicas=10)?
```

The question is a **durable event**, not an in-memory prompt. The daemon
can be killed between asking it and hearing the answer; the pause is
still there when the next process starts, with the same interrupt id, and
the call runs exactly once when it is finally approved.

## The verdict

Answer it over `POST /resume`, the same endpoint every other interrupt
uses:

```sh
curl -s -X POST http://localhost:7777/resume \
  -H 'Authorization: Bearer $MAST_INJECT_TOKEN' \
  -H 'Content-Type: application/json' -d '{
  "session_id":"incident-demo-1",
  "interrupt_id":"adk-8f3c...-a41b",
  "response":{"verdict":"approve","scope":"once","note":"oncall approved"}}'
```

| Field | Values | Meaning |
|---|---|---|
| `verdict` | `approve`, `reject`, `edit` | Required. `approve` runs the call as proposed; `reject` refuses it; `edit` runs it with the arguments in `args` instead. |
| `scope` | `once` | Optional, and `once` is the only scope a mutating call accepts. Anything wider is **refused**, not narrowed. |
| `args` | object | `edit` only: the arguments to run instead of the model's. |
| `note` | string | Optional; shown to the agent, and kept in the audit record. |

You do not send `approver`. Mast resolves it from the authenticated
caller and overwrites whatever the payload said — "who authorized this
write" is the question the gate exists to answer, and a self-asserted
answer is not one.

### Scopes wider than one call are refused

`{"verdict":"approve","scope":"session"}` does not approve the call and
does not approve the session. It is refused, audited as
`approval_scope_refused`, and the operator is told to re-issue for the
single call. Silently narrowing it would leave someone believing they had
a standing grant they did not have.

### Rejection stops the agent

A refused call comes back to the model as a refusal that says so: do not
retry, do not reach the same change by another route, report it. The
agent finishes its read-only work and stops rather than looking for a
way around.

## Editing a call before it runs

```sh
curl -s -X POST http://localhost:7777/resume \
  -H 'Authorization: Bearer $MAST_INJECT_TOKEN' \
  -H 'Content-Type: application/json' -d '{
  "session_id":"incident-demo-1",
  "interrupt_id":"adk-8f3c...-a41b",
  "response":{"verdict":"edit",
              "args":{"deployment":"api","replicas":2},
              "note":"ten would exhaust the node pool"}}'
```

Those are the arguments the tool receives. Four things are checked
first, and each of them refuses the call outright rather than running a
narrowed version of it:

- **An edit must be attributable.** An edited verdict with no
  authenticated approver is refused (`edit_unattributed`). An edit runs
  arguments no model proposed and no policy pattern vetted, so the
  record of who wrote them is the only trace of where they came from.
- **Every argument must be one the tool declares.** Mast checks this
  itself rather than leaving it to the schema, because an MCP server's
  input schema usually permits unknown properties.
- **The values must satisfy the tool's declared input schema.** A
  violation is refused (`edit_refused`) and neither the edited call nor
  the original runs.
- **The edited call is re-checked against policy.** The permission
  policy was matched against the *model's* call before the park; an edit
  produces a different call, so it is adjudicated again under its own
  key. A `deny scale_deployment(deployment=prod-*)` cannot be defeated by
  editing an approved staging call into a production one.

A tool that declares no input schema cannot be edited at all — mast
would have nothing to check the operator's arguments against.

## What mast records

An edit creates an audit problem worth knowing about. The agent
substrate resumes an approved call by re-firing the **original** call, so
the durable transcript still holds the arguments the model proposed while
the operator's are the ones that execute. Reading the transcript alone
would give you a confident, wrong answer about what mast did.

So the gate writes down what actually ran, and `sessions show` prints it:

```
Operator edit applied:
  Proposed: scale_deployment(deployment=api, replicas=10)
  Executed: scale_deployment(deployment=api, replicas=2)
  Approver: oncall@example.com
  Note:     ten would exhaust the node pool
```

The record is part of the session's own durable state, written with the
same event as the call it describes — so it survives a restart on the
same terms the pause does, and it is there for an aborted session too.

Every outcome is on the daemon's audit log as well, each with a named
outcome: `awaiting_approval`, `denied_by_policy`, `denied_by_operator`,
`approval_scope_refused`, `edit_unattributed`, `edit_refused`,
`edit_applied`, `apply`, `dry_run`.

## The gate is not the only boundary

Everything above is what mast enforces. What the *cluster* will accept from
mast is a separate grant, and on Kubernetes it should be a narrower one: an
approved call still has to get past the API server. See [cluster
permissions](/reference/cluster-permissions/) for the read/write RBAC split
that ships with the deployment manifests — and for the GKE IAM binding that
decides whether that split bounds anything.
