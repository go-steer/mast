---
title: Providers and models
description: Gemini, Claude, provider profiles for OpenAI-compatible and self-hosted servers, and the offline fakes — how a model is selected, why a specialist may cross providers, and why an unresolvable override refuses to start.
sidebar:
  order: 6
---

Model quality moves month to month. A platform team that hard-codes one
vendor's client into its agent layer is carrying that curve as a risk it
cannot cheaply unwind — which is why multi-provider is a pillar here rather
than a compatibility shim.

Gemini and Claude reach that table through mast's own adapters, and
everything OpenAI-compatible reaches it through **provider profiles**. The
pillar is the **substitutability** rather than the number: the tier
indirection, a meter that prices the (backend, model) pair, and a judged
corpus that runs nightly against both. There is no `pkg/provider`
interface to implement — the extension point is `mast.Config.Model`, a
field typed on ADK's `model.LLM`, and the root package it lives on is one
of the six paths the [v1.0 promise](/reference/stability/) covers.

## What you can point mast at

| Provider | Selected by | Notes |
|---|---|---|
| **Gemini** | a model id like `gemini-3.5-flash`, or `--provider=gemini` | Google Search and URL-context builtins are available through the provider |
| **Gemini on Vertex** | `--provider=vertex`, or `GOOGLE_GENAI_USE_VERTEXAI=true` | same models, ADC instead of a key |
| **Claude, first-party** | `claude-*` with `ANTHROPIC_API_KEY` set | |
| **Claude on Vertex** | `claude-*` with a Vertex project, or `--provider=anthropic-vertex` | context caching supported |
| **A provider profile** | `--provider=<profile>` with `--model`, or the profile's tier | Vertex AI partner models (`vertex-maas`), Ollama (`ollama`), vLLM, SGLang, or any OpenAI-compatible endpoint. See [Provider profiles](#provider-profiles) |
| **`echo`** | `--model=echo` (the default) | offline fake, no credentials, never emits tool calls |
| **`scripted`** | `--model=scripted` | replays recorded turns from a JSONL file (`MAST_SCRIPT`) |
| **`toolactor`** | `--model=toolactor` | offline fake that *does* drive tool calls |

Switching is a config change, not a code change. `--model` picks the model;
`--provider` is an alias that validates it and chooses the backend within a
family. Each family has two aliases for the same model ids — `gemini` /
`vertex` and `anthropic` / `anthropic-vertex` — because a backend is not a
different model line. Without an alias the backend comes from the
environment: `GOOGLE_GENAI_USE_VERTEXAI` for `gemini-*`, and for `claude-*`
`ANTHROPIC_API_KEY` selecting the first-party API with a Vertex project
selecting Vertex.

## Provider profiles

A model outside the Gemini and Claude families is named by a **profile** plus
a model id, not by a prefix. The same `gpt-oss-20b` is served by Vertex AI
and by your own vLLM, at different prices and with different credentials.
A profile declares the endpoint, the credential, the wire format, what the
server can do, and which models it serves.

Profiles come from [core-models](https://go-steer.github.io/core-models/),
the provider library mast shares with core-agent. Its built-ins work with
nothing else configured:

```sh
mast --provider ollama --model qwen3:1.7b "what does CrashLoopBackOff mean?"
GOOGLE_CLOUD_PROJECT=my-project \
  mast --provider vertex-maas --model openai/gpt-oss-20b-maas "..."
```

Your own go in `.agents/providers/<name>.yaml`, one profile per file. Most
start from a built-in with `extends`:

```yaml
# .agents/providers/house-vllm.yaml
name: house-vllm
extends: vllm
base_url: http://vllm.infra.svc:8000/v1
usage: {cached_tokens: unreliable}   # this vLLM build reports nonsense cached counts
models:
  - {id: Qwen/Qwen3-Coder-Next, tier: mid, context_window: 262144}
  - {id: gpt-oss-20b, tier: small}
tiers: {mid: Qwen/Qwen3-Coder-Next, small: gpt-oss-20b}
```

Then run it with `mast --provider house-vllm`. That picks the profile's
model for the task's tier; `--model` picks one explicitly. Every field and
built-in is on the
[profiles reference](https://go-steer.github.io/core-models/reference/profiles/).

What mast does with a profile:

- **It refuses early.** An unknown profile, a model it doesn't serve, a
  tier it doesn't declare, or a missing API key or Google credential fails
  at startup, naming the profile. A misspelled key in the YAML is a load
  error.
- **Tiers come from the profile.** A specialist's `tier: small` under
  `--provider house-vllm` runs the profile's small model. Gemini and Claude
  keep mast's own tier tables; a profile only ever adds tiers for its own
  provider.
- **Usage is measured, and cost is priced where it can be.** Every call
  reports input and output tokens. Cached and reasoning tokens are reported
  when the server sends them, and shown as not reported (never as zero) when
  it doesn't.
  - **Vertex AI partner models** are priced from mast's catalog (published
    rates, refreshed weekly).
  - **A self-hosted model** is priced only if its profile declares what it
    costs you, under `models[].rates` (USD per million tokens). Otherwise
    cost shows as `$—` and the meter counts the call as unpriced.
  - A `max_cost_usd` ceiling on an unpriced model is **refused at startup**,
    because it could never trip. Declare rates, or bound the run with
    `max_tokens` or `max_turns`.
- **No server-side built-ins.** A bundle that turns on `builtin_tools`
  under a profile is refused rather than run without them.
- **Which models are worth it.** Five Vertex AI partner models have been
  through mast's judged corpus against Claude. GLM 5.2 and Kimi K2 Thinking
  match it on intent coverage. Llama 4 Maverick is unsupported for tool
  use. The results, and what the numbers mean, are on core-models'
  [Tested models](https://go-steer.github.io/core-models/reference/tested-models/)
  page.
- **Retries honor the server.** A 429 waits as long as `retry-after-ms` or
  `retry-after` asks, at the HTTP layer, before mast's own one outer retry.

## Credentials

Mast takes credentials from the environment; it never reads a key out of a
workload bundle. Nothing here is a mast-specific variable — each backend
reads the same names its own SDK does, so a working `gcloud`/genai/Anthropic
environment already works.

| Backend | Environment |
|---|---|
| Gemini API | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`; if both are set, `GOOGLE_API_KEY` wins) |
| Gemini on Vertex | `--provider=vertex` with `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION` (or `GOOGLE_CLOUD_REGION`; defaults to `global`) — plus Application Default Credentials |
| Claude, first-party | `ANTHROPIC_API_KEY` |
| Claude on Vertex | `ANTHROPIC_VERTEX_PROJECT_ID` (falls back to `GOOGLE_CLOUD_PROJECT`) and `CLOUD_ML_REGION` (falls back to `GOOGLE_CLOUD_LOCATION`, then `us-east5`) — plus Application Default Credentials |

**Prefer the alias to the environment variable for Gemini on Vertex.**
`GOOGLE_GENAI_USE_VERTEXAI=true` still works and still flips the backend, but
without it *or* `--provider=vertex` a project alone does nothing: the run goes
to the API-key backend and fails asking for a key the deployment does not
have. `--provider=vertex` names the backend outright and fails at startup
naming the project variable if it is missing.

For `claude-*` there is no such switch — a resolvable project is enough. That
asymmetry is worth remembering when one process runs both families.

On Cloud Run or GKE, prefer the service account's Application Default
Credentials over a key in the environment: with Workload Identity there is no
key to leak, rotate, or forget to scope.

## The offline fakes are load-bearing

`echo`, `scripted`, and `toolactor` are not toys. They are how the whole
loop — inject, dispatch, park on an approval, `kill -9`, resume — runs in
CI and on a laptop with **no credentials at all**, which is what the
[quickstart](/quickstart/unattended-triage/) does in five minutes. An
acceptance suite that needs a live provider is an acceptance suite that
gets skipped.

`toolactor` matters specifically because `echo` never emits tool calls:
testing the write gate needs a fake that actually tries to call something.

### One recording, concurrent branches

`scripted` differs from the other two in one way that matters: it holds a
position in a transcript. Every specialist in the roster shares the one
instance, and [fan-out
dispatch](/concepts/specialists-and-dispatch/#fanout--the-whole-roster-at-once)
runs its analysts
**at the same time**, so a single position walked by N branches at once
would hand recorded turns out by whichever branch got there first.

It doesn't. A replay is per branch: each concurrent lane gets its own
cursor over its own decode of the same recording, and each starts at turn
0. So a three-analyst fan-out against a one-turn recording is three
analysts that each replay that turn — not one analyst that replays it and
two that fail with `script exhausted`.

Nothing changes for a sequential shape. A coordinator, a planner, a
single-agent replay and a resumed run are all one lane, and they go on
consuming one script in one order, exactly as recorded. Two things this
does *not* do:

- One transcript cannot describe N *different* branches. Every branch
  replays the same turns, because a recorded turn does not name the agent
  it belongs to.
- Two invocations running at once in one process — a daemon handling two
  incidents — still share the unbranched replay. A recording describes one
  session.

## Per-specialist models

A specialist may name its own model, and **it may name a different
provider than the rest of the roster**:

```yaml
# specialists/triage-classifier.specialist.md
model: gemini-2.5-flash-lite      # one cheap classifying turn

# specialists/OOMKilled.specialist.md
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
  bundle still runs credential-free in smoke and acceptance runs. The
  collapse is exact for `echo` and `toolactor`, which are stateless.
  `scripted` is a cursor, so it collapses to one *instance* but not to
  one *position* — see below.

## Tiers: the portable way to say the same thing

Naming a concrete model id binds the bundle to that provider, which is a
real cost for a bundle other deployments fork. Usually what the bundle
means is not "this step needs Haiku" but "this step is not worth the
frontier model" — so say that:

```yaml
# specialists/triage-classifier.specialist.md
tier: small                       # one cheap classifying turn

# specialists/OOMKilled.specialist.md
tier: mid                         # the one that has to reason
```

`small`, `mid`, `frontier`. Mast resolves the tier against whichever
provider it is actually running — the `--provider` alias if you passed
one, otherwise the root model id's own prefix — so the roster reads the
same and costs the right thing on either backend:

| tier | Gemini | Anthropic |
|---|---|---|
| `small` | `gemini-3.5-flash-lite` | `claude-haiku-4-5` |
| `mid` | `gemini-3.5-flash` | `claude-sonnet-5` |
| `frontier` | `gemini-3.8-flash` | `claude-opus-5` |

A tier default names the latest model in its line, and it moves only
after that model has been run — not when the newer id appears in the
pricing catalog. `gemini-3.8-flash` was priced on 2026-09-09 and became
the `frontier` default on 2026-10-06, once it had passed both runs below.
`claude-opus-5-5` and `claude-sonnet-5-5` are priced as of 2026-10-06, and
`claude-haiku-5-5` as of 2026-10-07; none is a default yet. Pin one with
`model:` if you want it today. Mast's catalog bills `claude-haiku-5-5` at its
up-to-100K-token price throughout, so a call with a longer prompt costs more
than mast reports; Anthropic charges five times as much above that line.

"Has been run" is a weekly job, not a judgement call: a candidate model
runs the same 31-scenario judged corpus the incumbent default runs every
night, graded by the incumbent so only one variable moves, and the two
boards are diffed. Scoring within noise of the incumbent — and passing
the outcome tier on the same model — is what promotes it.

Both behaviours above carry over unchanged: an unresolvable tier fails
startup, and the offline fakes collapse tiers back to the fake. Startup
logs each tier next to the id it became, so what a roster is spending is
readable without knowing the table. A root model whose provider cannot be
told from the alias or the id fails startup asking for `--provider`
instead of guessing a vendor.

A spec declares `model:` or `tier:`, never both — that is a load error,
not a precedence rule. Pin an id when the bundle has a reason to care
which vendor answers; declare a tier the rest of the time.

## Server-side built-in tools are off unless the bundle asks

Each provider ships tools that run on the vendor's own servers — Gemini's
`google_search`, `url_context` and `code_execution`, Anthropic's
`web_search`. They are the one capability mast cannot see: a built-in never
comes back as a tool call, so the permissions gate, the write gate and the
effect outbox all look right past it, and a `read_only` specialist could be
reading the public internet with nothing in the transcript to say so.

So mast starts every provider with all of them **off** and lets the bundle
turn one on:

```yaml
builtin_tools:
  web_search: true
```

mast's baseline rather than each vendor's is the deliberate part. Gemini
ships search and URL context on, Anthropic ships search off; inheriting
those would mean the same bundle reaching further on one provider than the
other, which is exactly what this page's promise rules out. The startup
line reports what the constructed model will send —
`builtin_tools=web_search` — so a key that did not take is visible rather
than assumed. See
[`builtin_tools:`](/reference/workload-bundle/#builtin_tools--the-providers-own-server-side-tools).

## Asking a model to think, or not to

mast does not ask any model to think. Every provider decides that for
itself on the request mast sends, which for a reasoning model usually
means it thinks. The knob exists for library embedders who build the
request themselves: `genai.ThinkingConfig`, with a token budget and an
`IncludeThoughts` flag.

Since v0.9 that knob reaches Anthropic in the shape the target model
accepts, which is not one shape. Anthropic replaced the budget-carrying
`thinking.type=enabled` with `thinking.type=adaptive` at the 4-6
generation and removed the old one at 4-7, so a budget sent to
`claude-opus-5` used to be a **400, not a degraded turn** — and
`claude-opus-5` is mast's own default Claude model. mast now picks by
model: a budget goes to older models as a budget, and to newer ones as
"think", because there is no field on the newer API that takes a number
of tokens.

A budget of **zero** is the one case worth knowing about. It reads as
"do not think", and it is the one request that could not say so: mast
sent no thinking parameter at all and the model thought anyway. It now
sends `thinking.type=disabled`, which every Claude model accepts, and
the reasoning stops.

`IncludeThoughts: true` asks for the reasoning text itself. It is off by
default, and that is a cost decision as much as a privacy one: mast does
not publish a model's thinking on any surface (see
[interop](/concepts/interop/#what-none-of-them-publish)), so paying for
text nothing reads would be waste. Older Claude models have no way to
express the request and ignore the flag, as they always have.

## Cost

Spend is computed from each provider's token accounting for the models
actually used, so a cross-provider roster still rolls up to one
`max_cost_usd`. See [budgets](/concepts/budgets/).

Providers do not agree on which token buckets exist, and the record mast
reads usage through is Google's, so a count Anthropic reports and Gemini
does not had nowhere to go. Since v0.9 each adapter also carries what it
was actually told — cache reads, cache writes, reasoning and tool-use
tokens, plus the model the backend says it ran — as a normalized record
beside the usual one, distinguishing *reported zero* from *not
reported*. That is what closed the cache-write under-billing described
in [budgets](/concepts/budgets/#where-the-dollar-figure-comes-from).

## Vertex context caching is built, and nothing wires it

mast carries a manager for an explicit Vertex context cache — create
it, extend its TTL, drop it when Vertex reaps it — and the Gemini
provider can stamp that cache onto each request. **Nothing turns it
on.** The `mast` binary never has: a daemon you start from the CLI runs
uncached. Until the packages outside the [v1.0
surface](/reference/stability/) moved under `internal/`
([#301](https://github.com/go-steer/mast/issues/301)), Go code could
construct the manager itself; it no longer can.

Nothing about it is configurable from a flag or a bundle, on purpose:
the cache is scoped to one system instruction and tool set, and
deciding when that is stable enough to cache is the embedding program's
call, not ours. That makes it a candidate option for a custom `main.go`
rather than a flag. If you have a workload that wants it, open an issue
saying so — that is what brings it back.

When it is wired, an evicted or expired cache is recovered
transparently — the turn that meets it retries uncached and the manager
creates a fresh cache on the next one. There is no state to clear and
no error your code sees.

## Reference

- [CLI](/reference/cli/) — `--model`, `--provider`, and the environment
  variables each backend reads.
- [Workload bundle](/reference/workload-bundle/) — per-specialist `model:`
  and `tier:`.
