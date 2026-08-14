---
title: Tools and MCP
description: How a tool reaches a specialist — the mcp.json catalog, the workload's tool catalog, the per-specialist allowlist — and the default-deny-unknown mutation predicate everything else is built on.
sidebar:
  order: 5
---

A specialist is only as capable, and only as dangerous, as the tools it can
reach. mast narrows that reach in three passes, and classifies what is left
so the [write gate](/concepts/approvals/) knows what to stop.

## Three narrowings

```
mcp.json                    what this deployment can talk to at all
   ↓
workload.yaml               which of those servers this workload uses
  tool_catalog.mcp[]        + per-tool policy overrides
   ↓
specialists/*.tmpl          which tools this one specialist may call
  tools.mcp[].tools[]
```

**`mcp.json`** is the deployment's server catalog — HTTP and local stdio
transports, credentials, and nothing workload-specific. It is a
control-plane file: a `stdio` entry names a command mast will execute, so
editing it grants code execution. Treat it like `config.json`, and use
`command_allowlist` and `env_mode: "clean"` to bound what an edited catalog
could ever do. A workload referencing a server that is not in the catalog
is a fatal load error — mast refuses to start rather than silently drop a
tool.

**`tool_catalog`** in the bundle names which servers this workload uses,
and carries per-tool policy overrides.

**The per-specialist allowlist** is where the real narrowing happens. Each
specialist names the tools it needs, and the set is intersected against the
catalog at dispatch time. A diagnoser that reads pods and logs gets exactly
that, even if the server publishes forty other verbs.

That last layer is what makes the capability split enforceable: mast can
prove at startup that a `read_only` specialist has no path to a mutating
tool, because the path is enumerated rather than inferred.

## The mutation predicate: unclassified means mutating

Every layer above is a permission question. This is the *classification*
question, and it is the one the write gate stands on.

MCP has an annotation for it — `readOnlyHint` — and the honest situation is
that mast cannot see it: the substrate's MCP toolset drops tool annotations
at conversion, so the hint never survives the trip. That left two options:
infer from the tool's name, or default-deny.

**mast defaults to deny.** Every MCP tool is treated as mutating until the
workload's catalog says otherwise:

```yaml
tool_catalog:
  mcp:
    - server: gke
  tools:
    - name: list_clusters
      mutating: false     # audit-logged at startup
```

Name-based inference was rejected on purpose. `get_` and `list_` look safe
until a server ships `get_recovery_token`, and a predicate that is right
99% of the time is a predicate that fires a mutation unseen on the
hundredth call. A declaration is reviewable in a diff; a heuristic is not.

The cost is real and worth stating: an un-declared read-only tool parks for
an approval it did not strictly need. That is the failure mode to have.

Two other classes exist beside plain `Mutating`. **Spawning** covers tools
that start sub-runs — `invoke_specialist`, the planner vocabulary — whose
inner calls cannot be individually guarded from the spawn site.
`invoke_remote_agent` classifies as plain `Mutating`, because effects on
the far side of a federation call are simply invisible from here.

## The failure mode to know about

**A tool name in an allowlist that the server does not publish disappears
silently.** The allowlist is intersected with what the server actually
offers, so a typo — or a catalog written against a different version of the
server — just narrows the specialist's tools. Nothing errors. The
specialist runs, has less to work with than its prompt assumes, and
produces a thinner answer.

This is not hypothetical: the shipped GKE triage catalog named three tools
the GKE MCP server does not have, and the symptom was diagnoses that were
merely *worse*, which is exactly the kind of bug that survives a demo. When
a specialist's answers seem oddly shallow, check its allowlist against the
server's published tool list before you touch the prompt.

## Builtins

Not every tool comes from MCP. Provider builtins — Gemini's Google Search
and URL context — are wired through the provider rather than the catalog
(see [providers](/concepts/providers/)). Engine control calls
(`finish_task`, `transfer_to_agent`, input requests) are part of the loop
itself, are excluded from the mutation scan, and never park.

## Reference

- [`mcp.json`](/reference/mcp-servers/) — schema, transports, credentials,
  stdio hardening.
- [Workload bundle](/reference/workload-bundle/) — `tool_catalog` and
  per-specialist `tools`.
- [Write gate](/reference/write-gate/) — what happens to a call classified
  mutating.
