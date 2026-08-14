---
title: "Quickstart: unattended triage (offline)"
description: Run the GKE triage workload end to end with no credentials and no network — inject an incident, pause for approval, kill the process, resume.
---

This walkthrough runs mast's anchor workload — GKE incident triage with 13
specialists — entirely offline on the built-in `echo` model. No credentials,
no network, no cluster. You'll inject an incident, watch the workload pause
for operator approval, kill the daemon, and resume the approval in a fresh
process: the durability pillar, live on your laptop.

You need Go and a clone of the repo (the workload bundle and specialists
ship as files under `examples/`):

```sh
git clone https://github.com/go-steer/mast.git
cd mast
```

## 1. Build and start the daemon

```sh
mkdir -p /tmp/mast-demo
go build -o /tmp/mast-demo/mast ./cmd/mast
```

Start it with the triage workload, graph dispatch, and a SQLite session DB
(that's the durability — omit `--session-db` and sessions are in-memory):

```sh
/tmp/mast-demo/mast \
  --workload=examples/workloads/gke-triage \
  --dispatch=graph \
  --model=echo \
  --listen=:7777 \
  --session-db=/tmp/mast-demo/sessions.db
```

Leave this running and open a second terminal.

## 2. Inject an incident

The workload's edge trigger is HTTP: POST an incident envelope to
`/inject`. Each incident UID gets its own session (`incident-<uid>`).

```sh
curl -s -X POST http://localhost:7777/inject -H 'Content-Type: application/json' -d '{
  "kind":"Pod","reason":"ImagePullBackOff","namespace":"default","name":"web-1",
  "uid":"demo-1","message":"back-off pulling image","cluster":"demo"}'
```

The SingleTurn classifier routes the incident to the `ImagePullBackOff`
specialist. The bundle sets `hitl.require_approval: true`, so the specialist's
proposed action parks on a **durable HITL interrupt** instead of executing.
The daemon log shows `HITL PAUSE` with the interrupt ID.

## 3. Inspect the pause

`mast sessions list` and `show` read the SQLite DB directly — they work with
or without a running daemon:

```sh
/tmp/mast-demo/mast sessions list --session-db=/tmp/mast-demo/sessions.db
/tmp/mast-demo/mast sessions show incident-demo-1 --session-db=/tmp/mast-demo/sessions.db
```

`show` prints the pending interrupt (`approve-ImagePullBackOff` — interrupt
IDs are deterministic per specialist), the approval message, and the exact
resume command.

## 4. Kill the daemon — the pause survives

In the first terminal, kill the daemon as rudely as possible, then restart
the same command:

```sh
kill -9 $(pgrep -f 'mast-demo/mast --workload')
```

```sh
/tmp/mast-demo/mast \
  --workload=examples/workloads/gke-triage \
  --dispatch=graph \
  --model=echo \
  --listen=:7777 \
  --session-db=/tmp/mast-demo/sessions.db
```

The pause was persisted to SQLite; the fresh process picks it up.

## 5. Resume with an operator verdict

Via curl against the daemon's `/resume` endpoint:

```sh
curl -s -X POST http://localhost:7777/resume -H 'Content-Type: application/json' -d '{
  "session_id":"incident-demo-1",
  "interrupt_id":"approve-ImagePullBackOff",
  "response":{"approved":true,"note":"rollback approved by oncall"}}'
```

Or the same thing through the CLI:

```sh
/tmp/mast-demo/mast sessions resume incident-demo-1 \
  --interrupt=approve-ImagePullBackOff \
  --response='{"approved":true,"note":"rollback approved by oncall"}'
```

The session resumes exactly where it paused and runs to completion. To
refuse instead, `mast sessions abort incident-demo-1 --reason="not today"`
writes a durable abort marker; later resumes are refused.

## 6. Look at the metrics

The inject listener also serves Prometheus metrics:

```sh
curl -s http://localhost:7777/metrics | grep '^mast_'
```

You'll see the turn, model-call, token, cost, HITL, and budget families —
all listed in the [metrics reference](/reference/metrics/).

## The whole thing, scripted

`scripts/demo-spike2.sh` runs three scenarios end to end (graph routing,
durable HITL across `kill -9`, and a $0.01 budget cap tripping mid-turn):

```sh
scripts/demo-spike2.sh
```

## Going real

- Swap `--model=echo` for a Gemini model id (e.g.
  `--model=gemini-2.5-flash`) with Google Cloud ADC available; MCP toolsets
  from the bundle's `tool_catalog` get wired to specialists.
- Set `MAST_INJECT_TOKEN` in the daemon's environment to require bearer
  auth on `/inject`, `/resume`, and `/abort` (unset = unauthenticated, dev
  only).
- Against a real cluster the roster's shape starts to matter: the twelve
  diagnosers hold read tools only and name the remediation in their finding,
  and the one `change-executor` specialist carries it out — parking for your
  approval before each call that changes anything. Mast refuses to start a
  roster that blurs that line. See [per-specialist
  capability](/reference/workload-bundle/#per-specialist-capability), and
  classify any tools you add with `tool_catalog.tools[].mutating`: an
  unclassified tool counts as mutating, so an unclassified read tool will
  stop and ask.
- For production topologies (Cloud Run + Postgres sessions, GKE, systemd)
  see `examples/deploy/` in the repo. The GKE kustomize base under
  `deploy/` is durable by default — the daemon runs as a StatefulSet
  with a PVC-backed `--session-db`, so pauses, abort markers, and
  shutdown interruption markers survive pod rescheduling. In-memory
  sessions (omitting `--session-db`) are a local-development opt-out,
  not a deploy default.
- That base also grants the daemon cluster-wide **read** and nothing else;
  the permission to change a namespace is a separate apply, once per
  namespace. See [cluster permissions](/reference/cluster-permissions/),
  including the GKE IAM caveat that decides whether the split bounds
  anything.
