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
| `9f81626` | attach: durable peer registry across hub restarts (#688) | **ported 2026-08-19, library half only** ([#180](https://github.com/go-steer/mast/issues/180)). `NewPeerRegistryWithState` and the snapshot/reload path landed as upstream wrote them. Upstream's `cmd/`+`pkg/config` half did not: mast's daemon has no `--attach-peer-hub`, so `Options.PeerRegistry` is an embedder-only surface here and there is no flag for a state-file flag to hang off. Adding one is a feature, not a port. One deliberate divergence: mast re-clamps every reloaded lease to the *running* max TTL — see below. |
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

### `9f81626` — a durable grant is still a grant

Upstream's durable peer registry ported almost verbatim: the same separate on-disk record type
(so `Peer.Owner`, which is `json:"-"` on the wire, still reaches disk and the #384 redaction
survives a restart), the same temp+rename snapshot, the same edge-triggered failure logging. Two
things came out differently.

**The `cmd/` half has nothing to attach to.** Upstream added `--attach-peer-state-file` next to
its existing `--attach-peer-hub`, plus the `pkg/config` and `pkg/compose` fields that flag needs.
mast has no `--attach-peer-hub`: `attach.Options.PeerRegistry` is reachable only from an
embedding program, and `cmd/mast` never sets it. Porting the flag would mean inventing the hub
flag first, which is a feature with its own design question (does an unattended daemon default to
being a hub?), not a port. The library half is where the defect #180 describes actually lives for
mast today, so that is what landed.

**A reloaded lease is re-clamped to the running config's ceiling.** #180 asked whoever took it to
check "whether a registration that outlives the process should also outlive a config change that
would no longer admit that peer," and the answer is no, for the reason [#166] gave about budget
grants: replaying a grant against a configuration that no longer supports it is arithmetic on a
number that stopped meaning anything. A lease is a grant — it says a peer counts as live until
*T* without needing to say anything further — and upstream reloads the recorded expiry as
written. Lower `WithMaxTTL` from five minutes to thirty seconds and restart, and the old grant
gets honored twice: once because its expiry is still four minutes out, and then indefinitely,
because `Heartbeat` re-derives the TTL from `LeaseExpiresAt - LastHeartbeat` and renews at the
ceiling the operator just deleted. mast clamps on load, before the expired-lease drop, so a peer
that is only still live under the withdrawn ceiling is not live at all; and the eager first
snapshot writes the narrowed lease down, so the clamp survives its own process.

The narrowing direction gets no such treatment and needs none. An owner is a restriction, so
carrying one forward can only ever restrict — that is why it is replayed as written while the
lease is not.

This is a candidate to send upstream: the same code has the same behavior there, and their
operator is no more likely to want a withdrawn ceiling honored than mast's.

[#166]: https://github.com/go-steer/mast/issues/166

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

- **Budget spend does not survive a restart.** *Closed by [#175](https://github.com/go-steer/mast/issues/175);
  kept here because the reasoning it forced is worth reading next to the gap that produced it.*
  `newMeterPool` minted every session's `budget.Meter` at zero, so a daemon restart handed each
  session its full ceiling back: a workload stopped by `max_cost_usd: 5.00` after $5.02 resumed
  with $5.00 available, and a crash loop could spend the cap once per restart indefinitely. PR E
  made the *watchdog* halt durable and deliberately stopped there. The two halves are not the
  same size. A trip is one latched bit that can be written when it happens and read once per
  process; spend is an accumulator that has to be reconstructed to the cent, which means either
  folding the session's priced events back through a fresh meter on first touch — correct, but it
  re-prices a whole transcript per session and inherits every model-attribution and
  unpriced-event edge case — or persisting the accumulator itself and reconciling it against a
  transcript that may have advanced past it. Either is its own PR with its own correctness
  argument, and bolting it onto a trip-latch port would bury it.

  **How it was settled.** Neither, in the end: mast keeps an append-only ledger
  (`eventlog.SpendStore`, one row per priced call in `agent_budget_spend`), written through a
  `budget.Config.OnSpend` hook and folded back by `budget.Meter.Restore` on a session's first
  touch after a restart. Replay lost on a substrate fact rather than on taste — ADK's database
  session service persists `UsageMetadata` and `Author` but has **no column for `ModelVersion`**
  (`session/database/storage_session.go`, adk/v2 v2.2.0), so every replayed call would miss
  `pkg/pricing`'s catalog and fall back to the flat per-1K rate that `pkg/budget` measures at
  5.9x error on a cache-warm session; a restored ceiling that wrong is worse than none, because
  it looks right. Replay also prices history at today's weekly-regenerated rates, retroactively
  rewriting money that already left the account. Checkpointing lost to the reconciliation problem
  named above; a ledger written at the same granularity the accumulator moves has nothing to
  reconcile, and its only loss window is one model call, always an undercount.

  **And the grants question inverted.** PR E persisted the *grants* an operator hands over on a
  reset for the audit trail but did not replay them, because raising a ceiling over an
  accumulator that had forgotten what it spent is arithmetic on a number that no longer means
  anything. With the spend durable, *not* replaying them became the bug: a session an operator
  rescued at $5.02/$5.00 would come back with the spend and without the rescue. They are replayed
  now, which required recording *which scope* each grant targeted (`GuardrailRecord.GrantScope`,
  nil for a pre-#175 row) — replaying a specialist's grant onto the session would raise a cap by
  an amount nobody granted it, and a legacy row cannot be attributed either way, so it counts
  toward the audit total and nothing else.

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

`9f81626` came off on 2026-08-19, taking the report from **42 commits / 50 files to 41 / 49** across
200 ported files (35 was the floor at triage; upstream has landed commits since, which is what a
delta-not-level count looks like from the other direction). `pkg/attach` drops from 21 commits to 20
and now reports **two** port SHAs, `25d8531` and `9f81626`, for the same reason `pkg/eventlog` does:
only `peers.go` was re-ported, so only `peers.go`'s trailer moved. The two new files add to the
ported-file total without adding to the drift-file total — the shape a clean addition makes.

35 is therefore the expected floor, not a backlog. The 13 n/a commits never go away either — they
are upstream commits on files mast owns a diverged copy of, and they will still be listed next
Monday. **Read the count as a delta, not a level.**

## Triage of 2026-08-20

The report reads **54 commits / 70 of 200 ported files** against core-agent `3de4134`. **19 SHAs
are new since 2026-08-17** and had no verdict here; upstream shipped all nineteen in two days, most
of them on the attach surface. Every one is verdicted below. Two have already come off.

The verdicts are not all the same strength, and the section says which is which. A verdict that
rests on reading mast's code is stated as a finding; one that rests on upstream's commit message
alone is stated as a candidate with the question that would settle it. **The instrument cannot tell
these apart and neither can a reader who only has the count** — that is what this ledger is for.

### Ported — 2 commits

| SHA | Upstream | mast |
|---|---|---|
| `04e54a3` | classify a cancelled turn as `canceled`, not retryable transient net (#817) | **Ported 2026-08-20**, [#206](https://github.com/go-steer/mast/issues/206) / PR #207. Ported for mast's own reasons: upstream's motivating producer is a TUI's ESC key, mast's is `--watchdog=enforce`. Protocol 1.4.0 → 1.5.0 |
| `6d30f9b` | label guardrail-refused turns instead of recording `error.type` unknown (#822) | **Ported by analogue 2026-08-20**, [#208](https://github.com/go-steer/mast/issues/208) / PR #209. Same defect, different fix shape — see below. Protocol 1.5.0 → 1.6.0 |

`6d30f9b` is worth recording as a *shape* divergence rather than a clean port. Upstream's
`SelfClassifyingError` returns a full classified error because its raisers already import the
attach package. mast's do not: `pkg/watchdog` is stdlib-only, and dragging `auth`, `eventlog` and
`permissions` into a leaf guardrail package to name one constant is the coupling
`pkg/attach/errors.go` already refused in the other direction for `pkg/budget`
([#135](https://github.com/go-steer/mast/issues/135)). mast's interface therefore carries the kind
string and nothing else, with the wire text owned by `pkg/attach` and the constant pinned from
`pkg/watchdog`'s own tests. **A future port of upstream's file will not apply cleanly here, and
should not be made to.**

### Absorbed or ahead — 3 commits

| SHA | Upstream | Why mast needs nothing |
|---|---|---|
| `1695fc9` | attribute approvals to the caller the daemon verified (#832) | **Convergent, same day.** mast shipped this as [#194](https://github.com/go-steer/mast/issues/194) (a durable approval names the authenticated caller, not `"operator resume --token"`) and [#198](https://github.com/go-steer/mast/issues/198) (wiring an `auth.Authenticator` into the inject listener so the name is not always `shared-bearer-token`). Independent arrivals at the same conclusion — worth noting because it is the second time this month |
| `6e609f3` | attribute each turn to the model that served it (#829) | **mast is ahead.** `pkg/providers/anthropic/llm.go:153` already stamps `LLMResponse.ModelVersion`, preferring the server's echo over the requested ID, with the same reasoning upstream wrote down. One residual, below |
| `b087eb6` | report MCP and skill tools in `/tools` with real attribution (#827) | **Convergent, opposite half.** mast's [#137](https://github.com/go-steer/mast/issues/137) (PR #205) fixed the *builtin* half of the same catalog on the same day; upstream fixed the MCP/skill half. One residual, below |

Two residuals fell out of reading those, and neither is covered by the commit that surfaced it:

- **`6e609f3` residual — a Vertex resource-path echo would go unpriced.** Upstream additionally
  rejects an echo shaped like `projects/…/models/claude-opus-4-5`, because it resolves to nothing
  in a pricing catalog and prices the turn at $0. mast ships `anthropic-vertex`
  (`pkg/providers/anthropic/anthropic.go:51`) and takes `final.Model` verbatim unless it is empty.
  `pricing.Catalog.LookupWithSource` prefix-matches *from the start of the string*
  (`strings.HasPrefix(low, k)`), so the documented `claude-opus-4-5@20251101` shape resolves and a
  resource path cannot. **mast's exposure is bounded and mast's failure mode is the better one**:
  `Meter.priceOf` falls back to the flat per-1k rate and increments `unpriced` rather than billing
  zero, so the budget still moves and the degradation is counted. Still worth the two-line guard.
  Filed as [#210](https://github.com/go-steer/mast/issues/210).
- **`b087eb6` residual — `ToolSourceSkill` is declared and never produced.** `pkg/attach/state.go:42`
  defines the constant; nothing in the repo emits it. Either mast's skills reach the catalog under
  another source or they do not reach it at all, and an unexercised declarative constant is not a
  feature ([adversarial-review lesson](./README.md)). Filed as
  [#211](https://github.com/go-steer/mast/issues/211).

### Confirmed analogues — port candidates with the finding, not the guess

Each of these was checked against mast's code, and the gap is real.

| SHA | Upstream | The mast finding |
|---|---|---|
| `6d1afd1` **(closed — [#216](https://github.com/go-steer/mast/issues/216), 2026-08-20)** | let a session's ACL actually be set (#797) (#831) | mast has the ACL — `auth.SessionACL` with Owner / Viewers / Contributors, enforced by `auth.Authorize`, persisted through `SessionACLStore`, and the owner is filled from the authenticated caller at registration since #194. What it has no door for is *amendment*: `auth.ActionSessionAdmin` documents itself as covering "ACL / metadata mutations on the session" and is wired to exactly one route, `DELETE /sessions/{id}`. **Less severe than upstream's** (mast's ACL is enforced, not inert) — but a viewer cannot be added to a running session |
| `453e3f0` **(closed — [#218](https://github.com/go-steer/mast/issues/218), 2026-08-21)** | report each subagent's configured tool grant on `/subagents` (#828) | `attach.SubagentCatalogInfo` carries Name, Description, Model, Root and Modes, and no tool grant. The operator question "what is this specialist allowed to touch" has no answer on the surface built to answer questions about specialists |
| `a58fdcc` **(closed — [#215](https://github.com/go-steer/mast/pull/215), 2026-08-20; the link half is n/a — mast dispatches injects synchronously, so no fan-in to link)** | root each turn in its own span and link the injects it answers (#807) | mast wires OTel for real (`pkg/observability/otel.go` builds a `TracerProvider` and an OTLP exporter) and then starts **one** span in the entire runtime, `digest.process`. There is no per-turn root span, so nothing links a turn to the inject that caused it. For the unattended product this is the audit artifact, which makes it a worse gap here than upstream |

### Design calls, not ports — 2 commits

`6c2c5c8` (park the loop on interrupt instead of cancelling and carrying on) and `0a6a056`
(pause/resume over HTTP, and `/interrupt` that actually holds the loop) are one change in two
commits, and mast already has all three endpoints. What upstream changed is the *semantics*:
interrupt stops meaning "cancel the turn" and starts meaning "park the loop".

**mast should not port this without deciding it.** mast's interrupt is precisely a cancel — that is
the producer #206 was about, and it is now load-bearing in two shipped behaviors: `turn-error`
kind `canceled` with `retryable: false`, and the `--watchdog=enforce` halt that rides the same
cancel. Parking instead would change what an operator's interrupt *means* on the wire two days
after mast documented what it means. Needs an owner and a decision recorded in
[`docs/README.md`](./README.md), not a port.

### Not applicable to the lean scope — 3 commits

| SHA | Upstream | Why |
|---|---|---|
| `d2bde30` | give sessions a short display name (attach protocol **1.6.0**) (#809) | A TUI/session-list affordance. mast's session identity is the workload and the session ID; no consumer has asked for a display name |
| `da3c006` | let an operator rename a session (#808) (#833) | The mutation half of `d2bde30`. Same verdict, same absence of a consumer |
| `05d730c` | make wake notifications actually reach an attached TUI (#814) | The defect is in `internal/coretuiremote` and core-tui's silent type assertion; mast has neither. mast does have `POST /wake`, but the fix's shipped half — a `wake` frame on the SSE stream, upstream's **1.7.0** — has no mast consumer today. A mast-web ask would change this verdict |

**`d2bde30` is the concrete proof of something [`DESIGN.md`](../DESIGN.md) now warns about.** Both
projects shipped a "1.6.0" this week: upstream's is session display names, mast's is the
`watchdog_halt` kind. The numbers collide and name different things. A client that reads a version
number as a feature set is wrong on both servers; `event_types` and `features` in the capabilities
frame are the only honest detection.

### Parity features — 3 commits, no verdict beyond "not in mast"

`c590015` (declare a whole MCP server read-only), `7dba589` (return the `prompt_id` an inject
assigned) and `e28dde1` (let `POST /inject` queue context without driving a turn). mast has the
surfaces all three extend — `pkg/mcp`, `pkg/inject` — and none of the extensions. No design doc
defers them, so house rule #7 does not apply; they are v0.5 parity candidates that want a consumer
argument before anyone spends a PR on them. `7dba589` is the cheapest and the most obviously
missing: an inject that drives a turn and tells the caller nothing about which turn it drove is
hard to build a client against.

### One that does not port as written — `32aed49`

Upstream's fix routes `spawn_agent` through the permissions gate. **mast has no `spawn_agent`** —
the only occurrence in the repo is a comment in `pkg/permissions/gate.go:160` describing the
upstream family. The commit does not apply.

The *general* claim behind it does deserve a look, and it is a bigger question than this commit.
mast consults its gate from exactly one place — `pkg/approval/plugin.go`'s two
`CheckMutatingToolCall` calls — while delegation happens through ADK-installed dispatch tools
(`task`, `single_turn`, `invoke_specialist`) that meet no gate. Whether that is a hole or a correct
reading of mast's allowlist story is a question for
[`docs/spike-findings.md`](./spike-findings.md)'s verified allowlist semantics and
[`docs/specialists-design.md`](./specialists-design.md), **not for a port of upstream's patch**.
Recorded here so the next triage does not re-derive it from scratch.

### `3de4134` — the baseline commit, and a port candidate

*(Verdicted 2026-08-21: **correct fix, not portable yet** — see [#221](https://github.com/go-steer/mast/issues/221).)*

`fix(usage): carry the digest subagent's cache buckets to the paying session (#845)` is both the
SHA this report was generated against and a change to a subsystem mast has. The defect is real in
mast's copy of the struct: `digest.Savings` carries `SubagentModel` / `SubagentInputTokens` /
`SubagentOutputTokens` and no cache buckets, so a subagent on a cache-warm model bills the paying
session at the uncached rate for reads it did not pay full price for.

It cannot be *observed* here, because **nothing in mast calls `pkg/digest` at all**. The package has
zero importers and is absent from `go list -deps ./cmd/mast`: the wrapper that drives digesting
upstream (`pkg/mcp/digest_wrap.go`) was never ported, and the descope was never written down as one.
So porting the cache buckets would add three more fields nobody fills to a struct nobody writes in a
package nobody calls — [#211](https://github.com/go-steer/mast/issues/211)'s shape a third time.

What shipped instead is the accuracy pass the finding demanded, since three surfaces read as though
digest were live: `attach.UsageInfo.DigestMethods` ("present when at least one `digest.Process` call
has fired", which is never), the attach protocol's v1.2.0 `latency_ms` and v1.3.0 `savings`
tool-result sidecars (specified as produced by two files mast does not have), and `Savings.Subagent*`
itself. Each is annotated where it lives, and the claim is held by a test rather than a comment —
`TestNothingInMastImportsDigest` fails the day something imports the package and names the three
annotations to correct. The port lands when #221 is answered with "wire it", and dies with "drop it".

### Baseline after this triage

Two commits came off (`04e54a3`, `6d30f9b`), but **neither moves the report**, and the reason is
the same one the 2026-08-17 section records for the watchdog cluster: mast's fixes landed in files
whose derivation trailers still name `25d8531c`. A port that does not re-port the file does not
bump the file's baseline, and inventing a trailer SHA mast never ported from would be a lie in the
one record this scheme rests on. `pkg/attach` will keep reporting both commits until it is next
re-ported wholesale.

That is the second time this quarter the count has failed to reward real work, and it is worth
saying plainly: **the count measures re-ports, not fixes.** Nine of the nineteen commits above are
verdicted as needing nothing from mast, and three of those are places mast arrived first. Read the
"port candidate" rows, not the number.

## Triage of 2026-08-21

The report reads **57 commits / 76 of 200 ported files** against core-agent `68ad89a`. **Nine SHAs
are new** since the 2026-08-20 baseline of `3de4134`, and all nine are verdicted below. Four became
mast issues, filed the same day.

One thing is worth reading off the batch before the individual rows. **Three of the nine commits are
about billing delegated work** — `57ac01c` bills a synchronously-invoked subagent's turns to the
parent, `f71c685` makes a declared budget bind on both delegation doors, `181327c` prices a cache
bucket the daemon can now ask for. Upstream is converging on the same conclusion from three
directions: *the door a delegation goes through decides whether it is counted, and the operator
cannot see which door was used.* mast has exactly one delegation door that leaves the outer runner,
and it counts nothing at all. That is [#226](https://github.com/go-steer/mast/issues/226), and it is
the most consequential finding this ledger has produced since the watchdog cluster.

### Confirmed analogues — 4 commits, all filed

Each was checked against mast's code; every row below is a finding, not a candidate.

| SHA | Upstream | The mast finding |
|---|---|---|
| `57ac01c` + `f71c685` | bill a synchronously-invoked subagent's turns to the parent (#849); a declared budget that binds on both delegation doors (#850) | **Worse here, and it is three consumers rather than one** — [#226](https://github.com/go-steer/mast/issues/226). `invoke_specialist` runs each specialist on a private runner with an in-memory session (`pkg/planner/dispatch.go:202`), so nothing riding the outer event stream sees a single event it emits: not the budget meter, not the metrics registry, not the watchdog (`cmd/mast/main.go:2536`–`2568`). A planner-dispatched specialist can spend without limit, report no tokens, and loop without being halted. Coordinator and graph dispatch are unaffected — `parallelagent` funnels sub-agent events upward — so an operator reads one bundle and gets two different enforcement stories depending on `dispatch:`. `MeterScopes`' per-specialist ceilings are unenforceable on this door for the same reason |
| `6813f6d` | add the dominant-tool-call density detector (#847) | **Same hole between the same two detectors** — [#227](https://github.com/go-steer/mast/issues/227). `RepeatedToolCallSignal` resets its run on any non-matching call; `AlternatingCycleSignal` skips near-uniform windows by construction (`uniform(tail[:p])`, `pkg/watchdog/cycle.go:165`). `a a a b a a a c a a a` trips neither until the interleaves happen to stop. The port's real work is de-duplication, not detection: mast appends every signal's alert, and under the `feedback` default three overlapping detectors on one loop is three paragraphs of steering for one behavior |
| `661f278` | a metrics page written from the code, plus a drift gate (#854) | **Half absorbed, half missing** — [#228](https://github.com/go-steer/mast/issues/228). mast is *ahead* on the page: `reference/metrics.md` lists exactly the sixteen families `pkg/observability` constructs, with labels and vocabularies. Nothing keeps it that way, and the same pipeline already carries the drift — `mast_scheduled_fires_total` and `mast_a2a_server_tasks_total` ship, are on the site page, and appear in neither the shipped nor the design-target column of `docs/observability-design.md` |
| `f90bc65` | a scripted provider that gives every Model call its own cursor (#853) | **Same defect, different door** — [#229](https://github.com/go-steer/mast/issues/229). `mock.NewScripted` returns one cursor, offline fakes collapse every per-specialist override back to that one instance (a documented feature of `BuildModel`), and fan-out runs its branches concurrently. Three branches then walk one script between them. The cursor is mutex-guarded, so `-race` stays silent while the replay is nondeterministic |

### Not applicable to the lean scope — 2 commits

| SHA | Upstream | Why |
|---|---|---|
| `64b1ceb` | opt-in `replace_all` for `edit_file`, and a refusal that names it (#851) | mast ships no file-editing built-ins. `edit_file` occurs in this repo only as a *name* in `pkg/permissions`' mutation vocabulary and its recommendation defaults; there is no `pkg/tools` and nothing to widen. mast's built-ins are the planner's control-plane vocabulary ([#219](https://github.com/go-steer/mast/issues/219)) |
| `9ccbcee` | frame every skill body as how, never what or where (#848) | mast has **no skills runtime at all**: the specialist loader refuses `tools.skills` outright rather than accept an inert grant (`pkg/specialists/loader.go:108`, [#211](https://github.com/go-steer/mast/issues/211)), and `pkg/permissions/gate.go:149` records that this build registers none of `list_skills` / `load_skill` / `load_skill_resource`. Nothing to frame |

`9ccbcee` should not be filed away as merely n/a, though — it is a **design input to
[`./skills-design.md`](./skills-design.md) that arrived before the subsystem**. Upstream's failure
was a subagent handed a fully specified goal that loaded a skill whose Step 0 said "acquire the
following context from the user", found no user, and improvised: 44 turns and $1.33 against $0.26
for the comparable run, with the answer still correct so the only visible symptom was the bill.
**mast's exposure would be strictly worse**, because "there is no operator to ask" is not an
accident of that run — it is mast's premise. Whatever mast's skills loader ends up being, the
framing trailer (a skill governs *how*, never *what* or *which subject*; parameters the task already
supplied are not reacquired; a step naming an absent tool is skipped, not improvised around) belongs
in it from the first commit, appended after the skill body rather than before it, because recency is
the mechanism of the bug.

### No exposure, and the reason was already written down — `181327c`

`feat(pricing): price 1-hour cache writes and ship the 1h prompt-cache TTL (#846)` touches
`pkg/pricing`, `pkg/usage` and the Anthropic provider — three things mast has. It still costs mast
nothing today, and the argument is already in mast's own source: `pricing.Rates`'
`CacheCreationInputPerMTok` doc says it holds exactly one write rate, the 5-minute one, and that "a
caller that starts requesting `ttl: "1h"` at the `cache_control` site would be undercharged by 37.5%
against this field; adding 1h support means adding a second rate here, not reusing this one."

mast has no such caller and no way to configure one. `pkg/providers/anthropic/convert.go:158` marks
the last system block with a bare `anthropic.NewCacheControlEphemeralParam()` — no TTL, so the
SDK's `omitzero` leaves the field unset and the request takes Anthropic's 5-minute default. There is
no `prompt_cache.ttl` key and no `--prompt-cache-ttl` flag. The undercharge upstream fixed is
reachable only by shipping the knob, which is the port's real content: **if mast ever offers a 1h
TTL, the rate field lands in the same PR, not after it.** Recorded here so that PR does not have to
rediscover why. (The digest half of the commit — carrying the 1h share through subtask result →
savings record → sidecar → observer — is moot for the third time running: nothing in mast calls
`pkg/digest`, [#221](https://github.com/go-steer/mast/issues/221).)

### `68ad89a` — the verdict shipped with the triage

`chore(ci): run the credential-free examples, and account for the rest (#855)` is upstream's answer
to finding `examples/parallel-spawn` had been exiting 1 for some time while its README advertised
"Exits 0". Its Go-program half does not transfer: mast has three Go examples, and two
(`examples/workflows/fan-out-fan-in`, `examples/workflows/llm-as-router`) carry their own tests
while the third (`examples/deploy/slim`) is gated by `scripts/check-slim-deps.sh`, which is a
stronger check than running it would be.

The transferable part is the rule, not the runner: *a new example is either covered or excluded with
a written reason, with no third state in which it is quietly uncovered.* mast had that third state
in its own idiom. Of three example **bundles**, only `gke-triage` was exercised — by
`dev/ci/presubmits/e2e.sh` and `deploy/projection_test.go` — while `bounded-triage` and `ns-audit`
were prose that nothing compiled. That matters here more than it would in a repo with static
examples, because mast's loaders keep *gaining* refusals: a tier that conflicts with a model, an
`output_schema` that will not parse, a roster whose read/write split does not hold, and
`tools.skills`, which started being rejected outright at #211. Each of those is a good refusal, and
each can turn a shipped example into one that no longer boots, with the failure landing on an
operator's first run.

`TestEveryExampleBundleStillBuilds` (`internal/compose/examples_test.go`) loads and composes every
directory under `examples/workloads`, offline, against the echo model — which is what makes it
credential-free, since offline fakes collapse tier and model overrides back to the one fake and a
real model name would send tier resolution looking for an API key. All three bundles pass today, so
this is preventive rather than corrective; it was verified by breaking one (`model:` beside `tier:`
in `ns-audit`) and watching the gate name the file. The tree is globbed rather than listed, so a new
example is covered by existing.

### Baseline after this triage

The count went **54 → 57** with no port, which is the instrument working as designed: nine upstream
commits landed and six of them touch packages mast carries a copy of. **Nothing here will move the
number, either.** The examples gate is mast-side and touches no ported file; the four filed issues
are fixes whose landing place is `pkg/planner`, `pkg/watchdog`, `pkg/observability` and
`pkg/providers/mock`, and — as the 2026-08-17 and 2026-08-20 sections both record — a fix that does
not *re-port* a file does not bump the file's derivation trailer.

Two of the nine are permanent residents: `64b1ceb` and `9ccbcee` are upstream commits on subsystems
mast does not have, so they will report every Monday until the packages they touch are re-ported or
mast grows the subsystem. That is the 13-commit n/a floor from 2026-08-17 becoming 15.

## Next triage

The weekly report regenerates [#153](https://github.com/go-steer/mast/issues/153) in place. Triage
again when the counts move materially, or when a security commit appears upstream — whichever comes
first. Start from the "port" and "watch" rows above rather than from a fresh count; the absorbed and
n/a verdicts do not need re-deriving unless the trailer they hang on changed.

Carried into the next pass from 2026-08-20, in the order they are worth doing:

1. ~~**`a58fdcc` — a per-turn root span.**~~ **Shipped 2026-08-20**
   ([#215](https://github.com/go-steer/mast/pull/215)). Every turn opens `mast.turn` at the shared
   chokepoint with ADK's tree beneath it. mast needs **no** inject→turn links, contrary to this
   report's own note: upstream's links exist because its inbox batches injects so one turn answers
   several, and mast dispatches an inject synchronously on the request goroutine, which makes the
   inject's server span the real parent already.
2. ~~**`6d1afd1` — a route that amends a session ACL.**~~ **Shipped 2026-08-20**
   ([#216](https://github.com/go-steer/mast/issues/216)). `GET`/`PUT /sessions/{id}/acl`, read at
   the read bar and write at the admin bar. Two findings the port itself produced: a `PUT` on a
   daemon that does not enforce ACLs is refused with 501 rather than accepted (an amendment nothing
   consults is [#211](https://github.com/go-steer/mast/issues/211)'s defect one layer up), and
   `Entry.ACL` had to become a lock-guarded accessor — a field written once at registration and read
   unlocked by every authorizing request is a torn read the moment amendment exists.
3. ~~**`453e3f0` — subagent tool grants on `/subagents`.**~~ **Shipped 2026-08-21**
   ([#218](https://github.com/go-steer/mast/issues/218)). Cheap as predicted, and the port produced
   two findings the upstream commit does not have. The grant is published by its *effect*
   (`mcp_grant: all|none|listed`, plus `whole_server` per entry) rather than transcribed, because
   `ToolAllowlist` reads presence per axis and JSON erases the nil-versus-empty distinction that
   carries it — a copied list would render the deny-all specialist and the inherit-everything one
   identically. And the built-in axis ships as `builtin_declared`, because nothing populates
   `specialists.BuildOptions.Tools`: filed as [#219](https://github.com/go-steer/mast/issues/219).
4. ~~**`3de4134`**~~ — **verdicted 2026-08-21**: correct fix, not portable, because `pkg/digest` has
   no caller in mast at all. The three surfaces that implied otherwise are annotated and the claim
   is now a test; the wire-it-or-drop-it call is [#221](https://github.com/go-steer/mast/issues/221).
   That emptied the 2026-08-20 list; the 2026-08-21 pass refilled it, below.
5. ~~**[#210](https://github.com/go-steer/mast/issues/210) / [#211](https://github.com/go-steer/mast/issues/211)**~~ — both shipped 2026-08-20.

Carried forward from 2026-08-21, in the order they are worth doing:

1. **[#226](https://github.com/go-steer/mast/issues/226) — a planner-dispatched specialist's spend
   reaches no meter, no metric and no watchdog.** Do this one first. It is the only finding here
   that costs real money on a live workload, it is the backstop the whole unattended thesis rests
   on, and it has been a `TODO` in `pkg/planner/dispatch.go` since v0.1 with no issue behind it.
   The budget half is the fix; the watchdog seam and the `pkg/effects` outbox are separate
   follow-ups on the same issue. **Metering half shipped 2026-08-21** (`7105fb9`): the dispatch tool
   hands each sub-run event to the host through `planner.Config.SubRunObserver`, and a cap crossed
   inside a dispatch stops the dispatch rather than the session. **Watchdog half shipped
   2026-08-21.** The row above said its enforcer taps a stream and so has nothing a process-scoped
   observer can join; the answer was to give the stream's per-event body a name —
   `watchdog.ObserveInto` and `watchdog.Drain`, with `Tap` re-expressed over both, so the two paths
   cannot drift. `SubRunObserver` was reshaped from a flat callback to a sink opened per dispatch
   (unreleased, so free to change): a meter is cumulative and was happy flat, but the watchdog dedups
   an aggregator's re-emissions *within* a run and counts repetition *across* runs, and a flat
   callback cannot even tell two parallel dispatches apart. Observation goes to the **session's**
   watchdog — a per-dispatch one resets the repeat count every dispatch — and a trip fires both
   levers, unlike the budget half, because a trip is a latch an operator must clear rather than a
   cumulative total. **#226 now stays open only for the effects outbox**, and the measured shape of
   that is worse than the issue said: see item 5.
2. ~~**[#228](https://github.com/go-steer/mast/issues/228) — a drift gate for the metrics page.**~~
   **Shipped 2026-08-21** (`0ccf5de`). The gate primes a registry, GETs `Handler()`, and compares
   the parsed scrape against the parsed page both ways — names, labels, and the enumerated values
   `Prime` materializes. Reading the scrape rather than regenerating names from
   `prometheus.CounterOpts` is the whole point: upstream derived its column and got two names wrong.
   The `observability-design.md` drift is folded in, in a delimited shipped inventory the same test
   holds to the registry, and the A2A / AG-UI sketches now say how the shipped families differ from
   them (`{workload}`, not `{skill}`; `interrupted`/`aborted`/`rejected`, not `interrupt`/`cancelled`).
3. ~~**[#227](https://github.com/go-steer/mast/issues/227) — the dominant-tool-call density
   detector.**~~ **Shipped 2026-08-21 as a constructor, undefaulted, as the triage asked.**
   `NewDominantToolCallSignal(window, threshold)` at 8-of-12, deferring structurally to whichever
   detector already owns the shape (`DeferToRepeatRun`, `DeferToCyclePeriod`; zero disables either,
   so an embedder wiring it alone sees every dominant loop). `NewDefaultWatchdog` does not wire it,
   and a test fails if that changes without the docs changing with it —
   **whether it joins the default set is still the open watchdog-governance call for a human.**
4. ~~**[#229](https://github.com/go-steer/mast/issues/229) — one scripted cursor, N concurrent
   branches.**~~ **Shipped 2026-08-21.** A replay is now keyed on the ADK branch tag the model's own
   context carries, so the fix stayed inside `pkg/providers/mock` — no change to `internal/compose`,
   `pkg/specialists` or `pkg/graph`. Upstream's `f90bc65` gave every `Model` call its own cursor;
   mast keys on the branch instead, because per-call would restart a multi-turn replay at turn 0 and
   the branch is ADK's own concurrency-isolation identity: every sequential shape inherits its
   parent's unchanged, so a coordinator, a planner and a resumed run keep walking one cursor in one
   order. The acceptance test the row said was blocked is written: a three-analyst fan-out against a
   **one-turn** recording, which cannot pass at all under a shared cursor.
5. **[#235](https://github.com/go-steer/mast/issues/235) — a planner-dispatched specialist's
   mutating tool calls bypass the write gate and the effect outbox.** Found while shipping item 1's
   watchdog half, and it is the same seam read one level wider: runner **plugins** are
   runner-scoped, and the dispatch tool builds its runner with none. #226 named only the missing
   outbox record; the write gate is missing too, so a mutating call inside a dispatch is not gated
   even under `hitl.on_mutation: require_approval`. Measured rather than inferred — an outer
   `BeforeToolCallback` sees `invoke_specialist` and `finish_task` and never the `scale_deployment`
   the specialist actually executed. The fix is not mechanical: an approval park suspends a turn to
   be resumed from a durable session and a sub-session is in-memory, so this needs a decision about
   where the boundary sits (give the sub-run the host's session service, gate at the dispatch
   boundary, or refuse a `change_executor` in a planner roster until one of those lands). Containment
   today is `ClassSpawning` plus `CheckCapabilitySplit`, which narrows the door to a roster that
   declares `capability: change_executor` out loud — narrower than "any workload", but a supported
   configuration walks through it, and `examples/workloads/gke-triage` is two commented lines away
   from being one. **Contained the same day** by `compose.CheckPlannerWriteSurface`: the combination
   is refused at composition, on both doors (`BuildRoot` and the pre-MCP `CheckRoster`), scoped to
   the promise actually broken — `on_mutation: apply` is exempt, since there is no gate under
   `apply` for a dispatch to bypass. That is containment, not the fix; #235 stays open for the
   boundary decision, and the check comes out with whichever answer lands.

Two open questions that are not ports and need an owner: whether mast follows upstream's
park-on-interrupt semantics (`6c2c5c8` / `0a6a056`, and it collides with what
[#206](https://github.com/go-steer/mast/issues/206) just documented), and whether ADK-installed
dispatch tools should meet the permissions gate (raised by `32aed49`, answerable only from mast's
own allowlist story).

And one row that is neither a port nor a question, but a **precondition to write down**: `181327c`
becomes portable the moment mast offers a prompt-cache TTL knob, and until then it is a rate mast
would carry unused. Whoever ships the knob ships the rate in the same PR. Nothing needs to happen
before then.
