package ui

import (
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/swarm"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"ds9s/internal/dockerx"
)

// rowMatchesFilter reports whether any of the given values contains the filter
// string (case-insensitive). An empty filter always matches.
func rowMatchesFilter(filter string, values ...string) bool {
	if filter == "" {
		return true
	}
	lower := strings.ToLower(filter)
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), lower) {
			return true
		}
	}
	return false
}

// rowMeta is stashed as the reference of column 0 of every row so action
// handlers (delete/logs/scale/describe) know what the selected row refers
// to without re-parsing displayed strings.
type rowMeta struct {
	kind        viewName
	id          string
	name        string
	serviceID   string // tasks: parent service ID (used for ServiceLogs)
	serviceName string // tasks: human-readable service name
	stackName   string // tasks: docker stack namespace (empty when not in a stack)
	containerID string // tasks: underlying container ID (used for ContainerRemove/kill)
	nodeName    string // tasks: Docker OS hostname of the node
	nodeAddr    string // tasks: Swarm advertise IP of the node — used as SSH target (more reliable than hostname)
}

func (a *App) refreshCurrent() error {
	// Save scroll/selection before refresh. We do NOT call table.Clear() on
	// auto-refresh — instead each refreshXxx updates cells in-place. This
	// keeps column widths stable (no recomputation from scratch) and prevents
	// tview's clampSelection from resetting the column offset to 0 during
	// the subsequent Draw() call.
	rowOff, colOff := a.table.GetOffset()
	selRow, selCol := a.table.GetSelection()
	prevRows := a.table.GetRowCount()

	a.refreshHeader()

	var err error
	switch a.current {
	case viewServices:
		err = a.refreshServices()
	case viewContainers:
		err = a.refreshContainers()
	case viewStacks:
		err = a.refreshStacks()
	case viewNodes:
		err = a.refreshNodes()
	case viewConfigs:
		err = a.refreshConfigs()
	case viewSecrets:
		err = a.refreshSecrets()
	case viewVolumes:
		err = a.refreshVolumes()
	case viewNetworks:
		err = a.refreshNetworks()
	default:
		err = fmt.Errorf("unknown view %s", a.current)
	}

	if err != nil {
		return err
	}

	// Blank out rows that are no longer in the dataset (table shrank).
	// We never call table.Clear() during auto-refresh, so we must erase
	// the leftover rows from the previous (larger) result set manually.
	newRows := a.table.GetRowCount()
	colCount := a.table.GetColumnCount()
	for row := newRows; row < prevRows; row++ {
		for col := 0; col < colCount; col++ {
			a.table.SetCell(row, col, tview.NewTableCell(""))
		}
	}

	// Restore scroll and selection. Call Select before SetOffset so that
	// tview's internal clampSelection (triggered by Select) runs first;
	// SetOffset then overrides the column offset unconditionally.
	rows := a.table.GetRowCount()
	if rows > 1 {
		if selRow < 1 || selRow >= rows {
			selRow = 1
		}
		a.table.Select(selRow, selCol)
		a.table.SetOffset(rowOff, colOff)
	}

	return nil
}

func setHeaderRow(table *tview.Table, cols ...string) {
	// Do NOT call table.Clear() here. Clearing the table on every auto-refresh
	// forces tview to recompute column widths from scratch, which shifts all
	// cell positions and causes the column-header row to flicker. It also resets
	// the column offset (via clampSelection in Draw), undoing any horizontal
	// scroll the user had. Instead we update the header cells in-place and let
	// refreshCurrent erase only the rows that are no longer in the dataset.
	for i, c := range cols {
		table.SetCell(0, i, tview.NewTableCell(c).
			SetSelectable(false).
			SetAttributes(1). // tcell.AttrBold = 1<<0 = 1 (NOT 1<<1 which is AttrBlink)
			SetTextColor(tview.Styles.SecondaryTextColor))
	}
}

func setCell(table *tview.Table, row, col int, text string, ref *rowMeta) {
	cell := tview.NewTableCell(text)
	if col == 0 && ref != nil {
		cell.SetReference(*ref)
	}
	table.SetCell(row, col, cell)
}

func (a *App) selectedRow() *rowMeta {
	row, _ := a.table.GetSelection()
	if row <= 0 {
		return nil
	}
	cell := a.table.GetCell(row, 0)
	if cell == nil {
		return nil
	}
	ref, ok := cell.GetReference().(rowMeta)
	if !ok {
		return nil
	}
	return &ref
}

// --- Services ----------------------------------------------------------------

func (a *App) refreshServices() error {
	ctx, cancel := a.ctx()
	defer cancel()
	services, err := a.store.Services(ctx)
	if err != nil {
		return err
	}

	setHeaderRow(a.table, "NAME", "MODE", "REPLICAS", "IMAGE", "STACK", "ID")
	row := 1
	for _, svc := range services {
		mode, replicas := serviceModeAndReplicas(svc)
		stack := svc.Spec.Labels["com.docker.stack.namespace"]
		image := ""
		if svc.Spec.TaskTemplate.ContainerSpec != nil {
			image = svc.Spec.TaskTemplate.ContainerSpec.Image
		}
		if !rowMatchesFilter(a.filterText, svc.Spec.Name, mode, image, stack) {
			continue
		}
		ref := &rowMeta{kind: viewServices, id: svc.ID, name: svc.Spec.Name}
		setCell(a.table, row, 0, svc.Spec.Name, ref)
		setCell(a.table, row, 1, mode, nil)
		setCell(a.table, row, 2, replicas, nil)
		setCell(a.table, row, 3, image, nil)
		setCell(a.table, row, 4, stack, nil)
		setCell(a.table, row, 5, dockerx.ShortID(svc.ID), nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d services", len(services)))
	return nil
}

func serviceModeAndReplicas(svc swarm.Service) (mode string, replicas string) {
	switch {
	case svc.Spec.Mode.Replicated != nil:
		desired := uint64(0)
		if svc.Spec.Mode.Replicated.Replicas != nil {
			desired = *svc.Spec.Mode.Replicated.Replicas
		}
		running := svc.ServiceStatus.RunningTasks
		return "replicated", fmt.Sprintf("%d/%d", running, desired)
	case svc.Spec.Mode.Global != nil:
		// DesiredTasks is computed by the Swarm manager as the number of
		// eligible nodes, so it works correctly without listing nodes manually.
		return "global", fmt.Sprintf("%d/%d", svc.ServiceStatus.RunningTasks, svc.ServiceStatus.DesiredTasks)
	default:
		return "unknown", "-"
	}
}

// --- Containers (Swarm tasks) -----------------------------------------------
// Uses TaskList (manager API) to show containers across ALL nodes, not just
// those running on the connected manager daemon.

// taskStateColor returns the row color for a given Swarm task state.
func taskStateColor(state string) tcell.Color {
	switch state {
	case "running":
		return tcell.ColorWhite
	case "starting", "preparing", "assigned", "accepted", "ready", "new", "pending":
		return tcell.ColorYellow
	case "failed", "rejected", "orphaned":
		return tcell.ColorRed
	default: // complete, shutdown, remove
		return tcell.ColorGray
	}
}

func (a *App) refreshContainers() error {
	ctx, cancel := a.ctx()
	defer cancel()
	tasks, err := a.store.AllTasks(ctx, !a.containersShowAll)
	if err != nil {
		return err
	}

	setHeaderRow(a.table, "NAME", "SERVICE", "NODE", "STATE", "IMAGE", "ID")
	row := 1
	for _, t := range tasks {
		if !rowMatchesFilter(a.filterText, t.Name, t.ServiceName, t.NodeName, t.State, t.Image) {
			continue
		}
		ref := &rowMeta{
			kind:        viewContainers,
			id:          t.ID,
			name:        t.Name,
			serviceID:   t.ServiceID,
			serviceName: t.ServiceName,
			stackName:   t.StackName,
			containerID: t.ContainerID,
			nodeName:    t.NodeName,
			nodeAddr:    t.NodeAddr,
		}
		color := taskStateColor(t.State)
		values := []string{t.Name, t.ServiceName, t.NodeName, t.State, t.Image, dockerx.ShortID(t.ID)}
		for col, v := range values {
			cell := tview.NewTableCell(v).SetTextColor(color)
			if col == 0 {
				cell.SetReference(*ref)
			}
			a.table.SetCell(row, col, cell)
		}
		row++
	}
	filterNote := "running only"
	if a.containersShowAll {
		filterNote = "running + stopped"
	}
	a.setStatus(fmt.Sprintf("%d tasks (cluster-wide, %s)", len(tasks), filterNote))
	return nil
}

// --- Stacks ----------------------------------------------------------------

func (a *App) refreshStacks() error {
	ctx, cancel := a.ctx()
	defer cancel()
	stacks, err := a.store.Stacks(ctx)
	if err != nil {
		return err
	}

	// SERVICES = "ok/total" (services where running >= desired)
	// REPLICAS = "running/desired" (aggregate across all services in the stack)
	setHeaderRow(a.table, "NAME", "SERVICES", "REPLICAS")
	row := 1
	for _, st := range stacks {
		if !rowMatchesFilter(a.filterText, st.Name) {
			continue
		}
		ref := &rowMeta{kind: viewStacks, id: st.Name, name: st.Name}
		setCell(a.table, row, 0, st.Name, ref)
		setCell(a.table, row, 1, fmt.Sprintf("%d/%d", st.ServicesOK, st.Services), nil)
		setCell(a.table, row, 2, fmt.Sprintf("%d/%d", st.ReplicasRunning, st.ReplicasDesired), nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d stacks", len(stacks)))
	return nil
}

// --- Nodes ----------------------------------------------------------------

func (a *App) refreshNodes() error {
	ctx, cancel := a.ctx()
	defer cancel()
	nodes, err := a.store.Nodes(ctx)
	if err != nil {
		return err
	}

	setHeaderRow(a.table, "HOSTNAME", "ROLE", "AVAILABILITY", "STATUS", "ENGINE", "ID")
	row := 1
	for _, n := range nodes {
		role := string(n.Spec.Role)
		if n.ManagerStatus != nil && n.ManagerStatus.Leader {
			role = "leader"
		}
		if !rowMatchesFilter(a.filterText, n.Description.Hostname, role, string(n.Spec.Availability), string(n.Status.State)) {
			continue
		}
		ref := &rowMeta{kind: viewNodes, id: n.ID, name: n.Description.Hostname}
		setCell(a.table, row, 0, n.Description.Hostname, ref)
		setCell(a.table, row, 1, role, nil)
		setCell(a.table, row, 2, string(n.Spec.Availability), nil)
		setCell(a.table, row, 3, string(n.Status.State), nil)
		setCell(a.table, row, 4, n.Description.Engine.EngineVersion, nil)
		setCell(a.table, row, 5, dockerx.ShortID(n.ID), nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d nodes", len(nodes)))
	return nil
}

// --- Configs ----------------------------------------------------------------

func (a *App) refreshConfigs() error {
	ctx, cancel := a.ctx()
	defer cancel()
	configs, err := a.store.Configs(ctx)
	if err != nil {
		return err
	}

	setHeaderRow(a.table, "NAME", "CREATED", "UPDATED", "ID")
	row := 1
	for _, c := range configs {
		if !rowMatchesFilter(a.filterText, c.Spec.Name) {
			continue
		}
		ref := &rowMeta{kind: viewConfigs, id: c.ID, name: c.Spec.Name}
		setCell(a.table, row, 0, c.Spec.Name, ref)
		setCell(a.table, row, 1, c.CreatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 2, c.UpdatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 3, dockerx.ShortID(c.ID), nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d configs", len(configs)))
	return nil
}

// --- Volumes ----------------------------------------------------------------

func (a *App) refreshVolumes() error {
	ctx, cancel := a.ctx()
	defer cancel()

	// With SSH config: fetch volumes from every node in parallel via SSH.
	if a.conn.Manager.SSH != nil {
		vols, errs := a.store.AllNodeVolumes(ctx)
		setHeaderRow(a.table, "NODE", "NAME", "DRIVER", "SCOPE", "MOUNTPOINT")
		row := 1
		for _, v := range vols {
			if !rowMatchesFilter(a.filterText, v.NodeName, v.VolumeName, v.Driver) {
				continue
			}
			ref := &rowMeta{kind: viewVolumes, id: v.NodeAddr + "/" + v.VolumeName, name: v.VolumeName}
			setCell(a.table, row, 0, v.NodeName, ref)
			setCell(a.table, row, 1, v.VolumeName, nil)
			setCell(a.table, row, 2, v.Driver, nil)
			setCell(a.table, row, 3, v.Scope, nil)
			mp := v.Mountpoint
			if len(mp) > 50 {
				mp = "…" + mp[len(mp)-49:]
			}
			setCell(a.table, row, 4, mp, nil)
			row++
		}
		nodeList := make([]string, len(vols))
		for i, v := range vols {
			nodeList[i] = v.NodeName
		}
		status := fmt.Sprintf("%d volumes across %d nodes", len(vols), countUniqueNodes(nodeList))
		if len(errs) > 0 {
			status += fmt.Sprintf("  [yellow](%d nodes unreachable)", len(errs))
		}
		a.setStatus(status)
		return nil
	}

	// No SSH: show manager volumes only.
	vols, err := a.store.Volumes(ctx)
	if err != nil {
		return err
	}
	setHeaderRow(a.table, "NODE", "NAME", "DRIVER", "SCOPE", "MOUNTPOINT")
	row := 1
	for _, v := range vols {
		if !rowMatchesFilter(a.filterText, a.conn.Manager.Name, v.Name, v.Driver) {
			continue
		}
		ref := &rowMeta{kind: viewVolumes, id: v.Name, name: v.Name}
		setCell(a.table, row, 0, a.conn.Manager.Name, ref)
		setCell(a.table, row, 1, v.Name, nil)
		setCell(a.table, row, 2, v.Driver, nil)
		setCell(a.table, row, 3, v.Scope, nil)
		mp := v.Mountpoint
		if len(mp) > 50 {
			mp = "…" + mp[len(mp)-49:]
		}
		setCell(a.table, row, 4, mp, nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d volumes (manager node only — no SSH config)", len(vols)))
	return nil
}

func countUniqueNodes(nodeNames []string) int {
	s := map[string]struct{}{}
	for _, n := range nodeNames {
		s[n] = struct{}{}
	}
	return len(s)
}

// --- Networks ---------------------------------------------------------------

func (a *App) refreshNetworks() error {
	ctx, cancel := a.ctx()
	defer cancel()

	if a.conn.Manager.SSH != nil {
		nets, errs := a.store.AllNodeNetworks(ctx)
		setHeaderRow(a.table, "NODE", "NAME", "DRIVER", "SCOPE", "ID")
		row := 1
		for _, n := range nets {
			if !rowMatchesFilter(a.filterText, n.NodeName, n.Name, n.Driver, n.Scope) {
				continue
			}
			ref := &rowMeta{kind: viewNetworks, id: n.ID, name: n.Name}
			setCell(a.table, row, 0, n.NodeName, ref)
			setCell(a.table, row, 1, n.Name, nil)
			setCell(a.table, row, 2, n.Driver, nil)
			setCell(a.table, row, 3, n.Scope, nil)
			setCell(a.table, row, 4, dockerx.ShortID(n.ID), nil)
			row++
		}
		status := fmt.Sprintf("%d networks", len(nets))
		if len(errs) > 0 {
			status += fmt.Sprintf("  [yellow](%d nodes unreachable)", len(errs))
		}
		a.setStatus(status)
		return nil
	}

	// No SSH: show manager networks (includes cluster-wide overlay networks).
	nets, err := a.store.Networks(ctx)
	if err != nil {
		return err
	}
	setHeaderRow(a.table, "NODE", "NAME", "DRIVER", "SCOPE", "ID")
	row := 1
	for _, n := range nets {
		if !rowMatchesFilter(a.filterText, n.Name, n.Driver, n.Scope) {
			continue
		}
		ref := &rowMeta{kind: viewNetworks, id: n.ID, name: n.Name}
		setCell(a.table, row, 0, "(cluster)", ref)
		setCell(a.table, row, 1, n.Name, nil)
		setCell(a.table, row, 2, n.Driver, nil)
		setCell(a.table, row, 3, n.Scope, nil)
		setCell(a.table, row, 4, dockerx.ShortID(n.ID), nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d networks (manager node — no SSH config)", len(nets)))
	return nil
}

// --- Secrets ----------------------------------------------------------------

func (a *App) refreshSecrets() error {
	ctx, cancel := a.ctx()
	defer cancel()
	secrets, err := a.store.Secrets(ctx)
	if err != nil {
		return err
	}

	setHeaderRow(a.table, "NAME", "CREATED", "UPDATED", "ID")
	row := 1
	for _, sec := range secrets {
		if !rowMatchesFilter(a.filterText, sec.Spec.Name) {
			continue
		}
		ref := &rowMeta{kind: viewSecrets, id: sec.ID, name: sec.Spec.Name}
		setCell(a.table, row, 0, sec.Spec.Name, ref)
		setCell(a.table, row, 1, sec.CreatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 2, sec.UpdatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 3, dockerx.ShortID(sec.ID), nil)
		row++
	}
	a.setStatus(fmt.Sprintf("%d secrets", len(secrets)))
	return nil
}
