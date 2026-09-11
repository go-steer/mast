---
title: Stability and versioning
description: What mast promises to keep working, what it does not, and what the v1.0 number will and will not mean.
sidebar:
  order: 8
---

mast is **pre-1.0**. Nothing on this page is a promise yet — it is the
promise mast will make at v1.0, published now so you can decide what to
depend on today.

## Today, before v1.0

Every exported path may change in any release. In practice the six
paths listed below are the ones under active compatibility discipline
and the ones least likely to move; the rest have changed between minor
releases and will again.

Restarting version numbers at v0.1.0 is what dropped mast's inherited
API stability promises when it forked from
[core-agent](https://github.com/go-steer/core-agent). **v1.0 is the
release that makes them again** — that, and nothing more, is what the
number will mean.

## What v1.0 will cover

Six import paths follow [semver](https://semver.org) from v1.0:

| Path | What lives there |
|---|---|
| `github.com/go-steer/mast` | The library front door: `Run`, `RunWorkload`, `ListSessions`, `ResumeSession`, `ResumeByToken`, `Pause`, `AckEffects`. |
| `github.com/go-steer/mast/pkg/agent` | Agent-mode constructors and `Config`. |
| `github.com/go-steer/mast/pkg/transcript` | The operator projection over sessions. |
| `github.com/go-steer/mast/pkg/workload` | Bundle types — the Go form of `workload.yaml`. |
| `github.com/go-steer/mast/pkg/specialists` | Specialist spec, registry and loader. |
| `github.com/go-steer/mast/pkg/budget` | Limits and the usage meter. |

Start from the [library embed
quickstart](/quickstart/library-embed/), which uses only these.

## What it will not cover

Everything else under `pkg/`: `a2a`, `agui`, `approval`, `attach`,
`attachadapter`, `auth`, `config`, `digest`, `effects`, `envelope`,
`eventlog`, `federation`, `graph`, `inject`, `instruction`, `mcp`,
`modeltier`, `monitor`, `notify`, `observability`, `permissions`,
`planner`, `pricing`, `providers`, `router`, `serverauth`, `taskclass`
and `watchdog`.

These are importable and they are not supported. A minor release may
change or remove them. If you need one of them to be stable, open an
issue saying what you are building — a package with a named consumer is
the kind that gets promoted.

### A covered path can drag one in

The list above is not quite a partition. If a covered package names a
type from an unsupported one in an exported signature, that type is
covered too, whatever this page says — you cannot change it without
breaking the covered package.

So the two lists are being reconciled before v1.0, one leak at a time,
and there are two kinds:

- **A type you only ever receive.** `pkg/transcript` hands back
  `approval.Decision` records. Their shape is already public as the
  `mast.decision/v1` JSON that `mast sessions export-decisions` emits,
  so it is committed either way, and a Go copy of it would just be a
  second name for one schema. These will be listed as covered rather
  than replaced.
- **A type you have to construct.** `budget.Limits` used to take a
  `*pricing.Catalog`, which meant committing the catalog's constructor,
  its config-discovery options and its rate struct — none of which is
  about budgets, and all of which is still moving as mast adds backends.
  The field is now a one-method `budget.Pricer` interface that the meter
  owns, and `pkg/budget` imports nothing else from mast.

The rule, if you are reading your own dependency on mast the same way:
prefer receiving a type over constructing one.

## The ADK version is part of the contract

`mast.Config` takes two types from
[ADK](https://google.golang.org/adk), by design, so you can inject your
own model or session store:

```go
cfg := mast.Config{
    Model:    myModel,      // google.golang.org/adk/v2/model.LLM
    Sessions: mySessions,   // google.golang.org/adk/v2/session.Service
}
```

Go's semantic import versioning makes `adk/v3/model.LLM` a *different
type* from `adk/v2/model.LLM`, so when mast moves to a new ADK major
your code will not compile against it until you move too. That is a
breaking change, so **an ADK major bump ships as a mast major.** Such a
release carries no other breaking changes: the migration is your import
paths and nothing else.

## Wire protocols version separately

The [attach, A2A and AG-UI surfaces](/concepts/interop/) and the inject
endpoint are consumed by programs mast's compiler never sees, so they
do not follow mast's Go major. They carry their own version
information — the attach capabilities frame, the A2A agent card — and
you should **feature-detect against that**, not against mast's release
number. A protocol addition will not force a Go major, and a Go major
will not invalidate a client speaking the older frame.

The [workload bundle schema](/reference/workload-bundle/) versions
independently for the same reason: it is edited by people who never
import Go, and a new YAML key should not cost anyone a major upgrade.
It carries a [`schema_version:`](/reference/workload-bundle/#schema_version--and-why-an-unknown-key-is-refused)
of its own — currently `1`, and omitting it means `1` — which moves
when a key changes shape or meaning, not when a key is added.

## The CLI is covered

The [`mast` binary](/reference/cli/) is a first-class consumer shape,
and its callers are shell scripts, systemd units and Kubernetes
manifests — none of which a compiler can warn. Promised from v1.0:

- **Flag names and their meanings.**
- **Subcommands and their verbs** — `mast sessions <verb>` and `mast stop`.
- **Exit codes:** `0` the work completed; `1` the work was attempted and
  failed; `2` the invocation was rejected before any work started; `3`
  serve mode's shutdown drain expired with sessions still interrupted
  (their work is durable but unfinished — this is the code to restart
  on).

Not promised, and safe to change in any release: log lines, stdout
prose, `--help` wording, and [metric names](/reference/metrics/), which
have their own compatibility rules.

## What v1.0 does not claim

It is not a production-readiness badge. It says the API stops moving.

What backs the release today is an outcome-evaluation tier that gates
every tag, per-version acceptance suites, and a cluster-permission
matrix measured against live GKE. What does not exist yet is a
published threat model — worth knowing for a product whose whole
premise is an agent acting while nobody watches. Track both on the
[roadmap](/roadmap/).
