# mast at v1.0.0-rc.4: where it stands, what to take from core-agent, and which core-agent v3.1 items apply (2026-10-29)

**Method.** Five agents ran read-only against mast `main` and against core-agent
`main`:

| mast ref | What it is |
|---|---|
| `54f266a` | Local checkout |
| `4a82b55` | `origin/main`, 10 commits ahead: dependency bumps plus #550/#551 |
| `b2e57ba` | `v1.0.0-rc.4` |

core-agent `main` was v3.0.0 GA plus docs commits.

| Agent | Lane |
|---|---|
| 1 | Product, positioning and 1.0 readiness |
| 2 | Code health, reliability and security of the code mast wrote itself |
| 3 | Port backlog in both directions. Where the existing instruments are blind, it works capability by capability. |
| 4 | Which core-agent v3.1 security and reliability issues also apply to mast |
| 5 | Which core-agent v3.1 architecture, product and process issues also apply to mast |

The instruments agent 3 used are `docs/sibling-sync.md` and the weekly drift
report in #153. The two v3.1 lanes checked mast's own code for each core-agent
issue rather than inferring from port SHAs.

Every finding is grounded in `file:line`, command output or an issue. The
synthesizer re-ran the load-bearing ones. Marks:

- **✔** re-verified by the synthesizer;
- **◐** reproduced or traced by an agent, not repeated;
- **○** plausible from code reading.

Companion core-agent documents:
[`2026-10-27-v3-good-bad-ugly.md`](https://github.com/go-steer/core-agent/blob/main/docs/assessments/2026-10-27-v3-good-bad-ugly.md)
and
[`2026-10-27-innovations.md`](https://github.com/go-steer/core-agent/blob/main/docs/assessments/2026-10-27-innovations.md).

---

## Bottom line

> **mast's engineering is in better shape than its release process or its
> reason to exist separately from core-agent.**
>
> - The promised API is small and gated.
> - The durability core is careful work, and in places stricter than
>   core-agent's.
> - CI is real: a 42-suite kill/restart harness, plus nightly judged evals on
>   both providers that actually run.
>
> But:
>
> - **The headline durability promise has a reproduced hole.** An approved
>   mutating call that is interrupted mid-execution runs again on auto-resume.
> - **Three 1.0 blockers sit at the edges of the promise:**
>   - lax `mcp.json` parsing;
>   - an apidiff gate that isn't required;
>   - library resume that resets the session ceiling.
> - **The rc window wasn't one.**
> - **Development stopped on 2026-10-15,** when attention moved to core-agent
>   v3.0.

**Don't cut 1.0 now.** The blocking question is not technical. mast and
core-agent now target the same platform/SRE user, with 15 shared package names
and the same GKE-triage demo, and one maintainer is building both. A 1.0 commits
that maintainer to semver, a deprecation cycle and a six-month v1 branch. Decide
mast's future first (§6).

---

## 1. Where mast stands

### The good

- **A small, enforced promised surface.** #301's mechanics are done.
  - About 30 packages moved under `internal/`.
  - `dev/api-surface.txt` promises 6 paths.
  - `internal/cli/testdata/cli-surface.txt` pins the CLI.
  - `dev/tools/verify-apidiff` already diffs against the rc tags.
  - Zero `Deprecated:` markers, and `deprecation_test.go` requires any future
    marker to name its removal version. 1.0 would freeze no shims.
- **The durability core is careful.**
  - `internal/effects` is at 92% coverage and `pkg/approval` at 89%, both
    `-race` clean.
  - Two concurrent approvals of the same call cannot double-execute; the
    outbox's call-level replay is what saves it.
  - A half-done mutating intent after a crash blocks auto-resume until an
    operator acks it. That is stricter than core-agent's tail repair.
  - Durable guardrail halts live in their own table.
  - Spend is a per-call ledger priced at call time, so it is correct across
    restarts. That is the fix core-agent's #1359 needs.
- **The scheduling lease is honest.** It consumes `Lost()` and cancels only the
  scheduling context. Single-replica is the documented posture (chart
  `replicas: 1`).
- **CI and evals are real.**
  - build, vet, fmt, `-race`, golangci-lint with gosec, govulncheck,
    slim-deps, apidiff, terraform tests, and a 42-suite e2e kill/restart
    harness.
  - Nightly judged evals on Claude (Vertex) and on Gemini, green every night
    from Oct 18 to Oct 29.
  - An outcome tier graded against a kind cluster, which gates releases.
- **Releases are signed.** cosign plus SLSA provenance, a Helm chart and a
  Terraform module.
- **Decisions are made on evidence.** The #499 prompt experiment compared 4
  variants over 18 real incidents.
- **The docs are candid about gaps.** The README lists install,
  single-replica, one-tenant and config-drift limits before anyone deploys.
- **Harness:product ratio is 0.41.** That is already inside the 1:2 budget
  core-agent is adopting.

### The bad

**M1. P0/P1 ✔: an approved mutating call that is interrupted mid-execution runs again on auto-resume.**

- **Reproduced.** The synthesizer re-ran the probe against an exact copy of
  `internal/effects` and got `executed=2` with `ScanDangling: mutating=0`.
- **Cause.**
  - When a call parks, ADK writes a placeholder response under the original
    call ID. `pairScan` closes the pair on any response (`effects.go:563`)
    before it reaches the placeholder check.
  - After approval, the call re-dispatches under the same ID. If that run is
    cut off before its real response is saved, nothing reads as dangling.
  - Auto-resume's nil-message continuation (`autoresume.go:236,288`) then
    re-dispatches it, and the write gate does no per-call-ID dedupe.
- **The record of who approved it is lost too,** because the decision rides on
  the tool response's `StateDelta`.
- **When it fires:** a rolling deploy whose drain expires during a long
  approved mutation.
- **Contradicts** durable-execution-design's "never double-fires a mutation".
  The UAT only kills the daemon while the call is still parked.

**M2. P1 ✔ (code): client disconnect cancels the turn on `/inject`, `/resume` and A2A.**

- `handleInject` passes `r.Context()` to the turn (`inject/server.go:620`).
- `/inject` and `/resume` return only after the whole turn, so a webhook
  sender with a 10–30s timeout cancels mid-tool.
- An approved call cut off this way lands in M1.
- #383 was fixed for AG-UI only.

**M3. P1 ◐: the flagship can end a session silently (#502).**

- 10 of 72 gke-triage runs ended with no answer and no error.
- No commit references #502, and it is not in the v1.0 milestone.
- core-agent's #1258 (durable per-turn cut and turn-error records) is the
  port that closes it (§2).

**M4. P1 ✔: 1.0 blockers at the edge of the promise.**

- **Lax `mcp.json` parsing.** `internal/mcp/catalog.go:150` uses plain
  `json.Unmarshal`, while workload, specialist and provider YAML decode
  strictly.
  - Reproduced (◐): `"command_alowlist"` loads a `/bin/sh` server with *no*
    allowlist, and `"env_mod":"clean"` loads as `inherit`, passing the daemon's
    whole environment to the child.
  - Stricter parsing after 1.0 is a major change under the compat policy.
- **apidiff isn't required.** Branch protection requires only `docs-lint`,
  `hygiene`, `lint` and `test`.
- **Library resume resets the session ceiling.** `mast.ResumeSession` builds a
  fresh `budget.New(...)` (`mast.go:297`). In a reproduced run (◐), the run
  parked after 2 calls / 53 tokens, and after resume the meter read 0/0.
  - `CheckCeilingPriced` runs only in `internal/cli/serve.go:1580`.
  - Both contradict the README's "budget ceilings… the same code the daemon
    runs".

**M5. P1: compatibility rule 6 would freeze model selection.**

- "Changing a default that changes what a workload does" is a major change
  (`compatibility-policy.md:78`), with no carve-out for tier mappings, the
  pricing catalog or provider-library behaviour.
- In the rc window alone:
  - #494 promoted a model;
  - #497 promoted another and #498 reverted it the same day;
  - core-models went from v0.3.0 to v0.13.1 in five days.

**M6. P1: the rc window wasn't a release-candidate window.**

- After rc.1 there were 12 `feat` commits and two "Breaking, deliberately"
  default changes (#520, #521).
- There are no `## v1.0.0-rc.N` headings, so each rc's notes are the whole
  cumulative `## Unreleased` section (rc.4's body is 99,875 characters).
- There are no written exit criteria.

**M7. P2 ◐: an MCP server can escape the write gate by publishing `readOnlyHint`.**

- Since #447, mast trusts a server's `readOnlyHint` for tools the bundle
  doesn't pin, and refreshes the hints on every `tools/list`. A hostile or
  compromised server can mark any unpinned tool read-only.
- `threat-model.md` §6 and §3.2 still say annotations are never consulted,
  which contradicts §2.D.

**M8. P2: the packaged event watcher has never worked (#550).**

- #552 turned it off by default, so event-driven triage, the README's lead use
  case, can't be done from `helm install`.

**M9. Other P2/P3 findings.**

- **○** Workloads sharing one session DB can auto-resume each other's sessions,
  because `appName` and `userID` are constants and auto-resume filters only by
  user.
- **Unbounded in-memory state:** per-session turn locks, meter and watchdog
  pools, and the A2A task registry are never evicted.
- **Per-turn DB scans:** every turn, status read and 1-second perms-stream tick
  runs a List scan and a full-history load.
- **Lease timing:**
  - a DB stall is never treated as lease loss;
  - the heartbeat timeout (10s) is longer than the stale window (8s);
  - staleness compares clocks across hosts.
- **Inject handlers** read request bodies without a size cap.
- **Three hand-written turn drivers** (daemon, one-shot, library) re-code the
  guardrail precedence and already disagree with each other.
- **No complexity ratchet,** although the lint config says it mirrors
  core-agent's.

**M10. Docs that are now false.**

- README calls v0.9.0 current and signed, but v0.9.0 is not signed.
- README says "two vendors", but the core-models profiles ship.
- README says plumbing lives in `cmd/mast`, but `cmd/mast` is 28 lines.
- The site banner says rc.2.
- `stability.md` says no threat model exists.
- The roadmap still lists done items as v1.0 blockers.
- The README says the sibling repos are private; they are public.
- The README overstates the slim-embed guarantee. CI checks only
  `examples/deploy/slim`, and the root package pulls in denylisted packages.
- AGENTS.md still carries the pre-fork text and stale versions.
- Twenty design docs say "draft" for subsystems that shipped.

### The ugly

- **Development has stopped.**
  - Commits per month: Jul 135, Aug 144, Sep 128, Oct about 72.
  - The last human commit was 2026-10-15. Since then, only Dependabot has
    landed anything.
  - In the same window the maintainer made 22 core-agent commits to ship v3.0.
- **The sibling split isn't holding.**
  - 15 package names are shared.
  - core-agent's v3.0 theme was unattended GKE autonomy, the same user and the
    same demo.
  - mast's #499 fix borrowed its principles from core-agent's
    gke-platform-agent prompts.
  - mast-web serves both products.
  - "Experimentation substrate" against "platform runtime" is a label, not a
    boundary.
- **One maintainer, no external users.** 1 star, 1 fork, and "nothing outside
  this repo imports any `pkg/` package" (#301).
- **The flagship dropped mast's own design.** #504 cut gke-triage from 14
  routed specialists to 2, because one generalist scored 17/18 against 13/18.
- **Port discipline has lapsed.**
  - The last commit triage in `sibling-sync.md` was 2026-09-21.
  - Since then 35 core-agent commits have touched packages mast ported, and
    none has a verdict.
  - The 48-hour security rule has been missed on every security row; the
    oldest is 29 days old.

---

## 2. What mast should take from core-agent

### Security, overdue under the 48-hour rule

| core-agent | mast state | Port |
|---|---|---|
| `4325dc93` Go 1.26.10 for GO-2026-6809 (#1346) | ✔ still `toolchain go1.26.9` and `Dockerfile` `GO_VERSION=1.26.9` | **Now.** Also port `verify-go-toolchain`, since mast now has a `toolchain` directive (sibling-sync.md:1574's trigger) |
| `01806fb1` childenv scrub + non-dumpable daemon (#1157) | ✔ no `PR_SET_DUMPABLE`; stdio MCP defaults to `env_mode: inherit` and gets the full environment, tokens included | Adapted: withhold the `MAST_*_TOKEN` names even under `inherit`; make the daemon non-dumpable while it holds credentials |
| `10fa3068` refuse a token-less loopback listener; token files (#1201) | warns and serves; no `*_TOKEN_FILE` | Adapted: refuse when a stdio MCP server is configured; add token-file sources |
| same commit: a client CA alone is not authentication | ✔ `internal/attach/server.go:212` returns true on `ClientCAFile` alone. The binary can't reach it today. | Latent fix plus a pinning test |

### Correctness and capability gaps

The file-level drift report cannot see these.

| core-agent capability | mast | Priority |
|---|---|---|
| Durable per-turn cut and turn-error records (#1258) | GAP; this is #502 | **P1** |
| Turn→session escalation after N consecutive turn stops | GAP. `turnloop.go`: "Nothing latches." | P2 |
| Provider retry stamped in the transcript; bare-400 retry (#1206, #1247) | GAP; 429/503 only, metric-only | P2 |
| Banked partial result on sub-run failure; absorbed-delegation disclosure (#1154, #1036) | GAP; `planner/dispatch.go:364` drops it | P2 |
| In-turn context cut at about 95% of the window (#975, without compaction) | GAP; #519 LC-10 overflowed a 65k window | P2 (judgement call) |
| ModelFacingBans-style tool-description sweep | GAP | P3 |
| Skills `requires:` and framing trailer | Skills subsystem not shipped | Take both when skills land |
| No-access baseline in evals | GAP | P3 |
| Per-turn USD ceiling; `approval_timeout`/`approval_notify_after`; auto mode | Decisions, not ports | — |

**Already absorbed or ahead:**
- per-session ceiling;
- turn-scoped refusal memory;
- deny-with-reason;
- durable halts;
- auto-continue breaker;
- atomic eventlog write;
- world-graded evals;
- allow-always handling (only allow-once is admissible);
- spend rebuild (the per-call ledger);
- tail handling (stricter than core-agent's).

**Ledger hygiene:** run the next sibling-sync triage from `fa98fe42`, and record
the n/a rows (#1176, #1179 ahead, #1213 → purser).

## 3. What core-agent should take from mast

Each row fixes an open core-agent v3.1 issue.

| mast mechanism | Fixes |
|---|---|
| Per-call spend ledger with `Restore`, priced at call time and never replayed (`pkg/budget/durable.go`) | core-agent **#1359** |
| `verify-apidiff` + `dev/api-surface.txt` + `dev/api-breaks.txt` + compatibility policy | core-agent **#1368** |
| A lease that consumes `Lost()` for self-started work (`internal/cli/schedlease.go:218`) | core-agent **#1361** |
| Guardrail state in an owned table, with no out-of-band drain | core-agent **#1360** (a design change upstream) |
| CI-run nightly judged and outcome evals across providers | core-agent **#1382** |

## 4. core-agent v3.1 issues in mast

The headline: mast ports core-agent's permissions package but barely uses it.
- There are no bash or file tools.
- The gate is built with empty `permissions.Options{}` ✔, and every mutating
  MCP call parks for approval.
- So most of core-agent's permission findings don't apply.

| Verdict | core-agent issues |
|---|---|
| **Same / variant, file in mast** | #1357 SHA pins · #1358 loopback CSRF/rebinding (*wider*: plain cross-site POST works on unauthenticated inject/AG-UI/A2A) · #1365 panic bulkhead · #1367 full-history scans (*worse*) · #1360 trip-write retry · #1361 lease stall/timing · #1366 eviction · #1353 cwd `.agents/` stdio (docs plus `env_mode` default) · #1368 apidiff not required · #1369 library resume / unpriced refusal · #1370+#1389 three turn drivers · #1371 no ratchet · #1374/#1379/#1380 CLI flags and help · #1375 lax `mcp.json` · #1381 README drift · #1382 archive judged boards · #1384 AGENTS.md drift · #1386 never-timeout loop in a ported test · #1387 CHANGELOG/rc headings · #1388 design-doc status · #966/#967/#994 eval growth |
| **Not present** | #1359 · #1363 · #1364 (approvals are durable parks) · #653 · #1372 · #1373 · #685 · #1376 · #1377 · #1378 · #1383 · #1385 · #1394 |
| **Not applicable** | #1352 and #1356 (latent: the bundles are copied but never loaded; prune them before wiring `FromConfig`) · #1354 · #1355 · #1362 · #651 · #1213 · #1391/#200 (an unclassifiable `execute_go_code` conflicts with the write gate's declared-effect model) |

## 5. Issues filed in mast

All filed 2026-10-29 with evidence and acceptance criteria. The first two groups
are in the `v1.0` milestone.

**v1.0 close-out (blockers):**
- #561: an approved mutating call re-executes after interruption (M1). The
  issue includes the probe.
- #562: `/inject`, `/resume` and A2A turns run on the request context (M2;
  finishes #383).
- #502 (existing): commented with the port of core-agent #1258, and moved into
  `v1.0`.
- #563: reject unknown keys in `mcp.json` (M4).
- #564: make `apidiff` a required check (M4).
- #565: library `ResumeSession` restarts the ceiling, and the unpriced refusal
  is daemon-only (M4).
- #566: scope compatibility rule 6 (M5).
- #567: rc process, covering per-rc headings, a bullet cap, a body-size check
  and exit criteria (M6).

**Security ports, overdue:**
- #568: Go 1.26.10 plus the toolchain resolver.
- #569: keep daemon tokens out of stdio MCP children, and make the daemon
  non-dumpable.
- #570: refuse a token-less loopback listener when a stdio MCP server is
  configured, and add `*_TOKEN_FILE`.
- #571: a client CA alone counts as authenticated (latent).
- #572: loopback CSRF and DNS rebinding on all four surfaces.
- #573: make `readOnlyHint` trust opt-in, and fix the threat model's
  contradiction (M7).
- #574: SHA-pin Actions and digest-pin base images.

**Reliability:**
- #575: panic recovery on daemon-owned turn goroutines.
- #576: make per-turn and per-tick session reads O(1).
- #577: retry failed guardrail trip and reset writes.
- #578: lease self-fencing, heartbeat timing and DB-clock staleness.
- #579: evict per-session in-memory state.
- #580: workloads sharing a session DB can resume each other's sessions.
- #581: escalate repeated turn stops to a session halt.
- #582: provider retry stamps, and the bare-400 retry.
- #583: keep the banked partial result, and disclose a failed sub-run.

**Hygiene and docs:**
- #584: README and site true-up (M10).
- #585: AGENTS.md rewrite, with a CLAUDE.md mirror check.
- #586: design-doc status lines.
- #587: prune the unreachable `builtin_allow` bundles.
- #588: ported never-timeout test loop, and the hidden-directory walk.
- #589: one turn driver and outcome reducer, plus the complexity ratchet.
- #590: CLI help, `--session` and `--output-format json`.
- #591: archive judged boards, grow the outcome tier, score exactly-once for
  real.
- #592: sibling-sync triage from `fa98fe42`.

**Housekeeping, maintainer call (not filed):** close #301, and close #499,
#500 and #503, which #504 addressed.

## 6. The decision that should come before 1.0

1. **Invest in mast as a separate product.**
   - Write the boundary against core-agent's v3 autonomy work into
     `positioning.md`.
   - Name an owner for sibling-sync.
   - Close out the rc with §5's first two groups (the `v1.0` milestone), cut rc.5 with its own notes
     and a no-change soak, then tag GA.
2. **Stay on 0.x.**
   - Keep shipping without the 1.0 promise.
   - Still do §5's security group, because those are live exposures.
3. **Fold the different parts into core-agent.**
   - Move workload bundles, the write gate and effect outbox, the per-call
     spend ledger, and the scheduling lease into core-agent, then archive mast.
   - §3 shows core-agent already needs three of those four.

Options 2 and 3 cost the least maintainer time. Option 1 is right only if mast
will get sustained investment, which the last two weeks of history argue
against.

---

## Confidence and caveats

- ✔ items were re-run or re-read by the synthesizer:
  - M1's double execution (probe re-run);
  - M2's request-context turn;
  - M4's three blockers (`json.Unmarshal`, the required-check list, the fresh
    `budget.New`);
  - the Go toolchain version;
  - the missing `PR_SET_DUMPABLE`;
  - the client-CA predicate;
  - no `recover()`;
  - the empty gate options.
- The inject/AG-UI CSRF in #13 is from code reading. An agent's scratch probe
  for it was blocked by the sandbox. The attach half is confirmed by mast's own
  passing `TestCSRF_SelfOriginPass`.
- The GitHub release-body limit cited in #8 was not verified.
- The local checkout is 10 commits behind `origin/main`, none of them touching
  the code these findings cite.
