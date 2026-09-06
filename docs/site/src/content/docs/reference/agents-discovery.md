---
title: ".agents/ discovery"
description: How mast finds its config root — exclusive single-location discovery, and what gets scanned inside it.
sidebar:
  order: 2
---

mast loads workloads, specialists, and A2A registrations from a single
`.agents/` config root. Discovery is **exclusive**: exactly one location is
used per invocation, first match wins, and there is **no cross-location
merging**.

## Discovery order

1. `$MAST_CONFIG_DIR` — if set, it is the canonical location and nothing
   else is consulted. If it doesn't exist, that's a **fatal error** (an
   explicit override never silently falls through).
2. `./.agents` in the process working directory.
3. `<user config dir>/mast/agents` — XDG-compliant on Linux, i.e.
   `~/.config/mast/agents`.
4. `/etc/mast/agents` (system-level).

Because selection is exclusive, an existing-but-empty higher-priority
location **shadows** a populated lower-priority one. That's by design
(deterministic, no merge-order bugs) but is a known operator footgun, so
startup logs loudly: which root was selected, why, what was found in it,
and which existing lower-priority locations it shadows.

## What's scanned inside the root

Per-directory scans are flat and non-recursive — nested subdirectories are
ignored, so you can keep e.g. `workloads/archive/`:

| Path | Contents |
|---|---|
| `<root>/workloads/*.yaml` (also `*.yml`) | [Workload bundles](/reference/workload-bundle/) |
| `<root>/specialists/*.tmpl` | Specialist templates (YAML frontmatter + instruction body) |
| `<root>/a2a/*.yaml` (also `*.yml`) | Static A2A agent registrations |

A missing subdirectory yields zero entries — not an error. Two files
defining the same name in the same directory is a **fatal** load-time
error, as is any validation failure: mast refuses to start on invalid
config. There is **no hot-reload**; config changes require a restart, and
the daemon says which configuration it is running so you can tell whether
one took effect — see [Changing a bundle under a running
daemon](/reference/workload-bundle/#changing-a-bundle-under-a-running-daemon).

## Name mode vs. path mode

`--workload=<value>` resolves two ways:

- **Name mode** — anything that isn't an existing directory is a workload
  name resolved via the discovery rules above. Only that bundle's roster of
  specialists is built (the root's `specialists/` dir may serve many
  workloads).
- **Path mode** — an existing directory is loaded directly as
  `<dir>/workload.yaml` + `<dir>/specialists/`. This is what the offline
  quickstart uses with `examples/workloads/gke-triage`.
