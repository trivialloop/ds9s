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

// StackSummary aggregates the services/tasks belonging to one stack
// (identified by the com.docker.stack.namespace label).
type StackSummary struct {
	Name     string
	Services int
	Tasks    int
}

func (s *Store) Stacks(ctx context.Context) ([]StackSummary, error) {
	services, err := s.conn.Client.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	tasks, err := s.conn.Client.TaskList(ctx, types.TaskListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}

	byStack := map[string]*StackSummary{}
	serviceStack := map[string]string{}
	for _, svc := range services {
		name := svc.Spec.Labels[stackLabel]
		if name == "" {
			name = "(none)"
		}
		serviceStack[svc.ID] = name
		entry, ok := byStack[name]
		if !ok {
			entry = &StackSummary{Name: name}
			byStack[name] = entry
		}
		entry.Services++
	}
	for _, t := range tasks {
		name := serviceStack[t.ServiceID]
		if name == "" {
			name = "(none)"
		}
		if entry, ok := byStack[name]; ok {
			entry.Tasks++
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

// ShortID trims a docker ID down to the conventional 12-char short form.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
