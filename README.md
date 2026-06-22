# ds9s

A [k9s](https://github.com/derailed/k9s)-inspired terminal UI for **Docker Swarm**.

Connects to one Swarm manager at a time (local socket, TCP+TLS, or through an
SSH hop — including a proxy-jump) and lets you browse and act on:

- **services** — scale, force-update/restart, delete, logs (all replicas)
- **containers / tasks** — cluster-wide view across all nodes, shell exec, kill, delete, logs, inspect
- **stacks** — health summary (services ok/total, replicas running/desired), delete entire stack
- **nodes** — read-only overview with leader indicator
- **configs** / **secrets** — list, inspect metadata, delete
- **volumes** / **networks** — per-node listing via SSH (or manager-only without SSH)

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

The same SSH credentials are reused to reach **worker nodes** for cross-node
operations (shell, kill, delete, volumes, networks). The `proxyJump` is
honoured for worker connections too.

If your SSH user isn't in the `docker` group, set `sudo: true`.
Because ds9s can't answer an interactive password prompt, this requires
passwordless sudo rights for the docker command, e.g. in `/etc/sudoers`:

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
| `:volumes` / `:vol` | Volumes (per node) |
| `:networks` / `:net` | Networks (per node) |
| `:alias` | List all commands and shortcuts |
| `:context` | Interactive manager switcher (arrow keys + Enter) |
| `:context <name>` | Switch directly to a named manager |
| `:quit` / `:q` | Quit |

### Keys (on the main table)

| Key | Action |
|---|---|
| `Enter` | **Logs** for services/tasks; describe for other resources; drill into stack |
| `d` | Describe — raw JSON inspect |
| `l` | Logs (container: service logs; service: all replicas; stack: all services) |
| `s` | **Services**: scale (prompt for replica count) / **Containers**: open shell in container |
| `k` | **Kill** container with SIGKILL (containers view — SSH to the owning node) |
| `u` | Force-update a service (rolling restart, no spec change) |
| `f` | **Containers view**: toggle filter between running only ↔ running + stopped |
| `Ctrl-D` | Delete selected resource (with confirmation — SSH to node for containers) |
| `r` | Force refresh |
| `?` | Help / aliases |
| `q` | Quit |
| `Esc` | Close current overlay / go back |

### Log viewer

When viewing logs (press `l` or `Enter` on a service/task):

| Key | Action |
|---|---|
| `f` / `F` | Toggle **FOLLOW** mode (default: on) |
| `w` / `W` | Toggle **WRAP** mode (default: off) |
| `Esc` | Close log view |

A shortcut bar at the bottom of the log window shows the current state of each toggle
(teal = active, grey = inactive).

### Shell in container (`s` on containers view)

Opens an interactive `/bin/sh` inside the selected container. When SSH is
configured, the shell is established directly over SSH to the node that hosts
the container — no separate `ssh` binary is needed and no agent key flooding
occurs. A header bar at the top of the shell window shows the node, stack,
service and container ID for quick orientation.

### Container filter (`f` on containers view)

Toggles between:
- **RUNNING** — only tasks whose desired state is `running` (default)
- **ALL** — includes stopped, failed, and shutting-down tasks, shown in color:
  - White — running
  - Yellow — starting / preparing
  - Red — failed / rejected / orphaned
  - Grey — shutdown / complete

> **Secrets**: Docker secret *values* are write-only by design — the payload
> is encrypted at rest and the API never returns it, even to managers. Only
> metadata (name, labels, timestamps) is accessible.

## Views

### Services
Columns: `NAME · MODE · REPLICAS · IMAGE · STACK · ID`

- Replicated mode: `REPLICAS` shows `running/desired` (e.g. `3/3`).
- Global mode: `REPLICAS` shows `running/desired` where desired = number of eligible nodes.

### Containers / Tasks
Cluster-wide Swarm task list (uses the manager's TaskList API — no SSH needed
to list containers). Columns: `NAME · SERVICE · NODE · STATE · IMAGE · ID`

Press `f` to include stopped/failed containers. Colors indicate state.

### Stacks
Columns: `NAME · SERVICES · REPLICAS`

- `SERVICES` = `healthy/total` — a service is healthy when running ≥ desired replicas.
- `REPLICAS` = `running/desired` — aggregate across all services in the stack.

### Nodes
Columns: `HOSTNAME · ROLE · AVAILABILITY · STATUS · ENGINE · ID`

The `ROLE` column shows `leader` for the current Raft leader, `manager` for
non-leader managers, and `worker` for worker nodes.

### Volumes / Networks
With SSH configured: fetched from every node in parallel.  
Without SSH: shows manager node only.

## Switching managers (`:context`)

Multiple managers can be configured in the YAML. Press `:` then type
`context` (or `ctx`) to open an interactive list. The UI reconnects in the
background; SSH tunnels are re-established transparently.

## Project layout

```
main.go                     CLI entrypoint (flags, config load, app bootstrap)
internal/config/            YAML config model + loader
internal/dockerx/           Docker Engine SDK wiring (unix/tcp+tls/ssh) and the
                             read/action layer (list/scale/delete/logs) used by the UI
internal/ui/                tview application: command bar, resource tables,
                             actions (delete/scale/logs/describe/shell/kill), modals
```
