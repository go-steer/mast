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

:::caution[The planner cannot gate a write, so it refuses to try]
The gate is a runner plugin, and under `planner` dispatch
`invoke_specialist` runs its specialist on a runner of its own — built
without it. A mutating call made from inside a planner dispatch would
neither park nor dry-run, and would leave no effect-outbox record.

So **that combination does not start.** A bundle that enables the planner,
holds a `capability: change_executor` specialist, and asks for anything
other than `on_mutation: apply` is refused at composition, naming the
specialist and the way out:

```
compose: workload "triage" enables the planner and declares
hitl.on_mutation: require_approval, but its roster holds change
executor(s) change-executor: …
```

Run that roster under `coordinator` or `graph`, where the runner carries
the gate and everything on this page applies normally. Or set
`hitl.on_mutation: apply` if the writes are genuinely meant to fire
unattended — there is no gate to bypass under `apply`, though the missing
outbox record still costs you exactly-once replay if a dispatch is
interrupted.

**This refusal is permanent, and it is worth knowing why.** A park is not a
suspended turn: it writes its question into the session event log, and a
resume matches that log and re-enters at the root agent. A planner
dispatch runs on a private, in-memory session that dies with the tool
call, and nothing re-enters a dispatch mid-flight — so there is nowhere
for an approval to come back to. `coordinator` and `graph` dispatch get
the gate at full per-call fidelity precisely *because* they share the
session log.

Asking you to approve `invoke_specialist` instead would not be the same
promise. That call's arguments are a specialist name and a sentence of
prose, so you would be approving an intention, with no typed arguments to
review, nothing to edit, and an audit record naming the dispatch rather
than the change. Approving a change and approving an agent are different
things, and mast would rather refuse than blur them.

What remains genuinely owed is the missing outbox record under `apply`
— [#235](https://github.com/go-steer/mast/issues/235).
:::

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
| `scope` | `once`, `change_set` | Optional; `once` is the default. `change_set` is accepted **only** when this question carries a change set, and authorizes the other calls quoted in it — see [below](#approving-a-whole-change-set). Anything wider is **refused**, not narrowed. |
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

## Approving a whole change set

When the parked call is one of several the specialist proposed together,
`sessions show` prints the rest of them and how to authorize them in one
answer:

```
State:     paused
Interrupt: adk-8f3c...-a41b
Message:   Approve mutating call scale_deployment(deployment=api, replicas=2)?
           It is one of 2 calls in the change set ScaleUp proposed;
           answer with scope=change_set to authorize all 2.
  Change set: 2 call(s) proposed by ScaleUp
    1. scale_deployment(deployment=api, replicas=2)
    2. scale_deployment(deployment=worker, replicas=2)
    freshness of scale_deployment: re-checked against get_deployment before the call fires
    Approve all with: --response='{"verdict":"approve","scope":"change_set"}' (valid for 600s)
```

That answer mints one **grant** per remaining call. A grant:

- authorizes one exact `(tool, arguments)` pair — a similar call the model
  composes later still parks;
- is **single-use** (a second identical call needs its own approval) and
  **durable** (a restart between the answer and the call changes nothing);
- is still adjudicated by the deny policy, and still recorded as an
  allow-once decision when it is spent, audited `approved_by_change_set`;
- is re-checked for freshness immediately before the call fires.

Some verdicts cannot mint anything, and each **refuses the whole verdict**
rather than executing the call in front of you and silently dropping the
rest (audited `change_set_scope_refused`, with the reason in the refusal
the agent is told to report):

- `scope: change_set` on a question that carries no change set;
- an `edit` verdict — an edit speaks only for the call it edits, and the
  rest of the set is still what the specialist proposed;
- a set with an argument too large to render in the question — an operator
  cannot approve what they cannot read;
- a set whose declared freshness check mast cannot evaluate.

### What voids a grant

Two bounds, and the question you get back says which one fired.

**The cluster moved.** If the tool declares a
[`precondition:`](/reference/workload-bundle/#precondition--what-makes-an-approval-stale),
mast re-runs that read just before the granted call fires and compares it to
the snapshot taken when the operator answered. Any declared field that moved
voids the grant and re-parks the call:

```
Message:   Approve mutating call scale_deployment(deployment=worker, replicas=2)?
           (you approved this as part of a change set, and mast is asking again:
           the cluster moved since this was approved: output.replicas was 1 at
           approval and is 5 now (precondition read get_deployment(deployment=worker)))
```

A voided grant stays voided. A precondition that fails now and happens to
match again ten seconds later — a Deployment scaled away and back — must not
silently re-authorize a call the operator has already been asked about again.

**The window ran out.** `hitl.change_set_ttl` (default 10 minutes) bounds
every grant, precondition or not, and its expiry re-parks the call the same
way:

```
Message:   Approve mutating call scale_deployment(deployment=worker, replicas=2)?
           (you approved this as part of a change set, and mast is asking again:
           the change set was approved at 14:02:11Z and that approval expired
           at 14:12:11Z)
```

A tool with no precondition is bounded by the clock alone, and the question
it parks with says exactly that rather than implying a check mast is not
making.

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
`edit_applied`, `apply`, `dry_run` — plus, for change sets,
`change_set_approved` (the answer that minted the grants),
`approved_by_change_set` (a granted call firing),
`change_set_scope_refused` (the verdict mast would not honor as a set) and
`change_set_refused` (a proposed change the specialist's own report failed
validation on).

## Exporting what was decided

An adjudication is worth more than a log line. *"A human looked at
`scale_deployment(deployment=api, replicas=10)` and said two"* is a labelled
example, and it is the highest-signal data a gated fleet produces. So the
gate also writes a **decision record** for every adjudicated call — approve,
reject, edit, a change-set grant being spent, and the calls mast refused on
its own — and one subcommand harvests them:

```
mast sessions export-decisions --session-db=/var/lib/mast/sessions.db \
    --workload=gke-triage \
    --since=2026-08-01T00:00:00Z --until=2026-09-01T00:00:00Z \
    --out=decisions.jsonl
```

Naming a `<session-id>` exports one session; omitting it exports every
session in the store. Without `--out` the rows go to stdout, which is the
form to pipe. The output is JSON Lines — a `_meta` provenance object, then
one object per decision:

```json
{"_meta":{"tool":"mast sessions export-decisions","version":"0.4.0","schema":"mast.decision/v1","exported_at":"2026-08-16T11:04:02Z","redaction":"approver_digest","source":"/var/lib/mast/sessions.db","workload":"gke-triage","records":2,"warning":"Tool arguments are exported verbatim, …"}}
{"decided_at":"2026-08-16T09:00:00Z","session":"incident-123","workload":"gke-triage","specialist":"change-executor","tool":"scale_deployment","outcome":"edit","scope":"once","authority":"operator_verdict","disposition":"authorized","proposed_key":"scale_deployment(deployment=api, replicas=10)","proposed_args":{"deployment":"api","replicas":10},"executed_key":"scale_deployment(deployment=api, replicas=2)","executed_args":{"deployment":"api","replicas":2},"approver":"sha256:4dc68d3cd589b641","note":"ten would exhaust the node pool"}
{"decided_at":"2026-08-16T09:02:11Z","session":"incident-123","workload":"gke-triage","specialist":"change-executor","tool":"rollout_restart","outcome":"reject","authority":"operator_verdict","disposition":"refused_by_operator","proposed_key":"rollout_restart(deployment=api)","proposed_args":{"deployment":"api"},"approver":"sha256:4dc68d3cd589b641","note":"restarting loses the heap dump"}
```

Two fields answer two different questions. `outcome` is what the operator
chose. `disposition` is what mast did — `authorized`, `refused_by_operator`,
or `refused_by_mast` — because a call the gate refused for its own reasons
(an unattributed edit, a malformed verdict, a deny policy) is not a human
saying no, and a dataset that conflates them teaches the wrong lesson.
`authority` is `operator_verdict` for a call somebody answered and
`change_set_grant` for one that fired on a grant minted by an earlier
answer, so a file cannot show one approval where four calls ran.

**Approver identities are digested by default.** The row carries `sha256:`
plus sixteen hex characters. The digest is stable, so you can still group by
approver, count how many distinct people approved a class of change, or find
the identity that approves everything — without naming anyone.
`--include-approver` exports raw identities instead, and `_meta.redaction`
records which mode produced the file (`approver_digest` or `none`), so a
redacted export cannot be mistaken for a raw one. Machine identities —
`mast:internal`, `mast:scheduler` — pass through in the clear: they name a
mechanism rather than a person, and hiding them would hide the one thing
worth knowing about an unattended run.

:::caution[Tool arguments are exported verbatim]
Argument values are **not** redacted, in either mode. The
proposed→executed pair is the entire label; strip it and the file records
only that somebody edited something. That makes an export exactly as
sensitive as the arguments your tools take — namespaces, hostnames, object
names, and anything else a remediation call carries. `--out` creates the
file `0600`, the same warning travels inside it as `_meta.warning`, and you
should treat the result with the same care as the cluster it describes.
:::

The export is inert. Mast never reads it back, nothing scores these rows,
and nothing retrains on them — what you do with the file is yours. It is
also a separate artifact from mast's own ported eval corpus, deliberately:
two datasets, two contracts.

## The gate is not the only boundary

Everything above is what mast enforces. What the *cluster* will accept from
mast is a separate grant, and on Kubernetes it should be a narrower one: an
approved call still has to get past the API server. See [cluster
permissions](/reference/cluster-permissions/) for the read/write RBAC split
that ships with the deployment manifests — and for the GKE IAM binding that
decides whether that split bounds anything.
