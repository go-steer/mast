# AX — prototype

Runs a mast one-shot turn as an [AX](https://github.com/google/ax) task.
AX runs each task in an Agent Substrate sandbox that can be suspended and
resumed. On suspend, Substrate snapshots `/workspace`; on resume, it
restores those files into a fresh container. mast keeps its session in
SQLite under `/workspace`, so a resumed task carries on the same
conversation.

This is a prototype. It proves the lifecycle works; it is not yet a useful
deployment shape (see [Limits](#limits)).

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

## Run on AX

Edit `spec.image` in [`task.yaml`](task.yaml), then:

```sh
ax apply -f examples/deploy/ax/task.yaml
ax resume mast-oneshot      # new tasks start Suspended
ax watch mast-oneshot
```

### Network and credentials

On Agent Substrate v0.2.0 and later an actor has no outbound network until it
has an egress policy, and upstream AX doesn't create one (google/ax#449).

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

Both need Substrate's credential injection (`sdsmint` egress gateway with a
credential provider) and the gateway's CA bundle in the sandbox.

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

- **One-shot only, so no tools.** A one-shot turn has no workload bundle,
  so no MCP servers or specialists. Running a real workload means serve
  mode, where turns arrive over HTTP. Substrate's router reaches the sandbox
  only on port 80, which AX's runner owns, so upstream AX gives callers no
  way in. A runner pass-through (`spec.http.port`, currently on a fork of
  AX) forwards those requests to mast's attach listener; with it, mast's
  session API (`POST /sessions`, `POST /sessions/<sid>/inject`, the event
  stream) works through the router, and a request wakes a suspended task.
- **Repeated prompt after an interruption.** Each re-run appends the
  prompt to the session again, so after N interruptions the model sees it
  N+1 times. One-shot mode has no way to continue a session without a new
  message.
- **Suspend gives no warning.** Suspend checkpoints and freezes the sandbox
  without sending SIGTERM (google/ax#451); only `/workspace` survives, and
  on Substrate v0.3.0 the command starts again on resume. A turn cut short
  re-runs, and mutating tools are at-least-once, as everywhere in mast.
- **No status back to AX.** AX doesn't read exit codes, usage, or pending
  approvals from the task. Check `result.txt` and `mast sessions` inside
  the sandbox (`ax ssh` with `spec.debug: true`).
- **Image runs as root**, because the runner binds port 80 and writes
  under `/ax`, as AX's own image does.
