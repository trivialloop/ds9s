package dockerx

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
)


const stackLabel = "com.docker.stack.namespace"

// Store provides the read/action operations the UI views need, on top of a
// single Connection.
type Store struct {
	conn *Connection
}

func NewStore(conn *Connection) *Store {
	return &Store{conn: conn}
}

// --- Nodes -----------------------------------------------------------------

func (s *Store) Nodes(ctx context.Context) ([]swarm.Node, error) {
	nodes, err := s.conn.Client.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Description.Hostname < nodes[j].Description.Hostname })
	return nodes, nil
}

// --- Stacks ------------------------------------------------------------------

// StackSummary aggregates health metrics for one stack.
type StackSummary struct {
	Name            string
	Services        int    // total number of services
	ServicesOK      int    // services where running >= desired (fully healthy)
	ReplicasDesired uint64 // sum of desired replicas across all services
	ReplicasRunning uint64 // sum of currently running tasks across all services
}

// Stacks builds one StackSummary per com.docker.stack.namespace group using
// ServiceStatus (no separate TaskList call needed).
func (s *Store) Stacks(ctx context.Context) ([]StackSummary, error) {
	// Status:true populates ServiceStatus.RunningTasks / DesiredTasks.
	services, err := s.conn.Client.ServiceList(ctx, types.ServiceListOptions{Status: true})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}

	byStack := map[string]*StackSummary{}
	for _, svc := range services {
		name := svc.Spec.Labels[stackLabel]
		if name == "" {
			name = "(none)"
		}
		entry, ok := byStack[name]
		if !ok {
			entry = &StackSummary{Name: name}
			byStack[name] = entry
		}
		entry.Services++
		running := svc.ServiceStatus.RunningTasks
		desired := svc.ServiceStatus.DesiredTasks
		entry.ReplicasRunning += running
		entry.ReplicasDesired += desired
		if running >= desired {
			entry.ServicesOK++
		}
	}

	out := make([]StackSummary, 0, len(byStack))
	for _, v := range byStack {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveStack deletes every service carrying the stack's namespace label.
// Networks/configs/secrets scoped to the stack are removed best-effort too.
func (s *Store) RemoveStack(ctx context.Context, stackName string) []error {
	var errs []error

	services, err := s.ServicesInStack(ctx, stackName)
	if err != nil {
		return []error{err}
	}
	for _, svc := range services {
		if err := s.conn.Client.ServiceRemove(ctx, svc.ID); err != nil {
			errs = append(errs, fmt.Errorf("removing service %s: %w", svc.Spec.Name, err))
		}
	}

	stackFilter := filters.NewArgs(filters.Arg("label", stackLabel+"="+stackName))

	nets, err := s.conn.Client.NetworkList(ctx, types.NetworkListOptions{Filters: stackFilter})
	if err == nil {
		for _, n := range nets {
			if err := s.conn.Client.NetworkRemove(ctx, n.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing network %s: %w", n.Name, err))
			}
		}
	}

	secrets, err := s.conn.Client.SecretList(ctx, types.SecretListOptions{Filters: stackFilter})
	if err == nil {
		for _, sec := range secrets {
			if err := s.conn.Client.SecretRemove(ctx, sec.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing secret %s: %w", sec.Spec.Name, err))
			}
		}
	}

	configs, err := s.conn.Client.ConfigList(ctx, types.ConfigListOptions{Filters: stackFilter})
	if err == nil {
		for _, cfg := range configs {
			if err := s.conn.Client.ConfigRemove(ctx, cfg.ID); err != nil {
				errs = append(errs, fmt.Errorf("removing config %s: %w", cfg.Spec.Name, err))
			}
		}
	}

	return errs
}

func (s *Store) ServicesInStack(ctx context.Context, stackName string) ([]swarm.Service, error) {
	all, err := s.Services(ctx)
	if err != nil {
		return nil, err
	}
	var out []swarm.Service
	for _, svc := range all {
		name := svc.Spec.Labels[stackLabel]
		if name == "" {
			name = "(none)"
		}
		if name == stackName {
			out = append(out, svc)
		}
	}
	return out, nil
}

// --- Services ----------------------------------------------------------------

func (s *Store) Services(ctx context.Context) ([]swarm.Service, error) {
	services, err := s.conn.Client.ServiceList(ctx, types.ServiceListOptions{Status: true})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Spec.Name < services[j].Spec.Name })
	return services, nil
}

// ScaleService updates the desired replica count of a replicated service.
// It is a no-op error for global services, which cannot be scaled.
func (s *Store) ScaleService(ctx context.Context, serviceID string, replicas uint64) error {
	svc, _, err := s.conn.Client.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspecting service: %w", err)
	}
	if svc.Spec.Mode.Replicated == nil {
		return fmt.Errorf("service %s is not in replicated mode, cannot scale", svc.Spec.Name)
	}
	svc.Spec.Mode.Replicated.Replicas = &replicas

	_, err = s.conn.Client.ServiceUpdate(ctx, svc.ID, svc.Version, svc.Spec, types.ServiceUpdateOptions{})
	if err != nil {
		return fmt.Errorf("scaling service %s: %w", svc.Spec.Name, err)
	}
	return nil
}

// ForceUpdateService triggers a rolling restart without changing the spec
// (equivalent to `docker service update --force`).
func (s *Store) ForceUpdateService(ctx context.Context, serviceID string) error {
	svc, _, err := s.conn.Client.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspecting service: %w", err)
	}
	svc.Spec.TaskTemplate.ForceUpdate++
	_, err = s.conn.Client.ServiceUpdate(ctx, svc.ID, svc.Version, svc.Spec, types.ServiceUpdateOptions{})
	if err != nil {
		return fmt.Errorf("force-updating service %s: %w", svc.Spec.Name, err)
	}
	return nil
}

func (s *Store) RemoveService(ctx context.Context, serviceID string) error {
	if err := s.conn.Client.ServiceRemove(ctx, serviceID); err != nil {
		return fmt.Errorf("removing service: %w", err)
	}
	return nil
}

// --- Containers / Tasks ------------------------------------------------------

// TaskInfo holds resolved Swarm task information (service and node names
// already expanded), so the UI layer doesn't need extra API calls.
type TaskInfo struct {
	ID          string
	Name        string // "serviceName.slot" (replicated) or "serviceName.shortID" (global)
	ServiceID   string
	ServiceName string
	NodeName    string // Docker OS hostname of the node (from node.Description.Hostname)
	NodeAddr    string // Swarm advertise address of the node (from node.Status.Addr) — more reliable for SSH
	State       string
	Image       string
	ContainerID string
}

// AllTasks returns all Swarm tasks whose desired state is "running", with
// service and node names resolved. This gives a cluster-wide view of every
// container without having to SSH into individual worker nodes.
func (s *Store) AllTasks(ctx context.Context) ([]TaskInfo, error) {
	tasks, err := s.conn.Client.TaskList(ctx, types.TaskListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	services, err := s.conn.Client.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	nodes, err := s.conn.Client.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	svcNames := make(map[string]string, len(services))
	for _, svc := range services {
		svcNames[svc.ID] = svc.Spec.Name
	}
	nodeNames := make(map[string]string, len(nodes))
	nodeAddrs := make(map[string]string, len(nodes))
	for _, n := range nodes {
		nodeNames[n.ID] = n.Description.Hostname
		nodeAddrs[n.ID] = n.Status.Addr
	}

	var infos []TaskInfo
	for _, t := range tasks {
		// Only show tasks the scheduler intends to keep running.
		if t.DesiredState != swarm.TaskStateRunning {
			continue
		}
		svcName := svcNames[t.ServiceID]
		containerID := ""
		image := ""
		if t.Status.ContainerStatus != nil {
			containerID = t.Status.ContainerStatus.ContainerID
		}
		if t.Spec.ContainerSpec != nil {
			image = t.Spec.ContainerSpec.Image
			// Strip the sha256 digest from the image name.
			if idx := strings.Index(image, "@"); idx != -1 {
				image = image[:idx]
			}
		}
		name := fmt.Sprintf("%s.%d", svcName, t.Slot)
		if t.Slot == 0 {
			// Global service tasks have no slot; use a short task ID suffix.
			name = fmt.Sprintf("%s.%s", svcName, ShortID(t.ID))
		}
		infos = append(infos, TaskInfo{
			ID:          t.ID,
			Name:        name,
			ServiceID:   t.ServiceID,
			ServiceName: svcName,
			NodeName:    nodeNames[t.NodeID],
			NodeAddr:    nodeAddrs[t.NodeID],
			State:       string(t.Status.State),
			Image:       image,
			ContainerID: containerID,
		})
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].ServiceName != infos[j].ServiceName {
			return infos[i].ServiceName < infos[j].ServiceName
		}
		return infos[i].Name < infos[j].Name
	})
	return infos, nil
}

func (s *Store) Containers(ctx context.Context) ([]types.Container, error) {
	containers, err := s.conn.Client.ContainerList(ctx, types.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Names[0] < containers[j].Names[0] })
	return containers, nil
}

func (s *Store) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	if err := s.conn.Client.ContainerRemove(ctx, containerID, types.ContainerRemoveOptions{Force: force}); err != nil {
		return fmt.Errorf("removing container: %w", err)
	}
	return nil
}

func (s *Store) InspectContainer(ctx context.Context, containerID string) (types.ContainerJSON, error) {
	return s.conn.Client.ContainerInspect(ctx, containerID)
}

// --- Configs / Secrets --------------------------------------------------------

func (s *Store) Configs(ctx context.Context) ([]swarm.Config, error) {
	cfgs, err := s.conn.Client.ConfigList(ctx, types.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configs: %w", err)
	}
	sort.Slice(cfgs, func(i, j int) bool { return cfgs[i].Spec.Name < cfgs[j].Spec.Name })
	return cfgs, nil
}

func (s *Store) RemoveConfig(ctx context.Context, id string) error {
	if err := s.conn.Client.ConfigRemove(ctx, id); err != nil {
		return fmt.Errorf("removing config: %w", err)
	}
	return nil
}

func (s *Store) Secrets(ctx context.Context) ([]swarm.Secret, error) {
	secrets, err := s.conn.Client.SecretList(ctx, types.SecretListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Spec.Name < secrets[j].Spec.Name })
	return secrets, nil
}

func (s *Store) RemoveSecret(ctx context.Context, id string) error {
	if err := s.conn.Client.SecretRemove(ctx, id); err != nil {
		return fmt.Errorf("removing secret: %w", err)
	}
	return nil
}

// --- Logs ----------------------------------------------------------------------

// ContainerLogs streams raw (multiplexed) logs for a single container.
func (s *Store) ContainerLogs(ctx context.Context, containerID string, follow bool) (io.ReadCloser, error) {
	return s.conn.Client.ContainerLogs(ctx, containerID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       "200",
		Timestamps: true,
	})
}

// ServiceLogs streams logs for all tasks of a service through the Swarm
// manager, avoiding the "container not found" error that occurs when tasks
// run on worker nodes other than the connected manager.
func (s *Store) ServiceLogs(ctx context.Context, serviceID string, follow bool) (io.ReadCloser, error) {
	return s.conn.Client.ServiceLogs(ctx, serviceID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       "200",
		Timestamps: true,
	})
}

// ServiceTaskContainerIDs returns the container IDs of all currently running
// tasks for a service, used to fan out / aggregate "service logs".
func (s *Store) ServiceTaskContainerIDs(ctx context.Context, serviceID string) ([]string, error) {
	tasks, err := s.conn.Client.TaskList(ctx, types.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", serviceID), filters.Arg("desired-state", "running")),
	})
	if err != nil {
		return nil, fmt.Errorf("listing tasks for service: %w", err)
	}
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.Status.ContainerStatus != nil && t.Status.ContainerStatus.ContainerID != "" {
			ids = append(ids, t.Status.ContainerStatus.ContainerID)
		}
	}
	return ids, nil
}

// StackContainerIDs returns the container IDs of all running tasks across
// every service in a stack.
func (s *Store) StackContainerIDs(ctx context.Context, stackName string) ([]string, error) {
	services, err := s.ServicesInStack(ctx, stackName)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, svc := range services {
		svcIDs, err := s.ServiceTaskContainerIDs(ctx, svc.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, svcIDs...)
	}
	return ids, nil
}

// --- All-node SSH helpers -----------------------------------------------------

// nodeCommandOutput SSHes to nodeAddr using the manager's SSH credentials
// and returns stdout of cmd. Sudo is prepended if the manager is configured
// with sudo:true.
func (s *Store) nodeCommandOutput(nodeAddr, cmd string) (string, error) {
	if s.conn.Manager.SSH == nil {
		return "", fmt.Errorf("no SSH config — cannot reach node %s via SSH", nodeAddr)
	}
	if s.conn.Manager.SSH.Sudo {
		cmd = "sudo -n " + cmd
	}
	return RunCommandOnNode(*s.conn.Manager.SSH, nodeAddr, cmd)
}

// --- Volumes -----------------------------------------------------------------

// NodeVolumeInfo is one volume entry with its owning node resolved.
type NodeVolumeInfo struct {
	NodeName   string
	NodeAddr   string
	VolumeName string
	Driver     string
	Scope      string
	Mountpoint string
}

// AllNodeVolumes lists volumes from every Swarm node via SSH, then sorts by
// node name + volume name. Nodes that are unreachable are silently skipped
// (only their error is accumulated separately so callers can decide).
// Requires SSH config on the manager; falls back to manager-only VolumeList
// otherwise.
func (s *Store) AllNodeVolumes(ctx context.Context) ([]NodeVolumeInfo, []error) {
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return nil, []error{err}
	}

	type result struct {
		infos []NodeVolumeInfo
		err   error
	}
	ch := make(chan result, len(nodes))

	for _, n := range nodes {
		n := n
		go func() {
			addr := n.Status.Addr
			name := n.Description.Hostname
			// TSV: name|driver|scope|mountpoint (| is safe for paths on Linux)
			out, err := s.nodeCommandOutput(addr,
				`docker volume ls --format '{{.Name}}|{{.Driver}}|{{.Scope}}|{{.Mountpoint}}'`)
			if err != nil {
				ch <- result{err: fmt.Errorf("node %s (%s): %w", name, addr, err)}
				return
			}
			var infos []NodeVolumeInfo
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, "|", 4)
				if len(parts) < 4 {
					continue
				}
				infos = append(infos, NodeVolumeInfo{
					NodeName: name, NodeAddr: addr,
					VolumeName: parts[0], Driver: parts[1],
					Scope: parts[2], Mountpoint: parts[3],
				})
			}
			ch <- result{infos: infos}
		}()
	}

	var all []NodeVolumeInfo
	var errs []error
	for range nodes {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.err)
		} else {
			all = append(all, r.infos...)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].NodeName != all[j].NodeName {
			return all[i].NodeName < all[j].NodeName
		}
		return all[i].VolumeName < all[j].VolumeName
	})
	return all, errs
}

func (s *Store) Volumes(ctx context.Context) ([]*volume.Volume, error) {
	resp, err := s.conn.Client.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	vols := resp.Volumes
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, nil
}

func (s *Store) RemoveVolume(ctx context.Context, name string, force bool) error {
	if err := s.conn.Client.VolumeRemove(ctx, name, force); err != nil {
		return fmt.Errorf("removing volume: %w", err)
	}
	return nil
}

// --- Networks ----------------------------------------------------------------

func (s *Store) Networks(ctx context.Context) ([]types.NetworkResource, error) {
	nets, err := s.conn.Client.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	sort.Slice(nets, func(i, j int) bool { return nets[i].Name < nets[j].Name })
	return nets, nil
}

func (s *Store) RemoveNetwork(ctx context.Context, id string) error {
	if err := s.conn.Client.NetworkRemove(ctx, id); err != nil {
		return fmt.Errorf("removing network: %w", err)
	}
	return nil
}

// NodeNetworkInfo is one network entry with its owning node resolved.
type NodeNetworkInfo struct {
	NodeName string
	NodeAddr string
	ID       string
	Name     string
	Driver   string
	Scope    string
}

// AllNodeNetworks lists networks from every Swarm node via SSH. Overlay
// ("swarm"-scoped) networks returned by the manager API are already
// cluster-wide; this call additionally surfaces node-local networks
// (bridge, host, null) from each node.
func (s *Store) AllNodeNetworks(ctx context.Context) ([]NodeNetworkInfo, []error) {
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return nil, []error{err}
	}

	type result struct {
		infos []NodeNetworkInfo
		err   error
	}
	ch := make(chan result, len(nodes))

	for _, n := range nodes {
		n := n
		go func() {
			addr := n.Status.Addr
			name := n.Description.Hostname
			out, err := s.nodeCommandOutput(addr,
				`docker network ls --no-trunc --format '{{.ID}}|{{.Name}}|{{.Driver}}|{{.Scope}}'`)
			if err != nil {
				ch <- result{err: fmt.Errorf("node %s (%s): %w", name, addr, err)}
				return
			}
			var infos []NodeNetworkInfo
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, "|", 4)
				if len(parts) < 4 {
					continue
				}
				infos = append(infos, NodeNetworkInfo{
					NodeName: name, NodeAddr: addr,
					ID: parts[0], Name: parts[1],
					Driver: parts[2], Scope: parts[3],
				})
			}
			ch <- result{infos: infos}
		}()
	}

	var all []NodeNetworkInfo
	var errs []error
	for range nodes {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.err)
		} else {
			all = append(all, r.infos...)
		}
	}
	// Deduplicate swarm-scoped networks (each node reports them).
	seen := map[string]struct{}{}
	deduped := all[:0]
	for _, ni := range all {
		key := ni.Scope + "|" + ni.ID
		if ni.Scope == "swarm" || ni.Scope == "global" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ni.NodeName = "(cluster)"
		}
		deduped = append(deduped, ni)
	}
	sort.Slice(deduped, func(i, j int) bool {
		if deduped[i].NodeName != deduped[j].NodeName {
			return deduped[i].NodeName < deduped[j].NodeName
		}
		return deduped[i].Name < deduped[j].Name
	})
	return deduped, errs
}

// --- Container kill ----------------------------------------------------------

// KillContainer sends SIGKILL to a container running on the manager node.
// For containers on worker nodes, use BuildSSHCmd + remote docker kill instead.
func (s *Store) KillContainer(ctx context.Context, containerID string) error {
	if err := s.conn.Client.ContainerKill(ctx, containerID, "SIGKILL"); err != nil {
		return fmt.Errorf("killing container: %w", err)
	}
	return nil
}

// --- Service spec edit -------------------------------------------------------

// ServiceSpec returns the full service struct (spec + version) for editing.
func (s *Store) ServiceSpec(ctx context.Context, serviceID string) (swarm.Service, error) {
	svc, _, err := s.conn.Client.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return swarm.Service{}, fmt.Errorf("inspecting service: %w", err)
	}
	return svc, nil
}

// ServiceUpdateSpec applies an edited ServiceSpec back to the Swarm manager.
func (s *Store) ServiceUpdateSpec(ctx context.Context, serviceID string, version swarm.Version, spec swarm.ServiceSpec) error {
	_, err := s.conn.Client.ServiceUpdate(ctx, serviceID, version, spec, types.ServiceUpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating service: %w", err)
	}
	return nil
}

// ShortID trims a docker ID down to the conventional 12-char short form.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
