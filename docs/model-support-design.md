# Model support: managed providers and self-hosted servers

**Status:** proposal, 2026-08-18. Nothing here is settled; the resolved-decisions
section at the bottom is empty on purpose and fills in as the open questions are
answered. Targets v0.5 and later. Scope is **mast**; core-agent has the same
shape of problem and is deliberately out of scope for this pass (see
[§9](#9-core-agent)).

---

## 1. The ask, and why it is not "add two more adapters"

mast serves Gemini (API key or Vertex) and Claude (first-party or Vertex). Two
vendors, four backends, and both of them have a hand-written adapter under
[`../pkg/providers/`](../pkg/providers/). That has been enough, and it is
running out: the models a platform team wants to point an unattended agent at
now include OpenAI's, xAI's, and — for anyone with a GPU budget or a data-egress
rule — an open-weight model on their own hardware.

The awkward part is that mast already *claims* the general case. Multi-provider
is one of the four pillars in [`./positioning.md`](./positioning.md), and the
shipped docs page
([`./site/src/content/docs/concepts/providers.md`](./site/src/content/docs/concepts/providers.md))
says "switching is a config change, not a code change". Inside the two families
that is true. Outside them the sentence describes an intention, and ten
places in the code quietly assume the two families are the world
([§3](#3-what-the-code-assumes-today)).

So this is not "write an OpenAI adapter". It is: decide what mast means by
*supported*, find the assumptions that only hold for two vendors, and pick an
order of work that gets the most model coverage per unit of adapter.

Two requirements come from the ask directly and shape everything below:

- **Managed providers must measure cost and usage as well as Gemini and Claude
  do today** — per-turn input/output/cached/reasoning tokens, a dollar figure
  the budget meter can enforce a ceiling against, and the same `/usage`,
  `/stats` and metrics surfaces.
- **Self-hosted models must report input/output tokens at minimum**, plus
  whatever KV-cache visibility the model server is willing to give up. Per-token
  *cost* is not a thing a self-hosted model has; pretending otherwise is worse
  than reporting nothing ([§4.5](#45-pricing-and-the-self-hosted-cost-fiction)).

---

## 2. What "supported" has to mean

A provider is supported when all eight hold. These are the acceptance bar for
every slice in [§6](#6-sequencing), and the reason the list is this long is
[`./sibling-sync.md`](./sibling-sync.md)'s first triage: mast shipped every tool
it defines to Claude as `{"type":"object","properties":{}}`, survived two green
judged nightlies and a live GKE run, and nothing errored. A provider that
returns text is not a provider that works.

| # | Requirement | Why it is on the list |
|---|---|---|
| **R1** | **Resolvable** from `--model`, a specialist `model:`, and a specialist `tier:` — and unresolvable *fails at construction*, never at the incident | The property `NewModelResolver` and `TierModelName` already enforce; a new provider must not become the first quiet downgrade |
| **R2** | **Agent-loop correct**: tool calls round-trip with their real input schema, multi-turn tool loops survive, reasoning/thinking blocks round-trip where the API requires echoing them, streaming accumulates, finish reasons map | The empty-schema P0 above. Measured, not eyeballed — see R8 |
| **R3** | **Usage measured per turn**: prompt, completion, total, cache-read, cache-write, reasoning — each either a number or an explicit *not reported*, never a zero standing in for unknown | "Same usage/stats as Gemini and Claude" is the ask; the zero-vs-unknown distinction is what makes a cost figure auditable |
| **R4** | **Priced, or explicitly unpriced**: a catalog entry keyed to the backend that actually served the tokens, or a rendered `$—` | `pkg/pricing` already refuses to render `$0` for an unknown rate. A new backend must not silently inherit a wrong rate — which is [#178](https://github.com/go-steer/mast/issues/178) today for Claude on Vertex |
| **R5** | **Tiered in both directions**: `modeltier.Classify` knows the model, `taskclass.ModelForTier` can name it | Without the reverse direction `--task` is inert and the small-tier-parent guard goes quiet; without the forward direction `tier:` rosters cannot run on the provider at all |
| **R6** | **Budget-enforceable**: `RatePer1K` resolves and `budget.Meter` prices the turn from the catalog rather than the flat fallback | A `max_cost_usd` ceiling on an unpriced model is a ceiling in name only |
| **R7** | **Observable**: `/usage` per-turn rows, `/stats`, `mast_*` metrics, and the per-specialist cost attribution `MeterScopes` produces | v0.4's `J-cost-tier` check asserts tier resolution against *reported* `ModelVersion`; a provider that doesn't populate it can't be checked |
| **R8** | **Testable offline first**: a recorded-turn fixture that runs credential-free in the U tier, and tool-calling metrics from the E/J tiers before the provider is called supported | [`./v0.3-plan.md`](./v0.3-plan.md) §2's tiers. Issues [#168–#172](https://github.com/go-steer/mast/issues/172) are building exactly the tool-calling measurement a new provider needs to pass. The gating layer is in: [#168](https://github.com/go-steer/mast/issues/168) shipped `internal/toolcatalog`, and **a new adapter's first test is its `toolwire_test.go`** — see below |

R2 and R8 are the ones that will actually cost time. Transport is a week;
proving a model drives mast's tool loop correctly is the rest of the slice.

**The R2/R8 floor a new adapter has to clear on day one** is
`internal/toolcatalog` (#168). It assembles the tool catalog a real mast turn
puts in front of a model — driving a coordinator+MCP rig and a planner rig
through an ADK runner with a recording model, so the catalog is *captured*
rather than enumerated — and `toolcatalog.Verify` states the invariant over
it: every tool arrives, with every argument it declares, with the required
ones still required, and with each argument's own schema intact.

An adapter supplies one thing: a reader that pulls its own wire spelling out
of a captured request body (`input_schema` for Anthropic,
`parameters`/`parametersJsonSchema` for Gemini) and hands it over as a
`toolcatalog.Wire`. See `pkg/providers/anthropic/toolwire_test.go` for the
pattern. Writing that reader is an hour; it is the difference between an
adapter that has been asserted against every construction path mast uses and
one that has been asserted against whichever tool its author happened to
think of — which is precisely how the empty-schema P0 above reached a
release.

The invariant is deliberately shared rather than reimplemented per adapter:
an adapter held to a weaker check than its siblings is how a defect hides.
Gemini's copy of the test is a dependency probe, not a test of mast code —
nothing in mast converts schemas on that path — and it is worth its keep for
the same reason, since `ParametersJsonSchema` is a pass-through `any` that
nothing would complain about losing.

---

## 3. What the code assumes today

Ten seams, all reachable from `--model`. The table is the requirements list
restated as work.

| Seam | Anchor | Assumption | What a third family breaks |
|---|---|---|---|
| Model construction | `internal/compose/compose.go:411` `BuildModel` | model id prefix (`gemini-`, `claude-`) selects the adapter | An arbitrary id (`Qwen/Qwen3-Coder-Next`, a Vertex MaaS `deepseek-ai/…-maas`, an Azure deployment name) has no prefix mast can dispatch on |
| Backend selection | `internal/compose/compose.go:529` `anthropicProvider` | one env-var probe order picks first-party vs Vertex | Every new family needs its own credential-detection story, and env-var probing does not scale to N endpoints |
| Tier family | `internal/compose/tier.go:54` `providerFamily` | explicit `--provider`, else the root id's prefix | Errors out on any id it doesn't recognize — correct behavior, wrong coverage |
| CLI validation | `cmd/mast/oneshot.go:322`, `cmd/mast/main.go:112` | `--provider` is a closed enum of five, each validating a model-id prefix | Enum grows per provider; prefix validation is not expressible for self-hosted names |
| Tier → model | `pkg/taskclass/taskclass.go:232` `ModelForTier` + `Providers()` | hand-kept per-provider table of three tiers | One new column per provider, and `TestModelForTier_ReturnsLatestInLine` needs the pricing catalog to know the family |
| Model → tier | `pkg/modeltier/modeltier.go:100` `Classify` | hand-kept substring table | Same, in reverse, and an unclassified model silently reverts to the universal 0.85 compaction threshold |
| Price catalog | `pkg/pricing/builtin.go` + `dev/regen-builtin-pricing/main.go:117` | `familyPrefixes = ["gemini-", "claude-"]`, and **ids containing `/` are dropped as router noise** (`main.go:451`) | The `/` rule drops exactly the ids the new backends use; the prefix list drops everything else |
| Flat rate fallback | `internal/compose/compose.go:579` `RatePer1K` | prefix switch with per-family fallbacks | A third family lands in `default: 0.001`, an invented number |
| Usage projection | `pkg/providers/anthropic/stream.go:106`, `pkg/providers/gemini` | provider usage folds into `genai.GenerateContentResponseUsageMetadata` | That struct has no cache-**write** bucket (documented undercount, `stream.go:123`), and nothing at all for KV-cache stats |
| The cost check's own matcher | `internal/evals/judge/cost.go:403` | reported `ModelVersion` relates to the resolved id by `HasPrefix` in either direction — right for Vertex's date suffixes (`claude-opus-5@20251101`) | A backend that reports an unrelated string (a Vertex MaaS `deepseek-ai/…-maas` id, an Azure deployment name, a vLLM local weights path) fails the *resolved-is-not-the-same-claim-as-ran* assertion and so **fails the nightly**. That is the check refusing to certify what it cannot verify — correct behavior — but it needs a per-profile identity rule rather than a prefix |

Under all ten sits one assumption worth naming on its own, because it is the
one that decides the shape of the fix:

> **A model id is globally unique, unprefixed, and names its own vendor.**

That holds for `gemini-*` and `claude-*` and for nothing else. `gpt-5.6-sol` is
served by OpenAI, by Azure under a deployment name the operator chose, and
(as `gpt-oss-*`) by Vertex MaaS and by a vLLM process on someone's cluster — at
four different prices, with four different auth stories, and in two of those
cases with a different wire dialect. `grok-4.6` is served by xAI and by Vertex.
The same model id therefore cannot key the price table, cannot pick the
credential, and cannot pick the transport.

---

## 4. Design

### 4.1 Three wire dialects, not N providers

Every model mast wants to reach speaks one of five wire shapes, and those
collapse into three families — genai, Anthropic Messages, and OpenAI-shaped.
Two of the five are already built:

| Dialect | Who speaks it | Status |
|---|---|---|
| `genai` | Gemini (API key + Vertex) | shipped — `pkg/providers/gemini` wraps ADK's `model/gemini` |
| `anthropic` | Claude on first-party, Vertex, and **Bedrock** | shipped for two of three — `pkg/providers/anthropic` |
| `openai-chat` (`/v1/chat/completions`) | Vertex MaaS partner models, xAI, vLLM, SGLang, Ollama, llama.cpp, NIM, Groq, Together, Fireworks, DeepSeek, Mistral, Moonshot, OpenRouter, a LiteLLM proxy | **not built — the whole of this proposal's leverage** |
| `openai-responses` (`/v1/responses`) | OpenAI first-party, xAI | **not built**; ADK ships an experimental one |
| `bedrock-converse` | Bedrock's non-Anthropic models (Nova, Llama) | not built; P1 at best ([§5](#5-the-prioritized-lists)) |

The single most valuable thing on that list is `openai-chat`, because it is one
adapter that reaches every managed long-tail provider *and* every self-hosted
server *and* Google's own partner-model surface. Vertex AI's MaaS models are not
served through `generateContent`; they are served through an OpenAI-compatible
Chat Completions endpoint at
`/v1/projects/{project}/locations/{region}/endpoints/openapi/chat/completions`,
under ADC — the credential mast already resolves for Gemini-on-Vertex and
Claude-on-Vertex.

**Decision proposed:** mast owns a `pkg/providers/openai` package implementing
**both** OpenAI-shaped dialects, with `openai-chat` as the default and
`openai-responses` opt-in per profile.

The alternative — adopt ADK's `model/openaimodel` and call it done — fails on
coverage. That package is Responses-only and marked `EXPERIMENTAL`, so it
reaches OpenAI and xAI and nothing else on the list. It is still worth wrapping
rather than reimplementing for the first-party OpenAI path, exactly as
`pkg/providers/gemini` wraps ADK's Gemini model rather than forking it; its
usage conversion already maps `cached_tokens` and `reasoning_tokens`, and it
already parks provider ids in `LLMResponse.CustomMetadata`, which is the seam
[§4.3](#43-usage-one-normalized-record-carried-beside-the-genai-one) builds on.

Dependency note for the slim-deps gate: `github.com/openai/openai-go/v3` is
already a **direct require of `google.golang.org/adk/v2`**, so it is in mast's
module graph and merely pruned from the build list today. Importing it adds no
new vendor relationship — but its go.mod names the Azure and AWS SDKs as direct
requires (for its `azure/` and `bedrock/` subpackages), so `go.sum` grows even
if we import neither. Worth a sentence in the PR, not a redesign.

### 4.2 A model is named by (profile, model id)

Replace prefix inference with a **provider profile**: a named, declared record
of endpoint + credential + dialect + capabilities + the models it serves. The
profile is what `--provider` names, and it is what keys the price table.

```yaml
# .agents/providers.yaml   (discovery per ./config-layout-design.md)
providers:
  - name: openai                      # built-in profile, shown for shape
    dialect: openai-responses
    auth: {kind: api_key, env: OPENAI_API_KEY}
    tiers: {frontier: gpt-5.6-sol, mid: gpt-5.6-terra, small: gpt-5.6-luna}

  - name: vertex-maas                 # built-in
    dialect: openai-chat
    base_url: https://{region}-aiplatform.googleapis.com/v1/projects/{project}/locations/{region}/endpoints/openapi
    auth: {kind: google_adc}
    tiers: {frontier: "deepseek-ai/deepseek-v4-pro-maas", small: "openai/gpt-oss-20b-maas"}

  - name: house-vllm                  # operator-authored, self-hosted
    dialect: openai-chat
    base_url: http://vllm.infra.svc:8000/v1
    auth: {kind: none}
    metrics_url: http://vllm.infra.svc:8000/metrics    # §4.4
    capabilities: {server_tools: false, response_schema: true, reasoning_echo: false}
    models:
      - id: Qwen/Qwen3-Coder-Next
        tier: mid
        context_window: 262144
        rates: {input_per_mtok: 0, output_per_mtok: 0}   # declared free; see §4.5
```

Consequences, each of which is the point rather than a side effect:

- **`--provider` stops being a closed enum** and becomes "a profile name",
  built-in ones included. `resolveModelSelection`'s per-family prefix
  validation becomes "is this id in the profile's model list, or does the
  profile allow open-ended ids" (self-hosted profiles must, since the operator
  names the model).
- **`providerFamily` gets an explicit answer** for every model reached through a
  profile, and the prefix fallback survives only as compatibility for
  `gemini-*` / `claude-*` roots started without `--provider`.
- **Tier tables move off the hard-coded switch for new providers.** `tiers:` in
  the profile is the same three-label vocabulary `taskclass.ModelForTier`
  serves, declared where the endpoint is declared. The built-in Gemini and
  Anthropic tables stay in Go — they are load-bearing for zero-config operation
  and pinned by cross-table tests — but a fourth column per provider does not
  get hand-added forever.
- **Cross-provider specialist overrides keep working unchanged**, because
  `BuildModel` still dispatches on a resolvable name; it just resolves through
  the profile registry rather than a prefix switch.

Open question OQ-1 ([§8](#8-open-questions)) is whether this lives in a new
`.agents/providers.yaml` or a `providers:` section of the existing
`.agents/config.json`; [`./config-layout-design.md`](./config-layout-design.md)
owns that call.

### 4.3 Usage: one normalized record, carried beside the genai one

`genai.GenerateContentResponseUsageMetadata` carries `PromptTokenCount`,
`CachedContentTokenCount`, `CandidatesTokenCount`, `ThoughtsTokenCount`,
`ToolUsePromptTokenCount` and `TotalTokenCount`. That is enough for Gemini,
nearly enough for Claude, and not enough for the rest:

- there is **no cache-write bucket** — the documented Anthropic undercount at
  `pkg/providers/anthropic/stream.go:123`, roughly
  `cache_creation_tokens × input_rate × 0.25` on every cache-warming turn.
  OpenAI's GPT-5.6 family bills cache writes at 1.25× input too, so a second
  provider is about to inherit the same gap;
- there is **nowhere to put KV-cache statistics** from a self-hosted server;
- there is **nowhere to record that a field was not reported**, as against
  reported zero — the distinction R3 turns on.

ADK's `model.LLMResponse` already carries `CustomMetadata map[string]any`, and
`openaimodel` already writes provider ids into it. So:

**Decision proposed:** define `pkg/providers/usage.Detail`, attach it under a
stable key (`mast.usage_detail`) from every provider adapter, and have
`budget.Meter` and the `/usage` projection read it when present and fall back to
the genai fields when absent.

```go
// Sketch. Every count is a *pointer or an accompanied Reported bitmask —
// nil/unset means "the provider did not say", which is not zero.
type Detail struct {
    CacheReadTokens   *int64  // Anthropic cache_read, OpenAI cached_tokens, vLLM cached_tokens
    CacheWriteTokens  *int64  // Anthropic cache_creation, OpenAI cache writes
    ReasoningTokens   *int64  // where it isn't already ThoughtsTokenCount
    ToolUseTokens     *int64
    ServedModel       string  // what the backend says it ran (R7 / J-cost-tier)
    ProviderRequestID string  // the id a support ticket needs
    Backend           string  // profile name — the price key, per §4.5
    Region            string  // Vertex/Bedrock region, for regional price uplifts
    KV                *KVStats // self-hosted only; see §4.4
}
```

Two properties come along for free and both are worth stating, because they are
existing bugs this closes rather than new features:

- The Anthropic cache-write undercount can finally be priced, and the fix is
  **smaller than the code comment describing it says**. `stream.go:128` calls for
  "a new `Rates.CacheCreationInputPerMTok` field, a `CostUSDWithCache` signature
  bump, and a sidecar" — the first two shipped since (`pkg/pricing/pricing.go:92`
  and `:133`, populated for every Claude row in `builtin.go` and refreshed from
  LiteLLM at `refresh.go:328`). Only the sidecar is missing, and the only caller
  of `CostUSDWithCacheWrites` today is `CostUSDWithCache` passing a hard-coded
  zero (`pricing.go:121`). The comment should be corrected in the same PR.
- `budget.priceOf` clamps `cached > prompt` today because a provider that
  over-reports the cached counter would otherwise be *credited* tokens. That
  guard generalizes to every new bucket and must be written once, in the meter,
  not per adapter.

### 4.4 Self-hosted: tokens from the response, KV stats from `/metrics`

The ask is "minimally input/output tokens, ideally some KV cache stats depending
on what the model server provides". The honest reading of what the servers
provide as of August 2026:

| Server | Per-request tokens | Per-request cached tokens | KV/prefix cache visibility |
|---|---|---|---|
| **vLLM** | yes, OpenAI-shaped `usage` | **unreliable** — `prompt_tokens_details` is null on the V1 engine even with `--enable-prompt-tokens-details`, a 14-month-old open bug with an incomplete fix; some builds do emit it | Prometheus: `vllm:prefix_cache_queries`, `vllm:prefix_cache_hits`, `vllm:kv_cache_usage_perc`, `vllm:gpu_cache_usage_*` |
| **SGLang** | yes | varies | Prometheus with `--enable-metrics`: `sglang_cache_hit_rate`, `sglang:hicache_host_used_tokens` / `_total_tokens`; OTel integration |
| **Ollama** | yes | no | `/api/metrics` has request/token counters but **no per-request KV stats** — the cache is opaque |
| **llama.cpp** | yes | no | Prometheus behind `--metrics` |
| **NVIDIA NIM** | yes (OpenAI-shaped) | varies by backend | Prometheus, Triton-shaped |

So per-request KV visibility is not something a design can require. Two tracks,
and the second is opt-in:

1. **Tokens are mandatory.** `usage.prompt_tokens`/`completion_tokens` come back
   from every one of these servers on the `openai-chat` dialect. When
   `prompt_tokens_details.cached_tokens` is present it populates
   `Detail.CacheReadTokens`; when it is absent the field stays nil and renders
   as *not reported*, never as zero. A profile may set
   `usage: {cached_tokens: unreliable}` to suppress the field entirely on a
   known-buggy server build rather than have operators chase a nonsense number.
2. **KV stats are a declared scrape.** A profile with `metrics_url:` gets its
   `/metrics` sampled by mast at session start and session end, and the delta
   (prefix-cache hits/queries over the window, peak KV utilization) is attached
   to the session's usage record as **fleet-level, time-correlated context** —
   labelled as such. It is not per-turn, it is not attributed to a specialist,
   and it never enters a cost figure. A shared inference server is serving other
   tenants during mast's window and pretending otherwise would produce a number
   that looks per-turn and isn't.

The scrape is worth building because it is the only route that works today —
the bug reports that show `prompt_tokens_details: null` also show
`vllm:prefix_cache_hits_total > 0` on the same process.

### 4.5 Pricing, and the self-hosted cost fiction

Four changes to `pkg/pricing` + the generator, in dependency order:

1. **Key rates by (profile, model), falling back to bare model.** Same model,
   different backend, different price — Claude on Vertex vs first-party is the
   case mast already has open as
   [#178](https://github.com/go-steer/mast/issues/178). Adding OpenAI-on-Azure
   and Grok-on-Vertex without this would be shipping three more instances of a
   bug we have already filed. This change closes #178 rather than joining it.
2. **Widen `familyPrefixes` and rehabilitate the `/` rule.** The generator drops
   any LiteLLM key containing `/` as router noise (`main.go:451`). Under (1)
   those keys become the *useful* ones: `vertex_ai/…`, `xai/…`,
   `azure/…` map onto profile-qualified mast keys. The eligibility predicate
   (chat mode, function calling published and true, not deprecated, text
   modality) carries over unchanged and is the right filter for the wider set.
3. **Model the rate shapes we currently cannot express.** OpenAI's GPT-5.6
   family bills requests over 272K input tokens at 2× input and 1.5× output
   **for the whole request**, adds a 10% uplift on data-residency endpoints, and
   charges cache writes at 1.25×. `Rates` has four scalars and no notion of a
   context-length tier, so a long-context OpenAI turn would be undercounted by
   half. Minimum viable: a `LongContext{ThresholdTokens, InputMult, OutputMult}`
   field, populated from LiteLLM's above-threshold cost fields. Anything still
   unmodelled must be *surfaced* as approximate — the meter already tracks
   `Unpriced()` for exactly this reason, and the precedent for how to behave is
   the cache-write gap: documented in the code, visible in the issue tracker,
   fixed on a slice, never silently wrong.
4. **Self-hosted is unpriced by default.** There is no per-token price for a
   model running on your own GPU; the real number is
   `$/GPU-hour ÷ tokens-per-hour`, which depends on batch occupancy mast cannot
   see. So: no catalog entry, `$—` in every display, and `RatePer1K` returns
   zero-with-unpriced rather than falling into today's invented `default: 0.001`.
   An operator who wants a number declares `rates:` in the profile, which feeds
   `pkg/pricing`'s existing `cfg-override` layer and is labelled `operator
   declared` in `/pricing` output. A budget ceiling against an unpriced model
   must fail loudly at startup, not silently never trip — that is R6, and it is
   the one place this design refuses a convenient default.

### 4.6 Capabilities are declared, and mismatches refuse at startup

Server-side builtins do not generalize: Gemini has Search grounding and URL
context, Anthropic has `web_search`, OpenAI has its own hosted tools, and a vLLM
process has none. Structured output is worse, because mast has a *shape* that
depends on it — the `bounded` dispatch shape
(`pkg/workload/bundle.go:308`) requires a specialist `output_schema:`, which
loads to a `*genai.Schema` and reaches the agent as `llmagent.Config.OutputSchema`
(`pkg/specialists/spec.go:42`). An adapter has to render that into whatever its
dialect calls structured output — `response_format: {type: json_schema}` on the
OpenAI shapes — and a server that accepts the field and ignores it turns the
shape's one guaranteed-parseable call into an unparseable paragraph at run time.
Note that "ignores it" is the *common* case on self-hosted servers: vLLM and
SGLang honor it only with a guided-decoding backend enabled, and Ollama's
support is partial.

So capabilities are declared per profile (`server_tools`, `response_schema`,
`reasoning_echo`, `parallel_tool_calls`, `streaming`) and `internal/compose`
refuses a bundle that needs one the profile does not have — at startup, naming
the profile and the capability. The precedent is `builtinsCompatible` in
`pkg/providers/gemini`, which silently dropped grounding on incompatible model
versions and cost a live debugging session for it
(`taskclass.go:253`'s note about research being unable to search).

### 4.7 What this deliberately does not build: a proxy

LiteLLM stays where it is: a **build-time price catalog** that
`dev/regen-builtin-pricing` reads, and a **runtime target** reachable as an
ordinary `openai-chat` profile pointed at a LiteLLM or OpenRouter base URL. Zero
code, and it is the supported answer for the long tail nobody has asked for yet.

What it does not become is a required hop. Three reasons, in the order they
matter for an unattended agent:

- an unattended workload's failure modes should not include a second process's,
  and mast's whole pitch is what happens when nobody is watching;
- approvals, the write gate, the watchdog and the budget meter are chokepoints
  mast owns (`runTurnPre`); a proxy that retries or re-routes underneath them
  makes a turn mast adjudicated and a turn the provider ran two different
  things;
- per-provider usage detail is precisely what a normalizing proxy flattens —
  the shipped illustration is LiteLLM's own open issue where a vLLM response
  carrying `cached_tokens: 149936` is reported downstream as "Cache Read Tokens:
  0". [§4.3](#43-usage-one-normalized-record-carried-beside-the-genai-one)'s
  whole point is not losing that.

---

## 5. The prioritized lists

### 5.1 Managed providers

**P0 — ship first (v0.5).** Three items, and the second is the highest
coverage-per-adapter on the whole page.

| # | Target | Dialect | Why now | Cost/usage story |
|---|---|---|---|---|
| 1 | **OpenAI first-party** — `gpt-5.6-sol` / `terra` / `luna` (GA 2026-07-09, 1.05M context) | `openai-responses`, wrapping ADK's `openaimodel` | The single most-asked-for provider; best-documented usage of any candidate | Complete: `cached_tokens`, `reasoning_tokens`, published rates. Needs the long-context multiplier of [§4.5](#45-pricing-and-the-self-hosted-cost-fiction)(3) — cached reads are 10% of input, writes 1.25× |
| 2 | **Vertex AI MaaS partner models** | `openai-chat` over ADC | One profile unlocks **Grok, DeepSeek V4, Qwen3, gpt-oss, Kimi K3, GLM, Llama 4** under the GCP auth, IAM, quota and billing mast already resolves. No new credential story, no new vendor relationship, and it is the path an enterprise on GCP will actually take | Usage is OpenAI-shaped; **prices differ from first-party**, so it depends on the (profile, model) key of §4.5(1) |
| 3 | **xAI first-party** — `grok-4.6`, `grok-4.3`, `grok-4.1-fast` | `openai-chat` (also speaks `openai-responses`) | Named in the ask; a profile and catalog rows once #2's dialect exists | Full `prompt_tokens_details.cached_tokens` + `completion_tokens_details.reasoning_tokens`; automatic prompt caching billed at a lower cached rate |

**P1 — next (v0.6), demand-ordered.**

| # | Target | Dialect | Note |
|---|---|---|---|
| 4 | **Claude on Bedrock** | `anthropic` | The cheapest item on this page: `anthropic-sdk-go` ships a `bedrock` subpackage, so this mirrors the existing `vertex.go` (~120 lines) and inherits every Anthropic usage bucket already mapped |
| 5 | **Azure OpenAI / AI Foundry** | `openai-responses` / `openai-chat` | Enterprise ask; `openai-go` ships an `azure/` subpackage. The wrinkle is real: the model is addressed by an operator-chosen **deployment name**, which is the strongest argument for the (profile, model) key — the id says nothing about the model or its price. Also serves Grok |
| 6 | **The OpenAI-compatible managed long tail** — Groq, Together, Fireworks, DeepSeek direct, Mistral, Moonshot | `openai-chat` | **Configuration, not engineering**, once #2 lands: a profile plus catalog rows plus tier entries. Ship them as built-in profiles in one PR and let demand decide which get a live smoke |
| 7 | **Bedrock Converse** (Nova, Llama, non-Anthropic) | `bedrock-converse` — a fourth dialect | Only worth a dialect if the demand is for Bedrock's *non-Claude* models specifically; #4 covers the Claude-on-AWS case without it |

**P2 — nice to have, no work scheduled.** OpenRouter (reachable as an
`openai-chat` profile today, per [§4.7](#47-what-this-deliberately-does-not-build-a-proxy)),
Cohere, IBM watsonx, OCI GenAI, Databricks, SageMaker custom endpoints.

**Explicitly demoted from the ask: Llama as a headline target.** The premise
that Llama is a top-three target has aged out. There is no Llama 5; Meta's
August 2026 open-weight return is Muse Glimmer, a 30B Apache-2.0 tool-calling
model rather than a frontier one, and the announced Muse Spark 1.2 weights had
not shipped as of mid-August. Llama 4 (Maverick/Scout) rides along free via
Vertex MaaS and Bedrock and needs no dedicated work. The open-weight models
worth naming as targets are the ones in
[§5.2](#52-self-hosted-model-servers) — GLM, DeepSeek, Qwen, Kimi.

### 5.2 Self-hosted model servers

The unit of support here is the **server**, not the model: one `openai-chat`
profile serves whatever weights the operator loaded. The list is therefore short
and the model question collapses into "which weights do we validate against".

**P0 — vLLM.** The default answer for serious self-hosting, OpenAI-compatible
Chat Completions, and the richest KV-cache metrics of any candidate. Ship with
the `metrics_url` scrape of [§4.4](#44-self-hosted-tokens-from-the-response-kv-stats-from-metrics)
and the `cached_tokens: unreliable` escape hatch, because the null
`prompt_tokens_details` bug is real, long-lived, and version-dependent.

**P0/P1 — SGLang.** Same dialect, so it is nearly free once vLLM works;
RadixAttention is on by default and `sglang_cache_hit_rate` plus the `hicache_*`
gauges make it the *best* KV-stats story on the list. Promote to P0 if the first
deployment target uses it.

**P1 — Ollama** (the developer-laptop loop: OpenAI-compatible, no per-request KV
stats, and honestly labelled as such) and **NVIDIA NIM** (OpenAI-compatible,
enterprise GPU deployments, Triton-shaped metrics).

**P2 — llama.cpp server** (works via the same dialect; `--metrics` for
Prometheus) and **Triton** directly.

**Not targeted — TGI.** Hugging Face put it in maintenance mode in December 2025
and archived the repository in March 2026, recommending vLLM or SGLang. Existing
deployments keep working and the `openai-chat` dialect will reach them
incidentally; we should not spend a validation slice on it.

**Weights to validate against**, in order: **GLM-5.3** (MIT, the strongest
permissively-licensed option), **DeepSeek V4 Flash** (cost-per-capability, MIT,
1M context), **Qwen3-Coder-Next** (Apache 2.0, 80B MoE that fits one node and
the best-reported tool-call formatting consistency), and **gpt-oss-20b** as the
small-tier smoke because it is also on Vertex MaaS, which makes it the one model
that exercises a managed and a self-hosted profile with the same id.

One warning that belongs in the plan rather than the retro: independent agentic
testing of open-weight models is materially harsher than vendor benchmarks, and
tool-calling conformance is where they diverge. R2/R8 are not ceremony for this
group — they are the whole risk.

---

## 6. Sequencing

Six slices. Each names its exit criterion; none is "the code compiles".

| Slice | Work | Exit criterion |
|---|---|---|
| **M0** | Usage detail sidecar ([§4.3](#43-usage-one-normalized-record-carried-beside-the-genai-one)) + the meter reading it. **Before any new provider**, retrofitted onto Anthropic | The cache-write undercount is gone: a cache-warming Claude turn prices within a cent of Anthropic's own console figure, pinned by a fixture test. Zero-vs-unreported is distinguishable in `/usage` |
| **M1** | Provider profiles ([§4.2](#42-a-model-is-named-by-profile-model-id)): registry, config surface, `--provider` opens up, `providerFamily` and `BuildModel` resolve through it | The four shipped backends run unchanged through profiles, with the prefix path kept only as a compat fallback. A bogus profile fails at construction naming the profile |
| **M2** | Pricing by (profile, model) ([§4.5](#45-pricing-and-the-self-hosted-cost-fiction) 1–2) | **Closes [#178](https://github.com/go-steer/mast/issues/178)**: Claude-on-Vertex prices off the Vertex table. Generator emits profile-qualified keys; cross-table invariant tests extended to every profile with a tier map |
| **M3** | `pkg/providers/openai` — `openai-chat` first, `openai-responses` wrapping ADK's | A recorded-turn fixture drives a full tool loop offline in the U/E tiers for both dialects, and the E-tier differentiator evals (exactly-once, refusal, rejection, budget) pass against it. Then a J-tier live run against OpenAI + Vertex MaaS + xAI, with tool-calling metrics from [#168–#172](https://github.com/go-steer/mast/issues/172) at parity with the Claude baseline — **that parity is the gate on calling any of them supported**. `J-cost-tier` needs the per-profile identity rule of [§3](#3-what-the-code-assumes-today)'s tenth seam before a MaaS id can pass it |
| **M4** | Self-hosted: capability declaration, `metrics_url` scrape, unpriced-by-default, `cached_tokens` reliability flag | A vLLM profile runs the triage bundle end to end; `/usage` shows tokens per turn, `$—` for cost, and a session-scoped prefix-cache-hit-rate labelled fleet-level |
| **M5** | Claude on Bedrock; the built-in long-tail profiles; Azure | Each ships with a tier map, catalog rows, and a live smoke, or it ships as a documented profile template with no support claim |

M0 through M2 are mast-internal and land regardless of which provider comes
first; they are also where the existing bugs get closed, which is the argument
for doing them first rather than bolting an OpenAI adapter onto the current
seams and inheriting three copies of #178.

**Testing** follows [`./v0.3-plan.md`](./v0.3-plan.md) §2's tiers, with one
addition worth naming: a **per-dialect conformance corpus** — recorded turns
covering a tool call, a parallel tool call, a reasoning round-trip, a refusal, a
max-tokens stop, and a cache hit — replayed offline in the U tier for every
dialect. The precedent is `pkg/attach/testdata/conformance`, and the reason is
that the failure mode we have actually shipped (an empty tool schema) is
invisible to a smoke test that only checks the model answered.

---

## 7. Risks

- **Tool-calling correctness, not transport, is the risk.** The empty-schema P0
  passed two judged nightlies. Every provider gates on #168–#172's measurement,
  and a provider that transports fine but calls tools worse is not supported.
- **Reasoning round-trip differs per dialect and fails loudly only sometimes.**
  Claude requires thinking blocks echoed with their signature or the second
  request of every tool loop 400s — the reason
  `pkg/providers/anthropic/stream.go:48` round-trips them through
  `genai.Part.ThoughtSignature`, and the reason redacted thinking gets a marker
  prefix rather than being dropped. OpenAI's Responses API
  preserves reasoning via `previous_response_id` or
  `include=["reasoning.encrypted_content"]`. A Chat Completions dialect drops
  reasoning between turns *silently* — no error, just a worse agent. The
  `reasoning_echo` capability flag exists to make that a declared property.
- **Table rot scales linearly with providers.** `modeltier.Classify`,
  `taskclass.ModelForTier` and the pricing catalog move together today only
  because `TestBuiltinModelsKnownToCompanionTables` fails the build. Extend that
  invariant to profile-declared tiers in M1, or the tables quietly stop
  describing reality by the third provider.
- **A wrong price is worse than no price.** Every one of the new backends has a
  price that differs from the first-party table for the same model id. #178 is
  the existing proof; M2 is the structural fix; anything that ships before M2
  ships the bug again.
- **Dependency surface.** `openai-go` drags Azure and AWS SDK module
  requirements into `go.sum` even when only the core client is imported.

---

## 8. Open questions

- **OQ-1 — where profiles live.** New `.agents/providers.yaml`, or a
  `providers:` section in `.agents/config.json`?
  [`./config-layout-design.md`](./config-layout-design.md) owns discovery order
  and precedence; this doc should not invent a second convention.
- **OQ-2 — do profile-declared tiers outrank the Go tables, or only extend
  them?** Extending is safer (zero-config behavior can't be broken by a config
  file); outranking is what an operator standardizing a fleet on one vendor
  actually wants.
- **OQ-3 — how much long-context/regional price modelling is worth building.**
  A `LongContext` multiplier covers OpenAI's 272K cliff; data-residency uplift,
  batch, fast-mode and provisioned throughput are each another field. Where is
  the line between "priced" and "priced approximately, and says so"?
- **OQ-4 — does the `metrics_url` scrape belong in mast at all**, or is
  fleet-level KV visibility the operator's Prometheus problem and mast's job
  merely to emit a correlatable session id? The counter-argument is that a
  library embed has no Prometheus.
- **OQ-5 — validation depth for P1 long-tail profiles.** Shipping a profile
  template with no live smoke is useful and is also a support claim we haven't
  earned. Is "documented template, explicitly unvalidated" a category we are
  willing to ship?
- **OQ-6 — which self-hosted deployment is the first real target**, since that
  decides vLLM vs SGLang ordering and which weights get the conformance corpus.

## 9. core-agent

core-agent has most of the same seams (mast's are ports of them) and a registry
mast deliberately dropped. Nothing here should land there by cherry-pick: per
[`./fork-design.md`](./fork-design.md)'s sync discipline the question is whether
multi-dialect provider support is *shared infrastructure* or lean-fork-specific,
and the 2026-08-17 watchdog call is the precedent for how that gets decided —
a feature both repos want is shared by definition. Worth answering before M3, not
before M0.

---

## Resolved decisions

*(Empty. Everything in this doc is a proposal until it appears here and in the
cross-reference table in [`./README.md`](./README.md).)*

---

## Sources

Landscape claims above were checked on 2026-08-18 rather than recalled:

- [Vertex AI partner models for MaaS](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/partner-models/use-partner-models) — the OpenAI-compatible Chat Completions endpoint shape, OAuth-only auth, `-maas` ids
- [xAI Grok models on Google Cloud](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/partner-models/grok) and [xAI docs](https://docs.x.ai/developers/models) — Grok on Vertex; both OpenAI dialects; usage fields
- [OpenAI API pricing](https://developers.openai.com/api/docs/pricing) and [GPT-5.6 coverage](https://www.aipricing.guru/openai-pricing/) — the Sol/Terra/Luna lineup, cached-read 10%, cache-write 1.25×, the 272K long-context multiplier
- [vLLM `prompt_tokens_details` bug #44961](https://github.com/vllm-project/vllm/issues/44961) and [vLLM prefix caching design](https://docs.vllm.ai/en/stable/design/prefix_caching/) — why per-request cached tokens are unreliable and Prometheus is not
- [SGLang Prometheus metrics](https://sgl-project-sglang-93.mintlify.app/observability/metrics) and [an inference-monitoring survey](https://www.glukhov.org/observability/monitoring-llm-inference-prometheus-grafana/) — the per-server KV-stats table, and TGI's maintenance/archive status
- [LiteLLM vLLM `cached_tokens` issue #22984](https://github.com/BerriAI/litellm/issues/22984) — the normalizing-proxy data-loss illustration
- Open-weight landscape (Kimi K3, GLM-5.2/5.3, DeepSeek V4, Qwen3.8-Max; no Llama 5): [BenchLM open-source leaderboard](https://benchlm.ai/best/open-source), [Morph](https://www.morphllm.com/best-open-source-coding-model-2026)
