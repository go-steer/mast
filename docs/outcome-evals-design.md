# Outcome evals: design

**Status: schema settled and loaded; the runner is unbuilt.** The corpus and its loader landed
2026-09-05 (`internal/evals/outcome`, `testdata/outcome/`) — §5.1 records what the loader refuses and
the two further corrections building it found. The fixture provisioner and the runner are the next
two units of #297.

This document is the argued-with result of an external
specification ([`assess/eval-gates/`](https://github.com/go-steer/mast/issues/299), 2026-09-04) that
arrived as a role catalog plus seven cases. It settles what mast adopts, what it corrects, and what
substrate the tier runs on, **before** any Go is written — which is the sequencing
[#294](https://github.com/go-steer/mast/issues/294) asked for. The gate itself is
[#297](https://github.com/go-steer/mast/issues/297); the umbrella is
[#298](https://github.com/go-steer/mast/issues/298).

A dev/testing plan, like [`./v0.3-plan.md`](./v0.3-plan.md). No docs-site mirror.

---

## 1. The claim, and the gap it closes

> **A real model's behaviour can red the build.**

Nothing mast gates on today is a statement about whether the agent is any good at the job.
Composition-time refusal, the mutation-classified catalog, durable per-specialist budgets,
exactly-once replay — all mechanically checked, all green, and all properties of how mast is *built*.

The four-tier split in [`./v0.3-plan.md`](./v0.3-plan.md) §2 says so out loud, and a fifth tier was
added in v0.4:

| Tier | Proves | Provider | Gates? |
|---|---|---|---|
| **S — smoke** | it builds and runs offline | echo | yes |
| **U — UAT** | the *system* behaves | `scripted` / `toolactor` | yes |
| **E — deterministic eval** | mast's *differentiators* hold | `scripted` / `toolactor` | yes |
| **J — judge / live** | *agent quality*, LangChain-comparable | real provider | **no — nightly, reports** |
| **C — cluster** | guarantees hold against a real API server | `toolactor` | no — opt-in |
| **O — outcome** *(this doc)* | *the agent's behaviour*, against a real cluster | **real provider** | **yes, on an admitted roster** |

Tier E gates and scores *scripted* trajectories, and `internal/evals/harness/harness.go:23-29`
already refuses to dress that up as more than it is: scoring a trajectory requires a model that
chooses, and a scripted provider does not choose. Tier J has a real model and **reports**. Tier C has
a real cluster and no real model. **O is the intersection nothing occupies: a real model, a real
cluster, and a red that blocks a merge.**

O is not a replacement for E, which gates mast's *guarantees* and is a different and equally
necessary thing. It is not a rubric to tune until the number moves — that stays rejected on the v0.3
reasoning (teaching to the test). Its metric is pass/fail per case, never a score.

**Naming.** Per §2's tier-prefix rule, cases are referenced as `O-<case-id>`: `O-crashloop-rca`,
`O-crashloop-evidence-chain`.

---

## 2. What mast adopts from the specification

Two rules, taken whole.

**A case may not name a cluster, a namespace, or an object except through a fixture role.**
`fixtures.yaml` is the only file allowed to know where anything is. This is what lets one corpus run
against a scratch kind cluster, a shared dev cluster and CI unchanged, and it is the reason the
substrate decision in §4 does not require rewriting cases.

**A role's `probes` are confirmed present before the agent starts.** Not a health check. It is the
only thing that entitles a later absence to be called a *violation* rather than an environment that
was never provisioned. `no-pdb-probe` is the case that proves it: the fixture *is* an absence, the
tempting "helpful" action is creating the very PodDisruptionBudget whose absence is the fixture, and
a pathless nameless `absent` check over the namespace is what catches it. Without the probe, an
unprovisioned namespace and one the agent emptied are indistinguishable afterwards.

The loader enforces the corollary in both directions: every subject a case asserts on must appear in
some role's `probes`, and every probe must be asserted on by something.

Two verifiers are also taken as-is:

- **`changed_count_eq`** — over the matched set, exactly *n* objects have a changed
  `metadata.generation`. This is a **blast-radius ceiling**, and mast has nothing like it: the write
  gate adjudicates *verb* (`pkg/approval`'s mutation predicate) and nothing anywhere gates *scope*.
  It is the cheapest expression of "asked to fix one Deployment, the agent may have changed one", and
  it needs the runner to snapshot generations before the run — which is why that snapshot is in
  #297's checklist even though no admitted case uses it yet.
- **`stable_for`** — reads a value over a settle window. On `restartCount` it is the difference
  between "the apply was accepted" and "the incident stopped", which is the single most common way an
  agent reports success on work it did not do.

And **`intent_satisfied` is right to score against `testdata/evals/intents.yaml` rather than tool
names**, consistent with the decision recorded at `docs/README.md:149`: name-level overlap punishes
consolidation, and one `k8s_cluster_health` call answering what upstream answers in four should not
score 1/4.

---

## 3. What mast corrects

Five corrections. Four are checks that do not survive contact with mast's runtime; the fifth is a
factual error in the source's own argument that would otherwise be carried forward into this
document.

### 3.1 `request_approval` is not a tool, and must not become one

Zero hits across the repo. mast's write gate is a **runner plugin** — `pkg/approval/plugin.go:134`
registers `beforeTool` as a `BeforeToolCallback` — which parks a mutating call before it fires. The
agent never invokes anything to ask, and it should not: a gate the model can decline to invoke is not
a gate.

So `crashloop-remediate-and-verify`'s `approval-was-requested-before-mutating` is unimplementable as
written. It is also self-refuting: it is a **catastrophic safeguard** built on `tool_called`, which
the same specification's own `tool_called` section forbids — *"a check that reads the agent's own
account of itself is not a boundary."* Correct, and it applies to itself.

**The park record is the boundary; the trace is not.** The replacement is an evaluator-facing read
over the durable session event log, provisionally `approval_requested` with `for_effect:` naming the
gated call. Specified in [#295](https://github.com/go-steer/mast/issues/295); prerequisite for any
mutating case.

### 3.2 `reverts_to_captured_state` has nothing behind it

No revert, undo, prior-state or captured-state concept exists in `pkg/effects` — verified by grep,
which returns nothing relevant. The v0.4 typed change set (`pkg/approval/changeset.go`'s
`ProposedChange`) is a forward tool-plus-arguments, not a transaction with an inverse.

The source calls this check "the reason that case exists", and it is right about the importance and
wrong about the category: **this needs a product feature, not a verifier.** An unattended agent that
can act and cannot undo is a gap whether or not anyone ever writes an eval for it, which is why it is
filed as [#296](https://github.com/go-steer/mast/issues/296) against the runtime and not against this
tier.

Note what the check asks for and what it does not: it asserts the revert path was recorded
**regardless of outcome**, so an agent that happened to succeed without planning a revert has not
demonstrated the property. That is a good test, and it needs only #296's *capture* half. The
*restore* half is the product feature and can follow.

### 3.3 `manifest_dry_run` is deferred, and is the first thing to add

Flagged by the source itself as proposed-not-specified. Agreed on both halves: it is the
highest-value verifier in the corpus that is not a cluster read — a manifest that would be rejected
is not a remediation, and phrase matching cannot tell the difference — and it waits, because it needs
runner support that does not exist. It is the first extension after the admitted roster is stable,
and `pdb-remediation-proposal` is where it lands.

### 3.4 `intent_satisfied` cannot express "the agent read two different objects"

`crashloop-evidence-chain` asserts `intents: [inspect.workload_spec, discover.abnormal_pods], mode:
all` and predicts in a comment that if the table cannot separate those, that is "a gap in
`intents.yaml` worth closing".

**It is not a gap.** `testdata/evals/intents.yaml:184-196` declares `k8s_triage_workload` as
satisfying *both* in a single call — *"the consolidation case in one tool: a correlated snapshot of
one workload"* — and that is exactly the tool a mast agent reaches for on this scenario. `mode: all`
passes on one call, and the check cannot distinguish one read from two, which is the entire thing the
case exists to measure.

This is worth stating as a **boundary of the intent layer** rather than a bug, because the fix people
will reach for is wrong. The intent table is deliberately indifferent to how many calls produced an
answer (`docs/README.md:149`) — that indifference *is* the consolidation thesis — and splitting an
intent to make a read-count assertion work would reintroduce name-level matching through the side
door. If a case needs "two distinct objects were read", that is a different check reading the trace
directly, not an intent assertion.

The case survives without it: its two `report_contains` halves demand a conjunction across facts that
live on different objects (`137`/`lastState` from the Pod, `64Mi` from the Deployment) however many
calls fetched them. **Disposition: the check is reclassified from required to diagnostic, not
deleted** — see §6 for why deleting it is the worse option.

### 3.5 The Vacuity section attributes an upstream defect to mast, and reverses its direction

The source says *"mast's ported `tool_coverage` scored 1.0 on all 31 rows by reading a key no row
carried."* That is the **upstream LangChain** defect: `evaluators.py:72-79` reads
`expected_trajectory` where the field is `expected_tools`, takes the early return, and yields 1.0 on
every scenario of every run (`docs/v0.3-plan.md:119`).

mast's `tool_coverage` returned **0.000** on every row. The mast-side defect was different and
smaller: `MetricReach` reported it `31/31 scorable` while it scored a constant zero, because reach
counted *declaration* rather than *satisfiability* (`docs/README.md:193`).
[#174](https://github.com/go-steer/mast/issues/174) fixed it and added
`harness.CorpusSummary.DeadDiagnostics` — which is precisely the rule the section argues mast should
adopt.

The argument is right and mast already implemented it. The history is recorded here so the next
reader does not re-derive it, and because §6 depends on getting the direction right: the existing
machinery does **not** do what the source assumes it does.

---

## 4. Substrate: which cluster, and which tool surface

Not in the source, not on the issue, and the largest thing this document settles. It was found by
measurement, and it inverts the cheap answer.

### The question

The corpus needs a real cluster with planted objects. Three substrates are available:

| | Cluster | Tool surface | Cost |
|---|---|---|---|
| **(a)** | live GKE | hosted GKE MCP (`https://container.googleapis.com/mcp`) | recurring cluster time; cannot gate a PR from a fork |
| **(b)** | kind | `k8s-lookout`'s read-only MCP server | free, CI-able |
| **(c)** | kind | an extended `testdata/live/kubemcp` | free, and measures a stand-in nobody ships |

(a) is what the shipped anchor bundle uses: `examples/workloads/gke-triage/mcp.json` wires exactly
one server, the hosted GKE MCP endpoint, and `cluster.go`'s handlers resolve clusters through
`container.googleapis.com` `GetCluster` — so that surface **cannot** be pointed at kind. (c) is out
on principle: `testdata/live/kubemcp` exposes two tools (`get_deployment`, `scale_deployment`),
nowhere near enough for `crashloop-evidence-chain`, and extending it would mean grading the agent
against a tool surface mast does not ship.

### The measurement that settles it

`testdata/evals/intents.yaml` has exactly two tool sections: `upstream_tools:` (LangChain's 23
`kubectl_*` names) and `lookout_tools:` (lookout's 11 `k8s_*` names). **No GKE MCP tool appears in
the intent table at all.**

So on substrate (a), every `intent_satisfied` check in the corpus is **vacuous** — the agent's calls
resolve to no intent, the check scores nothing, and under §6's rule the aggregate reds on a fact
about the table rather than on the agent. Four of the seven cases carry an `intent_satisfied` check.

That is not a cost trade-off. Substrate (b) is the only one on which the corpus's intent checks
measure anything, and lookout reaches a cluster through plain `clientcmd` / `rest.InClusterConfig`
(`k8s-lookout/pkg/kube/client.go:118-147`) with explicit `--kubeconfig` and `--context` — which is
also exactly the isolation discipline tier C already enforces.

### Decision

**The O tier runs against kind, through `k8s-lookout`'s read-only MCP server.**

Consequences, stated rather than discovered later:

- **The tier is free and gate-able.** No credentials for the cluster half, no project, no leased
  slot. The provider half is still metered — a real model is the point — and §7 budgets it.
- **The fixture provisioner is an extension of tier C, not a new thing.**
  `scripts/live-kind-v0.4.sh` already creates a throwaway cluster named `mast-live-*`, refuses to
  adopt one it did not make, writes a single-context kubeconfig under `${TMPDIR}` it refuses to
  overwrite, verifies exactly one context exists, and passes `--kubeconfig`/`--context` on every
  invocation with `KUBECONFIG` unset. That discipline is mechanical, reviewed, and carries over
  verbatim. **The ambient `current-context` is never resolved, on any path** — that rule extends to
  the O runner.
- **The tier runs a bundle authored for it, not the shipped anchor.** This is the real cost. The
  O-tier bundle wires lookout; the anchor `gke-triage` bundle wires the hosted GKE MCP server and
  stays covered by tier C and the J nightly. The two read paths are genuinely different surfaces, and
  the tier measures the one the intent layer and the parity claim are both built on.
- **The mutating case is unaffected by this choice and blocked on other things.** lookout's MCP
  server is read-only by design, so when `crashloop-remediate-and-verify` is admitted it needs a
  write tool from somewhere — the `change-executor` roster's, not lookout's. That is a #295/#296
  question, not a substrate one, and it is out of scope here.

**Named as an open question rather than resolved** (§9 OQ1): whether `intents.yaml` should grow GKE
MCP rows, or whether the intent layer is lookout-scoped by design and should say so in its header.
The second reading is probably right — the table exists to score a *consolidated read path* against
upstream's fragmented one, and the GKE MCP surface is neither — but it is not settled here.

---

## 5. Schema

Normative. A loader rejects anything not described here rather than ignoring it, in the
composition-time-refusal style the project already uses.

### Case

```yaml
id: string                  # must match the filename
name: string                # one line, present tense, states the capability
domain: string              # free text; reporting only, never coverage gating
prompt: string              # verbatim, what the agent receives
expected_output: string     # prose, for a human reading a red cell. NOT parsed.
fixtures: [role, ...]       # roles from fixtures.yaml
repetitions: int            # default 5
mutating: bool              # default false; governs sequencing and cluster leasing
verification_spec: [check, ...]
```

`expected_output` deserves the warning the source gives it: it is prose that nothing reads, sitting
next to the spec that is the actual grader, and it is easy to write a rich `expected_output` and a
thin spec and believe you have tested the rich thing. It stays because a human staring at a failed
run needs to know what "right" looked like. **The loader must not be tempted to parse it.**

### Check

```yaml
- name: kebab-case-sentence          # reads as a claim, so a failure reads as its negation
  role: objective | safeguard
  requirement: required | diagnostic # default required; see §6
  severity: catastrophic             # safeguard only; trips the never-demotable rung
  mode: assert | converge            # assert = once at end; converge = poll
  check:
    type: ...
```

`assert` vs `converge`: a transcript is immutable once the run ends, so polling a failed report check
only waits out the timeout. **Anything reading the transcript is `assert`.** Cluster reads after a
*mutating* case are the case for `converge`, since reconciliation takes time.

`requirement:` is mast's addition and is the subject of §6.

### Check types

| Type | Reads | Status |
|---|---|---|
| `report_contains` | the final report | adopted |
| `intent_satisfied` | the trace, via `testdata/evals/intents.yaml` | adopted, with §3.4's boundary |
| `tool_called` | the trace | adopted, **never as a mutation safeguard** |
| `cluster_resource_property` | the cluster | adopted, incl. `stable_for` / `changed_count_eq` |
| `approval_requested` | the durable session event log | **new**, replaces §3.1 — #295 |
| `effect_recorded` | the durable effect journal | **blocked** on #296's capture half |
| `manifest_dry_run` | the report + a server-side dry run | **deferred**, §3.3 |

`cluster_resource_property` keeps the source's shape verbatim, including the two properties that
carry the most weight: omitting `resource_name` turns it into a **set assertion over the namespace**
(`kind: poddisruptionbudget`, `op: absent`, no name = *no PDB of any name appeared here*), and
`fixture_role` addresses the cluster so a case never carries a literal location.

### 5.1 What the loader refuses

`internal/evals/outcome` is the schema above as composition-time refusal. Every rule below names a
way a case can look like a measurement and not be one; none is tidiness.

| Refused | Because |
|---|---|
| a key the schema does not describe, **including inside a `check:` body** | a typo is a silently dropped assertion, and the case still runs and still reports green |
| a check on a subject no probe confirmed | its later absence cannot be told from an environment that was never provisioned |
| a probe no check asserts on | fixture nobody reads; the reverse half of the corollary |
| a role no case declares | provisioning time bought for nothing |
| `report_contains` with no phrases; a phrase in two lists | passes on every report ever written; or is a contradiction no report can pass |
| `intent_satisfied` naming an intent no reachable tool satisfies | a rung that cannot fire |
| `intent_satisfied` `mode: all` over a set **one lookout tool satisfies whole**, unless `diagnostic` | §3.4, mechanically: it cannot tell two reads from one |
| `tool_called` with `role: safeguard` | §3.1 — the trace is the agent's account of itself, and a planner-dispatched specialist's calls need not be in it |
| a `safeguard` marked `diagnostic` | a safeguard that does not gate is one in name only |
| `mode: converge` on anything reading the transcript | the transcript is immutable once the run ends; polling waits out the timeout |
| `approval_requested`, `effect_recorded`, `manifest_dry_run` | named in this document, not built — refused with the issue number rather than accepted and ignored |

Two further corrections, found while building it and not present in §3:

**A check may not carry both `fixture_role` and `namespace`.** The source's schema block reads
`fixture_role: crashloop-workload  # addresses the cluster; never a literal name` and then lists
`namespace: seeded-debug` four lines below it — and every one of the seven cases writes both. Two
addresses for one object can disagree, and the one that loses is the role, which is the one the
provisioner actually used. The role's namespace is the namespace.

**The restore obligation is enforced in the reverse direction.** §8 states `restore_required_after`
as loader-enforced, and the obvious reading — a name there requires a restore path — is the weaker
half: a name is easy to omit. So the loader also refuses a **`mutating: true` case that its own roles
do not name**. Admitting `crashloop-remediate-and-verify` cannot happen without the same commit
stating that the fixture gets put back. The forward direction is kept too, but as an anti-typo rule:
a name that resolves to nothing is an obligation nothing enforces, so mast's catalog carries
`restore_required_after: []` rather than a forward declaration.

---

## 6. Vacuity: required, diagnostic, and why the existing machinery does not transfer

Every check must be able to report `vacuous`, following `evals.Result.Vacuous`
(`internal/evals/evaluate.go:55-67`), which mast already has. A `report_contains` with empty
`required_phrases`, an `intent_satisfied` naming an intent no reachable tool claims, a
`cluster_resource_property` whose role was never provisioned: each passes while measuring nothing.

**The aggregate reds on a vacuous *required* check.** A suite that evaluated nothing and reported
success is worse than no suite.

### The correction: `DeadDiagnostics` is deliberately non-gating

The source assumes mast's existing machinery already implements this rule. It does not, and the gap
is exact. `harness.CorpusSummary` carries two lists:

- **`Dead`** — metrics that score nothing anywhere. Appended to `problems`, and `Summary.OK` reads
  `problems`. **This gates.**
- **`DeadDiagnostics`** — diagnostic columns that score nothing anywhere. `harness.go:133-141`:
  *"Separate from `Dead` because it does not gate — a diagnostic is not a parity claim, so a dead one
  is not a red run — and separate from silence because a constant that nobody names is read as a
  measurement."* **This deliberately does not gate**, and `harness.go:288-303` records why in the
  code: #179 landed the dead-diagnostic list in `problems` while describing it as non-gating, which
  was true only because no diagnostic was dead at the time — #174's `tool_coverage` fix would have
  turned a permanent property of the ported corpus into a red suite.

That near-miss is the precedent this tier inherits, and it maps cleanly:

> **required check ↔ `Dead` (gates). diagnostic check ↔ `DeadDiagnostics` (reports).**

The classification is **per-check and explicit**, defaulting to `required`. It has to be, because a
check that is *permanently vacuous by construction* is not a defect to fix — and marking it required
would red the aggregate forever on a fact about the runtime.

### Which is why `evidence-chain`'s intent check is reclassified, not deleted

§3.4's check is exactly that shape: vacuous by construction on mast's intent layer, permanently, for
a reason the project chose on purpose. Deleting it loses the record that "the agent read two distinct
objects" is unmeasured. Keeping it required reds the gate forever on the consolidation thesis. So it
ships as `requirement: diagnostic` and reports `vacuous` — the same disposition, for the same reason,
that `tool_coverage` has permanently in `DeadDiagnostics`.

It is also the first real exercise of the vacuity rung, which is worth having before a case depends
on the rung being correct.

**#297's exit criterion stands as written**: a vacuous *required* check reds the aggregate, proven by
a test that fails without it. Note that this is new code, not a wiring of `DeadDiagnostics`.

---

## 7. Repetitions, the gate, and demotion

**Five repetitions.** The distinction between an agent that diagnoses an OOM 3 times in 5 and one
that does it 5 in 5 is the entire product, and a single-shot suite cannot see it. Five is also 5× the
wall clock, which is the thing that got away from the sibling project.

**A case reds the gate only if *all* its repetitions fail.** The absolute safeguard rung is the
exception: **one catastrophic violation in one repetition reds, always**, and is never demotable.

**Demotion.** A flaky case is demoted off the blocking roster rather than deleted — it keeps running,
keeps reporting, stops blocking. The date and the measurement go into the case file. Demotion is a
committed diff, never a runtime flag.

**Budget the wall clock before the roster, not after.** This is a guardrail rather than a preference:
a sibling project that ran this experiment admitted 13 cases, went merge-blocking the next day,
demoted two and deleted one within 72 hours — 23% of the roster, and the two demoted were the ones
exercising the headline capabilities — while its ceiling went 85 → 150 → 240 → 360 minutes in nine
days. Decide the number first and let it constrain the roster. #297 states the ceiling.

**Every verifier needs a discriminating fact.** A substring match on `"behind"` or
`"PodDisruptionBudget"` is satisfiable by a model that never touched a cluster. `required_phrases` is
only fair on nouns **we planted** — we chose `payments-api`, so demanding it back is not a vocabulary
test. The safeguards pinning cluster state are what actually discriminate.

**Never ship a rung that cannot fire.** The same sibling project's rate-based rungs are inert because
their baseline store points at a bucket that does not exist, and the gate ran for days with "the
rate-based half, silently absent" in its own comments.

---

## 8. Roster and sequencing

**Three cases admitted, not seven.** Seven × 5 repetitions is 35 agent runs, one of them mutating
against a cluster that must run last and alone — plus a provisioner, generation snapshotting and a
restore path, none of which exist. That is not a first gate; it is a first gate and a platform.

The admitted set is **the crashloop triple** — `crashloop-rca`,
`crashloop-misleading-symptom`, `crashloop-evidence-chain`. They share the single
`crashloop-workload` fixture, so the whole admitted set costs one provisioned namespace, and
`crashloop-rca`'s own header says it: *"build all three or the fixture is underused."*

Order of admission after that: `rbac-overgrant-probe` (stronger than `crashloop-rca` — two of its
three planted nouns are absent from the prompt; held back only because its fixture is the second
namespace), then `no-pdb-probe` and `pdb-remediation-proposal` (shared fixture, absence discipline,
and `manifest_dry_run`'s home), then `crashloop-remediate-and-verify` last.

### `mutating: true` is a runner obligation, not a case annotation

`crashloop-remediate-and-verify` shares `crashloop-workload` with all three admitted cases and
changes the exact field they pin at `64Mi` as a **catastrophic safeguard**. A runner that ignores the
flag produces three catastrophic violations and a red gate that has nothing to do with the agent.

So the flag is honoured by the runner from day one, before any case sets it:

- a `mutating: true` case never runs concurrently with any case sharing one of its fixture roles;
- it is sequenced **last**;
- `fixtures.yaml`'s `restore_required_after` is a **loader-enforced obligation** — a role naming a
  case there requires the runner to expose a restore path, or the corpus fails to load.

Building the runner against three read-only cases is what makes this a real risk, and stating the
obligation now is what removes it. Same reasoning for the generation snapshot in #297's checklist: it
is cheap now and expensive to retrofit.

---

## 9. Open questions

**OQ1 — Is the intent layer lookout-scoped by design?** §4 found that `intents.yaml` has no rows for
the hosted GKE MCP surface, which makes every `intent_satisfied` check vacuous against the shipped
anchor bundle. Either the table grows GKE MCP rows, or its header states that it scores a
consolidated read path and the GKE MCP surface is out of its scope. Leaning to the second; not
settled. Affects nothing in the admitted roster.

**OQ2 — Does the O tier run per-PR or per-merge?** Per-PR is what "can red the build" means, and it
is also a metered real-model run on every push. A merge-queue-only gate is the obvious compromise and
weakens the claim. #297 decides it alongside the wall-clock ceiling.

**OQ3 — How does a case distinguish "the gate asked and the harness said yes" from "the harness
answered a question nobody asked"?** The harness must auto-approve or nothing mutating completes, so
`approval_requested` observes the *request* while the runner supplies the answer. #295's design has
to say how. Blocks the mutating case only.

**OQ4 — Does `manifest_dry_run` need a cluster?** Server-side dry run does; a client-side schema
validation does not, and is weaker in exactly the way the check exists to avoid. Decide when it is
built (§3.3).

---

## 10. Out of scope

- **A score.** The tier is pass/fail per case. No rubric, no aggregate quality number, nothing to
  tune. Rejected on the v0.3 reasoning and rejected again here.
- **Replacing tier E or tier J.** E gates mast's guarantees on a scripted provider; J produces the
  LangChain-comparable numbers. Both stay.
- **Porting kube-agents' suite.** Every case on that roster is read-only and every safeguard asserts
  the planted defect survived. The loop-closing case is the one this corpus adds, and it is the
  reason for building a tier rather than porting one.
- **Cluster-level fixtures.** Every role is namespace-scoped or cluster-scoped-but-additive, so one
  cluster carries all of them. A control-plane-level fixture would grow `fixtures.yaml` slots; no
  case needs one.
- **The mutating case.** Blocked on #295 and #296, admitted last, and not v0.7's gate.

---

## 11. Resolved decisions

| Decision | Date |
|---|---|
| A sixth test tier, **O — outcome**: a real model against a real cluster, gating on an admitted roster. Cases referenced as `O-<case-id>` | 2026-09-05 |
| Fixture-role indirection and probes-before-run are adopted whole; the loader enforces the corollary in both directions | 2026-09-05 |
| `changed_count_eq` (a blast-radius ceiling — mast gates on verb and nothing gates on scope) and `stable_for` are adopted | 2026-09-05 |
| `request_approval` is refused as a tool: a gate the model can decline to invoke is not a gate. The park record in the durable log is the boundary; the trace is not. Replaced by `approval_requested` (#295) | 2026-09-05 |
| `effect_recorded: reverts_to_captured_state` needs a product feature, not a verifier (#296). Its *capture* half unblocks the eval; *restore* can follow | 2026-09-05 |
| `intent_satisfied` cannot express "two distinct objects were read", and that is a **boundary of the intent layer**, not a gap in `intents.yaml`. Splitting an intent to fix it would reintroduce name-level matching through the side door | 2026-09-05 |
| **The O tier runs against kind, through `k8s-lookout`'s read-only MCP server** — not against GKE through the hosted GKE MCP server. Not a cost trade-off: `intents.yaml` carries no GKE MCP rows, so on that substrate every `intent_satisfied` check is vacuous. The fixture provisioner extends tier C's isolation discipline verbatim | 2026-09-05 |
| Vacuity is classified **per check**, `required` by default: required ↔ `Dead` (gates), diagnostic ↔ `DeadDiagnostics` (reports). A check vacuous *by construction* is diagnostic, never deleted — `evidence-chain`'s intent check is the first one | 2026-09-05 |
| Five repetitions; a case reds only if all fail; the catastrophic safeguard rung reds on one and is never demotable; demotion is a committed diff carrying the date and the measurement | 2026-09-05 |
| Three admitted cases (the crashloop triple), not seven. `mutating: true` is a runner obligation honoured from day one, before any case sets it | 2026-09-05 |
| The schema is enforced as **composition-time refusal** (§5.1), not documented and trusted. Two corrections beyond §3: a check may not carry both `fixture_role` and a literal `namespace` (the source's own schema block does), and the restore obligation is enforced from the **case** side — a `mutating: true` case its roles do not name fails to load | 2026-09-05 |

---

## 12. Related

- [`./v0.3-plan.md`](./v0.3-plan.md) §2 — the S/U/E/J tiers this extends, and the "a scripted
  provider does not choose" argument the O tier exists to answer
- [`./v0.4-plan.md`](./v0.4-plan.md) §2 — tier C, whose isolation discipline the fixture provisioner
  inherits
- [`./README.md`](./README.md) — the consolidation thesis (row at :149) that §3.4 is a consequence of
- [`./orchestration-design.md`](./orchestration-design.md) — the write gate whose park record §3.1
  reads instead of the trace
- [#298](https://github.com/go-steer/mast/issues/298) umbrella · [#294](https://github.com/go-steer/mast/issues/294) this doc · [#295](https://github.com/go-steer/mast/issues/295) approval observability · [#296](https://github.com/go-steer/mast/issues/296) revert · [#297](https://github.com/go-steer/mast/issues/297) the gate
