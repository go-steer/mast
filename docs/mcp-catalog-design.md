# mast MCP catalog: design + position

**Status:** draft, 2026-07-01. Companion to [`./positioning.md`](./positioning.md) (open question #2 — build MCP servers or consume them — resolves here), [`./orchestration-design.md`](./orchestration-design.md) (workload bundles reference MCP servers by name in `tool_catalog.mcp[]`), [`./specialists-design.md`](./specialists-design.md) (specialists enumerate per-MCP-server tool allowlists), [`./deployment-design.md`](./deployment-design.md) (MCP server placement in cluster vs. external), and [`./library-api-design.md`](./library-api-design.md) (programmatic MCP server registration). Short doc — resolves a position question that other docs reference and lists the servers mast validates against, without duplicating protocol design (that lives with MCP itself).

## The position question

Positioning.md open question #2: *"MCP server catalog: build or consume? Should core-agent ship its own MCP servers (Prometheus, Cilium, Istio, GCP IAM/Logging) or stay a substrate that consumes others'? Probably the latter, but gke-parallel-triage shows there's value in shipping the wiring config even when servers are external."*

**Resolved:** consume, don't build. With one deliberate exception: **wiring config + starter deployments** for the MCP servers mast's audience most commonly uses.

## Sibling protocol: MCP vs. A2A

MCP and A2A are complementary, not competing. Concise distinction:

- **MCP is for tools** — structured function calls with defined input/output schemas. Synchronous request/response typical. Server-side is stateless per call (though may have state internally). Think: "call `get_k8s_resource(name=X)`, get back a K8s resource object."
- **A2A is for agents** — task-based interactions with negotiable state, streaming, HITL support, long-running task lifecycle. Server-side maintains task state and can push updates or request input. Think: "submit a task 'investigate incident-X', receive updates as investigation progresses, respond to input requests, get final structured verdict."

A single mast agent can and typically will use both: MCP for tool calls within its own reasoning + A2A to invoke *other agents* (via [`./federation-design.md`](./federation-design.md)'s `invoke_remote_agent` with `a2a://...` reference). See [`./a2a-design.md`](./a2a-design.md) for A2A specifics.

The mast MCP catalog and the A2A ecosystem are separate curation surfaces — the MCP catalog names MCP servers; A2A registries (Google Agent Registry, kagent) name A2A agents.

## Why "consume, don't build"

- **MCP-server-building is a distraction from mast's positioning.** Positioning is agent-infrastructure substrate; building yet-another-Prometheus-MCP-server competes with an existing ecosystem, drains engineering hours from the core loop / durability / observability / orchestration work that differentiates mast, and produces something the mast team is not domain-expert on maintaining.
- **The MCP ecosystem is real and growing.** For every MCP server mast operators want (GKE, Prometheus, GitHub, Cloud Logging, Slack, PagerDuty, Terraform Cloud, Vault, etc.), a first- or third-party server exists or is being built by someone closer to the domain.
- **Building creates lock-in temptation.** Mast-built MCP servers would tempt us to add mast-specific extensions ("richer" than plain MCP); operators would depend on those extensions; portability would erode. Consuming stock MCP servers keeps the boundary clean.
- **The (E) sibling-products framing applies.** Core-agent's audience might want first-party MCP servers as part of the experimentation substrate; mast's audience wants a substrate that composes with what they already run.

**The exception — wiring config.** GKE-parallel-triage taught us that operators don't want to figure out from scratch how to configure the GKE MCP server against their cluster, wire credentials, set the tool allowlist for a triage workload, and compose it with other servers. Shipping *ready-to-adapt wiring config* for the common MCP servers is high-value and low-cost — it's a few YAML files per server, not a codebase to maintain.

## What mast ships

Not MCP server implementations. What mast ships in the MCP catalog:

1. **Wiring config templates** for each cataloged server: `.agents/mcp/<server-name>.example.json` (a copy operators start from). Includes tool allowlist, credential wiring pattern, standard config knobs.
2. **Reference workload bundles** that use each cataloged server: `.agents/workloads/*.example.yaml` — e.g., an `incident-triage.example.yaml` that references the `gke` and `prometheus` servers as configured by their wiring templates.
3. **Deployment starters** for common MCP-server-plus-mast topologies: `examples/deploy/gke-with-prometheus-mcp/` — Helm/Kustomize manifest showing mast + Prometheus MCP server as sidecars in a GKE cluster.
4. **A catalog page** in the mast docs site listing the cataloged servers, their upstream source, their config template location, and a brief "when to use this" description.

## Cataloged servers (v0.1 → v0.3)

Cataloging criteria: the server must be (a) load-bearing for mast's audience (unattended platform/SRE workloads), (b) sufficiently stable to point operators at (has a release cadence, has a maintainer), and (c) reachable via standard MCP transport (not a bespoke integration).

### v0.1 catalog (essential for launch)

| Server | Upstream | Rationale | Wiring template |
|---|---|---|---|
| **`gke`** | community / Google-team; e.g. [mastersingh24/gke-agent](https://github.com/mastersingh24/gke-agent)-adjacent | GKE-parallel-triage is the reference example; Kubernetes-shape workloads are core audience | `.agents/mcp/gke.example.json` |
| **`prometheus`** | community; several exist | Alert-driven workloads (incident triage) universally need this | `.agents/mcp/prometheus.example.json` |
| **`github`** | first-party GitHub or well-maintained community | PR review, release management, cross-repo automation | `.agents/mcp/github.example.json` |
| **`cloud-logging`** | Google-team or community | Log-search-shaped workloads | `.agents/mcp/cloud-logging.example.json` |
| **`slack`** | community | HITL escalation to on-call, incident notifications | `.agents/mcp/slack.example.json` |

### v0.2 catalog (expanding after v0.1 operator feedback)

| Server | Rationale |
|---|---|
| **`pagerduty`** | Incident-response integration; on-call routing |
| **`terraform-cloud`** | Infrastructure-as-code review + apply workflows |
| **`vault`** | Secret retrieval for MCP servers that need credential access |
| **`aws-cloudwatch`** | Non-GCP operators; AWS-shaped audiences |
| **`aws-eks`** | Kubernetes-on-AWS |
| **`argocd`** | GitOps deployment workflows |

### v0.3+ catalog (based on operator demand signals)

Cataloging expands when: (a) multiple operators independently ask for the same server, (b) the server is available with sufficient stability, (c) the wiring template + reference workload can be maintained without becoming a chore.

Not committed to a specific list; the catalog grows as adoption grows.

## Server placement decisions

Different servers have different placement patterns; the catalog captures the recommendation per server.

| Placement | When to use | Servers |
|---|---|---|
| **In-cluster sidecar** | Server needs cluster-network access; short-lived tool calls | `gke` (in GKE deployments); `prometheus` (in-cluster) |
| **In-cluster DaemonSet** | Server needs per-node access; DaemonSet lifecycle | `cilium`, `istio` (v0.3+) |
| **In-cluster Deployment (dedicated)** | Server needs its own scaling / resources | `github` for orgs with high MCP call volume |
| **External SaaS** | Server is managed externally by vendor | `pagerduty`, `slack`, `github` (for orgs preferring SaaS-managed) |
| **Sidecar to mast** | Server is bespoke to a single mast workload | Rare; usually a signal the workload should just use a library instead |
| **Sidecar to another workload** | Server is used by both mast and something else | `prometheus-mcp` sidecar to the Prometheus server itself, exposed to mast via ClusterIP |

Per-server placement recommendation lives in the catalog page.

## Wiring template shape

`.agents/mcp/*.example.json` follows the existing MCP client config pattern (per core-agent's port to mast) with placeholder syntax operators substitute:

```jsonc
// .agents/mcp/gke.example.json
{
  "name": "gke",
  "transport": "stdio",
  "command": "/usr/local/bin/gke-mcp-server",
  "args": ["--project=${GCP_PROJECT}", "--cluster=${GKE_CLUSTER}"],
  "env": {
    "GOOGLE_APPLICATION_CREDENTIALS": "${GOOGLE_APPLICATION_CREDENTIALS}"
  },
  "allowlist": {
    // Tools most workloads will need; operator narrows for specific workloads
    // via workload bundle tool_catalog.mcp[].tools
    "default": [
      "get_k8s_resource",
      "describe_k8s_resource",
      "get_k8s_logs",
      "list_k8s_events",
      "list_namespaces",
      "list_pods",
      "list_deployments"
    ]
  },
  "credentials": {
    "resolver": "workload-identity",
    "notes": "Assumes GKE Workload Identity; for GKE Autopilot, ..."
  }
}
```

Each template also has a `.md` sibling explaining:
- What the server does
- What credentials it needs and how to wire them
- Common tool allowlist narrowings by workload class
- Known limitations / version compatibility notes
- Link upstream to the server's own docs

## Composition with mast subsystems

| Subsystem | MCP catalog interaction |
|---|---|
| **Orchestration (workload bundles)** | Bundles reference cataloged servers by name in `tool_catalog.mcp[].server`; per-workload tool narrowings compose with the server's default allowlist. |
| **Specialists** | Specialist `tools.mcp[]` allowlists reference cataloged servers; specialist descriptions reference the tools available in each. |
| **Library API** | Programmatic MCP server registration bypasses the catalog for library-embedded consumers; catalog templates remain useful as documentation. |
| **Deployment** | Deployment starters include the MCP servers relevant to the shape; e.g., `examples/deploy/gke-with-prometheus-mcp/` includes both mast and Prometheus MCP server as coordinated deployments. |
| **MCP credential resolution** | Positioning priority #6 — per-bundle credential resolution — composes with the catalog's `credentials.resolver` field. Cataloged servers include the recommended resolver per typical deployment. |
| **Observability** | Cataloged servers have known metric-emission patterns; observability starter dashboards include per-server panels. |
| **Permission gate** | Per-server tool allowlist narrowings in bundles + specialists compose with the mast-side permission gate; MCP calls that don't pass the gate never reach the server. |

## Cataloging discipline

- **New catalog entries require:** (a) an operator use case, (b) a maintained upstream, (c) wiring template + reference workload, (d) tested in `examples/`. Not a low bar; the catalog stays curated.
- **Catalog entries get retired when:** (a) upstream is abandoned or (b) operator demand disappears. Retirement includes a deprecation cycle in the catalog docs (same shape as library API deprecations per [`./library-api-design.md`](./library-api-design.md)).
- **Third-party servers not in the catalog** are still supported — they're just not documented by us. Operators wire them via bundle config directly.

## Community MCP server contributions

Third parties wanting to add wiring templates for their MCP servers:

- Open a PR against `mast` with the wiring template + reference workload + docs page.
- Cataloging PR review is a docs+config review, not a code review.
- Once cataloged, the template lives in mast's repo; upstream changes to the MCP server surface may require template updates (community-maintained).

Not the same as merging arbitrary MCP servers — the mast catalog is a *curation* of what we validate against, not an ecosystem hub.

## Phasing

| Version | Scope |
|---|---|
| **v0.1** | v0.1 catalog (gke, prometheus, github, cloud-logging, slack). Wiring templates + reference workloads. Catalog page in docs site. |
| **v0.2** | v0.2 catalog expansion (pagerduty, terraform-cloud, vault, aws-cloudwatch, aws-eks, argocd). Deployment starters for GKE-plus-MCP-sidecar shapes. |
| **v0.3+** | Operator-demand-driven catalog growth. Cross-cataloged-server reference workloads (e.g., "PagerDuty alert → GKE triage → Slack notify"). |

## Open questions

1. **Cataloging exclusivity.** If two competing MCP servers exist for the same target (two `gke-mcp-server` implementations), do we catalog both, pick one, or catalog neither? Bias: pick one, based on maintainer activity + operator feedback; document the alternative in the catalog page's notes.
2. **Version-pinning cataloged servers.** Wiring templates could pin to a specific MCP server version — safer against breakage, riskier against staleness. Bias: pin at minor version (`>=1.2,<1.3`); operator overrides for their environment.
3. **In-tree vs. external catalog docs.** Wiring templates + reference workloads live in `mast` repo; is the catalog page in the mast Hugo site or in a separate `mast-catalog` docs site? Bias: mast Hugo site — the catalog is core to the mast experience.
4. **Credential-injection helper.** Do we ship a small helper for the common credential-resolution patterns (Workload Identity, IAM Roles for Service Accounts, Vault agent)? Bias: yes as `pkg/mcp/credentials/` in v0.2; small surface, big usability win.
5. **Test coverage for wiring templates.** How do we validate that a template works? Bias: `examples/mcp/<server-name>/smoke-test.sh` per server; runs in CI against a stub MCP server that returns canned responses; not a full integration test.

## Out of scope

- **Building MCP server implementations.** Explicit non-goal.
- **A mast-managed MCP server registry service.** No auth-gated central registry; catalog is docs + templates in the mast repo.
- **Payment / marketplace mechanics.** If operators want commercial MCP servers, they procure them; we don't broker.
- **MCP protocol contributions.** Protocol changes go upstream to MCP itself; mast follows.
- **A mast-specific MCP dialect.** Standard MCP only.

## Related

- [`./positioning.md`](./positioning.md) — open Q #2 resolves here
- [`./orchestration-design.md`](./orchestration-design.md) — bundles reference cataloged servers
- [`./specialists-design.md`](./specialists-design.md) — specialists reference server + tool allowlists
- [`./deployment-design.md`](./deployment-design.md) — server placement patterns
- [`./library-api-design.md`](./library-api-design.md) — programmatic MCP registration
- [`./observability-design.md`](./observability-design.md) — per-server observability patterns
- Model Context Protocol spec — the underlying protocol
- [mastersingh24/gke-agent](https://github.com/mastersingh24/gke-agent) — the original GKE-shaped proof
