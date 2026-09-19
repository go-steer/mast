# Changelog

## Unreleased

### API Change

- **`mast sessions pause --interrupt` is now `--cancel-turn`, and the old
  spelling is refused.** `--interrupt` meant a bool on `pause` ("also cancel
  the in-flight turn") and a string on `resume` (the InterruptID being
  answered): one name, two types, two unrelated meanings, on sibling
  subcommands of the same verb family. `resume` keeps it, because the *noun*
  sense is the one the approval records, the resume-by-token path, and the
  rest of the corpus already use. The cost of the ambiguity was paid before
  the rename rather than hypothetically: three v0.9 docs were drafted saying
  `mast sessions --interrupt` before correction. **It is a refusal, not an
  alias** — accepting both spellings would carry the ambiguity past v1.0,
  which is the thing the rename exists to prevent, and a pause that silently
  did not cancel the turn is the misreading with a cost. What a caller gets
  instead of `flag provided but not defined: -interrupt` (which reads the same
  for a typo as for a rename) is a sentence naming the new flag and why it
  moved. The wire field is unchanged: `PauseRequest.Interrupt` sits beside
  `ResumeRequest.InterruptID` and so never collided, and `pkg/inject`'s JSON
  was not what this was about. Landed now because `cmd/mast` is inside the
  v1.0 promise ([#336](https://github.com/go-steer/mast/issues/336)), which
  freezes flag names at the tag — this was the last window in which the rename
  was free ([#337](https://github.com/go-steer/mast/issues/337)).

- **`--attach-listen` now implies a durable session database instead of
  refusing to start without one.** With no `--session-db` of your own, an
  attach daemon opens `~/.mast/sessions.db` and logs the path it chose plus
  the flag that relocates it. The old behaviour was a startup error telling
  you to pass a flag that had exactly one sensible value — attach live-tails
  from the eventlog overlay, which an in-memory store does not have, so
  there was never a second answer to give. Naming a path still wins and
  nobody's database moves. Two shapes are **refused rather than papered
  over**: `--session-db=` (explicitly empty) with `--attach-listen`, because
  asking for no store and asking for the attach surface are contradictory
  requests and silently honouring one of them is worse than saying so; and
  `--session-db-driver=postgres` without a DSN, because a DSN carries a
  host, a database and credentials, none of which can be invented — that
  error now says the implication does not apply rather than repeating
  "requires `--session-db`". The explicitly-empty case is why this reads
  whether the flag was *given* rather than what it holds: mast's
  `--session-db` is a string, so an explicit empty one is indistinguishable
  downstream from an absent one. **The new cost is a file appearing in a home
  directory nobody named**, which is why the path is on the startup line
  rather than only in the docs. Ported from `core-agent@164365c`, which had
  no path for mast to copy — upstream's `--session-db` is a bool beside a
  `--session-db-path` that already defaulted
  ([#329](https://github.com/go-steer/mast/issues/329)).

- **An AG-UI thread now bounds how many runs may queue on it, and refuses the
  rest with `409`.** A thread has always run one turn at a time — a mast
  session is a single-writer store — but the queue behind it was unbounded and
  invisible: a second run blocked on the per-session turn lock with the
  workload's `budget.max_wallclock_seconds` as its only ceiling, so a client
  that stacked runs could not tell waiting from wedged and what it eventually
  got was a timeout carrying nothing about why it waited. The new
  `agui.run_queue.depth` counts runs that may **wait**, defaulting to 3, so a
  thread admits four at once and the fifth is refused with `409` and a
  `Retry-After` **before the SSE upgrade** — a status a client can branch on
  rather than a stream opened for the sole purpose of carrying an error frame.
  Three choices are worth naming. **409, not 429**, because `MAST_AGUI_RATE`
  already returns 429 and means *arrival rate*: a client that cannot tell
  "back off everywhere" from "this one conversation is busy" picks the wrong
  remedy, and the rate limiter never saw this case anyway — one caller can
  hold any number of runs open on a thread as long as they arrive slowly
  enough. **`depth` counts waiters rather than runs**, which is what makes
  `depth: 0` mean the useful thing (no concurrency on a thread) instead of the
  absurd one (refuse everything). And **there is deliberately no way to spell
  "unbounded"** — unbounded is what the key exists to end — so a negative
  depth is refused at startup rather than read as `-1`. Refusals are counted
  as `mast_agui_runs_total{outcome="queue_full"}`, kept distinct from the
  `rejected` the credential-shaped refusals share, because one is answered
  with capacity and the other with a token. The bound is per derived session,
  so a busy thread never refuses a run addressed to another one, and
  `sessionTurnLocks` stays unbounded underneath it: inject, resume, scheduled
  fires and auto-resume have no client standing there to be refused. Landed
  before the freeze rather than after, because bounding a queue that is
  unbounded today changes what an existing bundle does — the breaking
  category `docs/compatibility-policy.md` calls invisible to any signature,
  and the one that would have cost two released minors and 90 days on the
  other side of v1.0 ([#384](https://github.com/go-steer/mast/issues/384)).

### Feature

- **`GET /healthz` is a readiness probe that can actually go red.** The
  daemon's health check was `GET /`, which consults nothing and answers a
  static `200 ok`. A daemon whose session database had been deleted, whose
  volume was unmounted under it, or whose Postgres had gone away stayed 1/1
  Ready and kept accepting injects it could not durably record — and because
  the route is *named* the health check, that looked like a readiness signal
  rather than the absence of one. `/healthz` runs the registered checks and
  answers `200 {"ok":true,"checks":{"session_db":"ready"}}` or `503` with the
  check `failed`. The one check today is the session store, registered when
  `--session-db` is set: a real bounded read against ADK's `events` table —
  the one table both the attach and the plain durable path create, which
  mast's own `agent_eventlog` overlay is not — rather than a pool ping, so it
  catches a dropped table, a lock past the busy timeout, a closed pool, a
  database that went away and (verified on mast's SQLite driver, not assumed)
  an unlinked file. It does not catch a read-only filesystem; a read still
  succeeds there. A daemon with no session store answers `200` and an **empty**
  `checks` object, because "up, with nothing durable to vouch for" is a
  different statement from "the database is fine" and an in-memory daemon
  answering 503 would never join its Service. The route is unauthenticated by
  construction — a kubelet has no bearer token — so the body carries only
  `ready`/`failed` per check and never the error, which routinely names a path
  or a DSN host; that goes to the log, once per health *transition* rather
  than once per probe, a distinction worth ~8,600 lines a day per pod at
  kubelet's default cadence. `deploy/base/50-statefulset-daemon.yaml` moves
  `readinessProbe` to `/healthz` and **leaves `livenessProbe` on `/`**: the
  action behind a liveness failure is a restart, and a restart does not fix a
  deleted volume — it turns one unready pod into a crash loop that also kills
  every in-flight turn ([#326](https://github.com/go-steer/mast/issues/326)).

### Bug or Regression

- **`GET /sessions/{id}/usage` now fills in the token breakdown it has
  promised since the route shipped.** The response declared eight token
  fields; mast populated a turn count and a dollar figure and left every
  bucket at zero. Three of the eight are `omitempty` and five are not, so a
  caller could not tell a measured zero from an unmeasured one, and all of
  them were the second kind — the shape this repo keeps producing, a claim
  that reads like an answer. What was missing was the producer, never ported
  from core-agent ([#356](https://github.com/go-steer/mast/issues/356)).
  `overall`, `per_model` and `per_turn` are now real: input split into
  cached, written and fresh, output with thinking inside it, per-call
  timestamps and the model each price resolved against. **The wire shape is
  unchanged** — v0.5's wire-literal pin holds, fields got populated, not
  renamed. The counts come off the same `OnSpend` hook the durable spend
  ledger does, rather than from a second reader of the event stream:
  splitting a prompt into cached, written and fresh is the meter's own
  reading of the provider's counters, and a report derived any other way
  would describe a different call than the one the money came from. Two
  consequences worth reading before acting on the numbers: a cache *write*
  counts as uncached, because that is how it bills (at a premium, not a
  discount); and `cost_usd_uncached_reference` is allowed to fall below
  `cost_usd` rather than being clamped to it, so a session that has warmed a
  cache it has not reused reports *cache cost $X, not yet repaid* instead of
  reporting nothing. `turns` and `cost_usd` still come from the meter, so
  `/usage` and `/guardrails` cannot disagree about what a session spent
  across a restart; the buckets beside them are this process's, because the
  ledger stores what a call cost and not what it was made of. `per_turn` is
  capped at 500 rows with absolute turn numbers, so a first row numbered
  above 1 is the record that rows were dropped.

- **An MCP server that asks mast for input now fails the call saying so,
  instead of returning an empty tool result.** mast refuses server-initiated
  input — elicitation, sampling, roots — on both protocol versions, and has
  since the feature landed. But only one of the two routes was actually
  mast's refusal. On the older protocol the server sends a request and mast
  declines it by name. On the 2026-07-28 protocol the server answers the call
  with an *input-required result* instead, and mast was handing that result
  to ADK's `mcptoolset`, which has no notion of one: it reads the content,
  finds none, and reports a tool that returned nothing. What made this look
  fine was an accident in `adk/v2` through v2.2.0, which errored on any
  empty-content result — so an input request surfaced as *an* error, just one
  that named nothing and came from the wrong layer. v2.4.0 replaced that
  branch with a formatter that renders empty content as `""` and returns
  success, and the accident stopped happening: the model began being told the
  tool returned nothing. It is now mast's own check, at the seam with ADK, and
  it names the request the server made
  ([#430](https://github.com/go-steer/mast/issues/430)). The unopinionated
  client still surfaces the result — a caller that owns the retry loop is
  where an approval gate would go if mast ever supports elicitation, and it
  cannot gate a request it is never told about. Two tests, one per client, pin
  that difference so collapsing them back together fails.

- **The docs site no longer builds by accident.** `docs/site/astro.config.mjs`
  imports `@astrojs/markdown-remark` and `docs/site/package.json` never
  declared it; the import resolved anyway because npm hoists that package to
  the top of `node_modules` as a transitive dependency of `astro`, and a
  hoisted package is indistinguishable from a declared one at resolution time.
  A `@astrojs/starlight` **minor** bump reshuffles the tree, the hoist stops,
  and the build dies with `Cannot find module` — with nothing in this repo
  changed. Declaring it is one line. The half worth reading is the second:
  `dev/tools/docs-lint` now fails when the site's own JavaScript imports a
  package `package.json` does not declare, and it lives there rather than in
  the site build because `docs-lint` is a required check on `main` and
  `ci-docs / build` is not — the one class of breakage this catches arrives
  *as a dependency bump*, which is exactly when nobody is reading an advisory
  red ([#422](https://github.com/go-steer/mast/issues/422)).

- **An OTel SDK minor bump can no longer stop the daemon from starting.**
  `pkg/observability` built its exported resource by merging mast's
  `service.name` — tagged with the schema URL of the `semconv` package mast
  imports — onto `resource.Default()`, whose schema URL is whichever semconv
  version the *SDK* was built against. `resource.Merge` refuses two different
  schema URLs, so that line asserted a version match between two dependencies
  that move on separate schedules. It held by coincidence until the SDK's
  semconv advanced, and then `SetupOTel` returned `conflicting Schema URL` to
  every process configured with an OTLP endpoint — which `cmd/mast` treats as
  fatal, so this was not degraded telemetry but a daemon that refuses to boot,
  citing a mismatch no operator caused. mast now merges a **schemaless**
  resource, which merges with anything and leaves the SDK's own schema URL on
  the result — a truer claim than mast naming a version it does not control.
  Bumping the import to the matching semconv would have fixed the pair and
  rebuilt the trap; the coincidence is on a weekly schedule now that
  Dependabot runs. Found by the auto-merge gate on its first day, on the real
  bump that triggers it ([#420](https://github.com/go-steer/mast/issues/420)).

- **A second replica of a scheduled workload no longer fires everything a
  second time.** Three loops in `serve` start turns nobody asked for — the
  scheduled trigger, the timed-pause scheduler and the boot auto-resume scan —
  and each of them ran once per replica. An operator who scaled a Deployment
  to 2 got every cadence, every timed resume and every cut-short session
  continued twice, with nothing in any log to say so; the first symptom was a
  duplicate remediation at the far end. All three now sit behind one lease
  (`scheduling/<workload>`) taken over the session store, so exactly one
  instance drives them and the others serve inject, AG-UI and A2A as before.
  One lease rather than three because "is this replica the one that acts on
  its own?" is one question, and three would let a deployment be half-leader.

- **The replica that does not drive them says so, at ERROR, naming the holder
  and all three loops it is not running.** For an operator this line *is* the
  feature: the failure it replaces is one you diagnose backwards from side
  effects, and the fix — scale to 1, or accept the passive replica — takes one
  command once you know. Two boot cases decide the other way and also say so:
  with no `--session-db` there is nothing to coordinate through, so the daemon
  keeps firing and warns naming the flag; and if the store **cannot answer**,
  it keeps firing and logs an ERROR explaining the risk. That second one fails
  open on purpose — duplicate work requires a second replica to actually
  exist, while failing closed would turn a database hiccup at boot into a
  single-replica workload that never fires again and stays quiet about it.

- **A timed pause created on a passive replica still fires.** `mast sessions
  pause --resume-at` can land on any instance and the record is durable
  wherever it lands, but the scheduler that arms it belongs to the leader. The
  leader now rescans the pause table every minute alongside its boot scan,
  bounding that timer's lateness at a minute instead of at the leader's next
  restart. It is idempotent by construction — pending timers are keyed by
  token and the fire path re-fetches the record — so re-arming a token
  somebody already resumed is a silent drop, not a second fire. A store the
  rescan cannot read logs once when it breaks and once when it recovers,
  rather than once a minute for as long as it is down.

- **A restart after `kill -9` takes its own abandoned lease back, in about
  ten seconds.** This is the half the first cut got wrong, and the UAT caught
  it: a SIGKILLed daemon never releases its lease, so its replacement found
  the lease held — by the corpse of itself — and went passive. Permanently. An
  OOM kill would have ended scheduled work until somebody restarted the pod a
  second time, which is a worse bug than the duplicate firing being fixed. A
  contested boot now polls for the staleness window plus two seconds before
  concluding the holder is alive, and says at INFO that it is waiting so the
  pause does not read as a hang. The instance lease is also tighter than the
  session lock it reuses — 2s heartbeat, 8s staleness, against 5s/30s —
  because this window is waited out on every contested boot, while a session
  lock's is only ever waited out by something stealing a stuck turn. The cost
  lands on a genuine second replica, which spends ten seconds at boot before
  it can announce that it is second: once per replica, against a crash cost
  paid on every crash. The window is bounded on purpose; retrying forever is
  takeover by another name.

  **This is not leader election and mast is still not multi-replica.** A
  replica that loses the race *at boot* stays passive for its whole life and
  does not take over when the leader dies later; Kubernetes restores the
  leader instead. Once the lease goes stale it is taken by whichever instance
  **boots** next, with no identity check — a redeploy or a scale-up claims it
  as readily as a restart of the pod that died. What never claims it is an
  instance already running, which asks once at startup and never again; that
  asymmetry, not the lease's expiry, is what "passive for its whole life"
  means. A leader that loses its lease mid-life stops its
  scheduling loops and keeps serving requests, rather than racing the instance
  that took it. Session-ownership handoff and per-pause claims — the parts
  that would make a fleet work — are still designed and not built
  ([#345](https://github.com/go-steer/mast/issues/345)).

- **The watchdog no longer counts a streamed tool call twice, and the
  assumption that let it get away with that is now a test rather than a
  comment.** `ObserveEvent` and `ObserveToolResults` skip partial events, so a
  call is counted once, on the event ADK actually runs it from. Nothing
  observable changes today: mast passes `StreamingModeNone` at every runner
  site, so no partial event exists to skip. What changes is what happens the
  day one of those sites passes `StreamingModeSSE` — previously, one tool call
  would have reached the watchdog once per provider chunk plus once more on the
  aggregate, and the repeat detectors would have halted a turn that did nothing
  wrong, three files away from the symptom. The subtler half is the one the
  upstream report did not predict: mast's per-turn dedup set already collapsed
  the calls the provider sends whole, but a provider that *streams* the
  argument object sends chunks naming the function with no arguments yet, and
  when those chunks carry a call ID the dedup collapses onto the **first** one
  — recording `{}` for every call to that tool, so the literal-compare detector
  reads two genuinely different calls as a repeat. A wrong halt, not a
  miscount. Partials still reach every other consumer of the stream untouched;
  they are not counted, not swallowed. Ported from `core-agent@dd2007f`
  ([#331](https://github.com/go-steer/mast/issues/331)).

  The pin is `TestEveryRunnerSiteIsNonStreaming`, which reads every `RunConfig`
  literal in non-test code and fails on any that sets `StreamingMode` to
  anything it cannot prove is `StreamingModeNone` — including a variable, which
  is the shape a `--stream` flag would arrive in. It does not forbid SSE; it
  forbids turning SSE on silently, and its failure message is the audit list
  for doing it properly, naming the consumers already safe and the ones that
  are not. The worst of those was `cmd/mast/agui.go`, which is the next entry;
  what the message now names as unaudited is the A2A pair
  ([#408](https://github.com/go-steer/mast/issues/408)).

- **The AG-UI emitter would have dispatched every tool call once per provider
  chunk the day streaming was turned on; it now drops partial events.** Same
  class as the watchdog fix above, found by auditing who else depended on one
  event being one whole model response, and nothing observable changes today
  for the same reason: no runner site sets `StreamingModeSSE`. What it
  prevents is worse than a miscount. Because every frame is minted per event,
  a streamed tool call produced a *complete* `TOOL_CALL_START`/`ARGS`/`END`
  triple per chunk with empty arguments — and a client that dispatches on
  `TOOL_CALL_END`, which is the frame the protocol has for exactly that, would
  run the tool several times, most of them on `{}`. The assistant-text half is
  a rendering bug beside it: an N-chunk answer became N one-fragment messages
  and then the whole answer again. `RunFinished.result` was always right,
  because the aggregate arrives last and wins — so the surface that looked
  correct was the one least able to reveal the problem. This makes the emitter
  correct under SSE; it does not make it stream. A caller who turns SSE on
  gets whole messages, not deltas, and feeding chunks into the delta frames
  AG-UI already has is
  [#407](https://github.com/go-steer/mast/issues/407) — deliberately deferred
  until something in the module can actually stream, so the result can be
  checked against a real client instead of a fixture. The one consumer still
  unaudited is A2A progress narration, which fragments rather than duplicates
  ([#408](https://github.com/go-steer/mast/issues/408))
  ([#400](https://github.com/go-steer/mast/issues/400)).

- **A Vertex context cache whose TTL has elapsed is now recognised as gone.**
  Vertex has two ways of saying the cache you hold a handle to is not there:
  `404` / `NOT_FOUND` naming "cached content" when the handle has been reaped,
  and `400` / `INVALID_ARGUMENT` naming "cache content" — no "d" — as
  *expired* when the TTL simply ran out. Nothing in mast matched the second.
  The consequence was not a lost cache but a wedged agent: the manager kept
  handing the dead name to every later turn, each one came back a `400` that
  reads like a configuration error, and nothing in the process ever
  reconsidered, so the session stayed down until a restart. The same verdict
  is now reached in one place, `internal/vertexcacheerr`, because the two
  packages that need it — the cache manager, which meets the error on a TTL
  refresh, and the Gemini wrapper, which meets it on a stamped
  `GenerateContent` — deliberately do not import each other, and their
  disagreeing is what the bug *was*. A refresh that is told the cache is gone
  now drops the handle rather than logging and keeping it; the two premises
  that branch used to state (that the handle is valid until it expires, and
  that recovery happens automatically on the next turn) were both false, and
  are deleted rather than softened. Transient refresh failures — a throttle, a
  deadline — still keep the handle, because they are not evidence about
  whether the cache exists. **Scope:** `cmd/mast` never wires the cache
  manager, so the daemon was never exposed; this is a fix for embedders that
  wire `ContextCacheName` / `ContextCacheInvalidate` themselves
  ([#325](https://github.com/go-steer/mast/issues/325)).

- **A token count a provider reports as negative is now read as zero, before
  anything reads it.** A miscount used to reach the durable spend ledger, the
  session totals behind `GET /usage`, the buckets handed to the pricer, and
  the `sub_total_tokens` a planner dispatch reports back to the model as the
  price of a specialist. The consequence is not a wrong invoice — it is a
  guardrail that opens: a credit walks the running total *backwards*, so a
  session already at its ceiling buys itself more room by being metered
  wrong, and on the durable ledger a negative delta cannot be corrected after
  the fact. **The placement is the fix, not the clamp.** Guarding the term
  inside the pricing call would have kept the negative out of the invoice and
  left it in all three of the other readers, which is the argument
  `core-agent@e385fb0` makes and the reason this was a port candidate at all;
  mast floors once, where the metadata enters the meter, and an AST test pins
  that the package reads a provider's usage record in exactly one function. A
  count that is too *high* is unaffected — that is `fitBucket`'s job and it
  moves in the other direction. Two of the four readers the filing named
  needed nothing: the snippet it quoted had already moved, and the OTel
  registry had guarded itself since it was written
  ([#332](https://github.com/go-steer/mast/issues/332)).

- **An unauthenticated inject listener no longer binds a non-loopback
  address.** Attach, A2A and AG-UI have each refused this shape since they
  were added; inject is the surface the policy was never retrofitted to,
  because the policy was written for the listeners that came after it. That
  left the refusal missing from the one port that is on by default and the
  most powerful of the four: `/inject` starts a turn, `/resume` releases a
  parked mutating call, `/stop` ends the daemon. Each of the three surfaces
  that already refuse justify it with a capability inject also has, and mostly
  had first — A2A cites `tasks/cancel` being destructive, AG-UI cites a run
  spending budget; `/resume` releasing a write gate is a stronger case than
  either. `mast` now exits at startup, before provider detection and workload
  resolution, naming both ways out: set `MAST_INJECT_TOKEN`, or bind
  `127.0.0.1:7777`. **A users file does not satisfy it.**
  `MAST_INJECT_USERS_FILE` gates `/resume` and `/monitor-ack` only — the
  remaining routes still read the shared token alone, so a daemon with a table
  and no token has `/inject` and `/abort` wide open, and counting the table
  would let an attribution feature quietly stand in for an authentication one.
  That is the single point where inject's policy differs from attach's, where
  enforced multi-session auth *does* count. **`--listen`'s default stays
  `:7777`.** Quietly relocating it to loopback would make every container
  deployment unreachable with no message saying why, which is the failure the
  refusal exists to prevent; the two shipped example deployments both already
  set a token, and the one repo script that bound `:PORT` ungated now binds
  loopback. What breaks is a deployment that was already serving `/resume` to
  its whole network ([#361](https://github.com/go-steer/mast/issues/361)).
  Landed before v1.0 deliberately: after the freeze, a startup that begins
  refusing a configuration it used to accept costs two released minors and 90
  days under `docs/compatibility-policy.md`.

- **A `users.json` Secret mounted into mast's own StatefulSet was refused at
  boot, and the recipe that arms it is the one mast ships.** `LoadUsersFile`
  rejected any group or other permission bit. Kubernetes `fsGroup` is the
  standard way to give a non-root pod read access to a Secret volume and it
  sets group-read on every file in that volume unconditionally, so a Secret
  mounted at `defaultMode: 0400` arrives as `0440` —
  and `deploy/base/50-statefulset-daemon.yaml` sets `fsGroup: 65532` because
  the PVC needs it. Nobody was hitting this, because the shipped recipe uses
  single-bearer auth and mounts no users file; the person who hits it is the
  next one to add per-person attribution to that manifest, and what they get
  is a boot loop blaming file permissions they did not set and cannot unset
  without removing the `fsGroup` the volume depends on. Group-read is now
  accepted **when the process is a member of the owning group**, and that
  equality is the whole security argument: group-read widens access to
  members of that group and to nobody else, so when the daemon is already one
  of them the bit grants no read it did not already hold. **Supplementary
  groups count, and that is the fix rather than a widening of it** — a pod
  that sets `fsGroup` without `runAsGroup` carries the gid supplementally and
  runs with a primary gid of 0, so a primary-only test would reject the most
  common manifest shape and change nothing. Everything else stays strict,
  including the bits that look harmless beside group-read: any other-bit is
  rejected because "other" is unbounded by definition, and group-**write** and
  group-**execute** are rejected even for our own group, because neither is
  needed to read a credential and a co-member who can write the file can swap
  the daemon's entire user table. A refusal now names the gid on both sides
  instead of dumping a mode, since "0440 is wrong" does not tell an operator
  which of the two numbers to change. The policy is a pure function over
  (mode, gid, membership oracle), so the cases that cannot be staged on disk —
  a foreign gid needs privileges CI does not have, an unreportable one cannot
  be produced at all — are tested rather than reasoned about; the gid lookup
  is the only platform-split part and reports "unknown" off POSIX, so the
  permissive branch cannot open where membership cannot be established.
  `deploy/` grows the multi-user recipe as a commented-out **direct** Secret
  mount: upstream kept the `chmod` initContainers in its recipes because their
  manifests pin tags that predate the fix, but mast never had one, so the
  thing to copy is the correct shape rather than a workaround with a note
  saying to delete it. Ported from
  [`go-steer/core-agent@1423bb1`](https://github.com/go-steer/core-agent);
  landed before v1.0 deliberately, because after the freeze a change to what a
  loader accepts is indistinguishable in shape from one that costs two minors
  and 90 days ([#328](https://github.com/go-steer/mast/issues/328)).

### Documentation

- **The corpus now says that the daemon's own bearer token is one principal
  holding every configured scope, so its per-workload scope check cannot
  fail.** `MAST_A2A_TOKEN` and `MAST_AGUI_TOKEN` are each a single token
  resolving to a single subject carrying the union of every exposed
  workload's declared scopes — necessarily, since one token has to drive all
  of them. Everything written about the check was *true* and read as
  something stronger: "a token lacking the scope is refused `403`" is a fact
  about the check and not about any deployment mast ships, and
  `/agui/agents.json` publishing per-workload scopes invites the reading that
  tokens differ per workload, when there is only one. So an operator cannot
  express "this token runs the reporting workload and not the deploying one",
  AG-UI thread ownership separates *deployments* rather than users (which is
  [#382](https://github.com/go-steer/mast/issues/382) behaving correctly on a
  constant input, not a defect in it), and rate-limit buckets are per
  workload rather than per caller. What changes all of that at once is a
  host-supplied `serverauth.TokenValidator` — an existing seam needing no
  change to either server — which is why this ships as a correction rather
  than a redesign; a shared token is a defensible default for a
  single-tenant daemon, and what was missing was anywhere to find that out.
  A new "Scopes and the shared token" section on the CLI page states it once
  and the other pages point at it, and a tripwire test
  (`cmd/mast/shared_token_test.go`) fails the day the daemon learns to issue
  discriminating tokens, naming the pages that must change with it. The
  multi-token daemon-side form is still open and is not freeze-exposed:
  these are environment variables, which the v1.0 promise does not cover
  ([#389](https://github.com/go-steer/mast/issues/389)).

- The pre-v1.0 decision sweep is settled and written down: AG-UI's
  concurrent-run policy (`agui.run_queue: {depth: 3}`, refused with a `409`
  before the stream opens) and the client-declared-tool gate (a bundle
  envelope that bounds the class rather than naming tools, mutating by
  default, root agent only, with an effect record marked as attested rather
  than observed). Two CLI decisions go with them —
  `mast sessions pause --interrupt` becomes `--cancel-turn` (shipped, above),
  and `--attach-listen` will imply `~/.mast/sessions.db`.
  `docs/ag-ui-design.md` open questions 4 and 6 are resolved, and the docs
  site now states both current limitations plainly rather than leaving a
  reader to discover them ([#98](https://github.com/go-steer/mast/issues/98),
  [#329](https://github.com/go-steer/mast/issues/329),
  [#337](https://github.com/go-steer/mast/issues/337),
  [#384](https://github.com/go-steer/mast/issues/384)).

## v0.9.0 (2026-09-17)

*The surfaces stop answering a question nobody asked.*

v0.8 was the release where claims the project had been making turned out not
to hold. v0.9 is the consequence: almost every entry below is a surface that
was reachable, authenticated and green, and that could not tell a caller the
one thing the caller needed. `GET /perms` returned 200 and an empty body on
every mast daemon. A session parked on a human approval reported `idle`, the
same string an idle session reports. `/agui/agents.json` advertised no
capabilities, so a client could not tell a workload that publishes no
reasoning from a model that did not think. No HTTP surface could show the
change an operator was being asked to approve — so a chat bot could offer
"approve the thing, whatever it is", which is uninformed consent with an audit
trail.

None of those were outages, and that is the point: **a surface that answers
confidently with nothing in it is worse than one that refuses**, because
nothing upstream can tell the difference. Where a route could not honour its
contract it now refuses rather than shrugs.

The second thread is latent correctness — three defects that had never once
misbehaved in production. Every outward text path concatenated the model's
thinking into its answer, and leaked nothing only because the default model
returns a signed thinking block with an empty body. The only thinking config
mast could send was rejected by that same model, and the unit test that let it
ship asserted what mast *sends* rather than what the model *accepts*, so it
passed for exactly as long as every live call failed. An AG-UI thread had no
owner, so any authenticated principal could read and continue another's
conversation. Each of these was one vendor's implementation detail away from
being real.

**Two behaviour changes to plan for, neither of them an API break.** An
authenticated AG-UI thread now folds the caller's identity into its session
id, so threads opened before this release are not reachable after it. And a
client disconnect no longer cancels the run it was watching — which is the fix
to a defect that re-fired mutating tool calls, and which also means hanging up
now stops nothing: a run with no reader is bounded by its wallclock budget,
the watchdog, and an explicit `mast sessions pause <id> --interrupt`.

**One breaking change, scheduled a release in advance.** `.tmpl` specialist
files no longer load; v0.8 renamed them to `<name>.specialist.md` and accepted
the old extension for one release with a startup warning. A bundle that
renamed its files in v0.8 needs nothing.

This is the last release before v1.0, which is the API freeze and carries no
other claim. `pkg/transcript`'s coverage of `pkg/approval` and the
compatibility policy below are both freeze preparation: what is frozen is now
checked by a test rather than asserted by a document.

- **An AG-UI client hanging up no longer destroys the run it was watching.**
  ([#383](https://github.com/go-steer/mast/issues/383)) The turn ran on the
  HTTP request's context, so a TCP reset, a closed laptop or a proxy timeout
  cancelled it. That is worse than losing the output: if the drop landed while
  a mutating tool was executing — after the effect, before its
  `FunctionResponse` was persisted — the session kept no record of the call,
  so the retry a *well-behaved* client makes re-applied the change. Measured
  at 1 call versus 2 on harnesses differing only in cancel timing.

  The turn now runs on `context.WithoutCancel` of the request: it keeps the
  request's values, including trace context, and loses only its cancellation.
  A frame that cannot be written **retires the stream** and the turn carries
  on to completion and to the transcript, which is `pkg/attach`'s posture for
  the same hazard — drop the subscriber, not the publisher. The stalled-reader
  case the old coupling was reaching for is handled where it belongs, at the
  per-frame write deadline, and a retired stream stops arming new ones instead
  of blocking each subsequent emit on a dead socket.

  **Behaviour change worth planning for: nothing transport-side cancels a turn
  any more.** A run whose client vanished is bounded by
  `budget.max_wallclock_seconds`, the watchdog, and an explicit
  `mast sessions pause <id> --interrupt` — the same three that bound a
  scheduled run with no client at all. AG-UI has no "stop" verb on mast yet;
  when a client needs to mean that, that is the shape to add. Reconnecting to
  a stream you dropped is still unbuilt and still v1.1; what a disconnect
  costs today is the tail of the event stream, not the work.

- **The AG-UI surface's remaining open calls are settled, and two of them were
  settled against the code rather than for it.**
  ([#98](https://github.com/go-steer/mast/issues/98)) Docs and decisions only —
  no behaviour changes in this entry. Six deferred slices were triaged against
  one question: *does this need a `workload.Bundle` key, and does it hold a
  defect today?* A key is exposed by the v1.0 freeze; a defect is not a feature
  and does not wait for one.

  Two recorded promises turned out to describe something the code does not do,
  and both are corrected in place rather than quietly dropped:

  - **A client disconnect does not resume a run — it destroys one.** The design
    text said mast reconnects via the same `threadID` + `runID` and resumes
    from the last durable event. In fact the stream context descends from the
    request, so a TCP reset cancels the turn; if that lands while a mutating
    tool is running, the session keeps no trace of the call and a well-behaved
    retry re-applies the change. Filed as
    [#383](https://github.com/go-steer/mast/issues/383) and **fixed in this
    release** (entry above), because it is a correctness fix, not the reconnect
    feature it was filed under. Reconnect proper — a replay cursor plus
    re-subscription — is v1.1, and is cheap to defer *because* #383 landed
    first.
  - **There is no concurrent-run policy.** The doc records a bounded queue of
    depth 3 with a `RunError` refusal and a metric; none of it exists. A second
    run blocks on the per-session turn lock, unbounded, and the only ceiling is
    `budget.max_wallclock_seconds` — so a caller queued behind a long turn gets
    a timeout that says nothing about why it waited. Filed as
    [#384](https://github.com/go-steer/mast/issues/384). The **key shape** is
    due before v1.0 even though the implementation may slip.

  **Client-declared tools (`RunAgentInput.tools`) were re-biased rather than
  implemented.** The recorded plan was to intersect them with the bundle's
  `tool_catalog` allowlist. That object is not an allowlist — it is a per-tool
  policy-override table (mutation class, capture/revert) that grants nothing —
  and the real per-specialist allowlist narrows *wired* toolsets: built-in tools
  the daemon constructed and MCP servers it dialed. A browser-executed tool is
  in neither and never can be, so the intersection is the empty set for every
  input and a bundle that opted in would have found no client tool callable. The
  question underneath is the one worth answering — what does it mean to allow a
  tool whose behaviour lives on the other side of the wire, when the mutation
  predicate, write gate, effect outbox and capture/revert all read a tool mast
  can describe? The gate gets designed and its key shape decided before v1.0;
  the implementation ships in v1.1. `tools` stays parsed-and-dropped until then.

  Also settled: **the protocol-version-pinning open question is struck, not
  answered** — every clause of it names an SDK that the hand-rolled,
  zero-dependency decision removed, so the `pkg/agui/VERSION.md` it called for
  would record the version of a dependency mast does not have. The **`agui://`
  federation client is deferred past v1.0** for want of a consumer. And the
  doc's version-keyed phasing table is **struck through rather than re-dated**,
  since a lapsed schedule read as a commitment is the failure shape
  [#300](https://github.com/go-steer/mast/issues/300) and
  [#291](https://github.com/go-steer/mast/issues/291) each already corrected.

- **An AG-UI thread now belongs to the caller who opened it.**
  ([#382](https://github.com/go-steer/mast/issues/382)) On an authenticated
  endpoint, any principal carrying the workload's scopes could previously
  continue — and read back — another principal's conversation by naming its
  `threadId`. Nothing had to be guessed: `threadId` is the client's own
  correlation string, not a secret, and under the default `per_thread` session
  model the thread *is* the durable session. The measured probe had a second
  authenticated caller recover the first one's secret verbatim over SSE.

  The cause was a gap between two questions that look like one. The server
  authenticated a caller and scope-checked it, then discarded it: `RunInput`
  carried no principal, so nothing downstream could bind a thread to a subject.
  Scope-checking answers *may this caller run this workload*. It is not an
  answer to *is this conversation yours*.

  **The fix is structural rather than a refusal.** The caller's tenant and
  subject are hashed to a fixed-width tag and folded into the derived session
  id, so two principals naming the same `threadId` address two different
  sessions. A foreign caller is not denied — they get a thread of their own and
  never reach the first. That is the same choice that already makes the session
  id daemon-derived instead of client-supplied, and it means there is no owner
  record to store or migrate, and no 403-vs-404 disclosure question to settle.
  The identity is hashed and not interpolated so a variable-width subject next
  to an attacker-chosen `threadId` cannot be made to collide across the
  delimiter.

  **Upgrade note.** Session ids on an authenticated AG-UI endpoint change
  shape, so a thread opened before this release is not reachable after it: its
  history stays durable but a new one starts. Endpoints with **no** validator
  have no subject to own anything and keep exactly the ids they had — their
  sessions carry over unchanged.

- **`agent.NewEchoModel` and `agent.NewToolActorModel` keep emitting plain
  text, and that is now a decision rather than an oversight.**
  ([#372](https://github.com/go-steer/mast/issues/372)) Neither offline fake
  produces a thinking block, which is a shape every live frontier model
  produces. Teaching them to was measured twice and detects nothing the
  per-site tests added in #370 do not already detect, so an embedder's fixtures
  that pin the current replies keep working. A test that needs a reasoning
  model should script a local double, the way `cmd/mast` already does to drive
  `agui.emit_reasoning` end to end. Recorded alongside it: **the v1.0 API
  freeze covers a fake's signature, not the content it emits** — a double
  stands in for a real model, and real models change what they return.

- **An AG-UI run now shows which agent is working.**
  ([#98](https://github.com/go-steer/mast/issues/98)) The stream brackets each
  stretch of a run in `STEP_STARTED` / `STEP_FINISHED` frames, and **the step
  name is the agent that authored it**. A single-agent workload gets one
  bracket around the whole run; a coordinator that hands off to a specialist
  gets one per handoff, so a chat UI can render "log-analyzer is working"
  without parsing tool calls. The bracket closes before the terminal frame on
  every disposition, including an abort and a HITL pause — a dangling
  `STEP_STARTED` outlives the run in a client that tracks it.

  **The design text's unit was wrong and had to be replaced rather than
  implemented.** It proposed `stepName: "turn-N"`. An AG-UI run drives exactly
  one turn through the shared chokepoint, so every run of every workload would
  have reported `turn-1` — a frame carrying no information at all. Authorship
  is what actually varies inside a run.

  **There is no bundle key and no `capabilities` bit**, which inverts the two
  publication surfaces that shipped before it in v0.9. Those are opt-in because
  they disclose something a client could not otherwise see. A step does not: a
  handoff already reaches the same stream as a `transfer_to_agent` /
  `invoke_specialist` tool call naming the same agent. A switch with nothing
  behind it is a bundle field that can be set wrong, and since #302 a
  misspelling is fatal at load.

  **One thing steps do not cover**, stated because the shape of the gap matters:
  a **planner** dispatch runs its specialist under a private runner whose events
  never reach the stream this emitter reads. Coordinator and graph dispatch
  never had that gap, which is why their handoffs bracket. So a planner
  workload's client sees the `invoke_specialist` tool call and nothing inside
  it, and the `ACTIVITY_*` frames that would fill it stay deferred — blocked on
  a publication decision about a specialist's interior reaching a browser, not
  on scheduling.

- **`/agui/agents.json` now says what a run on each workload will actually
  publish.** ([#377](https://github.com/go-steer/mast/issues/377)) Every
  descriptor carries a `capabilities` object:

  ```json
  "capabilities": {
    "state_delta": true,
    "state_keys": ["plan", "phase"],
    "reasoning": false
  }
  ```

  The two v0.9 bundle keys that opt a workload into publishing —
  `agui.state_projection` and `agui.emit_reasoning` — left a client with no way
  to know either one before starting a run, and no reliable way after: a stream
  with no reasoning in it looks the same whether the workload publishes none or
  the model didn't think. They are per-bundle, so `protocol_version` cannot
  carry them.

  - The bools are **never elided when `false`**. An absent key means a server
    older than this object; an explicit `false` is a promise that the frame
    family will not appear, and a client has to tell those apart.
  - `state_keys` is in the order the bundle declares, so a client can lay out
    panels before the first patch, and is omitted whenever `state_delta` is
    `false` rather than describing a channel that cannot open.
  - The values come from the same value the run's emitter is built from, via a
    new optional `agui.CapabilityReporter` on the `Backend`, rather than from a
    second read of the bundle beside the first. A capability claim that
    restates a config instead of reading what it describes is how `/tools`
    ([#364](https://github.com/go-steer/mast/issues/364)) and `/perms`
    ([#375](https://github.com/go-steer/mast/issues/375)) each spent releases
    advertising something untrue.
  - The endpoint **stays unauthenticated**, now as a decision rather than an
    inherited default: the descriptor may state what a client permitted to run
    would observe in the stream anyway, and may not state a property of the
    governance around the stream. That is why there is no HITL bit.

- **`GET /perms` stops describing every mast daemon as one that gates
  nothing.** ([#375](https://github.com/go-steer/mast/issues/375)) The route
  answered `200` with `{"mode":""}` on every release since it was ported. That
  is not an empty answer — it is a well-formed description of a daemon that
  permits everything and has adjudicated nothing, which is the opposite of the
  `require_approval` default, and a client cannot tell it from the truth.

  It now **refuses** when nothing is wired behind it, gated on a new `perms`
  capability flag (separate from `perms_stream`: a daemon can report its rules
  without streaming a live question, and mast is that daemon). The advertised
  bit and the route's behaviour are the same computation, so they cannot
  disagree.

  On a mast daemon it now answers, and carries more than it used to:

  - **`mode` and `on_mutation` are separate axes.** `on_mutation` is the
    bundle's `hitl.on_mutation`; `mode` describes the permissions gate that
    adjudicates an unparked call, and is **absent** under `apply` and
    `dry_run`, where mast builds no gate. Absent means "no gate", not
    "permissive". Reporting the write-gate policy as a permissions mode would
    have been the same class of lie this fixes.
  - **`approvals` is the session's durable decision log**, which until now no
    HTTP surface exposed at all — only `mast sessions export-decisions`, which
    a UI cannot reach. It is read per session and survives a restart.
  - Each row's `decision` follows what the gate **did**, not what the operator
    answered, because those come apart on a verdict mast refuses and on a call
    that fires under an earlier change-set grant. `refusal` keeps "a person
    said no" distinct from "mast refused the person's answer"; `change_set`
    marks a row nobody was asked about; `approver` names who answered; and
    `key` is the call that ran, which on an edit is the operator's version.

  **Breaking, narrowly:** a client that treated `200 {"mode":""}` as a valid
  answer from a mast daemon now gets `501`. There is no such client that was
  right.

- **An AG-UI client can watch the model think — if the workload says so.**
  ([#98](https://github.com/go-steer/mast/issues/98), resolving
  `docs/ag-ui-design.md` OQ 5) `agui.emit_reasoning: true` on a bundle streams
  that workload's reasoning as AG-UI's `REASONING_START` /
  `REASONING_MESSAGE_START` / `…CONTENT` / `…END` / `REASONING_END` frames,
  emitted before the answer's `TextMessage` triad because that is the order the
  provider streamed the parts in. It copies `agui.state_projection`'s shape
  deliberately — per bundle, off by default, logged at startup — so there is
  one scheme to learn rather than two.

  **Three things are not inherited:**

  - **Off emits nothing at all**, not an empty phase bracket and not a
    redaction marker. The existence of a thought is itself a disclosure: a
    bracket with no content tells a client the model deliberated and was
    censored.
  - **The opt-in is a named read, not a removed filter.** Publication calls a
    new `modeltext.Thought` predicate beside `modeltext.Text`, and the two are
    disjoint — so the eight surfaces
    [#370](https://github.com/go-steer/mast/issues/370) fixed are unchanged
    whether this is on or off. A thought still never becomes answer text and
    never becomes `RunFinished.result`, and *"reasoning reached a browser"*
    stays a grep for one symbol rather than an audit of every place a part's
    `Text` field is read.
  - **The thought signature has no frame under any setting.** It is a provider
    replay credential — Anthropic wants it back verbatim on the assistant turn
    before a `tool_result` — not a thought, so handing it to a browser
    publishes a token. AG-UI's sixth reasoning event,
    `ReasoningEncryptedValue`, is therefore deliberately unmodeled, and a
    thinking block with a signature and no prose emits nothing.

  The startup line is `WARN` where the projection's is `INFO`: a state
  allowlist is a list someone curated, and this is an unbounded channel from
  the model's private working-out.

  **Expect an opted-in workload to be quiet for now.** Under the request mast
  sends today `claude-opus-5` returns a thinking block whose body is empty and
  whose payload is all signature — #370's finding read from the other side.
  The key is real; the stream fills when the request asks for
  `display: summarized`.

  Activity events (`StepStarted`, `ActivitySnapshot`) were coupled with
  reasoning in #98's issue text and are **not** in this change. They are a
  different vocabulary with no sensitivity argument attached, and their
  planner-step half needs a seam the emitter does not have yet. *(The
  `StepStarted` half shipped separately later in this release; the
  `ActivitySnapshot` half is still out, and the seam is now named — see the
  step-events entry above.)*

- **Approving a parked mutation from a chat button works, and the parity
  scoreboard reads 19 of 19.**
  ([#364](https://github.com/go-steer/mast/issues/364)) switchboard forwards a
  button press as `POST /sessions/<app>/<id>/perms/respond`. Every mast daemon
  answered that **501**, so the last red parity row was mast's — re-attributed
  on 2026-09-14 after both repos had spent five weeks each passing its own
  tests against a different endpoint.

  **The issue offered three options and the answer is a fourth.** "Wire it" —
  `gate.SetPrompter(broker)`, which is what core-agent does — is inert here:
  mast's live gate entry point is `CheckMutatingToolCall`, which never prompts,
  it returns `ErrApprovalRequired` and the write gate parks the call. Nothing
  in this module reaches `AskApproval`, so a broker behind it would have had no
  callers rather than no subscribers. "Retire it" was wrong for a simpler
  reason: mast is not un-interactive, and its interactive surface already
  exists — the operator listener, where `POST /resume` answers a durable park.
  The route was not missing a mechanism; it was pointed at the wrong one.

  So the two prompt handlers now depend on an interface, `attach.PermsSource`.
  `PromptBroker` is one implementation — core-agent's blocking in-process
  prompt, unchanged, still available to an embedder with a terminal — and
  `cmd/mast` supplies the other, which projects open write-gate parks onto
  `/perms/stream` and answers a press by taking the same resume path an
  operator takes. **A client cannot tell which is underneath**, which is the
  deliverable.

  Three things a client sees:

  - **`decision` admits `deny` and `allow-once` only.** The four broader values
    are refused **400, by name**, rather than narrowed. This is not new policy
    — it is `RecordMutationVerdict`'s existing rule moved to where the operator
    can see it — and it is 400 rather than 403 because the operator *is*
    allowed to answer; the fix belongs in the client's button set.
  - **The ack names the approver.** `RespondPrompt` returns the identity the
    source recorded and the route echoes it, omitted when the source recorded
    nobody. A `PromptBroker` honestly records no one; a durable answer records
    the authenticated caller ([#194](https://github.com/go-steer/mast/issues/194)).
  - **`perms_stream` became exact.** The capability probe was a type assertion,
    so a registrant whose method returned nil advertised routes it 501s — and
    that frame is what a client reads to decide whether to render an approval
    UI at all.

  The frame's `kind` is `control_plane_write`, and that string is load-bearing
  rather than descriptive: switchboard derives the offered buttons from it,
  mapping that kind to exactly deny/allow-once and an **unrecognised** kind to
  all six. An invented kind would have put four buttons in front of an operator
  that this daemon answers 400.

  **The reusable lesson is about the test, not the route.**
  [#248](https://github.com/go-steer/mast/pull/248) filed a wire-contract test
  as this repo's accountability for the row and pinned the verdict vocabulary
  on `POST /resume` — which switchboard's approval client never calls.
  `pkg/attach/permswire_test.go` is the same discipline aimed at
  `/perms/{stream,respond}`: route paths, the `prompt` SSE event name, the
  frame and ack JSON keys, and the decision and kind vocabularies, every one a
  literal. A contract pin is worth what the other side actually calls.

  Stated rather than rounded off: **no live switchboard→mast press has been
  executed.** Agreement was established by reading the counterpart's call
  sites. The first real press is the confirmation.

- **The only thinking config mast could send was rejected by its own default
  model.** ([#369](https://github.com/go-steer/mast/issues/369)) A caller who
  asked `claude-opus-5` for a thinking budget got a **400, not a degraded
  turn**: Anthropic replaced the budget-carrying `thinking.type=enabled` with
  `thinking.type=adaptive` at the 4-6 generation and removed the old one at
  4-7, and `enabled` was the only shape `pkg/providers/anthropic` could build.
  mast's own `DefaultModel` is `claude-opus-5`.

  The two shapes are mutually exclusive and the split runs *inside* one
  provider, so mast now picks by model ID, measured per model rather than
  assumed — one live request per cell against Vertex `global`,
  anthropic-sdk-go v1.43.0:

  | model | `enabled` | `adaptive` |
  | --- | --- | --- |
  | `claude-haiku-4-5`, `claude-opus-4-5` | accepted | 400 |
  | `claude-opus-4-6` | accepted, deprecated | accepted |
  | `claude-opus-4-7`, `-4-8`, `claude-opus-5`, `claude-sonnet-5` | 400 | accepted |

  An **unknown** model takes `adaptive`. The migration runs one way, so the
  legacy list is closed and guessing the older shape for a model released
  tomorrow guesses wrong.

  **A budget of zero now disables thinking, which it never did.** It is the
  one thing `genai.ThinkingConfig` can say that means "do not think", and mast
  sent no thinking parameter at all — measured, `claude-opus-5` then returns a
  signed thinking block anyway. `thinking.type=disabled` is accepted by every
  model in the table above.

  **`IncludeThoughts` stops being a no-op** on adaptive models: it selects
  `display`, defaulting to `omitted`. The signature still comes back so a tool
  loop still replays, but mast does not pay for reasoning text that
  [#370](https://github.com/go-steer/mast/issues/370) filters out of every
  surface it has. Asking for it is the caller's explicit decision.

  Deliberately unused: `output_config.effort`, the field Anthropic's own 400
  message recommends. The set of legal bands is itself per-model
  (`claude-opus-4-6` rejects `xhigh`, `claude-haiku-4-5` rejects the parameter
  outright), so mapping a token budget onto a band would trade a one-bit table
  for a three-axis one and add a new way to 400. A budget is honoured exactly
  where the API can express it and read as "think" where it cannot.

  Nothing in mast sets `ThinkingConfig`, so this is reachable only by a
  library embedder building the `model.LLMRequest` themselves — it was still
  shipped code that could not succeed. **The test that let it ship is the
  reusable part**: `convert_test.go` asserted that a budget produced the
  `enabled` shape, a statement about what mast sends and never about what the
  model accepts, so it passed on every run while every live call failed.

- **A model's thinking no longer counts as its answer.**
  ([#370](https://github.com/go-steer/mast/issues/370)) Frontier providers
  return reasoning as a text part flagged `Thought` — Anthropic's thinking
  block, Gemini's thought part — in the same field as the answer, marked
  rather than separated. mast made that distinction in exactly one place, the
  stall detector in `pkg/agent`, and omitted it in eight others: the A2A
  result artifact and its streaming progress frames, the AG-UI `TextMessage`
  stream and `RunFinished.result`, `mast run`'s printed answer, the specialist
  result the planner reads back from a dispatch, the daemon's own operator
  log, and the eval trace's `FinalText`. Each of the eight tested
  `part.Text != ""`, which is true of a thinking block.

  **Nothing leaked, and that was luck rather than design.** Measured against
  live `claude-opus-5` on Vertex: under the request mast sends today the model
  returns a thinking block with a signature and an *empty* body, so the
  emptiness check skipped it by accident. The same model asked with
  `{"type":"adaptive","display":"summarized"}` returns the reasoning text
  populated. Three unrelated changes would have turned the omission into a
  disclosure — fixing the thinking config mast sends
  ([#369](https://github.com/go-steer/mast/issues/369)), adding a reasoning
  key to the bundle, or a vendor flipping a default — and none of them would
  have touched the eight lines or failed a test.

  All nine reads now go through one predicate, `internal/modeltext.Text`,
  which reports the caller-facing text of a part and whether there is any, so
  "the model said nothing" and "the model thought something I must not repeat"
  stay distinguishable at the call site. Reasoning is still replayed to the
  provider intact — a signed thinking block has to come back for the model to
  continue — and a redacted thinking block, which carries no text at all,
  reads as not-caller-facing rather than as an empty answer. This is the floor
  the planned per-bundle `agui.emit_reasoning` opt-in opts in *from*: a surface
  that publishes reasoning should do it by reading `Thought` parts
  deliberately, not by failing to filter them.

- **An out-of-process caller can now read the change it is being asked to
  approve.** `GET /parks` lists every parked session, oldest question first;
  `GET /parks/{session}` answers for one session whether it is parked or not
  ([#314](https://github.com/go-steer/mast/issues/314)). Both are on the
  inject listener, authenticated exactly like `POST /resume`, and both need
  `--session-db`. `POST /resume` has been able to *answer* a parked approval
  since v0.2; nothing outside the process could find out what it was
  answering, which reduces every chat or web approval to "approve the thing,
  whatever it is" — uninformed consent with an audit trail, which is worse
  than the CLI it replaces because it looks like the opposite. An approval
  park now carries the tool, the arguments, the rendered key, the gate policy
  that parked it, the proposing specialist, the verdict format the gate will
  accept, a staleness line when a grant no longer covers the call, and the
  change set the call belongs to. Alongside them: an operator hold if one is
  active, reported beside the parks and never as one, and the calls that
  already ran with a revert the gate captured (#296) — the first time
  anything outside the process could read one.

  **What the projection leaves behind is as much of the contract as what it
  carries**: no model output, no tool results — including the result of the
  read that produced a capture, where only the read's name, its digest and
  the declared field names travel, because the values are live cluster state
  and this body goes to chat relays — and nothing the write gate did not park
  on. `pkg/inject` declares the JSON shape and imports neither
  `pkg/transcript` nor `pkg/approval`, so `cmd/mast/parks.go` is the single
  place both vocabularies are in scope and a field added to the transcript
  cannot reach a client without a line someone wrote. Two answers are
  deliberately distinguishable: a session that exists and is not parked is
  200 with an empty list, and a daemon with no session store is 404 rather
  than an empty list — "nothing to approve" and "I cannot see parks" are
  different answers and only one of them means it is safe to stop looking.

- **A session waiting on a human now says so on the wire.** The attach
  `turn_state` reports `awaiting_permission` when the write gate has parked a
  mutating call and `awaiting_elicit` when the session asked a question
  ([#313](https://github.com/go-steer/mast/issues/313)). Both constants had
  been declared since attach mode shipped and neither had ever been produced:
  a parked session reported `idle`, the same string a session that finished
  its work reports, so a gateway had no frame to hang an approval card on.
  This is the last mast-side blocker on the parity board's one remaining red
  row (in-chat Approve/Reject, via
  [switchboard#84](https://github.com/go-steer/switchboard/issues/84)).

  The reason the bug survived four releases is that a parked session **is**
  finished, as far as the daemon can see — the gate records the question
  durably and returns rather than blocking inside the call. So the answer is
  read from the transcript, not from in-memory turn state, which has two
  consequences worth stating: it survives a restart, and it is in the boot
  snapshot every client gets on connect, so a client attaching an hour after
  the park sees the park. Two pauses are deliberately still `idle`: a
  `pause_session` hold, which is something an operator placed rather than a
  question, and the resume turn itself, where a live turn outranks the
  interrupt it is on its way to resolve.

- **`turn-complete` no longer overtakes the answer it terminates.** The
  terminal frame went straight to subscribers the instant the turn returned,
  while the turn's final text travelled event log → pump → fan-out, so a
  client that finalized its render on `turn-complete` could drop the last
  message ([#327](https://github.com/go-steer/mast/issues/327)). The frame is
  now held until the log has caught up to every subscriber. Every failure
  path degrades rather than hangs — no queryable log, an empty log, a query
  error, or a two-second timeout all release the frame and log the reason,
  because a late frame is a correctness bug and a missing one is worse.

  Shipped with #313 deliberately: both defects live in the same eleven lines
  of `pkg/attachadapter`, and `docs/sibling-sync.md` had recorded that doing
  one alone would mean touching those lines twice. Along the way #327 gave
  `pkg/eventlog`'s `LatestSeq` its first caller — its doc comment had named
  one for four releases and never had it.

- **An AG-UI client can now watch named session-state keys change, and by
  default it still sees none of them.** A workload's bundle declares
  `agui.state_projection: [plan, phase]`; a run's write to a named key is
  published as an AG-UI `StateDelta` carrying RFC 6902 patches
  (`{"op": "add", "path": "/plan", "value": …}`, keys sorted). This resolves
  open question 7 in `docs/ag-ui-design.md`
  ([#98](https://github.com/go-steer/mast/issues/98)), and closes the gap
  where `StateDelta` was an exported wire type mast never once emitted.

  The default is empty and that is the load-bearing part: session state is
  whatever the runtime put there — graph node results, judge verdicts,
  approval grants, captured change sets — and the AG-UI client is a browser.
  A key the list does not name produces **no operation at all**, not a
  redacted one, so a client cannot learn that it changed. An empty or
  duplicated entry is refused before the listener binds; the enabled list is
  logged at startup so the published surface can be read off the log rather
  than off the bundle you believe is mounted. See the new blast-radius
  section in [the threat model](https://go-steer.github.io/mast/reference/threat-model/).

  Two corrections ride along, both of them documentation that had drifted
  from the code. The `agui:` example in `docs/ag-ui-design.md` carried
  `streaming:` and `activity_events:` keys — neither field exists, and since
  [#302](https://github.com/go-steer/mast/issues/302) made an unrecognised
  bundle key a load error, a bundle copied from that example would not have
  started the daemon. They are removed, and a test now fails if either is
  ever implemented without the docs being restored. Separately, the doc's
  deferred list now says out loud that `RunAgentInput.tools` is parsed and
  then dropped rather than reaching the run.

- **The parity scoreboard reads 18 of 19, and the row still red is mast's.**
  No code change: the board was re-measured against the sibling repo rather
  than against this repo's notes about it
  ([#242](https://github.com/go-steer/mast/issues/242)). Row 17, the approver
  allowlist, shipped in switchboard on 2026-09-03 and is green — a press is
  refused against a per-channel list of asserted callers before the prompt is
  even located, and a list that would match nobody is refused at startup.

  Row 16, in-chat Approve/Reject, was recorded in six places as switchboard's
  to write. switchboard wrote it. **It answers `POST
  /sessions/<app>/<id>/perms/respond`, and a mast daemon returns 501 there** —
  no type in this module implements the `/perms` capabilities outside a test,
  because `internal/compose` builds the write gate with no prompter, which is
  the right posture for a substrate whose premise is that nobody is watching.
  So **approving a parked mutation from a chat button does not work today**;
  approvals continue to work through `mast sessions` and an authenticated
  `POST /resume`, unchanged. What mast does with the inherited `/perms`
  surface is [#364](https://github.com/go-steer/mast/issues/364); making the
  durable park visible to a gateway is
  [#313](https://github.com/go-steer/mast/issues/313).

  Worth naming, because it is the reason this went five weeks unnoticed: this
  repo's stated accountability for row 16 was the wire-contract test added in
  [#248](https://github.com/go-steer/mast/pull/248), and it pins the verdict
  vocabulary on `/resume` — an endpoint switchboard's approval client never
  calls. Both repos built a coherent half against a different route and each
  half passed its own tests.

- **`cmd/mast`'s daemon entry points take options structs instead of flat
  parameter lists.** No behaviour, flag or exit-code change — `serve()` took
  16 positional parameters, 11 of them consecutive strings, so the listener,
  session-DB and model arguments could be transposed at the call site and
  still compile ([#293](https://github.com/go-steer/mast/issues/293)). They
  are now `workloadOpts` / `modelOpts` / `listenOpts` / `sessionOpts` /
  `resumeOpts`, and the nine turn dependencies that `a2aBackend`,
  `aguiBackend` and `autoResumer` each held a private copy of are one
  embedded `turnDeps`. Across the seven functions touched: 91 parameters
  down to 41.

  The convention is shared with core-agent
  ([#685](https://github.com/go-steer/core-agent/issues/685)) and recorded in
  [`docs/sibling-sync.md`](docs/sibling-sync.md); the structs themselves are
  deliberately not shared, because the two signatures have exactly one
  parameter name in common and it means different things in each. It ships
  with `cmd/mast/paramgroup_test.go`, which fails if one of those entry
  points regrows a flat list — a convention nothing measures is not a
  convention.

- **mast has a threat model.**
  [`docs/threat-model.md`](docs/threat-model.md) and the [reference
  page](https://go-steer.github.io/mast/reference/threat-model/) collect the
  trust boundaries, the adversaries, the controls and the accepted risks that
  had been reasoned out across eight releases and written down in a YAML
  comment, five composition-time checks and half a dozen issues
  ([#305](https://github.com/go-steer/mast/issues/305)). No behaviour changes.
  It is a collection, not an audit, and mast has had no external security
  review — the document says so.

  The parts worth reading before you deploy:

  - **mast does not defend against prompt injection, and the write gate is
    the defence.** The boundary is placed after the model rather than before
    it: a successful injection produces a mutating tool call, and under the
    default policy that call parks for a human. There is no input filter and
    there will not be one. Two consequences are stated plainly — wherever the
    gate does not reach, injection reaches your infrastructure, and a
    read-only agent can still exfiltrate through a call's arguments.
  - **The workload bundle is a control-plane input, not configuration.**
    `tool_catalog.tools[].mutating` is consulted before mast's own
    classification, so a bundle can reclassify a mutating tool as read-only
    and take it out of the gate's scope. Write access to the bundle — the
    mounted ConfigMap, on GKE — is equivalent to write access to whatever the
    workload can reach. Engine control-flow calls are the unreclassifiable
    floor under this.
  - **Two defaults are named as accepted risks**: a bundle with no `budget`
    block has no ceiling, and the default watchdog posture (`feedback`) tells
    the model about a tool loop rather than stopping it. Only `enforce`
    cancels a turn.
  - **mast gates on verb and nothing gates on scope.** No ceiling bounds how
    many objects one approved call changes.
  - **Attribution is opt-in.** Without `MAST_INJECT_USERS_FILE`, an approval
    is recorded against `shared-bearer-token` rather than a person.
  - **The inject endpoint does not refuse an unauthenticated non-loopback
    bind**, unlike the attach, A2A and AG-UI surfaces, and it defaults to
    `:7777` on all interfaces. Every shipped deployment manifest sets
    `MAST_INJECT_TOKEN`; a bare `mast serve` does not.
    ([#361](https://github.com/go-steer/mast/issues/361) — changing it is a
    breaking CLI change, so it is filed rather than fixed here.)
  - **`docs/orchestration-design.md`'s classifier-first threat model is
    annotated as unenforced**, because that path is not implemented and
    neither is its `allowed_bundles` allowlist. The constraints are correct
    and are prerequisites if it is ever built.

- **mast has a compatibility and deprecation policy, in force from v1.0.**
  [`docs/compatibility-policy.md`](docs/compatibility-policy.md) and the
  [reference page](https://go-steer.github.io/mast/reference/compatibility/)
  say what counts as breaking, how long a deprecation is visible before the
  release that removes it, how one is announced, and how long a release is
  supported ([#304](https://github.com/go-steer/mast/issues/304)). The whole
  process until now was a single sentence written in 2026-07-01 that never
  said any of those things.

  The parts most likely to affect you:

  - **Two released minors *and* 90 days** between a `// Deprecated:` marker
    and the major that removes the symbol. Both floors — mast's fastest
    minor-to-minor gap has been three days, so a count on its own is a cycle
    nobody is awake for.
  - **A default that changes what your workload does is breaking**, even
    though nothing fails to compile. So is **validation getting stricter on a
    file that used to load**, with one fenced carve-out for a control that was
    accepted and then not applied.
  - **The support window is the current release.** mast does not backport to a
    previous minor; it never has. When v2.0 ships, `release/v1` gets security
    and correctness fixes for six months.
  - **A mast major triggered by an ADK major removes nothing** — that release
    keeps its promise that the migration is an import path and nothing else.
  - **Metric names have no compatibility rule.** The stability page said they
    had "their own compatibility rules", which implied a process that does not
    exist; it now says what is true.

  Enforced, not only written: `deprecation_test.go` at the module root fails a
  `Deprecated:` marker that does not name the release removing it. A
  deprecation with no end date is a permanent second way to do something.

- **Removed: `attach.EmitTarget`**, the pre-#506 spelling of
  `attach.OperatorEventTarget`, and the broadcaster fallback that probed it.
  This is the new policy's first application and the only marker in the tree
  it had to judge: an inherited deprecation, in an unsupported package, with
  no end date, carried for eight releases — while `docs/fork-design.md`
  claimed mast did not carry it at all.

  Not breaking for any mast consumer. The 2026-07-29 port pinned core-agent at
  a SHA chosen to include #519, so `OperatorEventTarget` has been the primary
  name in every mast release; the only program the fallback could have served
  is one implementing core-agent's old `SetAttachEmitter` method against
  *mast's* attach package. `pkg/attach` is not a covered path in any case.

- **The v1.0 promise now says which `pkg/approval` types it covers, and a test
  keeps the list honest.** `pkg/transcript` hands you `approval.Decision`,
  `approval.AppliedEdit` and `approval.CaptureRecord`, so those are frozen at
  v1.0 whether or not a document says so — a freeze is transitive through
  exported signatures. They are named as covered **by reference** rather than
  copied into `pkg/transcript`, because you only ever receive them and their
  shape is already public as the `mast.decision/v1` JSON that
  `mast sessions export-decisions` emits; a Go copy would be a second name for
  one schema ([#338](https://github.com/go-steer/mast/issues/338) task 2).

  Eight declarations are covered, not the three: the four enums
  `approval.Outcome`, `Scope`, `Authority` and `Disposition` come with
  `Decision`'s fields, and `approval.ProposedChange` comes with
  `CaptureRecord.Revert`. That last one is a correction to the issue's own
  table, found by following the fields. Everything else in `pkg/approval` —
  the write gate plugin, grants, the encoders — stays unsupported.

  No API changed. What is new is `pkg/transcript/freeze_test.go`, which walks
  the package's exported declarations, closes over their field types and fails
  on a leak the list does not name — because adding one otherwise compiles and
  passes every behavioural test.

  Also corrected: the uncovered-package list said "the other 27 packages under
  `pkg/`" over 28 names, and one of those names was `providers`, which holds
  no `.go` files and cannot be imported at all, while the five real packages
  beneath it were named by nothing. The honest figures are **5 covered and 32
  not**, and `providers/anthropic`, `providers/gemini`, `providers/mock`,
  `providers/usage` and `providers/vertexcache` are now listed individually.

- **Breaking: `.tmpl` specialist files no longer load.** v0.8.0 renamed them to
  `<name>.specialist.md` and accepted the old extension for one release with a
  startup warning; that window is now closed
  ([#349](https://github.com/go-steer/mast/issues/349), the removal
  [#292](https://github.com/go-steer/mast/issues/292) scheduled). This is a
  breaking change to a **file convention**, not to an API: no Go signature you
  call changed, and a bundle that renamed its files in v0.8 needs nothing.

  A directory holding one fails to load, naming every offending file:

  ```
  specialists: 2 file(s) in .agents/specialists use the .tmpl extension,
  which loaded with a warning in v0.8 and was removed in v0.9:
  Evicted.tmpl, OOMKilled.tmpl
  	rename each to <name>.specialist.md — the stem is the specialist name
  	your workload.yaml already lists, so renaming the files is the whole
  	migration
  	see https://github.com/go-steer/mast/issues/292
  ```

  Migration is the same one-liner as in v0.8; nothing inside the files
  changes, and the specialist name is the stem either way:

  ```sh
  cd .agents/specialists
  for f in *.tmpl; do mv "$f" "${f%.tmpl}.specialist.md"; done
  ```

  **The file is refused rather than ignored, deliberately.** mast's loader
  skips what it does not recognise, so simply dropping the extension would
  have made a `.tmpl` an *absent* specialist instead of a rejected one. The
  error you would get is `references specialist "OOMKilled" not found in
  .agents/specialists` — about a missing file, for a file sitting in that
  directory, spelled correctly — and a specialist your `workload.yaml` does
  not name by hand would have disappeared from the roster with nothing said at
  all. If you are upgrading v0.7 → v0.9 directly you never saw the v0.8
  warning; this message is what you get instead.

  Also removed, and breaking for an embedder that used them:
  `specialists.LegacyExtension`, `specialists.WarnLegacyExtension` and the
  `Spec.LegacyExtension` field. All three existed only to let a caller place
  the deprecation warning; there is no warning to place. `pkg/specialists` is
  one of the six packages the v1.0 freeze covers
  ([#300](https://github.com/go-steer/mast/issues/300)), which is why this
  removal is happening now rather than after it.

- **Anthropic cache writes are billed at their own rate, and a long-running
  session's reported cost goes up.** mast reads token counts through Google
  GenAI's usage record, which has a cache-*read* bucket and no cache-*write*
  bucket — so the Anthropic adapter folded `cache_creation_input_tokens` into
  the prompt count and the meter billed them at 1x instead of 1.25x. This has
  been true since caching landed and is worst exactly where it is least
  expected: the turn that warms a cache is the turn that pays for it. Measured
  rather than estimated — a 28,804-token warm on `claude-sonnet-5` billed
  **$0.057668 against a rate card $0.072070**, a fifth of the turn
  ([#352](https://github.com/go-steer/mast/issues/352)).

  Provider adapters now attach a normalized usage record beside the GenAI one
  (`pkg/providers/usage.Detail`) carrying cache reads, cache writes, reasoning
  and tool-use tokens plus the model the backend says it ran, and the meter
  prices from it when present. Every count is a pointer: a reported zero and a
  bucket the provider never mentioned are different claims and must not
  serialize the same way. Gemini stamps the record too, though nothing prices
  off it yet.

  **What this changes for you:** nothing you configure, and one number. A
  `max_cost_usd` ceiling on a cache-heavy Claude workload now trips where it
  should have been tripping all along, which is sooner than it did in v0.8.0;
  the spend ledger and `/usage` cost figure move the same way. Cache *hits*,
  the steady-state case, are unchanged. An embedder that implements
  `budget.Pricer` itself should handle the new `Call.CacheWriteTokens` field —
  ignoring it reproduces the old under-billing, which is why this landed before
  the v1.0 freeze rather than after it.

  Not fixed here: `/usage` still reports turns and cost only. The eight token
  fields `attach.UsageTotals` declares have never been populated by anything —
  the tracker was never ported from core-agent — so the zero-vs-unreported
  distinction is visible on the event log and not in the operator projection.
  Filed as [#356](https://github.com/go-steer/mast/issues/356).

- **A candidate model is measured on a schedule, and a baseline now has to have
  measured the same thing.** v0.8.0's deferral holds the gemini `frontier`
  default at `gemini-3.7-flash` until the judged corpus has been run against
  `gemini-3.8-flash` — which left the condition resting on someone remembering
  to run it. `evals-candidate-gemini.yml` runs it weekly instead: the same 31
  scenarios against the candidate, graded by the incumbent so the model under
  test is the only variable moving, diffed against the nightly's own board. It
  reports and never gates. The other half of the bar is the outcome tier on the
  same model, which has to be started by hand — `GITHUB_TOKEN` cannot fire a
  `workflow_dispatch`.

  That comparison exposed something the nightlies already had wrong.
  `dev/ci/evals-nightly-baseline.sh` took the *newest successful run* of a
  workflow as its baseline, and both nightlies accept a `model` dispatch input
  whose board uploads under the same artifact name — so one hand-run against
  another model became the next night's baseline. The numbers were never
  falsified: the delta compares the model pair recorded on both boards and
  captions a mismatch "the scores below are not comparable, they are two
  different measurements". What it cost was the trend, which is what a nightly
  is for. The lookup now walks back through the last few successful runs and
  takes the first board whose recorded model *and* grader match this run's,
  which also covers a run that skipped unconfigured and an artifact that aged
  out while an older one survived. A deliberate cross-model baseline is
  requested explicitly with `MAST_EVALS_BASELINE_MODEL`; the candidate workflow
  is the one caller that wants one.

  **Both of these were written on 2026-09-10 and neither was on `main` until
  now** ([#354](https://github.com/go-steer/mast/issues/354)). The pull request
  that carried them was stacked on another, that base was squash-merged into
  `main` 37 minutes before the child merged *into the base*, and the child's
  merge therefore landed on an orphaned branch. GitHub reports it as merged;
  the trunk never had it. So the candidate workflow has never run once, and
  every nightly since 2026-09-10 has been exposed to the baseline bug above.
  Nothing in v0.8.0's notes claimed either, so the shipped release was not
  false — but a merged badge over absent content is a failure mode no check in
  this repo can see, and the remedy is procedural: **merge a stack one pull
  request at a time, bottom-up, re-targeting each child onto `main` first.**

## v0.8.0 (2026-09-13)

*What was quietly not true is now refused.*

v0.7 added capability — a change that carries a route back, and a gate a real
model can red. v0.8 adds very little. It is the release where a run of things
the project had been asserting turned out not to hold, and each one is now
enforced, corrected, or written down honestly.

The pattern is worth naming, because it is why none of these were caught by a
test. Each was a *claim* rather than a code path: a provider default nobody
had read, a bundle key nobody had misspelled yet, two package names nobody had
resolved, a file extension nobody had questioned, a version string a green
lint had been reading past for three releases. Tests exercise what the code
does. None of these were about what the code does.

**Three breaking changes, and the first one is silent.** A Gemini workload
that had been getting grounded web search stops getting it, and does not
error — it answers worse. Read that entry before upgrading. The other two fail
loudly: an unrecognised key in `workload.yaml` is now a load error, and
`budget.Limits.Catalog` no longer exists. Separately, `.tmpl` specialist files
still load with a deprecation warning, and stop loading in v0.9.

Four corrections in this release changed no code and so have no entry below,
but they are the same pass and are on the docs site now. mast no longer claims
the library gets the daemon's subsystems — it gets the governance layer, while
everything that starts a turn with nobody calling is `package main`.
"Multi-provider" is stated as two vendors and four deployment paths rather
than implying six by counting a fake and a cache as peers. The deployment
ambition is settled as *someone else installs this*, with the four unpaid
bills listed on the roadmap rather than left to be discovered. And `docs-lint`
now reads version claims — the rule whose absence let the install page serve
v0.4.0 download URLs, and a green check, for three releases.

- **The provider's server-side built-in tools are now off by default, on every
  provider, and a bundle turns them on with `builtin_tools:`.** Breaking, and
  deliberately so: every Gemini model mast constructed was wrapped with
  `GoogleSearch: true, URLContext: true` and no config key reached it, so a
  workload that wants grounding must now ask for it
  ([#324](https://github.com/go-steer/mast/issues/324)).

  ```yaml
  builtin_tools:
    web_search: true      # gemini: google_search   anthropic: web_search
    url_context: true     # gemini only
    code_execution: false # gemini only
  ```

  What made this worth breaking is that these tools are invisible to every
  control mast has. A built-in runs inside the vendor's infrastructure and its
  result arrives folded into the response, so it never becomes a tool call: the
  permissions gate has nothing to allow, the write gate nothing to park, the
  effect outbox nothing to record, and the transcript shows a model that simply
  knew something. A specialist declared `read_only` — a declaration
  `CheckCapabilitySplit` verifies at startup and [#295](https://github.com/go-steer/mast/issues/295)
  turned into a runtime measurement — could read the public internet, and the
  four releases of hardening in front of it were all looking the other way.

  **mast's baseline is off, not each vendor's baseline**, and that is the part
  upstream did not do. Gemini ships search and URL context on; Anthropic ships
  search off. Inheriting them would mean one mast image changing an unattended
  agent's reach with a `--provider` flag, against the premise that the same
  bundle runs on either. It also makes the paths with no bundle to read — `mast
  run`, `mast.Run`, the eval rigs — safe by construction rather than by
  remembering to write a key. `pkg/providers/gemini.DefaultBuiltinTools()` is
  unchanged and still on: it is a recommendation to a library caller wrapping a
  Gemini model directly, and `internal/compose` no longer passes it.

  The keys are tri-state (`*bool`). Absent means mast's default; an explicit
  `false` records that somebody decided. The gate is per bundle — a specialist's
  `model:` override resolves through the same gate the root does, so a workload
  that turned search off cannot have it handed back by an analyst on another
  tier. There is deliberately no per-specialist axis; under a default-off
  baseline there is nothing for a specialist to reclaim.

  The daemon now logs what the **constructed model** will send —
  `model constructed name=gemini-3.7-flash builtin_tools=web_search` — rather
  than what the bundle said. YAML decoding does not reject unknown keys, so
  `builtin_tols:` is discarded in silence and a line derived from the same
  struct would have agreed with the typo. `builtin_tools=none` means the
  provider has these tools and all of them are off; no field at all means the
  backend has no such concept (the offline fakes). Ported from core-agent
  `cbcb624`, with the default flip and the dropped per-specialist axis as
  mast's own divergences.

- **`workload.yaml` carries a `schema_version:`, and an unrecognised key is now
  a load error rather than silence.** The bundle is edited by operators who
  never import Go, so it versions on its own clock rather than mast's
  ([#302](https://github.com/go-steer/mast/issues/302)).

  ```yaml
  schema_version: 1
  name: gke-triage
  ```

  **Absent means 1**, so every bundle written before the key existed loads
  unchanged and there is no migration. A bundle declaring a version this
  binary does not speak is refused, naming both numbers — the operator
  holding the file needs to know which of the two to change. An explicit
  `schema_version: 0` is refused too: absent is a file written before the key
  existed, a literal `0` is someone reaching for a version number and getting
  it wrong, and reading the second as the first would hide the mistake. A
  version bump is reserved for a key that changes **shape or meaning**, not
  for a key being added — `builtin_tools:` landed in this same release and the
  schema stayed at 1.

  **Breaking, deliberately: unknown keys are refused, not warned about.** This
  is the only file in a deployment that can declare a tool safe to run without
  an operator, and mast's own predicate is default-deny-unknown, so the
  failure being designed against is a misspelled block — `tool_catalogue:`,
  `hitl_polciy:` — that leaves a section of policy unapplied while the daemon
  logs a clean start and the workload runs all night. A `WARN` at boot is not
  a control. Strictness reaches nested keys, where the sharper version of the
  bug lives: `mutatin: true` on a catalog entry leaves the tool catalogued, so
  nothing looks missing, while it quietly reverts to default-deny-unknown.

  Decoding is **two passes and the order is the point**. A real bundle from
  the future does not arrive carrying only a version bump; it arrives carrying
  the keys that motivated it. The version is read first, alone, so the report
  is "this bundle is from a newer schema" and not "field `quarantine_policy`
  not found", which reads as a typo in a key copied out of correct
  documentation.

  **Specialist frontmatter is strict too, and deliberately takes no version of
  its own.** The reason it is strict is `tools:`, the one key that fails in the
  unsafe direction: `ToolAllowlist.InheritsAllMCP()` is `MCP == nil`, so a
  misspelled `toosl:` does not narrow the specialist to the servers its file
  lists — it hands over every MCP server the workload wires. (`capability:` is
  the milder case; absent resolves to `read_only`, so a typo fails safe.) It
  gets no `schema_version` because a specialist is only ever reached through a
  bundle that names it, so the bundle's version already governs the roster,
  and a second independently-versioned artifact would multiply the
  compatibility matrix across 39 mostly-prose files.

  This inverts one behaviour shipped days earlier in the same release:
  [#324](https://github.com/go-steer/mast/issues/324)'s startup summary reads
  the *constructed* provider precisely because an unknown `builtin_tools:` key
  used to be dropped in silence. That summary still earns its place — it
  catches the correctly-spelled key whose credential or provider cannot
  satisfy it, which no amount of decoder strictness can see.

  All 6 shipped bundles and all 39 shipped `.tmpl` files were checked against
  the strict decoder before it was adopted, and both checks are pinned as
  tests.

- **v1.0 now has a definition: it is the API freeze, and nothing else.**
  Restarting version numbers at v0.1.0 is what dropped the stability promises
  mast inherited when it forked from core-agent. v1.0 is the release that
  makes them again, and the number will carry no other claim
  ([#300](https://github.com/go-steer/mast/issues/300)).

  **Six import paths** follow semver from v1.0 — the module root,
  `pkg/agent`, `pkg/transcript`, `pkg/workload`, `pkg/specialists` and
  `pkg/budget` — with the other 27 packages under `pkg/` **named
  individually** as unsupported rather than described as a remainder, because
  a reader cannot check a remainder against their own import list. `cmd/mast`
  is in the promise too — flag names, subcommand verbs, and exit codes `0`
  through `3` — since its callers are shell scripts, systemd units and
  manifests, none of which a compiler can warn. That half is a check rather
  than a sentence: `cmd/mast/testdata/cli-surface.txt` pins the surface and
  deliberately excludes `--help` prose, so wording fixes do not churn the
  contract.

  Writing this down meant correcting what the corpus had been promising since
  2026-07-25, which was stability for "the five packages the four pillars
  stand on". **Two of those five have never existed.** There is no
  `pkg/provider` and no `pkg/tool`; the real provider extension point is
  `mast.Config.Model`, a *field* typed on ADK's `model.LLM`. And the mechanism
  that decision named for everything else — a `// Experimental: API may change
  without deprecation cycle` marker — was never once written into a `.go` file
  across seven releases. So for seven releases the answer to "what is not
  covered" had no enforcement and no readers. A named list in
  [`DESIGN.md`](./DESIGN.md#the-v10-stability-promise) replaces it, and
  [`docs/library-api-design.md`](./docs/library-api-design.md)'s surface table
  is marked superseded rather than patched — every one of its 18 rows is
  missing the `pkg/` prefix, so none of the paths resolve.

  **What v1.0 will not mean is production-ready.** The threat model
  ([#305](https://github.com/go-steer/mast/issues/305)) does not exist and the
  deprecation policy ([#304](https://github.com/go-steer/mast/issues/304)) is
  a gate on the tag rather than a follow-up to it. Two clocks stay separate
  from the Go one: wire contracts (attach, A2A, AG-UI, inject) and the bundle
  schema version, feature-detected by clients no Go compiler sees. One thing
  is not a policy choice at all — an ADK major can only ship as a mast major,
  because `mast.Config` exposes `model.LLM` and `session.Service` and Go's
  semantic import versioning makes `adk/v3/model.LLM` a different type from
  the v2 one. Such a release will carry no other breaking changes.

- **Specialist files are now `<name>.specialist.md`, not `<name>.tmpl`.** They
  are YAML frontmatter plus a Markdown body and have never been Go templates:
  `text/template` is imported by exactly one package in the module
  (`pkg/planner`, for the planner's instruction) and it reads none of them
  ([#292](https://github.com/go-steer/mast/issues/292)).

  The old name was not just inaccurate. A `{{ ... }}` in a body is ADK's
  placeholder syntax, not the author's, and
  [#272](https://github.com/go-steer/mast/issues/272) had to add a load-time
  refusal for it — a defect an author is invited to write by an extension
  that tells them they are writing a template.

  **`.tmpl` still loads.** mast warns at startup, naming each file and the
  spelling to move to. To migrate, rename; nothing inside the files changes,
  and the specialist name is the stem either way:

  ```sh
  cd .agents/specialists
  for f in *.tmpl; do mv "$f" "${f%.tmpl}.specialist.md"; done
  ```

  One specialist present under **both** extensions is a load error rather
  than a silent pick: mid-rename the leftover is usually the stale copy, and
  choosing it would mean edits to the renamed file quietly do nothing.

  Support for `.tmpl` is removed in v0.9
  ([#349](https://github.com/go-steer/mast/issues/349)).

- **`budget.Limits.Catalog` is now `budget.Limits.Pricer`, a one-method
  interface the meter owns.** Breaking, for anyone who set the field directly;
  the daemon and `mast.RunWorkload` are unaffected because both get their
  pricing from `internal/compose`. Migration: implement
  `PriceCall(backend, modelID string, c budget.Call) (float64, bool)` over
  whatever you were pricing with, or drop the field and set `RatePer1K`.

  The v1.0 promise ([#300](https://github.com/go-steer/mast/issues/300)) covers
  `pkg/budget` and does not cover `pkg/pricing`, which is not a coherent pair
  while `Limits` names `*pricing.Catalog` in an exported field a consumer has to
  construct: freezing the meter would have frozen `NewCatalog`, `Options`' three
  config-discovery fields, `ModelRates` and `Rates` along with it — around
  fourteen declarations, none of them about budgets. Worse, the only one the
  meter used is the one already scheduled to change: `docs/model-support-design.md`
  M2 owes `LookupFor` a re-key from the backend name onto a provider profile, so
  the frozen surface would have made mast's third backend
  ([#312](https://github.com/go-steer/mast/issues/312)) a major release.

  `Call` is a struct rather than three ints for the same reason — reasoning
  tokens and cache-*write* tokens are both on the list, and each arrives as a
  field instead of as a new signature. Nothing about the metered figure changes:
  the meter still derives the buckets from the provider's counters, clamps an
  over-reported cache count, and folds thinking tokens into output, and
  `internal/compose` holds the only adapter onto the builtin catalog. What
  changes is that `pkg/budget` now imports **nothing else in this module**, and
  the tests split along the same seam — the meter's own tests price against an
  explicit rate table and assert the derivation, while the adapter's assert the
  catalog's real rates.

  `Limits.IsZero` replaces two `== (budget.Limits{})` comparisons. Neither could
  have panicked — Go short-circuits an interface comparison when the dynamic
  types differ, and the zero value's pricer is always nil — but `==` on two
  `Limits` that both carry an uncomparable pricer does panic, and an operator
  that works only against one specific operand is not a property a caller should
  have to know.

  The other leak, `pkg/transcript`'s three `approval` records, gets the opposite
  remedy and is tracked in
  [#338](https://github.com/go-steer/mast/issues/338): they are outputs whose
  field set is already committed as `mast.decision/v1`, so a Go copy would be a
  second spelling of one JSON schema.

- **`gemini-3.8-flash` is priced and classified; the frontier default stays at
  `gemini-3.7-flash`.** The weekly regen picked up `gemini-3.8-flash` (and
  `claude-mythos-5-1`) from LiteLLM. 3.8-flash costs exactly what 3.7-flash
  costs — $0.75 / $3.75 per MTok, cached input $0.075 — on the same
  1,048,576-token window, so promoting it to the gemini/vertex `frontier`
  default would move no cost ceiling and no compaction trigger. That is the
  whole reason it was not promoted: with nothing measurable to weigh, the only
  argument left is that the id is newer, and next door that argument shipped an
  un-run frontier default (core-agent#579) that was reverted the following day
  when the parent agent stopped mid-plan. The bar is the one 3.7-flash cleared —
  the 31-scenario judged corpus over live Vertex, within noise of the current
  board, no mid-plan stall — and `--model gemini-3.8-flash` is how it gets run.
  Zero-config workloads are unaffected.

  `modeltier.Classify` learns the model now rather than at promotion, because
  the companion tables fail *open*: an operator who pins an id no table knows
  gets the universal 0.85 compaction threshold instead of their tier's, with
  nothing said. The deferral itself is recorded as data naming one successor, so
  a `gemini-3.9-flash` reds the build rather than inheriting an argument nobody
  made for it.

  One rate that is not what it looks like: 3.8-flash *arrived* at a promotional
  price rather than moving onto one, so no diff anywhere shows a change, and it
  **doubles to $1.50 / $7.50 on 2027-01-01** along with 3.6- and 3.7-flash. That
  is now in `introductoryRates`, verified against Google's dated pricing page.
  Size any 2027 ceiling against the later number.

## v0.7.0 (2026-09-06)

*A change mast makes carries a route back, the write gate's question is a
measurement, and a real model's behaviour can red the build.*

v0.6 closed the distance between what a bundle promised an operator and what
the runtime enforced. v0.7 is about the two things still taken on trust after
that: that a change could be walked back, and that any of it holds up with a
real model driving.

An external assessment triaged on 2026-09-04 had a headline finding that was
already stale by a day. The finding underneath it was not: mast could act and
could not undo. A mutating call now records what it overwrote, and the call
that puts it back, *before* it fires — and deliberately never fires that call,
because an automatic rollback is a mutating call nobody approved.

The other half is measurement. Every eval tier mast had measured mast: recorded
fixtures, or a judged nightly that scores a real model and gates nothing. The
new O tier puts a real model on a real cluster on every pull request and reds
it, a release now refuses a commit that tier has not passed, and the write
gate's park became readable to a grader along the way — a run where the gate
asked and a run where it never did used to project identically.

The parity scoreboard is unchanged at **17 of 19**, and both rows still red are
switchboard's to write.

**A change mast makes now carries a route back.** Through v0.6 the sequence
was propose → approve → act → record that it acted, which leaves an operator
who approves a change and watches it make things worse with no path back
except rebuilding the old state by hand under time pressure. A tool in the
workload's catalog can now declare a `capture:` block — a read-only tool
that records the target's prior state, the fields to keep, and optionally
the call that puts them back. The write gate runs that read **before** the
call fires and writes what it found, plus the proposed revert, into the
session's durable log; `mast sessions show` prints the old values and the
exact call and arguments that restore them.

Three things it deliberately does not do. mast does not **derive the read**
— which tool reads the object a write is about is domain knowledge, and the
same neutrality that keeps a Kubernetes schema out of mast keeps one out of
this. It does not **derive the inverse**: `scale_deployment` inverts by
re-scaling, `delete_pod` does not invert at all, and a patch inverts only
over the fields it touched, so a workload declares `revert:` or declares
nothing and gets a record whose undo is marked undeclared. And it does not
**fire the inverse** — the recorded revert is a proposal that goes back
through the same gate with a person answering, because an automatic rollback
is a mutating call nobody approved.

The capture covers all four paths a mutating call can take (`apply`, an
approved verdict, an edited verdict — recorded against the arguments the
operator actually ran — and a change-set grant being spent, captured before
the grant is marked consumed). It is **fail-closed**: everything happens
before the forward call, so a read that fails or a declaration that cannot
be honored refuses the call while nothing has happened yet. Six declaration
mistakes are refused before run time, including a capture whose read is the
changing tool itself, a **mutating** capture read (it would write to the
cluster before every gated call, unapproved and unrecorded, in the name of
reversibility), a **read-only** revert (offered as a way back, does nothing),
and a revert whose arguments all come from the change — that one re-applies
the change instead of undoing it, and is shaped closely enough like an undo
to be believed during an incident. A deployment that declares captures but
cannot run reads still starts and warns, since silence there teaches a
bundle author that their changes became reversible.

Restore stays out by decision, not by sequencing: mast can render the call
but cannot decide that firing it an hour later is still right
([#296](https://github.com/go-steer/mast/issues/296)).

**The write gate's record of what it asked is now readable as a
measurement.** Everything needed was already durable — a park writes the
gated call's name and arguments into the session event log, and the
decision that answers it rides the event that re-fires the call — but
nothing could ask for it: the eval trace treats the confirmation call as
engine control flow, so a run where the gate asked and a run where it never
did projected identically. A gated call now carries its question and its
answer, and the outcome tier gains an `approval_requested` check that reds a
workload that mutated without parking.

It reads the **question**, never the verdict, including on a call the
operator refused: the claim is that the change was put to a person, not that
they allowed it, and in a test the answer comes from the harness — a check
reading it would be asserting that the test rig ran. It compares arguments
rather than counting parks, because a gate that asks about one call and runs
another is not a gate. A call authorized by a change-set grant passes with
no question of its own, provided the set the operator approved lists it.

The public surface gains one field: `approval.Parked.CallID`, the id of the
call a pending confirmation is about, which is the key everything else joins
on ([#295](https://github.com/go-steer/mast/issues/295)).

**A real model's behaviour can now red the build.** Every eval tier mast had
measured mast: the deterministic E tier asserts against recorded fixtures, and
the judged J tier scores a real model but reports and gates nothing, on
purpose. So nothing in CI ever asked whether an agent built on mast solves the
incident it is pointed at, and every claim about agent behaviour rested on
someone running a workload by hand and being convinced.

The new **O — outcome** tier runs a real model against a real workload on a
real `kind` cluster, on every pull request, and reds it. Three crash-loop
cases from the triage corpus — an evidence chain, a misleading symptom, and a
root-cause analysis — five repetitions each, graded by verifiers that read the
run's tool calls, its final report, and the cluster's own state before and
after. Five repetitions because the distinction between an agent that
diagnoses an OOM three times in five and one that does it five in five is the
whole product, and a single-shot suite cannot see it.

Four rungs, in descending order of how little argument they admit: a
catastrophic safeguard violated in any single repetition, which demotion never
reaches; a required check that turned out **vacuous**; every repetition of a
case failing; and a case that ran fewer times than it should have — reported
ahead of the failure rung and suppressing it, because *0 of 5 ran* and *5 of 5
failed* send a reader to different places. An errored run counts as a failed
repetition and does not red on its own, since a provider timeout is not a
regression in mast.

A check that measured nothing is a red, not a pass. A forbidden-phrases check
passes hardest against the run that produced no report at all, and an intent
check where none of the tools called are in the intent table graded the tool
surface rather than the run. Both directions of a vacuous check are recorded
the same way, because the passing direction is the one nobody investigates.

The wall clock was budgeted before the roster rather than after — a sibling
project that admitted thirteen cases demoted two and deleted one within 72
hours while its ceiling went 85 → 360 minutes in nine days. The ceiling here
is **20 minutes** for the whole pass, expressed as a deadline the runner checks
between cases rather than as a job timeout: a deadline yields a short board
that reds under rung 4, a timeout yields a cancelled job and no board. A pass
measures 2m47s on local kind and 2m48s on a GitHub-hosted runner, so the
ceiling is roughly 7× the measurement on the machine that actually gates, and
admitting a fourth case has to argue for raising it in a reviewable diff. A
flaky case is demoted off the blocking roster by committed diff carrying the
date and the measurement — never deleted, never by runtime flag, and it keeps
running and reporting.

Two costs stated rather than left to be discovered. An unconfigured run
**fails** rather than skips, the opposite of the judged nightly, because for a
gate *green because it could not run* is exactly the rung that cannot fire; the
consequence is that a pull request from a fork cannot run the tier, so a
maintainer runs it on a branch here before merging one
([#294](https://github.com/go-steer/mast/issues/294),
[#297](https://github.com/go-steer/mast/issues/297)).

**A release now refuses a commit the outcome tier has not passed.** The O
tier reds a pull request but does not block it, and deliberately stays that
way — a fork cannot run it, so requiring it on `main` would make a fork's pull
request unmergeable forever. So the claim that a real model's behaviour can
stop bad code had a gap at the end of it: a tag.

The release workflow's first step now reads the `outcome` check run for the
SHA the tag points at and refuses on anything except success, *including on
its absence*, which is what a rung that cannot fire looks like at the one
moment it matters most. Gating there rather than on the merge is the stronger
of the two: it survives an admin merge, it costs a fork nothing, and it is a
statement about the artifact being published rather than about the process
that produced it — the same reasoning as the published-notes assertion beside
it, which exists because six releases composed correct notes and published
empty bodies. The accepted cost is that a red can land on `main` and the
refusal arrives at the tag instead. The dry run is gated too, so `gh workflow
run release.yml -f dry_run=true` is a full rehearsal.

**A metered cost is now the price of the call that was made.** Two defects, in
opposite directions, both in the number a `max_cost_usd` ceiling is compared
against — and v0.6's pre-call check is what made them matter, because a price
mast merely *reports* can be approximately right and a price a refusal is
computed from cannot.

`budget.Limits.Catalog` has been the exact-pricing path since it landed and
nothing had ever assigned it. All three construction sites built a `Limits`
with a flat blended rate and no catalog, so every session in the product
metered at the rate the catalog exists to replace. Installing it alone would
not have been enough, for a reason invisible from the calling side: the lookup
keyed off `session.Event.ModelVersion`, and ADK's streaming aggregator rebuilds
the final response — the only one carrying usage — without that field. A
catalog reached from the event alone would have missed on every Gemini call,
fallen back to the flat rate, and climbed `Unpriced` while looking configured.
That is the third place this field has turned out not to be there. The resolved
model name is now carried on `Limits` beside the catalog it is a key for, with
the backend next to it, resolved by the same function the client is built from
— so the price and the call cannot disagree about who is billing. The event is
still asked first, because a server echo names what was actually billed.
Measured against a live GKE triage workload on `gemini-3.7-flash`: the same
session priced **$1.13 flat against $0.19 exact**.

**Thinking tokens are output, and are billed as output.** Gemini reports
thoughts in their own counter, additive to the candidates rather than inside
them, and Google bills that bucket at the output rate; mast passed only the
candidate count, so on a reasoning model most of the output was free. A live
GKE triage run spent 6,449 thinking tokens against 1,180 candidate tokens — 85%
of billable output and about a third of the session's true cost, undercounted
in the direction a ceiling cannot afford, since a `max_cost_usd` set from a
figure that wrong lets a session run past the number the operator wrote down.
Anthropic's `output_tokens` already includes thinking and mast's Anthropic
provider never sets the field, so the sum is correct for both rather than
double-counting one.

The two move the meter in opposite directions and do not cancel: one removes a
5.9× overcharge, the other adds back the ~36% that overcharge was masking. Both
had to land before the number meant anything
([#266](https://github.com/go-steer/mast/issues/266),
[#267](https://github.com/go-steer/mast/issues/267)).

**The cluster read/write split now bounds the path mast actually uses, and
the narrowed IAM binding is the default.** `WRITE_SCOPE=namespaced` shipped
opt-in for four releases because nobody had run it against a live GKE
cluster. Running it produced the expected answer and an unexpected reason it
had been out of reach: GKE does not resolve the Workload Identity Federation
principal to the KSA's RBAC ServiceAccount subject. The API server sees an
RBAC **`User`** named `serviceAccount:<project>.svc.id.goog[<ns>/<ksa>]`, so
the shipped bindings granted the GKE MCP path — the one the agent's tools take
— nothing at all. Under the old `roles/container.admin` default that was
invisible, because IAM allowed every write anyway and the RBAC files read as
if they were the boundary; under the narrowing it would have made the daemon
read-only everywhere.

Both bindings now name both subjects, `setup-wif.sh` defaults to
`roles/container.viewer`, and `scripts/rbac-matrix.sh` runs its 20 cells once
per username and refuses to report green without `PROJECT_ID` — measuring only
the in-cluster path is worse than measuring nothing, since it goes green on a
cluster where mast cannot write. Measured 41/41 against the rendered shipped
manifests on live GKE: patch allowed in the remediable namespace, refused one
namespace over, Deployment delete refused, cluster-wide secret list refused
([#290](https://github.com/go-steer/mast/issues/290)).

*Upgrading:* substitute your project ID for `REPLACE_ME_PROJECT` and re-apply
`deploy/base` and `deploy/remediation-target`, or the MCP path stays unbound.
Re-running `setup-wif.sh` adds bindings and never removes one, so an existing
`roles/container.admin` needs an explicit `gcloud projects
remove-iam-policy-binding`; the matrix fails a cell until it is gone.

**The daemon now says which configuration it is running, and says when the
files on disk stop matching it.** mast reads its bundle once, at startup, and
reloads nothing — but in Kubernetes the ConfigMap is mounted as a whole
volume, so the kubelet rewrites those files under the running pod, and the
deployment deliberately keeps a stable ConfigMap name so an apply does not
roll the daemon. An edit could therefore land on disk and change nothing, with
no log line an operator could grep to find out. Startup now logs a
`workload config identity` line — the root, the bundle, and a digest over
exactly the files the loaders read — and once a minute the daemon re-hashes
them and warns, once per edit, naming what changed and that a restart is what
picks it up. It never reloads: what a mid-flight turn, a parked approval or an
armed cadence should do when the bundle changes underneath them is a design
question, and this is a diagnosis
([#289](https://github.com/go-steer/mast/issues/289)).

**A specialist's `tools.mcp` allowlist is now checked for existence.** It is
applied by dropping what does not match, so a name that matches nothing was
never an error — it was a capability quietly missing from a specialist whose
own file said it had one, and the first symptom was a model behaving oddly
mid-incident. The two halves are answered where each honestly can be. A
**server** the workload's `tool_catalog.mcp` does not declare now fails the
roster at startup, naming the specialist, the name, and the servers that do
exist — the catalog is already loaded and already validated, so the check
reaches no network. A **tool** the server does not serve needs a
`tools/list`, and mast's toolsets are lazy so a bundle still loads when a
server is down; that one is reported once, at WARN, the first time the
specialist's toolset lists. Naming fewer tools than a server offers is the
point of an allowlist and stays silent; only naming more is the mistake.
Found by porting a pair of deployed Python agents whose allowlists carried
eleven names no endpoint served — `list_datasets` for `list_dataset_ids`,
`query` for `execute_sql_readonly`, and five networking tools on a server
with no networking tools at all
([#278](https://github.com/go-steer/mast/issues/278)).

**A declared `edge_trigger.http.path` no longer answers 405.** The field is
accepted, validated, and read by nothing — the inject server's routes are
fixed and consult no bundle — which the field's own definition has always
said, and which from outside was indistinguishable from a field that works.
`POST /alert` on a workload declaring that path returned `405 Method Not
Allowed`, because `GET /` claimed the path and rejected the verb, so the
answer read as a client mistake and the debugging went there. Now: a bundle
declaring a path is warned about at startup next to the write gate and
watchdog posture, an unmatched path answers `404` listing the routes the
daemon actually serves, and a known path with the wrong verb answers `405`
naming the verb it wants. A misdirected `GET /resume` used to be absorbed by
the health route and answered `200 ok`; it is now a `405`. The declaration
is warned about rather than refused — it has been inert since the spike, and
per-workload path prefixes stay deferred; what changed is the honesty of the
answer, not the routing
([#277](https://github.com/go-steer/mast/issues/277)).

**A `{placeholder}` in a specialist's prompt body is a session-state lookup,
and now says so at load rather than at 3am.** ADK resolves every `{...}` in
an instruction before the prompt is sent: a bare identifier is a state key,
an `artifact.`-prefixed one is an artifact load. A template that says
"investigate `{project}`" was therefore asking for state nothing had set,
and the run died with `state key does not exist` — naming neither the
template, nor the line, nor the key. Nothing in a `.tmpl` file suggests
braces are syntax, and a prompt full of Kubernetes and GCP examples is
exactly where braces live.

Loading a specialist now refuses those templates, naming the file, every
offending line, and the key each one looks up. Only what ADK actually
resolves is refused, so manifests and jsonpath keep loading:
`{"replicas":1}`, `{.status.phase}` and `{app: web}` are all literal, while
`{app:web}` — the same line without the space — is a lookup of the
`app`-scoped key `web`. Doubling the braces is not an escape, `{{project}}`
being trimmed to the same key; the error says so, and points at `<project>`
for a literal and `{project?}` for a state lookup that cannot fail a run. An
optional marker does not rescue `{artifact.x?}`, which fails for want of an
artifact service mast does not run
([#272](https://github.com/go-steer/mast/issues/272)).

**A specialist stopped by a budget ceiling can now file what it already
found.** New bundle knob, `budget.final_report` (default off). Until now a
refused specialist returned nothing at all, which is the right answer for one
refused on its first call — nothing was looked at, and mast will not invent a
finding — and the wrong one for a diagnoser stopped on its twelfth turn after
six log queries and a quarter of a million tokens. The tokens were spent
either way and the incident got an unresolved delegation. With the flag on,
such a specialist gets exactly one more model call with every tool but its
report tool withdrawn, and an instruction to report what it can support and
say what it did not reach. mast synthesizes nothing: the model writes the
report, in its own output schema. Bounded three ways — once per specialist per
session, opt-in, and never granted to a specialist that has spent nothing —
and each grant is logged at WARN so the one-call overshoot is announced
([#271](https://github.com/go-steer/mast/issues/271)).

**`retrieve_raw` no longer parks a read at the write gate.** It is mast's
own builtin, registered whenever the MCP digest wrap is on, and it appears
in no server's `tools/list` — so an enumerated `tool_catalog` had no reason
to classify it and default-deny-unknown made it mutating. With
`on_mutation: require_approval` that meant the first digested response sent
the model to `retrieve_raw` and the write gate stopped a read, mid-diagnosis,
under an approval question naming a tool the operator had never declared. On
a real cluster one Deployment read is around 19 kB against an 8000-byte
threshold, so it fired on the first incident, on every shipped example.
mast's builtins now carry their own class: `retrieve_raw` reads mast's
scratch store, reaches no server, and classifies read-only. A
`tool_catalog.tools` override still outranks it, so a workload that wants it
gated can gate it ([#270](https://github.com/go-steer/mast/issues/270)).

**A change executor under `dispatch: graph` can now actually make the write
an operator approved.** With `hitl.require_approval` and
`hitl.on_mutation: require_approval` both on, one turn raised two parks: the
write gate held the mutating call, and the executor's own node raised a
result-approval interrupt on top of it — over a result that did not exist
yet (`Result: <nil>`). Answering either one stranded the other, and the
session went idle having changed nothing. The failure direction was safe;
the remediation half of a triage-then-fix workload was not available at all.

Two things were wrong and both are fixed. `workflow.RunNode` reports a child
that parked exactly as it reports a child that finished with no output, so
the node read a park as a return; it now runs children with
`WithRaiseOnWait` and parks behind the child's question rather than adding
one of its own. And a confirmation resume is not a workflow resume — it
carries no `RequestInput` response, so the graph re-enters at `Start` as a
fresh run with no resumed inputs — which meant a finding gate answered on an
earlier turn was unanswered again one turn later, re-running its specialist
and re-parking in front of the write the operator had just confirmed.
Verdicts are now recorded in session state, the same durability the
dispatched route already had.

The change executor also no longer raises a result-approval gate of its own
under `require_approval`. Its verdict was discarded on every path that could
receive it, and it was asked after the calls had already been made, each
past the write gate — a question with no consequence is worse than no
question, because an operator answering it believes they are deciding
something. A single-call remediation still costs two answers: one at the
finding gate, one at the write gate
([#269](https://github.com/go-steer/mast/issues/269)).

**A budget ceiling is durable when there is a database to write it to, not
when an operator happens to be watching.** The spend ledger hung off the
eventlog handle, the handle is built only under `--attach-listen`, and so a
daemon started with `--session-db` alone got durable sessions and a
`max_cost_usd` that reset on every restart — warned about in a message
naming the wrong flag. The watchdog's halt is attach-gated for a real
reason (`POST /guardrails/reset` is attach-only, and a halt nobody can
clear is worse than one a restart forgets); the ledger inherited that
argument by sharing plumbing with it. A ledger is not a latch. It now keys
on `--session-db`, which is the condition it always needed, and the
warning names that flag. The inversion mattered because the failure it
guards against — a crash loop spending the cap once per restart — is an
unattended one, and an unattended daemon is the least likely to have bound
an operator socket. Grants still replay on a daemon with no attach
surface, so a session an operator rescued does not come back wedged by a
restart they never made ([#274](https://github.com/go-steer/mast/issues/274)).

Fixing it exposed a drift the same issue covers: `eventlog.Open` injected
both `busy_timeout` and `_txlock=immediate` on SQLite, while
`OpenSessionService` injected only the first. That was survivable while
ADK's session service was the sole writer on that connection — one write
mutex kept it to one at a time — and stops being survivable the moment the
ledger writes its own rows outside that mutex, because SQLite answers a
snapshot upgrade with an immediate `SQLITE_BUSY` that `busy_timeout`
deliberately never retries. Both settings are now injected on both paths,
and the plain path has the concurrency test the overlay path already had.

## v0.6.0 (2026-09-03)

*No mutating call goes unrecorded, no call is paid for before it is checked,
and where enforcement cannot reach, mast refuses rather than pretends.*

This is the first release since the fork that the parity scoreboard did not
choose. It reads **17 of 19** and the two rows still red are switchboard's to
write, so v0.6 had to state its own claim. The claim is about the distance
between what a bundle promises an operator and what the runtime enforces.
Two places had one, and neither was a bug in the sense that something
reported a wrong answer — both were cases where `hitl.on_mutation` or
`budget.max_usd` said more than the code delivered, which is the specific
failure this product exists to not have.

**A ceiling is no longer crossed by the call that reports it.** Spend was
folded out of the event stream after a call returned, so the first thing that
happened when a workload ran out of money was that it spent more. On turns
that was not merely late but structurally unable to be right: `max_turns` was
checked with `>`, so **a workload capped at 3 turns had always made 4**. A
pre-call check now sits in front of that fold and asks a different question —
not *has a ceiling been crossed* but *can this ceiling still be respected* —
which is answerable without guessing what the next call will cost. It never
estimates, because a projection refuses affordable work on a bad guess and
permits unaffordable work on a worse one and an operator cannot tell those
apart; it refuses only where the arithmetic is a proof. The post-hoc fold is
untouched and is still the durable ledger.

**A specialist's ceiling is no longer the workload's.** Through v0.5,
cancelling the run was the only lever the fold had, so *one path is spent* and
*the workload is over* came out the same way because nothing could tell them
apart. Now a crossed specialist ceiling is handed to its coordinator as a
report it can route around, the same as a specialist that declines, and the
session finishes through whatever paths still have budget. The workload's own
ceiling still ends the turn — there is nothing left to route to.

**And where enforcement cannot reach, the refusal is now the design.** v0.5's
notes called the composition refusal on a planner roster holding a change
executor *containment*, and pointed at
[#235](https://github.com/go-steer/mast/issues/235) for the boundary
decision. #235 is closed, and it closed on an answer rather than on a fix: a
park is two halves that both live in the session event log and a resume
re-enters at the root, while a planner dispatch runs on a private in-memory
session that dies with the tool call — so there is nowhere for an approval to
come back to. Gating `invoke_specialist` itself would have an operator approve
a specialist name and a sentence of prose, which is lead row L7's own
description of the competitor. Five artifacts that described the refusal as
temporary now describe it as the answer, and name what you do instead —
`coordinator`, `graph`, or `on_mutation: apply`. The **recording** half was
separable and shipped: a dispatched specialist's mutating calls are on the
effect ledger even though no plugin of the outer session ever sees them.

Parity scoreboard: **17 of 19**, unchanged. No row moves, and that is the
point — all five workstreams here sit behind rows that were already green,
which is exactly why the board could not have chosen this release.

**What this release does not do.** It adds no tool, no bundle block, no
endpoint, and no flag. If v0.6 shipped correctly, an existing bundle behaves
the way its author already believed it did. The one exception is the
composition refusal above, and it is now the point rather than an
embarrassment.

- **A spent specialist closes one path, not the session.** A crossed
  specialist ceiling is a refusal its coordinator can route around; only
  the workload's own ceiling ends the turn (#252, W10.3).

  The thing that tells them apart is typed. `budget.Scope(err)` answers
  *whose* ceiling an enforcement error was, and it changes neither the
  sentinel nor a byte of the message, because `pkg/attach`'s turn-error
  classifier prefix-matches that text on purpose (#135/#208) and must
  keep working without importing `pkg/budget`. It is an error type
  rather than a substring test on the specialist's name because a
  difference in outcome should not rest on prose.

  **The runtime change is four branches; the work is what it does to
  reporting.** A run that quietly loses half its roster returns the same
  `nil` as one that did not. Four surfaces answer that: the trip counter
  and a WARN line naming the specialist, `cost_ceiling.scopes[]` on
  `GET /guardrails`, and `mast.Result.Exhausted` for a library caller —
  scoped rows only, since the workload's own ceiling is already in the
  error such a caller is holding instead of a `Result`. A session-scoped
  guardrail reset now says honestly that it cleared nothing, and names
  who is still out and the `scope=` to raise them with; before, it
  reported clearing a trip it had never touched, and then — once that
  was fixed — reported a cheerful "nothing was tripped" while a
  specialist sat spent.

  **Making a refusal cheap made retrying one free**, and that is the
  finding worth carrying out of this release. A Task agent declaring an
  `output_schema` has a `finish_task` whose parameters *are* that
  schema, so a default refusal payload does not validate — and ADK
  answers an invalid `finish_task` with *"you could retry calling this
  tool"*, a retry instruction rather than an error. Refused call,
  invalid report, retry, refused call, and no iteration costs a model
  call or a token. The v0.4 acceptance legs spun to **3,292**
  `finish_task` calls in ninety seconds, and had been passing for a
  release only because the session was cancelled first. mast declines to
  guess its way out: fabricating a schema-conforming finding was
  available and is the one thing it must not do, because a refusal is
  exactly the case where nothing was looked at, so a synthesized one
  would put an invented fault into an incident stream. The delegation
  resolves to nothing, and the loss is carried on the metric, the WARN
  line and the API surface instead.

- **A cost ceiling refuses the call that would cross it.** `max_turns`,
  `max_tokens` and `max_cost_usd` are now checked before the model is
  called rather than after it answers (#252, W10.1 / W10.2).

  The seam is an `llmagent` `BeforeModelCallback`, chosen by measuring
  it against a `model.LLM` wrapper on the one shape that discriminates
  them — a planner dispatch. Both reach it; runner plugins are what that
  boundary drops. What separates them is whose promise the identity is:
  a per-agent ceiling needs the agent name, and the callback is handed
  it as a declared parameter, where the wrapper would recover it by
  asserting on a `context.Context` that ADK merely happens to populate.
  The callback is installed inside `NewTaskAgent`, `NewSingleTurnAgent`
  and `NewCoordinator` rather than at the eleven construction sites, and
  first in the chain, so a caller's own callback cannot short-circuit
  past a ceiling. It rides the Go context, never the session id — inside
  a dispatch the session is the sub-runner's, and a meter resolved from
  it would be a fresh empty one that approves everything.

  **A refusal is a synthesized response, not an error and not a cancel.**
  Returned as an error it would reach the caller in the field ADK
  reserves for a broken tool, wrapped in three layers of workflow
  plumbing; a cap that fires must not arrive looking like a crashed
  tool. It is in the transcript where an operator reads it
  (`agent.RefusalMarker`), and it leaves no phantom spend — the meter's
  cost and the durable ledger's row count are asserted equal with and
  without the gate, because a refusal that wrote to one and not the
  other reconciles wrong.

  Two consequences of a refusal costing nothing had to be handled rather
  than noticed. The turn driver reported **OK** — the stream ends
  cleanly and a caller's `if err != nil` never fires for work that did
  not happen — so the driver reads a *delta* in the refusal count across
  the turn, since a run that finished its work and happened to land
  exactly on its cap did not stop for the budget. And the preflight door
  had the mirror bug: it asked whether anything had *crossed*, but a
  well-behaved session now stops **on** its cap without crossing, so it
  would have answered "I am out of budget" in prose forever.

  From outside this is one wall reported with two codes:
  `BUDGET_REFUSED` and `BUDGET_EXCEEDED` are both `cost_ceiling` and
  both carry the reset hint. They differ only in which one spent money.

- **A rate belongs to the (backend, model) pair.** Prices are keyed on
  `<backend>/<model>` over `anthropic`, `anthropic-vertex`, `gemini` and
  `vertex`, and looked up for the pair the call will actually be billed
  against (#178, W10.0).

  Through v0.5 a price was something mast *reported*: a number on a
  meter, next to a ledger row, in a release note. A number that is
  reported can be approximately right. W10.2 makes a price the input to
  a **refusal**, and a number a refusal is computed from is either the
  price of the call about to be made or it is not. It was not:
  `RatePer1K` took a model id and nothing else, and the builtin table is
  keyed on bare ids that are a mixture of two backends — LiteLLM's
  unprefixed `claude-*` rows are first-party Anthropic, its unprefixed
  `gemini-*` rows are Vertex. So every Claude-on-Vertex call in mast's
  history was priced off the first-party row **and** every
  Developer-API Gemini call off the Vertex row, in the opposite
  direction. Only the first half had been written down.

  **The alias is not the backend**, and getting that wrong would have
  moved the defect rather than fixed it: `GOOGLE_GENAI_USE_VERTEXAI`
  sends a bare `--provider` to Vertex, and Anthropic picks first-party
  or Vertex by which credential is present. The resolver is therefore
  shared with the code that builds the client, so a price cannot name a
  backend the call did not use. `pricing.LookupFor` falls back to the
  bare id, so a pair upstream does not price keeps the rate it has
  today rather than dropping to zero.

  **This moves no number.** Measured against LiteLLM first: every
  shipped model costs the same on both of its backends today. That
  agreement is upstream's to keep and not mast's to depend on — which is
  the argument for keying on the pair, not against it. It also means no
  test built on the shipped table can tell a pair-keyed lookup from a
  bare one, so the discriminating test prices the pairs apart in a
  synthetic catalog. Closes the live defect
  [#178](https://github.com/go-steer/mast/issues/178) left behind when
  it was closed without code, and delivers the pricing half of
  model-support M2.

- **A dispatched specialist's mutations reach the effect ledger, out of
  band.** Under `hitl.on_mutation: apply` — the policy the composition
  refusal exempts, and what an unattended workload actually sets — an
  interrupted planner dispatch now leaves a dangling intent the next
  boot's auto-resume scan can see (#235, W9.3).

  Recording is one-directional, so unlike the write gate it crosses the
  boundary freely. A recorder on the sub-run observer seam writes each
  dispatch's mutating intents and completions to the outer session's
  **companion operations row**, and the boot scan folds them in. Not
  primary-row session state, which is what this was first designed as:
  ADK's database session service treats a session handle as a write
  lease, and an out-of-band append to the primary row invalidates every
  other holder's — and a dispatch writes at precisely the moment the
  outer turn holds one, so it would have killed the very turn it was
  recording.

  The recorder runs **last** on the seam, after the budget meter and the
  watchdog, because an intent record claims a call is *about* to happen
  and every consumer ahead of it can still stop the sub-run; recording
  first would wedge a session into ambiguous-effect mode over a mutation
  a ceiling prevented. A failed intent write stops the dispatch — under
  `apply`, the record is the only control that call has.

- **The planner-dispatch refusal is permanent, and says why.** A planner
  roster holding a `change_executor` is still refused at startup
  whenever `hitl.on_mutation` asks for the write to be gated. What
  changed is that five artifacts stopped describing that as a stopgap
  awaiting #235 — a promise that had become the opposite of true, and a
  reader who believed it would be waiting for something that is not
  coming (#235, W9.1 / W9.2).

  **The exit criterion was a composition test, and it earned the
  distinction.** The refusal names three ways forward, so one test
  builds each of them through both doors and another pins the words and
  fails on hedging language. A stopgap can name a way out loosely; an
  answer cannot, because an escape nobody composed costs the operator
  the hour they spend on it.

- **The dominant-tool-call density detector stays opt-in**, and that is
  now recorded as a decision rather than as a pending one
  ([#227](https://github.com/go-steer/mast/issues/227)). The reason is
  not the detector's accuracy. mast's default watchdog posture is
  `feedback`, so an alert is not a log line an operator triages — it is
  a paragraph prepended to the next turn's prompt on a workload with
  nobody watching, which makes a false positive an *instruction*. And
  this detector's false positive is already written down one file over
  in the cycle detector's docs: a polling workload, which is exactly the
  shape v0.5's scheduled monitoring shipped. No behaviour changes; the
  signal remains available to any caller that builds its own set and
  knows its workload does not poll.

- **`scripts/uat-v0.6.sh`** — 42 acceptance assertions against a real
  daemon, offline and credential-free, wired into the e2e presubmit
  alongside the four earlier releases'. It measures a capped workload
  spending its cap and not one call more on two independently computed
  meters, a specialist's cap closing one path while the incident
  finishes through another, and a dispatched mutating call surviving a
  mid-flight `SIGKILL` onto the effect ledger.

  It also found that **no acceptance leg in any release had ever driven
  a planner dispatch.** A planner is offered `finish_task` alongside its
  control-plane vocabulary and the offline fake checked `finish_task`
  first, so every planner workload in every harness was answered by
  finishing it, having dispatched nothing. Every leg was green; the
  suite simply had no planner in it.

## v0.5.0 (2026-08-30)

*A scheduled cycle gathers its own facts without spending a token, learns
what changed from the classifier rather than from mast, speaks only when
something did, and takes an operator's acknowledgement without pretending it
was an approval.* v0.4 shipped the schedule and left the cycle empty; v0.5
fills it, and the whole release turns on one decision about who makes the
calls that fill it.

A run-to-run finding diff advances persisted state as a side effect of
answering, which makes it mutating — and under the shipped
`hitl.on_mutation: require_approval` default, a model holding that tool
parks the cycle for a human on every fire. An unattended monitor that needs
an approval to find out whether anything changed is not one. Declaring the
tool non-mutating would have put a false statement into an audited override,
and a dry-run-then-advance-later contradicts the ordering the notify path
depends on. So **the collection and state-advance legs are mast's, not the
model's**: a workload declares `monitor.collect`, mast runs those calls
itself at the top of each fire, and composition refuses to start if a
collect tool is reachable from any roster — including through the two
un-enumerated grants the capability split exempts a change executor from.
The cost claim then holds structurally, because a leg the model is not part
of cannot spend a token, and it is measured on both meter surfaces anyway.

The consequence worth knowing is what mast then declines to do with the
facts it gathered. The transitions are consumed verbatim from
[`k8s-lookout`](https://github.com/go-steer/k8s-lookout): no mast-side
fingerprinting, no severity comparison, no re-derivation of `escalated`. A
cycle whose classifier reported nothing does not wake the model at all —
not "wakes it and declines to post" — and the only evidence such a cycle ran
is a counter, deliberately, so that a healthy monitor and a dead one stop
looking alike. The one judgement mast does make is completeness rather than
correctness: a truncated record stream is a prefix of a healthy answer, so a
missing or disagreeing summary voids the cycle before any model call.

Parity scoreboard: **17 of 19** rows green at this tag, up from 11 at the
v0.4.0 tag (`docs/v0.3-plan.md` §1). v0.4 said the parity claim was v0.5's
to make, and mast has flipped every row that was mast's to flip. Two of the
six that moved — resource-name normalization and chat egress — were wholly
k8s-lookout's and switchboard's, and had been green since 2026-08-15 without
this repo noticing. The two still red, in-chat Approve/Reject and the
approver allowlist, are switchboard's to write; mast's accountability for
them is that the resume shape and the `X-Asserted-Caller` path do not move
underneath, which is now a test asserting the JSON names, verdict and scope
vocabularies and status codes as strings rather than as constants.

Landing alongside that scope rather than inside the claim above: the
tool-calling half of the eval board — a call is scored by its arguments and
its result rather than by its name, and a consequential miss is charged to
the tool that would have answered it — plus a durable budget ledger so a
ceiling bounds a workload instead of a process, MCP response digesting with
a way back to the full payload, a Vertex alias for Gemini, and the
attach-mode ACL work. Two measurement fixes are worth reading on their own:
the two judged nightlies were never measuring the same resilience, because
one SDK retries a provider's 429 twice and the other not at all; and the
v0.2 UAT's long-standing flake was `pipefail` promoting curl's EPIPE over a
matched `grep -q`, at nine sites, three of which were failing open.

**What this release will not let you do.** A monitoring workload that also
remediates — a planner roster holding a `change_executor` — is refused at
startup whenever `hitl.on_mutation` asks for the write to be gated. The
write gate and the effect outbox are runner plugins and `invoke_specialist`
builds its runner without them, so the alternative was executing an
unapproved write on a bundle that had asked for approval; the same roster
runs under `coordinator` or `graph`, where the gate reaches it. Under
`on_mutation: apply` the refusal does not fire, because there was no gate
for a dispatch to bypass — but the outbox record is still missing there, so
an interrupted dispatch leaves no dangling intent to scan for and no
recorded completion to replay. That is the durability the unattended posture
this release is built for actually wants, and it is documented rather than
refused over. This is containment; the boundary decision is #235.

- **The wire contract switchboard codes against is now pinned as
  literals.** `/resume` and `/monitor-ack` are consumed from another
  repo, so their JSON field names, the verdict and scope vocabularies
  (`approve | reject | edit`; `once`, `session`, `session_tool`,
  `always`, `change_set`), the `X-Asserted-Caller` header name, the
  `{confirmed, payload}` confirmation envelope and the `/resume` status
  codes are asserted as strings rather than as constants (#242, W6.2 /
  W6.3, mast's half). Tests only — no behaviour changed.

  The gap this closes is narrow and real: a Go rename compiles, passes
  every behavioural test in the package because the tests are renamed
  with it, and breaks a client the compiler cannot see. Adding an
  optional field still needs no coordination; renaming or removing one
  is now a failing test that says so.

  Noted rather than changed: `GET /resume` answers **200 `ok`** from the
  health route, not 405, because `GET /` is a catch-all under net/http's
  pattern matching. The assertion is on the dispatch — nothing resumes,
  nothing is acked — since the status code is the health handler's.

- **An operator can acknowledge a finding, and mast records who did —
  but an ack is not an approval.** A workload can declare a
  `monitor.ack` block naming one of its catalog tools, and the daemon
  opens `POST /monitor-ack` on the inject listener. An acknowledgement
  that arrives there is authenticated, attributed, recorded durably by
  `pkg/transcript`, and forwarded to the producer's own ack tool with
  `subject_key` and `ack_by` (#242, W4.6, closing parity board row 13
  on mast's side). It is not on the cadence: an ack arrives when
  somebody reads their chat.

  **`ack_by` comes from the credential, never from the body.** The
  identity is read off the authenticated caller at the moment of the
  request — a real person where the daemon has a user table
  (`MAST_INJECT_USERS_FILE`), `shared-bearer-token` where it does not —
  and a request that supplies `ack_by` itself is refused **400, by
  name**, rather than quietly overridden. This is the rule #194 settled
  for `ConsumedBy`, for the same reason: an attribution a caller writes
  about itself is worth nothing after an incident, and a silent
  override teaches an integrator that the field is accepted.

  **An ack is not a mutation approval**, and the implementation shares
  nothing with the write gate. No grant, no `approve | reject | edit`
  verdict, no freshness window, no change-set signature, and nothing in
  `sessions export-decisions` — reusing that machinery would put a
  suppression in the decision export as if it were an adjudication, and
  the two are different acts. Only one of them is a person taking
  responsibility for a change.

  **mast exposes no ack tool the model can call.** The ack is a
  `monitor.collect`-class call, so the same reachability fence covers
  it: a workload whose specialists can reach the ack tool by any route
  — named in an allowlist, covered by a whole-server grant, or
  inherited by a roster that declares none — refuses to start, naming
  the specialist, the tool and the block. mast now calls a tool nobody
  asked for in exactly three places, and each has its own fence; a
  fourth caller needs a fourth fence, not a fourth call site.

  The suppression itself stays with the producer. mast forwards the
  ack and reads the classification back; the next cycle reports the
  subject as `suppressed` because
  [`k8s-lookout`](https://github.com/go-steer/k8s-lookout) says so, and
  the record is still *reported* rather than filtered — mast does not
  decide what an operator stops seeing. A repeat ack of a subject
  already acked is answered from the durable record with
  `previously_acked_by` and `previously_acked_at`, which survives a
  daemon restart because it was never process state.

  New: `cmd/mast/monitorack.go` (the route's policy), `pkg/monitor`'s
  ack argument names, `pkg/transcript`'s durable ack record, the
  `monitor.ack` bundle block,
  `mast_monitor_acks_total{workload,outcome}`, and three acceptance
  legs (`U-ack` in `scripts/uat-v0.5.sh`) — one that an ack is
  attributed, forwarded, honoured by the producer and durable across a
  restart, one that it is not an approval, and one that no workload's
  tool roster contains it.

- **A monitoring cycle that changed nothing no longer wakes the
  model.** A workload can declare a `monitor.notify` block naming a
  conversation, and mast posts a cycle's assessment into it through
  [switchboard](https://github.com/go-steer/switchboard)'s message
  ingress — configured on the daemon with `--notify-url` and
  `MAST_NOTIFY_TOKEN` (#242, W4.5, closing parity board row 14).

  The headline is a negative. When the classifier reports an empty
  transition set, the turn **does not run at all** — not "runs and
  declines to post". A fifteen-minute cadence that spends a model call
  on every quiet cycle costs more per month in nothing-happened than
  the incidents it exists to catch. The skip is deliberately narrow: it
  applies only where the workload declared both
  `monitor.transitions_from` (so "nothing changed" is the classifier's
  answer and not mast's guess) and `monitor.notify` (so speaking is
  what the cycle is for). Every other workload runs every tick exactly
  as before.

  Consecutive speaking cycles extend **one** message rather than
  posting several, so an incident that takes six cycles to resolve
  reads as one growing story instead of six notifications an operator
  has to reassemble; the first quiet cycle closes the timeline, and the
  next incident gets a message of its own. Switchboard's two non-error
  answers to an append are handled rather than surfaced: a 409 ("I no
  longer remember that message" — a restart, another replica, a message
  posted from elsewhere) re-sends the whole story as an edit, and a 200
  carrying a continuation ref ("that message is full") retargets every
  later append. Timeline state lives in the process and is not
  persisted: after a restart the honest answer to "which message am I
  extending" is *none*, and a fresh post costs one extra message where
  appending to a stale one costs a story with a hole in it.

  There is **no queue, no retry and no spool**. A send that failed is
  an errored fire, counted
  `mast_monitor_notifications_total{outcome="error"}`, and the next
  cycle reports what is new then. Holding the assessment and re-sending
  it is the tempting fix and it is wrong: the classifier advanced its
  own state when it answered, so the replay would describe a world that
  has already moved on. This is the ordering the whole M4b chain was
  built around — advance state, *then* speak.

  Silence is bounded by a wall-clock deadman, `digest_after`, and not
  by a count of quiet cycles: a counted digest is silently re-timed by
  any change to `interval`, and what an operator is owed is a sign of
  life on a schedule they can reason about. A monitor that has been
  quiet for a week is otherwise indistinguishable from one that died a
  week ago. The clock starts at daemon startup, so a daemon booting
  into a quiet world does not immediately announce the quiet.

  A cycle that *breaks* now says so in the same channel — the gap W4.2
  named and left open. A collection that fails, a classification that
  arrives truncated or a turn that errors posts a mast-authored notice,
  and the next successful cycle posts another. Both are edge-triggered,
  once on the way down and once on the way back rather than on every
  failing cycle, because a channel that repeats itself on the cadence
  is one an operator mutes — and the mute costs them the incident
  report too. A health notice that cannot be delivered never fails the
  fire.

  Two refusals guard the configuration seam. A bundle that declares
  `monitor.notify` on a daemon with no `--notify-url` **will not
  start**: the workload's entire output is the message it was going to
  send. And `MAST_NOTIFY_TOKEN` is refused if it equals any of the
  daemon's inbound tokens (`MAST_INJECT_TOKEN`, `MAST_ATTACH_TOKEN`,
  `MAST_A2A_TOKEN`, `MAST_AGUI_TOKEN`) — they authorize opposite
  directions, and sharing one means anything that can read the chat
  bridge's configuration can inject turns here. It is an env var and
  never a flag, so the credential stays out of `ps` and out of the
  container spec's args. Which conversation and how long silence may
  last are the bundle's; which ingress and as whom are the deployment's,
  so one bundle moves between staging and production unedited.

  New: `pkg/notify` (a dependency-free ingress client — the
  `check-slim-deps` gate stands), `cmd/mast/notify.go` (the timeline
  policy), two counter families
  (`mast_monitor_notifications_total{workload,outcome}` with `posted`,
  `appended`, `replaced`, `rolled`, `quiet`, `health`, `error`, and
  `mast_monitor_digest_wakes_total{workload}`), and five acceptance
  legs (`U-notify-onchange` in `scripts/uat-v0.5.sh`) driven against a
  recording stub ingress at `testdata/uat/ingress` — because most of
  what has to be proved here is that a request is *absent*.

- **What changed since the last run comes out of the classifier, and
  mast does not second-guess it.** A workload can now point
  `monitor.transitions_from` at one of its `monitor.collect` keys; the
  result filed under that key is parsed into a transition set and rides
  the wake-up envelope as `transitions` — `{"scanned": 412, "records":
  [...]}` — alongside the raw `collected` results (#242, W4.4).

  The point of the feature is what it refuses to contain. There is no
  enum of transition classes, no severity comparison, no fingerprint,
  no "is this really new" check: a record's `transition=` value is the
  producer's own string, carried through to the model verbatim, and an
  unrecognized class is simply one mast has not seen.
  [`k8s-lookout`](https://github.com/go-steer/k8s-lookout) already
  keeps the per-run state and does the classification; a second
  implementation here would be a second thing to disagree with the
  first. `transitions_from` names *where* the verdict comes from, never
  what the verdict may say. The loader checks only that the key names a
  result some `monitor.collect` entry actually files.

  The one judgement mast does make is whether the bytes are a whole
  answer. A record stream is one record per line — logfmt or flat JSON,
  detected per line — terminated by a mandatory `scanned=<n>
  findings=<n> elapsed=<d>` summary, and a result without that line, or
  whose `findings=` disagrees with the records parsed, is void rather
  than quiet. A truncated stream is a prefix of a healthy answer, and
  the notify half declines to page on "nothing changed"; the two must
  not read the same. A record classified with no `subject_key` is
  malformed for the same reason — nothing downstream can ack or
  de-duplicate a subject it cannot name. Malformed aborts the cycle
  before any model call, on the same grounds as a failed collection: a
  monitor reporting calm because its parsing broke is worse than one
  reporting nothing.

  A quiet cycle is explicit, not absent. A workload that classifies and
  saw no change ships `"records": []`; a workload that names no source
  ships no `transitions` key at all. "We looked and nothing changed"
  and "we do not know" are different facts, and W4.5 acts on only one
  of them. The parsed set replaces the raw text under `collected`
  rather than accompanying it, so the model is never handed two
  spellings of the same fact.

- **A monitoring cycle gathers its own facts, before the model is woken
  and for zero model calls.** A workload can now declare a
  `monitor.collect` block: an ordered list of tool calls mast makes on
  its own behalf at the top of each scheduled fire, whose results reach
  the roster inside the wake-up envelope under `collected` (#242, W4.2).

  The shape is forced by what the interesting tools are. Knowing what
  *changed* since the last run means asking something that keeps per-run
  state, and a tool that advances persisted state as a side effect of
  answering is a mutating tool — correctly so, and mast's predicate
  defaults every un-annotated MCP tool to mutating anyway. Under the
  shipped default `hitl.on_mutation: require_approval`, a model holding
  that tool parks the cycle for an operator on *every fire*. An
  unattended monitor that needs somebody awake to authorize finding out
  whether anything changed is not unattended. The two alternatives were
  worse: declaring the diff `mutating: false` puts a false statement
  into an audited override, and a dry-run-then-advance-later split
  contradicts the ordering the notify half needs.

  So the collection leg is mast's, and it is fenced. The write gate's
  precondition read is bounded by *classification* — compose refuses a
  read that is mutating, so that exception can only widen towards safer
  calls. This one inverts that, so it is bounded by *reachability*:
  `compose.CheckMonitorCollectSurface` refuses to start if a collect
  tool is reachable from any roster, including through the two
  un-enumerated grants (`mcp:` with no `tools:` list, and no `tools.mcp`
  key at all) that the capability split exempts a declared change
  executor from. The refusal runs ahead of MCP wiring, so an operator
  without credentials reads the roster problem rather than a `403`
  standing in for it. `SingleTurn` specialists are exempt because they
  are built with no toolsets at all.

  The cost claim is structural rather than measured — a leg the model is
  not part of cannot spend a token — and is asserted anyway, off both
  meter surfaces `/steps` reads: a bounded monitoring cycle that
  collects costs exactly one model call, the same as an injected
  incident that does not. A collection failure aborts the cycle before
  any model call and counts as an errored fire; waking the model with a
  partial picture would let a monitor report calm because its collection
  broke, which is worse than reporting nothing because only the second
  is visibly broken.

- **A judged nightly no longer loses a corpus row to a provider's rate
  limiter.** On 2026-08-21 three of the 31 scenarios came back
  `Error 429 ... RESOURCE_EXHAUSTED` from Vertex, so the board scored 28
  rows, and an incomplete board is exit 1 by design — a quota blip
  presented as a broken measurement of mast (#239).

  The gate is unchanged: 28 rows compared against a 31-row baseline
  understates or overstates whatever is missing, and softening that
  would cost the tier the one honest signal it has. What changed is
  upstream of it. `internal/evals/judge.Retrying` wraps both models the
  tier builds — so the corpus, the grader and J-cost-tier all survive
  the same blip — and waits out a `429` or `503` on a bounded 3s/9s/27s
  schedule. Only when the call failed *before* the model yielded
  anything: a stream that already spoke cannot be replayed without
  handing the caller its content twice.

  This also removes an asymmetry that was invisible from mast's side.
  `anthropic-sdk-go` retries twice of its own accord and
  `google.golang.org/genai` does not, so the two nightlies were
  measuring different amounts of resilience; the Anthropic run on the
  same commit was green. Retryability is decided by `errors.As` on each
  SDK's own error type, never by matching text — the corpus is 31
  Kubernetes incidents, several of them about exhausted resources, and
  the grader is handed those responses verbatim.

  Retries are counted onto the board (`retries`, `retry_wait_seconds`),
  printed when non-zero, and diffed night over night, because a retry
  nobody can see is how a provider under worsening pressure keeps
  producing complete, green, increasingly slow boards with nothing to
  point at.

- **MCP tool responses are digested before the model reads them, on by
  default.** `pkg/digest` — a structural JSON pruner, a raw-payload
  store, and per-method telemetry — shipped in v0.2 with no caller in
  mast: the wrap that drives it upstream was never ported, so a triage
  loop paid for every byte of `managedFields` on every subsequent turn,
  and two attach protocol fields (`latency_ms` since v1.2.0, `savings`
  since v1.3.0) plus `/usage`'s `digest_methods` block were documented
  surfaces nothing could ever fill (#221). Both sidecars now ride
  digested responses only.

  Now every MCP toolset is wrapped. **Responses under 8000 bytes come
  back verbatim** — upstream wraps those too, but a passthrough
  re-serializes a small map into a JSON string and costs more tokens
  than the map it replaced. Verbatim means mast adds nothing at all to
  them, not even a timing field: the write gate re-reads a change-set
  precondition and compares it to what the same read returned at
  approval time, so a wall clock in an unchanged response would void an
  operator's grant roughly whenever the two reads landed in different
  milliseconds. For the same reason a precondition read runs the tool
  underneath the wrap, never a digest of it. Larger responses are pruned
  and arrive with `digest`, `raw_bytes`, `method`, `latency_ms`,
  `call_id` and a `savings` breakdown. Nothing is sent to a
  second model to do it: the port is deliberately structural-only, so
  digesting adds no spend, no latency tail, and no new failure mode to a
  tool call, and `savings.subagent_*` is absent by construction.

  Because a digest can drop a field that mattered, Task-mode specialists
  also get **`retrieve_raw`**, which exchanges a `call_id` for the
  original payload. It is registered only alongside a working store, and
  it reaches specialists as a built-in tool rather than a toolset on
  purpose — a toolset is matched to a `tools.mcp: - server:` entry by
  name, so the rosters that enumerate their tools, the posture mast
  recommends, would have been exactly the ones that silently lost the
  escape hatch. It grants no new reach: it returns bytes the specialist
  already received. Its description spends most of its length talking
  the model out of calling it, which is load-bearing — upstream measured
  a ~6× cost increase on an identical triage run from a model that
  re-inflated digests to double-check them.

  Every failure degrades to the undigested response: a tool call that
  worked does not fail because the thing meant to make it *smaller* did
  not. Raw payloads are scratch state under the system temp directory,
  never beside `--session-db`, and a store that cannot be created leaves
  `retrieve_raw` unregistered with a warning rather than refusing to
  serve.

  `--mcp-digest=false` turns the whole thing off; `no_digest: true` on a
  server in `mcp.json` opts out one server. `GET /tools` lists
  `retrieve_raw` when it is live and omits it when it is not.

- **A planner roster that holds a change executor no longer starts when
  the bundle asked for the write to be gated.** The write gate and the
  effect outbox are runner plugins, and `invoke_specialist` builds its
  runner itself — with none. So a mutating call made inside a planner
  dispatch did not park under `hitl.on_mutation: require_approval`, did
  not stop under `dry_run`, and left no durable record of what it did.
  Every other dispatch shape runs on the outer runner and was always
  gated normally.

  Measured, not inferred: an outer `BeforeToolCallback` over that shape
  sees `invoke_specialist` and `finish_task`, and never the
  `scale_deployment` the specialist executed between them.

  Composition now refuses the combination, on both doors — the library's
  `BuildRoot` and the binary's pre-MCP `CheckRoster`, so an operator
  without credentials reads the real reason instead of an OAuth 403. The
  message names the specialist and the way out: run the same roster
  under `coordinator` or `graph`, where the runner carries the gate.
  `on_mutation: apply` is exempt, because there is no gate under `apply`
  for a dispatch to bypass; what is still missing there is the outbox
  record, and it is documented rather than refused over.

  This is containment. Letting the gate reach inside a dispatch is #235,
  and it needs a decision rather than a wiring change: an approval park
  suspends a turn to be resumed from a durable session, and a
  sub-session is in-memory. `examples/workloads/gke-triage` ships
  `require_approval`, a `change-executor`, and a commented-out
  `planner:` block — its comment now says why uncommenting it stops.

- **One recording, replayed per concurrent branch.** `--model=scripted`
  holds a position in a transcript, and an offline fake collapses every
  per-specialist model override back to the one instance — so a whole
  roster shared one cursor while fan-out ran its analysts at the same
  time. Three branches walked one script between them, which the mutex
  around the cursor made quieter rather than safer: `-race` stayed
  silent, and the symptom was a `script exhausted` from whichever branch
  lost the race, or a branch that succeeded having replayed another
  branch's turn.

  A replay is now per ADK branch — its own cursor over its own decode of
  the same recording, each starting at turn 0. A three-analyst fan-out
  against a one-turn recording is three analysts that each replay that
  turn. **Nothing changes for a sequential shape**: a coordinator, a
  planner, a single-agent replay and a resumed run all inherit their
  parent's branch unchanged, so they go on consuming one script in one
  order, exactly as recorded. Keying on the branch rather than on each
  model call is what preserves that — a second user turn is a new call
  that must continue the recording, not restart it.

  Two things this does not do, both deliberate: one transcript cannot
  describe N *different* branches (a recorded turn does not name the
  agent it belongs to, and teaching it to is a wire-format change that
  goes to core-agent's `pkg/recording` first), and two invocations
  running at once in one process still share the unbranched replay.
  A malformed transcript now also fails at `NewScripted` rather than at
  whichever consumer happened to call first.

- **A fourth watchdog signal, for the loop neither of the other two can
  see.** `dominant-tool-call` trips when one call accounts for 8 of the
  last 12 — the `a a a b a a a c a a a` shape, where every interloper
  resets the consecutive-repeat detector's run and the cycle detector
  finds no repeating block. Nothing tripped until the interleaves
  happened to stop long enough for five in a row, which is a convergence
  delay rather than a miss, and on the live run upstream measured that
  delay was 22 identical calls over 2m20s of a loop that was degenerate
  by the fourth.

  It stands down structurally where another detector already owns the
  shape — a consecutive run at the repeat threshold, or a window that is
  a clean repetition of a 2–4 call block — because mast appends every
  signal's alert and three detectors on one loop would be three
  paragraphs of steering under the `feedback` default. Both deferrals
  are fields and zero disables either, so wiring this signal on its own
  gets it undeferred.

  **Not in the default signal set.** A third Critical detector changes
  what every unattended workload is told about itself, and a polling
  workload is exactly a dominant call with interleaves; defaulting it is
  a posture decision, still open. A library embedder wires it today with
  `watchdog.NewDominantToolCallSignal(12, 8)` in a custom signal list.

- **The metrics reference page is now checked against a real scrape.**
  `docs/site/.../reference/metrics.md` enumerates every family
  `pkg/observability` constructs, with labels and fixed vocabularies.
  Nothing verified that, and the drift is silent in the direction that
  matters: a family added without a page edit is a metric no dashboard
  learns exists, and a renamed one leaves a page that reads like
  documentation and behaves like a broken query.

  `pkg/observability`'s new gate primes a registry, does a GET on
  `Handler()`, and compares the parsed scrape against the parsed page
  in both directions — family names, label names, and the enumerated
  label values, which `Prime` materializes and which therefore appear
  in the scrape. The comparison deliberately reads the *published*
  artifact rather than regenerating expected names from
  `prometheus.CounterOpts`; upstream derived its column that way and
  got two names wrong.

  The drift that prompted it is fixed too: `docs/observability-design.md`
  listed `mast_scheduled_fires_total` and `mast_a2a_server_tasks_total`
  in neither its shipped nor its design-target column, and left the two
  AG-UI families reading as unshipped. All four are now in a delimited
  shipped inventory the same test holds to the registry, and the A2A /
  AG-UI design-target sketches say how the shipped families differ from
  them (`{workload}`, not `{skill}`; `interrupted`/`aborted`/`rejected`,
  not `interrupt`/`cancelled`). `mast_turns_total`'s v0.4
  `watchdog_halt` outcome is recorded there as well.

- **Five site links stopped hard-coding the deploy base.** Content is
  supposed to write `/reference/cli/` and let the
  `remark-prepend-base` plugin add `/mast` at build time; five links
  said `/mast/...` outright, which the plugin skips. They work today and
  would 404 the day the base changes — the one event the plugin exists
  to survive. `dev/tools/docs-lint` grew a fourth rule so they cannot
  come back.

- **A specialist dispatched by the planner is billed to the workload
  that dispatched it.** The planner runs every `invoke_specialist` call
  on a runner of its own, and a private runner is a private event
  stream: nothing it emitted reached the budget meter, the metric
  registry, or anything else riding the outer stream. A workload with a
  `max_cost_usd` could spend past it without limit as long as it spent
  through the planner's door, and per-specialist ceilings — which key
  on the event author — were unenforceable there for the same reason.
  Coordinator, graph and fan-out dispatch never had the gap.

  The dispatch tool now hands each sub-run event to the host
  (`planner.Config.SubRunObserver`, threaded through
  `compose.RootConfig`), which folds it into the same meter under the
  *outer* session's ID. `mast.RunWorkload` and the daemon wire it; a
  library caller composing the planner directly should too. Sub-run
  model calls also now show up in `mast_model_calls_total` and
  `mast_tokens_total`.

  A ceiling crossed inside a dispatch stops that dispatch, not the
  session: the planner gets its result back marked `"status":
  "halted"` with the reason and decides what to do next. That is the
  finer-grained stop `pkg/budget` documents as needing a pre-call seam
  — the tool body turns out to be one, for this shape.

- **The watchdog now watches inside a planner dispatch, and a trip there
  halts the session.** Same private runner, same blind spot: a
  specialist spinning inside one `invoke_specialist` call emitted its
  tool calls where the session's watchdog could not see them, so
  `--watchdog=enforce` could not halt a loop it could not observe —
  which is the exact shape the watchdog exists to catch. The dispatch
  now feeds those events to the session's watchdog, the same one the
  outer stream feeds, through the seam the metering half opened.

  The session's, and not one per dispatch, because the signals count
  repetition: a per-dispatch watchdog resets that count every time a
  dispatch ends, and a specialist making three identical calls in each
  of ten dispatches would never reach a threshold of five. The cost is
  worth knowing — the planner's own calls and its specialists' now land
  in one signal set, so an `invoke_specialist` between two dispatches
  breaks a consecutive run, the interleaved shape `dominant-tool-call`
  exists for. A specialist spinning inside *one* dispatch, which is what
  this catches, has no interleaver.

  **A trip stops the dispatch *and* cancels the turn**, deliberately
  unlike a crossed budget cap. A ceiling is cumulative, so stopping the
  sub-run is the whole remedy; a watchdog trip is a latch meaning an
  operator has to reset the session, and a halt that stopped only the
  dispatch would let the planner re-dispatch the same specialist
  immediately. Under `warn` and `feedback` nothing is cancelled — the
  alert is retained for `GET /guardrails` and the model-facing paragraph
  is queued for the **planner**, since the specialist's sub-session is
  already gone and the planner is the one that would dispatch again.

  Library embedders driving the watchdog themselves get
  `watchdog.ObserveInto` and `watchdog.Drain`, the per-event and
  end-of-run halves of `Tap`, now exported so a caller with events but
  no stream to wrap runs the same code the stream does. **Breaking, for
  the unreleased seam only:** `planner.SubRunObserver` is now
  `SubRun(sessionID, specialist) SubRunSink` — a sink per dispatch
  rather than one flat callback — because the watchdog needs dispatch
  boundaries and a flat callback cannot tell two parallel dispatches
  apart.

  #226's third consumer is still open, and worse than it read: runner
  **plugins** are runner-scoped and the dispatch tool builds its runner
  with none, so a mutating call inside a dispatch reaches neither the
  effect outbox nor the write gate — it is not gated even under
  `hitl.on_mutation: require_approval`. Filed with a repro as #235.

- **Every example bundle is now composed by a test, not just the one.**
  `examples/workloads/gke-triage` was exercised by the e2e presubmit and
  a projection test; `bounded-triage` and `ns-audit` were prose that
  nothing compiled. mast's loaders keep gaining refusals — a `tier:`
  that conflicts with a `model:`, an `output_schema` that will not
  parse, a roster whose read/write split does not hold, `tools.skills`
  since #211 — and each of them can turn a shipped example into one
  that no longer boots, with the failure landing on an operator's first
  run rather than in CI. All three build today; the gate is preventive.
  The tree is globbed rather than listed, so a new example is covered
  by existing.

- **A proposed change has to name a tool an executor can actually run,
  and `tools.builtin` never was one.** The write gate's producer
  contract checks a finding's `proposed_change` against the set of
  tools the roster's change executors hold, so an operator is not
  offered an approval that will fire nothing. It built that set from
  each executor's MCP allowlist *and* its `tools.builtin` list —
  and nothing populates `specialists.BuildOptions.Tools`, so a
  specialist is built holding no built-in tools whatever its
  frontmatter declares. An executor declaring `builtin:
  [patch_k8s_resource]` therefore had proposals for that tool accepted
  at report time and nothing to run them with at approval time, which
  is the exact failure the contract exists to prevent.

  The executable surface is now the MCP allowlist and nothing else. An
  executor whose whole declared surface is built-in names has an empty
  one, refuses every proposal, and says so at startup instead of
  leaving an operator to discover it mid-incident.

  The axis itself is documented for what it is rather than removed: a
  claim the spec makes about itself, read by the capability split, the
  fan-out branch check, and the write-surface startup log — all of
  which hold the specialist to the claim, which is sound whether or
  not it grants anything. `specialists-design.md`'s normative table
  now says so, including that absent and empty mean the same thing on
  this axis, unlike `mcp:`. #219.

- **A session that has to be resumed from disk keeps the ACL it was
  stored with.** It did not. `resumeAndRegister` read the persisted row,
  authorized the caller against it, and then registered the rebuilt
  session under whatever ACL the `SessionResumer` returned — which for
  any realistic resumer is the zero value, because the ACL is the
  registry's durable state and a factory has no reason to go re-read it.
  A zero ACL means no owner, and no owner means admins only. So the
  owner's own request produced the entry that then refused them, as a
  404, on a session they own; it stayed that way until the next
  eviction. A viewer added by `PUT /acl` before a restart was shut out on
  the same path.

  The row now wins whenever an ACL store is wired. The resumer's return
  value is still honored where there is nothing else to go on — no store,
  or no row for this session — and a store read that fails is still fatal
  only when a resume gate is present, because a transient error is no
  reason to refuse a resume the daemon was not going to authorize anyway.
  Fixes a second symptom of the same discard: a resumed session skipped
  its `LastTouchedAt` update and sorted wrong in `GET /sessions`.

  Not reachable through the shipped daemon, which wires neither an ACL
  store nor multi-session auth; it is reachable by any embedder that
  turns both on. Nothing caught it because the test double returned the
  persisted ACL by hand, which no real resumer does. #223.

- **`pkg/digest` says it has no caller, and a test holds it to that.**
  The package ships complete — content router, structural pruner, CCR
  store, telemetry, an OTel span — and nothing in mast imports it. The
  MCP wrapper that drives digesting upstream was never ported, and the
  descope was never written down, so three surfaces described a live
  feature: `attach.UsageInfo.DigestMethods` ("present when at least one
  `digest.Process` call has fired", i.e. never), the attach protocol's
  v1.2.0 `latency_ms` and v1.3.0 `savings` tool-result sidecars
  (specified as produced by two files this repo does not have), and
  `Savings.Subagent*`.

  Each is annotated where it lives, and the claim is a test rather than
  a comment: `TestNothingInMastImportsDigest` fails the day something
  imports the package, and names the annotations to correct. The wire
  fields stay on the response shape — it is wire-compatible with
  core-agent's daemon, which does populate them.

  Whether to wire the package or drop it is #221. Two fixes rode along,
  both safe precisely because nothing calls it: the span attributes are
  `mast.digest.*` rather than the ported `core_agent.digest.*`, and the
  dangling references to core-agent's design docs and issue numbers are
  qualified as such.

- **The roster listing says what each specialist is allowed to touch.**
  `GET /sessions/{id}/subagents` carried a specialist's name, model,
  root, invocation and `capability`, and no tool grant — so the endpoint
  built to answer *what can this thing do* answered only the coarsest
  half of it. Entries now carry a `tools` object.

  It reports the grant's **effect**, not the frontmatter's syntax,
  because the syntax means opposite things one character apart: a spec
  with no `mcp:` key inherits every MCP toolset the workload has, one
  that writes `mcp: []` is denied all of them, and both are an empty
  list on the wire. `mcp_grant` is `"all"`, `"none"` or `"listed"`
  accordingly, and `whole_server` draws the same distinction per entry —
  a listed server with no `tools:` of its own passes whole.

  The built-in axis ships as `builtin_declared` rather than `builtin`.
  Nothing populates `specialists.BuildOptions.Tools`, so a specialist is
  built holding no built-in tools and the list narrows nothing; what
  reads it is the write gate and the capability-split check. Publishing
  it as `builtin` would tell an operator the specialist can call tools
  it does not hold. The third axis, `skills`, has no field at all: the
  loader refuses a non-empty one, so it can never reach a running
  roster.

- **A session's ACL can be amended, not just granted.**
  `auth.ActionSessionAdmin` has documented itself as covering "ACL /
  metadata mutations on the session" since the authorization matrix was
  written, and the only route behind it was `DELETE`. An ACL was set once
  at session creation and never afterwards: adding a viewer to a running
  session meant deleting the session and making a new one, and a departed
  contributor kept write access until someone did.

  `GET /sessions/{id}/acl` reads the list back and `PUT` replaces it.
  `GET` sits at the **read** bar rather than the admin bar — everyone the
  ACL admits can already read the transcript, so the membership list is
  not the sensitive part, and a viewer who wants write access needs to
  know whom to ask. `PUT` is a whole-document replace: omission clears,
  because omission-means-leave-alone would leave no way to remove the
  last viewer, and revocation is half the point.

  A `PUT` against a daemon that does not enforce ACLs is **refused with
  501**, not accepted. An amendment nothing consults would report success
  for an access restriction that does not exist — the same defect shape
  as an allowlist axis with no subsystem behind it. `GET` still answers
  there, and its `enforced` and `persisted` fields are how an operator
  learns that the ACL governs nothing, or that the grant they just made
  lives only until the next restart.

  Ownership **transfer** is daemon-admin only, for the case where the
  owner left, and a non-admin who sends an `owner` field gets a 403
  rather than a silently dropped field — an ignored field in an accepted
  request reads as a completed transfer. An owner cannot be cleared at
  all: a session nobody owns is reachable by admins alone, which is a
  lockout rather than an edit.

  Behind it, `Entry.ACL` stopped being an exported field. It was written
  once at registration and read without a lock by every authorizing
  request, which a runtime amendment turns from a stale read into a torn
  one. It is now an accessor returning cloned slices, written only
  through `SessionRegistry.SetACL`, which persists before it touches
  memory and carries the row's timestamps forward so an ACL edit does not
  re-date the session.

- **An unattended turn is now one trace instead of several orphans.**
  ADK's spans parent off whatever context reaches `runner.Run`, and mast
  only sometimes handed it one with a span on it. Turns driven by a
  request — inject, resume, attach, A2A, AG-UI — landed under otelhttp's
  server span and read correctly. The turns that define the product did
  not: a scheduled trigger firing, a boot-time auto-resume, and `mast run`
  all start from a daemon or process context with no span, so ADK's
  `invoke_agent` became a **trace root**, and every model call, tool call
  and MCP call in that turn hung off a trace that started nowhere and
  named no session.

  Every turn now opens one `mast.turn` span at the shared chokepoint,
  with ADK's tree beneath it. It carries what an operator needs to find
  the turn again — `mast.session.id`, `mast.workload.name`,
  `mast.turn.kind`/`.detail` (the existing turn label, split so the
  bounded half is groupable), `mast.turn.outcome`, `mast.cost.usd` for
  the turn's own spend, and `mast.turn.queued_ms`, since one session
  runs one turn at a time and queueing behind another turn otherwise
  reads as a slow model.

  The span opens **before** the turn lock, so a turn refused at the
  chokepoint leaves a record under a span-only `refused` outcome.
  `mast_turns_total` is deliberately unchanged: it has only ever counted
  turns that started, and moving that would move every dashboard's
  denominator. Span outcome and metric outcome now come from one call,
  so they cannot drift.

- **A specialist's `tools.skills` allowlist is refused rather than
  silently ignored.** A specialist spec is the file that states what a
  sub-agent may touch, and two of its three allowlist axes are enforced
  code — `builtin` by the write gate and the capability-split check,
  `mcp` by `filterToolsets`. `skills` was read by no production code at
  all, because mast ships no skills subsystem for it to narrow: there is
  no `pkg/skills`, no SKILL.md loader, no `invoke_skill`. So
  `skills: [a, b]` looked exactly like a whitelist and granted the same
  as an absent field.

  A non-empty list now fails the load naming the file, the same way a
  misspelled `capability` or `tier` already does. `skills: []` still
  loads — present-but-empty means deny-all on every axis, and deny-all
  is what this build does. Two adjacent declarations, the
  `tools` wire's `skill` source and the `skill` plan-exempt namespace,
  are annotated in place rather than removed: an embedder can still
  produce the first, and the second should be re-decided when a loader
  lands. `docs/skills-design.md` now records that its v0.1 loader has
  not shipped through v0.4.

- **A Vertex resource-path model echo no longer costs a session its
  per-model rates.** `ModelVersion` is stamped from the model the API
  echoes back, because where that disagrees with the request the server's
  answer is the billed one. But the field is read as a pricing key, and
  `pricing.Catalog` matches by exact key then by longest prefix *anchored
  at the start of the string* — so `claude-opus-4-5@20251101` finds its
  row and `projects/{p}/locations/{l}/publishers/anthropic/models/…` finds
  nothing. On `anthropic-vertex`, an echo carrying a resource path now
  falls back to the requested model ID.

  The failure this avoids was already bounded — `Meter.priceOf` degrades
  to the flat per-1k rate and counts the turn as unpriced rather than
  billing it at zero, so the ceiling still trips — but it discards every
  per-model rate including cache reads, which is the largest term on a
  cache-warm agent.

- **A watchdog halt reports as a watchdog halt, not as a Vertex config
  error.** `turn-error` gains a `watchdog_halt` kind carrying
  `retryable: false` and a hint naming the reset endpoint, and the attach
  protocol goes to **1.6.0**. A halt's message is assembled from the looping
  tool's name and up to 200 bytes of its arguments, and the classifier was
  substring-scanning that text — so a tool called `parse_manifest` made the
  halt a `config_error` advising the operator to check `model.vertex.location`,
  arguments echoing "not found" made it `model_not_found`, and a failure
  streak quoting "permission denied" made it `auth_error` with an IAM hint.
  Three of four realistic halts sent an operator to debug the wrong
  subsystem mid-incident; the reason's own "how to clear this" sentence sat
  past the message cap and never arrived.

  Errors mast raises itself now declare their kind (`SelfClassifyingError`,
  consulted before every heuristic). String matching stays the default for
  errors that arrive from a provider, where wrapper churn makes a type
  switch fragile — it was only ever wrong for the errors mast wrote.

- **A turn somebody stopped on purpose no longer reports as a retryable
  network fault.** `turn-error` gains a `canceled` kind carrying
  `retryable: false`, and the attach protocol goes to **1.5.0**. An operator
  interrupt and a `--watchdog=enforce` halt both cancel the turn's context,
  and both used to arrive on the stream as `transient_network` /
  `retryable: true` — which is the one decision that flag exists to drive,
  driven backwards. A client keying a re-run affordance off it was being
  invited to re-drive the very loop the watchdog had just stopped.

  A turn that ran out of *time* is unchanged and still retryable: nobody
  asked for that one.

  Additive in the same way `cost_ceiling` was — no new event type, no new
  field, and §2.6 already requires a consumer to read an unrecognized kind
  as `unknown`. Note that the version numbers on the two attach
  implementations have diverged: core-agent shipped the same value at its
  1.8.0. Feature-detect against the capabilities frame, not the number.

- **`GET /sessions/{id}/tools` now lists the daemon's own tools, not just the
  MCP servers'.** A planner-dispatch daemon with no MCP servers used to answer
  with an empty catalog — accurate about its MCP tools, and wrong as an answer
  to "what can this thing do", since the planner's dispatch vocabulary is most
  of what it can do. Builtins carry `source: "builtin"`, no `server`, and the
  same `gate_state` projection as anything else, and they sort ahead of the
  servers.

  They are also listed unconditionally: unlike an MCP server, a builtin cannot
  fail to answer or go away between polls, so it needs neither the 30-second
  TTL nor the partial-failure handling — and it stays in the response when
  every server is down, which is the case that made the gap worth closing.

  The enumeration is not a second list. `planner.Vocabulary` is the function
  `planner.New` itself calls to attach the tools, and `compose.BuildRoot` now
  returns what it wired alongside the root, so a tool added to the planner
  joins the catalog without anyone remembering to add it. What is still
  missing is the handful ADK installs itself — `finish_task`, a coordinator's
  transfer tools — which stay out rather than being hand-listed, because a
  catalog naming tools that do not exist is worse than one omitting tools that
  do.

- **A resume against the daemon can name the person who approved it.** Point
  `MAST_INJECT_USERS_FILE` at a users file and `/resume` accepts each row's
  token as well as the shared one, recording `alice@example.com` on the pause
  instead of `shared-bearer-token`. `MAST_INJECT_PROXY_IDENTITIES` grants
  named identities the right to answer on someone's behalf via
  `X-Asserted-Caller`, for a chat relay with an approve button.

  Per-person attribution was already reachable by embedders; the daemon could
  not get there, and the reason turns out to be a defect rather than missing
  wiring. The shared-token gate and the authenticator both read
  `Authorization: Bearer`, so configuring both made `/resume` unreachable by
  *either* credential — the user token failed the gate, and the shared token
  cleared the gate and then failed authentication. The gate is now split for
  that one route.

  The other routes are deliberately not widened. A user table says who
  approved; it is not a second way in, and `/inject` and `/abort` still take
  `MAST_INJECT_TOKEN` and nothing else. The shared token also keeps working
  on `/resume` and is still recorded as `shared-bearer-token` — configuring a
  table must not retroactively attribute an unmigrated emitter's approvals to
  a person. Requiring attribution is done by leaving `MAST_INJECT_TOKEN`
  unset, and is then total: there is no unattributed way to answer a gate. A
  request presenting the shared token *and* `X-Asserted-Caller` is refused
  rather than quietly recorded as the shared credential, because a token that
  cannot name its own holder does not get to vouch for someone else's.

  Both variables are checked at startup — a proxy list with no table, or one
  naming an identity the table does not have, refuses to boot rather than
  turning into 403s with no explanation attached.

- **Three of the four consequential misses are the same tool, and the board
  now says so.** `k8s_resource_top` is the only tool in the catalog that
  answers either saturation question, and two unrelated model families
  skipped it independently. Each miss is charged to a tool — but only when
  exactly one tool would have answered it. With 14 of the table's 19 intents
  served two or three ways, charging a miss to every candidate would triple
  the numbers and name no cause, so a miss with several possible answers is
  counted as shared rather than pinned on anyone.

  The ranking leads with how many corpus scenarios a tool is the sole answer
  for, then with how often it was actually missed. That order is deliberate:
  the first is a property of the fixture and the second is one model on one
  night, and the first is what says whether fixing the tool is worth an
  afternoon. Skipping a sole-source tool does not risk the miss, it
  guarantees it, which is a different fact from a model that chose a
  different route to the same answer.

  This exists to make an experiment readable. `judge.toolDescription` lives
  in this repo rather than in k8s-lookout, so a tool that keeps being skipped
  can have its description rewritten and the board re-run — and the nightly
  delta prints the result as `k8s_resource_top: 3 miss(es) → 0`. The scores
  would not have shown it: a fix worth keeping moves `intent_coverage` by a
  fraction of one row's mean. The competing hypothesis is that the models
  reason past the tool rather than fail to find it, and the same experiment
  tells the two apart.

- **The judged board names the questions a different tool choice would have
  answered — and there are four of them, not twenty-nine.** The obvious
  thing to measure is whether a run called the tools that had data for its
  scenario. That was computed against both v0.4.0 boards before being
  rejected, and the data is the whole argument: it flags 11 of 31 scenarios
  on Claude and 18 of 31 on Gemini, and the flagged rows score *better* on
  `response_quality`, not worse. Most skipped tools were redundant with one
  that had already answered the question — 14 of the intent table's 19
  intents are satisfiable by two or three different tools, which is mast's
  consolidated read path showing up in the measurement rather than a defect
  in it.

  What the board reports instead is a **consequential miss**: a question the
  corpus expected answered, that a tool in the catalog would have answered,
  that the run never asked. Across both boards that is four rows, all node
  or pod saturation, so it is printed as a list — the scenario, the intent,
  and the tools that would have served it — with a tally by intent, because
  the same intent recurring across rows is one tool's description or
  discoverability rather than several unrelated incidents. The nightly delta
  names each one that arrives and each one that clears.

  An unsatisfied question no read-only tool can answer is deliberately not
  in that list. LC-13 expects a rollback, lookout excludes write tools by
  design, and it is already reported as a structural ceiling; counting it
  here would publish a deliberate scope decision as a permanent
  tool-selection failure. An expected tool name the intent table has never
  seen is a third thing again — a gap in the fixture, which deflates
  `intent_coverage` on purpose but is nobody's miss. The score and the list
  are now derived from one shared reading of the corpus, so a board cannot
  say one thing in its number and another in its enumeration.

- **The judged board says how a tool was called, not just that it was.** It
  recorded which tools a run reached for and nothing about how, which left
  two different failures looking identical: the model never called the tool,
  and the model called the right tool against a namespace that held nothing,
  read the empty result as "no problem here", and reported anyway. Both
  landed as an unsatisfied intent; only the first is a tool-*selection*
  problem, and the fix for the second is not the same one.

  The trace now retains each call's result alongside its arguments — the
  arguments say what the model asked for, and only the result says whether
  it learned anything. ADK's `error` key rides in the same payload, which is
  what keeps a failed call distinguishable from an empty one. Both are
  persisted onto the board with the arguments, so a run can be read after
  the fact rather than re-run.

  Every recorded call is then checked against the schema the model was
  actually shown — read off the built tools rather than written beside them,
  so it cannot go stale — and the violations are **listed, not averaged**:
  an unknown tool name, a missing required argument, a wrong type, a value
  outside a declared enum, an invented argument, an errored call, a call
  with no completion, and a well-formed call that found nothing. A rate over
  rows that call different numbers of tools is not comparable to itself
  between runs, and the actionable part is never the mean — it is which
  tool, which argument, which scope. Malformed calls are printed one by one
  and empty reads are counted, because an empty read is ordinary triage
  right up until *every* completed read in a row is empty; those rows are
  now named, since a score on one of them measures what the model already
  knew about Kubernetes rather than what it read. The nightly delta reports
  the same counts, which move when no mean does.

  The fixture's clean reading is unchanged, deliberately. The signal that a
  read found nothing is returned beside the prose rather than encoded in it:
  a flag in the tool's payload would change what the model reads and move
  every score on the board in the same run that introduced the column.
  Arguments and results are recorded verbatim, which is the answer v0.4's
  W8 already settled for decision exports — a violation whose scope has been
  redacted is one nobody can act on — and the board states its own exposure,
  which for a corpus of synthetic fixtures is none.

- **The tool-schema invariant is now stated over every construction path, not
  the one that was noticed.** #154 was a total tool-calling outage: every
  mast-authored tool and every MCP tool reached Claude as a bare name with an
  empty input schema, because the converter handled only the typed
  `genai.Schema` spelling while ADK v2 populates `ParametersJsonSchema`. The
  regression test that shipped with the fix covers `functiontool.New` — the
  shape of that bug, not the shape of the bug class.

  New `internal/toolcatalog` assembles the tool catalog a real turn puts in
  front of a model, and it is **captured rather than enumerated**: a
  coordinator+MCP rig and a planner rig are driven through an ADK runner with
  a recording model, and what the model was handed is what gets asserted.
  Nothing lists the construction paths, so none can be forgotten — a tool
  wired into either shape is covered the next time the test runs. Today that
  is ten tools spanning all three paths: typed `Parameters` (`finish_task`,
  the delegation tool), `ParametersJsonSchema` via `functiontool.New` (the
  planner vocabulary, `pause_session`, the federation invoke tool), and
  `ParametersJsonSchema` via `pkg/mcp`. Expectations are derived from each
  declaration rather than written down, so they cannot go stale.

  `toolcatalog.Verify` states the invariant once, shared by every provider's
  test: every tool arrives, a tool with arguments arrives with a non-empty
  properties map, every argument name survives and none is invented, every
  required argument is still required, and each argument's own schema is
  still a non-empty object. An adapter held to a weaker check than its
  siblings is how a defect hides — the Gemini path was fine throughout #154,
  so nothing measured against it would have caught the Anthropic converter.

  Both adapters now have one. Anthropic's asserts the real Messages request
  body and fails on eight of ten tools with the #154 fix reverted. Gemini's
  is a dependency probe — mast converts nothing there, and
  `ParametersJsonSchema` is a pass-through `any` that nothing would complain
  about losing — plus a separate check that built-in tool injection sits
  alongside function declarations instead of replacing them. Both run
  offline, credential-free, in the ordinary `go test ./...` presubmit.

- **Every durable approval named the channel instead of the human.** The
  audit answer to "who approved this?" is `PauseRecord.ConsumedBy`, and it is
  most of the reason a durable gate is worth more than an in-memory prompt.
  The daemon wrote the literal `"operator resume --token"` into it on every
  HTTP resume, and `{"resumed_by": "operator"}` into the response the resumed
  turn sees — so an incident review could establish that a session was
  resumed over HTTP and nothing else. Both now name the caller the request
  authenticated as.

  The identity is read from the credential, and never from the request body.
  `ResumeRequest` deliberately has no approver field: an attribution a caller
  writes about itself is worth nothing after an incident, the same reasoning
  that makes attach's `GuardrailResetRequest.Caller` `json:"-"`. It is not in
  the token either — a token is a bearer capability, so a claim minted at
  pause time is a claim about whoever *later* holds it, and a token handed to
  a colleague has to attribute the colleague. Both halves of the plumbing
  already existed (`pkg/inject` resolves the caller onto the request context;
  `cmd/mast` renders it); the daemon just never read them.

  New `auth.Attribution(ctx, fallback)` renders the three shapes — an
  identity, `alice@example.com (asserted by sa:switchboard)` on the
  `X-Asserted-Caller` proxy path a chat relay needs, and the fallback. The
  fallback is a **mechanism name**, not `"unknown"`: a context with no caller
  is an in-process path (the timed-pause scheduler, boot auto-resume, an
  embedder calling the library), and the code always knows which. `"timer"`
  and the direct-DB CLI's `"operator resume --token --session-db"` keep their
  literals for that reason — the first is pinned normatively in
  `docs/durable-execution-design.md`, and the second is honest about a path
  with no credential to read. `mast.ResumeByToken` picks up an embedder's
  `auth.WithCaller` identity and otherwise says `library ResumeByToken`.

  What this does **not** do: nothing wires an `auth.Authenticator` into the
  daemon's inject listener, so a CLI-launched daemon records
  `shared-bearer-token` — honest about what a shared credential proves, which
  is that *someone* holding it resumed. Per-person attribution is reachable
  by embedders today and needs a config surface to reach the daemon.
  `newResumeByToken` was lifted out of `serve()` to a top-level constructor
  so the identity it records is asserted without standing up a daemon; an
  audit field nobody tests is a field that drifts back.

- **The intermittent `/metrics` failure in the v0.2 UAT was never about
  metrics: `pipefail` was reporting a match as a miss.** `assert_metric` ran
  `curl -s "$BASE/metrics" | grep -Fxq "$want"`. `grep -q` exits the instant it
  matches, which closes the pipe under curl; curl fails its next write and
  exits 23; `pipefail` reports the highest-numbered failure in the pipeline, so
  it promotes curl's 23 over grep's own 0 and the assertion fails on a metric
  line that is right there in the scrape. Whether it happened came down to
  whether the body had been fully written when grep quit — which is why it
  needed a loaded box to show up, and why it moved between assertions from run
  to run instead of sitting on one. Measured against the real 5,566-byte scrape
  with the match at line 12 of 89: **30 wrong in 600** for the piped form, **0
  in 600** for a here-string. End to end under `nproc` busy loops, the pre-fix
  script failed 3 of 4 runs; the fixed script, same load, 0 of 4.

  Nine sites across five scripts had the shape, and the interesting ones are
  not the UAT's. In `assert_hasnt` (v0.3, v0.4) and `assert_no_session_label`
  (v0.2) a *match* is the violation, so the promoted status turns a detected
  violation into a pass — these were failing **open**. So was the preflight in
  `scripts/live-kind-v0.4.sh`: `kind get clusters | grep -qx "$CLUSTER" && die
  ...` is a refusal to adopt a cluster the script did not create, and the `&&`
  could not fire, so the script would have gone on to adopt it. Every site now
  captures first and matches with a here-string, which is not a pipeline and so
  carries grep's status and nothing else's.

  `dev/tools/shell-lint` (wired into `dev/ci/presubmits/all.sh` and the
  `hygiene` CI job, with a `--self-test` that fails if the rule is ever
  defanged) keeps the shape out. It is deliberately one rule rather than a
  shellcheck adoption: this one has a measured failure rate and a fail-open
  direction behind it, and a second rule should have to clear the same bar.
  `scripts/uat-v0.3.sh` already carried a comment documenting this exact hazard
  for `awk`, written when one instance of it was fixed in `show_field` — a note
  next to one occurrence is not a guard against the other eight.

- **A pricing regen said 32 rows changed and could not say which one mattered,
  and the answer turned out to be two scheduled price doublings nobody had
  written down.** Every regen re-stamps `UpdatedAt` on every row, so the diff a
  reviewer opens is dominated by rows that did not move. On 2026-08-19 that was
  31 of 32; the one that did — `gemini-3.6-flash`, halved — read exactly like
  the other 31 from the PR body. `dev/regen-builtin-pricing --report` now writes
  a "What moved" section that `pricing-regen.yml` puts at the top of the
  auto-PR: the rows whose *values* changed, counted and itemized, computed from
  the same timestamp-normalized comparison `--check` already used. Silence in
  that section is now a claim rather than an absence.

  Chasing that one row is what turned up the rest. `gemini-3.6-flash` had not
  been permanently cut — Google had moved it onto `gemini-3.7-flash`'s
  introductory rate, and **both revert from $0.75/$3.75 to $1.50/$7.50 on
  2027-01-01**, cache reads doubling alongside. `gemini-3.7-flash` is the
  gemini/vertex frontier default, so that is a `max_cost_usd` ceiling buying
  half the tokens it is budgeted for, silently, from the day the rate changes.
  Both rows are now in `introductoryRates` with the date. `pkg/taskclass`'s
  frontier comment justified the promotion partly on 3.7-flash being "half the
  price" of 3.6-flash, which stopped being true the day after it was written —
  rewritten to rest on the UAT, which is what actually chose the model.

  In the other direction, `claude-sonnet-5`'s increase to $3/$15 was
  **cancelled**: the introductory $2/$10 is now Anthropic's standard price. The
  dated assertion shipped in v0.4.x would have failed CI on 2026-09-01 over a
  rate that is correct — a guard firing in the direction that gets guards
  deleted. `introductoryRate.resolved` records that outcome as data, and
  inverts the assertion to pin the rate at the former promotional number
  instead. A table whose only exits are "fires" and "gets deleted" cannot say
  that a price was provisional and then became standard.

- **The guard against a constant metric was reporting a constant metric as
  healthy.** `internal/evals/measurability.go` exists to catch a column that
  is green because it never ran, and its own doc comment names upstream's two
  constant functions as the cautionary case. mast's `tool_coverage` has been
  0.000 on all 31 rows of every board since the corpus was ported — the
  dataset expects upstream's `kubectl_*` names, mast emits `k8s_*`, and the
  intersection is empty — and the reach table read `31/31 scenarios scorable`
  for it throughout.

  The cause was a definition, not a line: reach asked whether a scenario had
  *declared* an expectation, when the question is whether the expectation can
  be *met*. `intent_coverage` sitting beside it never had the bug, because it
  builds its denominator after mapping names through the intent table — two
  definitions of reach in one file, and only the declaration-shaped one was
  wrong. `ToolCoverage` now marks a scenario vacuous when no expected name is
  one the runtime can emit, which the intent table's `lookout_tools` answers
  by construction. The score does not move: an unsatisfiable expectation still
  reports 0/N and still names the tools, because the consolidation penalty is
  why the metric is on the board. What changes is that the harness stops
  averaging that 0 and says the column is a constant.

  A dead **diagnostic** is now reported on a line of its own rather than
  appended to the run's problems. The previous release described that line as
  non-gating and implemented it as gating; nothing caught it because no
  diagnostic was dead yet. With `tool_coverage` permanently dead — a fact
  about two tool surfaces, not a defect — that would have turned the whole
  deterministic tier red.

- **A hub restart un-federated the deployment, silently.** `attach.PeerRegistry`
  was memory-only: every registration went with the process, and each peer
  stayed invisible until its own heartbeat failed and it re-registered. That is
  a 20-60 second window in which "who is in the fleet?" answers wrong rather
  than answering slowly — recoverable where somebody is watching, and mast's
  premise is that nobody is. `attach.NewPeerRegistryWithState` snapshots the
  registry to a JSONL file on every mutation and reloads it at startup, so the
  hub comes back already knowing its fleet. Ported from core-agent
  `9f81626`; registrations are still in-memory by default, and
  `NewPeerRegistry` is unchanged.

  Two things the state file is careful about. It is **not** the wire shape:
  `Peer.Owner` is `json:"-"` because discovery responses must not leak it, so
  persisting `Peer` directly would reload every registration ownerless and
  quietly undo the enumerate-then-delete hardening — a separate on-disk record
  carries the owner, and the restart is tested through the HTTP handlers to
  prove the redaction survives it. And **a lease is re-clamped to the running
  configuration's ceiling**, not replayed at the width it was granted: this is
  what #166 settled for budget grants, arrived at again. Lower the max TTL and
  restart, and an unclamped reload would honour the withdrawn ceiling twice —
  once for the outstanding lease, then indefinitely, because a heartbeat renews
  at whatever width it reloaded. That divergence from upstream is mast's; the
  clamp runs before the expired-lease drop and is written back, so a peer only
  live under the old ceiling is not live at all.

  The daemon is unaffected: `cmd/mast` has no peer-hub flag, so
  `attach.Options.PeerRegistry` remains an embedder-only surface. Upstream's
  `--attach-peer-state-file` half of the port has nothing to hang on here and
  did not land.

- **`severity_accuracy` was partly measuring markdown, and the rest of it
  cannot be acted on.** Two findings, one metric.

  The extractor read `## SEVERITY: CRITICAL` as no severity at all. It
  stripped the `Severity:` label before the markdown decoration, and the
  label pattern is anchored — so a line carrying both matched neither
  pass, and the run was recorded as having declared nothing. On the
  2026-08-19 boards that hit **7 of 31 rows on Claude and 0 of 31 on
  Gemini**: the published Claude figure of 0.419 is 0.548 once the
  decorated verdicts are read, and part of the cross-family gap the
  nightly has been reporting was one family's heading style. A missing
  verdict and a wrong verdict both score 0, which is why the column read
  as a model that could not classify rather than a harness that could not
  parse. `rig.go` already named this hazard — a coordinator rewrite would
  "move severity_accuracy for a formatting reason rather than a capability
  one" — for a different formatting change.

  The remaining misses cannot be attributed to anyone. #179 asked for them
  to be partitioned against the corpus's *own* severity definitions:
  a row where the label contradicts the definition it ships with is a
  corpus defect, one where the agent escalated past a definition the label
  honours is mast's. That partition is not derivable. The definitions are
  twelve enumerated conditions, taken verbatim from upstream — CRITICAL is
  "service down, crash loops, OOM kills, 0 ready endpoints", WARNING is
  "no PDB, missing probes, :latest images, wildcard RBAC", INFO is
  "right-sizing, orphaned PVs, suspended CronJobs". Measured over the
  corpus, **the CRITICAL conditions occur in 5 of the 31 scenarios and the
  WARNING and INFO conditions in none**, while 20 of the 31 rows are
  labelled WARNING or INFO. On almost every row that misses there is no
  stated definition to partition against, so "the corpus is wrong" and
  "the rubric escalates" are indistinguishable in principle, not merely by
  this metric. Where the rubric does decide a row it contradicts the label
  twice in five — LC-03 is an OOMKill labelled WARNING, LC-10 has 0
  endpoints and is labelled WARNING — and both model families miss both by
  answering what they were told to answer.

  So `severity_accuracy` is now **diagnostic**, the disposition
  `tool_coverage` has: still on every board, still per-row legible, no
  longer a parity claim. Tuning the rubric until it moves was rejected in
  v0.3 as teaching to the test and there is nothing here to tune against
  anyway. `judge.TestSeverityRubricDoesNotSpanCorpus` pins the measurement
  that justifies the demotion and fails if either the corpus or the rubric
  moves toward covering the other — which is the condition for promoting
  it back.

  The demotion opened one gap, now closed: `evals.DeadMetrics` names
  gating metrics only, so a diagnostic that can score nothing anywhere
  would have become a silent column instead of a harness failure. The
  corpus summary reports that case separately — it does not gate, and it
  still says it is reporting a constant.

- **A budget ceiling a restart resets is a ceiling on what a workload spends
  per process.** `newMeterPool` minted every session's meter at zero, so a
  daemon restart handed each session its full cap back: a workload stopped by
  `max_cost_usd: 5.00` after $5.02 resumed with $5.00 available. mast's
  restarts are automatic and unattended, so a crash loop could spend the cap
  once per restart, indefinitely — the exact situation the ceiling was bought
  for. #166 made the watchdog halt durable and deliberately stopped here,
  because a trip is one latched bit and spend is an accumulator that has to
  come back to the cent.

  It comes back as a ledger: one row per priced model call in
  `agent_budget_spend`, written through a new `budget.Config.OnSpend` hook and
  folded into a fresh meter by `budget.Meter.Restore` on a session's first
  touch after a restart — session totals and each specialist's own, so scoped
  ceilings survive too.

  **Why not replay the session's own events**, which would have needed no new
  state: ADK's database session service persists `UsageMetadata` and `Author`
  but has no column for `ModelVersion` (`session/database/storage_session.go`,
  adk/v2 v2.2.0). Every replayed call would therefore miss `pkg/pricing`'s
  catalog and fall back to the flat per-1K rate that `pkg/budget` measures at
  5.9x error on a cache-warm session, and a restored ceiling that wrong is
  worse than none because it looks right. Replay would also price history at
  today's weekly-regenerated rates, rewriting money that already left the
  account, and its cost grows with the transcript — on the first turn after
  every restart, for exactly the long sessions that survive restarts. Against
  checkpointing the accumulator: that has to be reconciled against a
  transcript that may have advanced, where a ledger written at the same
  granularity the accumulator moves has nothing to reconcile. Its only loss
  window is a call whose row had not landed, which is always an undercount,
  never a phantom charge.

  **Operator grants are replayed now too**, which #166 explicitly declined to
  do — correctly at the time, since raising a ceiling over an accumulator that
  had forgotten what it spent is arithmetic on a number that no longer means
  anything. With the spend durable that inverts: a session an operator rescued
  at $5.02/$5.00 would otherwise come back with the spend and without the
  rescue. This needed `GuardrailRecord.GrantScope`, because replaying a
  specialist's grant onto the session would raise a cap by an amount nobody
  granted it. A pre-#175 row carries no scope, cannot be attributed either
  way, and is skipped — it still counts toward the audit total, which is why
  that total and the replayed figure can differ on an old database.

  Durable spend also makes a wedged session permanently wedged, so a turn on a
  session already past its ceiling is now refused **before** the model call
  rather than after it. Enforcement is derived from usage-against-ceiling on
  every priced event, so without the preflight a scheduler retrying every
  minute would buy one model call a minute forever. The refusal is a `409`
  naming `POST /sessions/{id}/guardrails/reset`, the same shape the watchdog's
  is.

  Restore fails open the way the watchdog's does — an unreadable ledger logs
  and the turn runs — and the read is retried next turn rather than latched as
  "restored to nothing". All of it requires `--attach-listen`, which already
  requires `--session-db`: with no durable connection the pool behaves exactly
  as it did before, and a daemon with budget ceilings and no attach surface now
  warns at startup, as one with `--watchdog=enforce` already did.

- **A rate can be current in its source and still be wrong, and nothing in
  the table could say so.** `claude-sonnet-5` carries Sonnet 5's
  *introductory* price of $2 in / $10 out per MTok, which lapses
  2026-08-31 in favour of $3 / $15. `Rates.UpdatedAt` cannot catch that:
  it records when the row was last read from LiteLLM, and the weekly regen
  re-reads LiteLLM, gets the same promotional number back, and stamps a
  fresh date on it — so the freshness signal reports maximum confidence in
  a value that has become false. The 2026-08-19 regen did exactly this,
  re-dating the row to the day with no change to the price.

  It cannot be fixed at the source either: LiteLLM's catalog carries no
  expiry metadata on any row.

  `pkg/pricing/introductory_rates_test.go` now holds the expiry as a dated
  assertion — past the end date, a row still reading the promotional rate
  fails the build. It runs inside `pricing-regen.yml` as well as ordinary
  CI, so an expiry LiteLLM has not caught up with stops the regen rather
  than being rubber-stamped by it. This matters beyond `/pricing`:
  `claude-sonnet-5` is the Anthropic mid-tier default, so an understated
  rate lets a workload spend past its `max_cost_usd` ceiling before the
  guardrail trips.

  Crude on purpose, and scoped to the one row we know about. #188 tracks
  the general version — a `provisional_until` field, `/pricing` marking
  provisional rows, and a regen that reports which rates actually moved
  instead of reading identically whether any did.

- **Gemini could only reach Vertex AI through an environment variable.**
  `--provider` had no value that named the backend: `BuildModel` handed
  genai an empty client config and let it decide from
  `GOOGLE_GENAI_USE_VERTEXAI`. Claude got an explicit
  `anthropic-vertex` alias at port time; Gemini did not, and the tier
  table has carried a `vertex` family since the port that nothing could
  ask for (`pkg/taskclass.Providers()`).

  The failure mode is the reason this matters more than the ergonomics.
  A deployment whose credential is the service account's ADC — Cloud
  Run, GKE with Workload Identity — that forgot the variable did not
  get "you meant Vertex, say so". It got an API-key error from the
  *other* backend, naming a credential it deliberately does not have,
  which points away from the fix.

  `--provider=vertex` now serves `gemini-*` against Vertex outright:
  `Backend: genai.BackendVertexAI` with the project from
  `GOOGLE_CLOUD_PROJECT` and the location from `GOOGLE_CLOUD_LOCATION` /
  `GOOGLE_CLOUD_REGION` (falling back to genai's own `global`). A
  missing project fails at construction naming the variable, not inside
  the SDK as a struct dump. This is core-agent's `vertex` provider
  (`pkg/models/gemini.NewVertex`) ported, down to the name, so the two
  repos describe the same thing the same way.

  The env-var path is untouched: with no alias, genai's env-driven
  selection still decides, and `geminiClientConfig` hands it the same
  empty config it always did. What changes is that there is now a way to
  be explicit. Empty-chunk tolerance follows the backend actually in use
  rather than the environment, so a Vertex run by alias no longer reads
  Vertex's candidate-less heartbeat chunks as malformed.

  One adjacent fix, since the same switch decides it: a Gemini-family
  alias no longer refuses a `claude-*` specialist override. The alias
  picks a *Gemini* backend and says nothing about Anthropic's, so the
  override now detects its own backend the way an unaliased run does —
  which is what `NewModelResolver` has documented as allowed since
  cross-provider overrides were resolved (2026-08-12).

- **An MCP server could turn a call around and ask mast for input, and mast
  would answer.** The MCP protocol lets a server request elicitation,
  sampling, or the client's roots in the middle of a call the client already
  made. mast's approval gate sits on tool *dispatch*: an operator approves a
  specific call and that call runs. A server-initiated request happens
  inside a call the gate has already cleared, so nothing the operator
  approved covers it — and the SDK answers it and then **retries the
  original call**. Against a stateful HTTP fixture, one approved dispatch
  ran the server's tool 20 times.

  This was filed as latent, on the reasoning that mast registers no
  elicitation or sampling handler so such a request would error. That holds
  for those two. It does not hold for `roots/list`, which go-sdk answers
  itself out of the client's own root set — no handler, no capability
  opt-in, no error — so the round trip completed against mast today.

  The documented off switch, `MultiRoundTripOptions.Disabled`, is also only
  half of it: the client-side middleware it governs runs on protocol
  ≥ 2026-07-28, and `StreamableServerTransport.SupportsProtocolVersion`
  serves that version only when the server is stateless. An ordinary
  stateful HTTP server negotiates 2025-11-25 and its *server*-side
  middleware sends the request directly, which `Disabled` never sees.

  `pkg/mcp` now builds its own MCP client for both transports, with the
  middleware disabled, no client capabilities advertised, and a receiving
  middleware that refuses `elicitation/create`, `sampling/createMessage`,
  and `roots/list` by name. Refusing by method rather than by omitting a
  handler is the point: a future elicitation handler, added for a good
  reason, has to delete a line someone reviews instead of silently
  acquiring a gate bypass. Both protocol regimes are pinned in
  `pkg/mcp/mrtr_test.go`, alongside two tripwires that fail if the SDK's
  defaults move.

  An MCP server that needs elicitation to complete a tool now fails against
  mast rather than proceeding unapproved. On an unattended deployment there
  is no operator to answer the question anyway; supporting elicitation with
  the gate wired through it is separate work.

## v0.4.0 (2026-08-17)

*An operator approves the exact call that will fire, the loop runs on a
schedule without an orchestrator, and every verdict becomes a labelled eval
row.* v0.3 put an operator in front of every mutating call; v0.4 makes what
they approve a typed object rather than a paragraph, and closes the honest
gap v0.3 shipped with — the change executor was unreachable from a diagnosis
in both shipped dispatch shapes.

Parity scoreboard: **11 of 19** rows green, up from 7 at the v0.3.0 tag
(`docs/v0.3-plan.md` §1). The four this release is accountable for — the
per-specialist model tier, the scheduled trigger, the bounded analysis path,
and the decision→eval feedback loop — are all flipped. The remaining eight
are v0.5's, and seven of those are halves of surfaces k8s-lookout and
switchboard have not landed yet: the parity claim is v0.5's, not this one's.

Landing alongside that scope rather than inside the claim above: the
behavioral watchdog port — two more detectors, the `feedback` and `enforce`
postures, a `safety.watchdog` field on the bundle, and halt state that
survives the process. **It changes a default.** A workload that never typed
`--watchdog` ran at `warn`, which on an unattended deployment is off with
extra steps: the alert goes to a pod log nobody is tailing. The default is
now `feedback`, so a detected runaway is told to the party that can stop it.
`--watchdog=warn` restores the previous behavior; the entries below give the
reasoning for the divergence from upstream's `enforce`.

- **Claude could not see the arguments of any tool mast defines.** Every
  mast-authored tool — the planner's dispatch tools, `pause_session`,
  and every MCP tool — reached the Anthropic wire as
  `{"type":"object","properties":{}}`: a name, a description, and no
  parameters. The model had to infer argument names from prose.

  ADK v2's `functiontool.New` derives a declaration into
  `ParametersJsonSchema` and leaves the typed `Parameters` field nil;
  `toolsParam` handled only `Parameters` and sent the canonical
  "no arguments" shape for everything else. The declarations ADK builds
  internally — `finish_task` among them — *do* use `Parameters`, which
  is why this survived a green nightly and a live GKE run: the failure
  is silent degradation on mast's own tools, not an error anybody sees.
  Gemini is unaffected; it takes the genai declaration natively.

  Found by triaging the first drift report (below) against
  `core-agent@ee3d6ec`; upstream fixed the same gap in their #754. The
  regression test builds its declaration with `functiontool.New` rather
  than by hand — a hand-built `genai.Schema` fixture exercises the
  branch that always worked, which is how this survived the existing
  tool tests.

- **A turn could die on the eventlog's write lock instead of waiting for
  it.** SQLite takes one database-wide write lock, and mast opens two
  connection pools against the same file — ADK's session service and the
  overlay that assigns event seq numbers. Under the default deferred
  `BEGIN`, an `AppendEvent` reads the session row first, taking a read
  snapshot, and the first write then attempts a snapshot→write upgrade.
  SQLite refuses that upgrade with an *immediate* `SQLITE_BUSY` when
  another connection holds the lock — `busy_timeout` deliberately never
  retries an upgrade, because two upgraders waiting on each other is a
  deadlock. So the 5s busy timeout mast already sets did nothing for the
  one case that bites.

  `eventlog.Open` now injects `_txlock=immediate` on the SQLite DSN, so
  every read-write transaction begins with `BEGIN IMMEDIATE` and waits
  on `busy_timeout` like an ordinary contended writer. Reads (`Get`,
  `List`) use no explicit transaction and stay lock-free under WAL.

  `pkg/eventlog/service.go`'s write mutex reads like it already covers
  this and doesn't: it serializes writes made *through the service
  wrapper*, not writes another connection makes on the same file. The
  concurrent writer here is the daemon's own machinery — the scheduler
  firing a cadence, auto-resume replaying a marked session, an A2A or
  AG-UI submission, an attach inject. Unattended is the shape that hits
  this most often and has nobody watching when it does.

  Ported from core-agent's #576, found by the triage below; upstream hit
  it through auto-continue, which mast does not have. The regression
  test holds the write lock on an independent connection and fails on
  pre-fix code with `database is locked (5) (SQLITE_BUSY)`.

- **A Vertex context cache that fails to be created is retried, instead
  of disabling caching for the daemon's lifetime.** `Caches.Create`
  failures split into two classes — context cancellation retried,
  everything else permanently sticky — and IAM errors landed in the
  permanent class. That is exactly backwards for the way a fresh
  deployment fails: a pod that starts before its Workload Identity
  binding has propagated gets `403 PERMISSION_DENIED` on its first
  turn, becomes correctly configured a couple of minutes later, and
  pays full input price for the rest of its life regardless. mast is
  the deployment shape this happens to — an unattended daemon starts
  when its controller schedules it, not when its bindings land, and
  nobody is watching the first turn.

  A non-context failure now gets 15s, 30s, 1m, 2m, 4m — six attempts
  over roughly eight minutes — before the manager gives up for good,
  which preserves what the sticky failure was protecting: a genuinely
  misconfigured project stops being hammered, loudly. Retries are
  demand-driven off `Init`, so the schedule is a floor on spacing
  rather than a timer and an idle daemon issues no RPCs at all. Each
  attempt logs its number and next eligible delay, so a rollout can be
  read as "waiting on IAM" versus "fix your config".

  Ported from core-agent's #723. The two new tests fail on the old
  sticky behavior.

- **The watchdog gained two detectors, and learned to read tool results
  as well as tool calls.** Its one signal — five identical calls in a
  row — is structurally blind to the loop that actually happens: an
  agent alternating between two tools, `list_agents → check_agent`
  forever, where no call is ever followed by itself.
  `alternating-tool-cycle` scans for a repeating short cycle instead.
  Argument comparison is also path-aware now, so `main.go`, `./main.go`,
  and `/workspace/main.go` are one file rather than three; the match is
  on a path-component boundary, so `a/doc.go` and `b/doc.go` stay
  distinct.

  `tool-failure-streak` is the one to read in a report. It fires when
  three tool calls in a row all return errors with none succeeding
  between, which is the state where a workload writes a confident
  summary of a system that nothing it ran could reach. The calls look
  ordinary; the results are the story. No prose is inspected — a
  detector that tried to recognize an over-confident claim would be a
  heuristic about English wearing the costume of a runtime guarantee.

  Outcome observation is an optional extension (`ToolResultObserver`)
  rather than a widening of the `Watchdog` interface, which is a
  documented plug-in point: a third-party watchdog should not break to
  gain a signal it may not want. Responses dedup against the same
  per-turn set as calls under a separate key space, since the streaming
  aggregator re-emits both and a double-counted failure would trip the
  streak at half its threshold.

  Ported from core-agent's #679 and #690 — the first two of seven; see
  `docs/sibling-sync.md` for the rest of the cluster and the
  sync-classification call that unblocked it.

- **`--watchdog=enforce`: the loop detector can now stop the loop, in
  the turn where it is happening.** The default is unchanged
  (`--watchdog=warn`, log and keep going), because a false positive
  that stops an unattended incident responder mid-triage is worse than
  one that annotates it. What is new is that the other posture exists,
  and that choosing it does something during the incident rather than
  after.

  Two things had to change for it to be worth having. First, alerts now
  drain **in-turn** — as soon as an observation lands — not only at the
  turn boundary. The loop the watchdog catches lives *inside* one turn:
  the model emits a tool call, the flow runs it and calls the model
  again, all within a single `Run`. A turn-boundary drain never fires
  while that is happening, and never at all if the turn does not end.
  That was a real gap in warn mode too — a looping session logged
  nothing until it stopped looping. Second, `repeated-tool-call` and
  `alternating-tool-cycle` are **Critical**; `tool-failure-streak`
  stays **Warn**, deliberately, because stopping a daemon three denials
  into a legitimate RBAC probe would make the backstop the outage.
  Severity is a property of the pattern; the posture decides the
  reaction.

  Under enforce, the first Critical alert cancels the turn through the
  same context handle a budget trip and an operator abort use, and the
  session refuses **every** subsequent turn — auto-resume, a scheduled
  fire, an attach inject — at the `runTurnPre` chokepoint, until
  `POST /sessions/{id}/guardrails/reset`. Structural, not advisory: a
  refusal only at the entry point an operator happens to use is a
  refusal that auto-resume walks straight through, re-driving the loop
  that tripped it. Turns that end this way are counted as
  `watchdog_halt` on `mast_turns_total`, and
  `GET /sessions/{id}/guardrails` reports `advisory: false` under
  enforce whether or not it has fired yet — "will this thing stop my
  agent?" is the question the field answers.

  Upstream keeps this state on its `Agent` struct; mast has no `Agent`,
  so it lives in a `watchdog.Enforcer` the daemon holds per session and
  the halt itself is the caller's `cancel()`. The package decides
  *whether* a session is halted and why; it never decides how a turn
  dies. Ported from core-agent's #628 and #719.

- **`--watchdog=feedback`: the loop detector tells the party that can
  stop the loop.** Every posture before this one routed the observation
  to an operator. On an unattended workload — the deployment mast exists
  for — that operator is a pod log nobody is tailing, and even a
  watching one can only interrupt a turn already in flight. The model
  choosing the next tool call is the only party that can decide not to
  make it, and it was the one party never told.

  Each alert now carries a model-facing `Guidance` sentence alongside
  its operator-facing `Reason`, and from `feedback` up they are
  prepended to the session's next prompt as a `[watchdog]` block framed
  as *an automated observation about your own previous turn — this is
  not a message from the user, and the user cannot see it*. The split
  matters because the readers can do different things: mast's operator
  text names `POST /sessions/{id}/interrupt` and the bundle's budget
  ceiling, and telling a looping model about an endpoint it cannot call
  is at best noise. A test asserts every shipped signal sets `Guidance`
  and that none of them leaks an operator control, so a fourth signal
  cannot ship with only half the prose.

  The three postures are now a ladder — `warn` < `feedback` <
  `enforce`, each including the one before it. **`enforce` implies
  `feedback` deliberately:** an enforce halt is cleared by an operator
  reset, and a reset resumes a model whose context still ends in the
  loop it was halted for, so without the injected observation the very
  next turn re-issues the same call and the reset is a treadmill. For
  the same reason a reset clears the halt but **keeps** the queued
  observation. The queue is bounded at four (oldest dropped), nothing is
  queued below `feedback` so flipping a long-running deployment up a
  rung cannot deliver a stale backlog, and it drains on read so an
  observation lands exactly once even if that turn fails.

  `feedback` is a correction, not a backstop — nothing stops a model
  that reads the block and loops anyway, which is why a workload with a
  bounded tool loop still wants `enforce`, which now gets both. It is also the one rung
  that does not apply to one-shot mode, whose whole mechanism is a next
  turn a one-shot does not have; it logs a line saying so rather than
  quietly running `warn`. The block is steering, not a trust boundary.
  Ported from core-agent's #678, and the commit that settled which side
  of the fork this feature lives on — shared infrastructure, not
  lean-fork-specific. See `docs/fork-design.md` § "Sync discipline under
  (E)".

- **A workload ships its own watchdog posture, and the default is no
  longer "off".** The three postures above were reachable only through
  `--watchdog`, and unset meant `warn` — log the alert and keep going.
  On mast that is off with extra steps: every mast run is unattended, so
  the operator the log is addressed to is a pod log nobody is tailing.

  Bundles now declare `safety.watchdog: warn | feedback | enforce`, and
  the posture resolves from three sources in order — the `--watchdog`
  flag, then the bundle, then mast's default. The bundle is where it
  belongs because the bundle is mast's deployment unit; the flag stays
  above it so an operator debugging a halted workload can drop the
  posture for one run without editing, and later forgetting to revert,
  the deployed manifest. The daemon logs which source won at startup
  (`watchdog posture resolved mode=… source=…`) — `enforce` otherwise
  announces itself only by refusing a turn. An unrecognized posture is a
  load error naming the field, not a silent fall back to the default:
  the failure mode being avoided is a workload whose author believes the
  halt is armed.

  **The default is now `feedback`.** Upstream defaults an unattended run
  to `enforce` and the premise is right — a warn-only backstop on a
  deployment nobody watches is not a backstop. The conclusion is not
  mast's: `alternating-tool-cycle` has a workload-shaped false positive
  (a scheduler-driven daemon watching a rollout settle calls the same
  tool with the same arguments on purpose), and on an unattended
  deployment a false halt is an outage that waits for the morning, where
  a false `feedback` costs one paragraph the model may disregard.
  Recoverable beats unrecoverable when nobody is watching. Set
  `safety.watchdog: enforce` on a workload whose tool loop is bounded by
  construction. Two tests pin the default, because this changes runtime
  behavior for every deployment that never typed the flag.

  This also closes a gap that was mast's alone: the library-embed
  surface (`mast.RunWorkload`) ran its turn with **no watchdog tap at
  all** — the one mast surface with no runaway backstop of any kind. It
  now reads the same `safety.watchdog` field and taps the same signals,
  with the rungs bounded by what a library call holds: `enforce`
  abandons the runaway turn, but there is no cross-call session state
  for the "refuse every later turn" half and no next turn for `feedback`
  to inject into.

  Ported from core-agent's #665 — the fourth of seven. The commit's
  other half, a `$10` per-session cost ceiling armed by default, is
  deliberately **not** ported: upstream keys it on a run being
  unattended, which in mast is a constant, and a fixed dollar cap on the
  lifetime of a `single_session` daemon is a scheduled outage rather
  than a runaway guard. `docs/sibling-sync.md` § "Deliberately not
  ported" carries the full reasoning.

- **A halt that a restart cleared was not a halt.** `--watchdog=enforce`
  latched its trip in a map on the daemon's `watchdogPool`, so a crash,
  an OOM kill, or a pod roll started a fresh process with the backstop
  disarmed. That is the one deployment shape enforce mode exists for:
  mast's restarts are automatic and nobody is watching, so the runaway
  loop → halt → crash → restart cycle could repeat indefinitely, each
  restart handing the loop a clean slate.

  Trips and resets are now appended to `agent_guardrail_log`, a table
  mast owns on the connection `pkg/eventlog` already holds, and folded
  forward on a halted session's next turn — before any model call,
  whichever surface drives it. The reset is durable in the same place,
  so clearing a halt clears it for good, and the row records the
  authenticated caller and any runway they granted. That upgrades the
  reset audit from a log line to a queryable row, closing a gap
  `cmd/mast/guardrails.go` had flagged in a comment.

  Three constraints worth knowing. Persistence is wired **only under
  `--attach-listen`**, because `POST /guardrails/reset` is attach-only
  and a persisted halt with no reachable reset is a brick rather than a
  backstop; enforce mode without an attach listener now says so at
  startup. **Configuration still wins over history** — a deployment
  dialed back to `feedback` does not inherit a halt only `enforce` could
  have produced. And restore **fails open**: an unreadable guardrail
  table logs a warning and lets the turn run, because a storage fault
  must not halt every session in the deployment with no trip behind it.

  Ported from core-agent's #671 — the last of seven, closing the
  watchdog cluster — but by a different route. Upstream writes both rows
  into the ADK session's own event stream from inside its agent's write
  lease; mast has no `Agent` to hang that lease on, and an out-of-band
  session append while a turn holds the handle trips ADK's
  optimistic-concurrency check. A table mast owns has no such
  contention. **Budget spend still does not survive a restart** — the
  meter starts every process at zero — which is a larger, separate piece
  of work, recorded in `docs/sibling-sync.md` § "Found while porting,
  not fixed here" rather than quietly folded into this.

- **An MCP server that rejects a call now says why.** Google's GKE MCP
  surface answers an IAM denial with HTTP 403 and a body naming the exact
  permission to grant. The operator saw `Forbidden`. HTTP MCP transports
  are now wrapped so a 4xx/5xx carries the server's own text — the
  permission name, the quota metric — through to the log, the model, and
  the attach surface.

  Narrower than the upstream fix it comes from (core-agent's #305,
  `b1101f9`), because the MCP SDK closed most of that gap in the interval:
  go-sdk v1.7.0 surfaces a standard `{"error":{"message":..}}` object
  itself. mast covers only what v1.7.0 still drops — the MCP tool-result
  error shape (`{"result":{"isError":true,...}}`, which decodes as a
  *result* and falls through to the status line, and is precisely what
  the GKE surface sends) and transient statuses, where the SDK returns
  early without reading the body at all. Standard error objects on
  non-transient statuses are left to the SDK, so the typed
  `*jsonrpc.Error` stays in the chain.

  Three things it deliberately does not touch: anything but POST, so the
  SSE reconnect keeps its retry budget; 404, whose `ErrSessionMissing`
  sentinel is worth more than the body; and any response that is
  non-JSON, over 32 KiB, or has no extractable text — all replayed
  intact. An extracted error does not tear down the MCP session.
  `TestSDKStillDropsTheseBodies` drives each case through the bare SDK
  and through mast's wrap in the same run, so the day the SDK closes one
  of the remaining holes it fails with instructions to delete the branch.

- **How far behind core-agent each ported package has fallen is now a
  number, reported weekly.** 182 files in this repo carry an
  `// Originally derived from go-steer/core-agent@<sha>` trailer, frozen
  at four different upstream snapshots, and nothing reconciled them —
  `pkg/pricing` went five weeks behind while three model families
  shipped, and it surfaced because a human noticed.

  - `dev/upstream-drift` answers `git log <ported-sha>..origin/main --
    <upstream-path>` for every trailer it finds, and aggregates by
    package. It is deliberately not a diff: every ported file already
    differs from upstream by construction (the trailer itself, `~/.mast`
    paths, mast-flavored comments), so a diff is 100% noise. Trailers
    may carry an explicit `:<upstream-path>` suffix for files renamed on
    the way in — three of them turned out to need one
    (`pkg/watchdog/bridge.go` ← `pkg/agent/watchdog.go` and two more),
    which is the bug class the tool has a distinct `missing-path` status
    for: an upstream rename makes the query return zero commits, and
    zero commits reads as "in sync". Once watchdog's real upstream path
    was known its drift went from 4 commits to 9.
  - `.github/workflows/upstream-drift.yml` runs it every Monday into one
    long-lived tracking issue, edited in place so a quiet week doesn't
    re-notify. It never fails a build — drift is the expected state
    here, much of it in code P1.3 will re-port wholesale, and a red
    build nobody can turn green trains people to ignore red builds.
    `GITHUB_TOKEN` suffices, unlike `pricing-regen`: the org restriction
    is on creating *pull requests* and the loop guard is on *pushes*.
  - The point is the deferral in `docs/fork-design.md`, which says to
    revisit extracting an `agent-substrate` shared library once "the
    shared surface has stabilized" without saying how to tell. Now
    there's a weekly sample. Baseline at first run: **48 upstream
    commits across 53 of 182 ported files in 17 packages**, against
    core-agent `main` at `ee3d6ec`.
  - `dev/ci/presubmits/fmt.sh` now covers `dev/`, which has held real Go
    programs since `regen-builtin-pricing` landed. vet, lint and test
    already reached them via `./...`; gofmt takes paths, so it had to be
    told.

- **`fetch_url` is no longer exempt from plan-first gating.** Under
  `require_plan_artifact`, read-only tools are exempt so research can
  happen before the plan is recorded. `fetch_url` was in that list: it
  reads, but it reads *across the network*, and egress before a plan
  exists is an exfiltration channel rather than research. Inert in mast
  today — no `fetch_url` tool is registered — which is exactly how it
  survived: a dead entry in a live table. `planExemptTools` is real
  policy, consulted by `CheckMutatingToolCall` on every mutating call,
  so the next tool named `fetch_url` would have inherited the exemption
  silently.

  This is the last sixth of core-agent's #465 security roundup. mast
  already had the other five for free, because `pkg/attach` was ported
  two days *after* that roundup landed; this one was missing because
  `pkg/permissions` was ported two days *before* it. Port timing is not
  a security control, which is the finding, not the fix.

  `docs/sibling-sync.md` is new and is where findings like that now
  live: every one of the 48 commits in the first drift report has a
  verdict, and the doc records what the instrument cannot see. The
  split is deliberate — the machine keeps the inventory, the doc keeps
  the verdicts — because the per-commit ledger `fork-design.md`
  originally specified went unwritten for three weeks. Also corrected
  along the way: `pkg/permissions/denylist.go` claimed the package was
  unwired and that "nothing in mast is protected by these checks",
  false since the v0.3 write gate; ten trailers left at `83ec071` by a
  re-port, which would have re-reported eight absorbed commits every
  Monday forever; and an 8 MiB response cap in `pkg/pricing/refresh.go`
  justified by a payload size ("tens of KB") that measures 1.67 MiB.

- **The model tables are generated from a rule, refreshed weekly, and
  can no longer drift apart quietly.** Mast forked its pricing code
  early and then went five weeks without a bump while three model
  families shipped. Nothing broke, which is the problem: a model with no
  rate is metered at a flat fallback and counted as unpriced, so the
  symptom of a stale table is a `max_cost_usd` that quietly means a
  different number of dollars.

  - `dev/regen-builtin-pricing` decides membership by **rule** rather
    than by a hand-curated list — every chat-mode, tool-calling,
    priced, non-deprecated Gemini/Anthropic model in LiteLLM's catalog
    ships built-in. That took `pkg/pricing`'s table from 12 models to
    31, and it now emits context windows beside the rates. Rejected
    near-misses are printed with their reason, so a model dropping out
    is visible rather than silent.
  - `.github/workflows/pricing-regen.yml` runs it every Monday and
    opens an auto-PR when the catalog has actually moved. Drift is
    detected with `--check`, which normalizes away the regen date — the
    old shape would have opened a no-op date-churn PR every week. No new
    repository settings: the PR is opened by the `go-steer-bot` App,
    whose credentials are already org secrets — `GITHUB_TOKEN` cannot do
    it, both because the org forbids it creating PRs and because pushes
    it authors never trigger the checks `main` requires.
  - Two new invariant tests fail the build when the tables separate:
    every priced model must classify in `pkg/modeltier` and carry a
    context window, and every tier default must be the **latest** model
    in its line. The second one is why the Anthropic defaults moved —
    frontier and mid were two generations behind (`claude-opus-4-7` /
    `claude-sonnet-4-6` with Opus 5 and Sonnet 5 shipped and priced).
  - `gemini-3.5-pro` is gone from `pkg/modeltier` (upstream
    core-agent#786). It is a model id that never shipped — the 3.5
    generation went flash-first — and it had been sitting in the
    classifier since the fork as a needle matching nothing. Harmless,
    but a phantom entry in a hand-maintained table is a claim that
    somebody checked.
  - Prompt-cache *writes* are now priced. Anthropic bills three
    disjoint input buckets — uncached input, cache reads at 0.1x, cache
    writes at 1.25x — and charging writes at the read rate under-reports
    a cache-heavy turn. `Rates.CostUSDWithCacheWrites` takes all three.

  **Tier defaults that moved:** Gemini `small`
  `gemini-2.5-flash` → `gemini-3.5-flash-lite`, Gemini `frontier`
  `gemini-3.6-flash` → `gemini-3.7-flash`, Anthropic `mid`
  `claude-sonnet-4-6` → `claude-sonnet-5`, Anthropic `frontier`
  `claude-opus-4-7` → `claude-opus-5`. The Gemini frontier bump is half
  the per-token price of what it replaces ($0.75/$3.75 per MTok against
  $1.50/$7.50) and was promoted off a live Vertex UAT — all 31 judged
  corpus scenarios ran through it and scored within noise of the
  `gemini-3.6-flash`-era board. That UAT is the bar on purpose: a
  frontier bump taken on the strength of a spec sheet is how you ship a
  parent agent that stops mid-plan.

- **The judged nightly now runs on two providers, on two boards.**
  `.github/workflows/evals-nightly-gemini.yml` runs the same 31 parity
  scenarios against `gemini-3.7-flash` at 07:30 UTC, half an hour behind
  the Claude board. Deliberately a second workflow rather than a matrix
  leg: each provider keeps its own artifact and its own history, so a
  delta is a delta against the same model — one shared run would make
  each provider's night-to-night comparison depend on whether the other
  provider had a good night, and one provider's outage would erase the
  other's baseline. The logic is not forked; `dev/ci/evals-nightly.sh`
  and `dev/ci/evals-nightly-baseline.sh` now take the provider, the
  workflow to read history from, and the artifact name as environment,
  and the two workflows differ only in their env blocks.

  No new repository settings: `roles/aiplatform.user` on the
  impersonated service account already covers both publishers, so the
  existing WIF provider serves both. `vars.MAST_EVALS_VERTEX_REGION`
  defaults to `global` on the Gemini side, which is where the flash line
  is served.

  This is a second *board*, not a second tier ladder — `tier: frontier`
  on Gemini resolves through `pkg/taskclass.ModelForTier`, and
  `J-cost-tier` prices the tiers the product ships rather than the model
  under test. (The two now name the same model; see the model-table
  entry below. They remain separate knobs.)

  **Where both boards stand at this tag.** Fired by hand against the
  shipped tier table so the release quotes a measurement rather than
  last night's, over 31 scenarios each. Claude is run
  [`32029654748`](https://github.com/go-steer/mast/actions/runs/32029654748),
  taken with the Anthropic tool-schema fix above in place; Gemini is
  [`32026851892`](https://github.com/go-steer/mast/actions/runs/32026851892),
  taken before it, which that provider's path does not go through.
  What landed between either board and the tag is docs, derivation-header
  bumps, an eventlog locking fix, the Vertex cache retry, and the watchdog
  port — none of it on the path either board measures. The judged rig's
  link closure does not reach `pkg/watchdog`, `pkg/mcp`, `cmd/mast`, the
  library entry point, or `pkg/providers/vertexcache` at all; of the three
  packages it does share with those commits, `pkg/eventlog` gained a file
  and modified none, `pkg/workload` gained an optional field that is
  absent from the rig's bundles, and the `pkg/providers/gemini` change is
  a comment. The first Claude board of the day is not quoted anywhere: it
  ran before the tool-schema fix and is superseded by this one.

  | metric | Claude | Gemini |
  |---|---|---|
  | `intent_coverage` | 0.973 | 0.951 |
  | `response_quality` | 0.855 | 0.976 |
  | `severity_accuracy` | 0.419 | 0.484 |
  | `effect_ordering` | 1.000 | 1.000 |
  | `exactly_once` | 1.000 | 1.000 |
  | `tool_coverage` | 0.000 | 0.000 |

  Claude is `claude-opus-5` graded by `claude-haiku-4-5`; Gemini is
  `gemini-3.7-flash` graded by *itself*, which flatters it — the board
  prints that warning on every run and its `response_quality` should be
  read with it. **These are report-only**; only `J-cost-tier` gates. The
  two columns are not comparable to each other, and neither is
  comparable to the previous board on the same provider: the models
  under test changed, and the harness refuses to call that a delta.

  Three things the pair says, two of which are not about either model:

  - `severity_accuracy` sits near half on both, and the misses run
    almost entirely one way: 13 of Claude's 18 and 15 of Gemini's 16
    are the run pitching a scenario *higher* than the corpus expects,
    mostly `expected WARNING, got CRITICAL`. Exactly one miss on each
    board goes the other way. Two unrelated model families failing in
    the same direction is a rubric that over-escalates, not a model
    quirk.
  - `tool_coverage` is **0.000 on all 31 scenarios on both boards**,
    every row reading `0/2 expected tool names called verbatim`. It is
    diagnostic-only so it gates nothing, but it scores against the
    upstream corpus's tool *names* and mast's tools are not named that:
    it measures nothing as written.
  - Claude's `response_quality` reads 0.855 against 0.984 on the last
    `claude-opus-4-7` board, and that pair of numbers should not be
    subtracted. Two things changed underneath it: the frontier default,
    and the Anthropic tool-schema conversion the entry above fixes — the
    corpus builds its cluster tools with `functiontool.New`, which is
    exactly the path that was sending Claude a name and no arguments. Of
    the seven scenarios below full marks at this tag, five turn on
    `correct_diagnosis`: the run declines to commit to the root cause
    the corpus expects. The other two are scored *not specific* or *not
    actionable* with the diagnosis accepted. Whether that residue is the
    corpus rewarding confident assertion or the run genuinely
    underperforming is not something one board can say, and this
    release does not claim it either way.

  The first two are findings against the corpus rather than the
  release, and neither is fixed here.

- **New: a workload can wake itself up, and the cadence survives the
  daemon** (#132, W4.1). Until now every run started with somebody
  calling in — an inbound POST, an operator, an external cron holding
  the schedule on mast's behalf. A bundle can now hold its own:

  ```yaml
  edge_trigger:
    scheduled:
      interval: 15m
      jitter: 45s        # optional; defaults to a tenth of the interval, capped at 30s
      prompt: Sweep every namespace for pods in CrashLoopBackOff and report what you find.
  ```

  The cadence is **anchored, not restarted**: fires land on
  `anchor + k×interval`, and the anchor is written to the session store
  the first time the trigger comes up. A daemon that is redeployed twice
  a day therefore resumes the phase it had, instead of quietly moving a
  02:00 sweep to the middle of the afternoon — the failure mode where
  the schedule keeps working and stops meaning what it said. Jitter
  applies to each fire and is re-drawn every time, so it cannot
  accumulate into drift; it exists because N replicas started by one
  rollout waking on the same second is a self-inflicted thundering herd
  against one API server.

  **A tick the daemon was down for is skipped, not caught up.** Coming
  back after an outage that spanned three ticks fires none of them and
  logs one line naming how many were dropped and over what window. A
  periodic run samples the *current* state of the world, so a sample
  nobody took is owed to nobody; the alternative is a crash-looping
  daemon that buys a fresh backlog of model runs at every restart, about
  the crash. (mast's timed-pause scheduler still catches up, deliberately:
  a pause is a promise about one specific parked session that nobody else
  will keep.)

  Each fire opens its own session, named for its tick and listed by
  `mast sessions list` like any other, and runs as
  **`mast:scheduler`** — a namespaced identity no human login can take,
  so an audit row can never read a scheduled run as somebody's. It goes
  through the same chokepoint every other turn kind uses, which is the
  point: a mutating call inside a scheduled run still parks for a real
  approver, still burns the workload's budget, still refuses to start
  while the daemon is draining. Unattended is not unsupervised.

  Fires are counted by `mast_scheduled_fires_total{workload,outcome}`
  (`ran`, `skipped`, `error`, `missed`). A malformed `interval:` or
  `jitter:` is a load error naming the bundle file, not a trigger that
  silently never fires. One daemon per session store, as everywhere
  else in mast — there is no leader election, and two replicas of a
  scheduled workload each keep their own cadence.

- **New: the judgement an operator spends on one call survives as data**
  (#132, W8). Approving, rejecting or correcting a mutating call used to
  leave a log line and a transcript entry — legible to whoever was
  reading the terminal at the time, and to nobody afterwards. The
  highest-signal data a gated fleet produces is exactly this: a human
  looked at `scale_deployment(deployment=api, replicas=10)` and said two,
  or said no. That is a labelled example, and mast was throwing it away.

  Every adjudication the write gate makes is now a durable record on the
  session's event log, and a new subcommand harvests them:

  ```
  mast sessions export-decisions --session-db=/var/lib/mast/sessions.db \
      --workload=gke-triage --since=2026-08-01T00:00:00Z --out=decisions.jsonl
  ```

  The output is JSON Lines: a `_meta` provenance object, then one object
  per decision. Each row carries what the model proposed, what actually
  executed, which of `approve` / `reject` / `edit` the operator chose,
  and whether the call was authorized, refused by the operator, or
  refused by mast — that last distinction because a gate that rejected an
  unattributed edit is not a human saying no, and a dataset that
  conflates them teaches the wrong lesson. A change-set grant being spent
  writes its own row too, so a file cannot show one approval where four
  calls ran.

  **Approver identities are digested by default.** The row carries
  `sha256:` plus sixteen hex characters, which is stable — you can group
  by approver, count how many distinct people approved a class of change,
  or find the identity that approves everything — without naming anyone.
  `--include-approver` opts into raw identities, and `_meta.redaction`
  records which mode produced the file, so a redacted export can never be
  mistaken for a raw one. Machine identities (`mast:internal`) pass
  through in the clear: they name a mechanism, not a person, and hiding
  them would hide the one thing worth knowing about an unattended run.

  **Argument values are exported verbatim, and that is deliberate.** The
  proposed→executed pair is the entire label; strip it and the file
  records only that somebody edited something. So an export is exactly as
  sensitive as the arguments your tools take — namespaces, hostnames,
  anything a remediation call carries. `--out` writes `0600`, the warning
  travels inside the file, and you should treat the result like the
  cluster it describes.

  v0.4 captures and exports. Nothing scores these rows, nothing retrains
  on them, and mast never reads the file back — what you do with it is
  yours.

- **New: `dispatch: bounded` — one cheap model call, a report forced to a
  schema, and a step count you can read off the meter** (#132, W4.3). Some
  workloads exist to answer one question on a schedule, and their whole
  value is that they cannot get expensive. Running them under
  `coordinator` gave you a plausible-looking cheap run and no way to prove
  it: the shape allowed delegation, retries and follow-up turns, so "this
  costs one call" was a hope about a prompt rather than a property of the
  build.

  A bounded workload is a fourth dispatch shape — a roster of exactly one
  `SingleTurn` specialist, built as a single node with no orchestrator
  above it:

  ```yaml
  # workload.yaml
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

  The specialist gets no tools, one turn, and its reply is validated
  against `output_schema:` before the turn ends — a model that answers in
  prose fails the run rather than returning an unparseable paragraph that
  looks like success. `Result.Usage.ModelCalls` is `1`, the daemon logs
  `"session_model_calls":1`, and `mast_model_calls_total` advances by one:
  the count comes off the meter, not off a stopwatch or a token estimate.

  **The refusals are the shape.** A roster with two specialists, a spec
  left in the default Task mode, a spec with no `output_schema:`, or a
  bundle that also enables the planner is a startup error naming what it
  found — the count and the names, or `Task (the default for a spec with
  no mode:)` for the author who wrote nothing. None of it is inferred:
  `dispatch: auto` never picks `bounded`, because a one-specialist roster
  is an ordinary coordinator and a cost ceiling nobody asked for is not a
  favor.

  `examples/workloads/bounded-triage` is the worked example, on the same
  `finding.json` report contract the GKE triage bundle uses.

- **The nightly now checks that a cheap tier is actually billed cheaply**
  (#132, W0.6). "A tiered specialist's tokens are priced at the model it
  ran on" has been true since v0.3 and unit-tested since, but it could
  not be demonstrated end to end anywhere: the offline fakes that make
  the free test tiers credential-free are exactly what collapses every
  tier back onto one model, leaving no two rates to compare. `J-cost-tier`
  runs in the judged nightly — a `tier: small` analyst under a
  `tier: frontier` synthesis, one turn, against real models — and reads
  the meter's per-scope snapshot back:

  ```
  J-cost-tier — every tiered specialist was billed at its own rate (root claude-opus-5 at $0.01500/1K)
    specialist  tier      resolved          calls  tokens  billed    at root   rate/1K
    analyst     small     claude-haiku-4-5  1      848     $0.00254  $0.01272  $0.00300
    _synthesis  frontier  claude-opus-5     1      1094    $0.01641  $0.01641  $0.01500
  ```

  The counterfactual column is the point: "not billed at the parent's
  rate" means nothing without the number it would have been. The check
  also refuses to pass vacuously — a tier that resolved to one model but
  *ran* as another is a finding, so is a specialist that made no call,
  and a roster whose tiers all collapsed onto the root is reported as
  proving nothing rather than as green. Unlike the scores on that board
  it does gate the nightly job, because its verdict is arithmetic over
  the meter's own numbers and does not depend on what the model said.

  That block is transcribed from run
  [`32029654748`](https://github.com/go-steer/mast/actions/runs/32029654748),
  not composed for the changelog. The Gemini board
  ([`32026851892`](https://github.com/go-steer/mast/actions/runs/32026851892))
  measured the same shape against its own ladder — `analyst` at
  `tier: small` resolved to `gemini-3.5-flash-lite` and was billed
  $0.00038 for 274 tokens at $0.00140/1K, where the root
  `gemini-3.7-flash`'s $0.00225/1K would have charged $0.00062.

  On both boards the `frontier` row is a **control, not a measurement**:
  that tier resolves to the model the nightly already runs as root, so
  its rate cannot disagree with the parent's, and the board says so in
  its own notes rather than counting it. One real scope per provider is
  what this claim rests on. Worth recording that the Gemini side proved
  nothing at all until the tier table moved: on the previous table
  `small` resolved to `gemini-2.5-flash` at exactly the root's
  $0.00060/1K, so the counterfactual equalled the billed figure on
  *both* rows and the check was vacuous on that provider — passing, and
  empty. It is a measurement there for the first time in this release.

- **New: a specialist can declare how much model it is worth, not which
  vendor's** (#132, W1.1a). `model: claude-haiku-4-5` in a specialist's
  frontmatter says two things at once — "this step is cheap" and "this
  bundle runs on Anthropic" — and only the first one is usually meant. A
  spec can now say the first alone:

  ```yaml
  ---
  description: Inspects pod state and returns a 3-bullet digest.
  tier: small          # small | mid | frontier
  ---
  ```

  Mast resolves the tier against the provider it is actually running —
  the `--provider` alias when you passed one, otherwise the root model
  id's own prefix, which is the same dispatch `--model` already makes —
  through `pkg/taskclass.ModelForTier`. `tier: small` is
  `gemini-2.5-flash` under a Gemini root and `claude-haiku-4-5` under an
  Anthropic one, so the roster reads the same and costs the right thing
  on either backend. A root model whose provider can be told from
  neither fails startup asking for `--provider` rather than guessing a
  vendor and billing you for the guess.

  Every property `model:` overrides already had carries over: an
  unresolvable tier fails startup instead of quietly inheriting the
  parent's model, resolution is memoized per resolved id so twelve
  `tier: small` analysts open one client, and the offline fakes
  (`echo`, `scripted`, `toolactor`) collapse every tier back to the fake
  so a tiered bundle still runs with no credentials. Startup logs each
  tier next to the id it resolved to.

  **Metering follows the tier.** A tiered specialist's tokens are priced
  at the model it actually ran on, not the parent's rate — build and
  pricing now go through one `SpecModelName`, because the alternative is
  a bundle that runs cheap and bills expensive, which is the same
  fiction in the other direction.

  `model:` and `tier:` on one spec is a load error naming the file, not
  a precedence rule. The shipped `ns-audit` bundle is the worked
  example: four `tier: small` namespace analysts under one `tier: mid`
  synthesis step.

- **New: one answer can approve a whole change set — bounded by a clock the
  bundle sets and a re-read the bundle declares** (#132, W7). A remediation
  is usually several calls: scale two Deployments, or patch a ConfigMap and
  restart what reads it. Until now each one parked on its own, so a
  three-call fix was three questions, and an operator who had already read
  the plan answered the same question three times.

  Answering a parked call with `{"verdict":"approve","scope":"change_set"}`
  now mints a grant for **each remaining call in that finding's proposed
  change set**. The per-call gate spends them silently. A grant authorizes
  one exact `(tool, arguments)` pair — not the tool, not the verb — and is
  single-use, durable across a restart, and re-checked before it fires. A
  deny policy still adjudicates first, and a spent grant is still recorded
  as an allow-once decision in the audit log: the grant removes the
  *question*, never the accounting. If any call in the set cannot be granted
  the whole verdict is refused with a reason, rather than quietly covering
  the part that fits — a partial "approve all of this" is the one outcome
  the operator did not choose.

  **What makes an approval go stale is the cluster, not the clock.** A set
  approved twenty seconds ago against a Deployment somebody has since scaled
  by hand is stale; one approved an hour ago against an untouched object is
  not. So a tool in `tool_catalog` can declare its own freshness re-read:

  ```yaml
  - name: scale_deployment
    mutating: true
    precondition:
      read: get_deployment            # must be declared mutating: false
      args_from: {name: deployment}   # the read's "name" <- this call's "deployment"
      fields: [output.replicas]
  ```

  The read runs at approval time and again just before the granted call
  fires. If a watched field moved, the grant is voided and the call goes
  back to the operator with a question that names the field and both values
  — `output.replicas was 1 at approval and is 5 now`. `hitl.change_set_ttl` (default `10m`)
  is the backstop for everything a precondition cannot see. A tool that
  declares no precondition gets the TTL and nothing else, and its parked
  question says so rather than implying a check mast is not making.

  Two things worth knowing before writing your first `precondition:`. Each
  call is checked against **its own** object — that is what `args_from` is
  for; a precondition watching the field the set itself rewrites would have
  call 1 invalidate call 2 by succeeding. And `fields:` paths start
  `output.` for MCP tools, because that is how the structured result of an
  MCP tool arrives.

  Proven offline in `scripts/uat-v0.4.sh` (`U-changeset`, five legs: the
  set, a `scope: once` control, cluster drift, the expiring window, and a
  SIGKILL between the question and the answer), and against a real API
  server by the new opt-in live tier below.

- **New: an opt-in live acceptance tier over a throwaway kind cluster**
  (#132, W7). `MAST_LIVE_KIND=1 ./scripts/live-kind-v0.4.sh` runs the
  change-set legs against a real Kubernetes API server: one approval scales
  two Deployments, and a second leg has a person run `kubectl scale` out of
  band while an approved call is held open, which voids the next grant and
  re-parks it. Two claims need this — that a declared `fields:` path is
  right about a real tool's real JSON, and that "the cluster moved" means
  somebody else moved it — and neither can be staged by a harness writing
  its own fixture file.

  It creates its own cluster, refuses to adopt or delete one it did not
  create, writes a single-context kubeconfig under `/tmp`, and never
  resolves the ambient `current-context`; the MCP fixture behind it repeats
  those checks itself before it will start. It needs a container runtime and
  takes minutes, so it is **not** part of `dev/ci/presubmits/e2e.sh` and does
  not gate the build.

- **New: a finding carries the call it recommends, and that is the call the
  operator approves** (#132, W7.0). Until now a diagnosis recommended a
  remediation in prose — "scale the api Deployment back to 2 replicas" — an
  operator agreed with the prose, and the change executor composed a tool
  call from it on a later turn. What reached the cluster was a call nobody
  had looked at. Prose cannot be approved, only agreed with.

  A workload's report schema can now declare `proposed_change`: a
  possibly-empty list of `{tool, arguments}` entries. Every entry has to
  name a tool the workload actually declares and carry arguments that fit
  *that tool's own* input schema, and both are checked at the moment the
  report is returned — a report that names an unknown tool, or arguments the
  tool would reject, comes back to the specialist as an error it can correct
  rather than failing the run. The check is the same code that validates an
  operator's edit at the gate, so the two cannot drift apart on what
  "schema-valid arguments" means. `examples/workloads/gke-triage` declares
  the field on all twelve diagnosers; `recommended_actions` stays, because
  "what should happen" and "what call makes it happen" are different
  questions and an empty proposal still needs prose.

  **Empty is a first-class answer.** "Raise the memory limit, but I don't
  know to what" is an honest diagnosis, and a specialist that cannot name an
  exact call says so in `escalate` instead of inventing a plausible one to
  fill a field. Nothing changes for a roster that does not declare the
  field.

  Under graph dispatch the diagnoser→executor edge becomes structural: a
  finding with a non-empty change set that the operator approved routes to
  the change executor, which receives those calls verbatim. The record is
  durable per specialist, because a confirmation resume re-enters the graph
  at START and re-runs the nodes above it. A roster with two change
  executors logs an error and leaves the edge unwired rather than guessing.

  End to end, offline, in `scripts/uat-v0.4.sh`: the parked question quotes
  `replicas=2` — not the `replicas=10` the same model proposes when it picks
  a call for itself — and approving it runs `apply_change` exactly once,
  with `replicas=2` in the tool's own ledger.

- **New: `GET /sessions/{id}/guardrails` + `POST /guardrails/reset` — and a
  way out of a budget trip that didn't exist before** (#135). mast's cost
  ceilings are real: `budget:` in the bundle bounds the session, `budget:`
  on a specialist bounds that specialist, and the meter cancels the run
  context the moment either is crossed. What was missing was any recovery.
  Enforcement is re-derived from accumulator-vs-ceiling on every priced
  event, with no flag in between — so a session past its cap crossed it
  again on the first event of the next turn, forever. The only cure was
  restarting the daemon, which drops every *other* session's in-flight turn
  with it. Unattended is exactly where nobody is watching for that.

  The read answers "what is armed, what tripped, why" in all three of mast's
  dimensions — cost, tokens, and model calls — plus a per-specialist
  breakdown, because a session halted by `max_turns: 40` has spent six
  tenths of a cent and a dollars-only view sends the operator looking for
  the wrong cause. Wire names and status codes are core-agent's so one
  client speaks both daemons.

  "Reset" means raising the ceiling, never zeroing the accumulator: /usage,
  the eventlog-derived cost, and the ceiling check keep counting the same
  dollars, so a post-incident review of a session that spent $40 doesn't
  find it reporting $10. A reset that provably wouldn't survive the next
  turn is refused with 409 and the numbers rather than performed — and the
  check runs before anything is mutated, so a refusal costs the operator
  nothing. A grant never *imposes* a ceiling: "+5 turns" on a session with
  no turn cap would otherwise cap it at 5 and wedge it four calls later.

  Turn errors now classify as `cost_ceiling` with the reset endpoint in the
  hint; the watchdog is reported `advisory: true`, because mast's only logs.

- **New: `GET /sessions/{id}/subagents` — the roster the daemon loaded**
  (#134). The route block in `pkg/attach/handlers.go` has advertised this
  endpoint in a comment since the attach surface landed and never registered
  it, so mast-web's specialists view 404'd against every mast daemon.
  `/agents` was the only roster read available, and it lists *spawned*
  instances — always empty on mast, since every dispatch shape resolves its
  specialists inside the turn. "What is running" is the wrong answer to
  "what can this thing do".

  Entries are wire-compatible with core-agent's `SubagentCatalogInfo`
  (`name`, `description`, `model`, `root`, `modes`) plus three mast-native
  fields. `modes` carries only what is true in core-agent's vocabulary —
  `["sync"]` under planner dispatch, where `invoke_specialist` really is a
  parent tool, and empty otherwise; mast has nothing spawnable by reference,
  so `"async"` is never emitted. Claiming a mode to make the field look
  populated is the upstream defect the issue warned about (core-agent#741).

  What `modes` cannot say, `invocation` does: `parent_tool`, `transfer`,
  `graph_node`, or `fanout_branch` — how the composed root actually reaches
  this specialist. **Empty means nothing in this shape reaches it**, which
  is how a roster orphan becomes visible without reading `internal/compose`
  (the `change-executor` orphan the first live GKE run found was exactly
  this). `capability` and `agent_mode` carry the declared read/write half
  and the ADK mode, spelled as the frontmatter spells them.

- **Fixed: `GET /sessions/{id}/tools` answered "200, no tools" on every mast
  daemon** (#133). `attachadapter.Config` has carried a `ToolsFn` since the
  attach surface landed; `cmd/mast` never set it, so the endpoint took the
  nil-means-empty path and returned a well-formed empty list — a read that
  looks like an answer. Any operator client asking a mast daemon what tools
  it holds got told: none.

  The daemon now projects the MCP toolsets it wired at composition time,
  which is the only place the per-server attribution survives — ADK exposes
  no accessor for a built agent's tools, so by the time the adapter holds an
  agent, "which server did this come from" is already gone. Each entry
  carries `source: "mcp"`, the server name, and a `gate_state` derived from
  the mutation predicate and `hitl.on_mutation`: `allowed` for a read (or a
  write under `apply`), `prompted` under `require_approval`, `denied` under
  `dry_run`. Listings are cached for 30s and bounded at 5s per refresh; one
  unreachable server is omitted and logged rather than blanking the catalog,
  and an outage that takes down every server serves the last good answer
  instead of caching the emptiness.

  Not yet listed: mast's non-MCP tools (the planner's dispatch calls), which
  are registered inside `internal/compose` with no handle surviving the call.
  A second hand-maintained list would drift, and a wrong catalog is worse
  than a partial one.

  The regression guard is a reflection test over the rendered
  `attachadapter.Config`: a new func field on it fails `cmd/mast`'s tests
  until serve either wires it or records why it can't. The bug was not a
  broken projection — it was an assignment nobody made, and nothing asserted
  on the served artifact.

- **Fixed: every GitHub Release mast has ever cut published with an empty
  body.** The notes were composed from this file by `dev/release/notes.sh`,
  printed in full to the workflow log, and passed to GoReleaser as
  `--release-notes` — and then dropped, because `.goreleaser.yaml` set
  `changelog: disable: true` and GoReleaser loads the release-notes file
  *inside* the changelog pipe it was thereby skipping. The config comment
  said the two went together; they are mutually exclusive. Leaving the pipe
  enabled is what honours the flag, since the file short-circuits it before
  any git-log changelog is generated. v0.3.0's body was backfilled by hand;
  v0.1.0-pre through v0.2.0 are still empty.

  The release workflow now reads the *published* release back and fails if
  its body is under 200 characters. Every check that existed passed on every
  one of those releases, because each one checked the step before the one
  that broke: the notes script was unit-tested, the compose step echoed
  correct output, and the dry run never publishes and so never had a body to
  look at.

## v0.3.0 (2026-08-14)

- **New: the write gate — a tool call that changes anything stops and asks,
  once per call, and the pause survives `kill -9`** (docs/v0.3-plan.md
  W2.1–W2.3; parity rows 9–11). Approval used to be per *turn*: a specialist
  finished, its whole result parked, and approving it authorized whatever it
  did next. The gate is now in front of the call. `hitl.on_mutation` takes
  `require_approval` (the default), `apply` (run it, audit it) or `dry_run`
  (never run it, report what would have run); the older `require_approval:
  true` spelling still parses, and declaring both forms is refused rather than
  silently resolved.

  What counts as mutating is a declaration, not a guess. mast reads the
  built-in annotation where a tool has one, and otherwise the workload's
  `tool_catalog.tools[].mutating` — **a tool named nowhere counts as
  mutating**. That default is load-bearing rather than cautious: ADK's
  mcptoolset drops MCP's own `readOnlyHint` annotations on the way through, so
  for an MCP tool the catalog is the *only* place the answer can come from,
  and the failure mode of guessing "read-only" is a silent unreviewed write.
  The cost is that adding a read tool without classifying it makes it stop and
  ask.

  Every gate decision is an audit record — `awaiting_approval`, `apply`,
  `dry_run`, `denied_by_policy`, `denied_by_operator` — carrying the tool, the
  arguments, the specialist, and the approver. The approver is taken from the
  authenticated caller on `/resume`, never from the payload: a body claiming
  `"approver":"sre-oncall"` over an unauthenticated connection has that field
  overwritten, because an approval whose author is self-asserted is not an
  approval. And an approval is `AllowOnce`. A verdict that tries to widen
  itself to the session is refused and audited as
  `approval_scope_refused` — approving one `delete_k8s_resource` cannot
  become standing permission to delete.

- **New: a specialist declares whether it may change anything —
  `capability: read_only | change_executor`** (docs/v0.3-plan.md W2.4; parity
  row 12). Defaults to `read_only`. A roster where a `read_only` specialist
  can reach a mutating tool is refused at construction
  (`internal/compose.CheckCapabilitySplit`), on every path that builds a root
  and under every dispatch shape, so the check cannot be dodged by entering
  through a different door. The shipped `gke-triage` roster was rebuilt around
  it: the twelve diagnosers gave up their write tools and name the remediation
  in their finding, and one new `change-executor` specialist holds the write
  surface behind its own report contract (`schemas/change-report.json`).

  This replaces a boundary that used to live in prompt text — "diagnose only,
  do not remediate" in a specialist's `.tmpl`. A prompt is a request; a
  construction-time refusal is a property. Rosters that relied on the prose
  version keep working only if they now say so in the field.

- **New: an operator can correct a call instead of approving it —
  `{"verdict":"edit"}`** (docs/v0.3-plan.md W2.5; parity row 13). The resume
  payload is three-valued: `approve`, `reject`, or `edit` with replacement
  `args`. An edit is checked before it runs — attributable to an
  authenticated approver, restricted to arguments the tool declares,
  schema-valid against that declaration, and re-adjudicated against policy, so
  an edit cannot turn a permitted call into a forbidden one. A tool that
  declares no input schema cannot be edited at all. What was applied is
  written to the session as a durable `AppliedEdit` record and printed by
  `mast sessions show`, because ADK re-issues the *original* call on resume:
  without that record the log would say the operator approved arguments that
  never ran.

- **New: the deploy base mirrors the capability split in Kubernetes RBAC**
  (docs/v0.3-plan.md W2.6; parity row 14). `deploy/base` grants the daemon
  cluster-wide read and nothing else, with secrets excluded; the permission to
  change a namespace is a separate, per-namespace apply under
  `deploy/remediation-target/`. `scripts/rbac-matrix.sh` proves both halves
  against a live or dry-run cluster.

  **Read the caveat before trusting the split on GKE.** GKE authorizes a call
  if *either* IAM or RBAC allows it, and mast reaches the cluster through the
  GKE MCP server as its Workload Identity principal, which
  `scripts/setup-wif.sh` binds to `roles/container.admin` — so today IAM, not
  RBAC, is what bounds the daemon, and the RBAC split documents intent rather
  than enforcing it. The narrowing (`WRITE_SCOPE=namespaced`, dropping the
  binding to `roles/container.viewer` and carrying writes on the namespaced
  Role) ships as an opt-in rather than the default, because the
  WIF-principal-to-RBAC-subject mapping has not been verified against a live
  cluster.

- **Fixed: the shipped `gke-triage` catalog named three tools the GKE MCP
  server does not have.** `rollout_undo`, `patch_resource` and `scale_resource`
  are not tools `https://container.googleapis.com/mcp` offers; its 23 real
  tools are now enumerated, each classified to match its own `readOnlyHint`,
  verified against a live `tools/list`. This mattered more than a typo
  normally would, in both directions. ADK's allowlist predicate is a map
  lookup on the tool name, so an allowlisted name the server does not offer is
  dropped **silently**: `change-executor` would have started, declared its
  write capability, passed the capability-split check vacuously, and held zero
  write tools. In the other direction, sixteen genuinely read-only tools were
  absent from the catalog, so default-deny-unknown would have parked ordinary
  reads at the write gate. Found by pointing the workload at a real cluster —
  no offline test could have caught it, since the fakes answer whatever is
  declared.

- **Known limitation: the change executor is operator-invoked, not
  automatic** (docs/v0.3-plan.md W7.0). The gate above is real, and the
  executor is the only specialist that could reach it — but in both shipped
  dispatch shapes an incident ends at a finding. A diagnoser names its
  remediation in prose, graph nodes are terminal, and the coordinator is
  instructed to return analysis only, so nothing hands the diagnosis to the
  executor. Closing that needs the finding to carry a *typed* proposed change
  rather than a sentence — scoped as W7.0 and deliberately not patched with
  coordinator prompt text, which is the same prompt-held boundary W2.4 just
  finished deleting. What v0.3 claims is the narrower statement: a remediation
  call cannot fire without an operator, and that survives `kill -9`.

- **New: `dispatch: fanout` — the whole roster runs concurrently over one
  incident, one synthesis specialist merges the findings, one approval gate
  on the merged report** (docs/v0.3-plan.md W3; parity row 7). A bundle sets
  `dispatch: fanout` and names a `_synthesis` specialist; every other
  specialist becomes a branch, run under a `fanout.max_concurrency` cap
  (default 4, `-1` for unbounded). Synthesis receives each analyst's reported
  payload plus an explicit list of who returned nothing — silence is
  reported, never filled in. `examples/workloads/ns-audit` is the shipped
  example: four read-only namespace analysts plus synthesis.

  Fan-out branches are structurally read-only, checked at construction. A
  roster whose analyst can reach a mutating tool is refused before the daemon
  serves, and so is one that grants an MCP server without enumerating its
  tools — under mast's default-deny-unknown predicate an un-enumerated grant
  *is* a grant of mutating tools. `request_operator_input` is refused too:
  every branch runs before the one approval gate, and a branch has no pause
  the outer graph could record. The shipped `gke-triage` roster is
  deliberately one of the refused ones — it holds `patch_k8s_resource`, which is
  why fan-out ships a second example rather than converting the anchor.

- **Fixed: an agent inside a parallel branch could not see its own tool
  results.** Not a regression — nothing shipped on that path before — but
  worth stating, because it decided the shape above. `workflow.ParallelWorker`
  suppresses every branch event that is not an output event, and an LLM
  agent's working memory *is* the session event list, so a branch agent's
  second model call received a prompt identical to its first: it re-issued
  the same tool call and looped until something cancelled it. The fan-out is
  built on ADK's `parallelagent` instead, which delivers branch events to the
  runner. Three things follow: a tool-using analyst works, per-specialist
  `max_turns` / `max_cost_usd` are enforced inside a branch (the meter
  buckets by event author, and a suppressed event is an unmetered one), and a
  branch's work is in the durable log where crash recovery can see it.
  Approving a merged report resumes without re-running any analyst, including
  after the daemon has been restarted.

- **Fixed: the shipped `gke-triage` example could not complete a run offline**
  (docs/v0.3-plan.md W1.4). Since `output_schema:` landed, the twelve
  diagnosers declare a report contract that becomes their `finish_task`
  parameters; the offline fake models (`--model=echo`, `--model=toolactor`)
  answered with a hard-coded `{"result": "<text>"}`, which violates it. The
  call was refused, retried unchanged, and the run died on the specialist's
  newly-enforced `max_turns` — `POST /inject` returned **500** on the bundle
  the README and `scripts/demo-spike2.sh` point at. Neither the schema work
  nor the budget work is individually at fault; the composition is, and
  nothing caught it because no test drove the shipped bundle end to end.

  The fakes now read the `finish_task` declaration off the request and
  synthesize a value for every property in it (`pkg/agent/schemafill.go`), so
  a roster can change its report shape without anyone touching a fake. Where
  no `output_schema:` is declared, ADK's default single-`result` wrapper is
  recognized and the previous text goes out unchanged. Two smaller fixes fell
  out: the echo model's unschema'd digest read `diagnosed from envelope:`
  with nothing after the colon when a specialist was reached as a
  workflow-graph node (the incident is now recovered from the whole history,
  not just the last user message), and `--model=toolactor` answered a
  classifier turn with prose, which routed every incident to `_fallback`
  instead of to its specialist.

- **New end-to-end acceptance harness: `scripts/uat-v0.3.sh`**
  (docs/v0.3-plan.md W1.4, tier U). It drives the **shipped**
  `examples/workloads/gke-triage` bundle — not a fixture — under
  `--dispatch=graph --model=echo`, credential-free and network-free, and
  asserts on what an operator actually receives: the report quoted in the
  approval prompt that `mast sessions show` prints. Three legs, 29
  assertions: a declared `output_schema:` is answered in full (every
  property, a valid enum member, one `finish_task` call, no budget error, and
  the routed specialist named rather than `_fallback`); the same roster with
  the schema removed falls back to the default wrapper; and a deliberately
  violating report is *refused* and reaches the gate with no result. The last
  two are what make the first a statement about mast rather than about the
  fake. `dev/ci/presubmits/e2e.sh` now runs this alongside the v0.2 spine —
  both, in order — so the durable-execution harness is added to, not replaced.

- **Per-specialist budgets are enforced, not just parsed**
  (docs/v0.3-plan.md W1.2). A specialist's declared `max_turns` and
  `max_cost_usd` are now ceilings on that specialist's own spend. The roster's
  declarations become per-agent scopes on the session meter
  (`internal/compose.MeterScopes` → `budget.Config.Scopes`), and
  `budget.Meter.Observe` buckets each usage event by its author, checking it
  against that specialist's ceiling and against the workload's. Composition is
  tightest-cap-wins by construction rather than by arithmetic — whichever
  ceiling a call crosses first stops the run — and when one call crosses both,
  the error names the specialist, because that is the more specific fact and
  the one an operator acts on. A specialist running on its own `model:` is
  priced at that model's rate, so a cheap analyst's tokens are no longer
  billed at the synthesizer's.

  Attribution is by `session.Event.Author`, which carries the agent's name on
  every dispatch shape mast builds — a coordinator's sub-agent tool, a
  workflow-graph node, the planner's `invoke_specialist`. The design docs had
  named `Event.Branch` as the eventual seam; it is empty in the
  coordinator/sub-agent-tool shape, so a branch-keyed meter would have metered
  nothing there while passing under graph dispatch. Corrected in
  `pkg/specialists`, `pkg/graph` and `docs/specialists-design.md`.

  Two limits, recorded in the `pkg/budget` package doc rather than left
  implicit: metering reads the event stream, so a ceiling is crossed *by* the
  call that reports it (the cap bounds spend within one call's overshoot, it
  does not pre-authorize a call); and a crossed specialist ceiling stops the
  session rather than handing the coordinator a refusal it could route
  around. Both need a seam in front of the model call.

- **Specialists can declare a report contract: `output_schema:`**
  (docs/v0.3-plan.md W1.3). The field names a JSON-Schema document
  (`.json`/`.yaml`/`.yml`) **relative to the `.tmpl` file's own directory** —
  a shared file rather than an inline block, because a report shape is a
  contract with its consumers and inlining it makes that contract private to
  one specialist. `pkg/specialists/schema.go` reads, type-normalizes and
  checks the document at *load* time, so a malformed contract fails the roster
  on startup instead of on the first turn that dispatches to it. Refused at
  load: unknown keys (`propertys:` constrains nothing), untyped nodes, arrays
  without `items`, objects with no `properties`, `required` names that are not
  properties, and a non-object top level.

  Enforcement is ADK's, and it is the same on both agent modes: in `Task` mode
  the schema becomes the `finish_task` declaration and a non-conforming call
  comes back as a validation error naming the missing key; in `SingleTurn`
  mode the reply is validated on the way out and the delegation is refused.
  Either way the model that asked sees the refusal and no result, so
  non-conforming output cannot become a specialist's answer. Mast gains no
  `Finding` Go type — the shape is a workload asset.

  The `gke-triage` bundle ships that asset:
  `examples/workloads/gke-triage/schemas/finding.json`, referenced by all
  twelve diagnosers (the `triage-classifier` router emits a token, not a
  report, and is deliberately unschema'd). The test asserts the twelve resolve
  to *one* file — twelve individually-valid schemas would satisfy "every
  diagnoser is typed" and still be the drift a shared asset exists to prevent.

  Fixed while wiring the deploy mirror: `50-statefulset-daemon.yaml` projected
  3 of the 13 specialists the ConfigMap carries. A ConfigMap `items:` list
  projects only what it names, so nine failure modes routed to `_fallback` in
  a real deployment with nothing in the logs to say a dedicated specialist
  existed.

- **Judge tier: the 31 parity scenarios are scored against a live provider**
  (docs/v0.3-plan.md W0.5). `internal/evals/judge` stands up a fixture cluster
  per scenario over lookout's read-only tool surface, runs one SRE agent
  against it, and grades the answer with upstream's `_QUALITY_PROMPT`
  verbatim — `{reasoning, score 1-5, specific, actionable, correct_diagnosis}`
  normalized to `(score-1)/4`. `evals --tier=judge`, with `--model`,
  `--grader`, `--provider`, `--out` and `--baseline`.
  `.github/workflows/evals-nightly.yml` runs it at 07:00 UTC against
  Anthropic-on-Vertex over keyless WIF and posts the board plus a delta
  against the previous night's artifact. It **reports and never gates**: no
  score, however low, can produce a non-zero exit — only "a scenario did not
  run" or "a metric scored nothing anywhere" can.

  This flips the last W0 parity row. First numbers over 31 scenarios against
  `claude-opus-4-7`: `intent_coverage` 0.917, `response_quality` 0.968,
  `severity_accuracy` 0.484, `tool_coverage` 0.000 (diagnostic — mast's read
  surface shares no tool *name* with the corpus, which is the whole reason
  `intent_coverage` is the primary metric), `effect_ordering` and
  `exactly_once` vacuous on every row because a read-only surface cannot
  produce a mutation to order.

  Structural ceilings are reported, never folded into the score: LC-13 expects
  a write tool, so its `intent_coverage` is capped at 0.50 and it scores 0.50.

  Pointing `--model`/`--grader` at compose's `echo` fake runs all 31 rows in
  ~2.5s with no credentials, so the tier's plumbing is exercised in CI without
  spending a metered run. That dry run is what caught the grade parser
  matching `(?s)\{.*\}` — greedy from the reply's first brace to its last, so
  a restated JSON contract or a quoted `{key='node-role', …}` made every row
  report "unparseable", which on a board is indistinguishable from a real
  failure. Now a brace-depth scanner taking the last complete object.

  Two defects the first live board found, both fixed before shipping: a tool
  that answered a scenario but whose half of the fixture was empty returned a
  bare header, which the model read as broken tooling and refused to diagnose
  around — an empty half now says the same thing a non-answering tool says.
  And the fixture-override rule was drawn at "only where the quoting rule is
  silent", which admitted rows whose sole derived observation was a bare
  identifier; it is now drawn at displacement — an override may enrich a thin
  fixture, but every span the quoting rule vouched for must still reach the
  agent.

- **ADK upgraded to v2.2.0** (from v2.1.0), ahead of the v0.3 workstreams that
  build on the code it changes.

  **Fixed by the upgrade: a cancelled workflow-graph run reported success.**
  Under v2.1.0 the scheduler reacted to invocation-context cancellation only in
  its `doneChan` select arm, and a ready `doneChan` does not have to win against
  a queued node completion — `cancelAll` never touches the parent context, so a
  run could drain and return cleanly after being cancelled. mast cancels
  in-flight turns from outside the graph (attach session eviction, the dispatch
  deadline, daemon-level abort), so this was reachable in shipped builds.
  `TestGraphRunSurfacesExternalCancellation` pins it, and was verified to fail
  on v2.1.0.

  Also improved: confirmation resume is now deterministic (re-dispatch follows
  request order instead of Go's randomized map iteration, which made the
  resumed calls, the assembled response, and the last-writer-wins `StateDelta`
  merge vary run to run) and no longer re-dispatches a call whose response is
  already in the event snapshot — i.e. no running a tool twice inside one
  resume. `memory.InMemoryService.Search` no longer races concurrent writes.

  **Wire-visible:** ADK put JSON tags on `session.Event`, `EventActions`, and
  `LLMResponse` — PascalCase to camelCase with `omitempty`. The attach `agent`
  frame embeds that struct verbatim, so the change is visible to SSE consumers;
  mast-web already reads both casings, and stored sessions decode either way
  (`encoding/json` matches field names case-insensitively). Attach protocol
  stays at 1.4.0: mast never specified that payload's shape.

  The bump drags dependencies ADK's own go.mod requires: MCP go-sdk 1.4.1 →
  1.7.0, genai 1.63 → 1.66, glebarez/sqlite 1.8.0 → 1.11.0, jsonschema-go
  0.4.2 → 0.4.3, gorm 1.31.0 → 1.31.2, grpc 1.82.1 → 1.83.0, plus otel/api/
  genproto. Note for the v0.3 approval work: MCP 1.7.0 enables SEP-2322
  multi-round-trip client middleware by default, which auto-fulfills
  server-initiated input requests. mast registers no elicitation or sampling
  handler, so such a request errors today rather than self-fulfilling — but a
  handler added later would satisfy server input requests without traversing
  mast's approval gate. `MultiRoundTripOptions.Disabled` is the off switch.

  Not fixed upstream: adk-go#1229 (the concurrent-append affordance tracked in
  #51). `session/database` is untouched in v2.2.0, so the ops-row convention
  stays.

- **The parity eval suite is a CI gate** (docs/v0.3-plan.md W0.4).
  `scripts/evals.sh` → `dev/ci/presubmits/evals.sh` → an `evals` job in
  `.github/workflows/ci.yml`, wired into `all.sh` so local == CI. Runs
  credential-free in a couple of seconds and prints one board. Exit 0 green, 1
  a scenario missed its declared outcome or a metric scores nothing, 2 the
  harness could not run. `--format=json` emits the same report in the shape
  W0.5's nightly will diff run-to-run.

  The deterministic tier does **not** score the 31 ported scenarios — a
  scripted provider does not choose, so replaying a fixture would assert only
  that the script equals itself. What it gates instead is that the measurement
  works: `CorpusReach` reports how many scenarios each metric can score at all,
  and a gating metric that scores nothing anywhere fails the run. Both of the
  upstream harness's custom-code evaluators are constant functions today, and
  the test suite reproduces both defects to prove the guard catches them.

  The expected-fail allowlist is the report's headline (`3 of 5` today), each
  entry printed with the workstream that removes it. There is deliberately no
  second allowlist file: `differentiators.Scenario.Expect` is the one record,
  and it is checked in both directions.

- **Fixed: the eval rig inherited ADK's default GORM logger**, emitting ~48KB
  of SQL chatter per suite run. `pkg/eventlog.Open` has silenced this since
  v0.1, but `internal/evals/differentiators` builds ADK's session service
  directly and never picked it up.

- **The five differentiator scenarios** (docs/v0.3-plan.md W0.3).
  `internal/evals/differentiators` drives the composed runtime — the
  `internal/compose` root, the `pkg/effects` outbox, a `pkg/budget` meter, a
  real SQLite session store — with a scripted model and checks one invariant
  per scenario: `E-exactly-once`, `E-ambiguous-refusal`, `E-budget-exhaustion`,
  `E-approval-rejected`, `E-approval-edited`. These are the situations the
  upstream harness structurally cannot express, because it scores a single
  uninterrupted trajectory against a list of expected tool names.

  Two of the five pass today; three are declared expected-fail against W1.2 and
  W2. The declaration is checked *in both directions*, so landing a capability
  without flipping its entry fails the suite by naming the entry to remove. A
  scenario has three outcomes, not two — Pass, Fail, and **Broken**, the zero
  value, for a fixture that could not produce a run. Broken is never
  allowlistable, and every scenario must return both a stated reason and a
  non-empty trace, so "the capability is missing" is always an observation
  about a run that happened rather than the absence of one.

- **Fixed: `TraceFromEvents` counted task delegations as effects.** The walk
  excluded engine control-flow and sub-agent-named calls, but only on the call
  side; a delegation's completion then had no recorded call to pair with and
  fell through the orphan-completion fallback, classified mutating by
  default-deny. Two delegations to the same specialist scored as one effect
  applied twice, and every trace's tool set was inflated with `finish_task` and
  the specialist's own name. The exclusion now applies to both sides.

- **Deterministic evaluators** (docs/v0.3-plan.md W0.2). `internal/evals` scores
  a recorded run with no provider and no cluster: `intent_coverage` (the primary
  trajectory metric), `tool_coverage` (name-level, emitted as a diagnostic only
  so the consolidation penalty stays visible instead of scored),
  `severity_accuracy`, and the two mast-only invariants `effect_ordering` and
  `exactly_once`. `TraceFromEvents` is the single adapter onto ADK's event log;
  everything downstream is a pure function over a plain struct, so a test can
  construct a double-fired mutation or an orphaned completion directly.

  Its pairing rules mirror `pkg/effects` — same event indexing, same empty-ID
  skip, same treatment of a confirmation placeholder as *not* a completion, so a
  declined approval never scores as an executed effect. One deliberate
  divergence: long-running calls are kept rather than deferred, because a
  finished run's completed blocking tool is an effect like any other and
  dropping it would blind `exactly_once` to a re-fired mutation.

  Two guards exist specifically because upstream's equivalents are constant
  functions on this dataset: every result carries a `Vacuous` flag when it
  scores 1.0 for want of anything to measure, and `exactly_once` keys effect
  identity on tool name plus canonicalized arguments rather than call ID, which
  would make it structurally incapable of failing.

- **Parity scenario corpus and intent table** (docs/v0.3-plan.md W0.1, W0.1a).
  The 31 LangChain SRE-agent evaluation scenarios are ported to
  `testdata/evals/scenarios/langchain-sre.jsonl`, and `testdata/evals/intents.yaml`
  maps their 23 distinct tool names onto 19 diagnostic intents plus the lookout
  tools that satisfy each. `internal/evals` loads and validates both. Nothing
  is scored yet — the evaluators are W0.2 — but the corpus and the mapping the
  scoreboard will be computed from now exist and are tested.

  The mapping is at *intent* level, not tool-name level, because lookout
  consolidates: 22 of the 31 scenarios are fully answered by a single lookout
  call, and name-level set overlap would score a better-factored read path as a
  regression. Three properties of the upstream data are recorded rather than
  smoothed over: the corpus is ported from the `.jsonl` (what upstream actually
  uploads and scores) even though its `.json` sidecar is an unwired *repair* of
  it; 7 of the 23 tool names do not exist in upstream's own registry, so 16 of
  71 tool references are unsatisfiable and are annotated `unreachable_upstream`;
  and both upstream custom-code evaluators are constant functions on this
  dataset, so neither provides an adoptable baseline. See the plan's W0.1/W0.1a
  findings for the detail.

- **Per-specialist model overrides are honored** (docs/specialists-design.md
  open Q#4; docs/v0.3-plan.md W1.1). A specialist `.tmpl` has always been able
  to declare `model:`, and `pkg/specialists` has always parsed it — but `Build`
  constructed every specialist with the parent's model, so the field did
  nothing and "cheap analysts, frontier synthesis" was unreachable. `Build` now
  resolves the override through a `specialists.ModelResolver` that
  `internal/compose` supplies, dispatching on the model id exactly as `--model`
  does. **Cross-provider overrides are allowed** (a `gemini-*` specialist under
  a `claude-*` parent): the dispatch is already id-based, and refusing would
  mean maintaining a provider-family classifier as a second source of truth
  beside it. Resolution is memoized per model id, so a roster of eight analysts
  on one tier opens one client.

  Two behaviors are deliberate. A **declared override that cannot be resolved
  fails the build** rather than falling back to the parent's model — silent
  fallback is the bug being fixed, and it would let a bundle read as tiered
  while everything ran on one tier; the corollary is that credentials for every
  provider in the roster must resolve at construction. And an **offline-fake
  parent** (`--model=echo` / `scripted` / `toolactor`) collapses every override
  back to itself, so tiering a bundle cannot break the credential-free smoke
  and acceptance runs.

  Behavior change for library consumers: `specialists.Build` /`BuildAll` now
  return an error when a `Spec.Model` is set and `BuildOptions.Resolve` is nil.
  Callers going through `mast.RunWorkload` / `mast.ResumeSession` / `cmd/mast`
  get a resolver wired automatically. `internal/compose.BuildRoot` takes a
  `context.Context` first argument.

  No shipped bundle declares an override yet: naming a concrete model id binds
  a bundle to one provider, so tiering `gke-triage` waits on a
  provider-portable `tier:` field (proposed as W1.1a).

## v0.2.0 (2026-08-10)

- **AG-UI server — Stage 2: HITL interrupt/resume lifecycle**
  (docs/ag-ui-design.md "Implementation status"; #84). Turns the Stage-1
  honest-placeholder `RunError{interrupt}` into the real human-in-the-loop
  loop. A turn that parks on a HITL primitive (a `request_operator_input`-class
  long-running tool, or a programmatic / external-signal pause) now closes the
  SSE stream with a terminal `RunFinished` whose `outcome` is
  `{type: "interrupt", interrupts: [{id, message, responseSchema?, expiresAt?}]}`,
  projected from the durable session's pending-interrupt state rather than
  fabricated. The client resumes by starting a **new run** whose
  `RunAgentInput.resume` carries one entry per interrupt (`status:
  "resolved" | "cancelled"`, optional payload); the daemon reconciles each
  entry against the session's open interrupt ids, builds the resume
  function-response, and drives the resume turn through the same `runTurnPre`
  chokepoint every other turn kind uses. A resume that references no open
  interrupt (or an unknown id) is refused with `409` (`ErrNotResumable`)
  instead of silently forking a fresh turn, and a resume run may carry empty
  input — the `resume` array alone drives it. Since a resume is a new run,
  reaching the parked session under `session_model: per_run` (keyed on
  `runId`) requires the resume to carry `parentRunId`; the default
  `per_thread` reaches it via the shared `threadId`. The terminal interrupt frame
  records a new `interrupted` outcome on `mast_agui_runs_total{workload,outcome}`.
  Still hand-rolled in `pkg/agui` with zero new dependencies. Per-key state
  deltas, client-declared tools, and the `agui://` federation client remain
  follow-on stages.

- **Effect-outbox durability hardening** (docs/durable-execution-design.md
  "Recorded-effect outbox"; #71). Two follow-ups from the outbox gate review.
  **Sub-agent/tool name collision is now refused at construction (N2):** the
  dangling scan excludes every FunctionCall named after a sub-agent (task
  delegations are engine control flow, not effects), so a genuine mutating
  tool that shared a specialist's name was invisible to the outbox — a
  fail-open hole. mast now refuses to start when a composed sub-agent name
  also names a mutating- or spawning-class tool (`effects.CheckNameCollisions`,
  wired in all three construction paths — daemon, one-shot, and the library
  entrypoints), turning a silent durability gap into a clear startup error the
  operator fixes by renaming one side. A read-only tool of the same name is
  harmless and still allowed. Coverage is bounded by what is known by name at
  construction — mast's builtins and the tool names declared in
  `tool_catalog.tools`; a mutating tool never declared there (the common case
  for MCP verbs, since `tool_catalog.tools` is an override list) is not
  enumerable and remains the authoring rule's responsibility: do not name a
  specialist after a mutating tool.
  **Direct `--session-db` ack now warns (N4):** `mast sessions ack-effects
  --session-db=…` cannot serialize its watermark write against a running
  daemon (mast has no on-disk liveness signal to probe), so it prints a clear
  warning that the path is safe only when no daemon serves the DB.

- **Local (stdio) MCP server hardening** (docs/mcp-catalog-design.md
  "Implementation status"; #89). Three measures bound the blast radius of a
  `mcp.json` catalog that launches local commands. **Environment scoping:** a
  stdio server may set `env_mode: "clean"` to start its child from an empty
  environment, passing through only the daemon variables named in
  `env_passthrough` (plus the configured `env`) — so a `clean` server never
  sees the daemon's provider keys or cloud credentials unless named.
  **Command allowlist:** a new catalog-level `command_allowlist`, when
  non-empty, makes any stdio server whose resolved `command` is not listed a
  fatal load error (both sides `${VAR}`-expanded before comparison).
  **Control-plane coverage beyond `.agents/`:** the permission gate now
  accepts an explicit set of registered control-plane paths
  (`Options.ControlPlanePaths`) so a catalog loaded from a path-mode workload
  directory or a non-`.agents` config root can be write-protected once the
  gate is runtime-wired, closing the parent-directory heuristic's gap. Default
  behavior is unchanged (`env_mode` defaults to `inherit`; an empty allowlist
  imposes no restriction).

- **`/abort` re-abort now returns HTTP 409, not 500** (#88). Aborting a
  session that is already terminal is a state conflict, not a server fault:
  the inject `/abort` door now maps the `ErrAlreadyAborted` sentinel to
  `409 Conflict`, mirroring `/pause`. The durable abort marker was already
  idempotent (the `mast_aborts_total` counter stays at 1); this fixes only
  the status code. (A2A `tasks/cancel` keeps its idempotent-success
  semantics — the operator door reports the conflict instead.)

- **Local (stdio) MCP servers + generic catalog wiring**
  (docs/mcp-catalog-design.md "Implementation status"; #87). mast now wires
  every MCP server referenced by a workload generically from the `mcp.json`
  catalog, dispatched by transport kind — the previous build special-cased a
  single hard-coded HTTP `gke` toolset. Two transports are supported:
  streamable **HTTP** (with optional Google OAuth / ADC bearer auth, the GKE
  path) and local **stdio**, where mast launches a `command` (with
  `args`/`env`, `${VAR}`-expanded against the daemon environment) as a child
  process and speaks MCP over its stdin/stdout. Because a stdio server needs
  no cloud credentials, real tool calls can now be driven fully offline under
  `--model scripted`. A workload that references a server missing from
  `mcp.json` is a fatal load error; `mcp.json` is treated as a
  privilege-bearing control-plane file (a stdio entry is code execution) and
  each launch is logged for audit. New `pkg/mcp` catalog loader + transport
  dispatch (`Catalog`/`NewToolset`), a new `docs/site` reference page, and the
  unblocking prerequisite for the deferred blocking-tool UAT legs.

- **End-to-end UAT harness for the v0.2 durable-execution spine**
  (docs/uat-v0.2-plan.md "Implementation status"; #12). `scripts/uat-v0.2.sh`
  drives a real `mast` daemon process — boot, inject, pause, abort, timed
  resume, drain, restart, scrape — against the offline echo model and a real
  SQLite session DB, asserting on session state, exact `/metrics` lines, HTTP
  status, and process exit codes. It is deterministic, credential-free, and
  network-free (a fixed test bearer, no live provider), and runs in under a
  minute as a new `e2e` presubmit (`dev/ci/presubmits/e2e.sh`, wired into
  `all.sh` and a new `e2e` job in CI). It ships the no-blocking-tool subset of
  the scenario catalogue — metric priming + cardinality, auth, operator gate
  pause + token lifecycle (consumed-replay no-op vs expired-token rejection),
  timed-pause fire-and-resume, terminal abort marker + idempotency, and the
  clean-drain / usage exit codes. The scenarios that need a controllable
  registered blocking tool (crash-mid-effect ambiguity, drain-expired exit 3,
  mid-turn cancel, loop-breaker) are deferred until local/stdio MCP support
  lands; the plan doc records why. A minimal fixture bundle lives under
  `testdata/uat/`. Building the harness surfaced one latent wart — `/abort`
  returns HTTP 500 on an already-aborted session instead of mirroring
  `/pause`'s 409 (the durable marker is idempotent regardless) — tracked for a
  follow-up fix.

- **AG-UI server — Stage 1: server core** (docs/ag-ui-design.md
  "Implementation status"; #84). mast gains its fourth ecosystem interop
  surface — AG-UI, the agent↔user protocol CopilotKit apps and
  chat-platform bots speak — alongside MCP, A2A, and attach. With
  `--agui-listen`, a workload that opts in via the bundle's new `agui:`
  section is served at a per-workload HTTP endpoint: a client POSTs an
  AG-UI `RunAgentInput` and receives the turn back as a Server-Sent
  Events (`text/event-stream`) run stream — `RunStarted`, a `StateSnapshot`
  echoing the input state, the model's answer as a `TextMessage` triad
  with `ToolCall*` frames for tool activity, then one terminal
  `RunFinished` / `RunError`. A `GET /agui/agents.json` descriptor lists
  the exposed endpoints. Like the A2A server, it is **hand-rolled with
  zero new dependencies** (`pkg/agui`, over the shared `pkg/serverauth`)
  rather than wrapping the community AG-UI Go SDK — so every run drives
  the same `runTurnPre` chokepoint every other turn kind funnels through
  (turn-lock, abort / gate-pause refusal, budget meter, watchdog, effects
  outbox), and the deployment slim-graph gate stays green. The session id
  is always daemon-derived from the AG-UI `threadId`/`runId` and
  namespaced under `agui-` (`session_model: per_thread` default, or
  `per_run`), fenced off the reserved `…:mast-ops` namespace — a client
  never supplies a raw session id. Auth is a shared bearer validator
  (`MAST_AGUI_TOKEN`, per-workload scopes; non-loopback binds refused
  without it) with per-caller rate limiting (`MAST_AGUI_RATE` /
  `MAST_AGUI_BURST`, HTTP `429` + `Retry-After`). New metrics
  `mast_agui_runs_total{workload,outcome}` and
  `mast_agui_run_duration_seconds{workload}`. An interrupted turn maps to
  an honest `RunError{interrupt}` — the full HITL interrupt/resume
  lifecycle, per-key state deltas, client-declared tools, and the
  `agui://` federation client are follow-on stages.

- **A2A server — Stage C: `message/stream` over SSE** (docs/a2a-design.md
  "Mast as A2A server"; #15, #78). The A2A endpoint now streams turns:
  `POST /a2a` `message/stream` runs a turn exactly like `message/send` but
  emits its progress as Server-Sent Events (`text/event-stream`), one
  JSON-RPC response per `data:` frame — an initial `Task` snapshot, a
  `status-update` per model response (its text carried as progress), then a
  closing `artifact-update` (the agent's answer) and a final `status-update`
  (`final: true`) with the terminal state. `message/send` and
  `message/stream` share one `runTask` body, differing only in whether an
  `emit` callback is threaded through the turn's event loop. Updates are
  **message-granular** (one per model response, not token deltas — token
  streaming needs `StreamingModeSSE` across all turn kinds and is a
  follow-on). The SSE response is upgraded lazily on the first emitted
  frame, so auth, scope, and rate-limit refusals — all decided before the
  turn starts — ride a normal JSON-RPC error rather than a truncated
  stream, and `message/stream` shares the `message/send` rate-limit bucket.
  The agent card now advertises `capabilities.streaming: true`. This closes
  the A2A server umbrella (#78); `message/stream` no longer answers the
  `-32004` unsupported-operation error.

- **A2A server — Stage B2: pluggable rate-limiter seam** (docs/a2a-design.md
  "Rate limiting"; #78). `message/send` — the only budget-consuming verb —
  can now be rate limited through a pluggable `a2a.RateLimiter` seam on the
  server config (the seam AG-UI is designed to reuse, #11); control-plane verbs
  (`tasks/get`, `tasks/cancel`) are deliberately never gated, so an operator
  can always read or cancel a task. The built-in `TokenBucketLimiter` keeps
  an independent bucket per **(caller, workload)** — caller being the token's
  tenant claim if set, else its subject — and the daemon builds it from
  `MAST_A2A_RATE` (requests/second) + `MAST_A2A_BURST` (bucket depth;
  defaults to `ceil(rate)`, min 1). `MAST_A2A_RATE` unset means no limiting;
  a malformed value fails startup. A refused send returns the retryable
  `-32000` with an advisory `Retry-After` header and records a `rejected`
  task-outcome metric. The tenant-claim → session-isolation half of B2
  stays **deferred**: ADK v2.1.0's `IsolationScope` is an event/task-level
  field (the workflow `finish_task` machinery), not a session-create or
  tenant seam, so multi-tenant session isolation waits on an upstream
  session-scope seam or a mast-side user-namespacing design. `Principal.Tenant`
  ships now as the rate limiter's caller identity. SSE streaming
  (`message/stream`) remains Stage C.

- **A2A server — Stage B1: `message/send` turn execution + trace
  propagation** (docs/a2a-design.md "Mast as A2A server"; #78). The A2A
  endpoint now runs turns: `POST /a2a` `message/send` drives a synchronous
  turn through the same `runTurnPre` chokepoint every other turn kind
  funnels through (budget, pause, abort, turn-lock, effects outbox by
  construction). A task id **is** a mast session id; a message with a
  `taskId` continues that task, and one without routes to the single
  exposed skill and mints a fresh task. The agent's final answer is
  captured off the turn's event stream and surfaces as a `result` text
  artifact on the terminal task. An **in-process task registry** is the
  authority for `completed`/`failed` (which a transcript read cannot
  prove): `tasks/get` consults it first and falls back to the session's
  log-proven state for tasks this process did not run (e.g. after a
  restart) or that are non-terminal (the transcript is authoritative for
  `working`/`input-required`, so only terminal outcomes are pinned). The
  registry write is **cancel-wins**: a task canceled as its turn finishes
  still reports `canceled` and never leaks the model's answer as a result
  artifact, regardless of which write lands last. The A2A surface
  addresses **only tasks it minted** (the `a2a-` id prefix) — `tasks/get`,
  `tasks/cancel`, and `message/send` continuation all report `-32001` for
  any other session id, so a caller cannot read, cancel, or drive a turn
  into another surface's session (operator incidents, attach, autoresume)
  by presenting its id. The server assigns a `contextId` when the caller
  omits one (A2A v0.3), returned on the task so follow-ups can be grouped.
  Stage B1 is text-only — a message with no text parts is rejected
  (`-32602`), and an endpoint exposing more than one skill refuses a
  selector-less fresh send (`-32004`); a HITL pause returns
  `input-required`; a session that is aborted or gate-paused refuses the
  call at the chokepoint and reports its durable state; and new tasks are
  refused with the retryable `-32000` once shutdown drain begins.
  **Distributed tracing** is wired both directions: the A2A client injects
  the caller's W3C trace context (`traceparent`/`baggage`) on every
  outbound JSON-RPC call, and the server extracts an inbound one so the
  turn's spans parent under the caller's span (a no-op when tracing is
  disabled). The mast A2A client sends structured inputs as a `data` part,
  which this text-only server does not yet ingest — there is no mast↔mast
  `message/send` round trip until a later stage. Rate limiting and tenant
  → `WithIsolationScope` are deferred to Stage B2; SSE streaming
  (`message/stream`) remains Stage C.

- **A2A server — Stage A: agent card, read/control surface, auth**
  (docs/a2a-design.md "Mast as A2A server"; #78). mast can now expose its
  workloads to the [A2A](https://a2a-protocol.org) ecosystem as a server,
  on its own listener (`--a2a-listen`, e.g. `127.0.0.1:7780`), separate
  from the inject and attach surfaces. A workload opts in via the bundle's
  `a2a.expose` section (`skill_name`, `skill_description`, `auth.scopes`).
  This stage ships the discovery + control surface and its auth so they
  can be exercised end-to-end before turn execution lands: an **aggregated
  agent card** at `/.well-known/agent-card.json` (all exposed workloads as
  skills) plus **per-workload cards** at
  `/.well-known/agent-card/<name>.json`, and a single JSON-RPC 2.0
  endpoint `POST /a2a` serving `tasks/get` and `tasks/cancel`.
  `tasks/cancel` routes to the same terminal-abort path the `/abort` door
  uses (marker-first, then sweep the in-flight turn), idempotently.
  `tasks/get` projects the session's log-proven state onto the A2A task
  lifecycle and **never reports `completed`** from a transcript-only read
  (the event log cannot prove a turn finished vs. is in flight).
  (`message/send` turn execution landed in Stage B1, above;
  `message/stream` remains recognized-but-unsupported, `-32004`, until
  Stage C.) **Auth** is
  pluggable via the `a2a.TokenValidator` interface (built-in
  `StaticBearerValidator`, keyed off `MAST_A2A_TOKEN`); card endpoints are
  public, `/a2a` requires a valid bearer when a validator is configured
  (401 otherwise), and each skill's `auth.scopes` are enforced per call on
  reads *and* the destructive `tasks/cancel` (403 on a missing scope).
  Because `tasks/cancel` is destructive, a non-loopback `--a2a-listen`
  bind is refused at startup without a token (mirroring the attach
  surface's #376 policy); bind loopback or set `MAST_A2A_TOKEN`. A new
  observability family
  `mast_a2a_server_tasks_total{workload,outcome}` counts task-lifecycle
  transitions. Build-vs-buy: hand-rolled over the wire types this repo
  already owns so every A2A task runs through the same `runTurnPre`
  chokepoint every other turn kind funnels through (budget, pause, abort,
  turn-lock, effects outbox by construction), rather than adopting ADK's
  `adka2a.Executor`, which drives the runner directly and bypasses it.

- **Observability fixed-registry v0.2 pass + teardown watchdog**
  (docs/observability-design.md "Metric families",
  docs/durable-execution-design.md "Shutdown contract" item 6; closes
  #50). The v0.2 durable-execution surface built over the sprint — the
  interruption/abort markers, pause planes, timed-pause scheduler, and
  boot-time auto-resume — was previously observable only through logs.
  This pass canonicalizes it into **five fixed counter families** — the
  `mast_autoresume_total` family shipped earlier with #41, and this pass
  adds the four below it — all low-cardinality, all primed to zero per
  workload, and each incremented at the write site so a counter advances
  only when the durable operation it names actually happened (with one
  deliberate inversion, `mast_marker_write_failures_total`, which
  advances only when a marker write *failed*):
  `mast_autoresume_total{workload,outcome}` (boot-pass dispositions;
  #41), `mast_marker_write_failures_total{workload,operation}` (a marker
  write that failed, previously silent; `operation` ∈ `mark`/`clear`
  interruption marker, `pause` planned-stop gate-pause write),
  `mast_aborts_total{workload}`,
  `mast_gate_pauses_total{workload,source}` (`source` ∈
  `operator`/`planned_stop`), and
  `mast_timed_pause_fires_total{workload,outcome}` (`outcome` ∈
  `resumed`/`skipped`/`error`). The registry stays fixed — callers
  increment through typed methods and cannot mint names or labels — and
  the shipped names supersede the pre-implementation
  `mast_pauses_total`/`mast_resumes_total` sketch (they split by the
  mechanism that emits them, not a single `reason` label). Also adds a
  **teardown watchdog** on the shutdown unwind: after the (already
  bounded) drain completes, `serve()` arms a 15s watchdog over the
  deferred teardown (OTel flush, eventlog/attach `Close`, context
  cancels); on overrun it dumps every goroutine's stack to stderr and
  force-exits with a dedicated code `4` (distinct from the
  drain-expiry `3`), so a wedged `Close` or leaked goroutine surfaces a
  diagnostic instead of hanging silently until the supervisor's
  SIGKILL. A healthy teardown exits first and the watchdog never fires.

- **Boot-time auto-resume — the v0.2 durable-execution closer**
  (docs/durable-execution-design.md "Boot-time auto-resume"; closes
  #41). On boot the daemon scans for `interrupted` sessions (turns cut
  short by a prior shutdown) and drives a continuation turn for each
  eligible one through the same chokepoint every other turn kind uses,
  so unattended work restarts on its own — on by default
  (`--auto-resume`, `--auto-resume=false` disables), serve mode with a
  durable `--session-db` only. **The guarantee is the operational form
  of exactly-once: auto-resume never double-fires a mutation.** A
  session carrying **any** dangling mutating tool call (an ambiguous
  prior effect the recorded-effect outbox surfaces) is skipped
  (`skipped_ambiguous`) and left for an operator `ack-effects`, never
  resumed — an ack watermark does not pair the call, so re-running it
  would either replay the raw `tool_use` to the provider or falsely
  synthesize a did-not-happen response. A dangling **read-only** call
  on the single last function-call event is repaired with a synthetic
  `interrupted before completion` response (`.ID`/`.Name` set for
  ID-pairing and Gemini) and the turn re-runs; a transcript already
  ending on a completed model turn just has its stale marker cleared
  (`cleared`, no spurious "Continue" turn); a genuine trailing user /
  paired-tool turn re-invokes the model over history with a nil
  message. Rails: `--auto-resume-window` (default `1h`) skips work
  already stale at crash (`skipped_stale`); a per-session restart-loop
  breaker (3 attempts / 10m, durably pre-incremented so a process that
  SIGSEGVs mid-turn still counts) plus a per-boot turn cap bound a
  poison session (`skipped_loopbreak`); a `preTurn` recheck under the
  session turn lock skips a session a concurrent inject/resume advanced
  between scan and turn (`skipped_superseded`). Slice-1 drives
  `coordinator` dispatch only (`skipped_unsupported` otherwise, and for
  foreign user IDs and deferred sub-run delegations). Every decision is
  counted in `mast_autoresume_total{workload,outcome}`. `mast stop
  --pause-sessions` opts a session out — a gate pause outranks
  `interrupted`, handing those sessions back to the operator instead of
  continuing them. New store seams: `ScanInterrupted`,
  `Summary.InterruptedAt`, and `RecordAutoResumeAttempt` /
  `ClearAutoResumeAttempts`; new effects seam: `ScanDangling` (mutating
  vs repairable vs deferred, sharing `scanHistory`'s pairing core, which
  keeps its exact shipped output).

- **Programmatic pause/abort — the v0.2 durable-execution surface**
  (docs/durable-execution-design.md "The v0.2 pause/abort mechanics",
  designed in #72; closes #42). Two pause planes: an **interrupt
  pause** (`pause_session`, a long-running builtin in the planner
  vocabulary when a durable store exists — the body writes a token
  record keyed by its own function-call ID to the companion ops row,
  then parks; a record-write failure returns an error result, never a
  tokenless park) and a **gate pause** (`mast sessions pause` /
  `POST /pause` / `mast.Pause`) enforced at the daemon's turn
  chokepoint: every turn kind — inject, attach, resume, timer —
  refuses gate-paused sessions with `session_paused` (HTTP 409), and
  `--interrupt` additionally cancels the in-flight turn. **Resume
  tokens** (`mrt_` + 128-bit random) are minted, never caller-chosen;
  scope-checked before execution; 7-day default TTL that `PauseSpec`
  may only shorten; consumed on the durable append of the resume
  FunctionResponse (a resume turn that fails before the append leaves
  the token live for retry); expired tokens refuse with the pause
  intact — `mast sessions extend-token` / `POST /extend-token` is the
  audited recovery. `mast sessions resume --token=...` resolves the
  session itself (`--session-db` direct mode clears gate pauses only,
  on DBs no daemon serves); `mast.ResumeByToken` is the library twin.
  A **timed-pause scheduler** (min-heap, boot ops-scan seeded) fires
  `resume_at` timers through the normal budget-wrapped resume paths;
  refused fires requeue with backoff; abort purges a session's timers
  and tokens. **Terminal abort**: aborted sessions now refuse ALL turn
  kinds at the chokepoint (v0.1 only refused resume — inject/attach
  turns ran on aborted sessions) and abort cancels the in-flight turn
  (marker first, then sweep). **Planned stop** (`mast stop` /
  `POST /stop`): the SIGTERM drain path with interruption markers
  classified `operator stop`; `--pause-sessions` gate-pauses the
  marked set so boot-time auto-resume (#41) hands them back to the
  operator; new exit code **3** = drain window expired with
  interrupted survivors (0 = clean drain — exit codes encode work cut
  short, not initiator). The `paused` state derivation gains two
  sources: unanswered long-running parks — **fixing a shipped v0.1 gap
  where `request_operator_input` parks projected `idle` or
  `interrupted`** (and were boot-repair candidates) — and the gate
  marker; `show` prints pause records, tokens, and ready-to-paste
  token resume commands.

- **Recorded-effect outbox (`pkg/effects`) — the v0.2 durable-execution
  guard for mutating tools under at-least-once re-execution**
  (docs/durable-execution-design.md, resolves open question #8; closes
  #69; unblocks #41). The three runner construction paths mast owns
  (serve, one-shot, library) attach an ADK runner plugin that: refuses
  mutating and sub-run-spawning tool calls with a structured
  `ambiguous_prior_effect` error while the session carries a dangling
  mutating tool call from an interrupted turn (read-only work
  proceeds); replays a call's recorded completion instead of
  re-executing when the log already holds one for the exact
  function-call ID (a nil recorded payload replays an explicit marker
  result rather than re-executing); and treats unknown tools — MCP
  tools included — as mutating (ADK drops MCP `readOnlyHint`
  annotations before mast can read them). All history reads happen
  once per turn at turn start; task delegations (sub-agent-named
  calls the coordinator deliberately leaves unresolved across HITL
  turns), engine control calls, long-running calls, and empty-ID
  calls never read as dangling effects — the pre-merge adversarial
  gate caught both an unreachable replay branch and a false-positive
  wedge on the default coordinator composition in the first
  implementation, and the suite now pins both on the real wire
  shapes. New surfaces: `tool_catalog.tools[].mutating` per-tool
  overrides in the workload bundle (audit-logged at startup);
  `mast sessions ack-effects <id>` (daemon `/ack-effects`, serialized
  against in-flight turns; `--session-db` direct mode for DBs no
  daemon serves — the interrupted turn usually leaves NO pending
  interrupt, so resume alone cannot reach it);
  `mast sessions resume --ack-effects` + `ack_effects` on `/resume`
  for the paused case; and `mast.AckEffects` for library embeds. The
  watermark covers only intents persisted at or before it and is not
  a transcript state. The suite also pins the substrate property the
  design rests on: a tool's own FunctionCall event is durable before
  the tool runs.

## v0.1.2 (2026-07-31)

Patch release: the v0.1.1 shutdown contract hardened through two
further adversarial review rounds, the second gated pre-merge. All
twelve v0.1.2-milestone issues (#53-#58, #60-#65) are closed; the
fixes are backed by reproducers verified to fail on the pre-fix code.

- **Round-three hardening from the pre-merge adversarial gate (#60,
  #61, #62, #63, #64, #65).** The third review round refuted two of
  the round-two fixes and the fixes here were validated by re-running
  the discovering reproducers against them (now the standing rule):
  (1) the #55 fix had reintroduced false-`interrupted` through the
  opposite door — `end()` read the marked flag outside the write
  mutex — so the clear decision moved fully inside it; the
  40-sessions-finishing-during-drain reproducer is in the suite and
  fails 3/3 on the pre-fix code (#60). (2) `/inject` could mint a
  reserved `:mast-ops` session from the untrusted payload UID; now
  refused like every other surface, with reserved-ID rejections
  answering HTTP 400 on all three write endpoints (#61). (3) Two concurrent turns on
  the SAME session lost one to ADK's stale-session check — the daemon
  now runs one turn per session, queueing same-session injects and
  resumes behind the in-flight turn (#62). (4) The pre-mark pass is
  bounded by the drain window again, and the drain-expiry log
  separates survivors with durable markers from those whose mark
  write failed (#63). (5) The "every SQLite construction" hardening
  claim is now true — the one-shot and sessions-CLI paths route
  through the same hardened opener, ops-row writers serialize through
  a store-level mutex, and a failed clear keeps its bookkeeping so
  the marker stays visible (#64). (6) Drain refusal answers 503 +
  Retry-After instead of a 500, the InterruptSelfAuditor capability
  is compile-time pinned, and the docs site caught up with the v0.1.2
  behavior changes (#65).

- **The default `--session-db` path gets the attach path's SQLite
  write hardening; fixes silent loss of markers and transcript events
  under concurrent sessions (#53, #54, #55, #56, #57, #58).**
  Adversarial re-review of v0.1.1 found all SQLite write safety
  (serialization + WAL + busy_timeout) lived in `pkg/eventlog` and
  engaged only with `--attach-listen`; the plain path — what
  `deploy/base` runs — opened raw SQLite and, under concurrent
  incidents, lost transcript events (killing their turns) and
  drain-time interruption markers (silently, warn-only), falsifying
  v0.1.1's SIGKILL-survivability claim on the shipped deploy shape.
  Both paths now share `eventlog.OpenSessionService`; a concurrency
  test pins the behavior. Also from the same review: the write-lease
  regression tests were neutralized by future-pinned timestamps and
  passed on the pre-fix code — rewritten with natural timestamps and
  verified to fail against it (#54); mark/clear ordering is now
  serialized with its store writes so a turn finishing mid-pre-mark
  can no longer be left falsely `interrupted` (#55); the reserved
  `:mast-ops` suffix is enforced on every surface — Get/show, resume,
  abort, and the attach resumer all refuse ops-row IDs instead of
  presenting phantom sessions or driving turns into marker rows
  (#56); the attach interrupt-audit event moved from the protocol
  layer (where it could stale the interrupted turn's session handle —
  the last write-lease violation, present upstream too) into the
  adapter's serialized turn loop (#57); and the drain now closes the
  inject listener before pre-marking, gates the inject/resume
  handlers, and reports only genuinely-marked survivors (#58).
  Marker-write failures are now error-level logs. Known limitations,
  tracked in #50: no marker-failure metric yet (v0.2 fixed-registry
  work), no teardown watchdog, and boot-time auto-resume remains
  deliberately deferred behind the recorded-effect outbox (#41).

## v0.1.1 (2026-07-31)

Patch release: the SIGTERM shutdown contract, shipped and then
hardened by adversarial review — plus the durable-by-default GKE base.
All seven v0.1.1-milestone issues (#38-#40, #45-#48) are closed.

- **Session markers move to a companion ops row; fixes shutdown
  markers and abort killing live turns on database stores (#45, #46,
  #47, #48).** Adversarial review of the shutdown contract found that
  ADK's database session service treats a session handle as a write
  lease (optimistic concurrency on `last_update_time`): appending the
  interruption marker — or an operator abort — to the session's own
  row invalidated the live runner handle, killed the in-flight turn
  with a `stale session error`, and (for shutdown markers) the dying
  turn's cleanup then erased the marker and the drain logged clean.
  All marker writes (abort + interruption) now go to a companion ops
  row (`<sid>:mast-ops`, reserved suffix, hidden from `sessions
  list`), mirroring core-agent's derived-session-ID fix for the same
  ADK behavior; projections fold ops-row state into the primary, and
  legacy v0.1.0 abort markers in primary-row state are still honored.
  Consequences: abort truly is marker-not-preemption now (its
  documented contract), markers work even before the runner has
  created the session, and marker events no longer appear in the
  model-visible transcript. Also from the same review: `/resume`
  turns now get the workload wallclock budget like inject and attach
  turns; new attach-injected turns are refused once a shutdown drain
  begins (live tail unaffected); cancelled drain survivors get a
  short bounded grace to unwind before teardown; and the tracker's
  marker writes are timeout-bounded. Regression tests now run the
  marker paths against a real SQLite database service with a held
  live handle — the in-memory-only test blind spot that hid the bug.

- **The `deploy/` kustomize base is durable by default (#40).** The
  daemon converts from a bare Deployment to a **StatefulSet** with a
  1Gi `volumeClaimTemplate` mounted at `/var/lib/mast` and
  `--session-db=/var/lib/mast/sessions.db` — bringing the shipped base
  in line with the v0.1 GKE row in `docs/deployment-design.md`, which
  already pinned SQLite-on-GKE to StatefulSet+PVC (a rescheduled
  bare-Deployment pod loses the session DB, and with it durable
  pauses, abort markers, and the new shutdown interruption markers).
  `fsGroup: 65532` makes the volume writable for the non-root user;
  the single-replica RollingUpdate recreate preserves the old
  `Recreate` semantics the RWO claim needs. In-memory sessions remain
  a deliberate opt-out (omit `--session-db`), not a deploy default.

- **Shutdown contract: SIGTERM now actually drains (#38, #39, #40).**
  The daemon's graceful-shutdown path returned as soon as the drain
  *began* (`http.Server.ListenAndServe` unblocks the moment `Shutdown`
  is called), so in-flight turns died at process exit with no
  bookkeeping. `mast serve` now: drains in-flight turns — inject and
  attach alike — for up to the workload's
  `budget.max_wallclock_seconds` (30s without a budget); durably marks
  every in-flight session **before** waiting, so a SIGKILL mid-drain
  still leaves the markers on disk; clears the marker for turns that
  finish inside the window; and cancels survivors' contexts instead of
  abandoning them. `pkg/transcript` derives the new `interrupted`
  state (precedence `aborted > paused > interrupted > idle`), and
  `mast sessions list --state=interrupted` filters on it. Deploy
  surfaces sized to match: the K8s base sets
  `terminationGracePeriodSeconds: 330` against the demo workload's
  300s turn ceiling, and the standalone unit gains
  `TimeoutStopSec=330` plus `Restart=always` (the daemon exits 0 on
  any signal-initiated stop; `on-failure` left it down after a stray
  SIGTERM). Boot-time auto-resume of interrupted sessions is
  deliberately deferred to v0.2 behind the recorded-effect outbox
  (#41); planned-stop classification folds into the v0.2 pause/abort
  work (#42). See docs/durable-execution-design.md, "Shutdown
  contract".

## v0.1.0 (2026-07-30)

Phase 1 complete: **all eleven v0.1 exit criteria** from
[`docs/fork-design.md`](./docs/fork-design.md) are green. This release
finishes the staged adapter ports from
[`go-steer/core-agent`](https://github.com/go-steer/core-agent) — the
ported packages are originally derived from core-agent at the
per-stage pins `83ec0713` (P1.3a, 2026-07-27), `b8dd225e` (P1.3b,
2026-07-29), and `25d8531c` (P1.3c, 2026-07-30); every ported file
carries a per-file derivation header with its stage's SHA. Everything
else is mast-native on ADK v2.1.0.

- **`pkg/session` renamed `pkg/transcript`.** The operator-projection
  package collided with ADK v2's own `session` package, forcing an
  alias (`mastsession` / `adksession`) in every file touching both —
  including library embedders' code. Renamed before the v0.1 freeze
  (this package is one of the five stable-from-v0.1 surfaces), while
  the change costs nothing; mirrors core-agent's own rename (#513).
  The `mast sessions` CLI and the root-package API
  (`mast.ListSessions` / `mast.ResumeSession`) are unchanged.

- **P1.3c: the operator attach surface, ported and wired
  (`--attach-listen`).** `pkg/attach` (HTTP/SSE protocol v1.4.0:
  session listing, seq'd replay + live tail, inject/wake/interrupt,
  capabilities frames, agent card, prompt broker, peer registry,
  per-caller rate limiting), `pkg/auth`, and `pkg/eventlog` port from
  `core-agent@25d8531c` — pinned at the first HEAD after attach went
  quiet, deliberately including #519's transport-neutral
  OperatorEventTarget seam so mast never carries the deprecated
  emitter shape. The eventlog lands in the re-scoped shape the fork
  design called for: ADK v2's `session/database` owns the session
  tables; the package adds the seq-overlay + Since/Watch stream on
  top. New `pkg/attachadapter` bridges mast's runner-driven daemon
  into the Registrant contract (one injected message = one serialized
  turn; typed operator events in spec order; interrupt cancels the
  in-flight turn; callers ride the turn context into eventlog
  metadata). `mast serve --attach-listen` binds the surface (TCP or
  `unix:` socket; requires `--session-db`; bearer auth via
  `MAST_ATTACH_TOKEN`; loopback-only without auth). **Exit criterion
  4 verified with the real client:** mast-web (headless chromium,
  proxy mode) connected to a live daemon, listed its sessions, and
  round-tripped a prompt through a real turn over SSE.

- **Build identity moved to `internal/version`.** `mast --version`
  output is unchanged; the version string is now importable so the
  attach capabilities frame and agent card can report it
  (`mast/<version>` server banner). GoReleaser ldflags path updated.

- **One-shot turns get a `--timeout` deadline (default 5m; `0`
  disables).** A one-shot against an unresponsive backend hung forever
  — genai's silent retry-with-backoff on quota errors looks exactly
  like a hang from the outside, observed live. The deadline covers the
  whole turn (model construction included) and trips with an error
  naming the flag. Serve mode is unaffected; workload budgets own its
  wallclock ceilings.

- **gemini mid-tier default is now `gemini-3.5-flash`.** Mid-tier
  classes (research, chat) previously defaulted to `gemini-2.5-pro`,
  which predates mixed built-in + function tools — so `--task=research
  --provider=gemini` could never ground: observed live, the model
  hallucinated a `search` tool (ADK's tool-not-found recovery answered
  it) and then apologized. The 3.5-flash line supports the mix, is
  already classified mid by `pkg/modeltier`, and is cheaper per the
  pricing catalog.

- **One-shot mode refuses flags placed after the prompt.** Go's flag
  package stops parsing at the first positional argument, so
  `mast --task=x "prompt" --session-db=y` silently ran with in-memory
  sessions and sent `--session-db=y` to the model as prompt text (hit
  live twice). A trailing token that names a defined flag is now a
  hard error with an explanation; prompts that legitimately mention
  flag-like words are unaffected when quoted.

- **Fix: the Anthropic adapter respects `stream=false`.** The ported
  adapter ignored model.LLM's stream flag and always yielded
  partial-text chunks; under ADK v2's `StreamingModeNone` every
  fragment became a runner event — ~30 noise log lines per one-shot
  turn on the first live anthropic-vertex run (the runner persists
  only non-partial events, so session stores were unaffected). With
  `stream=false` the caller now sees exactly one TurnComplete
  response; the transport still streams SSE underneath (pause_turn
  continuation and the #487 close discipline depend on that shape).
  core-agent's adapter has the same signature quirk; flagged upstream.

- **Fix: `--session-db` creates missing parent directories (SQLite).**
  SQLite won't create intermediate directories and reports a missing
  parent as "unable to open database file: out of memory (14)"
  (SQLITE_CANTOPEN) — hit on the first smoke run with
  `--session-db=/tmp/mast/smoke.db` before `/tmp/mast` existed. The
  sqlite dialector path now MkdirAlls the parent (0750) so an
  unattended daemon's first boot works against an empty state
  directory; `file:` URIs are unwrapped, in-memory forms untouched.

- **Live-smoke fallout: three provider fixes (2026-07-29).** The first
  credentialed runs surfaced three port seams, all fixed:
  - *gemini frontier-tier default is now `gemini-3.6-flash`.* The
    ported tier table (and core-agent's, still) said `gemini-3.5-pro` —
    a model id that never shipped. Both directions updated together per
    the table's own maintenance note (`taskclass.ModelForTier` and
    `modeltier.Classify`, which now recognizes `gemini-3.6-flash` as
    frontier). Known gap: the builtin pricing catalog (generated
    2026-07-20) has no `gemini-3.6-*` entry yet, so budget metering uses
    the flat non-zero fallback rate until the next catalog regen.
  - *Gemini built-ins now skip pre-3.0 models when function tools are
    present.* Gemini 2.x rejects server-side search built-ins alongside
    client-side function declarations ("Multiple tools are supported
    only when they are all search tools"), and mast's Task/SingleTurn
    agents always carry `finish_task` — so blanket injection 400'd
    every turn on `gemini-2.5-pro`. The wrapper now degrades per
    request (model keeps working unGrounded, one operator log line);
    requests with no function tools keep built-ins on every generation.
  - *Anthropic tool schemas normalized to JSON Schema draft 2020-12.*
    genai marshals `Schema.Type` as uppercase proto enums
    ("OBJECT"/"STRING"), which Anthropic's strict `input_schema`
    validation rejects — hit by ADK v2's `finish_task` declaration on
    the first anthropic-vertex run. `schemaToInput` now recursively
    lowercases type enums (and drops `TYPE_UNSPECIFIED`), leaving
    schema data untouched. core-agent shares this latent seam; flagged
    upstream.

- **P1.3b: provider adapters + watchdog (2026-07-29).** The staged adapter
  ports resume — core-agent closed all four cleanup milestones on
  2026-07-28, so P1.3b's gate (the correctness bugs #357/#367/#370/#363/#372)
  is cleared; attribution pinned at
  `go-steer/core-agent@b8dd225e9ae7fdeb3ff23772cc5be25eed34b818`. New
  packages: `pkg/providers/anthropic` (first-party + Vertex backends;
  preserves the thinking-block round-trip #357, per-request tool_use ID
  synthesis #367, streaming-close #487, pause_turn continuation, and the
  three-bucket prompt-cache usage fold); `pkg/providers/gemini` (the
  builtin-tool wrapper — GoogleSearch/URLContext defaults, server-side
  invocation gating, Vertex context-cache stamp/strip + eviction retry,
  empty-response retry #220 — plus the GoogleSearch grounding audit
  projection); `pkg/providers/vertexcache` (context-cache manager with the
  transient-Init retry #370; relocated from core-agent's `internal/` so
  compose and library consumers can wire it); `pkg/providers/mock`
  (scripted JSONL replay with a self-contained `RecordedTurn` format);
  `pkg/watchdog` (repeated-tool-call signal plus the session-event bridge
  carrying the #363 aggregator-dedup fix). Deliberate reshape, mirroring
  core-agent #492 item 7: no `*config.Config` constructors and no `init()`
  registry — per-provider Options structs are the construction API, and
  `internal/compose.BuildModel` dispatches `echo`/`scripted`/`gemini-*`/
  `claude-*` (`--provider` picks the Anthropic backend; without it,
  `ANTHROPIC_API_KEY` then a Vertex project decides). CLI: `--provider`
  grows `anthropic`, `anthropic-vertex`, and `scripted`; claude-* budget
  rates derive from the builtin pricing catalog; every runner stream now
  runs through a per-session watchdog tap (alerts are logged — the
  model-context routing of core-agent #159 remains bucket-3 work).

- **P1.3a: the ADK-independent adapter packages (2026-07-27).**
  *(Recorded at roll time — this stage landed in PRs #21/#22 without a
  CHANGELOG entry.)* Pinned at `go-steer/core-agent@83ec0713`:
  `pkg/taskclass` (task-class profiles + tier defaults, and with it
  one-shot mode — `mast --task=<class> "<prompt>"`), `pkg/permissions`
  (ported, deliberately not runtime-wired: the package doc records
  core-agent #385's gate findings as wiring-time inputs), `pkg/pricing`
  (builtin catalog wired into budget-meter rates), `pkg/instruction`,
  `pkg/digest` (minus the ADK-v1-entangled `store_eventlog.go` — an
  honest descope), and `pkg/modeltier`.

- **CI split into parallel jobs (core-agent's ci.yml shape).** The single
  `build-test` job running `all.sh` sequentially becomes four parallel
  jobs — `test` (build/vet/fmt/-race tests), `lint`, `hygiene`
  (mod-tidy/govulncheck/slim-deps), `docs-lint` — each step still
  invoking the identical presubmit scripts, so the scripts remain the
  single source of truth and local `all.sh` remains the sequential
  equivalent. Buys: per-check status on PRs (a red run names the
  failing category instead of one opaque `build-test`), shorter wall
  clock (slowest leg paces instead of the sum), per-job re-runs,
  `concurrency: cancel-in-progress` on superseded pushes, and cached
  golangci-lint/govulncheck binaries. Branch protection needs its
  required check renamed from `build-test` to the four new job names.

- **CI parity with core-agent: lint, mod-tidy, vuln, docs-lint presubmits.**
  Four checks ported from core-agent's presubmit set: `lint`
  (golangci-lint v2.12.1 pinned, same linter set and settings as
  core-agent's `.golangci.yml` so ported code lints identically on both
  sides), `mod-tidy` (`go mod tidy` must be a no-op; content-compared,
  not git-compared, so uncommitted local edits don't false-positive),
  `vuln` (govulncheck, symbol-level), and `docs-lint` (prose-drift rules
  over README + site content: tool/specialist counts, variant counts,
  pinned self-install snippets — with a self-test so a defanged regex
  fails loudly). First run caught real issues: two reachable
  vulnerabilities (grpc → v1.82.1 for GO-2026-6061; pgx/v5 → v5.9.2 for
  GO-2026-5004, a SQL-injection path reachable through the Postgres
  session store), three doc-drift instances, and a sweep of lint
  findings — including `serve()` restructured to return errors instead
  of `os.Exit` so the OTel flush and signal cleanup defers actually run
  on fatal startup errors. Ported-file attribution lines moved from
  inside the license comment block to their own comment group below it
  (goheader can't express an optional template suffix; the convention
  is otherwise unchanged). core-agent's `verify-version-fallback` is
  deliberately not ported: mast's ldflags fallback is the constant
  string `dev`, which cannot go stale.

- **Presubmit tests run under the race detector.** `dev/ci/presubmits/
  test.sh` (and therefore CI) now runs `go test -race -timeout 5m ./...`,
  matching core-agent's bar — the P1.3b ports introduced mast's first
  real concurrency, and the ported regression tests were written against
  `-race` upstream. The vertexcache tests' 1s poll deadlines widen to 10s
  so mast doesn't inherit core-agent's #499 flake under loaded CI (a
  passing poll still returns in milliseconds).

## v0.1.0-pre (2026-07-26)

Phase-1 pre-release: nine of fork-design's eleven v0.1 exit criteria are
green; `--task` profiles and attach-mode reachability remain gated on the
P1.3 adapter ports per the revised trigger. Highlights below; details in
the per-item entries that follow.

- Workflow-graph and SubAgents dispatch on ADK v2.1.0; durable HITL
  surviving process death; budget metering with cost + turn caps; the
  full 13-specialist GKE triage roster; `.agents/` config discovery;
  sessions operator surface (CLI + HTTP); observability v0.1; A2A v0.3
  client + federation adapter + `invoke_remote_agent`; planner scaffold;
  forkable workflow starters; the slim-embed guarantee with CI
  enforcement; presubmits-as-CI; deploy starters incl. Cloud Run with
  Postgres session store; top-level `mast` library API.

- **P1.4 interop slice: A2A client + federation surface (2026-07-26).** The
  v0.1 slice of the 2026-07-25 re-cut ([`docs/fork-design.md`](./docs/fork-design.md)
  P1.4): `pkg/federation` with the frozen `Adapter`/`Handle` interface
  (chosen so v0.2 streaming/HITL propagation is additive, not breaking —
  rationale in the package doc), `a2a://<name>/<skill>` reference parsing, a
  scheme-keyed adapter registry, and the planner tool
  `invoke_remote_agent(reference, inputs)`; `pkg/a2a` with the synchronous
  A2A v0.3 client (agent-card fetch + cache, JSON-RPC 2.0 `message/send`
  with `A2A-Version` header and bearer auth from env-var references,
  direct-message and task-opened replies, bounded `tasks/get` polling,
  `tasks/cancel` on cancellation/timeout) and static `.agents/a2a/*.yaml`
  registrations wired into `pkg/config` root scanning. Fork-design exit
  criterion 9 is covered by an httptest round-trip through
  `invoke_remote_agent`. Server, streaming, registry discovery: v0.2 per
  [`docs/a2a-design.md`](./docs/a2a-design.md) phasing.

- **P1.1 bootstrap (2026-07-26).** The repo grows code: the spike-validated
  prototype graduates from the standalone `mast-prototype` repo
  (tags `spike1`/`spike2`; verified findings in
  [`docs/spike-findings.md`](./docs/spike-findings.md)) — workload-bundle +
  specialists loaders, workflow-graph and SubAgents dispatch shapes, durable
  HITL on ADK's SQLite session service, budget metering, per-specialist MCP
  tool allowlists, GKE MCP wiring, inject/resume HTTP endpoints, and the GKE
  triage example workload. Pinned to `google.golang.org/adk/v2 v2.1.0`.
  Provenance per [`docs/fork-design.md`](./docs/fork-design.md): attribution
  is by reference, not git history — prototype history remains in
  `mast-prototype`.
