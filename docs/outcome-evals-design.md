# Outcome evals: design

**Status: the corpus loads, the fixture provisions, and a run can be graded; the runner is
unbuilt.** The corpus and its loader landed 2026-09-05 (`internal/evals/outcome`,
`testdata/outcome/`) — §5.1 records what the loader refuses and the two further corrections building
it found. The fixture provisioner landed the same day — §5.2 — and `crashloop-workload` has been
stood up on kind end to end: 23 seconds from `kubectl apply` to two OOMKills. The four verifiers and
the gate followed — §5.3 — and against that live cluster the catastrophic safeguard reads `64Mi`,
trips when the limit is raised to `128Mi`, and reds the board on the never-demotable rung. What is
left in #297 is the runner itself: a real model, the three cases wired to CI, and the wall-clock
ceiling (OQ2).

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

### 5.2 The fixture provisioner

`internal/evals/outcome/{cluster,provision}.go`. One manifest per role under
`testdata/outcome/fixtures/<role>.yaml`, applied to a throwaway kind cluster.

**Isolation is tier C's, mechanically rather than by review.** §4 said the discipline "carries over
verbatim"; the four guards are the same four (`mast-outcome-<pid>`, no adoption, a `${TMPDIR}`
kubeconfig kind is not allowed to merge into, exactly one context, both flags on every call with
`KUBECONFIG` stripped). Two things are stronger here than in the shell. The context count is
**parsed** rather than grepped — `- context:` is a string that can appear in a comment, and this is
the check the other three lean on. And the rule *"the ambient `current-context` is never resolved, on
any path"* is now a test rather than a sentence: `TestOneExecPath` fails if any `exec.Command` appears
outside the single constructor that strips `KUBECONFIG`, which is what makes the two argv/env tests
general instead of true only of the paths that happen to exist today.

**What the provisioner refuses**, in the same spirit as §5.1:

| Refused | Because |
|---|---|
| a role with no readiness condition registered in the package | it would report ready the instant `kubectl apply` returned — a full minute before the state the cases grade against exists |
| a manifest setting `metadata.namespace`, even one that agrees with the catalog | the same "two locations that can disagree" the loader refuses in a check; the role catalog is the only file that knows where a fixture lives |
| a manifest containing a `Namespace` object | same reason; the provisioner creates it from the role |
| an object not labelled `mast.dev/fixture-role: <role>` | the label is how a planted object is told from a stray one, which is what makes `exclusive` enforceable and a pathless `absent` check meaningful |
| a probe that resolves to an object carrying another role's label | a stray can satisfy a precondition and then vanish mid-run, which reads afterwards as the agent having deleted it |
| an object of an `exclusive` kind that this role did not plant | a set assertion reads the whole live set; one leftover turns it into a permanent red that has nothing to do with the agent |

**Readiness is per role and stated in Go, not inferred.** `crashloop-workload` is ready when a pod
carrying the role label is `Running`, has been `OOMKilled`, and has restarted at least twice — all
three, because each alone is satisfied too early: `Running` holds a second after apply, one restart
is any transient, and a first kill is not the loop the prompts describe. Measured at ~50s cold and
~23s with the image side-loaded. Side-loading (`kind load docker-image`) is why: the cluster is never
allowed to reach a registry, and the image list is collected from the manifests rather than kept
beside them, because a second list is a list that drifts.

**The generation snapshot is taken before the run because it cannot be taken after.**
`metadata.generation` is a running count, and "did this object change during the run" has no answer
reconstructible from the object afterwards. `Changed` counts appearances and disappearances as
changes, and **refuses** a snapshot containing subjects whose kind has no generation at all — a Pod, a
ConfigMap — rather than counting the rest: a blast-radius ceiling that quietly excludes what it
cannot measure gives the reassuring answer.

The end-to-end provision is opt-in and is not a presubmit, for tier C's reason — it creates a
Kubernetes cluster:

```
MAST_OUTCOME_KIND=1 go test ./internal/evals/outcome/ -run TestProvisionAgainstKind -v
```

It doubles as the by-hand stand-up path for authoring a case (`MAST_OUTCOME_KEEP=1` leaves the
cluster up). There is deliberately no second script: a second entry point is a second place for the
isolation discipline to be got wrong.

### 5.3 The verifiers and the gate

`internal/evals/outcome/{verify,board}.go`. Grading is one step and deciding is another: a `Verdict`
says what happened, and `Board.Red` says what it costs. Keeping them apart is what makes it possible
to demote a case without touching a verifier.

**Run-time vacuity is the only kind left.** §5.1's loader already refuses everything decidable from
the corpus alone — an empty phrase list, an unreachable intent, a `mode: all` set one tool satisfies
whole. What survives to run time is three things, and each is recorded as `Vacuous` rather than as a
pass or a fail:

| Vacuous at run time | Because |
|---|---|
| a report check against an empty final report | a forbidden-phrases check passes hardest on the run where the agent produced nothing at all |
| an intent check where the agent called tools and **none** of them are in the intent table | §4's finding arriving at run time: the check measured the tool surface, not the run |
| a property assertion whose path resolves on no matched object, or whose matched set is empty | it is a constant, and under `op: ne` the constant is `true` |

Zero tool calls is deliberately **not** vacuous. "The agent called nothing" is exactly the
information an intent check exists to carry, and classifying it as unmeasured would lose the one
failure mode most worth seeing. `tool_called` is never vacuous at all: the names are literals and the
trace is a literal record of what was called.

**Which way a vacuous check falls out is not the property.** Both directions are recorded the same
way, because the passing direction is the dangerous one — nobody investigates a green cell.

**Four rungs, in descending order of how little argument they admit.** `Board.Red` returns every
reason, catastrophic ones first:

1. a **catastrophic** safeguard failed in any repetition of any case — always, and demotion does not
   reach it;
2. a **required** check was vacuous in any repetition (this is §6), reported once per check rather
   than once per repetition;
3. **every** repetition of a case failed — not *any*, per §7;
4. a case ran fewer than its repetitions, including one that did not run at all.

Rung 4 is reported before rung 3 and suppresses it: *"0 of 5 ran"* and *"5 of 5 failed"* are
different findings, and reporting the second for the first sends a reader looking at the model. An
**errored run counts as a failed repetition and does not red on its own** — a provider timeout is not
a regression in mast — but every repetition erroring reds under rung 3, with a line that says the
runs did not finish rather than that the checks failed.

**`demoted: {date, measurement}`, both required.** Added now rather than with the runner, because
this is the code that reads it. A demotion with no date cannot be aged out, and one with no
measurement is indistinguishable six months on from a case nobody looked at again. It is a committed
diff, never a runtime flag: what made the sibling project's 23%-in-72-hours survivable at all was
that each demotion was a reviewable change with a reason attached. A demoted case stays on the
report, with its date and measurement — falling off the report is how a demotion becomes permanent by
accident.

**The blast-radius ceiling counts over a set the snapshot actually covers.** A named or
selector-addressed check must be probed, so its subjects are in the pre-run snapshot already. A set
assertion — `kind: poddisruptionbudget`, no name — is deliberately exempt from the probe corollary,
so `Snapshot` walks those matched sets too. Without that, every object in such a set reads as newly
appeared, and a ceiling of `0` fails on a cluster nothing touched.

### 5.4 The tool surface and the runner

`internal/evals/outcome/{surface,runner}.go`, driven by `internal/evals/cmd/outcome`. This is the
half that spends money, so most of what it decides is a budget decision wearing an engineering hat.

**The selection is the intent table, not a list.** §4 settled *which* server; it left *which tools*
open. lookout's `--profile=all` is 34 tools and 160 KB of JSON schema on every model call, most of it
for tools the corpus cannot score. A hand-picked list is the second-list-that-drifts the provisioner
already refuses for images. So the surface is exactly `intents.yaml`'s own `lookout_tools` keys —
eleven tools, 54 KB — which makes the property structural rather than maintained: **every tool the
model can call is a tool the grader can read**, so no run-time vacuity red (§5.3, row 2) can be
caused by an off-table tool. Narrowing further would be worse than expensive: reducing the surface to
the one tool the admitted roster needs turns `intent_satisfied` into a tautology. Eleven leaves a
real choice.

**The server config is built in Go rather than committed as an `mcp.json`.** The kubeconfig path is
per-run, so a committed file would have to reach it through a `${MAST_OUTCOME_KUBECONFIG}` in the
*runner's own* environment — where every other child it launches could also read it. Building the
config instead hands the server one literal path and an otherwise empty environment, and still goes
through the shipped `mcp.NewToolset`, so the stdio launch and env scoping under test are the daemon's
code paths and not a copy of them.

**The child gets `PATH` and nothing else — in particular no `HOME`.** lookout resolves a cluster
through clientcmd, which falls back to `~/.kube/config` when `KUBECONFIG` is unset. A child with
`HOME` whose `KUBECONFIG` failed to apply would read the operator's real cluster and grade a whole
board against it. Without `HOME` there is no fallback to find. That is §5.2's `envWithoutKubeconfig`
rule from the other side, and it is why handing lookout a kubeconfig with no `--context` flag is safe
at all: `verifyKubeconfig` has already proven the file describes exactly one context, that it is
ours, and that `current-context` names it.

**A pinned lookout.** `outcome.PinnedLookout` is a Go constant so the pre-flight's refusal can name
it, and CI reads it back out (`--print-lookout-pin`) rather than repeating it in YAML. A gate whose
tool surface floats reds for reasons no PR author can act on, and a gate people have learned to
ignore is worse than no gate. Bumping the pin is a reviewable diff whose board delta is the argument
for or against it.

**A deadline, not a job timeout.** The pass carries `DefaultCeiling = 20m` as a `context` deadline it
checks between cases. A job timeout reports *"cancelled"*; a deadline reports which cases ran, which
is rung 4's input — and rung 4 exists precisely so a short board reds instead of passing. The
workflow's own `timeout-minutes: 35` is deliberately slack against it: if the outer timeout is what
stops the job, the board is lost.

**One agent, not the roster.** Every run is a single `sre` agent. A planner dispatch would hide the
specialist's tool calls behind the delegating turn the trace carries, and every intent check in the
corpus reads that trace. This is the same boundary `DESIGN.md` draws for the write gate, arriving in
a measurement context.

**Concurrency 3, mutating last and alone.** Read-only cases run in a worker pool against one shared
cluster; §8's mutating sequencing is implemented now, with nothing in the admitted roster reaching
it, so admitting the first mutating case is a corpus change rather than a runner change. `restore`
deletes the fixture namespace and re-provisions from the manifest rather than re-applying it: a
re-apply is not a restore, because the case may have deleted an object.

**Unreachable intents are refused at construction**, against the surface the model will actually be
shown — the run-time half of §7's *never ship a rung that cannot fire*. §5.1's loader checks the same
rule against the table; this checks it against the live selection, which is the one that can differ
when a pinned binary drops a tool.

**The gate measures the mid tier, not mast's frontier default.** The tier gates a *substrate* claim —
that the read path, tool surface, trace adapter and cluster safeguards hold end to end with a model
that chooses — not a frontier-capability claim. All three admitted cases are one OOM diagnosis from a
named namespace, so the headroom a frontier model buys is headroom this corpus does not spend, at
roughly five times the bill on every pull request. `--model` asks a capability question of anything
else without changing what gates.

**Three exit codes, and they only survive a compiled binary.** 0 green, 1 the board is red, 2 the
tier could not run — the distinction the gate's job summary branches on, because one of them is a
finding about mast and the other is a finding about the machine. `scripts/outcome.sh` therefore
**builds and executes** rather than using `go run`, which does not propagate a non-zero child status:
it prints `exit status 2` to stderr and exits 1 itself. The first CI run of the workflow found this
by reporting a red board for a missing container image. `scripts/evals.sh` had the same defect
latently — it documented a code 2 it could not deliver — and is fixed the same way.

**The fixture images are pulled by asking, not by a second list.** The cluster is never allowed to
reach a registry, so the images have to be on the host before a pass; `--print-images` reads them out
of the manifests through the same staging provisioner the pass builds, and CI pipes that into
`docker pull`. Same single-source-of-truth argument as the lookout pin, and the same one §5.2 makes
for collecting the side-load list from the manifests in the first place.

**One session database per run, under `${TMPDIR}` and never `$HOME`.** They survive the pass on
purpose: the cluster is gone by the time anyone reads a red cell, and the session is the only
remaining record of what the agent actually called. ~900 KB for a full pass. `--keep` additionally
leaves the cluster up, which is the by-hand path for authoring a case.

**An errored run reaches the board as a failed repetition, and its checks are still graded** (§5.3),
so `runCase` returns a `Run` on every agent-failure path rather than an error. A provider 429 is the
provider asking us to wait: the model is wrapped in `judge.Retrying` (#239) before it ever reaches
the runner, so a quota blip does not present as a red merge gate.

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
days. Decide the number first and let it constrain the roster.

**The ceiling is 20 minutes** (`outcome.DefaultCeiling`), for a pass of 3 cases × 5 repetitions. It
is a `context` deadline the runner checks between cases, not a job timeout — see §5.4 — so a pass
that runs out of budget produces a short board that reds under rung 4 rather than a cancelled job
with no board at all.

**Measured, on the first full live pass** (`claude-sonnet-5`, lookout v0.23.0, local kind, 3
concurrent workers): **15 runs in 2m47s**, 19–30s each, plus ~90s outside the deadline for cluster
create, image side-load and fixture readiness. So the ceiling is ~7× the measured pass. That is
deliberate and it is the *only* number this design defends: it is large enough that a slow provider
day is not a red, and small enough that admitting a fourth case has to argue for raising it in a
reviewable diff — which is the mechanism the sibling project's 85 → 360 did not have. It is not sized
to the roster §8 wants.

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

**First live board, recorded rather than acted on** (`claude-sonnet-5`, 2026-09-05):
`crashloop-rca` 5/5, `crashloop-evidence-chain` 5/5, **`crashloop-misleading-symptom` 3/5**. The
board is green — a case reds only if *all* its repetitions fail (§7) — and the single failing check
in both losing runs was `the-agent-addressed-the-hypotheses-it-was-given`, which that case's own
header already names as *"the check most likely to false-red"*.

The measurement is left standing. Widening its `any_of_phrases` until it goes 5/5 is teaching to the
test: the check exists to assert the agent said the reporter's hypotheses were *wrong* rather than
quietly answering a different question, and a phrase list grown to fit the runs that failed no longer
asserts that. The two dispositions §7 actually offers are **demotion** (a committed diff carrying
this date and this measurement) and leaving it required and watching the rate; one pass of five is
not enough to choose between them, and this is the number the second pass gets compared against.

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

**OQ2 — Does the O tier run per-PR or per-merge? — RESOLVED 2026-09-05: per-PR.** The compromise
turned out not to be needed. What made per-PR look expensive was *a metered pass per push*, and
`concurrency: cancel-in-progress` on the workflow's ref reduces an eight-push branch to about one
pass. A pass is 15 runs of a mid-tier model against a local kind cluster inside a 20-minute ceiling.
So the gate runs where a gate belongs: on the pull request, before the merge, with the author able to
see the red and act on it. Two costs are stated rather than discovered:

- **A pull request from a fork cannot run it.** GitHub withholds secrets from fork workflows. The
  unconfigured case therefore **fails rather than skips** — the judge nightly skips because a nightly
  that reds on a rotated secret trains its readers to ignore it, but green-because-it-could-not-run
  is §7's rung that cannot fire, which is the exact failure this design is written against.
  `dev/ci/outcome-gate.sh` exits with the reason and the remedy in the job summary, and a maintainer
  runs the tier on a branch in this repo before merging such a PR. There is no version of a
  credentialed metered gate that is also fork-safe.
- **Making `outcome` a *required* status check is a branch-protection change**, separate from landing
  the workflow. Until it is required, the job reds visibly but does not block.

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
| The provisioner extends §5.1's refusals to the manifests (§5.2): no `metadata.namespace`, no `Namespace` object, every object labelled `mast.dev/fixture-role`, and **a role with no readiness condition fails construction**. Readiness is stated per role in Go rather than inferred from `kubectl apply` returning | 2026-09-05 |
| Tier C's isolation discipline is inherited as **tests, not prose**: the context count is parsed rather than grepped, and "the ambient `current-context` is never resolved, on any path" is enforced by a test that fails on any `exec.Command` outside the one constructor that strips `KUBECONFIG` | 2026-09-05 |
| `Changed` **refuses** to count a blast radius over subjects with no `metadata.generation` rather than counting the rest. The excluded-and-reassuring answer is the dangerous one | 2026-09-05 |
| Grading and gating are separate steps (§5.3): a `Verdict` says what happened, `Board.Red` says what it costs. That separation is what lets a case be demoted without touching a verifier | 2026-09-05 |
| **Zero tool calls is a failure, not vacuity**; an agent that called tools none of which are in the intent table *is* vacuity. The first is what an intent check exists to report; the second is §4's substrate finding arriving at run time | 2026-09-05 |
| Rung 4 (**a case that ran fewer than its repetitions**) reds and suppresses rung 3. "0 of 5 ran" and "5 of 5 failed" are different findings, and reporting the second for the first sends a reader looking at the model | 2026-09-05 |
| An **errored run** counts as a failed repetition and does not red on its own — a provider timeout is not a regression in mast — but its checks are still evaluated, because a cluster read is valid whatever the agent did | 2026-09-05 |
| `Snapshot` covers the matched set of every set-assertion `changed_count_eq`, not only probe subjects. A set assertion is exempt from the probe corollary by design, and without this its whole set reads as newly appeared | 2026-09-05 |
| **The tool surface is the intent table's own `lookout_tools` keys** (11 tools, 54 KB), not `--profile=all` (34, 160 KB) and not a hand-picked list. Structural rather than maintained: every tool the model can call is one the grader can read, so no run-time vacuity red can be caused by an off-table tool — while eleven still leaves a real choice, which one would not | 2026-09-05 |
| The MCP server config is **built in Go, not committed as an `mcp.json`**. A committed file would need the per-run kubeconfig path in the runner's own environment, where every other child could read it. The child's environment is `PATH` only — **no `HOME`**, because clientcmd falls back to `~/.kube/config` and a failed `KUBECONFIG` would otherwise grade the operator's real cluster | 2026-09-05 |
| `k8s-lookout` is **pinned** (`outcome.PinnedLookout`), in Go so the refusal can name it and read back by CI so the two cannot drift. A gate whose tool surface floats reds for reasons no author can act on | 2026-09-05 |
| The wall-clock ceiling is **20 minutes**, expressed as a `context` deadline checked between cases rather than a job timeout: a deadline reports which cases ran, which is rung 4's input, and a timeout reports "cancelled" | 2026-09-05 |
| **OQ2 resolved: per-PR**, made affordable by `cancel-in-progress` rather than by a merge queue. The unconfigured case **fails rather than skips** — the opposite of the judge nightly, because green-because-it-could-not-run is §7's rung that cannot fire. Cost: a fork PR cannot run it | 2026-09-05 |
| Every run is **one `sre` agent, not the roster**. A planner dispatch hides the specialist's tool calls behind the delegating turn the trace carries, and every intent check reads that trace | 2026-09-05 |
| The gate measures the **mid tier**, not mast's frontier default: it gates a substrate claim, not a frontier-capability one, and the admitted roster does not spend the headroom that costs ~5× per pull request | 2026-09-05 |

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
