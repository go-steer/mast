---
title: Why mast
description: The thesis — unattended, library-embedded, multi-provider, durable — plus an honest account of the workloads mast is the wrong tool for.
---

If you're building an agent in Go, your starting point is
[ADK v2](https://google.golang.org/adk/v2): a model interface, a
tool-calling loop, sessions, workflow graphs. That is a real substrate, and
for a single agent loop with a human watching it, it is enough.

mast is what you need when the loop stops having a human watching it.

## The thesis

**mast is agent infrastructure for unattended, library-embedded,
multi-provider, durable workloads.** Four pillars, each of which is a
requirement someone hit in production rather than a feature someone wanted:

1. **Unattended.** The agent runs in a Cloud Run pod, a Kubernetes
   operator, a scheduled job, a webhook consumer. Nobody is going to notice
   the runaway loop, catch the bad remediation, or answer the clarifying
   question. So ceilings, gates, and audit are not add-ons — they are the
   difference between a demo and something you would point at a cluster.
2. **Library-embedded.** mast composes as a Go library inside a larger
   service, not only as a standalone binary. Both shapes are first-class
   and get the same subsystems; only the config-injection surface differs.
   The [slim-embed guarantee](/quickstart/library-embed/) is CI-enforced:
   you pay for what you import.
3. **Multi-provider.** The same config switches between Gemini and Claude
   without code changes, [down to a single specialist](/concepts/providers/).
   Model quality moves month to month, and being pinned to one vendor's
   curve is a risk a platform team should not have to carry.
4. **Durable.** Sessions survive process restarts, pod rescheduling, and
   `kill -9`. A paused approval outlives the process that asked the
   question. *Unattended without durable is unwatched but fragile* — which
   is why durability is a pillar and not a storage detail.

## Where mast fits

Two honest routings — the sooner you get one, the better:

| If you are… | Use | Why |
|---|---|---|
| Someone who needs one simple agent loop in Go | **raw ADK v2** | Smallest surface, no governance, no durability. That niche is ADK's, and wedging a thin mast-branded layer in front of it would shrink every time ADK improves. |
| A platform or SRE team putting agents where no human is watching | **mast** | Keep reading. |

Come back from ADK when the loop must survive restarts, needs budget or
permission governance, needs provider switching, or stops having a human
watching it. Adding those is an import, not a migration — you start inside
mast either way.

The scope decision behind that: mast invests in the deployed shape —
durability, governance, operator surfaces, provider portability — and not
in interactive editing UX. There is no LSP integration, no AST tooling, no
syntax-aware diff UI, and the file-editing tools exist to serve unattended
workloads rather than an editor. That is a boundary chosen once, not a
backlog item.

## Where mast wins

- An agent runs inside a Cloud Run pod, a Kubernetes operator, or a
  scheduled job, mostly unattended.
- An agent is a Go library compiled into a larger service.
- The same config has to switch between Gemini and Claude.
- Multi-tenant auth, audit logs, cost ceilings, and permission gates are
  first-class requirements rather than someday-items.
- The workload is platform- or SRE-shaped: incident triage, deployment
  automation, drift detection, runbook automation.

## Where it does not

- The agent's home is a developer's editor rather than your infrastructure.
- The work is open-ended exploratory bug hunting in unfamiliar codebases.
- LSP-style symbol and AST awareness is core to the workflow.
- One process, one loop, one human watching — no durability or governance
  needed.

## The sibling, not the successor

mast is a lean fork of
[core-agent](https://github.com/go-steer/core-agent), and both are
maintained. They have different jobs: core-agent is the broader
experimentation and integration substrate, with an interactive TUI and a
wider surface; mast is the narrow, unattended, deploy-shaped one. Neither
replaces the other, and a decision that assumes "mast is the future
core-agent" is starting from a false premise.

The full argument, including the kept/cut/change-shape decisions this page
summarizes, is in
[`docs/positioning.md`](https://github.com/go-steer/mast/blob/main/docs/positioning.md).

## Next

- [Concepts](/concepts/) — how the pieces fit together.
- [Quickstart: unattended triage](/quickstart/unattended-triage/) — the
  whole loop offline, no credentials, in about five minutes.
- [Roadmap](/roadmap/) — what is shipped, and what is honestly not.
