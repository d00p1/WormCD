package swarm

import (
	"context"
	"fmt"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// createService creates a service and waits until it runs. A bad image makes
// its tasks fail, and that failure is the error Apply returns.
func (r *Runtime) createService(scope runtime.Scope) func(context.Context, desired.Service) error {
	return func(ctx context.Context, service desired.Service) error {
		spec, err := r.serviceSpecFor(ctx, scope, service)
		if err != nil {
			return err
		}
		if _, err := r.api.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec}); err != nil {
			return fmt.Errorf("create service %s: %w", service.Name, err)
		}
		return r.waitConverged(ctx, service.Name, 0)
	}
}

// updateService replaces the whole spec of an existing service with ServiceUpdate
// and waits until the new version runs. The version is read from ServiceInspect
// when Apply runs, the same way as for ConfigUpdate: a service changed between
// Plan and Apply is overwritten with the desired spec.
func (r *Runtime) updateService(scope runtime.Scope) func(context.Context, desired.Service, runtime.Entry) error {
	return func(ctx context.Context, service desired.Service, _ runtime.Entry) error {
		spec, err := r.serviceSpecFor(ctx, scope, service)
		if err != nil {
			return err
		}
		current, err := r.inspectOwnedService(ctx, scope, service.Name)
		if err != nil {
			return err
		}
		_, err = r.api.ServiceUpdate(ctx, current.ID, client.ServiceUpdateOptions{
			Version: current.Version,
			Spec:    spec,
		})
		if err != nil {
			return fmt.Errorf("update service %s: %w", service.Name, err)
		}
		return r.waitConverged(ctx, service.Name, current.Version.Index)
	}
}

// serviceSpecFor converts a desired service after looking up the Docker IDs of
// the configs and secrets it references. Config and secret names are final by
// now: configs were created earlier in this Apply.
func (r *Runtime) serviceSpecFor(ctx context.Context, scope runtime.Scope, service desired.Service) (swarm.ServiceSpec, error) {
	ids := referenceIDs{Configs: make(map[string]string), Secrets: make(map[string]string)}
	for _, ref := range service.Configs {
		if _, ok := ids.Configs[ref.Source]; ok {
			continue
		}
		inspected, err := r.api.ConfigInspect(ctx, ref.Source, client.ConfigInspectOptions{})
		if err != nil {
			return swarm.ServiceSpec{}, fmt.Errorf("inspect config %s: %w", ref.Source, err)
		}
		ids.Configs[ref.Source] = inspected.Config.ID
	}
	for _, ref := range service.Secrets {
		if _, ok := ids.Secrets[ref.Source]; ok {
			continue
		}
		inspected, err := r.api.SecretInspect(ctx, ref.Source, client.SecretInspectOptions{})
		if err != nil {
			return swarm.ServiceSpec{}, fmt.Errorf("inspect secret %s: %w", ref.Source, err)
		}
		ids.Secrets[ref.Source] = inspected.Secret.ID
	}
	return serviceSpec(service, scope, ids)
}

// deleteServices removes the services whose plan entry is Delete. Each one is
// checked for ownership before it is removed. A service that is already gone
// is not an error: the plan was made before it disappeared, and the desired
// state does not want it either.
func (r *Runtime) deleteServices(ctx context.Context, result *runtime.Result, scope runtime.Scope, entries []runtime.Entry) error {
	prefix := string(desired.KindService) + "/"
	for _, entry := range entries {
		name, ok := strings.CutPrefix(entry.Key, prefix)
		if !ok || entry.Action != runtime.Delete {
			continue
		}
		current, err := r.inspectOwnedService(ctx, scope, name)
		if cerrdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = r.api.ServiceRemove(ctx, current.ID, client.ServiceRemoveOptions{})
		if cerrdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("remove service %s: %w", name, err)
		}
		result.Deleted = append(result.Deleted, entry.Key)
	}
	return nil
}

// inspectOwnedService reads a service by name and refuses one that scope does not
// own, so an update or removal never touches a service WormCD did not create.
func (r *Runtime) inspectOwnedService(ctx context.Context, scope runtime.Scope, name string) (swarm.Service, error) {
	inspected, err := r.api.ServiceInspect(ctx, name, client.ServiceInspectOptions{})
	if err != nil {
		return swarm.Service{}, fmt.Errorf("inspect service %s: %w", name, err)
	}
	if !inScope(inspected.Service.Spec.Labels, scope) {
		return swarm.Service{}, fmt.Errorf("service %s: not owned by %s/%s", name, scope.Application, scope.Target)
	}
	return inspected.Service, nil
}
