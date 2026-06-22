package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// confirm shows a yes/no modal and runs onYes if the user confirms.
func (a *App) confirm(question string, onYes func()) {
	modal := tview.NewModal().
		SetText(question).
		AddButtons([]string{"Cancel", "Confirm"}).
		SetDoneFunc(func(idx int, label string) {
			a.pages.RemovePage("confirm")
			a.tv.SetFocus(a.table)
			if label == "Confirm" {
				onYes()
			}
		})
	a.pages.AddPage("confirm", modal, true, true)
	a.tv.SetFocus(modal)
}

// prompt shows a single-line text input modal and calls onSubmit with the
// entered text if the user presses Enter (Escape cancels).
func (a *App) prompt(label, defaultValue string, onSubmit func(value string)) {
	input := tview.NewInputField().SetLabel(label).SetText(defaultValue)
	frame := tview.NewFrame(input).SetBorders(0, 0, 1, 1, 1, 1)
	frame.SetBorder(true).SetTitle(" ds9s ")

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(frame, 60, 0, true).
			AddItem(nil, 0, 1, false), 5, 0, true).
		AddItem(nil, 0, 1, false)

	input.SetDoneFunc(func(key tcell.Key) {
		a.pages.RemovePage("prompt")
		a.tv.SetFocus(a.table)
		if key == tcell.KeyEnter {
			onSubmit(input.GetText())
		}
	})

	a.pages.AddPage("prompt", flex, true, true)
	a.tv.SetFocus(input)
}

func (a *App) showHelp() {
	help := tview.NewTextView().SetDynamicColors(true)
	help.SetText(`[yellow::b]ds9s - help[-:-:-]

[aqua]Command bar (press : to open)[-]
  :services, :svc        switch to services view
  :containers, :co, :ps  switch to containers view
  :stacks                switch to stacks view
  :nodes                 switch to nodes view
  :configs               switch to configs view
  :secrets               switch to secrets view
  :alias, :aliases       list all commands and aliases
  :context               list configured managers
  :context <name>        switch to another manager
  :quit / :q             quit ds9s

[aqua]Global keys[-]
  :        open the command bar
  r        force refresh
  Enter    describe selected resource (JSON)
  d        describe (JSON)
  l        view logs (containers/services/stacks)
  s        scale a service
  u        force-update (rolling restart) a service
  Ctrl-D   delete the selected resource (with confirmation)
  ?        this help
  q        quit
  Esc      close overlay / go back

Press Esc or Enter to close this help.`)
	help.SetBorder(true).SetTitle(" Help ")
	help.SetDoneFunc(func(key tcell.Key) { a.pages.RemovePage("help"); a.tv.SetFocus(a.table) })
	help.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape || event.Key() == tcell.KeyEnter {
			a.pages.RemovePage("help")
			a.tv.SetFocus(a.table)
			return nil
		}
		return event
	})
	a.pages.AddPage("help", help, true, true)
	a.tv.SetFocus(help)
}

// --- Delete ------------------------------------------------------------------

func (a *App) handleDelete() {
	ref := a.selectedRow()
	if ref == nil {
		return
	}
	a.confirm(fmt.Sprintf("Delete %s %q?", ref.kind, ref.name), func() {
		ctx, cancel := a.ctx()
		defer cancel()
		var err error
		switch ref.kind {
		case viewServices:
			err = a.store.RemoveService(ctx, ref.id)
		case viewContainers:
			// ref.containerID is the actual container ID; ref.id is the task ID.
			// Removal only succeeds if the container runs on the connected manager.
			if ref.containerID == "" {
				err = fmt.Errorf("container ID not available (task not yet running)")
			} else {
				err = a.store.RemoveContainer(ctx, ref.containerID, true)
			}
		case viewStacks:
			if errs := a.store.RemoveStack(ctx, ref.id); len(errs) > 0 {
				err = errs[0]
			}
		case viewConfigs:
			err = a.store.RemoveConfig(ctx, ref.id)
		case viewSecrets:
			err = a.store.RemoveSecret(ctx, ref.id)
		default:
			err = fmt.Errorf("delete not supported for %s", ref.kind)
		}
		if err != nil {
			a.setStatus(fmt.Sprintf("[red]%v", err))
			return
		}
		a.setStatus(fmt.Sprintf("Deleted %s %q", ref.kind, ref.name))
		if e := a.refreshCurrent(); e != nil {
			a.setStatus(fmt.Sprintf("[red]%v", e))
		}
	})
}

// --- Scale ---------------------------------------------------------------------

func (a *App) handleScale() {
	ref := a.selectedRow()
	if ref == nil || ref.kind != viewServices {
		a.setStatus("[yellow]scale (s) only applies to services")
		return
	}
	a.prompt(fmt.Sprintf("Replicas for %s: ", ref.name), "", func(value string) {
		replicas, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			a.setStatus(fmt.Sprintf("[red]invalid replica count %q", value))
			return
		}
		ctx, cancel := a.ctx()
		defer cancel()
		if err := a.store.ScaleService(ctx, ref.id, replicas); err != nil {
			a.setStatus(fmt.Sprintf("[red]%v", err))
			return
		}
		a.setStatus(fmt.Sprintf("Scaled %s to %d replicas", ref.name, replicas))
		if e := a.refreshCurrent(); e != nil {
			a.setStatus(fmt.Sprintf("[red]%v", e))
		}
	})
}

// --- Force update (restart) -----------------------------------------------------

func (a *App) handleForceUpdate() {
	ref := a.selectedRow()
	if ref == nil || ref.kind != viewServices {
		return
	}
	a.confirm(fmt.Sprintf("Force-update (restart) service %q?", ref.name), func() {
		ctx, cancel := a.ctx()
		defer cancel()
		if err := a.store.ForceUpdateService(ctx, ref.id); err != nil {
			a.setStatus(fmt.Sprintf("[red]%v", err))
			return
		}
		a.setStatus(fmt.Sprintf("Force-update triggered for %s", ref.name))
	})
}

// --- Describe (inspect) ----------------------------------------------------------

func (a *App) handleDescribe() {
	ref := a.selectedRow()
	if ref == nil {
		return
	}
	ctx, cancel := a.ctx()
	defer cancel()

	var (
		data interface{}
		err  error
	)
	switch ref.kind {
	case viewServices:
		data, _, err = a.conn.Client.ServiceInspectWithRaw(ctx, ref.id, types.ServiceInspectOptions{})
	case viewContainers:
		data, _, err = a.conn.Client.TaskInspectWithRaw(ctx, ref.id)
	case viewNodes:
		data, _, err = a.conn.Client.NodeInspectWithRaw(ctx, ref.id)
	case viewConfigs:
		data, _, err = a.conn.Client.ConfigInspectWithRaw(ctx, ref.id)
	case viewSecrets:
		// Secret payload is never returned by the API; only metadata is shown.
		data, _, err = a.conn.Client.SecretInspectWithRaw(ctx, ref.id)
	case viewStacks:
		a.setStatus("[yellow]describe a service within the stack instead")
		return
	}
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
		return
	}

	pretty, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
		return
	}
	text := string(pretty)
	if ref.kind == viewSecrets {
		// Docker secrets are write-only by design: the payload is encrypted at
		// rest and never returned by the API, even to admins. Only metadata
		// (name, labels, timestamps) is accessible.
		text += "\n\n// Note: the secret VALUE is not retrievable via the Docker API.\n// It is encrypted at rest and exposed only inside authorised containers\n// (mounted at /run/secrets/<name>)."
	}
	a.showTextPage("describe", fmt.Sprintf(" %s: %s ", ref.kind, ref.name), text)
}

// --- Logs ------------------------------------------------------------------------

func (a *App) handleLogs() {
	ref := a.selectedRow()
	if ref == nil {
		return
	}

	switch ref.kind {
	case viewContainers:
		// Tasks view: stream via the parent service (works across all nodes).
		if ref.serviceID == "" {
			a.setStatus("[yellow]no service associated with this task")
			return
		}
		a.streamServiceLogs(ref.name, []string{ref.serviceID})
	case viewServices:
		// Use ServiceLogs: the manager aggregates logs from all task replicas,
		// even those running on worker nodes the client cannot reach directly.
		a.streamServiceLogs(ref.name, []string{ref.id})
	case viewStacks:
		ctx, cancel := a.ctx()
		defer cancel()
		services, err := a.store.ServicesInStack(ctx, ref.id)
		if err != nil {
			a.setStatus(fmt.Sprintf("[red]%v", err))
			return
		}
		if len(services) == 0 {
			a.setStatus("[yellow]no services for this stack")
			return
		}
		ids := make([]string, len(services))
		for i, svc := range services {
			ids[i] = svc.ID
		}
		a.streamServiceLogs(ref.name, ids)
	default:
		a.setStatus("[yellow]logs (l) only applies to containers/services/stacks")
	}
}

// openLogView creates the fullscreen log view: a bordered Flex containing the
// log TextView and a one-line shortcut bar (k9s-style). Default: FOLLOW ON
// (auto-scroll to newest line). Keys: f=toggle follow, w=toggle wrap, Esc=close.
//
// NOTE: never call a.tv.Draw() from inside InputCapture — that runs on the
// tview event goroutine which already holds the screen lock, causing a
// deadlock. tview redraws automatically after every InputCapture return.
func (a *App) openLogView(title string) (*tview.TextView, context.Context, context.CancelFunc) {
	follow := true // accessed only on tview main goroutine (closures below)
	wrap := false  // wrap off by default so long log lines are not broken

	view := tview.NewTextView().SetDynamicColors(false).SetMaxLines(10000)
	view.SetWrap(wrap)
	view.SetBorder(false)

	bar := tview.NewTextView().SetDynamicColors(true)

	renderBar := func() {
		fFg, fBg := "white", "teal"
		if !follow {
			fFg, fBg = "white", "grey"
		}
		wFg, wBg := "white", "grey"
		if wrap {
			wFg, wBg = "white", "teal"
		}
		bar.SetText(fmt.Sprintf(
			"  [%s:%s:b] f [-:-:-] [white]%-6s[-]   [%s:%s:b] w [-:-:-] [white]%-4s[-]   [white:grey:b] Esc [-:-:-] [white]CLOSE[-]",
			fFg, fBg, "FOLLOW",
			wFg, wBg, "WRAP",
		))
	}
	renderBar()

	outer := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(view, 0, 1, true).
		AddItem(bar, 1, 0, false)
	outer.SetBorder(true).SetTitle(fmt.Sprintf(" logs: %s ", title))

	// SetChangedFunc runs on the main goroutine (inside QueueUpdateDraw).
	// Calling ScrollToEnd here is safe; calling Draw here is also safe because
	// we are in a queued update, not inside an active draw lock.
	view.SetChangedFunc(func() {
		if follow {
			view.ScrollToEnd()
		}
		a.tv.Draw()
	})

	ctx, cancel := context.WithCancel(context.Background())
	view.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			cancel()
			a.pages.RemovePage("logs")
			a.tv.SetFocus(a.table)
			return nil
		}
		switch event.Rune() {
		case 'f', 'F':
			follow = !follow
			if follow {
				view.ScrollToEnd()
			}
			renderBar()
			// Return nil — tview redraws after InputCapture; do NOT call Draw() here.
			return nil
		case 'w', 'W':
			wrap = !wrap
			view.SetWrap(wrap)
			renderBar()
			return nil
		}
		return event
	})

	a.pages.AddPage("logs", outer, true, true)
	a.tv.SetFocus(view)
	return view, ctx, cancel
}

// streamContainerLogs tails logs from one or more local containers, prefixing
// each line with the short container ID when aggregating more than one.
func (a *App) streamContainerLogs(title string, containerIDs []string) {
	view, ctx, _ := a.openLogView(title)
	multiplex := len(containerIDs) > 1
	for _, id := range containerIDs {
		id := id
		go a.tailOne(ctx, view, id, multiplex)
	}
}

// streamServiceLogs tails logs via the Swarm ServiceLogs API (manager-side
// aggregation), so replicas on worker nodes are included.
func (a *App) streamServiceLogs(title string, serviceIDs []string) {
	view, ctx, _ := a.openLogView(title)
	multiplex := len(serviceIDs) > 1
	for _, id := range serviceIDs {
		id := id
		go a.tailOneService(ctx, view, id, multiplex)
	}
}

func (a *App) tailOne(ctx context.Context, view *tview.TextView, containerID string, prefixSource bool) {
	rc, err := a.store.ContainerLogs(ctx, containerID, true)
	if err != nil {
		a.tv.QueueUpdateDraw(func() { fmt.Fprintf(view, "[error opening logs for %s: %v]\n", containerID, err) })
		return
	}
	defer rc.Close()

	prefix := ""
	if prefixSource {
		prefix = containerID[:min(12, len(containerID))] + " | "
	}

	// Docker multiplexes stdout/stderr with an 8-byte header per frame when
	// the container was started without a TTY; stdcopy understands both
	// framed and raw streams transparently is not guaranteed, so we try a
	// scanner first and fall back gracefully for already-demultiplexed data.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		_, _ = stdcopy.StdCopy(pw, pw, rc)
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		select {
		case <-ctx.Done():
			return
		default:
		}
		a.tv.QueueUpdateDraw(func() { fmt.Fprintf(view, "%s%s\n", prefix, line) })
	}
}

func (a *App) tailOneService(ctx context.Context, view *tview.TextView, serviceID string, prefixSource bool) {
	rc, err := a.store.ServiceLogs(ctx, serviceID, true)
	if err != nil {
		a.tv.QueueUpdateDraw(func() {
			fmt.Fprintf(view, "[error opening logs for service %s: %v]\n", serviceID, err)
		})
		return
	}
	defer rc.Close()

	prefix := ""
	if prefixSource {
		prefix = serviceID[:min(12, len(serviceID))] + " | "
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		_, _ = stdcopy.StdCopy(pw, pw, rc)
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		select {
		case <-ctx.Done():
			return
		default:
		}
		a.tv.QueueUpdateDraw(func() { fmt.Fprintf(view, "%s%s\n", prefix, line) })
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// showTextPage renders arbitrary text (used for describe/inspect output) in
// a fullscreen, Esc-to-close page.
func (a *App) showTextPage(pageName, title, text string) {
	view := tview.NewTextView().SetDynamicColors(false).SetText(text)
	view.SetBorder(true).SetTitle(title)
	view.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			a.pages.RemovePage(pageName)
			a.tv.SetFocus(a.table)
			return nil
		}
		return event
	})
	a.pages.AddPage(pageName, view, true, true)
	a.tv.SetFocus(view)
}

// --- Enter (drill-down) -----------------------------------------------------------

func (a *App) handleEnter() {
	ref := a.selectedRow()
	if ref == nil {
		return
	}
	switch ref.kind {
	case viewStacks:
		// Drill into services filtered by this stack.
		a.switchView(viewServices)
	case viewServices, viewContainers:
		// Default action for services/tasks is to show logs.
		a.handleLogs()
	default:
		a.handleDescribe()
	}
}
