# ds9s — Claude Code context

## What this is

A k9s-inspired terminal UI for Docker Swarm, written in Go.
Browse and act on services, containers (cluster-wide tasks), stacks, nodes, configs, and secrets.

## Build

```bash
go mod tidy   # once, to regenerate go.sum if deps change
go build -o ds9s .
./ds9s --config ~/.config/ds9s/config.yaml
```

Requires Go ≥ 1.21. The toolchain in `go.mod` auto-upgrades if needed (golang.org/x/time requires ≥ 1.25.0).

## Key dependencies

| Package | Version | Purpose |
|---|---|---|
| `github.com/rivo/tview` | v0.42.0 | Terminal UI framework (widgets, layout, event loop) |
| `github.com/gdamore/tcell/v2` | v2.8.1 | Low-level terminal cells, color, keyboard events |
| `github.com/docker/docker` | v24.0.9 | Docker SDK — Swarm API calls |
| `github.com/docker/docker/pkg/stdcopy` | (same) | Demultiplex Docker log streams (stdout/stderr) |

## Project layout

```
main.go                          CLI flags, config load, App bootstrap
internal/config/config.go        YAML config: Manager, SSHConfig, TLSConfig
internal/dockerx/
  connection.go                  Dial Docker: unix socket / TCP+TLS / SSH proxy
  store.go                       Read/action layer (list, scale, delete, logs)
internal/ui/
  app.go                         App struct, tview wiring, global keys, command bar
  views.go                       Table rendering per resource (services, nodes, …)
  actions.go                     Interactive actions: logs, describe, scale, delete, modals
```

## Architecture

### App struct (`app.go`)

```
App
├── tv         *tview.Application   — event loop, screen, focus
├── pages      *tview.Pages         — base layer + overlays (modals, logs, describe)
├── root       *tview.Flex (rows)
│   ├── header   *tview.TextView    — "ds9s  manager:X  host:Y  view:Z"
│   ├── table    *tview.Table       — the main resource table
│   ├── hints    *tview.TextView    — k9s-style shortcut bar (view-specific)
│   ├── cmdInput *tview.InputField  — ":" command bar
│   └── status   *tview.TextView    — status / error line
├── conn       *dockerx.Connection
└── store      *dockerx.Store
```

### Event model — critical rules

1. **`globalKeys` guards `a.tv.GetFocus() != a.table`** — returns the event unmodified
   when focus is not on the table. This lets modals, TextView overlays, and InputField
   manage their own keys without interference.

2. **Never call `a.tv.Draw()` from inside `InputCapture`** — tview holds the screen lock
   during event dispatch; a re-entrant Draw() deadlocks immediately. tview redraws
   automatically after every InputCapture that returns nil.

3. **`SetChangedFunc` is safe to call `Draw()`** — it is invoked inside `QueueUpdateDraw`,
   so the lock is not held.

4. **Background goroutines must use `QueueUpdateDraw`** — never mutate tview widgets
   directly from a non-main goroutine.

5. **`cmdInput.SetDoneFunc` checks `a.tv.GetFocus() == a.cmdInput`** before resetting
   focus to table — prevents stomping focus when `runCommand` already opened an overlay.

### Connection types (`dockerx/connection.go`)

- **Unix socket** — `host: unix:///var/run/docker.sock`
- **TCP + TLS** — `host: tcp://...`, TLS fields optional
- **SSH** — runs `docker system dial-stdio` on the remote; supports `proxyJump` and `sudo`

### Store layer (`dockerx/store.go`)

- `AllTasks()` — cluster-wide tasks: 3 API calls (TaskList + ServiceList + NodeList),
  filtered to `DesiredState == running`, names resolved.
- `ServiceLogs()` — uses `client.ServiceLogs()` (streams through manager, works for
  tasks on worker nodes). Do NOT use `ContainerLogs` for Swarm services.
- `Stacks()` — uses `ServiceList(Status:true)` only; `ServiceStatus.RunningTasks`/
  `DesiredTasks` already computed by Swarm, no TaskList needed.
- `ServiceTaskContainerIDs()` — legacy helper, not used in the main flows; kept for
  possible future use.

### Log viewer (`actions.go: openLogView`)

Layout:
```
outer (tview.Flex, FlexRow, border+title)
├── view  *tview.TextView   — scrollable log content (focus target)
└── bar   *tview.TextView   — k9s-style shortcut bar: <f> FOLLOW  <w> WRAP  <Esc> CLOSE
```

- `follow bool` and `wrap bool` captured by closures; safe as plain bools because
  `SetChangedFunc` and `InputCapture` both run on the tview main goroutine.
- `renderBar()` redraws the shortcut bar using tview color tags.
- Pressing `f` toggles follow; pressing `w` toggles line wrap; `Esc` cancels context
  and removes the page.
- Logs are streamed via `tailOneService` / `streamServiceLogs` goroutines that write
  via `QueueUpdateDraw`.

## Conventions

### rowMeta

Every data row stores a `rowMeta` as the cell reference of column 0:

```go
type rowMeta struct {
    kind        viewName
    id          string  // primary resource ID (service/node/stack name/...)
    name        string  // human-readable name
    serviceID   string  // containers view: parent service ID (for ServiceLogs)
    containerID string  // containers view: actual container ID (for ContainerRemove)
}
```

Handlers retrieve it with `a.selectedRow()`.

### viewName

String constants (`viewServices`, `viewContainers`, `viewStacks`, `viewNodes`,
`viewConfigs`, `viewSecrets`) used as keys everywhere — in `globalKeys` switches,
`handleEnter`, `handleLogs`, etc.

### Hints bar

`updateHints()` is called from `refreshHeader()` (which is called by `refreshCurrent()`
on every poll tick). Each chip: `[black:teal:b] key [-:-:-] [white]LABEL[-]`.
View-specific chips are prepended; common chips (`:`, `r`, `?`, `q`) are appended.

## Common pitfalls

| Symptom | Root cause |
|---|---|
| Crash / freeze when pressing a key in log view | `a.tv.Draw()` called from inside InputCapture — remove it |
| Esc doesn't work on an overlay | `globalKeys` is intercepting it — check the `GetFocus() != table` guard |
| Enter confirms a modal AND triggers an action | Same root cause as above |
| Logs say "container not found" for Swarm services | Used `ContainerLogs` instead of `ServiceLogs`; containers run on worker nodes |
| Container delete "not found" for tasks | Used task ID (`ref.id`) instead of `ref.containerID` |
| `cmdInput` SetDoneFunc steals focus from overlay | Missing `if a.tv.GetFocus() == a.cmdInput` guard before `SetFocus(table)` |
