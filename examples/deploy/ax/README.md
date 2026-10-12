# AX

Runs mast as an [AX](https://github.com/google/ax) task. AX runs each task
in an [Agent Substrate](https://github.com/agent-substrate/substrate)
sandbox that can be suspended and resumed. mast keeps its sessions in SQLite
under `/workspace`, which survives every suspend, so a resumed task carries
on the same conversations.

Two shapes:

- **[Serve mode](#serve-mode)** ([`task-serve.yaml`](task-serve.yaml)): a
  long-lived agent with a workload. Clients open sessions and talk to it
  through Substrate's router. It suspends when idle and wakes on the next
  request. This is the useful shape, and it needs three AX features that
  are only on a fork today.
- **One-shot** ([`task.yaml`](task.yaml)): a single turn with no workload,
  which runs on upstream AX. It's mainly a way to see the lifecycle.

Tested on Agent Substrate v0.3.0 and v0.4.0.

## How it fits together

AX does not run your command as the container entrypoint. It always starts
`/usr/local/bin/ax-task-runner`, which prepares the workspaces, serves
health checks on port 80, and then starts `spec.command`
([AX runner contract](https://github.com/google/ax/blob/main/docs/runner.md)).
The image here carries three binaries:

| Binary | Role |
|---|---|
| `ax-task-runner` | AX's runner, built from AX source at a pinned commit |
| `ax-mast-entry` | Small wrapper around one mast turn ([`entrypoint/`](entrypoint/main.go)) |
| `mast` | mast itself |

The wrapper exists because the runner starts `spec.command` on every boot,
and AX's sandboxes boot in ways a plain one-shot does not expect:

- **The golden boot.** Substrate boots each new task template once,
  snapshots it, and restores that snapshot into each task, with the
  command's process already running. Egress is denied during that boot and
  `/workspace` is empty (google/ax#427, google/ax#428). With
  `--wait-for-egress`, the wrapper waits until it can complete a TLS
  handshake with the model endpoint, so the turn starts only in the
  real task.
- **Resume after completion.** When mast exits successfully, the wrapper
  writes `/workspace/.mast/done`. On later boots it sees the marker and does
  not run the turn again.
- **Resume after interruption.** If the turn was cut off (suspend, crash,
  model error), there is no marker, so the next boot runs mast again
  against the same `--session-db`. The new turn sees the earlier history.
- **Output.** AX has no way to read a command's output back yet, so the
  wrapper copies mast's stdout to `/workspace/.mast/result.txt`.

## Build

From the repo root:

```sh
docker build -f examples/deploy/ax/Dockerfile -t REGISTRY/mast-ax:dev .
docker push REGISTRY/mast-ax:dev
```

`AX_REF` pins the AX commit the runner is built from. AX's API is
`v1alpha1`, so bump it deliberately.

## Run the one-shot on AX

Edit `spec.image` in [`task.yaml`](task.yaml), then:

```sh
ax apply -f examples/deploy/ax/task.yaml
ax resume mast-oneshot      # new tasks start Suspended
ax watch mast-oneshot
```

## Serve mode

[`task-serve.yaml`](task-serve.yaml) runs `mast` directly, without the
wrapper, with:

- `--attach-listen=127.0.0.1:8484`: the session API (`POST /sessions`,
  `POST /sessions/<app>/<sid>/inject`, `/wake`, the event stream);
- `--session-db=/workspace/.mast/sessions.db`: sessions survive suspend,
  resume and restarts;
- `--instructions=/workspace/repo`: the workspace's `AGENTS.md` tells the
  agent what this deployment is for (which cluster it serves, for example);
- `MAST_GOOGLE_AUTH=injected`: the egress gateway adds the Google token.

A client reaches mast through Substrate's router, with the actor named in a
header and mast's attach token as the bearer:

```sh
ROUTER=http://atenet-router.ate-system.svc.cluster.local
curl -X POST -H "ate-target-actor: default/mast-serve" \
  -H "Authorization: Bearer $MAST_ATTACH_TOKEN" -d '{}' $ROUTER/sessions
```

If the task is suspended, the router resumes it and holds the request until
it's running. The task suspends again after `spec.idle.suspendAfter`
without requests.

Three task fields aren't in upstream AX yet. They're on a fork
([mastersingh24/ax](https://github.com/mastersingh24/ax)), and the runner in
the image has to come from it too:

| Field | Does |
|---|---|
| `spec.http.port` | the runner forwards requests that reach port 80 to this port, so the router can reach mast |
| `spec.idle.suspendAfter` | AX suspends the task after this long without requests (an open request, such as an event stream, counts as activity) |
| `spec.egress` | the actor's egress policy, with credentials the gateway adds |

Without them the task runs, but nothing can reach mast and it never
suspends on its own.

## Network and credentials

On Agent Substrate v0.2.0 and later an actor has no outbound network until it
has an egress policy, and upstream AX doesn't create one (google/ax#449).
`spec.egress` (above) creates it.

Keep credentials out of the task. AX's built-in `GEMINI_API_KEY` injection,
and anything in `spec.env`, ends up in plain text in the actor template and
its snapshots. Substrate's egress gateway can add credentials to outbound
requests instead, so the sandbox never holds them:

- **API keys** (Gemini API): an egress rule replaces `x-goog-api-key`; run
  mast with `GEMINI_API_KEY=placeholder`.
- **Google OAuth** (Vertex AI models, Google-managed MCP servers such as
  GKE's): an egress rule replaces `Authorization` with a token served by a
  credential provider; run mast with `MAST_GOOGLE_AUTH=injected`, which
  makes it send `Authorization: Bearer placeholder` instead of looking for
  Application Default Credentials.

Both need Substrate's credential injection: on v0.4.0, the egress gateway
plus the Kubernetes credential provider (`ATE_CREDENTIAL_PROVIDER`). The
sandbox also needs the gateway's CA. AX projects it as a trust bundle named
`egress-mitm.ate.dev` together with `system-roots.ate.dev`, so one file
holds both.

## On Agent Substrate v0.4.0

- **Snapshot type.** A template's snapshots are either full (process
  memory, root filesystem and `/workspace`) or data-only (`/workspace`
  only). With full snapshots a woken mast is the same process, mid-thought.
  With data-only snapshots it restarts from the image and reloads each
  session from the session store the first time the session is used.
  Either way the conversation survives. Full snapshots also tie the task to
  the image it was taken with and to the **CPU platform**: gVisor won't
  restore onto a host missing any CPU feature recorded in the snapshot
  (`incompatible FeatureSet`), so keep a worker pool on one CPU platform
  (N4 and C4 share one; GKE's E2 mixes AMD and Intel). Prefer data-only
  snapshots for agents you keep for a long time.
- **Waking takes time.** The router holds a request while its actor resumes
  for at most `--parked-request-budget`, 5 s by default, and a resume after
  a long suspend can take longer. A client then gets a `504` although the
  wake succeeds. Raise the budget on `atenet-router` (we use `60s`) or have
  clients retry.

## Try it locally

The runner reads its specs from files as well as the environment. A
host directory mounted at `/workspace` stands in for the durable volume,
so a second `docker run` on the same directory behaves like a resume:

```sh
docker run --rm -p 8080:80 \
  -v "$PWD/ws:/workspace" -v "$PWD/specs:/spec:ro" \
  REGISTRY/mast-ax:dev --task-file /spec/task.yaml --workspace-file /spec/ws.yaml

curl -i http://127.0.0.1:8080/readyz
```

Use `--model=echo` in the task's command to run without credentials, and
drop `--wait-for-egress`, which has no meaning outside Substrate.

## Limits

- **Serve mode needs an AX fork.** Substrate's router reaches the sandbox
  only on port 80, which AX's runner owns, so upstream AX gives callers no
  way in. See [Serve mode](#serve-mode).
- **One-shot has no tools.** A one-shot turn has no workload bundle, so no
  MCP servers or specialists.
- **Repeated prompt after an interruption.** Each re-run appends the
  prompt to the session again, so after N interruptions the model sees it
  N+1 times. One-shot mode has no way to continue a session without a new
  message.
- **Suspend gives no warning.** Suspend checkpoints and freezes the sandbox
  without sending SIGTERM (google/ax#451). On Substrate v0.3.0, and on
  v0.4.0 with data-only snapshots, the command starts again on resume, so
  only `/workspace` survives. A turn cut short
  re-runs, and mutating tools are at-least-once, as everywhere in mast.
- **No status back to AX.** AX doesn't read exit codes, usage, or pending
  approvals from the task. Check `result.txt` and `mast sessions` inside
  the sandbox (`ax ssh` with `spec.debug: true`).
- **Image runs as root**, because the runner binds port 80 and writes
  under `/ax`, as AX's own image does.
