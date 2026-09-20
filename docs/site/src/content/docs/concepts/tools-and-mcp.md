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
specialists/*.specialist.md          which tools this one specialist may call
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
question, and it is the one the write gate stands on. Four sources answer
it, and they are consulted in this order — the first one that speaks wins:

1. **Engine control calls** (`finish_task`, `transfer_to_agent`, input
   requests) — part of the loop, never scanned.
2. **The workload's `tool_catalog.tools[].mutating` declaration** — your
   word, audit-logged at startup.
3. **mast's own builtins**, whose implementation is in this repo.
4. **The MCP server's `readOnlyHint` annotation** — the server's word.

If none of them speaks, the tool is **mutating**. That default is the whole
posture: an unclassified tool parks for approval rather than running.

### Your declaration outranks the server's

MCP publishes a per-tool `readOnlyHint`, and mast reads it. A tool whose
server declares itself read-only is classified read-only and does not park
under the default [`on_mutation: require_approval`](/reference/write-gate/).
A tool that declares nothing is unchanged: still mutating, still parks.

That is a remote server's self-description being taken at its word, so the
order above matters. A `tool_catalog` declaration is read *first* and wins
in both directions, which is where you go when you do not want to extend
that trust to a particular tool:

```yaml
tool_catalog:
  mcp:
    - server: gke
  tools:
    - name: delete_cluster
      mutating: true      # pinned; no server can unpin it
    - name: list_clusters
      mutating: false     # pinned; needed only if the server is silent
```

mast trusts the hint by default because `mcp.json` is control-plane config
that the [write gate protects](/reference/write-gate/) — wiring a server is
already the grant, and asking you to re-authorize it tool-by-tool would be
asking the same question twice. There is deliberately **no config key to
turn the hint off**; the `tool_catalog` override above is the answer, and it
is the auditable one. What you get instead is a line in the log per server
at startup, counting how many of its tools called themselves read-only,
mutating, and neither — so a server whose annotations you did not expect is
visible without a debug build.

Two edges worth knowing. A tool the server annotates as *not* read-only, and
a tool it does not annotate at all, are indistinguishable on the wire — MCP
defines the hint as a plain boolean whose default is false — so both stay
mutating, which is the same answer mast gave before it read the hint. And
because tool names are not namespaced by server, two servers can publish the
same name; if they disagree about it, the tool is **mutating** and both
servers are named in a warning.

Name-based inference remains rejected. `get_` and `list_` look safe until a
server ships `get_recovery_token`, and a predicate that is right 99% of the
time is one that fires a mutation unseen on the hundredth call. A
declaration — yours or the server's — is a statement someone made; a
heuristic is a guess mast made.

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
and URL context, Anthropic's web search — are wired through the provider
rather than the catalog, and they are the one class of tool none of the
three narrowings above reaches: they run on the vendor's servers and never
come back as a tool call, so there is no name for an allowlist to match and
no call for the gate to hold. mast therefore ships them **off** on every
provider and takes the opt-in from the bundle's
[`builtin_tools:`](/reference/workload-bundle/#builtin_tools--the-providers-own-server-side-tools)
block (see [providers](/concepts/providers/)). Engine control calls
(`finish_task`, `transfer_to_agent`, input requests) are part of the loop
itself, are excluded from the mutation scan, and never park.

A second builtin appears only when it has something to do: `retrieve_raw`,
the escape hatch for the [MCP response
digest](/reference/mcp-servers/#digesting-large-tool-responses). It is
registered on Task-mode specialists when digesting is on and a wired
server is being digested, and it exchanges a digested response's
`call_id` for the original payload. It sits outside the allowlist axes
above on purpose — it returns bytes the specialist has already been
given, so there is no reach for an allowlist to withhold.

It is also classified **read-only**, and you do not have to declare it to
get that. Default-deny-unknown is the right stance for a tool that arrived
from somewhere mast cannot inspect; it is the wrong stance for a tool mast
itself registered, whose implementation is in this repo. `retrieve_raw`
reaches no server and takes no argument but a key mast minted, so there is
nothing to be uncertain about — and before this was fixed, an operator with
an enumerated catalog met an approval question naming a tool they had never
declared, mid-diagnosis, on the first response big enough to digest. A
workload that wants it gated anyway can still say so: a `tool_catalog.tools`
override outranks the builtin class, the way it outranks every other
default.

## Reference

- [`mcp.json`](/reference/mcp-servers/) — schema, transports, credentials,
  stdio hardening, response digesting.
- [Workload bundle](/reference/workload-bundle/) — `tool_catalog` and
  per-specialist `tools`.
- [Write gate](/reference/write-gate/) — what happens to a call classified
  mutating.
