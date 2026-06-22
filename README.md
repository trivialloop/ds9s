# ds9s

A [k9s](https://github.com/derailed/k9s)-inspired terminal UI for **Docker Swarm**.

Connects to one Swarm manager at a time (local socket, TCP+TLS, or through an
SSH hop — including a proxy-jump) and lets you browse and act on:

- **services** (scale, force-update/restart, delete, logs)
- **containers / tasks** (delete, logs, inspect)
- **stacks** (delete the whole stack: services + their networks/configs/secrets, logs aggregated across the stack)
- **nodes** (read-only overview)
- **configs** / **secrets** (list, inspect metadata, delete)

## Build

```bash
go build -o ds9s .
```

Requires Go 1.22+ and normal internet access (to fetch modules):

```bash
go mod tidy   # only needed once, to (re)generate go.sum on your machine
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
manager — the same trick the docker CLI itself uses for `ssh://` hosts —
using that as the transport for every API call. Nothing needs to be exposed
on the network beyond SSH itself.

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

Command bar (press `:`):

| Command | View |
|---|---|
| `:services` / `:svc` | Services |
| `:containers` / `:co` / `:ps` | Containers / tasks |
| `:stacks` | Stacks |
| `:nodes` | Nodes |
| `:configs` | Configs |
| `:secrets` | Secrets |
| `:quit` / `:q` | Quit |

Keys (on the table, not in the command bar):

| Key | Action |
|---|---|
| `Enter` | Inspect / drill in |
| `l` | Logs (container: itself; service: all its tasks; stack: all its services' tasks) |
| `d` | Describe (raw JSON inspect) |
| `s` | Scale a service (prompts for replica count) |
| `u` | Force-update a service (rolling restart, no spec change) |
| `Ctrl-D` | Delete the selected resource, with confirmation |
| `r` | Force refresh |
| `?` | Help |
| `q` | Quit |

Deleting a **stack** removes every service carrying its
`com.docker.stack.namespace` label, plus the networks/configs/secrets scoped
to it — mirroring `docker stack rm`.

## Project layout

```
main.go                     CLI entrypoint (flags, config load, app bootstrap)
internal/config/            YAML config model + loader
internal/dockerx/           Docker Engine SDK wiring (unix/tcp+tls/ssh) and the
                             read/action layer (list/scale/delete/logs) used by the UI
internal/ui/                tview application: command bar, resource tables,
                             actions (delete/scale/logs/describe), modals
```

## Known limitations / next steps

- Stacks are reconstructed from the `com.docker.stack.namespace` label rather
  than read from a `docker-compose.yml` — ds9s never deploys stacks, only
  inspects/removes what's already running, same as k9s never creates
  Kubernetes objects.
- No live "watch" — Swarm's API doesn't expose one, so views poll on
  `refreshRate` (default 2s) plus manual `r`.
- Multi-manager: you connect to one manager per ds9s session; switching
  managers means relaunching with `--manager <name>` for now (an in-app
  `:context` switcher would be a natural follow-up, the same way k9s switches
  Kubernetes contexts).
- No xray-style tree view of stack → service → task yet (`Enter` on a stack
  just jumps to the services view for now).
