// Package ui implements the ds9s terminal UI on top of tview/tcell,
// deliberately mirroring k9s' interaction model: a ':' command bar to switch
// resource views, a table per resource, and a handful of resource-specific
// key bindings (logs, scale, delete...).
package ui

import (
	"context"
	"fmt"
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
)

// aliases lets users type short forms in the command bar, k9s-style.
var aliases = map[string]viewName{
	"svc": viewServices, "svcs": viewServices, "service": viewServices, "services": viewServices,
	"co": viewContainers, "ctr": viewContainers, "container": viewContainers, "containers": viewContainers, "ps": viewContainers,
	"stack": viewStacks, "stacks": viewStacks, "stk": viewStacks,
	"node": viewNodes, "nodes": viewNodes, "no": viewNodes,
	"config": viewConfigs, "configs": viewConfigs, "cm": viewConfigs,
	"secret": viewSecrets, "secrets": viewSecrets, "sec": viewSecrets,
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
	cmdInput *tview.InputField
	status   *tview.TextView

	current  viewName
	stopPoll chan struct{}
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
		current: viewServices,
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
		a.tv.SetFocus(a.table)
	})

	a.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.header, 1, 0, false).
		AddItem(a.table, 0, 1, true).
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
	// Don't intercept keys while the command bar or a modal has focus.
	if a.tv.GetFocus() == a.cmdInput {
		return event
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
	}

	switch event.Key() {
	case tcell.KeyCtrlD:
		a.handleDelete()
		return nil
	case tcell.KeyEnter:
		a.handleEnter()
		return nil
	case tcell.KeyEscape:
		a.tv.SetFocus(a.table)
		return nil
	}

	switch event.Rune() {
	case 'l':
		a.handleLogs()
		return nil
	case 's':
		a.handleScale()
		return nil
	case 'd':
		a.handleDescribe()
		return nil
	case 'u':
		a.handleForceUpdate()
		return nil
	}

	return event
}

func (a *App) runCommand(cmdline string) {
	if cmdline == "" {
		return
	}
	switch cmdline {
	case "q", "quit", "exit":
		a.confirm("Quit ds9s?", func() { a.tv.Stop() })
		return
	}
	if vn, ok := aliases[cmdline]; ok {
		a.switchView(vn)
		return
	}
	a.setStatus(fmt.Sprintf("[red]unknown command/resource: %s", cmdline))
}

func (a *App) switchView(vn viewName) {
	a.current = vn
	if err := a.refreshCurrent(); err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
	}
}

func (a *App) refreshHeader() {
	a.header.SetText(fmt.Sprintf(" [yellow::b]ds9s[-:-:-]  manager:[green]%s[-]  host:[grey]%s[-]  view:[aqua]%s[-]",
		a.conn.Manager.Name, a.conn.Manager.Host, a.current))
}

func (a *App) setStatus(msg string) {
	a.status.SetText(" " + msg)
}

func (a *App) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
