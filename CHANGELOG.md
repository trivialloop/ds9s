# Changelog

All notable changes to ds9s are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

---

## [0.1.0] - 2026-06-23

### Added

- **Services** view — list, scale, force-update, logs, describe, delete
- **Containers / Tasks** view — cluster-wide Swarm task list across all nodes
  - `s` — interactive shell in container (SSH to owning node for workers)
  - `k` — SIGKILL (SSH to owning node for workers)
  - `Ctrl-D` — delete container (SSH to owning node for workers)
  - `f` — toggle filter between running only ↔ running + stopped
  - Color coding: yellow = starting, red = failed/rejected, grey = stopped
- **Stacks** view — health summary (services ok/total, replicas running/desired)
- **Nodes** view — hostname, role (leader/manager/worker), availability, status
- **Configs** and **Secrets** views — list, inspect metadata, delete
- **Volumes** view — per-node listing via SSH (or manager-only without SSH)
- **Networks** view — per-node listing via SSH (or manager-only without SSH)
- **`:` command bar** — switch views by name/alias, with autocomplete
- **`/` text filter** — live filter across all columns, active on all views
- **Log viewer** — follow mode, wrap mode, works for services and containers
- **Context switcher** (`:context`) — interactive manager switcher
- **Cross-node SSH** — shell, kill and delete route through SSH to worker nodes
  using the manager's credentials; `proxyJump` is honoured
- **Confirmation dialog** — Enter to confirm, Esc to cancel, orange border
- Shell window header: Node, Stack, Service, Container displayed at top
- Binary version injected at build time via `-ldflags -X main.version`
