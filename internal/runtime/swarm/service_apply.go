package swarm

import (
	"context"
	"fmt"
	"slices"
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
	configIDs, err := resolveIDs(ctx, "config", service.Configs,
		func(ref desired.ConfigRef) string { return ref.Source },
		func(ctx context.Context, name string) (string, error) {
			inspected, err := r.api.ConfigInspect(ctx, name, client.ConfigInspectOptions{})
			return inspected.Config.ID, err
		})
	if err != nil {
		return swarm.ServiceSpec{}, err
	}
	secretIDs, err := resolveIDs(ctx, "secret", service.Secrets,
		func(ref desired.SecretRef) string { return ref.Source },
		func(ctx context.Context, name string) (string, error) {
			inspected, err := r.api.SecretInspect(ctx, name, client.SecretInspectOptions{})
			return inspected.Secret.ID, err
		})
	if err != nil {
		return swarm.ServiceSpec{}, err
	}
	return serviceSpec(service, scope, referenceIDs{Configs: configIDs, Secrets: secretIDs})
}

// resolveIDs looks up the Docker ID of every object that refs name, once per name.
func resolveIDs[R any](ctx context.Context, kind string, refs []R, name func(R) string,
	inspect func(context.Context, string) (string, error),
) (map[string]string, error) {
	ids := make(map[string]string, len(refs))
	for _, ref := range refs {
		objectName := name(ref)
		if _, ok := ids[objectName]; ok {
			continue
		}
		id, err := inspect(ctx, objectName)
		if err != nil {
			return nil, fmt.Errorf("inspect %s %s: %w", kind, objectName, err)
		}
		ids[objectName] = id
	}
	return ids, nil
}

// deleteServices removes the services whose plan entry is Delete. Each one is
// checked for ownership before it is removed. A service that is already gone
// is not an error: the plan was made before it disappeared, and the desired
// state does not want it either.
func (r *Runtime) deleteServices(ctx context.Context, result *runtime.Result, scope runtime.Scope, entries []runtime.Entry) error {
	for _, entry := range entries {
		key, ok := desired.ParseKey(entry.Key)
		if !ok || key.Kind != desired.KindService || entry.Action != runtime.Delete {
			continue
		}
		name := key.Name
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

// preflightServices checks every desired service before the first mutation: it
// needs a plan entry, its references must be declared, and its spec must be
// convertible. A mode change is refused, because Docker does not allow it in
// place.
func preflightServices(desiredState desired.Desired, entries map[string]runtime.Entry) error {
	for _, service := range desiredState.Services {
		key := desired.Key{Kind: desired.KindService, Name: service.Name}.String()
		entry, ok := entries[key]
		if !ok {
			return fmt.Errorf("%s: missing from the plan", key)
		}
		switch entry.Action {
		case runtime.Create:
		case runtime.Update:
			if slices.Contains(entry.Changes, "mode") {
				return fmt.Errorf("%s: service mode cannot change in place (%s); recreate the service by hand",
					key, strings.Join(entry.Changes, ", "))
			}
		default:
			continue
		}
		if err := validateService(service, desiredState); err != nil {
			return err
		}
	}
	return nil
}

// validateService runs the checks of serviceSpec that need no Docker IDs, and
// checks that every reference is declared in the desired state.
func validateService(service desired.Service, desiredState desired.Desired) error {
	if err := checkDeclared(service.Name, "config", service.Configs,
		func(ref desired.ConfigRef) string { return ref.Source },
		desiredState.Configs, func(config desired.Config) string { return config.Name }); err != nil {
		return err
	}
	if err := checkDeclared(service.Name, "secret", service.Secrets,
		func(ref desired.SecretRef) string { return ref.Source },
		desiredState.Secrets, func(secret desired.Secret) string { return secret.Name }); err != nil {
		return err
	}
	// serviceSpec is the only place that checks the mode and the mounts. Every
	// reference is declared by now, so the object names stand in for the Docker
	// IDs, and the conversion runs without any API call.
	ids := referenceIDs{Configs: make(map[string]string), Secrets: make(map[string]string)}
	for _, ref := range service.Configs {
		ids.Configs[ref.Source] = ref.Source
	}
	for _, ref := range service.Secrets {
		ids.Secrets[ref.Source] = ref.Source
	}
	scope := runtime.Scope{Application: desiredState.Application, Target: desiredState.Target}
	_, err := serviceSpec(service, scope, ids)
	return err
}

// checkDeclared fails for the first reference to an object that the desired
// state does not declare.
func checkDeclared[R, D any](service, kind string, refs []R, refName func(R) string,
	objects []D, objectName func(D) string,
) error {
	declared := make(map[string]bool, len(objects))
	for _, object := range objects {
		declared[objectName(object)] = true
	}
	for _, ref := range refs {
		if !declared[refName(ref)] {
			return fmt.Errorf("service %s: %s %s is not declared in the desired state", service, kind, refName(ref))
		}
	}
	return nil
}
