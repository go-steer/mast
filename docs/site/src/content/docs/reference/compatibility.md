---
title: Compatibility and deprecations
description: How mast is allowed to change things after v1.0 — what counts as breaking, how long a deprecation lasts, and how long a release is supported.
sidebar:
  order: 9
---

[Stability and versioning](/reference/stability/) says *what* mast
promises at v1.0. This page says *how a promised thing is allowed to
change* — because a stability promise with no deprecation process is
not a promise, it is a claim that nothing will ever change, and the
first time that is broken the promise is worth nothing backwards as
well as forwards.

Everything here **takes effect at v1.0**. mast is pre-1.0 today and
still breaks covered paths without a cycle; this is published now so
you can read it before you decide what to depend on.

## What counts as breaking

### In a [covered import path](/reference/stability/#what-v10-will-cover)

- **Removing or renaming anything exported** — type, function, method,
  field, constant, or a constant's value.
- **Changing a signature**, including adding a parameter.
- **Adding a method to an exported interface.** Assume mast treats
  every exported interface as one you might implement, unless its doc
  comment says otherwise. In a covered path that means the budget
  extension points, `budget.Pricer` and `budget.Detailer`.
  (`federation.Adapter` is the other interface you might implement, but
  `pkg/federation` is unsupported, so this policy does not bind it.)
- **Adding a struct field whose zero value changes behaviour.** Adding
  the field itself is not breaking — mast does not defend unkeyed
  composite literals — but a field that does something when you leave
  it unset is a breaking change wearing an additive costume. This is
  why `budget.Call` is a struct: new usage buckets arrive as fields
  whose zero value is the old behaviour.
- **Raising the `go` directive in `go.mod`**, which stops you building
  on your current toolchain.

### With nothing failing to compile

- **A default that changes what your workload does.** The sharpest
  example already happened: mast used to construct every Gemini model
  with search grounding and URL context on, and v0.8.0 turned them off
  and put them behind `builtin_tools:`. Nothing failed to compile and
  no YAML became invalid — a workload just quietly stopped being
  grounded. A change your build cannot catch is treated as *more*
  serious than one that breaks it.
- **Validation getting stricter on a file that used to load.** v0.8.0
  made an unrecognised workload-bundle key a load error, and v0.9 made
  a leftover `.tmpl` specialist file a refusal. Both were breaking and
  both were deliberate.

  There is **one carve-out**: if the loose behaviour accepted a
  *control* and then did not apply it — a permissions block that read
  as configured and was not — the fix ships in a minor, because every
  release it survives is a release someone runs unprotected. The error
  will name the fix, and the changelog will still call it breaking.
  Stricter parsing that is merely tidier does not qualify.

### Outside Go

- **Renaming a field on a wire contract or a durable record** — the
  attach, A2A, AG-UI and inject surfaces, and the `mast.decision/v1`
  JSON that `mast sessions export-decisions` writes. These are read by
  programs mast's compiler never sees.
- **Removing a CLI flag or verb, or changing what an exit code means.**
- **Removing a bundle key, or changing what one means** — which bumps
  `schema_version`.

### Not breaking, and free to change in any release

Anything in the [32 unsupported
packages](/reference/stability/#what-it-will-not-cover) or under
`internal/`; the starters under `examples/`, which are yours the moment
you copy them; log lines, stdout prose and `--help` wording; and metric
family names.

## How long a deprecation lasts

**Deprecated in a minor, removed in a major** — that is semver, not a
choice. What mast chooses is how long it has to be visible first, and
the answer is **both** of:

- **two released minors**, and
- **90 days**.

Neither alone means anything here. mast's minors have shipped three
days apart at their fastest, so "two minors" can be under a week; and a
quiet stretch could satisfy 90 days with the deprecation visible in one
release nobody installed. The count makes the time meaningful and the
time makes the count meaningful.

For calibration: the only deprecation mast has ever run was the
specialist file extension. `.tmpl` became `.specialist.md` in v0.8.0
and stopped loading in v0.9 — one minor, about a week. That was
defensible for a pre-1.0 file convention whose removal raises a loud
error naming every affected file. It is exactly the shape this policy
rules out afterwards.

### An ADK major does not take anything away

mast [ships an ADK major as a mast
major](/reference/stability/#the-adk-version-is-part-of-the-contract),
and promises that such a release carries no other breaking changes — so
your migration is an import path and nothing else. That promise wins
over this one: **a major that exists only because ADK bumped removes
nothing.** Deprecations wait for the next major.

The cost, said out loud: mast's major number can move for a reason that
has nothing to do with mast, and a queue of matured deprecations can
sit through one. Harvesting them into the ADK release was considered
and refused, because "an import path and nothing else" is the entire
reason that upgrade is cheap.

## How you will hear about it

Four places, every time.

1. **A `// Deprecated:` marker in godoc**, in the form `staticcheck`
   and gopls read, naming both the replacement and the release that
   removes it:

   ```go
   // Deprecated: use OperatorEventTarget. Removed in v2.0.
   ```

   The removal version is mandatory and a test enforces it. A
   deprecation with no end date is not a cycle — it is a permanent
   second way to do something.

2. **The [changelog](https://github.com/go-steer/mast/blob/main/CHANGELOG.md)**,
   in the release that announces it and again in the release that
   removes it. The changelog entry *is* the migration document: it
   carries the replacement's exact signature, or the exact error text
   you will see. A major additionally gets a migration page here.

3. **This site**, in the same change.

4. **The running system**, wherever it can say it. A deprecated CLI
   flag keeps working and writes one line to stderr naming its
   replacement. A deprecated bundle key loads and warns. This is the
   only channel that reaches whoever inherited the deployment and has
   read none of the other three.

### The removal itself refuses by name

When the deprecated thing finally goes, the code that used to accept it
**rejects it and says so** — it does not quietly stop recognising it. A
loader that skips what it does not understand turns a removed thing
into a *missing* thing, and you get an error about something else,
somewhere else, two steps later. The v0.9 `.tmpl` removal is the
worked example: the specialist directory fails to load, names every
file still using the old extension, and states the rename.

## How long a release is supported

**The current release.** A fix lands on `main` and ships in the next
release; if it cannot wait, a patch release is cut from `main`. mast
does not backport to a previous minor.

That is a description of what has always happened here before it is a
policy: there has never been a release branch, no fix has ever been
backported, and both patch releases so far were cut straight from
`main`. Publishing a longer window would be promising something with no
branch and no CI behind it.

**One exception, for majors.** When v2.0 ships, a `release/v1` branch
is cut at that tag and gets security and correctness fixes — not
features — for **six months**. A major is the one upgrade that is
genuinely expensive, so "take the latest" stops being an answer.

## The parts that keep their own clocks

The [bundle schema](/reference/workload-bundle/), the [wire
protocols](/concepts/interop/) and the CLI do not move with mast's Go
major; [Stability and versioning](/reference/stability/) covers each.
Two details that belong here rather than there:

- **Removing a bundle key bumps `schema_version`**, and is loud by
  construction, because an unrecognised key is already a load error.
  You get a refusal, not a section of policy that silently stopped
  applying.
- **Metric names have no compatibility rule at all.** The registry is
  fixed and the [metrics page](/reference/metrics/) is held to a real
  scrape in both directions, but that keeps the *documentation*
  honest — it does not protect your dashboard. A rename ships with a
  changelog entry and nothing else.

## There is no experimental tier

An earlier plan was that every unsupported package would carry an
`// Experimental:` marker. Seven releases later there were zero of them
in the tree, which is why the promise is a [named
list](/reference/stability/#what-it-will-not-cover) instead: you can
check a list against your own imports, and you were never going to grep
for a marker nobody wrote.

So there is no middle tier. A path is on the covered list or it is not,
and a package added after v1.0 is uncovered until that list changes.

**To get a package promoted**, open an issue saying what you are
building on it. Promotion breaks nobody, so it ships in a minor.
