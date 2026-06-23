// Package ui implements the ds9s terminal UI on top of tview/tcell,
// deliberately mirroring k9s' interaction model: a ':' command bar to switch
// resource views, a table per resource, and a handful of resource-specific
// key bindings (logs, scale, delete...).
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"ds9s/internal/config"
	"ds9s/internal/dockerx"
)

// viewName identifies the resource views ds9s knows how to render.
type viewName string

const (
	viewServices   viewName = "services"
	viewContainers viewName = "containers"
	viewStacks     viewName = "stacks"
	viewNodes      viewName = "nodes"
	viewConfigs    viewName = "configs"
	viewSecrets    viewName = "secrets"
	viewVolumes    viewName = "volumes"
	viewNetworks   viewName = "networks"
)

// aliases lets users type short forms in the command bar, k9s-style.
var aliases = map[string]viewName{
	"svc": viewServices, "svcs": viewServices, "service": viewServices, "services": viewServices,
	"co": viewContainers, "ctr": viewContainers, "container": viewContainers, "containers": viewContainers, "ps": viewContainers,
	"stack": viewStacks, "stacks": viewStacks, "stk": viewStacks,
	"node": viewNodes, "nodes": viewNodes, "no": viewNodes,
	"config": viewConfigs, "configs": viewConfigs, "cm": viewConfigs,
	"secret": viewSecrets, "secrets": viewSecrets, "sec": viewSecrets,
	"volume": viewVolumes, "volumes": viewVolumes, "vol": viewVolumes, "pv": viewVolumes,
	"network": viewNetworks, "networks": viewNetworks, "net": viewNetworks, "ns": viewNetworks,
}

// App owns the whole UI: the tview application, layout primitives, the
// current docker connection/store, and the currently displayed view.
type App struct {
	tv  *tview.Application
	cfg *config.Config

	conn  *dockerx.Connection
	store *dockerx.Store

	pages    *tview.Pages
	root     *tview.Flex
	header   *tview.TextView
	table    *tview.Table
	hints    *tview.TextView
	cmdInput *tview.InputField
	status   *tview.TextView

	current  viewName
	stopPoll chan struct{}

	// containers view filter: false = running only (default), true = running + stopped
	containersShowAll bool

	// text filter — applied on every refresh to skip non-matching rows
	filterText string
	filterBar  *tview.InputField

	// cached rendered text — only update widgets when content actually changes
	// to prevent unnecessary screen redraws (flicker) during auto-refresh.
	lastHeaderText string
	lastHintsText  string
}

// NewApp wires up the UI for the given config; the manager to connect to
// first is cfg.Current (or the first manager if unset).
func NewApp(cfg *config.Config) (*App, error) {
	mgr, err := cfg.ManagerByName(cfg.Current)
	if err != nil {
		return nil, err
	}
	conn, err := dockerx.Connect(*mgr)
	if err != nil {
		return nil, fmt.Errorf("connecting to manager %s: %w", mgr.Name, err)
	}

	a := &App{
		tv:      tview.NewApplication(),
		cfg:     cfg,
		conn:    conn,
		store:   dockerx.NewStore(conn),
		current: viewContainers,
	}
	a.build()
	return a, nil
}

func (a *App) build() {
	a.header = tview.NewTextView().SetDynamicColors(true)
	a.header.SetBorder(false)

	a.table = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	a.table.SetBorder(true)
	a.table.SetBorderColor(tcell.ColorSteelBlue)

	a.status = tview.NewTextView().SetDynamicColors(true)

	a.cmdInput = tview.NewInputField().SetLabel(":")
	a.cmdInput.SetFieldBackgroundColor(tcell.ColorBlack)
	a.cmdInput.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			a.runCommand(a.cmdInput.GetText())
		}
		a.cmdInput.SetText("")
		// Only return focus to the table when the command didn't open an
		// overlay page (which would have called SetFocus itself).
		if a.tv.GetFocus() == a.cmdInput {
			a.tv.SetFocus(a.table)
		}
	})

	// Autocomplete: suggest view aliases and command names.
	// For ":context <name>" also suggest known manager names.
	a.cmdInput.SetAutocompleteFunc(func(current string) []string {
		if current == "" {
			return nil
		}
		fields := strings.Fields(current)
		cmd := fields[0]
		// Sub-command completion: context/ctx <manager-name>
		if (cmd == "context" || cmd == "ctx") && strings.ContainsRune(current, ' ') {
			partial := ""
			if len(fields) >= 2 {
				partial = fields[1]
			}
			var suggestions []string
			for _, name := range a.cfg.Names() {
				if strings.HasPrefix(name, partial) {
					suggestions = append(suggestions, cmd+" "+name)
				}
			}
			return suggestions
		}
		// Top-level command/alias completion.
		allCmds := []string{
			"services", "svc", "svcs", "service",
			"containers", "co", "ctr", "container", "ps",
			"stacks", "stack", "stk",
			"nodes", "node", "no",
			"configs", "config", "cm",
			"secrets", "secret", "sec",
			"volumes", "volume", "vol", "pv",
			"networks", "network", "net", "ns",
			"alias", "aliases",
			"context", "ctx",
			"quit", "q", "exit",
		}
		var suggestions []string
		for _, s := range allCmds {
			if strings.HasPrefix(s, current) {
				suggestions = append(suggestions, s)
			}
		}
		return suggestions
	})

	a.hints = tview.NewTextView().SetDynamicColors(true)

	a.filterBar = tview.NewInputField().SetLabel("[yellow]/[-] ")
	a.filterBar.SetLabelWidth(3)
	a.filterBar.SetFieldBackgroundColor(tcell.ColorBlack)
	a.filterBar.SetChangedFunc(func(text string) {
		a.filterText = text
		a.table.Clear()
		a.lastHeaderText = "" // force header refresh (filter badge)
		if err := a.refreshCurrent(); err != nil {
			a.setStatus(fmt.Sprintf("[red]%v", err))
		}
	})
	a.filterBar.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			a.filterText = ""
			a.filterBar.SetText("")
			a.table.Clear()
			a.lastHeaderText = ""
			if err := a.refreshCurrent(); err != nil {
				a.setStatus(fmt.Sprintf("[red]%v", err))
			}
		}
		// Hide the filter bar and return focus to the table.
		a.root.ResizeItem(a.filterBar, 0, 0)
		a.tv.SetFocus(a.table)
	})

	a.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.header, 1, 0, false).
		AddItem(a.filterBar, 0, 0, false). // hidden until '/' is pressed
		AddItem(a.table, 0, 1, true).
		AddItem(a.hints, 1, 0, false).
		AddItem(a.cmdInput, 1, 0, false).
		AddItem(a.status, 1, 0, false)

	a.pages = tview.NewPages().AddPage("main", a.root, true, true)

	a.tv.SetRoot(a.pages, true)
	a.tv.SetInputCapture(a.globalKeys)

	a.refreshHeader()
	a.setStatus("Ready. Press [:] to switch views, [?] for help, [q] to quit.")
}

// Run starts the UI event loop and the background refresh poller. It blocks
// until the user quits.
func (a *App) Run() error {
	a.stopPoll = make(chan struct{})
	go a.pollLoop()
	defer close(a.stopPoll)
	defer a.conn.Close()

	if err := a.refreshCurrent(); err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
	}
	return a.tv.Run()
}

func (a *App) pollLoop() {
	interval := time.Duration(a.cfg.RefreshRate) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Skip auto-poll for views that require SSH to every node —
			// those only refresh on demand (press r).
			if a.current == viewVolumes || a.current == viewNetworks {
				continue
			}
			a.tv.QueueUpdateDraw(func() {
				if err := a.refreshCurrent(); err != nil {
					a.setStatus(fmt.Sprintf("[red]%v", err))
				}
			})
		case <-a.stopPoll:
			return
		}
	}
}

func (a *App) globalKeys(event *tcell.EventKey) *tcell.EventKey {
	// Only intercept shortcuts when the main table has focus.
	// Modals, prompt inputs, text views, and the command input all manage
	// their own key handling — passing the event through lets them work.
	if a.tv.GetFocus() != a.table {
		return event
	}

	switch event.Key() {
	case tcell.KeyCtrlD:
		a.handleDelete()
		return nil
	case tcell.KeyEnter:
		a.handleEnter()
		return nil
	}

	switch event.Rune() {
	case ':':
		a.tv.SetFocus(a.cmdInput)
		return nil
	case '?':
		a.showHelp()
		return nil
	case 'q':
		a.confirm("Quit ds9s?", func() { a.tv.Stop() })
		return nil
	case 'r':
		if err := a.refreshCurrent(); err != nil {
			a.setStatus(fmt.Sprintf("[red]%v", err))
		}
		return nil
	case 'l':
		a.handleLogs()
		return nil
	case 's':
		// Contextual: scale on services, shell exec on containers.
		if a.current == viewContainers {
			a.handleShell()
		} else {
			a.handleScale()
		}
		return nil
	case 'd':
		a.handleDescribe()
		return nil
	case 'u':
		a.handleForceUpdate()
		return nil
	case 'e':
		a.handleEdit()
		return nil
	case 'k':
		a.handleKill()
		return nil
	case '/':
		a.root.ResizeItem(a.filterBar, 1, 0)
		a.tv.SetFocus(a.filterBar)
		return nil
	case 'f':
		if a.current == viewContainers {
			a.containersShowAll = !a.containersShowAll
			a.table.Clear() // force full rebuild so shrinking row count removes stale rows
			a.lastHintsText = ""
			if err := a.refreshCurrent(); err != nil {
				a.setStatus(fmt.Sprintf("[red]%v", err))
			}
		}
		return nil
	}

	return event
}

func (a *App) runCommand(cmdline string) {
	if cmdline == "" {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(cmdline), " ", 2)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "q", "quit", "exit":
		a.confirm("Quit ds9s?", func() { a.tv.Stop() })
		return
	case "alias", "aliases":
		a.showAliases()
		return
	case "context", "ctx":
		if arg == "" {
			a.showContexts()
		} else {
			a.switchContext(arg)
		}
		return
	}
	if vn, ok := aliases[cmd]; ok {
		a.switchView(vn)
		return
	}
	a.setStatus(fmt.Sprintf("[red]unknown command: %s  (try :alias for help)", cmd))
}

func (a *App) switchView(vn viewName) {
	a.current = vn
	// Clear any active text filter when switching views.
	a.filterText = ""
	a.filterBar.SetText("")
	a.root.ResizeItem(a.filterBar, 0, 0)
	a.lastHeaderText = ""
	// Full reset when switching views: clear the table so cells from the
	// previous view don't bleed through, and reset scroll to the top-left.
	a.table.Clear()
	a.table.SetOffset(0, 0)
	if err := a.refreshCurrent(); err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
	}
}

func (a *App) refreshHeader() {
	text := fmt.Sprintf(" [yellow::b]ds9s[-:-:-]  manager:[green]%s[-]  host:[grey]%s[-]  view:[aqua]%s[-]",
		a.conn.Manager.Name, a.conn.Manager.Host, a.current)
	if a.filterText != "" {
		text += fmt.Sprintf("  filter:[orange]%s[-]", tview.Escape(a.filterText))
	}
	if text != a.lastHeaderText {
		a.lastHeaderText = text
		a.header.SetText(text)
	}
	a.updateHints()
}

// updateHints refreshes the k9s-style shortcut bar below the table.
// Each hint is a colored chip: active key in teal, label in white.
func (a *App) updateHints() {
	chip := func(key, label string) string {
		return fmt.Sprintf("[black:teal:b] %s [-:-:-] [white]%s[-]", key, label)
	}
	// Common hints present on every view.
	common := "  " + chip("/", "FILTER") + "   " + chip("r", "REFRESH") + "   " + chip("?", "HELP") + "   " + chip("q", "QUIT")

	var extra string
	switch a.current {
	case viewServices:
		extra = chip("Enter", "LOGS") + "   " + chip("d", "DESCRIBE") + "   " +
			chip("e", "EDIT") + "   " + chip("l", "LOGS") + "   " +
			chip("s", "SCALE") + "   " + chip("u", "UPDATE") + "   " + chip("Ctrl-D", "DELETE")
	case viewContainers:
		filterLabel := "RUNNING"
		if a.containersShowAll {
			filterLabel = "ALL"
		}
		extra = chip("Enter", "LOGS") + "   " + chip("d", "DESCRIBE") + "   " +
			chip("s", "SHELL") + "   " + chip("k", "KILL") + "   " +
			chip("l", "LOGS") + "   " + chip("f", filterLabel)
	case viewStacks:
		extra = chip("Enter", "SERVICES") + "   " + chip("e", "EDIT") + "   " +
			chip("l", "LOGS") + "   " + chip("Ctrl-D", "DELETE")
	case viewNodes:
		extra = chip("Enter", "DESCRIBE") + "   " + chip("d", "DESCRIBE")
	case viewConfigs, viewSecrets:
		extra = chip("Enter", "DESCRIBE") + "   " + chip("d", "DESCRIBE") + "   " + chip("Ctrl-D", "DELETE")
	case viewVolumes:
		extra = chip("Enter", "DESCRIBE") + "   " + chip("d", "DESCRIBE") + "   " + chip("Ctrl-D", "DELETE")
	case viewNetworks:
		extra = chip("Enter", "DESCRIBE") + "   " + chip("d", "DESCRIBE") + "   " + chip("Ctrl-D", "DELETE")
	}

	var text string
	if extra != "" {
		text = "  " + extra + "   " + common[2:]
	} else {
		text = common
	}
	if text != a.lastHintsText {
		a.lastHintsText = text
		a.hints.SetText(text)
	}
}

func (a *App) setStatus(msg string) {
	a.status.SetText(" " + msg)
}

func (a *App) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// showInfoPage renders colored text (aliases, contexts…) in a fullscreen
// Esc-to-close page. Unlike showTextPage (which disables color tags to safely
// display raw JSON), this one enables tview color markup.
func (a *App) showInfoPage(pageName, title, text string) {
	view := tview.NewTextView().SetDynamicColors(true).SetText(text)
	view.SetBorder(true).SetTitle(title)
	view.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape || event.Key() == tcell.KeyEnter {
			a.pages.RemovePage(pageName)
			a.tv.SetFocus(a.table)
			return nil
		}
		return event
	})
	a.pages.AddPage(pageName, view, true, true)
	a.tv.SetFocus(view)
}

func (a *App) showAliases() {
	var sb strings.Builder
	sb.WriteString("[yellow::b]ds9s — help & aliases[-:-:-]\n\n")
	sb.WriteString("[aqua]Resource views (use with :)[-]\n")
	sb.WriteString("  :services    :svc  :svcs  :service\n")
	sb.WriteString("  :containers  :co   :ctr   :container  :ps\n")
	sb.WriteString("  :stacks      :stack  :stk\n")
	sb.WriteString("  :nodes       :node   :no\n")
	sb.WriteString("  :configs     :config :cm\n")
	sb.WriteString("  :secrets     :secret :sec\n")
	sb.WriteString("  :volumes     :volume :vol  :pv\n")
	sb.WriteString("  :networks    :network :net :ns\n")
	sb.WriteString("\n[aqua]Other commands[-]\n")
	sb.WriteString("  :alias  	:aliases     	— show this help\n")
	sb.WriteString("  :context 	:ctx			— switch to another manager\n")
	sb.WriteString("  :quit   	:q   :exit     	— quit\n")
	sb.WriteString("\n[aqua]Keyboard shortcuts (on the main table)[-]\n")
	sb.WriteString("  :        open command bar\n")
	sb.WriteString("  Enter    logs (services/containers) or drill-down (other)\n")
	sb.WriteString("  d        describe / JSON inspect\n")
	sb.WriteString("  e        edit service spec in $EDITOR (services, stacks)\n")
	sb.WriteString("  l        view logs (containers / services / stacks)\n")
	sb.WriteString("  s        scale replicas (services)  /  shell in container\n")
	sb.WriteString("  k        kill container with SIGKILL (containers view)\n")
	sb.WriteString("  u        force-update / rolling restart (services)\n")
	sb.WriteString("  Ctrl-D   delete selected resource (with confirmation)\n")
	sb.WriteString("  r        force refresh\n")
	sb.WriteString("  ?        this help\n")
	sb.WriteString("  q        quit\n")
	sb.WriteString("  Esc      close current overlay / go back\n")
	sb.WriteString("\nPress [Esc] or [Enter] to close.")
	a.showInfoPage("aliases", " Help & Aliases ", sb.String())
}

func (a *App) showContexts() {
	list := tview.NewList().ShowSecondaryText(true)
	for _, m := range a.cfg.Managers {
		m := m // capture for closure
		secondary := ""
		if m.Name == a.conn.Manager.Name {
			secondary = "(current)"
		}
		list.AddItem(m.Name, secondary, 0, func() {
			a.pages.RemovePage("contexts")
			a.tv.SetFocus(a.table)
			a.switchContext(m.Name)
		})
	}
	list.SetBorder(true).SetTitle(" Switch Context — ↑↓ navigate · Enter select · Esc cancel ")
	list.SetDoneFunc(func() {
		a.pages.RemovePage("contexts")
		a.tv.SetFocus(a.table)
	})
	a.pages.AddPage("contexts", list, true, true)
	a.tv.SetFocus(list)
}

// switchContext closes the current connection and opens a new one to the named
// manager. The Docker connection is established in a background goroutine so
// the UI stays responsive during the SSH handshake.
func (a *App) switchContext(managerName string) {
	mgr, err := a.cfg.ManagerByName(managerName)
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
		return
	}
	if managerName == a.conn.Manager.Name {
		a.setStatus(fmt.Sprintf("[yellow]already connected to %s", managerName))
		return
	}
	a.setStatus(fmt.Sprintf("Connecting to %s…", managerName))
	go func() {
		conn, err := dockerx.Connect(*mgr)
		a.tv.QueueUpdateDraw(func() {
			if err != nil {
				a.setStatus(fmt.Sprintf("[red]connecting to %s: %v", managerName, err))
				return
			}
			_ = a.conn.Close()
			a.conn = conn
			a.store = dockerx.NewStore(conn)
			a.cfg.Current = managerName
			a.table.Clear()
			a.table.SetOffset(0, 0)
			if e := a.refreshCurrent(); e != nil {
				a.setStatus(fmt.Sprintf("[red]%v", e))
				return
			}
			a.setStatus(fmt.Sprintf("Switched to [green]%s[-]", managerName))
		})
	}()
}
