# ds9s

A [k9s](https://github.com/derailed/k9s)-inspired terminal UI for **Docker Swarm**.

Connects to one Swarm manager at a time (local socket, TCP+TLS, or through an
SSH hop — including a proxy-jump) and lets you browse and act on:

- **services** — scale, force-update/restart, delete, logs (all replicas)
- **containers / tasks** — cluster-wide view across all nodes, logs, delete, inspect
- **stacks** — health summary (services ok/total, replicas running/desired), delete entire stack
- **nodes** — read-only overview with leader indicator
- **configs** / **secrets** — list, inspect metadata, delete

## Build

```bash
go mod tidy   # only needed once, to (re)generate go.sum
go build -o ds9s .
```

## Configure

Copy `config.example.yaml` to `~/.config/ds9s/config.yaml` (or pass
`--config /path/to/file.yaml`) and edit it. Three connection styles are
supported per manager:

```yaml
current: prod-manager1
refreshRate: 2

managers:
  - name: local
    host: unix:///var/run/docker.sock

  - name: prod-manager-tcp
    host: tcp://10.0.0.10:2376
    tls:
      ca: /home/me/.docker/ca.pem
      cert: /home/me/.docker/cert.pem
      key: /home/me/.docker/key.pem

  - name: prod-manager1
    ssh:
      addr: manager1.example.com:22
      user: deploy
      privateKey: ~/.ssh/id_ed25519
      # password: "..."                # alternative to privateKey
      # proxyJump: bastion.example.com:22
      # knownHosts: ~/.ssh/known_hosts  # omit to skip host key verification
      # sudo: true                     # see below if your user isn't in the docker group
```

For the SSH case, ds9s opens the SSH session itself (optionally hopping
through `proxyJump` first) and runs `docker system dial-stdio` on the
manager — the same trick the docker CLI uses for `ssh://` hosts — using that
as the transport for every API call. Nothing needs to be exposed on the
network beyond SSH itself.

If your SSH user isn't in the manager's `docker` group, set `sudo: true`.
Because ds9s can't answer an interactive password prompt, this requires
passwordless sudo rights for the docker command on the manager, e.g. in
`/etc/sudoers`:

```
deploy ALL=(root) NOPASSWD: /usr/bin/docker
```

Run with a specific manager without touching the config:

```bash
./ds9s --manager prod-manager-tcp
```

## Usage

### Command bar (press `:`)

The command bar supports **autocomplete**: type a few letters and suggestions
appear; use arrow keys to navigate them.

| Command | View |
|---|---|
| `:services` / `:svc` | Services |
| `:containers` / `:co` / `:ps` | Containers / tasks (cluster-wide) |
| `:stacks` / `:stk` | Stacks |
| `:nodes` / `:no` | Nodes |
| `:configs` / `:cm` | Configs |
| `:secrets` / `:sec` | Secrets |
| `:alias` | List all commands and shortcuts |
| `:context` | Interactive manager switcher (arrow keys + Enter) |
| `:context <name>` | Switch directly to a named manager |
| `:quit` / `:q` | Quit |

### Keys (on the main table)

| Key | Action |
|---|---|
| `Enter` | **Logs** for services/tasks (default); describe for other resources; drill into stack |
| `d` | Describe — raw JSON inspect |
| `l` | Logs (container: itself; service: all replicas; stack: all services) |
| `s` | Scale a service (prompt for replica count) |
| `u` | Force-update a service (rolling restart, no spec change) |
| `Ctrl-D` | Delete selected resource (with confirmation) |
| `r` | Force refresh |
| `?` | Help |
| `q` | Quit |
| `Esc` | Close current overlay / go back |

### Log viewer

When viewing logs (press `l` or `Enter` on a service/task):

| Key | Action |
|---|---|
| `f` / `F` | Toggle **FOLLOW** mode (default: on). Follow auto-scrolls to newest lines; Pause lets you scroll back freely |
| `w` / `W` | Toggle **WRAP** mode (default: off). Wrap breaks long lines to fit the terminal width |
| `Esc` | Close log view |

A shortcut bar at the bottom of the log window shows the current state of each toggle
(teal = active, grey = inactive).

The log view loads the last 200 lines and then streams new lines in real time
(equivalent to `docker service logs --tail 200 -f`).

> **Secrets**: Docker secret *values* are write-only by design — the payload
> is encrypted at rest and the API never returns it, even to managers. Only
> metadata (name, labels, timestamps) is accessible. The secret value is
> available only inside authorised containers at `/run/secrets/<name>`.

## Views

### Services
Columns: `NAME · MODE · REPLICAS · IMAGE · STACK · ID`

- Replicated mode: `REPLICAS` shows `running/desired` (e.g. `3/3`).
- Global mode: `REPLICAS` shows `running/desired` where desired = number of eligible nodes.

### Containers / Tasks
Cluster-wide Swarm task list (uses the manager's TaskList API — no need to
SSH into worker nodes). Only tasks whose *desired state* is `running` are
shown. Columns: `NAME · SERVICE · NODE · STATE · IMAGE · ID`

### Stacks
Columns: `NAME · SERVICES · REPLICAS`

- `SERVICES` = `healthy/total` — a service is healthy when running ≥ desired replicas.
- `REPLICAS` = `running/desired` — aggregate across all services in the stack.

### Nodes
Columns: `HOSTNAME · ROLE · AVAILABILITY · STATUS · ENGINE · ID`

The `ROLE` column shows `leader` for the current Raft leader, `manager` for
non-leader managers, and `worker` for worker nodes.

## Switching managers (`:context`)

Multiple managers can be configured in the YAML. Press `:` then type
`context` (or `ctx`) to open an interactive list. Use `↑`/`↓` to navigate
and `Enter` to switch. The UI reconnects in the background; SSH tunnels are
re-established transparently.

You can also switch directly: `:context prod-manager2`.

## Project layout

```
main.go                     CLI entrypoint (flags, config load, app bootstrap)
internal/config/            YAML config model + loader
internal/dockerx/           Docker Engine SDK wiring (unix/tcp+tls/ssh) and the
                             read/action layer (list/scale/delete/logs) used by the UI
internal/ui/                tview application: command bar, resource tables,
                             actions (delete/scale/logs/describe), modals
```

## Known limitations

- Stacks are reconstructed from the `com.docker.stack.namespace` label — ds9s
  never deploys stacks, only inspects/removes what is already running.
- No live "watch" — Swarm's API does not expose one; views poll on
  `refreshRate` (default 2 s) plus manual `r`.
- Container deletion in the tasks view only works for containers running on the
  currently connected manager node; worker-node containers will return "not
  found" (by design — use service delete/scale instead).
- No xray-style tree view of stack → service → task yet (`Enter` on a stack
  jumps to the services view for now).
