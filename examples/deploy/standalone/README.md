# Standalone deployment — systemd unit

The standalone shape from
[`docs/deployment-design.md`](../../../docs/deployment-design.md): one
mast binary on a Linux host (VM, bare metal), SQLite session store on
local disk. Simplest possible topology — no coordination, sessions
durable across restarts via `--session-db`.

Files here:

| File | Installs to |
|---|---|
| [`mast.service`](./mast.service) | `/etc/systemd/system/mast.service` |
| [`mast.env.example`](./mast.env.example) | `/etc/mast/mast.env` (edit first) |

## Walkthrough

1. **Build and install the binary.**

   ```sh
   go build -o mast ./cmd/mast
   sudo install -m 0755 mast /usr/local/bin/mast
   ```

2. **Create the service user and config tree.**

   ```sh
   sudo useradd --system --home-dir /var/lib/mast --shell /usr/sbin/nologin mast
   sudo mkdir -p /etc/mast/agents/workloads /etc/mast/agents/specialists
   ```

   `/etc/mast/agents` is the `.agents/` config root
   ([`docs/config-layout-design.md`](../../../docs/config-layout-design.md));
   the unit pins it via `MAST_CONFIG_DIR` so discovery never depends
   on systemd's working directory. Drop your `workloads/*.yaml` and
   `specialists/*.tmpl` there (see
   [`examples/workloads/gke-triage/`](../../workloads/gke-triage/) for
   the shape), then point `--workload=<name>` in the unit at your
   workload's name.

3. **Create the env file** (bearer token + optional GCP/OTel env):

   ```sh
   sudo install -m 0600 mast.env.example /etc/mast/mast.env
   sudoedit /etc/mast/mast.env   # set MAST_INJECT_TOKEN etc.
   ```

4. **Install and start the unit.**

   ```sh
   sudo install -m 0644 mast.service /etc/systemd/system/mast.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now mast
   ```

   `StateDirectory=mast` creates `/var/lib/mast`; the session DB lands
   at `/var/lib/mast/sessions.db`. Session state never goes anywhere
   else on the host.

5. **Verify.**

   ```sh
   systemctl status mast
   curl -sf http://127.0.0.1:7777/          # health, unauthenticated
   sudo -u mast mast sessions list --session-db=/var/lib/mast/sessions.db
   ```

## Notes

- **Durability:** pauses (HITL interrupts) and aborts survive restart
  because they live in the SQLite DB — kill the process mid-pause and
  `mast sessions resume` still works after restart
  ([`docs/durable-execution-design.md`](../../../docs/durable-execution-design.md)).
- **Backups:** back up `/var/lib/mast/sessions.db` like any SQLite
  file (e.g. `sqlite3 ... ".backup"`); mast adds no backup layer.
- **Exposure:** the unit binds `127.0.0.1`. If a remote producer needs
  to POST events, front mast with a TLS-terminating reverse proxy and
  keep `MAST_INJECT_TOKEN` set — the token is the only auth.
- **Outgrowing this shape:** more than one instance means an external
  session store — see [`../cloud-run/`](../cloud-run/) (Postgres) and
  the v0.2 multi-instance row in deployment-design.
