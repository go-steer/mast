# mast skills: design

**Status:** draft, 2026-07-01. Companion to [`./positioning.md`](./positioning.md) (skills reversed from cut list to keep list — see resolved-decisions), [`./specialists-design.md`](./specialists-design.md) (specialists and skills coexist as complementary authoring models, not replacements), [`./orchestration-design.md`](./orchestration-design.md) (workload bundles enumerate a skills roster; planner tool vocabulary gains `invoke_skill`), [`./fork-design.md`](./fork-design.md) (bucket 2 ports the loader from core-agent), [`./mcp-catalog-design.md`](./mcp-catalog-design.md) (three-way curated-surface comparison: MCP tools / A2A agents / skill templates), [`./a2a-design.md`](./a2a-design.md) (skill discovery via A2A registries), [`./library-api-design.md`](./library-api-design.md) (`mast/skill` package), and [`./config-layout-design.md`](./config-layout-design.md) (`.agents/skills/` layout). Covers **SKILL.md format support as a first-class consumable subsystem** — mast loads Anthropic-SKILL.md-format bundles, registers each as a callable tool, and integrates with published skill catalogs.

## Why this reverses the earlier cut

The original decision (see [`./specialists-design.md`](./specialists-design.md) history) cut skills from mast's scope with this reasoning: *"Skills load Anthropic-SKILL-formatted bundles so users can drop existing Claude Code skills into a project. That's a real value-add for core-agent's audience (developers experimenting locally, embedded consumers). It's not mast's audience — mast operators write GKE runbooks, not Anthropic-compat skill bundles."*

That reasoning inverted in 2026 as **the GKE team and broader Google teams began publishing skills as first-class artifacts for platform-team work**. When mast's audience (platform teams) is exactly the audience the skill publishers are targeting, cutting skills would mean mast operators can't consume the ecosystem their own vendor is publishing into. The cost of skill support (bucket-2 port + composition wiring) is low; the cost of *not* supporting it is invisibility to a fast-growing ecosystem.

The reversal is **additive, not a specialists rollback**. Specialists and skills solve different problems:

- **Specialists** = mast-authored subagents. Full control over budget, HITL policy, agent mode, tool allowlist, model choice. Written for a specific deployment; live in the operator's config repo.
- **Skills** = declarative task templates in the Anthropic-SKILL.md format. Consumed from publishers (GKE team, community, internal teams, your own). Format-portable across the ecosystem (Claude Code, other agent frameworks, mast).

Both surface to the planner as callable tools; the choice is *authoring model* (write your own vs. consume a published template), not *consumer scope*. Both coexist in a workload bundle's rosters. Neither is deprecated in favor of the other.

## SKILL.md format support

Mast loads bundles following the Anthropic-SKILL.md convention. Each skill is a directory containing at minimum a `SKILL.md` file with YAML frontmatter + markdown body, plus optional auxiliary files the skill references.

### Bundle layout

```
gke-triage.skill/                # directory (or single-file variant if the spec allows)
  SKILL.md                       # required — the skill definition
  resources/                     # optional — reference material the skill body cites
    common-errors.md
    remediation-templates/
  examples/                      # optional — usage examples
    example-1.md
```

### SKILL.md file

```markdown
---
name: gke-triage
description: |
  Triage a Kubernetes pod failure. Given a pod reference and symptom
  observation, identify root cause and propose remediation.
version: 1.2.0
license: Apache-2.0
publisher: gke-team@google.com
allowed_tools:                    # skill-declared tool needs; mast intersects with bundle allowlist
  - read_file
  - grep
  - mcp:gke:*
  - mcp:prometheus:*
model_hint: gemini-2.5-pro        # advisory; mast may substitute
input_schema:
  type: object
  properties:
    pod: {type: string, description: "namespace/pod format"}
    symptom: {type: string}
  required: [pod, symptom]
output_schema:
  type: object
  properties:
    root_cause: {type: string}
    remediation: {type: string}
    confidence: {type: number}
---

# GKE Triage

You are diagnosing a failed Kubernetes pod.

## Approach

1. Fetch the pod's current state and recent events.
2. If the pod has containers with recent restart counts, examine logs.
3. Correlate against Prometheus alerts if the symptom matches a known pattern.
4. Return the structured `{root_cause, remediation, confidence}` payload.

## Common patterns

...
```

### Field reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | Skill identifier; must be unique within the loaded set |
| `description` | string | yes | Human-readable; used by the planner for tool selection |
| `version` | semver | yes | Skill version; drives update / drift detection |
| `license` | string | recommended | Skill license (Apache-2.0, MIT, etc.); surfaced for audit |
| `publisher` | string | recommended | Identifier for the publisher (email, org, or opaque handle) |
| `allowed_tools` | []string | no | Skill's tool needs; mast intersects with bundle allowlist (see policy layering below) |
| `model_hint` | string | no | Preferred model; advisory only — mast may substitute |
| `input_schema` | JSON Schema | recommended | Structured input shape for the skill |
| `output_schema` | JSON Schema | recommended | Structured output shape for the skill |
| Body (markdown) | — | yes | The skill's system prompt / instruction |

Additional publisher-specific fields (extensions like signatures, provenance attestations, cost hints, HITL escalation policies) are preserved on load and available for consumers via the library API but not interpreted by mast v0.1.

## File layout + discovery

Consistent with the other subsystems ([`./config-layout-design.md`](./config-layout-design.md)):

```
.agents/
  skills/                        # skill bundles
    gke-triage.skill/
    k8s-upgrade.skill/
    ...
```

Discovery order (per config-layout-design):
1. `$MAST_CONFIG_DIR/skills/` (env override)
2. `./.agents/skills/`
3. `~/.config/mast/agents/skills/`
4. `/etc/mast/agents/skills/`

First-match-wins per config-layout precedence rules.

### Registry discovery (v0.2+)

For deployments that consume skills from registries:

```yaml
# .agents/skills/registry.yaml
registries:
  - name: google-agent-registry
    endpoint: https://agentregistry.googleapis.com/v1/skills
    auth:
      type: google-iam
    filters:
      publisher_prefix: [gke-team@, sre-team@]
      required_labels: {tier: production}
    cache_ttl_hours: 24
```

At startup + on `SIGHUP`, mast queries the registry, downloads matching skill bundles, and registers them alongside statically-configured ones. Discovered skills carry a `registry:<name>` prefix in their loaded ID to prevent collision with local skills.

### CLI

```
mast skills list                                # all loaded skills
mast skills show <name>                         # skill detail (frontmatter + body preview)
mast skills sync <registry>                     # refresh discovered skills from a registry
mast skills install <ref>                       # install a specific skill (e.g. google://gke-team/incident-triage@v1.2)
mast skills validate <path>                     # validate a local SKILL.md bundle
mast skills verify <name>                       # verify signature / provenance (v0.3+)
```

## Publisher / consumer split

Skills have two roles that mast supports separately:

### Consumer (v0.1)

Mast operators consume published skills. Primary path:

- Static: drop `SKILL.md` bundles under `.agents/skills/` and reference them in workload bundles.
- Dynamic: point `.agents/skills/registry.yaml` at a catalog (Google Agent Registry, community catalog, corporate internal catalog); mast auto-loads matching skills.

### Publisher (v0.3+)

Mast operators publishing their own skills for consumption by teams / other mast instances / other frameworks:

```
mast skills publish ./my-skill.skill/ --registry=corporate-internal
mast skills publish ./my-skill.skill/ --registry=google --namespace=team-x
```

Publishing produces a signed bundle (v0.3+ once signing infrastructure is in place). Publishing to Google Agent Registry requires the appropriate IAM role; other registries have their own auth.

**Not publishing SKILL.md format upstream contributions from mast** — the format is Anthropic's; format-evolution contributions go there. Mast produces conformant SKILL.md bundles per the current spec version.

## Policy layering

Skills are declarative; deployment policy is layered on top by mast without modifying the skill:

- **Tool allowlist intersection.** Skill declares `allowed_tools`; workload bundle declares `tool_catalog`. The effective tool set for a skill invocation is the *intersection* — skill can't reach tools the bundle doesn't grant; bundle can't grant tools the skill doesn't request. Both are enforced.
- **Budget caps.** Bundle-level `budget.max_cost_usd` applies to skill invocations. Skills may declare hints (`cost_hint`, `duration_hint`) which observability captures for planning; hints are not enforcement.
- **HITL policy inheritance.** Bundle-level `hitl_policy` applies. Skills can request HITL via the same `RequestInputEvent` mechanism as specialists.
- **Model substitution.** Skill's `model_hint` is advisory; bundle's model config takes precedence. Useful for consuming a skill written for Model X while running the deployment on Model Y.
- **Tenancy scope.** `WithIsolationScope(tenantID)` propagates into skill execution; skills see tenant-scoped state via state-bound access.

Policy layering means **operators can consume a skill safely without needing to fork it** for local policy differences. Skill authors ship intent; operators apply enforcement.

## Skill vs. specialist decision guide

| Consideration | Reach for a specialist when… | Reach for a skill when… |
|---|---|---|
| Origin | You're authoring for a specific deployment | You're consuming a published template |
| Format | Mast-native `.tmpl` schema | Cross-framework Anthropic-SKILL.md |
| Complexity | Complex — mode, budget, HITL, tool allowlist, model override, per-invocation overrides | Straightforward — task template with prompt + tool needs |
| Update cadence | Rare (you own it) | Regular (publisher updates; you sync) |
| Provenance | Local git repo | Signed / provenance-attested from publisher |
| Portability | mast-only | Portable to Claude Code / other frameworks |
| Composition | Composes into workflow reference graphs as agent nodes | Composes as callable tools; can also appear as agent nodes if wrapped |

The two aren't mutually exclusive within a workload — most bundles will use both. Specialists for the mast-native custom logic; skills for the shared/published ones.

## Composition with other subsystems

| Subsystem | Skills interaction |
|---|---|
| **Specialists** ([`./specialists-design.md`](./specialists-design.md)) | Coexist as complementary authoring models. Planner sees both in its tool vocabulary. Migration between them is bidirectional (skill can be re-authored as specialist for local policy control; specialist can be published as a skill for cross-team consumption). **Specialists can also invoke skills directly** via their `tools.skills` allowlist — a specialist may decompose its task by delegating pieces to skills (three-way policy layering: `skill.allowed_tools ∩ specialist.tools ∩ bundle.tool_catalog`; budget bounds compose similarly). |
| **Orchestration** ([`./orchestration-design.md`](./orchestration-design.md)) | Workload bundle gains `skills:` roster field alongside `specialists:`. Planner tool vocabulary gains `invoke_skill(name, inputs)`. Bundle-learning can propose skill additions to bundles when patterns match published skill descriptions. |
| **Workflow scaffolding** ([`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md)) | Skills can appear as agent-shaped nodes in reference graphs. Wrapped via a lightweight shim (planner-invocation shape → node-invocation shape). |
| **Durable execution** ([`./durable-execution-design.md`](./durable-execution-design.md)) | Skill invocations pause/resume like specialist invocations. HITL requests from skill body surface via the same `RequestInputEvent` mechanism. |
| **Observability** ([`./observability-design.md`](./observability-design.md)) | Standard span attributes: `mast.skill.name`, `mast.skill.version`, `mast.skill.publisher`. Metric families: `mast_skill_invocations_total{skill, publisher, outcome}`, `mast_skill_invocation_duration_seconds{skill}`. |
| **Memory** ([`./memory-design.md`](./memory-design.md)) | Skill usage patterns feed per-tenant reducers (`skill_usage_by_workload.<workload>` similar to tools/specialists). Bundle-learning proposes skill additions. |
| **Multi-tenant** ([`./deployment-design.md`](./deployment-design.md)) | Per-tenant skill allowlists via bundle `isolation.scope`; cross-tenant skill sharing follows same `federation.cross_tenant.allow` opt-in rules. |
| **MCP catalog** ([`./mcp-catalog-design.md`](./mcp-catalog-design.md)) | Sibling curated surface — MCP for tools, skills for templates, A2A for agents. Different scopes, complementary. |
| **A2A** ([`./a2a-design.md`](./a2a-design.md)) | Skills can be discovered via A2A registries (Google Agent Registry lists both A2A agents + skills). Discovery adapter shared. Mast-hosted skills can also be exposed as A2A skills via workload bundle `a2a.expose` + skill mapping. |
| **Federation** ([`./federation-design.md`](./federation-design.md)) | Skills execute locally by default. Cross-instance skill invocation (a remote mast executing a skill on parent's behalf) is possible via `invoke_remote_agent("mast://.../skill:<name>")` but usually not necessary — skill portability means local execution is preferred. |
| **Attach mode + `mast-web`** | mast-web renders installed skills catalog view (browse, search, inspect); shows per-skill invocation history + observability. |
| **Library API** ([`./library-api-design.md`](./library-api-design.md)) | `github.com/go-steer/mast/skill` package: `skill.Load`, `skill.Register`, `skill.Registry`, and extension point `skill.Source` for custom skill sources (Kubernetes ConfigMaps, external CMS, etc., analogous to `specialist.Source`). |
| **Config layout** ([`./config-layout-design.md`](./config-layout-design.md)) | `.agents/skills/` per canonical layout; discovery order + hot-reload semantics per config-layout rules. |

## Google Agent Registry / community catalog integration

Google Agent Registry (per [`./a2a-design.md`](./a2a-design.md)) is one of the primary skill catalogs mast integrates with. Others follow the same pattern.

### Google Agent Registry

- Registry endpoint: `https://agentregistry.googleapis.com/v1/skills` (verify against actual product docs).
- Auth: Google IAM (Workload Identity in-cluster; user credentials for laptop use).
- Publisher-scoped browsing: `gke-team@google.com`, `sre-team@google.com`, etc.
- Version pinning: `google://gke-team/incident-triage@v1.2` refers to a specific version.
- Update policy: configurable per registry — `always-latest`, `pin-version`, `major-version-pin`.

### Community catalogs

Same registry protocol; different endpoint + auth. Mast is agnostic — a community catalog serving SKILL.md via a REST endpoint compatible with the registry-discovery format works out of the box.

### Corporate internal catalogs

Same pattern; typically fronted by internal auth (mTLS via service mesh, OIDC via corporate IdP, etc.). Auth extension point via [`./library-api-design.md`](./library-api-design.md).

### Discovery composition

A deployment may consume skills from multiple registries simultaneously (Google-published + corporate-internal + local). Precedence on name collision: local `.agents/skills/` > registry order (as declared in `registry.yaml`). Collisions log warnings; explicit rename via `skills.rename` config field breaks ties.

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | SKILL.md format loader (`pkg/skills/` ported from core-agent, adapted for v2). `.agents/skills/` file layout + static discovery. CLI: `mast skills list/show/validate`. Planner `invoke_skill(name, inputs)` tool. Composition with workload bundles (`skills:` roster field). Basic observability (metric families + span attributes). |
| **v0.2** | Registry discovery: Google Agent Registry, community, corporate. `mast skills sync/install`. `mast-web` skills catalog view. Hot-reload on `SIGHUP`. |
| **v0.3** | Skill signing / provenance verification. Publishing (`mast skills publish`). Bundle-learning integration (propose skill additions based on usage patterns). |
| **v0.4+** | Cross-format import (WASM-shaped skills from other ecosystems, if standards emerge). Skill authoring UI in `mast-web`. Skill-marketplace mechanics if a real ecosystem emerges around it. |

## Open questions

1. **SKILL.md format version tracking.** The format is evolving; mast should support a version range. Bias: track the spec version mast tests against per release; document explicitly; version-negotiate on load if the bundle declares a `schema_version` field.
2. **Skill signing / provenance verification.** Not v0.1; but the design should not preclude it. Bias: v0.3 introduces optional cosign-based verification for skill bundles from registries; local `.agents/skills/*` bundles are trusted by convention (they're in the operator's config repo).
3. **Skill-injected instruction vs. specialist system prompt layering.** A workload bundle can enumerate both specialists and skills; how are their contents composed into the resulting agent's context? Bias: separately — each skill is a discrete tool invocation (skill body loaded into the tool's context, not the parent's); specialists are similarly encapsulated. No cross-pollination unless the skill/specialist explicitly reads state via state-bound access.
4. **Skill `allowed_tools` intersection semantics.** Skill declares `mcp:gke:*`; bundle allows `mcp:gke:get_k8s_resource`. Result: skill can call `get_k8s_resource`; nothing else. Empty intersection: skill invocation errors on load with a clear message.
5. **Local override of published skills.** Operator wants to consume `google://gke-team/incident-triage@v1.2` but override the model_hint. Options: (a) fork the skill locally, (b) `.agents/skills/overrides/gke-triage.overrides.yaml` with sparse patches, (c) workload-bundle-level override in the `skills:` roster entry. Bias: (c) for simple field overrides; (a) for behavior changes.
6. **Migration path from skill → specialist.** When operator outgrows a skill, they can re-author as a specialist for finer control. `mast skills convert <name> --to-specialist > .agents/specialists/<name>.tmpl` — feasible but low-priority (v0.3+).
7. **Skill discovery beyond A2A registries.** WASM-based skill marketplaces (WASI-based sandboxed skills); Git-based catalogs (skills served from a repo, discovered via git submodule); IPFS-shaped decentralized catalogs. Not v0.1; capture as future direction.
8. **Skill hot-reload semantics.** When a registry-discovered skill updates upstream, does the running mast pick it up? Bias: no auto-pickup — operators explicitly `mast skills sync` or the scheduled sync fires (v0.2 with `SIGHUP` semantics). Running sessions keep the skill version they started with; new sessions get the newer version.

## Out of scope

- **Skill format evolution beyond Anthropic-SKILL.md.** Format-evolution contributions go to Anthropic's spec; mast consumes.
- **Skill-marketplace payment mechanics.** If operators want paid skills, they procure them; we don't broker.
- **Skill-authoring IDE.** Authors use their editor + `mast skills validate`. UI is v0.4+.
- **Cross-runtime skill execution.** A skill running in Python ADK vs. mast may have subtle behavioral differences (provider quirks, tool-call semantics); we don't guarantee identical behavior — only spec-conformant behavior.
- **Automatic dependency resolution across skills.** Skill A "requires" skill B — not in v0.1. Compose via workload bundle ordering + planner reasoning if needed.
- **Skill unit testing framework.** Skills are prompts + schemas; testing is scenario-based, not unit-based. Operators use golden-trace comparison ([`./orchestration-design.md`](./orchestration-design.md) evaluation section).

## Related

- [`./positioning.md`](./positioning.md) — skills reversal (cut → keep) rationale
- [`./specialists-design.md`](./specialists-design.md) — coexisting authoring model
- [`./orchestration-design.md`](./orchestration-design.md) — bundle roster + planner vocabulary integration
- [`./workflow-scaffolding-design.md`](./workflow-scaffolding-design.md) — skills as agent-shaped nodes in reference graphs
- [`./a2a-design.md`](./a2a-design.md) — skill discovery via Google Agent Registry
- [`./mcp-catalog-design.md`](./mcp-catalog-design.md) — three-way curated-surface comparison
- [`./federation-design.md`](./federation-design.md) — cross-instance skill invocation edge case
- [`./durable-execution-design.md`](./durable-execution-design.md) — skill invocation pause/resume
- [`./observability-design.md`](./observability-design.md) — skill span attributes + metric families
- [`./memory-design.md`](./memory-design.md) — skill-usage patterns feed learning
- [`./library-api-design.md`](./library-api-design.md) — `github.com/go-steer/mast/skill` package
- [`./config-layout-design.md`](./config-layout-design.md) — `.agents/skills/` file layout
- [Anthropic Skill format spec](https://docs.anthropic.com/en/docs/claude-code/skills) — the underlying format (verify current URL)
- Google Agent Registry — one of the primary catalogs mast consumes
