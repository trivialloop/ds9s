package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"ds9s/internal/dockerx"
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
		case viewVolumes:
			err = a.store.RemoveVolume(ctx, ref.id, false)
		case viewNetworks:
			err = a.store.RemoveNetwork(ctx, ref.id)
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
	case viewVolumes:
		data, _, err = a.conn.Client.VolumeInspectWithRaw(ctx, ref.id)
	case viewNetworks:
		data, _, err = a.conn.Client.NetworkInspectWithRaw(ctx, ref.id, types.NetworkInspectOptions{})
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
		a.switchView(viewServices)
	case viewServices, viewContainers:
		a.handleLogs()
	default:
		a.handleDescribe()
	}
}

// --- Edit (service spec / stack sub-menu) ------------------------------------

func (a *App) handleEdit() {
	ref := a.selectedRow()
	if ref == nil {
		return
	}
	switch ref.kind {
	case viewServices:
		a.editService(ref.id, ref.name)
	case viewStacks:
		a.showStackEditMenu(ref.name)
	default:
		a.setStatus(fmt.Sprintf("[yellow]edit (e) not supported for %s", ref.kind))
	}
}

// editService opens the service spec JSON in $EDITOR (suspending the TUI),
// then applies the edited spec back to the Swarm manager on save.
func (a *App) editService(serviceID, serviceName string) {
	ctx, cancel := a.ctx()
	svc, err := a.store.ServiceSpec(ctx, serviceID)
	cancel()
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]inspect service: %v", err))
		return
	}

	specJSON, err := json.MarshalIndent(svc.Spec, "", "  ")
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]marshal spec: %v", err))
		return
	}

	tmpFile, err := os.CreateTemp("", "ds9s-edit-*.json")
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]create temp file: %v", err))
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(specJSON); err != nil {
		tmpFile.Close()
		a.setStatus(fmt.Sprintf("[red]write temp file: %v", err))
		return
	}
	tmpFile.Close()

	original := string(specJSON)

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}

	var editErr error
	a.tv.Suspend(func() {
		cmd := exec.Command(editor, tmpPath)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		editErr = cmd.Run()
	})
	if editErr != nil {
		a.setStatus(fmt.Sprintf("[red]editor exited with error: %v", editErr))
		return
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]reading edited file: %v", err))
		return
	}
	if string(edited) == original {
		a.setStatus("[yellow]no changes — service not updated")
		return
	}

	var newSpec swarm.ServiceSpec
	if err := json.Unmarshal(edited, &newSpec); err != nil {
		a.setStatus(fmt.Sprintf("[red]invalid JSON after edit: %v", err))
		return
	}

	ctx2, cancel2 := a.ctx()
	err = a.store.ServiceUpdateSpec(ctx2, serviceID, svc.Version, newSpec)
	cancel2()
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]update failed: %v", err))
		return
	}
	a.setStatus(fmt.Sprintf("[green]service %s updated", serviceName))
	_ = a.refreshCurrent()
}

// showStackEditMenu presents a list of services in the stack; the selected
// service is then opened in editService.
func (a *App) showStackEditMenu(stackName string) {
	ctx, cancel := a.ctx()
	services, err := a.store.ServicesInStack(ctx, stackName)
	cancel()
	if err != nil {
		a.setStatus(fmt.Sprintf("[red]%v", err))
		return
	}
	if len(services) == 0 {
		a.setStatus(fmt.Sprintf("[yellow]no services in stack %s", stackName))
		return
	}

	list := tview.NewList().ShowSecondaryText(false)
	for _, svc := range services {
		svc := svc
		list.AddItem(svc.Spec.Name, "", 0, func() {
			a.pages.RemovePage("stack-edit")
			a.tv.SetFocus(a.table)
			a.editService(svc.ID, svc.Spec.Name)
		})
	}
	list.SetBorder(true).SetTitle(fmt.Sprintf(" Edit service in [%s] — ↑↓ navigate · Enter select · Esc cancel ", stackName))
	list.SetDoneFunc(func() {
		a.pages.RemovePage("stack-edit")
		a.tv.SetFocus(a.table)
	})
	a.pages.AddPage("stack-edit", list, true, true)
	a.tv.SetFocus(list)
}

// --- Shell exec (s on containers view) ---------------------------------------

// handleShell opens an interactive /bin/sh inside the selected container.
// For SSH-connected managers the shell is opened via Go crypto/ssh directly
// (no system ssh binary — no agent key flooding, proper PTY raw mode).
// The TUI is suspended while the shell session is active.
func (a *App) handleShell() {
	ref := a.selectedRow()
	if ref == nil || ref.kind != viewContainers {
		return
	}
	if ref.containerID == "" {
		a.setStatus("[red]container ID not available (task not yet started)")
		return
	}

	// Use nodeAddr (Swarm advertise IP) as the SSH target — more reliable than
	// the OS hostname which may not be resolvable from outside the cluster.
	sshTarget := ref.nodeAddr
	if sshTarget == "" {
		sshTarget = ref.nodeName
	}

	a.setStatus(fmt.Sprintf("Opening shell in %s on %s…", ref.containerID[:min(12, len(ref.containerID))], sshTarget))

	var shellErr error
	a.tv.Suspend(func() {
		// Clear the screen so the shell starts on a clean terminal instead of
		// showing whatever was behind the ds9s TUI.
		fmt.Print("\033[H\033[2J\033[3J")

		if a.conn.Manager.SSH != nil {
			shellErr = dockerx.ShellInContainer(*a.conn.Manager.SSH, sshTarget, ref.containerID)
		} else {
			// Local docker exec (manager socket, no SSH).
			cmd := exec.Command("docker", "exec", "-it", ref.containerID, "/bin/sh")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if err != nil && err.Error() != "exit status 1" && err.Error() != "exit status 130" {
				shellErr = err
			}
		}
	})
	if shellErr != nil {
		a.setStatus(fmt.Sprintf("[yellow]shell exited: %v", shellErr))
	} else {
		a.setStatus("Shell session ended.")
	}
}

// --- Kill container (k on containers view) -----------------------------------

func (a *App) handleKill() {
	ref := a.selectedRow()
	if ref == nil || ref.kind != viewContainers {
		return
	}
	if ref.containerID == "" {
		a.setStatus("[red]container ID not available")
		return
	}
	// Use Swarm advertise IP (nodeAddr) — more reliable for SSH than OS hostname.
	sshTarget := ref.nodeAddr
	if sshTarget == "" {
		sshTarget = ref.nodeName
	}
	a.confirm(fmt.Sprintf("Kill container %q with SIGKILL?\n(Swarm will restart it automatically)", ref.name), func() {
		a.setStatus(fmt.Sprintf("Killing %s on %s…", ref.containerID[:min(12, len(ref.containerID))], sshTarget))
		go a.doKill(ref.containerID, sshTarget, ref.name)
	})
}

func (a *App) doKill(containerID, sshTarget, displayName string) {
	var killErr error

	if a.conn.Manager.SSH != nil && sshTarget != "" {
		remoteKill := "docker kill " + containerID
		if a.conn.Manager.SSH.Sudo {
			remoteKill = "sudo -n " + remoteKill
		}
		_, killErr = dockerx.RunCommandOnNode(*a.conn.Manager.SSH, sshTarget, remoteKill)
	} else {
		// Local Docker API — works only if container is on the manager node.
		ctx, cancel := a.ctx()
		defer cancel()
		killErr = a.store.KillContainer(ctx, containerID)
	}

	a.tv.QueueUpdateDraw(func() {
		if killErr != nil {
			// Show the error in a dismissible page — it would be overwritten by the
			// auto-refresh status if we only used setStatus().
			msg := fmt.Sprintf("[red]Kill failed for %s on %s[-]\n\n%v\n\n[grey]Press Esc or Enter to dismiss[-]",
				displayName, sshTarget, killErr)
			a.showInfoPage("kill-error", " Kill Error ", msg)
		} else {
			a.setStatus(fmt.Sprintf("[green]killed %s", displayName))
		}
	})
}
