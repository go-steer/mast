# Sibling sync: mast ↔ core-agent

[`fork-design.md`](./fork-design.md) § "Sync discipline under (E)" says each repo carries this
doc, listing shared-infrastructure SHAs ported in either direction, SHAs explicitly *not* ported
with a one-line reason, and a security-fix correlation table. It was specified on 2026-07-26 and
went unwritten until 2026-08-17, when the first [`dev/upstream-drift`](../dev/upstream-drift)
report ([#153](https://github.com/go-steer/mast/issues/153)) produced something to triage.

This is that doc. It is **not** hand-maintained per commit — that is what went wrong the first
time. The machine keeps the *inventory*; a human keeps the *verdicts*.

---

## How the two halves work

**The inventory is generated.** Every ported file carries a
`// Originally derived from go-steer/core-agent@<SHA>[:<upstream-path>]` trailer, written at port
time by whoever did the port. `dev/upstream-drift` reads all 182 of them and asks, per file: which
upstream commits have touched this source path since the SHA it was frozen at?
`.github/workflows/upstream-drift.yml` runs it every Monday and rewrites one long-lived tracking
issue in place. It never fails a build — drift is the expected state here, and a red build nobody
can turn green trains people to ignore red builds.

**The verdicts are here.** A commit count is not a decision. This file records what each drifted
commit *means* for mast, using the vocabulary below.

| Verdict | Meaning |
|---|---|
| **absorbed** | The change's effect is present in mast — either the file was re-ported at a later SHA, or mast reached the same behavior independently. |
| **ahead** | mast already had it, or has a superset. |
| **n/a** | The code it changes has no counterpart in mast's lean scope. Nothing to port, ever. |
| **divergent** | mast made the opposite call deliberately, or the surface is mast-native. Nothing to port unless the decision is revisited. |
| **port** | Should come across. Tracked below with its state. |
| **watch** | No action now; revisit when a stated condition changes. |

### What the instrument does *not* tell you

Four limits worth stating, all found by running the first triage:

1. **A trailer is a port-batch baseline, not per-file provenance.** `pkg/attach`'s 80 files all say
   `@25d8531` because that is the batch they came over in. Any file subsequently updated against
   newer upstream keeps reading as drifted. This produced the largest single class of false
   positives in the 2026-08-17 triage — see [#149](https://github.com/go-steer/mast/pull/149) below.
   **Rule: a re-port bumps the trailer.** Otherwise the detector reports absorbed drift forever.
2. **One SHA per file cannot express "absorbed everything except X."** Where that happens, the
   trailer moves and the exception is recorded here. Two such exceptions exist today
   (`4ac5efb`, `5bc4393` on `pkg/taskclass/taskclass.go`).
3. **A commit's verdict is per-file, not per-commit.** `6012677` is a six-hardening security
   roundup whose `pkg/attach` half is absorbed and whose `pkg/permissions` half is not, purely
   because those two packages were ported two days apart.
4. **It measures files, not capabilities.** A feature mast implemented from scratch in a
   mast-native package is invisible to it; so is a feature upstream built that mast has no file
   for. Both showed up in this triage.

---

## Security-fix correlation

Security fixes land in both repos within 48 hours — the one rule in the sync table with no
exceptions.

| Upstream | Landed upstream | mast state | Notes |
|---|---|---|---|
| `6012677` attach security roundup, six hardenings (#385/#465) | 2026-07-27 | **partial → closed 2026-08-17** | Five of six were already in mast: `/whoami` reports only server-verified auth sources, `/events?since=0` replay is capped, the SSE boot-frame ordering race is fixed, redirect-hop header stripping and the `acceptEdits` blast-radius documentation all came over with the `pkg/attach` port at `25d8531` two days later. The sixth — dropping `fetch_url` from the plan-first exemption set — did not, because `pkg/permissions` was ported at `83ec071`, *before* the roundup. Closed by the change described under "What this triage changed". |

The 48-hour rule was met in spirit and missed in mechanism: mast got five sixths of a security
commit for free because of port timing, and missed the sixth for the same reason. Port timing is
not a security control. When a security fix lands upstream, check every ported package's trailer
against it explicitly rather than assuming the next batch will sweep it up.

---

## Triage of 2026-08-17

Against core-agent `origin/main` @ `ee3d6ec`. 48 upstream commits across 53 of mast's 182 ported
files, in 17 packages. Every commit below has a verdict; nothing is left unclassified.

### Absorbed or ahead — 20 commits

No action. Listed so the next triage doesn't re-derive them.

| Commit | Subject | Verdict |
|---|---|---|
| `b98803c` | anthropic: send tool parameters Claude can actually see (#754) | **ported 2026-08-17**, [#154](https://github.com/go-steer/mast/pull/154). See "What this triage changed". |
| `c07a4b4` | anthropic: normalize genai type enums in `input_schema` (#542) | absorbed — verified at code level. |
| `6676bf9` | gemini: enforce the builtins Gemini 3.0+ constraint (#546) | absorbed — verified at code level. |
| `1b5207c` | pricing: promote `internal/pricing` to `pkg/pricing` (#507) | absorbed at port; mast has never had an `internal/pricing`. |
| `59d27e5` | taskclass,modeltier: real gemini tier defaults (#545) | absorbed by [#149](https://github.com/go-steer/mast/pull/149). |
| `d2bd1ad` | gemini small tier → `gemini-3.5-flash-lite` (#561) | absorbed by #149. |
| `ffca0ee` | pricing,modeltier,usage: close the cross-table gaps (#569) | absorbed by #149. |
| `cebba9d` | teach every model table about `gemini-3.7-flash` (#752) | absorbed by #149. |
| `afea653` | derive the model tables from one rule instead of four lists (#774) | absorbed by #149 — which is where mast's `dev/regen-builtin-pricing` came from. |
| `f054590` | promote `gemini-3.7-flash` to the Gemini frontier default (#777) | **ahead** — post-dates the `cafe310` port SHA, and mast set that default anyway. |
| `09b6cd1` | drop the `gemini-3.5-pro` needle from the companion tables (#786) | **ahead** — post-dates `cafe310`; mast dropped it independently and says why in `pkg/taskclass/taskclass.go`. |
| `bdc4834` | usage: bill Anthropic cache-write tokens at their premium rate (#769) | absorbed by #149, which closed the same bug as mast [#121](https://github.com/go-steer/mast/issues/121). |
| `4d2e954` | anthropic: wire prompt caching on by default (#772) | absorbed — mast has marked the last content block ephemeral since the provider was written. |
| `254435e` | attach: operator-facing reset for a tripped watchdog / cost ceiling (#670) | **ahead.** mast's `GuardrailInfo`/`GuardrailResetRequest` are a superset: per-scope ceilings, token and turn top-ups, an `Advisory` flag, and an alert count upstream doesn't carry. |
| `9a75b2a` | attach: advertise resolved build version in agent card (#574) | absorbed — `AgentCard.Version` defaults to the ldflag-injected `internal/version.Version`. |
| `08b10f8` | attach: self-audit operator interrupts (#588) | absorbed — `InterruptSelfAuditor` is wired in `pkg/attach/handlers.go`. |
| `c986933` | attach: refuse message intake during shutdown drain (#567) | absorbed in shape. mast refuses through `turnTracker.isDraining()` at every intake — inject, A2A, AG-UI, scheduler, auto-resume. It does **not** send `Retry-After`; see "Deliberately not ported". |
| `daa78fc` | shutdown: bound every teardown wait (#548) | absorbed by a mast-native design — `defaultDrainBound`, `armTeardownWatchdog`, `storeWriteTimeout`, per-session turn locks. The MCP-child-reaping half is a **watch** item below. |
| `49c8415` | loaders: read `$HOME/.agents/` as a user-scope root (#352) | absorbed — `instruction.WithHomeAgentsRoot`. |
| `517b909` | loaders: `WithContentRoots` for instruction + skills (#608) | absorbed in shape — mast's `Load(projectRoot, userRoot, opts...)` plus the per-caller overlay covers the same need without the skills half. |

### Not applicable to the lean scope — 13 commits

These change code mast does not have and will not grow. `fork-design.md`'s "core-agent-specific
feature" row: not ported unless someone here explicitly asks.

| Commit(s) | Why not |
|---|---|
| `6a4119b` module path → `/v2` (#327) | mast is `github.com/go-steer/mast`, its own module at v0. Structural to core-agent only. |
| `36b4842` `core_agent.*` subsystem meters (#528) | **divergent.** mast has `pkg/observability` with `mast_*` metric names. Porting upstream's names would be a regression in a mast-native surface. |
| `f8e5256` subagent catalog (#634), `8d812e7` subagent persisted turns (#687), `db97d4d` report "sync" only where the sync tool exists (#743), `9dc8510` fold the background poller tools (#633), `6d5f671` declarative `subagents[]` (#603) | mast replaced skills and background subagents with **specialists** ([`specialists-design.md`](./specialists-design.md)). There is no background-subagent poller, no spawn/stop tool family, and no sync tool. |
| `4ac5efb` advisory plan mode (#684), `c0007a5` `record_plan` reports the gate it armed (#757), `80fb9a6` descriptions name only registered tools (#759), `bf3bbbf` gate search-shaped bash behind the native tools (#676), `5bc4393` investigation classes drop bash and require a plan (#677) | All five hinge on `pkg/tools`, which mast does not have — no `bash`, no `record_plan`, no `fetch_url`, no `--task` CLI surface. `4ac5efb` and `5bc4393` also touch `pkg/taskclass/taskclass.go`; those are the two recorded exceptions to that file's trailer bump. |
| `7672f25` auto-continue: resume MAX_TOKENS-truncated text turns (#585) | mast has no auto-continue. The `pkg/eventlog/service.go` half is a query helper with no consumer here. |

### Port — 7 commits

Ranked by what it costs mast to keep not doing them.

| Commit | Subject | State |
|---|---|---|
| `e7a21da` | eventlog: `BEGIN IMMEDIATE` on SQLite (#576) | **ported 2026-08-17** ([#157](https://github.com/go-steer/mast/pull/157)). See "What this triage changed" — this one is live in mast for a reason upstream's commit message doesn't mention. |
| `9f81626` | attach: durable peer registry across hub restarts (#688) | **open.** mast's `PeerRegistry` is in-memory: a daemon restart drops every registration and every peer has to re-register before federation works again. mast is the *unattended* sibling, so this bites harder here than upstream. |
| `c319565` | vertexcache: retry a failed `Caches.Create` on a bounded backoff (#723) | **ported 2026-08-17** ([#161](https://github.com/go-steer/mast/pull/161)) — the vertexcache half. Upstream squashed three unrelated changes into this SHA; the `pkg/attach` + `pkg/eventlog` `BranchLister` half is subagent-branch resolution, which mast does not have, and the `pkg/models/gemini` touch is a comment. So `c319565` keeps reporting under `pkg/eventlog`, correctly. |
| `ef9b9b5` | telemetry: Go runtime metrics + Gemini API-key otelhttp wrap (#525) | **open, low.** mast has neither. Runtime metrics are the more useful half for a long-lived daemon; the otelhttp wrap only covers the API-key Gemini path, which mast uses less than Vertex. |
| `b1101f9` | mcp: surface JSON-RPC error body on 4xx/5xx (#305) | **ported 2026-08-17, narrowed** ([#167](https://github.com/go-steer/mast/pull/167)). The triage called this a re-implementation; it turned out to be a re-implementation of *less than half*, because the MCP SDK closed most of the gap in the interval. See below. |
| `cfcbe22` | vertexcache: close the lost-retry race in the transient-cancel test (#547) | **verdict corrected 2026-08-17: absorbed.** mast's `TestInit_TransientCancelRetriesInsteadOfStickyFail` already polls `Init` rather than firing once, and carries the comment explaining why. The triage read the row from the drift report and not from the test. |
| `6a4810b` | vertexcache: widen async deadlines to a shared `testWait` (#517) | **verdict corrected 2026-08-17: absorbed but for one site, now closed** ([#161](https://github.com/go-steer/mast/pull/161)). mast had widened every `waitFor` deadline at port time but left one `time.After(time.Second)`; the port lifts all of them onto the shared `testWait` constant. |

### The watchdog cluster — 7 commits, governance question resolved

`635a9eb` enforce mode (#628) · `e42a511` route alerts into the model's next turn (#678) ·
`6510a65` halt in-turn, not at the turn boundary (#719) · `ef7dfb6` tool-failure-streak signal
(#690) · `317e18e` cycle detection + path-canonicalized args (#679) · `5682659` safe autonomous
defaults + `safety.watchdog` config (#665) · `4ac0337` persist guardrail trip-state across a
process restart (#671)

At triage time mast's watchdog was the pre-`635a9eb` shape: one signal (`repeated-tool-call`),
observe-only, alerts surfaced through `Tap`. `cmd/mast/guardrails.go` was explicit that
`watchdogModeWarn` is "the only watchdog posture mast ships", and the trip state lived in an
in-memory `watchdogPool` that a restart cleared.

**The governance question this raised.** `fork-design.md` § "Sync discipline under (E)" listed
**watchdog→model routing** as an example of a *lean-fork-specific feature* — mast-only, "not
ported to core-agent unless someone there explicitly asks for it." Upstream built it anyway, as
`e42a511`, on 2026-08-12. So one of the sync table's four categories had a member sitting on the
wrong side of the fork.

**Resolved 2026-08-17: port all seven and reclassify.** watchdog→model routing moves to *shared
infrastructure*; `fork-design.md`'s lean-fork-specific row gets a different example. The
reasoning is the one that makes mast mast: the unattended sibling is the deployment where a
runaway or an unverified conclusion costs the most and is noticed the least, so a soaked upstream
implementation is worth more here than a mast-original one. The classification was aspirational,
not load-bearing.

The port ships as five PRs rather than one, because each changes what an operator sees:

| PR | upstream | what it adds |
|---|---|---|
| A | `317e18e`, `ef7dfb6` | **ported 2026-08-17.** Cycle detection, path-canonicalized args, tool-failure-streak, and result observation at the bridge. |
| B | `635a9eb`, `6510a65` | **ported 2026-08-17.** Enforce mode + in-turn halt — flips the loop detectors to Critical. |
| C | `e42a511` | **ported 2026-08-17.** Alert→model routing (`Alert.Guidance`, feedback mode). The reclassification lands with this one. |
| D | `5682659` | **ported 2026-08-17, half of it.** The `safety.watchdog` bundle field, three-source precedence (`--watchdog` > bundle > default), a default posture, and a startup line naming which source won. Two divergences, below. The commit's other half — a `$10` session cost ceiling armed by default — is declined; see "Deliberately not ported". |
| E | `4ac0337` | **ported 2026-08-17, by a different route.** An `enforce` halt and its reset are written to a mast-owned table and folded forward, so a restart adopts the halt instead of clearing it. Stores the trip only; see the budget gap below. |

Two adaptations run through all five. Severity stays **Warn** for `tool-failure-streak` — under
an enforce posture a Critical alert would halt a daemon three denials into a legitimate RBAC
probe, making the backstop the outage. And nothing ships inert: `Alert.Guidance` arrives with PR
C, the posture that reads it, rather than with PR A as an unread field.

Alert prose is rewritten for mast's affordances. Upstream's reasons point an operator at a
`/interrupt` slash command and `--max-turn-cost-usd`; mast has neither — its interrupt is
`POST /sessions/{id}/interrupt` on the attach surface and its ceilings come from the workload
bundle. `pkg/watchdog/cycle_test.go` asserts on that directly, so the text cannot drift back.

**PR D diverges twice, deliberately.** *The default posture is `feedback`, not upstream's
`enforce`.* Upstream's premise is right — an unattended run with a warn-only watchdog has no
backstop, and warn on a deployment nobody is tailing is indistinguishable from off — but the
conclusion does not transfer. mast's `alternating-tool-cycle` detector has a workload-shaped
false positive: a scheduler-driven daemon watching a rollout settle calls the same tools with the
same arguments on purpose. Upstream's operator is at a terminal and clears a false halt in
seconds; mast's is asleep, and a false halt is an outage that waits for the morning. A false
`feedback` costs one paragraph the model may disregard. Recoverable beats unrecoverable when
nobody is watching, and a workload whose loop is bounded by construction declares
`safety.watchdog: enforce` and gets upstream's posture. Both halves of the divergence are
test-pinned (`TestResolveWatchdogDefaultIsFeedback`, `TestDefaultModeActsWithoutHalting`), because
it changes runtime behavior for every deployment that has never typed the flag.

*And the port covers a surface upstream's does not.* mast's library-embed path (`mast.RunWorkload`)
ran `r.Run` with no `watchdog.Tap` at all — the one mast surface with no runaway backstop of any
kind. It now reads the same `safety.watchdog` field and taps the same signals, with the rungs
bounded by what that surface holds: `enforce` abandons the runaway turn, but there is no
cross-call session state for the "refuse every later turn" half and no next turn for `feedback`
to inject into. This is mast's analog of the `ReproduceAgent` gap upstream closed in the same
commit: a real agent path that the posture plumbing had simply never been wired into.

**PR E takes a different route to the same guarantee.** Upstream writes both the trip and the
reset as rows in the ADK session's own event stream, folding the session's events forward on
restore. mast cannot: an out-of-band `Get`-then-`AppendEvent` while the runner holds the session
bumps `last_update_time` and trips ADK's optimistic-concurrency check — the write-lease
constraint that already forced attachadapter to defer its interrupt audit to between turns, and
the reason `cmd/mast/guardrails.go` had settled for a log line as its only reset audit. A reset
arrives from an operator mid-incident, which is exactly when a turn is running. Upstream solves
it with a pending queue drained from inside the agent's write lease; mast has no `Agent` to hang
that lease on, so it writes to `agent_guardrail_log`, a table it owns, on the connection
`pkg/eventlog` already holds for its overlay — the same connection `pkg/attach`'s
`SessionACLStore` shares. Different table, no session row touched: nothing to race, no queue to
drain, and the row can be written inline where the decision is made. It also closes a gap mast's
own comment had flagged — the reset audit is now a durable row naming the caller, not just a log
line.

Three constraints the mast version adds. It is wired **only under `--attach-listen`**, because
`POST /guardrails/reset` is attach-only and a persisted halt with no reachable reset is a brick
rather than a backstop (`--attach-listen` already requires `--session-db`, so the store exists
exactly when the reset does); enforce mode without an attach listener now warns about that at
startup. **Configuration still wins over history** — `Enforcer.Adopt` refuses unless the current
mode enforces, so a deployment dialed back to `feedback` does not inherit a halt only `enforce`
could have produced, which would otherwise make the posture change unreachable. And restore
**fails open, loudly**: an unreadable table logs and continues, because a storage fault must not
halt every session in the deployment with no trip behind it.

With PR E the cluster is closed, so `pkg/watchdog` moves to a single `6510a65` baseline —
every file, not just `watchdog.go` and `bridge.go`. The earlier note here named only those two;
that was under-specified. The detector maps upstream `pkg/agent/watchdog*.go` commits onto the
package, so the package reports zero only when no file trails the newest of them, and a mixed
set of baselines inside a package whose port is a re-implementation rather than a file-for-file
copy says less than one baseline does. The seven commits it was reporting are exactly the seven
PRs A–E ported; `6510a65` is the newest of them that touches `pkg/agent/watchdog.go`. That drops
the report from 40 commits / 49 files to **36 / 44**.

`cmd/mast` still reports `e42a511` afterwards, and that one is a mapping artifact rather than a
gap: PR C landed the routing in `main.go` and `oneshot.go`, which are mast-native and carry no
trailer, while the only trailered file in the package (`safety.go`) is derived from `5682659`
and should not claim a commit it did not come from.

### `b1101f9` — what a re-implementation looks like when the dependency moved too

Upstream wrote `pkg/mcp/errbody.go` on 2026-07-17 against go-sdk **v1.4.1**, where a non-2xx
response resolved to `http.StatusText(code)` and nothing else: a GKE MCP IAM denial reached the
operator as `sending "tools/call": Forbidden`, with the permission name they had to grant thrown
away. Their fix wraps the HTTP transport and extracts the message from any 4xx/5xx JSON body.

mast is on go-sdk **v1.7.0**, and the SDK has since taken half the job. `checkResponse` now
decodes the body and surfaces a standard `{"error":{"message":..}}` object itself. Porting
upstream's file verbatim would have duplicated that — and duplicating it is not free, because
intercepting at the transport discards the typed `*jsonrpc.Error` the SDK wraps into the chain.

So the port covers only what v1.7.0 still drops, both of which mast hits on its primary surface:

- **The MCP tool-result error shape.** `container.googleapis.com/mcp` answers an IAM denial with
  403 and a body of `{"result":{"isError":true,"content":[{"type":"text","text":"Permission
  '...' denied"}]}}`. That decodes as a JSON-RPC *result*, not an error, so the SDK falls through
  to the status line. This is the exact case upstream's commit message quotes — and the one the
  SDK's own fix does not reach.
- **Transient statuses.** For 429/500/502/503/504 the SDK returns early with the status text and
  never reads the body, so a quota denial names no quota metric.

Three exclusions the mast version adds, all because the SDK it composes with is newer than the
one upstream wrote against. It only inspects **POST**, leaving the SSE reconnect (GET) and the
teardown (DELETE) alone — failing a GET from the transport would spend the SDK's reconnect budget
on a status it handles directly. It skips **404**, which the SDK translates to `ErrSessionMissing`
so it can skip a redundant DELETE; that sentinel is worth more than the body. And it skips the
standard error object on a non-transient status, as above.

The interesting part is not the code, it is that **the verdict "port this" had a shelf life**.
The triage row was written from upstream's commit message, which described a defect that was
two-thirds fixed elsewhere by the time anyone acted on it. What made the difference was running
the real client against a canned server and reading what it actually printed, before writing a
line of the port. `TestSDKStillDropsTheseBodies` keeps that honest permanently: each case drives
the exchange twice, once through the bare SDK and once through mast's wrap, and asserts the text
is absent from the first and present in the second. A version bump that closes one of the
remaining holes fails the test with "the SDK now surfaces this body itself — drop mast's branch
for it", which is the only way a compatibility shim ever gets deleted.

`pkg/mcp` moves to a single `b1101f9` baseline. `auth.go` carried `c5efbb9`, and `b1101f9`'s
only change to `lifecycle.go` is the eight-line transport-wrap hunk — which mast now has, in
`newHTTPToolset` rather than `transportFor`. The report drops from 36 commits to **35**; the
three left on `pkg/mcp` are `daa78fc`, `49c8415`, and `6a4119b`, all triaged elsewhere on this
page.

### Deliberately not ported

- **`Retry-After` on drain refusal (`c986933`).** mast refuses intake during drain but sends no
  hint about when to retry, because mast's drain window is a function of the workload's turn
  budget (`drainBound`) rather than a fixed timeout — a `Retry-After` computed from it would be a
  guess dressed as a contract. Revisit if a caller ever needs to distinguish "draining" from
  "gone".
- **The `$10` unattended session cost ceiling (the second half of `5682659`).** Upstream arms a
  default per-session dollar cap whenever a run is unattended, discriminating on TTY / `-p` /
  `--no-repl`. Four reasons it does not port. The discriminator is a *constant* in mast — every
  run is unattended, so the rule degenerates to "always", which is a different decision than the
  one upstream made. The session unit differs: a `single_session` daemon holds one session for its
  entire life, so a fixed dollar cap on a session's lifetime is not a runaway guard, it is a
  scheduled outage for a legitimate deployment shape. The opt-out would be a breaking change —
  distinguishing `max_cost_usd: 0` from unset means pointer-ifying `workload.Budget.MaxCostUSD`, a
  public field on a package `docs/library-api-design.md` calls stable as of v0.2, for two non-test
  read sites' worth of benefit. And mast already composes three ceilings plus per-specialist
  scopes plus a guardrails grant endpoint, so the gap upstream is filling is one mast filled from
  the bundle instead. A `no_cost_ceiling: bool` escape hatch and an "arm only when the bundle
  declares no `budget:` block at all" rule were both considered; both trade a legible default for
  an inferred one. Revisit if a shipped workload ever runs up a bill nobody's ceiling caught.
- **MCP child reaping (the second half of `daa78fc`).** mast launches stdio MCP servers through
  `pkg/mcp/catalog.go` and its teardown is mast-native; whether an orphaned child can survive a
  `mast` exit is **unverified**. Marked watch rather than n/a for that reason — it is a claim
  nobody has tested, not a decision anybody made.

### Found while porting, not fixed here

- **Budget spend does not survive a restart.** `newMeterPool` mints every session's
  `budget.Meter` at zero, so a daemon restart hands each session its full ceiling back: a
  workload stopped by `max_cost_usd: 5.00` after $5.02 resumes with $5.00 available, and a crash
  loop can spend the cap once per restart indefinitely. PR E makes the *watchdog* halt durable and
  deliberately stops there. The two halves are not the same size. A trip is one latched bit that
  can be written when it happens and read once per process; spend is an accumulator that has to
  be reconstructed to the cent, which means either folding the session's priced events back
  through a fresh meter on first touch — correct, but it re-prices a whole transcript per session
  and inherits every model-attribution and unpriced-event edge case — or persisting the
  accumulator itself and reconciling it against a transcript that may have advanced past it.
  Either is its own PR with its own correctness argument, and bolting it onto a trip-latch port
  would bury it. Recorded as a gap so nobody reads "guardrails are durable" as covering both.
  PR E does persist the *grants* an operator hands over on a reset, for the audit trail, but does
  not replay them: raising a ceiling over an accumulator that has forgotten what it spent is
  arithmetic on a number that no longer means anything.

---

## What this triage changed

Everything below landed as a direct result, as a chain of PRs running from
[#154](https://github.com/go-steer/mast/pull/154) onward. The roster grows as the port backlog
closes, so read the links rather than a count — an earlier revision of this line carried a tally
that three PRs had already outrun.

**[#154](https://github.com/go-steer/mast/pull/154) — the Anthropic tool-parameter bug.** Triaging
`b98803c` turned up the same defect live in mast: every tool mast defines reached Claude as
`{"type":"object","properties":{}}`. ADK v2's `functiontool.New` derives its schema into
`ParametersJsonSchema` and leaves the typed `Parameters` field nil; `toolsParam` handled only
`Parameters` and fell through to the canonical no-arguments shape for everything else. The
planner's dispatch tools, `pause_session`, and every MCP tool were affected. It survived two green
judged nightlies and a live GKE run because ADK's *internal* declarations — `finish_task` among
them — do use the typed field, so the tool the eval harness asserts on converted correctly while
the tools the workload dispatches went out blind. Nothing errors: Anthropic accepts an empty input
schema and validates nothing against it.

This is the strongest argument for the detector that exists. The bug was in mast's own code, in a
package with tests, on a path exercised by every Anthropic run — and it took an upstream commit
title to find it.

**This PR — four hygiene and correctness items:**

- **`fetch_url` dropped from `planExemptTools`**, closing the last sixth of security roundup
  `6012677`. Inert in mast today (there is no `fetch_url` tool), which is exactly why it survived:
  a dead entry in a live table. It is removed rather than left as harmless, because the next person
  to read that table should not have to work out which entries are real.
- **The stale port-status note in `pkg/permissions/denylist.go` corrected.** It said the package is
  "compiled, tested, NOT wired into the mast runtime" and that "nothing in mast is protected by
  these checks". Both have been false since the write gate landed —
  `internal/compose/writegate.go` and `pkg/approval` call it. It also said of the `fetch_url` and
  `acceptEdits` concerns "track upstream's resolution and adapt at wiring time; do not wire as-is",
  which upstream resolved on 2026-07-27.
- **Ten trailers bumped `83ec071` → `cafe310`** on the files [#149](https://github.com/go-steer/mast/pull/149)
  re-ported: `pkg/pricing/{catalog,file,pricing,pricing_test,refresh,refresh_test}.go`,
  `pkg/modeltier/{modeltier,modeltier_test}.go`, `pkg/taskclass/{taskclass,taskclass_test}.go`.
  (`pkg/pricing/builtin.go` carries no trailer — it is generated by `dev/regen-builtin-pricing`.)
  Without this the detector reports already-absorbed commits every Monday forever. Recorded
  exceptions: `4ac5efb` and `5bc4393` touch `pkg/taskclass/taskclass.go` and are **not** absorbed —
  they are `pkg/tools`-shaped and n/a per above.
- **The "tens of KB" comment in `pkg/pricing/refresh.go` corrected.** It is the stated
  justification for mast's 8 MiB response cap; upstream measures the LiteLLM catalog at 2–3 MiB and
  caps at 32 MiB. The cap is still fine — the reason given for it was not.

**[#157](https://github.com/go-steer/mast/pull/157) — `BEGIN IMMEDIATE` on SQLite (`e7a21da`).**
Ported unchanged; the reasoning is not upstream's. core-agent found this through auto-continue,
which mast does not have. Here the second writer is the daemon's own ingress — the scheduler
firing a cadence, auto-resume replaying a marked session, an A2A or AG-UI submission, an attach
inject — landing on ADK's connection pool while the overlay pool writes its own rows.
`pkg/eventlog/service.go`'s write mutex reads like it already covers this and does not: it
serializes writes that go *through the wrapper*, not writes another connection makes on the same
file. Under the default deferred `BEGIN`, an `AppendEvent` that reads before it writes fails
*immediately* with `SQLITE_BUSY` rather than waiting out `busy_timeout`, because SQLite refuses to
retry a snapshot→write upgrade. The regression test holds the write lock on an independent
connection and fails on pre-fix code with `database is locked (5) (SQLITE_BUSY)`.

`pkg/eventlog/sql.go`'s trailer moves `25d8531` → `e7a21da` per the re-port rule. That absorbs
exactly this commit; `8d812e7` and `c319565` touch the same file and post-date it, so they keep
reporting, correctly.

**[#161](https://github.com/go-steer/mast/pull/161) — the vertexcache cluster, and two corrected
verdicts.** `c319565`'s vertexcache half is ported: a non-context `Caches.Create` failure now gets
15s / 30s / 1m / 2m / 4m before the manager goes sticky-failed, instead of going sticky-failed on
the first one. The failure it fixes is one mast is *more* exposed to than upstream, not less — an
unattended daemon starts when its controller schedules it, not when its Workload Identity binding
lands, and nobody is watching the first turn. Retries are demand-driven off `Init`, which the
gemini wrapper already calls on every non-cached model call, so an idle daemon issues no RPCs.

Porting it corrected two verdicts this triage got wrong, both by reading the drift report instead
of the code: `cfcbe22` is already absorbed, and `6a4810b` was absorbed but for a single
`time.After(time.Second)` that the port-time widening missed. Both rows above now say so. The
lesson is the same one the eventlog port taught in the other direction — a "low" verdict on a test
commit is still a claim about mast's code, and the only way to check it is to open the test.

Both vertexcache trailers move `b8dd225` → `c319565`, which is the whole set: those three commits
are every upstream change to the package since `b8dd225`.

**PR A of the watchdog cluster — `317e18e` + `ef7dfb6`.** mast's watchdog grows from one detector
to three. `alternating-tool-cycle` catches the shape the consecutive-repeat check is structurally
blind to — the `list_agents → check_agent` loop that survived an operator stop during upstream's
GKE UAT, where no call is ever followed by itself. Path canonicalization closes the other half:
`main.go`, `./main.go`, and `/workspace/main.go` now compare equal, so a repeat cannot hide behind
a spelling. `tool-failure-streak` is the one that matters most here — it reads tool *outcomes*
rather than calls, and fires when three in a row all error with none succeeding between, which is
the situation where an unattended workload writes a confident report about a system nothing it ran
could reach. mast is the deployment where that costs the most and is checked the least.

Result observation arrives as an optional interface (`ToolResultObserver`) rather than a widening
of `Watchdog`, which is documented as a plug-in point, and `Tap` feeds responses through the same
per-turn dedup set as calls under a separate key prefix — the streaming aggregator re-emits both,
and a double-counted failure would trip the streak at half its threshold.

All four wiring gates were checked against pre-port behavior: with the two new signals removed
from `NewDefaultWatchdog` and `matches` reverted to a literal args compare, every acceptance test
fails with "alerts = []". The detectors are reachable from the shipped default, not merely
constructible.

**PR B of the watchdog cluster — `635a9eb` + `6510a65`.** `--watchdog=enforce` exists, and the
default stays `warn`. The two commits are one shipment because `635a9eb` alone is close to
useless in mast: mast's runaway shape is a loop *inside* a single turn — model calls a tool, the
flow runs it and calls the model again, all within one `Run` — and a turn-boundary drain neither
fires while that happens nor at all if the turn never ends. `6510a65` is the fix upstream shipped
after hitting exactly that, and it also closes a real gap in mast's *warn* mode: before it, a
looping session logged nothing until it stopped looping.

Three adaptations. **Where the state lives:** upstream hangs enforce state off its `Agent` struct
(`WithWatchdogEnforce`, `preflightWatchdog`, `ResetWatchdog`); mast has no `Agent`, so it is a
`watchdog.Enforcer` the daemon holds per session in `watchdogPool`, and the halt itself is the
caller's existing `cancel()` — the same handle a budget trip and an operator abort already use.
The package decides *whether* a session is halted and why; it never decides how a turn dies.
**Where the refusal sits:** at the `runTurnPre` chokepoint, after the abort/gate-pause checks, so
auto-resume, a scheduled fire, and an attach inject are all refused by construction rather than
by each caller remembering to ask. **Who names the remedy:** `NewEnforcer` takes the remedy
sentence, because the daemon can name the session's own reset endpoint and a one-shot has none to
name — the same posture applies to one-shot mode, where the halt just ends the turn.

Severity moves with this PR, not against it: `repeated-tool-call` and `alternating-tool-cycle`
become Critical, `tool-failure-streak` stays Warn per the adaptation above. Severity is a property
of the pattern, so it is asserted in `pkg/watchdog` tests independently of any posture.

Both arms were checked against pre-fix behavior. Neutering the in-turn drain (`if false &&
observed`) makes the drain tests fail on timing rather than on outcome — the alert arrives at
event 20 instead of event 5, and the halt test consumes 500 events instead of ~5. Neutering the
chokepoint preflight alone still fails `TestWatchdogEnforceRefusesEverySubsequentTurn`, because
the refusal then comes from the post-loop check as a plain error rather than as `ErrConflict` —
the session is stopped, but a caller cannot tell a halted session from a failed turn.

**PR C of the watchdog cluster — `e42a511`.** `--watchdog=feedback`: the observation reaches the
model. Every posture before this one routed a runaway-loop alert to an operator, and on an
unattended workload — the deployment mast exists for — that operator is a pod log nobody is
tailing. The model about to make the same call for the sixth time is the only party that can
decide not to, and it was the one party never told. This is also the commit the governance
question was about; the reclassification is recorded in `fork-design.md` and in the
resolved-decisions table.

The postures become a ladder — `warn` < `feedback` < `enforce`, each including the one below —
and **`enforce` implies `feedback`** rather than replacing it. An enforce halt is cleared by an
operator reset, and a reset resumes a model whose context still ends in the loop it was halted
for; without the injected observation the next turn re-issues the same call and re-trips. That is
also why `watchdogPool.reset` clears the signals, the trip, and the alert residue but deliberately
**keeps** the queued observation: the reset undoes the halt, not the correction.

Four adaptations. **Where the queue lives:** upstream keeps pending alerts on `Agent` and prepends
inside `Run`; mast has no `Agent`, so it is a `watchdog.Feedback` held per session in
`watchdogPool`, symmetric with the `Enforcer` from PR B. **Where the injection happens:** at
`runTurnPre`, before `r.Run`, building a *new* `*genai.Content` with a leading text part rather
than appending to the caller's slice — `msg` belongs to the inject handler or the scheduler, and
growing their slice would leak the block into a retry of the same message. **Three signals, three
guidances:** upstream had one signal to write a model-facing sentence for; mast has three, and its
operator-facing `Reason` strings name `POST /sessions/{id}/interrupt` and the bundle's budget
ceiling — affordances the model does not have, and naming them invites a hallucinated call for
them. `TestBuiltinSignalsCarryModelFacingGuidance` asserts every shipped signal sets `Guidance`
and that none of them leaks an operator control, so a fourth signal cannot ship with only half the
prose. **One-shot says no:** the feedback rung is the one that does not carry over to one-shot
mode, whose whole mechanism is the next turn a one-shot does not have. It logs a line saying so
instead of quietly running warn behind an operator who asked for more.

The bound is upstream's: four pending alerts, oldest dropped, and nothing queued at all below
`feedback` so that flipping a long-running deployment up a rung cannot deliver a backlog about
turns that ended hours ago. Draining happens on read, not on turn success — an observation lands
exactly once even if that turn fails, because by the time a retry lands the signal describes
behavior several turns back, and a block that re-appears until something succeeds is a prompt
leak. The block is steering, not a trust boundary; nothing downstream grants authority based on
it.

Three arms checked against pre-fix behavior. Neutering the prepend fails the reach, once-only, and
post-reset tests with "no watchdog block". Narrowing `Mode.Feeds()` to feedback-only fails
`TestWatchdogResetKeepsTheQueuedObservation` at the queue assertion — enforce stops implying
feedback, and the treadmill is back. Making `reset` delete the queue fails the same test one line
later, which is the assertion that the reset does not undo the correction.

**[#167](https://github.com/go-steer/mast/pull/167) — the MCP error body (`b1101f9`), two-thirds
of which the SDK had already fixed.** An IAM denial from the GKE MCP server reached the operator
as `Forbidden`, with the permission name they needed dropped. mast now extracts the server's own
text — but only for the two shapes go-sdk v1.7.0 still discards, because the SDK closed the rest
of the gap in the month between upstream's commit and this port. The reasoning, the three
exclusions, and the test that will tell us when the remaining branches can be deleted are in
"`b1101f9` — what a re-implementation looks like when the dependency moved too", above. The
general lesson is worth stating on its own: **a triage verdict is a claim about two codebases and
everything between them, and the dependency counts.** This one was written from upstream's commit
message and would have shipped a duplicate of the SDK's own fix if the first step had not been to
run the real client and read what it printed.

---

## Observations that are not drift

Two things this triage surfaced that the detector cannot see, recorded so they are not rediscovered:

- **`taskclass.Profile` has four fields nothing in mast reads** — `CompactionThreshold`,
  `AgenticToolsEnabled`, `UseAgenticSmallModel`, `AskMode`. mast consumes `Tier` (via
  `ModelForTier`) plus the package-level `AgentMode`/`PlannerEnabled`/`Instruction`. Declarative
  fields nothing enforces are the failure class `docs/spike-findings.md` and the write-gate work
  both keep running into; these are inert rather than wrong, but they read as settings.
- **`pkg/permissions`'s exemption table names a tool universe mast doesn't have** — `read_file`,
  `bash`, `glob`, the `skill` namespace, the `spawn_agent` family — and its comments point at
  `pkg/tools/gate.go` and `pkg/skills/load.go`, neither of which exists here. Correcting the
  comments is in this PR; deciding whether the table itself should be pruned to mast's actual tool
  surface is a bigger question, deferred.

Added 2026-08-19, from a different direction — not this triage, and not a commit:

- **Gemini on Vertex had no provider alias** ([#186](https://github.com/go-steer/mast/issues/186),
  closed by [#187](https://github.com/go-steer/mast/pull/187)). core-agent registers `vertex` as a
  first-class provider (`pkg/models/gemini.NewVertex`, `config.ProviderVertex`) that sets the
  backend on the client config, and reads `GOOGLE_GENAI_USE_VERTEXAI` only to *guess* one when
  nothing is configured. mast kept the guess and dropped the way to be explicit: `BuildModel`
  passed an empty `genai.ClientConfig{}`, so the env var was the only route to Vertex — even
  though `pkg/taskclass.Providers()` has listed a `vertex` family since the port. mast now has the
  alias, named the same as core-agent's.

  **The general point is about the instrument.** `dev/upstream-drift` measures commits landing
  upstream *after* mast's port SHA. A capability core-agent already had at fork time, that mast's
  pruning dropped, produces no commit and therefore no row — it is invisible to the detector by
  construction, and will stay invisible however many Mondays pass. This one surfaced from a
  reader's question about a README example, which is not a sync process. The "what the instrument
  does not tell you" section above lists the known blind spots; **fork-time omissions belong on
  that list**, and the only instrument for them is reading core-agent's surface against mast's
  when touching a subsystem.

---

## Baseline after this triage

The report goes **48 → 40** commits across **53 → 48** files. (The file count *rose* by three as
PRs B and C added trailered files to a package that still reports drift — a port that adds files
to a behind-baseline package widens the denominator without widening the backlog. The commit count
is the signal.) `pkg/pricing` drops to zero;
`pkg/modeltier` to 2 and `pkg/taskclass` to 1, all of which are the `f054590` / `09b6cd1`
**ahead** rows — upstream commits that post-date the `cafe310` port SHA and whose content mast
already has. Those three will keep reporting until mast next re-ports from a SHA at or after them,
which is correct: the detector cannot know mast got there first, and inventing a trailer SHA mast
never ported from to silence it would be a lie in the one record this whole scheme rests on.
`pkg/eventlog` drops from 4 commits to 3 and now reports **two** port SHAs, because `sql.go` alone
moved to `e7a21da` — the per-file trailer is what the detector reads, so a package can sit at
several baselines at once and the aggregate row says so. `pkg/providers/vertexcache` drops to
zero: the package is fully current with upstream as of `c319565`.

The watchdog cluster did not move the count until it closed, on purpose. PRs A through D ported
`317e18e`, `ef7dfb6`, `635a9eb`, `6510a65`, `e42a511`, and `5682659` into new files carrying those
SHAs as their own baselines, while every file the detector maps those commits onto stayed at its
pre-cluster trailer. A partial port does not bump a baseline; bumping mid-cluster would have
silenced the commits still outstanding, which is precisely the lie the trailer scheme exists to
prevent. **PR E closed it**, and `pkg/watchdog` went to zero in one bump to `6510a65` — taking the
report from 40 commits / 49 files to **36 / 44**.

`b1101f9` came off next, moving `pkg/mcp` to a single `b1101f9` baseline and the report to **35 /
44** across 198 ported files. The file count rose without the drift-file count moving, which is
the shape a clean addition makes: `errbody.go` is new, current, and reports nothing.

35 is therefore the expected floor, not a backlog. The 13 n/a commits never go away either — they
are upstream commits on files mast owns a diverged copy of, and they will still be listed next
Monday. **Read the count as a delta, not a level.**

## Next triage

The weekly report regenerates [#153](https://github.com/go-steer/mast/issues/153) in place. Triage
again when the counts move materially, or when a security commit appears upstream — whichever comes
first. Start from the "port" and "watch" rows above rather than from a fresh count; the absorbed and
n/a verdicts do not need re-deriving unless the trailer they hang on changed.
