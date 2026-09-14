# Compatibility and deprecation policy

**Status: settled 2026-09-14, in force from v1.0**
([#304](https://github.com/go-steer/mast/issues/304)). Companion to
[`../DESIGN.md`](../DESIGN.md) § "The v1.0 stability promise", which says
*what* is covered; this doc says *how a covered thing is allowed to
change*. It supersedes the one sentence
[`./library-api-design.md`](./library-api-design.md) has carried since
2026-07-01 and resolves that doc's open question 6.

A stability promise with no deprecation process is not a promise. It is
a claim that nothing will ever change, which is false, and the first
time it is broken the promise is worth nothing retroactively. mast has
shipped eight minors and broken things freely — correctly, because
pre-1.0 permits it. v1.0 is where that stops
([#300](https://github.com/go-steer/mast/issues/300)), and this is the
process that has to exist before the tag rather than after the first
thing that needs it.

Everything here takes effect **at v1.0**. Until then it is published so
you can read it before you decide what to depend on; v0.9 → v1.0 may
still break covered paths without a cycle, and this document does not
retroactively govern anything already shipped.

---

## 1. What counts as breaking

Non-obvious for this codebase, so it is enumerated rather than left to
"you know it when you see it". Each of these is a **major** for the
surface it names.

### Go API, in a [covered path](../DESIGN.md#the-v10-stability-promise)

1. **Removing or renaming an exported symbol** — type, function,
   method, field, constant, or a constant's value.
2. **Changing a signature.** Including adding a parameter, and
   including a return type that is the same shape under a different
   name.
3. **Adding a method to an exported interface.** Assume every exported
   interface is implemented outside this module unless its doc comment
   says otherwise; the compiler cannot tell you, and a consumer's
   implementation breaks silently at their next build, not at yours.
   In a covered path that means the budget extension points,
   `budget.Pricer` and `budget.Detailer` — which exist in that shape
   because of this rule, taking a `budget.Call` struct so a new usage
   bucket is a field rather than a new signature
   ([#338](https://github.com/go-steer/mast/issues/338),
   [#352](https://github.com/go-steer/mast/issues/352)).
   `federation.Adapter` is the module's other consumer-implemented
   interface and this policy does **not** bind it: `pkg/federation` is
   uncovered, and `DESIGN.md`'s package map calling that interface
   "frozen" is a pre-#300 use of the word meaning its shape is settled,
   not a v1.0 commitment.
4. **Adding a field to an exported struct whose zero value does not
   mean what the previous release did.** Adding the field itself is
   not breaking — mast does not defend unkeyed composite literals —
   but a field that changes behaviour when left unset is a breaking
   change wearing an additive costume. This is why `budget.Call` is a
   struct rather than a parameter list: the usage buckets
   [#352](https://github.com/go-steer/mast/issues/352) added arrive as
   fields whose zero value is the old behaviour, and the one bucket
   where zero would have lied is a pointer, because "the provider did
   not say" is not "nothing".
5. **Raising the `go` directive in `go.mod`.** A consumer on the
   previous toolchain stops being able to build. It has been `go
   1.26.6` since the P1.1 bootstrap and has never moved.

Not breaking, and free to change in any release: anything in the 32
uncovered `pkg/` packages, anything under `internal/`, the starters
under `examples/` (fork-and-forget is their supported lifecycle —
[`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)
§3), log lines, stdout prose, `--help` wording, and metric family
names.

### Behaviour, with no signature change at all

6. **Changing a default that changes what a workload does.** This is
   the category the issue's own list missed and the one with the
   sharpest precedent:
   [#324](https://github.com/go-steer/mast/issues/324) turned the
   providers' server-side built-in tools off by default. Nothing failed
   to compile, no YAML became invalid, and a Gemini workload silently
   lost grounded search. A change nobody's build can catch is *more*
   dangerous than one that breaks it, not less.

7. **Tightening validation on a file that previously loaded.** Two live
   precedents, both deliberate and both breaking:
   [#302](https://github.com/go-steer/mast/issues/302) made an
   unrecognised bundle key a load error, and
   [#349](https://github.com/go-steer/mast/issues/349) made a leftover
   `.tmpl` specialist a refusal.

   **One carve-out, fenced narrowly.** If the loose behaviour accepted
   a declaration and then did not apply it — a *control* that read as
   configured and was not — the fix ships in a minor, because the
   status quo is a policy that silently does not hold and every release
   it survives is a release someone runs unprotected. The fence is that
   the thing being ignored must be a control, not a convenience; the
   refusal must name the fix; and the CHANGELOG must call it breaking
   anyway. #302 is the case: a misspelled `tool_catalogue:` left a
   section of permissions unapplied while the daemon logged a clean
   start.

   Everything outside that carve-out — stricter parsing, a narrower
   range, a newly-required key — is a major.

### Contracts a Go compiler never sees

8. **Renaming a field on a wire contract or a durable record.** attach,
   A2A, AG-UI and inject are consumed by repos whose compiler cannot
   see this one, and `mast.decision/v1` is read by whatever an operator
   pipes `mast sessions export-decisions` into. v0.5 pinned these as
   literal tests precisely because a Go rename passes every behavioural
   test and breaks a client in a repo the compiler cannot see. These
   version on their own clocks — §5.
9. **Removing or renaming a CLI flag or subcommand verb, or changing
   what an exit code means.** Pinned by
   `cmd/mast/testdata/cli-surface.txt` and `TestCLISurface`; before
   that file existed a flag rename passed every test in the tree.
10. **Removing a bundle key, or changing what one means.** Bumps
    `schema_version` — §5.

---

## 2. The cycle

**A covered symbol is deprecated in a minor and removed in a major.**
That is semver, not a choice. The part that *is* a choice is how long
the deprecation has to be visible first, and the answer is **both** of:

- **two released minors**, and
- **90 days**,

measured from the release that first shipped the marker to the release
that removes it.

Neither floor alone means anything here. mast's minors have come three
days apart at their fastest (v0.3.0 → v0.4.0, and again v0.6.0 →
v0.7.0), so "two minors" can be under a week — a deprecation cycle
nobody is awake for. And a quiet stretch of patch releases could
satisfy 90 days with the deprecation visible in exactly one release
anyone actually installed. The count makes the time meaningful; the
time makes the count meaningful.

The one deprecation mast has ever run is the calibration. `.tmpl`
specialist files were renamed in v0.8.0 (2026-09-13) and stopped
loading in v0.9 (2026-09-14 on `main`) — **one minor and about a
week.** That was right for a pre-1.0 file convention whose removal
raises a loud error naming every affected file; it is exactly the shape
this policy forbids post-1.0, and saying so is the point of recording
it.

### The ADK major does not harvest deprecations

`mast.Config` exposes `model.LLM` and `session.Service`, so under Go's
semantic import versioning an ADK major bump **is** a breaking change
to mast's public API and can only ship as a mast major. `DESIGN.md`
already promises that such a release carries *no other* breaking
changes, so the migration is an import path and nothing else.

That promise and this policy collide, and the collision resolves in
favour of the promise: **a mast major that exists only because ADK
bumped does not remove anything.** Deprecations wait for the next
major.

The accepted cost, stated rather than discovered later: mast's major
number can advance for a reason that is not about mast, and a queue of
matured deprecations can sit through one. The alternative — harvesting
them into the ADK major because the consumer is editing import paths
anyway — was refused, because "your migration is an import path and
nothing else" is the entire thing that makes an ADK major cheap, and a
harvest makes it arbitrarily expensive for someone who never asked for
either change.

---

## 3. How a deprecation is announced

All four, every time. A deprecation announced in one place is a
deprecation most of its audience does not hear.

1. **A godoc marker**, in the exact form the tooling reads:

   ```go
   // Deprecated: use OperatorEventTarget. Removed in v2.0.
   ```

   `staticcheck`'s SA1019 and gopls both surface this at the
   consumer's keyboard, which is the only announcement that reaches
   someone who never reads a changelog. **The removal version is
   mandatory** and is enforced: `deprecation_test.go` at the module
   root walks every `.go` file and fails a `Deprecated:` marker that
   does not name the release that removes it. A deprecation with no end
   is not a cycle, it is a permanent second way to do something.

2. **A CHANGELOG entry** in the release that announces it, and another
   in the release that removes it. **The CHANGELOG entry is the
   migration document** — the older promise of "a migration doc" never
   said what one was, and in practice this is what has served:
   [#339](https://github.com/go-steer/mast/pull/339)'s entry carries
   the replacement's exact signature, and #349's carries the error text
   an operator will actually see plus the rename that fixes it. A major
   release additionally gets a migration page on the docs site.

3. **The docs site**, per [`../AGENTS.md`](../AGENTS.md) house rule #4,
   in the same PR.

4. **The running system, wherever it can say it.** A deprecated CLI
   flag keeps working and writes one line to stderr naming its
   replacement — free, because stdout prose and log lines are not part
   of the promise. A deprecated bundle key loads and warns. This is the
   only channel that reaches an operator who inherited a deployment and
   has read nothing.

### The removal must be a refusal, not a silence

[#349](https://github.com/go-steer/mast/issues/349)'s lesson, and it
generalises past file extensions: **when the deprecated thing goes
away, the code that used to accept it must reject it by name.** Deleting
the branch that handled it is not the change — a loader that skips what
it does not recognise turns a removed thing into an *absent* thing, and
the operator gets an error about something else, somewhere else, two
steps later. The error raised at the removal is the last and best
migration document, because it is the only one that finds the person
who missed the other three.

---

## 4. Support window

**The current release.** A fix lands on `main` and ships in the next
release; if it cannot wait, a patch release is cut from `main`. mast
does not backport to a previous minor.

That is a description before it is a policy. There has never been a
release branch in this repo, no fix has ever been backported, and both
patch releases to date (v0.1.1, v0.1.2) were cut straight from `main`.
Promising a support window mast has no branch, no CI and no evidence
for is the failure #300 catalogued — a commitment nothing could ever
fail because of.

**The one forward-looking exception, for majors.** When v2.0 ships, a
`release/v1` branch is cut at the v2.0 tag and receives security and
correctness fixes — not features, not performance — for **six months**.
A major is the one upgrade that is genuinely expensive, so "just take
the latest" stops being an answer there.

This clause is the only thing in this document with no precedent behind
it, and it carries a real cost that is being accepted deliberately: a
second branch, a second CI configuration, and a second release path,
maintained by the same people. Six months is chosen as what one
maintainer can actually sustain, not as what sounds generous.

---

## 5. Everything that is not the Go API has its own clock

Already settled, restated here so this document is the one place to
look.

**The bundle schema.** `workload.yaml` is edited by operators who never
import Go, so a new YAML key must not cost anyone a Go major and a Go
major must not cost anyone a YAML edit. It carries
`schema_version:` — currently `1`, absent means `1`
([#302](https://github.com/go-steer/mast/issues/302)). Adding a key
does not bump it; a key that **changes shape or meaning** does, and so
does removing one. A bundle declaring a version this binary does not
speak is refused by name rather than partially read. Specialist
frontmatter takes no version of its own: it is reached only through a
bundle that names it, so the bundle's version governs the roster.

Removing a key is loud by construction, because unrecognised keys are
already a load error — the operator gets a refusal, not a section of
policy that quietly stopped applying. That is the §3 rule holding
without anyone having to remember it.

**Wire contracts.** attach, A2A, AG-UI and inject version by their own
protocol fields — the capabilities frame, the agent card — not by
mast's Go major. A protocol addition does not force a Go major, and a
Go major does not invalidate a client speaking the older frame.
Feature-detect. Within a version they are additive only; a field whose
meaning changes needs a new version field, because a client that
feature-detects correctly still cannot detect a field that started
lying.

**Metric names.** Not promised, and this document does not promise
them. `pkg/observability` holds a fixed registry and the reference page
is held to a real scrape in both directions, but that gate keeps the
*documentation* honest — it is not a compatibility rule and does not
protect a dashboard. A rename ships with a CHANGELOG entry and nothing
more. Inventing a dual-export window here would be a mechanism nothing
implements, which is the specific failure this whole exercise exists to
avoid.

---

## 6. There is no experimental tier

The 2026-07-25 rule said every unpromised package would carry
`// Experimental: API may change without deprecation cycle until
<version>`. Seven releases later there were **zero** in the tree
(#300). A marker nobody writes is not a boundary.

So mast has no experimental tier and will not add one. **A path is in
the covered list in `DESIGN.md` or it is not covered** — a list in one
file a reviewer can diff, and one a reader can check their own import
list against, which is something a per-file marker never was. A new
package added post-1.0 is uncovered by default and becomes covered only
by an edit to that list.

**How a package gets promoted.** Someone names what they are building
on it. Promotion is additive — it removes nothing and breaks nobody —
so it ships in a minor, together with the freeze-closure check that
makes the new coverage real (`pkg/transcript/freeze_test.go` is the
pattern: a covered package's exported signatures drag in every type
they name, whatever a list says, and that closure is a test rather than
a paragraph — [#338](https://github.com/go-steer/mast/issues/338)).

---

## 7. Resolved decisions

| Decision | Date |
|---|---|
| **Two released minors *and* 90 days** between the marker and the removal, both floors required. Minors alone are meaningless at a cadence whose fastest gap is three days; time alone is meaningless if the deprecation shipped in one release nobody installed | 2026-09-14 |
| **A mast major triggered by an ADK major removes nothing.** `DESIGN.md`'s "no other breaking changes" promise is what makes an ADK bump cheap; harvesting deprecations into it would make it arbitrarily expensive for a consumer who asked for neither change. Accepted cost: mast's major can advance for a reason that is not about mast, and matured deprecations can sit through one | 2026-09-14 |
| **The removal version is mandatory in the `Deprecated:` marker, and that is enforced by a test** rather than asked for in prose. A deprecation with no end date is a permanent second way to do something — which is what mast's only deprecation marker had become | 2026-09-14 |
| **The CHANGELOG entry is the migration document.** The 2026-07-01 sentence promised "a migration doc" and never said what one was; what has actually served is the changelog entry carrying the replacement signature or the exact error text. Majors additionally get a site page | 2026-09-14 |
| **Changing a default that changes what a workload does is breaking**, with no signature change and nothing failing to compile (#324's precedent). A change no build can catch is more dangerous than one that breaks the build, not less | 2026-09-14 |
| **Tightening validation is breaking, with one fenced carve-out**: if the loose behaviour accepted a *control* and then did not apply it, the fix ships in a minor and is still labelled breaking. The fence is "a control, not a convenience", the refusal must name the fix, and #302 is the case it was written from | 2026-09-14 |
| **Support window is the current release; mast does not backport.** A description of eight releases of practice before it is a policy — no release branch has ever existed here. The single exception is a `release/v1` branch at v2.0, six months, security and correctness only, and it is flagged in place as the one unprecedented clause | 2026-09-14 |
| **No experimental tier, ever.** The covered list in `DESIGN.md` is the mechanism, because a reader can diff a list and cannot grep for a marker nobody wrote. Promotion needs a named consumer and ships in a minor | 2026-09-14 |
| **Metric names get no compatibility rule.** The registry-vs-page test keeps the docs honest and protects no dashboard; the site said "their own compatibility rules", which implied a process that does not exist, and now says what is true | 2026-09-14 |
| Resolves [`./library-api-design.md`](./library-api-design.md) open question 6, against its own bias. That doc guessed "2 minors pre-1.0, 1 major post-1.0"; the count survives, the pre-1.0 half is moot, and the real-time floor the bias lacked is what makes it bite | 2026-09-14 |

---

## 8. Related

- [`../DESIGN.md`](../DESIGN.md) § "The v1.0 stability promise" — what is
  covered, the by-reference closure, and the CLI surface
- [`./library-api-design.md`](./library-api-design.md) — the superseded
  one-sentence promise and OQ 6
- [`./fork-design.md`](./fork-design.md) — why versioning restarted at
  v0.1.0, which is what dropped the inherited promises in the first
  place
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — catalog-entry
  retirement, which points here for its cycle shape
- [`../CHANGELOG.md`](../CHANGELOG.md) — the migration document, per §3
- [#304](https://github.com/go-steer/mast/issues/304) this doc ·
  [#300](https://github.com/go-steer/mast/issues/300) the promise ·
  [#302](https://github.com/go-steer/mast/issues/302) the bundle schema
  version · [#338](https://github.com/go-steer/mast/issues/338) the
  freeze closure
