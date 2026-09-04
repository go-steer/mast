# WIP Inventory: `mast` agent framework

Status: IN PROGRESS (written incrementally). Every claim should carry a `file:line`
citation. Anything not verified is marked **UNVERIFIED**. Anything that exists only
in `docs/` or `DESIGN.md` is marked **DESIGN-DOC ONLY**.

Repo under inventory: `/home/user/projects/mast`
Consumer under inventory: `/home/user/projects/core-sre-agent`

---

## 0. IMPORTANT: which tree these citations refer to

The primary checkout `/home/user/projects/mast` (branch `main`) is **30 commits
stale** — it sits at `5540537` (2026-08-14) while `origin/main` and the `v0.4.0`
tag are both at `bbe6418` (2026-08-17). `git diff --stat HEAD v0.4.0` =
**239 files, +32551/-662**.

The v0.4.0 tree *is* checked out on disk, as a git worktree:

```
/home/user/projects/mast/.claude/worktrees/v0.4   bbe6418 [v0.4-work]  == tag v0.4.0 == origin/main
```

**All `file:line` citations below are repo-relative paths resolved against that
worktree**, i.e. `pkg/agent/agent.go:120` means
`/home/user/projects/mast/.claude/worktrees/v0.4/pkg/agent/agent.go:120`.
Where a fact comes from the stale `main` checkout instead, it is called out.

Other worktrees present (stale dev branches, not inventoried):
`.claude/worktrees/v0.2`, `.claude/worktrees/v0.3`,
`.claude/worktrees/langchain-assess`.

---

## 1. Version / state

**Tags** (`git tag --sort=-creatordate`): `v0.4.0`, `v0.3.0`, `v0.2.0`, `v0.1.2`,
`v0.1.1`, `v0.1.0`, `v0.1.0-pre`. **v0.4.0 is the current release and is
identical to `origin/main`** (`git rev-list --left-right --count
v0.4.0...origin/main` → `0 0`). Nothing has landed on main since the tag.

**Commit count:** 199 on the local `main` (stale); the v0.4.0 tag is 30 commits
ahead of that, so ~229 total. First commit `2365980`, **2026-06-14** — the repo
is ~2 months old. The fork pivot ("rebuild-lean-core", ADK v2) is dated
**2026-07-11** (`4606255`), so the current architecture is ~5 weeks old.

**Contributors:** `git log --format='%an' | sort | uniq -c` →
```
199 Gari Singh
```
**One human author, zero other contributors.** (Commit subjects reference PR
numbers up to #173, so the workflow is PR-based, but single-author.)

**Size:** 1772 `.go` files; ~236k lines of non-test Go, ~255k lines of test Go
(so tests slightly outnumber source — but see §7, much of that is
`internal/evals` fixtures).

**ADK version:** `google.golang.org/adk/v2 v2.2.0` (`go.mod:22`). Go
**1.26.6** (`go.mod:3`). Notable direct deps that shape the inventory:
`github.com/anthropics/anthropic-sdk-go v1.43.0` (`go.mod:6`),
`github.com/modelcontextprotocol/go-sdk v1.7.0` (`go.mod:9`),
`gorm.io/gorm v1.31.2` + `gorm.io/driver/postgres` + `glebarez/sqlite`
(`go.mod:10,19,20`), `prometheus/client_golang` (`go.mod:10`), OTel SDK
(`go.mod:12-16`), `mvdan.cc/sh/v3` (`go.mod:24` — a shell parser, i.e. there is
command-line parsing for a bash-ish tool).

**Branch sprawl:** 60+ local branches, ~12 remote. Many are landed feature
branches never deleted (`w2-approval`, `w3-fanout`, `w0.5-judge`, …). Not a
correctness signal, but it makes "what is shipped" hard to read from the branch
list — use the tag.

**Top-level dirs:** `pkg/` (31 packages), `internal/` (`compose`, `evals`,
`version`), `cmd/mast`, `examples/`, `deploy/`, `docs/`, `testdata/`, `dev/`,
`scripts/`. There is also stray untracked scratch at the repo root in the stale
checkout (`resume`, `resume-langchain`, `resume-models`, `resume-prompt.md`,
`resume-prompt2.md`) — not part of the release.

## 2. Core runtime

### Size correction

The earlier "236k lines" figure was inflated by the four sibling worktrees under
`.claude/`. Real size of the v0.4.0 tree: **~57.1k lines of non-test Go across
`pkg/` + `internal/` + `cmd/`**. Largest single file is `cmd/mast/main.go`
(2383 lines). `pkg/attach` alone is ~9k lines — the biggest package by far,
which tells you where the effort went (a remote TUI/daemon control plane).

### There is no mast agent loop — the loop is ADK v2's

`pkg/agent` is explicitly "the bucket-1 shim over ADK v2's runner and llmagent
primitives" (`pkg/agent/modes.go:15-19`). The three constructors are thin
wrappers over `llmagent.New`:

- `NewCoordinator` → `llmagent.ModeChat` (`pkg/agent/agent.go:62-80`)
- `NewTaskAgent` → `llmagent.ModeTask` (`pkg/agent/modes.go:95-113`)
- `NewSingleTurnAgent` → `llmagent.ModeSingleTurn` (`pkg/agent/modes.go:133-146`)

Multi-agent dispatch is likewise ADK's: ADK auto-installs a `task` tool per
Task sub-agent and `single_turn` per SingleTurn sub-agent
(`pkg/agent/agent.go:26-31`, `pkg/router/router.go:15-41`). So "mast's agent
loop" is a claim about ADK, not about mast. What mast adds are callback seams
(`BeforeModelCallbacks`/`AfterModelCallbacks` plumbed through both configs —
`pkg/agent/agent.go:50-51`, `pkg/agent/modes.go:84-85`) and the fact it had to
disable ADK's `transfer_to_agent` on Task specialists to avoid a hard crash
(`pkg/agent/modes.go:45-67` documents the "workflow: RunNode called outside a
dynamic node" failure at length — an honest but load-bearing ADK workaround).

`pkg/router` is 133 lines and does one thing: build a Chat coordinator with the
workload's specialists as SubAgents (`pkg/router/router.go:53-...`). It is
**not** a model router. The package doc concedes that explicit
`workflow.Workflow` shapes ("fan-out-fan-in, adversarial verifier, autonomous
loop") "remain available … those land as a follow-on"
(`pkg/router/router.go:37-41`) — i.e. **not shipped in router**. (`pkg/graph`
does ship a fan-out; see §5.)

### Session / state / persistence

Two layers, both GORM-backed, both real:

- **`pkg/eventlog`** — "durable, append-only audit log that backs …
  session.Service" (`pkg/eventlog/eventlog.go:17-35`). SQLite / MySQL / Postgres
  via GORM. It *layers on top of* ADK's own `database.SessionService`: ADK owns
  `events`/`sessions`/`state`, mast adds an `agent_eventlog` overlay table
  (`pkg/eventlog/sql.go:53-68`) carrying a monotonic `seq`.
- **Honest durability caveat in mast's own words:** "Two GORM connections (ADK's
  and ours) share the same database file/DSN — **atomic-across-tables writes are
  not provided in v1**; the AppendEvent path writes ADK first, then the overlay,
  and surfaces overlay-write errors so callers can retry (event_id is
  unique-indexed for safe idempotency)" (`pkg/eventlog/eventlog.go:27-31`).
  So on `kill -9` between the two writes you get an ADK event with no seq row.
  Idempotent retry exists; automatic reconciliation on restart: **UNVERIFIED**.

**Durable across `kill -9`? Conditionally yes, and it is easy to get wrong.**
`buildSessionService` (`cmd/mast/main.go:2289-2313`) falls back to
`session.InMemoryService()` when `--session-db` is empty, logging
`"no --session-db; sessions are in-memory and will NOT survive restart"`
(`cmd/mast/main.go:2294-2296`). **In-memory is the default when the flag is
unset.** For a k8s SRE daemon that is a foot-gun: the durable path is opt-in.

Storage hardening that *is* shipped: WAL + `busy_timeout` + write serialization
for SQLite (`pkg/eventlog/plain.go:46-71`, `pkg/eventlog/sql.go:230-305`), a
serialized-write wrapper (`pkg/eventlog/plain.go:73-104`), and an
`agent_run_lock` table with heartbeat/lease semantics so two processes don't run
the same session (`pkg/eventlog/lock.go:37-271`: `AcquireLock` :103,
`heartbeatLoop` :226, `Lost()` :224). SQLite `BEGIN IMMEDIATE` fix landed at
`a36f3a7` (in the v0.4.0 range).

Event stream API: `Since(fromSeq)` replay and `Watch(fromSeq)` live-tail by
200ms **polling** (`pkg/eventlog/eventlog.go:59-71`, default interval
`WithWatchInterval` :191). Not LISTEN/NOTIFY; polling is fine at low scale but
is a real cost at fleet scale on Postgres.

Session state labels are derived only from what the log can prove —
`StatePaused` / `StateAborted` / `StateInterrupted` / `StateIdle`, and the
package explicitly refuses to claim "running" or "completed" because that is
in-process state (`pkg/transcript/transcript.go:34-50`). That is a genuinely
careful design and worth crediting.

### Compaction / summarization — NOT SHIPPED IN MAST

- `pkg/modeltier` documents per-tier compaction thresholds (frontier 0.85, mid
  0.65, small 0.35 — `pkg/modeltier/modeltier.go:82-86`) and says they are
  "consumed by pkg/agent's `DefaultCompactor`" (`pkg/modeltier/modeltier.go:71`).
  **`DefaultCompactor` does not exist.** `grep -rn "Compactor"` over the whole
  v0.4.0 tree returns exactly two hits, both prose comments
  (`pkg/modeltier/modeltier.go:71`, `pkg/watchdog/watchdog.go:66`). There is no
  compactor implementation, no threshold consumer, no trigger.
- The `/slash/compact` HTTP route exists (`pkg/attach/handlers_operator.go:83`,
  `pkg/attach/handlers_slash.go:38-58`) but it is a **capability interface the
  embedding application must implement** (`CompactSlashProvider`,
  `pkg/attach/state.go:746`). The only implementations in the repo are two test
  doubles (`pkg/attach/capabilities_test.go:44`,
  `pkg/attach/operator_slash_test.go:52`). If the host doesn't implement it the
  route returns **501 Not Implemented** (`pkg/attach/handlers_slash.go:41`).

**Verdict: mast ships no context compaction.** For a long-running incident agent
this is the single largest runtime gap.

### Tool-result token budgeting — PRESENT BUT UNWIRED (`pkg/digest`)

> **Correction / key finding:** `grep -rn "github.com/go-steer/mast/pkg/digest"`
> over the entire v0.4.0 tree returns **zero hits**. Nothing in `cmd/mast`,
> `internal/compose`, `pkg/mcp` or `pkg/specialists` imports it, and
> `retrieve_raw` is registered nowhere outside the package. `pkg/digest` is a
> library available to embedders, **not a behaviour of a running mast daemon.**

The description below is of the code that exists, not of anything that runs.


`pkg/digest` (~1.2k lines) keeps large tool responses out of the parent context
via three primitives (`pkg/digest/digest.go:17-33`): a content router
(`pkg/digest/router.go`), a deterministic structural JSON pruner
(`pkg/digest/pruner_json.go`), and a CCR store keyed by tool-call ID with a
`retrieve_raw` tool so the model can fetch the full payload back
(`pkg/digest/store.go`). Payloads over `MaxPassthroughBytes` are truncated with
a `…<N more bytes>` suffix (`pkg/digest/digest.go:178-190`). Ported from
core-agent (`pkg/digest/digest.go:15`), inspired by Netflix Headroom.
Descoped in the port: the eventlog-backed durable store — only in-memory and
filesystem stores exist (`pkg/digest/digest.go:34-41`, explicitly labelled a
"Port descope note (P1.3a)"). Note the comment is stale: it says "mast has
neither the eventlog package nor ADK v1" but `pkg/eventlog` now exists, so the
descope was never revisited.

### Prompt caching — CODE EXISTS, DEFAULT OFF, VERTEX PATH UNWIRED

- **Anthropic:** an `ephemeral` `CacheControl` marker on the **last system
  block only** (`pkg/providers/anthropic/convert.go:144-158`), opt-in via
  `CacheSystem` (`pkg/providers/anthropic/anthropic.go:100`,
  `pkg/providers/anthropic/vertex.go:54`). Tool definitions and conversation
  prefix are **not** cached, which is where most of the tokens are in a
  tool-heavy SRE loop. Cache-read tokens are surfaced
  (`pkg/providers/anthropic/stream.go:119-136`).
- **Gemini/Vertex:** a full explicit `CachedContent` manager —
  create/update/delete with TTL refresh (`pkg/providers/vertexcache/manager.go`,
  475 lines; `Create` :248, `Update` :351), plumbed onto
  `GenerateContentConfig.CachedContent` (`pkg/providers/gemini/builtins.go:231`),
  with a retry-on-cache-eviction wrapper (`builtins.go:313-421`) and a bounded
  retry on failed `Caches.Create` (`e6cca0f`). This is genuinely more mature
  than the Anthropic side **as code** — but `pkg/providers/vertexcache` has
  **zero importers** outside its own tests and the `dev/upstream-drift` path
  table (`dev/upstream-drift/main.go:125`). Nothing in `internal/compose` or
  `cmd/mast` constructs a cache manager or sets the Gemini `CacheName` hook.
- **Anthropic `CacheSystem` is off by default and nothing turns it on:**
  `internal/compose/compose.go:527-528` — "CacheSystem stays off, matching
  core-agent's default (no non-test caller ever enabled it)."

**Net: a mast daemon as shipped runs with no prompt caching on either
provider.** The plumbing is there for an embedder; the defaults are off and
there is no flag in `cmd/mast` to flip them (**UNVERIFIED** whether any env var
does — I found none).

### Other unwired packages (same test applied)

- `pkg/instruction` (768 lines, system-prompt loader with dedup/frontmatter
  strip/truncation): imported **only** by its own external test
  (`pkg/instruction/load_for_session_test.go:26`). Unwired.
- `pkg/config` (444 lines): exactly one importer, `cmd/mast/main.go:59`.
- `pkg/modeltier`: 2 importers.
- `pkg/router`: 3 importers.

### (original caching notes)

### Model tiering — SHIPPED but string-matching

`pkg/modeltier` (202 lines) classifies a model ID into `frontier`/`mid`/`small`
by substring matching on the model name (`pkg/modeltier/modeltier.go:53-181`,
`containsAny` :181). It is a lookup table with hand-written special cases (e.g.
a comment at :164 reclassifying a model "to match its actual" behaviour). It
is not a router: nothing routes a request based on tier at runtime; tiers are
declared per-specialist in the spec (see §5) and used for pricing/eval.

### Cost / budget metering — SHIPPED

`pkg/budget` (494 lines) meters tokens/cost/calls per scope from
`session.Event` usage (`pkg/budget/budget.go:185` `Observe`), with `Limits`,
`Trip` breach records (`pkg/budget/trips.go:41-63`), grant-based raises
(`Grant` :108) and an `Unpriced()` counter (`budget.go:313`) that admits when
it couldn't price a call. Prices come from `pkg/pricing`, which has a builtin
table (`pkg/pricing/builtin.go`), file override (`pkg/pricing/file.go`) and a
weekly generated refresh (`pkg/pricing/refresh.go`, landed `fa4d14d`).

## 3. Safety and human-in-the-loop

This is the strongest part of mast, and the part with the most real code behind
it. It is also where the biggest piece of dead code lives (the plan-first gate).

### Two orthogonal gates

Declared in the workload bundle (`pkg/workload/bundle.go:215-253`):

- `hitl.require_approval` (bool) — **result-level**: pauses the workflow after
  each specialist result via a durable `RequestInput` interrupt
  (`bundle.go:219-227`). Consumed by `pkg/graph/graph.go:259` and
  `pkg/graph/fanout.go:549` (fan-out parks the merged report on one gate,
  `fanout.go:507`).
- `hitl.on_mutation` (enum) — **per-tool-call write gate**, three values
  (`pkg/approval/grant.go` … actually `pkg/approval/plugin.go:40-57` and
  `pkg/workload/bundle.go:193-212`): `require_approval` (park the call),
  `apply` (execute unattended, policy still applies), `dry_run` (never execute;
  tell the model truthfully that it did not happen).
- `hitl.change_set_ttl` — bounds how long one approval authorizes a change set
  (`bundle.go:241-252`).
- A `hitl_policy:` spelling is folded onto `hitl:` at load, and setting both is
  an error (`pkg/workload/loader.go:49-59`).

**Default is fail-safe:** empty `on_mutation` resolves to `require_approval`
(`bundle.go:255-262` `EffectiveOnMutation`). The rationale is written down:
"the failure mode of the other default is an unattended agent writing to a
production cluster that nobody agreed to" (`bundle.go:230-233`).

**But the default binds to the bundle, not the process.** `compose.WriteGate`
returns `(nil, nil)` when there is no bundle (`internal/compose/writegate.go:104-106`),
with an argued rationale (`writegate.go:88-99`): a library embed with no bundle
has no channel to un-park on, so gating there would be a hang, not safety.
**Consequence for a library consumer: if you embed mast without a workload
bundle, there is no write gate at all.** This matters for §9.

### How mutations are classified

`effects.NewPredicate` (`pkg/effects/effects.go:118-134`), **default-deny-unknown**:

1. control-surface calls (`adk_request_input`, `adk_request_confirmation`,
   `finish_task`, `transfer_to_agent`, `exit_loop`, `pause_session`, …) →
   `ClassReadOnly` (`effects.go:85-97`);
2. per-tool overrides from the bundle's `tool_catalog.tools[].mutating` →
   whatever the operator declared (`effects.go:123-128`, bridged in
   `internal/compose/compose.go:384-390` `MutationPredicate`);
3. mast's own builtins from a hardcoded table (`effects.go:100-109`);
4. **everything else, MCP tools included, defaults to `ClassMutating`**
   (`effects.go:132`).

That is the right default, but note what it means operationally: **every MCP
tool is mutating until the operator writes it into `tool_catalog`.** A
read-only `lookout` diagnostic tool will park for approval unless explicitly
declared `mutating: false`. mast does **not** read MCP `readOnlyHint` to seed
this (see §4) — the classification is entirely operator-declared YAML.

The same predicate feeds both the write gate and the effects outbox, on
purpose, so "recorded as an effect" and "needs approval" cannot disagree
(`pkg/approval/plugin.go:74-79`, `internal/compose/writegate.go:37-41`).

### How approvals are persisted and resumed

- The park is ADK's `toolconfirmation` long-running tool
  (`adk_request_confirmation`, referenced `pkg/effects/effects.go:88`), i.e. a
  durable session event. `pkg/transcript` detects a paused session by a pending
  `RequestedInput` **or** an unanswered `LongRunningToolIDs` entry
  (`pkg/transcript/transcript.go:22-30`) — the second source was added because
  v0.1 planner parks were invisible to operators and were being picked up as
  auto-resume candidates (finding H1, `transcript.go:28-30`).
- Approvals of a **change set** mint per-call **grants** written into ADK's
  `StateDelta` (`pkg/approval/grantgate.go:269-285`, key from
  `GrantStateKey`, `pkg/approval/grant.go:81`), so they persist through the
  session store to disk.
- Grant semantics are tight and documented at `pkg/approval/grant.go:29-52`:
  one grant authorizes **one exact normalized (tool, args) signature** (a model
  that proposed `scale(replicas=2)` and calls `scale(replicas=20)` parks);
  grants are consumed once (bound to the function-call id that spent them);
  grants expire (`Freshness`, TTL default `approval.DefaultGrantTTL`); grants
  can carry a **precondition read** re-executed by mast (not the model) before
  the call fires, comparing the world at approval time to now
  (`internal/compose/writegate.go:66-79`, `pkg/approval/grant.go:297-357`).
- Preconditions are fail-closed: a workload whose precondition read is itself
  classified mutating is a **startup failure**, not a warning
  (`internal/compose/writegate.go:213-220`). If the deployment cannot run reads
  on its own behalf, no grants are minted and calls park one at a time
  (`writegate.go:76-79`, warning at `:236-238`).
- A granted call still passes `permissions.Gate.CheckMutatingToolCall` — an
  approval does not override a configured deny (`pkg/approval/grant.go:50-52`).

**Process death mid-approval:** the park is a durable event and the grants are
durable state, so a restart can see the pending interrupt (`pkg/transcript`
`StatePaused`) and resume. Two honest caveats mast itself writes down:
(a) the eventlog overlay write is not atomic with ADK's event write
(`pkg/eventlog/eventlog.go:27-31`); (b) "the window between an external effect
committing and its completion event persisting cannot be closed, only detected"
— dangling intents (`pkg/effects/effects.go:139-144`). The effects outbox runs
**before** the gate so a replayed call is never re-approved
(`internal/compose/writegate.go:99-102`, `pkg/approval/grantgate.go:265`).
There is a `sessions.go:542` note about "a park with no token record: the crash
window between …", i.e. the gap is known and handled by degrading, not by
pretending.

Shutdown handling is careful: markers are written **before** draining so a
SIGKILL mid-drain leaves them on disk (`pkg/transcript/transcript.go:415`,
`cmd/mast/main.go:1096`, `cmd/mast/shutdown.go:107`).

### The plan-first gate — PRESENT AS CODE, NOT WIRED

`pkg/permissions/gate.go` is 1105 lines, **ported verbatim from core-agent**
(`pkg/permissions/gate.go:15`: "Originally derived from
go-steer/core-agent@83ec071…"). It carries the full core-agent surface:
`ModeAsk`/`ModeAllow`/`ModeYolo`/`ModePlan`/`ModeAcceptEdits`
(`gate.go:42-59`), a bash denylist (`pkg/permissions/denylist.go`), path scope
(`pkg/permissions/scope.go`), per-session sub-gates (`DeriveForSession`,
`gate.go:70-77`), and `RequirePlanArtifact` — "mutating tool calls are denied
until `planRecorded` flips to true (via `MarkPlanRecorded`, called by the
`record_plan` tool's handler)" (`gate.go:118-124`, option at `:215-225`).

**None of it is reachable from a mast deployment today.**

- `pkg/permissions/settings.go:17-27` says it outright: "mast's `pkg/config` is
  the `.agents` workload/specialist loader and **has no permissions block yet**
  — wiring the gate into the runtime (and therefore into config loading) is its
  own future workstream."
- `compose.WriteGate` builds `permissions.New(permissions.Options{})` — a
  default gate with **no deny patterns, ask mode, and plan-first off** — and
  says so in a comment: "the deny policy and plan-first pre-check become
  reachable the moment a caller supplies a configured gate"
  (`internal/compose/writegate.go:117-124`).
- **There is no `record_plan` tool anywhere in mast.** `grep -rn "record_plan"`
  over the v0.4.0 tree hits only `pkg/permissions/gate.go:141,175,219`,
  `pkg/permissions/settings.go:55`, and a comment in
  `pkg/attach/handlers_slash.go:156`. Nothing registers the tool that would flip
  the flag.
- The `/slash/replan` route exists (`pkg/attach/handlers_slash.go:159-175`) but
  is another host-implemented capability interface (`ReplanProvider`) returning
  **501** if unregistered, with the error string pointing at a config key
  (`permissions.require_plan_artifact`) that `pkg/config` cannot parse.

**Verdict: mast's gate is purely mutation-triggered.** core-agent's plan-first
gate exists in mast as ported, tested-in-isolation, unreachable code. Anyone
citing "mast has a plan-first gate" is citing `docs/` and dead code, not
behaviour.

## 4. Tooling / MCP integration

`pkg/mcp` is small (829 lines across 5 files) because it delegates to ADK v2's
`tool/mcptoolset` + `modelcontextprotocol/go-sdk v1.7.0`.

### Transports — both shipped

`NewToolset` dispatches on `cfg.Transport` (`pkg/mcp/toolset.go:44-64`):

- **`http`** → `mcpsdk.StreamableClientTransport` (`pkg/mcp/toolset.go:113-116`).
  Streamable HTTP only; no SSE-legacy path, no WebSocket.
- **`stdio`** → `exec.Cmd` child, launched **lazily on first tool use**
  (`pkg/mcp/toolset.go:166-195`, and `cmd/mast/main.go:1479-1481`).
- Anything else is a hard error (`pkg/mcp/toolset.go:57-59`).

Catalog is a versioned `mcp.json` next to the workload
(`pkg/mcp/catalog.go:58-73`, `LoadCatalog` :130; filename via
`mastmcp.CatalogFileName`, wired at `cmd/mast/main.go:1461-1493`). A workload
referencing an undefined server is fatal, not silently dropped
(`cmd/mast/main.go:1470-1473`).

### Security hardening around stdio — genuinely good

- `command_allowlist` at catalog level bounds which executables mast may spawn,
  matched **post-`${VAR}`-expansion on both sides** (`pkg/mcp/catalog.go:64-72`,
  enforced `catalog.go:153`).
- `env_mode: clean` starts the child from an empty environment so only
  `env_passthrough` + `env` reach it — explicitly "to keep unrelated daemon
  secrets out of a local MCP server" (`pkg/mcp/catalog.go:101-111`).
  `env_passthrough` under `inherit` is **rejected** rather than ignored,
  because it "would give a false sense of scoping" (`catalog.go:108-111`).
  An unrecognised `EnvMode` fails closed (`pkg/mcp/toolset.go:38-42`).
- Resolved stdio command **and args** are audit-logged at wire time
  (`cmd/mast/main.go:1481-1484`) — args are called out as the security-relevant
  payload.
- Auth for HTTP: **Google OAuth / ADC only** (`pkg/mcp/catalog.go:117-128`);
  the `AuthConfig` doc says "only Google OAuth is wired today"
  (`catalog.go:117-118`). No bearer-token, no OAuth2 client-credentials, no
  mTLS. Token is pre-fetched at construction to fail fast
  (`pkg/mcp/toolset.go:19-22`).
- `jsonRPCErrorTransport` surfaces the server's own error text instead of a
  bare status line, capped at 32 KiB (`pkg/mcp/errbody.go:31-45`). Ported from
  core-agent and **narrowed**, because go-sdk v1.7.0 closed most of the gap
  (`errbody.go:15-18`).

### `readOnlyHint` / tool annotations — STILL DROPPED

**No.** mast does not preserve MCP tool annotations, for the same reason
core-agent didn't. mast documents it three times:

- `pkg/effects/effects.go:63-67`: "MCP annotations are advisory and **ADK
  v2.1.0's mcptoolset drops them entirely** (`convertTool` copies
  name/description/schemas only), so default-deny-unknown is both the designed
  and the only implementable stance".
- `pkg/workload/bundle.go:62-66`: same statement in the `tool_catalog` doc.
- `internal/evals/differentiators/rig.go:242`.

`grep -rn "readOnlyHint\|ReadOnlyHint"` over the tree returns **one** hit, a
prose mention at `pkg/workload/bundle.go:196`. So: the ADK v1 → v2 upgrade did
**not** fix this. The consequence (see §3) is that every MCP tool is classified
mutating until an operator hand-writes `tool_catalog.tools[].mutating: false`
in YAML. For a k8s-lookout MCP server exposing dozens of read-only diagnostics,
that is a real, manual, per-tool config burden — and getting it wrong in the
permissive direction silently un-gates a write.

### Tool filtering / allowlisting — SHIPPED, with careful semantics

Per-specialist `tools:` block (`pkg/specialists/spec.go:124-136`), three axes:
`builtin`, `mcp`, `skills`. Presence semantics are normative and non-obvious:
**absent = inherit everything on that axis; present-but-empty = deny everything
on that axis; non-empty = whitelist** (`spec.go:127-131`). `mcp: []` is
therefore not the same as no `mcp:` key (`InheritsAllMCP`, `spec.go:138-141`).

Implementation: `filterToolsets` (`pkg/specialists/register.go:188-209`) maps
allowlist entries to toolsets **by `Toolset.Name()`**, narrowing with
`tool.FilterToolset` + `tool.AllowedToolsPredicate` (`register.go:206`).

There is a good war story here worth quoting as evidence of real usage:
ADK's `mcptoolset` reports a fixed `Name()` of `"mcp_tool_set"` for every
instance, so three MCP servers produced three identically-named toolsets and
**every allowlist lookup missed — specialists were built with zero MCP tools**.
mast fixes it with a `named` wrapper (`pkg/mcp/toolset.go:52-73`). "Found
2026-08-14 in W2.4, when the first roster that enumerated its tools (rather
than inheriting the catalog) got none" (`toolset.go:63-65`). That bug shipped
in v0.3 and was only found ~3 days before the v0.4.0 tag.

### Token budgeting on tool results — NOT WIRED

See §2: `pkg/digest` exists, is well-designed, and has **zero importers**.
There is no truncation, no summarisation, and no `retrieve_raw` escape hatch
on MCP tool results in a running mast daemon. A `kubectl get pods -A -o json`
through an MCP tool goes into the context whole.

Related but different: `pkg/mcp/errbody.go` caps *error* bodies at 32 KiB
(`errbody.go:31`). Successful results are uncapped as far as I can see
(**UNVERIFIED** whether ADK's mcptoolset imposes its own cap).

## 5. Multi-agent

### Five dispatch shapes, selected by `--dispatch` (`cmd/mast/main.go:110`)

`internal/compose/compose.go:58-91`:

| shape | wired? | where |
|---|---|---|
| `coordinator` | yes | `pkg/router/router.go` — ADK SubAgents pattern |
| `graph` | yes | `pkg/graph/graph.go` — explicit `workflow` graph, SingleTurn classifier → `StringRoute` → DynamicNode per specialist |
| `fanout` | yes | `pkg/graph/fanout.go` — concurrent read-only analysts + `_synthesis` merge |
| `bounded` | yes | `internal/compose/bounded.go` — one SingleTurn specialist, one model call, schema-forced report |
| `auto` | yes | `RosterShape(cfg.Specs)`, `compose.go:339-341`; never picks `bounded` |

`planner.Enabled` in the bundle overrides `--dispatch` entirely
(`internal/compose/compose.go:325-336`).

### Specialists — the most developed multi-agent surface

`.tmpl` files: YAML frontmatter + Markdown body as system prompt
(`pkg/specialists/spec.go:15-18`). Declarative fields that are actually
enforced:

- `mode: Task | SingleTurn` (`spec.go:71-82`).
- `model:` **or** `tier: small|mid|frontier` — declaring both is a **load
  error** (`spec.go:28-38`). Tier resolves via `BuildOptions.ResolveTier` →
  `pkg/taskclass.ModelForTier` (`internal/compose/tier.go`). This is the
  portable half of model tiering and it does work.
- `output_schema:` — a JSON-Schema file, normalised at load, enforced by ADK
  on both modes (`spec.go:40-49`): Task mode via the `finish_task` declaration
  (invalid call rejected back to the model), SingleTurn via reply validation
  (failure is a run error).
- `capability: read_only | change_executor` (`spec.go:85-105`) — the
  read/write roster split. The reasoning is sound: "an allowlist alone cannot
  distinguish 'this specialist may write' from 'somebody added a write tool to
  a diagnoser and nobody noticed'. A prompt saying *do not mutate* is not a
  control" (`spec.go:90-95`). Default is `read_only`.
- `budget:` (`spec.go:107-118`) — but enforcement is **split and partial**
  (`spec.go:50-63`): `max_wallclock_seconds` is enforced **only in graph
  dispatch** (mapped to `workflow.NodeConfig.Timeout`); `max_turns` and
  `max_cost_usd` are enforced by the session meter via budget scopes bucketed
  by event author, with "two known limitations" the package doc points at but
  does not enumerate here. So a specialist wallclock cap in `coordinator` or
  `fanout` dispatch is **not enforced** — read that carefully before relying on it.
- `tools:` allowlist with the presence semantics described in §4.

### Planner — v0.1 SCAFFOLD, two of four tools are stubs

`pkg/planner` package doc is unusually candid (`pkg/planner/planner.go:15-42`):

- `invoke_specialist(name, input)` — **implemented** (`pkg/planner/dispatch.go`).
- `run_shape_llm_router` and `run_shape_fan_out_fan_in` — "**declared** (their
  schema is part of the pinned vocabulary contract) but **return a structured
  `not_implemented` result**; the Phase-2 'reference-graph library' item wires
  them" (`planner.go:25-29`). Explicit stubs, still stubs at v0.4.0.
- `request_operator_input(message, schema)` — implemented, parks on a durable
  long-running-tool interrupt (`planner.go:30-32`).
- `pause_session` — registered only when `Config.PauseRecorder` is set
  (`planner.go:64-67`).
- "The planner introduces no budget machinery of its own" and there is "one
  known gap" in sub-invocation metering (`planner.go:34-42`).

### Router — not a model router

`pkg/router` builds a coordinator. Nothing routes by cost/tier/latency at
runtime. See §2.

### Federation / A2A — server SHIPPED, client UNWIRED

- **A2A server:** real and wired. `pkg/a2a/server.go` (853 lines),
  `pkg/a2a/server_card.go`, agent card, task lifecycle; backed by the daemon's
  transcript store in `cmd/mast/a2a.go` (709 lines), exposed via `--a2a-listen`
  (`cmd/mast/main.go:250`). mast can *be* an A2A agent.
- **A2A client / outbound federation: code complete, never constructed.**
  `pkg/federation` (501 lines) defines a deliberately frozen `Adapter`/`Handle`
  contract (`pkg/federation/federation.go:15-52` — a long design essay about
  three candidate signatures) and `NewInvokeRemoteAgentTool`
  (`pkg/federation/tool.go:45`). `pkg/a2a/adapter.go` implements the adapter.
  **`grep -rn "NewInvokeRemoteAgentTool\|NewRegistry"` finds only tests**
  (`pkg/federation/tool_test.go`, `pkg/a2a/adapter_test.go`,
  `pkg/federation/registry_test.go`). `cmd/mast` never builds a registry, never
  loads `<root>/a2a/*.yaml` into an adapter, and never adds
  `invoke_remote_agent` to the planner's tool list. The tool doc even says
  "cmd/mast (or a library consumer) constructs the Registry" — cmd/mast
  doesn't (`tool.go:35-39`).
- Streaming and HITL propagation over A2A are explicitly **v0.2 future**, and
  the v0.1 synchronous client hard-fails on a remote `input-required`
  (`pkg/a2a/client.go:298-303`). That is a real limitation for cross-agent
  incident work: a remote agent that needs approval kills the call.

### Subagents — a PROTOCOL SURFACE, not an implementation

`/slash/subagent` (`pkg/attach/handlers_slash.go:111-142`) and `/subagents`
require the embedding host to implement `SubagentSpawner`
(`pkg/attach/state.go:761-763`). If no `BackgroundAgentManager` is wired the
route returns **501** with "subagent spawn not registered (no
BackgroundAgentManager wired)" (`handlers_slash.go:136`). **There is no
`BackgroundAgentManager` type in mast at all** — the identifier appears only in
comments and one error string (`grep` hits: `handlers_slash.go:25,136,151`,
`state.go:92,197`, `permissions/prompter.go:130`, and one test double). So:
declarative subagents — no; background subagents — no; the *wire protocol* for
an operator to ask a host app to spawn one — yes.

Also note the design admission at `handlers_slash.go:24-34`: the spawn is
executed **synchronously** and blocks the operator's HTTP POST for 5–30s; "we
don't try to deliver the result via SSE for v1".

### Summary: wired vs stub

| capability | status |
|---|---|
| Chat coordinator + Task/SingleTurn specialists | **wired** |
| Explicit workflow graph (classify → route) | **wired** |
| Concurrent fan-out + synthesis merge | **wired** |
| Bounded single-call analysis | **wired** |
| Declarative specialist roster (tier, schema, capability, allowlist) | **wired** |
| Planner `invoke_specialist` / `request_operator_input` | **wired** |
| Planner `run_shape_*` | **stub, returns `not_implemented`** |
| A2A server | **wired** |
| A2A client / federation / `invoke_remote_agent` | **built, never constructed** |
| Subagents / background agents | **protocol only, 501 in mast** |

## 6. Autonomy

**Yes — `mast serve` is a real unattended daemon.** This is the clearest
architectural difference from a one-shot analyzer, and most of it is shipped.

### Triggers (`edge_trigger:` on the bundle, `pkg/workload/bundle.go:457-463`)

- **`http`** — inbound POST of an `envelope.InjectPayload`
  (`pkg/inject/server.go:15-19`). Explicitly aimed at "k8s-event-watcher and
  any other source". **v0.1 spike scope: single-session, single-bearer;
  multi-session substrate and `X-Asserted-Caller` proxy identity are
  deferred** (`pkg/inject/server.go:20-23`) — though `cmd/mast` derives a
  per-incident session ID `"incident-"+payload.UID`
  (`cmd/mast/main.go:1512-1518`), and a caller-middleware exists in
  `pkg/attach/caller_middleware.go`, so the inject package doc may be stale
  (**UNVERIFIED** which is authoritative at runtime).
  HTTP status discipline is good: 503+Retry-After on drain, 400 on
  permanently-bad payload, 409 on session-state conflict
  (`pkg/inject/server.go:28-60`).
- **`scheduled`** — self-waking cadence, no external cron
  (`pkg/workload/bundle.go:347-368`, driver `cmd/mast/schedtrigger.go`).
  Three design choices, all argued in `schedtrigger.go:21-70`:
  missed ticks are **skipped, not caught up** (so a crash-looping daemon does
  not spend money on a backlog); the cadence is **anchored and persisted**
  (`pkg/transcript/schedule.go`) so a restart resumes phase; fires are
  **jittered** (default interval/10, capped) so N replicas don't stampede
  (`bundle.go:370-378`). Minimum interval enforced (`bundle.go:421`); no
  default interval, on purpose (`bundle.go:364-367`).
  Contrast with the **timed-pause scheduler** (`cmd/mast/pausesched.go`) which
  *does* catch up on boot, with the distinction reasoned out
  (`schedtrigger.go:40-46`). That is careful engineering.
- A2A inbound (`--a2a-listen`) and the attach control plane are additional
  ways in.
- **No message-queue trigger.** `bundle.go:458-459` says "Other transports (a
  message queue) will join here" — future tense.

### Auto-resume on boot (`cmd/mast/autoresume.go`, on by default)

`--auto-resume` defaults **true** (`cmd/mast/main.go:122`). On boot the daemon
scans for sessions a prior shutdown interrupted and drives a continuation turn
for each eligible one. The safety story is the best-reasoned thing in the repo:

- "auto-resume never double-fires a mutation", resting on the recorded-effect
  outbox: completed effects replay from the log; a session carrying **any**
  dangling mutating intent is **excluded** and left for an operator's
  ambiguous-effect ack (`autoresume.go:40-47`).
- Restart-loop breaker: `autoResumeMaxAttempts = 3` per
  `autoResumeAttemptWindow = 10m`, plus `autoResumeBootCap = 50` continuation
  turns per boot (`autoresume.go:52-63`).
- TOCTOU guard against a concurrent inject advancing the session
  (`errAutoResumeSuperseded`, `autoresume.go:65-71`).
- Resumes go through the same `runTurnPre` chokepoint as every other turn, so
  abort/pause refusal, per-session turn lock, budget meter and outbox all apply
  (`autoresume.go:73-83`).
- **Scope limit, stated:** "Slice-1 scope … **coordinator dispatch only**;
  dangling read-only calls are repaired, dangling delegations are deferred"
  (`autoresume.go:48-51`). So a `graph`/`fanout`/`bounded` workload does not
  get auto-resume.

### Loop control / termination

- **Budget caps** at two levels: workload `budget:` (`max_turns`,
  `max_wallclock_seconds`, `max_cost_usd` — `pkg/workload/bundle.go:134-144`,
  "the tightest cap wins") composing over per-specialist budgets. One turn =
  one model call (`bundle.go:137-140`). Enforced by `pkg/budget`'s meter with
  `Trip` records and a kill switch. Caveat from §5: `max_wallclock_seconds` on
  a *specialist* only binds in graph dispatch.
- **Behavioral watchdog** (`pkg/watchdog`, 1683 lines, ported from core-agent
  — `watchdog.go:15`). Pure heuristics on per-turn telemetry, **no LLM calls**
  (`watchdog.go:19-22`). Three detectors ship: repeated identical tool calls;
  alternating a→b→a→b cycles with path-canonicalised argument comparison
  (`cycle.go`, `canonical.go`); tool-failure streaks read off outcomes not
  calls (`failure.go`) (`watchdog.go:27-40`).
  Three postures, a ladder (`enforce.go`, `feedback.go`):
  `warn` (log + attach guardrail endpoint) → `feedback` (inject the alert's
  Guidance into the next-turn prompt) → `enforce` (Critical alert cancels the
  turn in flight and refuses the next until an operator POSTs
  `/sessions/{id}/guardrails/reset`) (`cmd/mast/main.go:124`).
  **v0.4.0 changed the default from `warn` to `feedback`** with an explicit
  rationale that `warn` on an unattended deployment "is off with extra steps"
  (`CHANGELOG.md` v0.4.0 entry). Halt state survives the process (`69a65f9`,
  `Adopt`, `pkg/watchdog/enforce.go:198-219`) — but only if the attach store is
  there; the daemon warns: "watchdog is in enforce mode without
  `--attach-listen`: a halt will not survive a restart, and there is no reset
  endpoint to clear one" (`cmd/mast/main.go:555`).
  **Deferred detectors (named, not built):** tools-without-text,
  files-not-touched, context-growth-rate, cost-burn-rate; plus "prompt" mode
  and "auto" mode (escalate to a frontier model) and an SSE alert surface
  (`pkg/watchdog/watchdog.go:42-51`).
- **`pause_session`** self-pause tool and durable `request_operator_input`
  parks (see §3).
- Graceful shutdown with a drain window and pre-marked interruption records
  (`cmd/mast/shutdown.go`, 472 lines).

### Honest read

The autonomy machinery is the part of mast that is most clearly *finished*.
The two soft spots: auto-resume is coordinator-dispatch-only, and the
`inject` HTTP surface is still described by its own package doc as a v0.1
single-session, single-bearer spike.

## 7. Observability & evals

### Metrics — SHIPPED, with a deliberate cardinality discipline

`pkg/observability` (607 lines). The design contract is written down
(`registry.go:15-33`) and is unusually disciplined for a young repo:

- **Fixed registry.** "Metric names live here and only here. Callers increment
  pre-declared families through typed methods; they cannot mint new metric
  names or labels" (`registry.go:19-24`).
- **Session ID is never a label** (`registry.go:29-31`) — correlation goes
  through logs/traces. This is the right call and most projects get it wrong.
- Fixed outcome vocabulary: `ok`, `error`, `budget_exceeded`, `watchdog_halt`
  (`registry.go:45-57`), with a note that `watchdog_halt` "should page
  differently" from the other two.

The 15 metric families (`grep -oE '"mast_[a-z_]+"'`): `mast_turns_total`,
`mast_model_calls_total`, `mast_tokens_total{kind}`, `mast_cost_usd_total`,
`mast_budget_trips_total`, `mast_hitl_pauses_total`, `mast_hitl_resumes_total`,
`mast_gate_pauses_total`, `mast_aborts_total`, `mast_autoresume_total`,
`mast_scheduled_fires_total`, `mast_timed_pause_fires_total`,
`mast_marker_write_failures_total{operation}`, `mast_agui_runs_total`,
`mast_agui_run_duration_seconds`. That is a genuinely SRE-shaped set —
cost, HITL, and durability-failure counters, not just request counts.

### Tracing — thin by design

`SetupOTel` (`pkg/observability/otel.go:29-68`) installs an OTLP gRPC exporter
+ W3C propagator, gated on standard `OTEL_EXPORTER_OTLP_*` env vars, no-op
otherwise. "**mast does not open custom spans in v0.1**: ADK v2's runner emits
the unified span tree (session/turn/node/tool), and mast only decorates. This
function only makes that tree leave the process" (`otel.go:36-40`). So trace
quality is entirely ADK's. Nothing correlates a trace to an incident ID at the
span level as far as I can see (**UNVERIFIED**).

### Cost accounting — SHIPPED end to end

`pkg/budget` meter → `pkg/pricing` catalog (builtin table + file override +
weekly regenerated tables, `.github/workflows/pricing-regen.yml`) →
`mast_cost_usd_total` + per-turn log line
(`cmd/mast/main.go:2233-2236`: `session_tokens`, `session_cost_usd`,
`session_model_calls`). `Meter.Unpriced()` (`pkg/budget/budget.go:313`) counts
calls it could not price rather than silently reporting $0 — good.

### Eval harness — REAL, IN-REPO, AND HONEST ABOUT ITS LIMITS

`internal/evals` (~4.6k lines across 5 sub-packages). Deliberately `internal/`:
"a repo quality gate, not one of mast's embedding contracts"
(`internal/evals/dataset.go:19-22`).

Two tiers (`internal/evals/harness/harness.go:15-46`, CLI at
`internal/evals/cmd/evals/main.go`):

1. **Deterministic tier** (default, free, gating). Loads the corpus + intent
   table and checks *that the metrics can score them* — it does **not run**
   them. The reasoning is worth quoting because it is the opposite of the usual
   eval theatre: "Scoring a trajectory requires a model that chooses, and a
   scripted provider does not choose — replaying a fixture and asserting the
   tools match the fixture asserts that the script equals itself"
   (`harness.go:23-27`). What the free tier gates is that the measurement "is
   not a constant function".
2. **Judge tier** (metered, live credentials, nightly). Runs the corpus against
   a live model over a fixture cluster and LLM-grades it. **It reports, it does
   not gate** (`harness.go:66-69`, exit-code contract at
   `cmd/evals/main.go:24-32`).

**Corpus:** `testdata/evals/scenarios/langchain-sre.jsonl` — **32 lines**
(the docs say "31 ported LangChain scenarios"; the file has 32 lines, likely a
trailing newline or an off-by-one — minor). Plus `testdata/evals/intents.yaml`
and `testdata/evals/judge/`. This is a **ported third-party corpus**, not a
mast-authored one, and it is small.

**Metrics emitted** (`internal/evals/evaluate.go:27-31`,
`internal/evals/judge/quality.go:34`):
`intent_coverage`, `tool_coverage`, `severity_accuracy`, `effect_ordering`,
`exactly_once`, `response_quality`. `response_quality` is a 1–5 LLM-judge
rubric with boolean sub-flags (`specific`, `actionable`, `correct_diagnosis`)
normalised to 0–1 (`quality.go:42-66`, rubric prompt at `:99`).
`internal/evals/judge/cost.go` (448 lines) prices a tiered roster against real
models — and `fcdb9f9` explicitly makes it "refuse to pass when there is
nothing to price".

**Differentiator suite** (`internal/evals/differentiators`, 1833 lines): five
(actually six) scenarios "upstream's harness structurally cannot express", run
against the composed runtime: `E-exactly-once`, `E-ambiguous-refusal`,
`E-budget-exhaustion`, `E-approval-rejected`, `E-approval-edited`,
`E-feedback-capture` (`scenarios.go:58,145,240,326,408,525`). **All six are
declared `Expect: Pass` at v0.4.0** — the harness supports an expected-fail
declaration checked bidirectionally (`harness.go:36-46`), and none are
currently failing. These are self-authored tests of mast's own
differentiators, so treat them as regression tests, not competitive evidence.

**CI:** `.github/workflows/ci.yml` + `evals-nightly.yml` +
`evals-nightly-gemini.yml` (a second judged nightly on Gemini, `22385e5`),
plus `upstream-drift.yml` (tracks how far each core-agent-ported package has
drifted, `c45c079`) and `pricing-regen.yml`. Presubmit scripts in
`dev/ci/presubmits/` include `vuln.sh`, `slim-deps.sh`, `docs-lint.sh`.
This is a well-run CI setup for a two-month-old single-author repo.

**Caveat on the "parity scoreboard".** The v0.4.0 CHANGELOG claims "11 of 19
rows green, up from 7". That scoreboard lives in `docs/v0.3-plan.md` §1 / the
v0.4 plan and is a **self-authored comparison against LangChain's SRE agent**,
scored by mast's own harness. Several v0.4.0 commits are about the *provenance
of the board* rather than the code (`04e4108`, `e1f1b50`, `060142b`, `ca2abc1`,
`93ff27a`, `bbe6418`) — i.e. the author was repeatedly correcting overclaims in
the release notes. That is good hygiene, and also a signal that the board is
marketing-adjacent. Do not cite it as third-party evidence.

## 8. Gaps for a k8s SRE product

Scored against what an always-on incident-response agent actually needs. Each
row is SHIPPED / PARTIAL / ABSENT, with the citation that settles it.

| Need | Verdict | Evidence |
|---|---|---|
| Durable inbound queue | **ABSENT** | `pkg/inject/server.go:15-23` — "For the v0.1 spike this endpoint is single-session and single-bearer." Inject is a synchronous HTTP POST with no persistence in front of it; `ErrUnavailable`/`ErrConflict` (`:28-60`) push retry back onto the emitter. If the daemon is down or busy, the alert is simply refused. |
| Missed-tick catch-up | **ABSENT by design** | `cmd/mast/schedtrigger.go:21-70` — missed ticks are skipped, not caught up. Defensible for a poller, wrong for an alert pipeline. |
| Crash recovery of in-flight work | **PARTIAL** | `cmd/mast/autoresume.go:40-83` gives exactly-once resume with `autoResumeMaxAttempts = 3` / `autoResumeBootCap = 50`, but is explicitly "Slice-1 scope … coordinator dispatch only". Requires `--session-db`; without it `cmd/mast/main.go:2294-2296` warns sessions "will NOT survive restart". |
| Incident dedup / fingerprinting | **ABSENT** | No dedup primitive exists. The nearest thing is `cmd/mast/main.go:1512-1518` `sessionIDFor` → `"incident-" + p.UID`, which *isolates* per-incident sessions but performs no suppression, correlation, or flap detection: two POSTs with the same UID land in the same session and both run. The `dedup` grep hits are SSE broadcaster dedup (`pkg/attach/broadcaster.go`), unrelated. |
| On-call routing / paging | **ABSENT** | No PagerDuty, Opsgenie, Slack, or webhook notifier anywhere in `pkg/`, `internal/`, or `cmd/`. The only `slack` string in the repo is an example identity in a doc comment (`pkg/auth/auth.go:102`, `proxy_by="sa:slack-bot"`). Approvals surface over the attach HTTP API only — an operator must be watching. (core-sre-agent wrote its own `internal/notify` + `switchboard.go`, 724 lines, precisely because mast has none.) |
| Multi-tenancy | **ABSENT, and documented as such** | `pkg/serverauth/auth.go:42-47`: "Tenant does NOT yet drive session isolation: ADK v2.1.0's IsolationScope is an event/task-level field …, not a session-create or tenant seam. Multi-tenant session isolation is deferred." `Tenant` exists only as a rate-limit bucket key (`pkg/serverauth/ratelimit.go:145-146`). Two tenants against one daemon share the session store. |
| Rate limiting | **SHIPPED** — the strongest row here | `pkg/serverauth/ratelimit.go` — `RateLimiter` seam + `TokenBucketLimiter` per (caller, workload), reused by A2A (`pkg/a2a/server.go:424-434`) and AG-UI (`pkg/agui/server.go:509-518`). Separately, `pkg/attach/rate_limit.go:37-62` gates only the **cost-bearing** attach endpoints (10/min, burst 5, lazy prune at 1024 buckets) with an explicit cost-DoS threat model. Caveat: `TokenBucketLimiter`'s bucket map "is not evicted — … bounded eviction is a follow-on" (`ratelimit.go:73-75`). |
| Cost ceilings | **SHIPPED** | `pkg/budget` + `meterPool` (`cmd/mast/main.go:1524+`), per-session meters, workload `MaxCostUSD`/`MaxTurns` composing with per-specialist budgets. See §2/§6. Caveat from §5: `max_wallclock_seconds` is only enforced in graph dispatch (`pkg/specialists/spec.go:15-63`). |
| Secrets handling | **PARTIAL** | Real hygiene at the MCP subprocess boundary: `EnvModeClean` starts a child from an empty environment with an explicit `EnvPassthrough` allowlist (`pkg/mcp/catalog.go:47-110`, `pkg/mcp/toolset.go:181-196`), and it fails closed on an unknown mode (`toolset.go:40`). Error bodies are capped at 32 KiB (`pkg/mcp/errbody.go:15-45`). But mast's *own* credentials are plain env vars with no vault/rotation seam: `ANTHROPIC_API_KEY` (`pkg/providers/anthropic/anthropic.go:77-121`), `MAST_ATTACH_TOKEN` / `MAST_INJECT_TOKEN` / `MAST_A2A_TOKEN` / `MAST_AGUI_TOKEN` (`cmd/mast/main.go:352,636`, `cmd/mast/a2a.go:608`, `cmd/mast/agui.go:510`). Bearer tokens are compared with `crypto/subtle` (`pkg/serverauth/auth.go:31`), which is right, but they are static shared secrets. |
| Output redaction | **PARTIAL / narrow** | The only redaction that ships is approver-identity redaction on decision export (`pkg/transcript/decisions.go:46-48`, default `approver_digest`; `cmd/mast/sessions.go:560,614-617`). There is **no redaction of tool output** — no secret-value scrubbing on the path from a k8s tool result into the model context or the event log. For a k8s agent that can read `Secret` objects this is a live exposure: everything a tool returns is persisted verbatim into `agent_eventlog`. (Sanitization of k8s specs happens inside lookout, outside mast.) |
| Audit trail | **SHIPPED** | `pkg/eventlog` with monotonic `seq`, caller/proxy metadata sidecar (`eventlog.go:83-103`), decision export (`pkg/transcript/decisions.go`). The best-built substrate in the repo — and, per §9, unused by the product. |
| Single-writer safety | **PARTIAL** | `agent_run_lock` with heartbeat leases (`pkg/eventlog/lock.go:37-271`) prevents two daemons driving one session. But `pkg/eventlog/eventlog.go:27-31` concedes "atomic-across-tables writes are not provided in v1" — ADK's tables and the overlay are two GORM connections; a crash between the two writes leaves a seq-less event, mitigated only by a unique index and caller retry. |
| Horizontal scale | **ABSENT** | Everything above is single-process: in-memory meter/watchdog pools keyed by session, unevicted limiter maps, SQLite-first storage, and `pkg/attach/rate_limit.go`'s own framing as "single-instance scale". There is no leader election beyond the per-session lock and no shared-state story for N daemons. |

### The four that would actually block a product

1. **No durable ingress.** Alerts arrive over a single-bearer, single-session
   HTTP POST that can refuse. There is no queue, no retry, no backpressure, and
   no at-least-once guarantee between the alert source and the agent.
2. **No dedup or correlation.** An alert storm produces N independent model
   runs. Combined with §6's cost model this is the failure mode that shows up
   as a bill.
3. **No notification egress.** An agent that needs approval has no way to reach
   a human who isn't already polling the attach API. This is the direct
   contradiction of the "autonomous, always-on" framing: the HITL gate is real
   (§3) but has no doorbell.
4. **No tool-output redaction into a durable log.** A k8s agent plus a
   verbatim, on-disk event log plus no scrubber is a secret-exfiltration path.

Notably, three of these four are exactly the things core-sre-agent had to build
itself (`internal/notify`, `internal/scheduler`, and lookout-side
sanitization) — see §9.

## 9. How core-sre-agent actually uses mast

Paths in this section are relative to `/home/user/projects/core-sre-agent`
(module name `github.com/go-steer/k8s-sre-agent`, HEAD `31420f7` "Require mast
v0.4.0 and drop the local replace"). 83 Go files, ~13.9k non-test lines, 11
commits, 1 author.

### It imports five mast packages. Five.

Exhaustive count of `github.com/go-steer/mast/...` import lines across the
whole repo (tests included):

| mast package | import sites |
|---|---|
| `pkg/pricing` | 7 |
| `pkg/agent` | 6 |
| `pkg/budget` | 5 |
| `pkg/specialists` | 3 |
| `pkg/providers/anthropic` | 1 |

That is the complete list. Sites: `cmd/sre-agent/main.go:68-69`,
`cmd/sre-eval-live/main.go:52-53`, `cmd/sre-eval/main.go:39`,
`cmd/sre-monitor/main.go:68`, `internal/evals/limits.go:20-21`,
`internal/evals/runner.go:23-24`, `internal/evals/usage.go:23`,
`internal/llm/provider.go:25`, `internal/sre/agent.go:33-34`,
`internal/sre/stall.go:18`, plus tests.

Measured against §2–§7 of this report, the flagship consumer uses **none of**:
`pkg/eventlog` (durable sessions, seq log, run lock), `pkg/attach` (operator
control plane), `pkg/approval` (write gate, change-set grants),
`pkg/permissions` (the ported core-agent gate), `pkg/effects` (mutation
classification), `pkg/workload` (bundles, HITL policy, triggers, budgets),
`pkg/mcp` (catalog, allowlist, env hygiene, error caps), `pkg/watchdog`,
`pkg/inject`, `pkg/transcript`, `pkg/graph`, `pkg/router`, `pkg/planner`,
`pkg/federation`, `pkg/a2a`, `pkg/agui`, `pkg/observability`, `pkg/modeltier`,
`pkg/digest`, `pkg/config`, `pkg/auth`, `pkg/serverauth`, `pkg/taskclass`,
`pkg/envelope`, `pkg/instruction`.

What it *does* use reduces to: the three `llmagent` mode constructors
(`pkg/agent`), the specialist YAML loader/builder (`pkg/specialists`), the cost
meter (`pkg/budget`), the model price table (`pkg/pricing`), and the Vertex
Anthropic provider (`pkg/providers/anthropic`). Four of those five are
essentially libraries; only `pkg/specialists` carries framework opinion.

> **This is the single most load-bearing finding in the report.** The mast
> "autonomous SRE runtime" — durable event log, HITL write gate, watchdog,
> attach/inject control plane, dispatch shapes, observability registry, MCP
> layer — is not exercised by the product built on top of it. It is a
> framework whose only real consumer imports it as a thin library.

### No durable sessions anywhere in the product

Every runner in the repo is `runner.NewInMemory(...)`
(`internal/evals/runner.go:73`, plus test sites `internal/sre/agent_test.go:307`,
`transfer_test.go:56`, `hitl_live_test.go:352`, …). All four production
binaries (`cmd/sre-agent/main.go:285`, `cmd/sre-monitor/main.go:336`,
`cmd/sre-eval-live/main.go:366`, `cmd/sre-eval/main.go:157`) construct
`&evals.Runner{...}`, which is the in-memory path. There is no
`session.Service`, no `--session-db`, no `eventlog` reference in the repo.

So the §2 answer ("does state survive `kill -9`?") is, for the actual product:
**no, and not even by misconfiguration — the durable path was never wired.**

### It reimplements mast features rather than importing them

| Concern | mast has | core-sre-agent has instead |
|---|---|---|
| Write gate / HITL | `pkg/approval`, `internal/compose/writegate.go` | `internal/approval/approval.go:15-42` — its own gate on ADK `RequestConfirmation` / `adk_request_confirmation`, fail-closed (nil Approver denies) |
| Tool allowlist filtering | `specialists.filterToolsets` (`pkg/specialists/register.go:188-209`) | `internal/sre/agent.go:393-417` — `allow()`, a local re-derivation of the same semantics |
| Scheduled/monitor loop | `cmd/mast/schedtrigger.go`, `pkg/workload` triggers | `internal/scheduler/scheduler.go:15-45` — its own "three clocks" loop |
| MCP toolset naming | `pkg/mcp/toolset.go:52-73` (`named` wrapper) | `internal/lookout/lookout.go:234-245` — its own `Named`/`named`, with the comment "Named() fixes that here rather than in mast" (`:41-46`) |
| Mutation classification | `pkg/effects` | `internal/readonly/readonly.go:15-40` (four guard layers) + `internal/lookout/lookout.go:200-231` |
| Stall detection | `pkg/watchdog` | `internal/sre/stall.go` (`FinishOnStall`, an `AfterModelCallback`) |

The duplication is not accidental — `internal/lookout/lookout.go:41-46` says
outright that the fix belongs "here rather than in mast". The consumer is
routing around the framework.

Its own packages dwarf its mast usage: `internal/kubewrite` (1493 lines),
`internal/faults` (1258), `internal/notify` (724), `internal/kuberead` (648),
`internal/bounded` (443), `internal/kindcluster` (416), plus `internal/monitor`,
`internal/schema`, `internal/kubectl`.

### It does enable prompt caching, which mast does not

`internal/llm/provider.go:59-61` calls
`anthropic.NewVertex(ctx, anthropic.VertexOptions{CacheSystem: true})`.
mast's own composition keeps it off (`internal/compose/compose.go:527-528`:
"CacheSystem stays off, matching core-agent's default (no non-test caller ever
enabled it)"). So the one `CacheSystem: true` caller in the world is the
downstream product, and mast's own default is stale.

### The `lookout` question — the critical one

**Does anything wire `lookout mcp` as a toolset?**

**In mast: no.** Grepping `lookout` across the v0.4 worktree finds it only in
(a) `docs/`, (b) `testdata/evals/intents.yaml` (an intent-mapping table naming
lookout tools, `:183+`), and (c) the eval rigs. And the eval rigs are fixtures,
not integrations:

- `internal/evals/judge/rig.go:225` —
  `Toolsets: []tool.Toolset{&staticToolset{name: "lookout-fixture", tools: tools}}`.
- `internal/evals/judge/rig.go:388-397` — `staticToolset` is a trivial fixed
  tool-list `tool.Toolset`, "the minimum `tool.Toolset` a fixture needs to reach
  `specialists.Build`'s allowlist filter". It never spawns a binary.
- `internal/evals/judge/rig.go:296-308` — `readOnlyPolicies` declares **every**
  lookout tool non-mutating (`effects.ToolPolicy{Name: name, Mutating: &no}`).

That last one is a concrete correctness gap, not just a scoping note: the real
lookout binary declares **3 of 24 tools as writers** (`k8s_findings_diff`,
`k8s_findings_ack`, `k8s_triage_status` — see
`core-sre-agent/internal/lookout/lookout.go:200-206`). mast's fixture asserts
they are all reads. So mast's differentiator evals score a write-gate story
against a tool surface that, by construction, contains no writes.

**In core-sre-agent: yes — but bypassing mast entirely.**
`internal/lookout/lookout.go` (319 lines) is the real integration:

- Package doc `:15-21`: "lookout is consumed as a *subprocess* speaking MCP over
  stdio, never as a Go import. k8s-lookout depends on core-agent, which depends
  on ADK v1, while this repo is on ADK v2; linking both majors into one binary
  is the failure mode this indirection exists to prevent."
- `spawn()` `:114-133`: `exec.Command(resolved, "mcp")` with
  `cmd.Env = append([]string{"KUBECONFIG=" + cfg.Kubeconfig}, cfg.Env...)` —
  an empty base env, i.e. it independently reinvents mast's `EnvModeClean`.
- `Toolset()` `:88-111`: `mcptoolset.New(mcptoolset.Config{Transport:
  &mcp.CommandTransport{Command: cmd}})` — **ADK's mcptoolset directly**.
  `mast/pkg/mcp` is not imported.
- `ToolInfo` `:136-148`: repeats mast's finding that "mcptoolset.convertTool …
  drops `mcp.Tool.Annotations` entirely, so `ReadOnlyHint` … is not reachable
  from a `tool.Toolset`".
- `Surface()` `:157-186`: opens a **second, independent MCP handshake** purely
  to recover `t.Annotations.ReadOnlyHint`, because the ADK path threw it away.
  `UnclassifiedWriters` `:219-231` fails closed on anything it cannot classify.
- `Scan()` `:260-283` / `Pipe()` `:285-319`: a third integration path that
  shells out to `lookout health`, `lookout triage delta`,
  `lookout findings diff --report=-` as plain CLI, not MCP at all.

So the "mast + k8s-lookout" stack, as actually built, is: **ADK v2 + lookout
over stdio MCP + ~14k lines of bespoke k8s code, with mast contributing five
utility packages.** mast is not the integration layer between them; the
consumer wrote that itself.

### One thing mast should have absorbed and didn't

The `readOnlyHint` workaround is solved *twice, independently* (mast at
`pkg/mcp` + `pkg/effects` policy overrides; core-sre-agent at
`internal/lookout/Surface()`), and neither solution is reusable by the other.
Same for the `mcp_tool_set` naming bug (`pkg/mcp/toolset.go:52-73` vs
`internal/lookout/lookout.go:234-245`). That is the clearest available evidence
that the framework/consumer boundary is not working.
