---
title: Providers and models
description: Gemini, Claude, and the offline fakes — how a model is selected, why a specialist may cross providers, and why an unresolvable override refuses to start.
sidebar:
  order: 6
---

Model quality moves month to month. A platform team that hard-codes one
vendor's client into its agent layer is carrying that curve as a risk it
cannot cheaply unwind — which is why multi-provider is a pillar here rather
than a compatibility shim, and why the provider surface is one of the
interfaces stable since v0.1.

## What you can point mast at

| Provider | Selected by | Notes |
|---|---|---|
| **Gemini** | a model id like `gemini-2.5-flash` | Google Search and URL-context builtins are available through the provider |
| **Claude, first-party** | `claude-*` with `ANTHROPIC_API_KEY` set | |
| **Claude on Vertex** | `claude-*` with a Vertex project, or `--provider=anthropic-vertex` | context caching supported |
| **`echo`** | `--model=echo` (the default) | offline fake, no credentials, never emits tool calls |
| **`scripted`** | `--model=scripted` | replays recorded turns from a JSONL file (`MAST_SCRIPT`) |
| **`toolactor`** | `--model=toolactor` | offline fake that *does* drive tool calls |

Switching is a config change, not a code change. `--model` picks the model;
`--provider` is an alias that validates it and, for `claude-*`, chooses the
backend — without it, `ANTHROPIC_API_KEY` selects the first-party API and a
Vertex project selects Vertex.

## The offline fakes are load-bearing

`echo`, `scripted`, and `toolactor` are not toys. They are how the whole
loop — inject, dispatch, park on an approval, `kill -9`, resume — runs in
CI and on a laptop with **no credentials at all**, which is what the
[quickstart](/quickstart/unattended-triage/) does in five minutes. An
acceptance suite that needs a live provider is an acceptance suite that
gets skipped.

`toolactor` matters specifically because `echo` never emits tool calls:
testing the write gate needs a fake that actually tries to call something.

## Per-specialist models

A specialist may name its own model, and **it may name a different
provider than the rest of the roster**:

```yaml
# specialists/triage-classifier.tmpl
model: gemini-2.5-flash-lite      # one cheap classifying turn

# specialists/OOMKilled.tmpl
model: claude-sonnet-4-6          # the one that has to reason
```

Overrides are dispatched by model id, exactly like `--model`, and
resolution is memoized per id — eight specialists on one tier share one
client. Specialists that declare nothing inherit the process model.

This is the tiering knob that makes a twelve-specialist roster affordable:
the classifier does not need the model the diagnoser does, and neither
needs the model the change executor does. Pair it with [per-specialist
budgets](/concepts/budgets/).

Two behaviours to know before you tier a roster:

- **An override that cannot be resolved fails startup.** It is never
  quietly downgraded to the parent's model. A bundle that *reads* as
  tiered while silently running everything on one tier is worse than one
  that refuses to boot — you would be paying for the expensive tier and
  believing you weren't, or running the cheap one on work that needed
  better. Credentials for every provider the roster names must resolve at
  construction.
- **Offline fakes collapse overrides.** Under `echo`, `scripted`, or
  `toolactor`, every override resolves back to the fake — so a tiered
  bundle still runs credential-free in smoke and acceptance runs.

One trade-off worth weighing: naming a concrete model id binds the bundle
to that provider. If other deployments fork your bundle, leaving the model
unset and letting `--model` decide keeps it portable.

## Cost

Spend is computed from each provider's token accounting for the models
actually used, so a cross-provider roster still rolls up to one
`max_cost_usd`. See [budgets](/concepts/budgets/).

## Reference

- [CLI](/reference/cli/) — `--model`, `--provider`, and the environment
  variables each backend reads.
- [Workload bundle](/reference/workload-bundle/) — per-specialist `model:`.
