package ui

import (
	"fmt"

	"github.com/docker/docker/api/types/swarm"
	"github.com/rivo/tview"

	"ds9s/internal/dockerx"
)

// rowMeta is stashed as the reference of column 0 of every row so action
// handlers (delete/logs/scale/describe) know what the selected row refers
// to without re-parsing displayed strings.
type rowMeta struct {
	kind viewName
	id   string
	name string
}

func (a *App) refreshCurrent() error {
	a.refreshHeader()
	switch a.current {
	case viewServices:
		return a.refreshServices()
	case viewContainers:
		return a.refreshContainers()
	case viewStacks:
		return a.refreshStacks()
	case viewNodes:
		return a.refreshNodes()
	case viewConfigs:
		return a.refreshConfigs()
	case viewSecrets:
		return a.refreshSecrets()
	default:
		return fmt.Errorf("unknown view %s", a.current)
	}
}

func setHeaderRow(table *tview.Table, cols ...string) {
	table.Clear()
	for i, c := range cols {
		table.SetCell(0, i, tview.NewTableCell(c).
			SetSelectable(false).
			SetAttributes(1<<1). // bold
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
	for i, svc := range services {
		row := i + 1
		mode, replicas := serviceModeAndReplicas(svc)
		stack := svc.Spec.Labels["com.docker.stack.namespace"]
		image := ""
		if svc.Spec.TaskTemplate.ContainerSpec != nil {
			image = svc.Spec.TaskTemplate.ContainerSpec.Image
		}
		ref := &rowMeta{kind: viewServices, id: svc.ID, name: svc.Spec.Name}
		setCell(a.table, row, 0, svc.Spec.Name, ref)
		setCell(a.table, row, 1, mode, nil)
		setCell(a.table, row, 2, replicas, nil)
		setCell(a.table, row, 3, image, nil)
		setCell(a.table, row, 4, stack, nil)
		setCell(a.table, row, 5, dockerx.ShortID(svc.ID), nil)
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
		return "global", fmt.Sprintf("%d", svc.ServiceStatus.RunningTasks)
	default:
		return "unknown", "-"
	}
}

// --- Containers ----------------------------------------------------------------

func (a *App) refreshContainers() error {
	ctx, cancel := a.ctx()
	defer cancel()
	containers, err := a.store.Containers(ctx)
	if err != nil {
		return err
	}

	setHeaderRow(a.table, "NAME", "IMAGE", "STATE", "STATUS", "NODE", "ID")
	for i, c := range containers {
		row := i + 1
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		node := c.Labels["com.docker.swarm.node.id"]
		ref := &rowMeta{kind: viewContainers, id: c.ID, name: name}
		setCell(a.table, row, 0, name, ref)
		setCell(a.table, row, 1, c.Image, nil)
		setCell(a.table, row, 2, c.State, nil)
		setCell(a.table, row, 3, c.Status, nil)
		setCell(a.table, row, 4, dockerx.ShortID(node), nil)
		setCell(a.table, row, 5, dockerx.ShortID(c.ID), nil)
	}
	a.setStatus(fmt.Sprintf("%d containers", len(containers)))
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

	setHeaderRow(a.table, "NAME", "SERVICES", "TASKS")
	for i, st := range stacks {
		row := i + 1
		ref := &rowMeta{kind: viewStacks, id: st.Name, name: st.Name}
		setCell(a.table, row, 0, st.Name, ref)
		setCell(a.table, row, 1, fmt.Sprintf("%d", st.Services), nil)
		setCell(a.table, row, 2, fmt.Sprintf("%d", st.Tasks), nil)
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
	for i, n := range nodes {
		row := i + 1
		ref := &rowMeta{kind: viewNodes, id: n.ID, name: n.Description.Hostname}
		setCell(a.table, row, 0, n.Description.Hostname, ref)
		setCell(a.table, row, 1, string(n.Spec.Role), nil)
		setCell(a.table, row, 2, string(n.Spec.Availability), nil)
		setCell(a.table, row, 3, string(n.Status.State), nil)
		setCell(a.table, row, 4, n.Description.Engine.EngineVersion, nil)
		setCell(a.table, row, 5, dockerx.ShortID(n.ID), nil)
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
	for i, c := range configs {
		row := i + 1
		ref := &rowMeta{kind: viewConfigs, id: c.ID, name: c.Spec.Name}
		setCell(a.table, row, 0, c.Spec.Name, ref)
		setCell(a.table, row, 1, c.CreatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 2, c.UpdatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 3, dockerx.ShortID(c.ID), nil)
	}
	a.setStatus(fmt.Sprintf("%d configs", len(configs)))
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
	for i, sec := range secrets {
		row := i + 1
		ref := &rowMeta{kind: viewSecrets, id: sec.ID, name: sec.Spec.Name}
		setCell(a.table, row, 0, sec.Spec.Name, ref)
		setCell(a.table, row, 1, sec.CreatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 2, sec.UpdatedAt.Format("2006-01-02 15:04"), nil)
		setCell(a.table, row, 3, dockerx.ShortID(sec.ID), nil)
	}
	a.setStatus(fmt.Sprintf("%d secrets", len(secrets)))
	return nil
}
