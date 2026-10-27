package swarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Apply creates the networks, configs, secrets and volumes that a plan asks
// for, in that order. Entries with Noop or Delete actions do nothing here:
// deletions belong to Prune, which runs after the services have moved off the
// old objects.
//
// Configs change in place only by their labels, through ConfigUpdate: Docker
// fixes a config's data, so a content change arrives as a new name (the hash is
// part of it). Secrets have no in-place update in v0, so an Update on them is an
// error. Service changes are not applied until #18.
func (r *Runtime) Apply(ctx context.Context, plan runtime.Plan) (runtime.Result, error) {
	desiredState := plan.Desired
	scope := runtime.Scope{Application: desiredState.Application, Target: desiredState.Target}
	if err := validateScope(scope); err != nil {
		return runtime.Result{}, err
	}
	if plan.Application != scope.Application || plan.Target != scope.Target {
		return runtime.Result{}, fmt.Errorf("plan desired state belongs to %s/%s, not %s/%s",
			scope.Application, scope.Target, plan.Application, plan.Target)
	}

	entries := make(map[string]runtime.Entry, len(plan.Entries))
	for _, entry := range plan.Entries {
		entries[entry.Key] = entry
		if isService(entry.Key) && entry.Action != runtime.Noop {
			return runtime.Result{}, fmt.Errorf("%s: %s is not implemented yet (see #18)", entry.Key, entry.Action)
		}
	}

	if err := preflight(desiredState, entries); err != nil {
		return runtime.Result{}, err
	}

	var result runtime.Result
	if err := applyObjects(ctx, &result, entries, desired.KindNetwork, desiredState.Networks,
		func(n desired.Network) string { return n.Name },
		r.createNetwork(scope), nil); err != nil {
		return result, err
	}
	if err := applyObjects(ctx, &result, entries, desired.KindConfig, desiredState.Configs,
		func(c desired.Config) string { return c.Name },
		r.createConfig(scope), r.updateConfig(scope, r.api)); err != nil {
		return result, err
	}
	if err := applyObjects(ctx, &result, entries, desired.KindSecret, desiredState.Secrets,
		func(s desired.Secret) string { return s.Name },
		createSecret, nil); err != nil {
		return result, err
	}
	if err := applyObjects(ctx, &result, entries, desired.KindVolume, desiredState.Volumes,
		func(v desired.Volume) string { return v.Name },
		r.createVolume(scope), nil); err != nil {
		return result, err
	}
	return result, nil
}

// preflight checks the whole plan before the first mutation, so a plan with an
// unsupported change is refused without creating anything. Every managed object
// needs an entry, because a missing entry would otherwise be skipped silently.
func preflight(desiredState desired.Desired, entries map[string]runtime.Entry) error {
	if err := checkObjects(entries, desired.KindNetwork, desiredState.Networks,
		func(n desired.Network) string { return n.Name }, onUpdate(networkUpdateError)); err != nil {
		return err
	}
	if err := checkObjects(entries, desired.KindConfig, desiredState.Configs,
		func(c desired.Config) string { return c.Name }, onUpdate(configChangeError)); err != nil {
		return err
	}
	if err := checkObjects(entries, desired.KindSecret, desiredState.Secrets,
		func(s desired.Secret) string { return s.Name }, secretCheck); err != nil {
		return err
	}
	return checkObjects(entries, desired.KindVolume, desiredState.Volumes,
		func(v desired.Volume) string { return v.Name }, onUpdate(volumeUpdateError))
}

// checkObjects reports an error for a managed object that has no entry in the
// plan, and runs check on the entry of every such object.
func checkObjects[T desired.Managed](entries map[string]runtime.Entry, kind desired.Kind, objects []T,
	name func(T) string, check func(runtime.Entry) error,
) error {
	for _, object := range objects {
		if !object.Managed() {
			continue
		}
		key := desired.Key{Kind: kind, Name: name(object)}.String()
		entry, ok := entries[key]
		if !ok {
			return fmt.Errorf("%s: missing from the plan", key)
		}
		if err := check(entry); err != nil {
			return err
		}
	}
	return nil
}

// onUpdate limits check to Update entries. Create and Noop entries of a kind
// that has no in-place update are fine.
func onUpdate(check func(runtime.Entry) error) func(runtime.Entry) error {
	return func(entry runtime.Entry) error {
		if entry.Action != runtime.Update {
			return nil
		}
		return check(entry)
	}
}

// Health is not implemented yet; it arrives with service support (#18). The
// result is explicit about that, so the core can report the target as not
// converged instead of failing.
func (r *Runtime) Health(context.Context, runtime.Scope) (runtime.Health, error) {
	return runtime.Health{Healthy: false, Problems: []string{"not implemented (see #18)"}}, nil
}

// applyObjects runs the plan entry of every managed object of one kind. A
// Create calls create and records the key; an Update calls update and records
// it. Entries marked Noop/Delete are skipped. A nil update means the kind has no
// in-place update, and preflight has already refused every Update of it.
// preflight has also checked that every managed object has an entry.
func applyObjects[T desired.Managed](ctx context.Context, result *runtime.Result, entries map[string]runtime.Entry,
	kind desired.Kind, objects []T, name func(T) string,
	create func(context.Context, T) error, update func(context.Context, T, runtime.Entry) error,
) error {
	for _, object := range objects {
		if !object.Managed() {
			continue
		}
		key := desired.Key{Kind: kind, Name: name(object)}.String()
		entry := entries[key]
		switch entry.Action {
		case runtime.Create:
			if err := create(ctx, object); err != nil {
				return err
			}
			result.Created = append(result.Created, key)
		case runtime.Update:
			if update == nil {
				return fmt.Errorf("%s: update is not supported", key)
			}
			if err := update(ctx, object, entry); err != nil {
				return err
			}
			result.Updated = append(result.Updated, key)
		}
	}
	return nil
}

func (r *Runtime) createNetwork(scope runtime.Scope) func(context.Context, desired.Network) error {
	return func(ctx context.Context, network desired.Network) error {
		_, err := r.api.NetworkCreate(ctx, network.Name, client.NetworkCreateOptions{
			Driver:     network.Driver,
			Options:    network.DriverOpts,
			Internal:   network.Internal,
			Attachable: network.Attachable,
			Labels:     desired.MergeLabels(network.Labels, scope.Application, scope.Target),
		})
		if err != nil {
			return fmt.Errorf("create network %s: %w", network.Name, err)
		}
		return nil
	}
}

// configSpec is the spec that both ConfigCreate and ConfigUpdate send for a
// managed config.
func configSpec(config desired.Config, scope runtime.Scope) swarm.ConfigSpec {
	return swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name:   config.Name,
			Labels: desired.MergeLabels(config.Labels, scope.Application, scope.Target),
		},
		Data: config.Data,
	}
}

func (r *Runtime) createConfig(scope runtime.Scope) func(context.Context, desired.Config) error {
	return func(ctx context.Context, config desired.Config) error {
		_, err := r.api.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: configSpec(config, scope)})
		if err != nil {
			return fmt.Errorf("create config %s: %w", config.Name, err)
		}
		return nil
	}
}

// updateConfig changes the labels of an existing config with ConfigUpdate. Docker
// rejects any change to the data, so a change to anything but labels is refused
// before the API is called. The version is read from ConfigInspect at the moment
// Apply runs, so a config changed between Plan and Apply is not detected: there is
// no protection against that change.
func (r *Runtime) updateConfig(scope runtime.Scope, reader configReader) func(context.Context, desired.Config, runtime.Entry) error {
	return func(ctx context.Context, config desired.Config, entry runtime.Entry) error {
		if err := configChangeError(entry); err != nil {
			return err
		}

		inspected, err := reader.ConfigInspect(ctx, config.Name, client.ConfigInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect config %s: %w", config.Name, err)
		}
		_, err = r.api.ConfigUpdate(ctx, config.Name, client.ConfigUpdateOptions{
			Version: inspected.Config.Version,
			Spec:    configSpec(config, scope),
		})
		if err != nil {
			return fmt.Errorf("update config %s: %w", config.Name, err)
		}
		return nil
	}
}

// createSecret always fails: the model carries no secret payload, and v0 only
// references external secrets. preflight refuses a managed secret before this
// runs, so it is only reached if that check is skipped.
func createSecret(_ context.Context, secret desired.Secret) error {
	return secretCreateError(desired.Key{Kind: desired.KindSecret, Name: secret.Name}.String())
}

func (r *Runtime) createVolume(scope runtime.Scope) func(context.Context, desired.Volume) error {
	return func(ctx context.Context, volume desired.Volume) error {
		_, err := r.api.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name:       volume.Name,
			Driver:     volume.Driver,
			DriverOpts: volume.DriverOpts,
			Labels:     desired.MergeLabels(volume.Labels, scope.Application, scope.Target),
		})
		if err != nil {
			return fmt.Errorf("create volume %s: %w", volume.Name, err)
		}
		return nil
	}
}

func isService(key string) bool {
	return strings.HasPrefix(key, string(desired.KindService)+"/")
}

// configChangeError refuses a config update that changes anything but labels.
func configChangeError(entry runtime.Entry) error {
	for _, change := range entry.Changes {
		if !strings.HasPrefix(change, "labels.") {
			return fmt.Errorf("%s: config changes other than labels cannot be applied in place (%s); a content change must use a new name",
				entry.Key, strings.Join(entry.Changes, ", "))
		}
	}
	return nil
}

// secretCheck refuses every managed secret: v0 cannot create one, and secrets
// have no in-place update.
func secretCheck(entry runtime.Entry) error {
	switch entry.Action {
	case runtime.Create:
		return secretCreateError(entry.Key)
	case runtime.Update:
		return secretUpdateError(entry)
	default:
		return nil
	}
}

func secretCreateError(key string) error {
	return fmt.Errorf("%s: managed secrets are not supported in v0", key)
}

// secretUpdateError refuses secret updates. Secrets have no in-place update in
// v0: Docker secrets are immutable, and the diff never asks for one.
func secretUpdateError(entry runtime.Entry) error {
	return fmt.Errorf("%s: secrets are immutable (%s); a content change must use a new name",
		entry.Key, strings.Join(entry.Changes, ", "))
}

// networkUpdateError refuses a network update. v0 has no in-place network
// update, so the network must be recreated by hand (see #23).
func networkUpdateError(entry runtime.Entry) error {
	return fmt.Errorf("%s: in-place update is not supported in v0 (%s); recreate the network by hand after detaching its services; see #23",
		entry.Key, strings.Join(entry.Changes, ", "))
}

// volumeUpdateError refuses a volume update. v0 has no in-place volume update,
// and recreating a volume deletes its data (see #23).
func volumeUpdateError(entry runtime.Entry) error {
	return fmt.Errorf("%s: in-place update is not supported in v0 (%s); recreating the volume deletes its data, so back it up and recreate it by hand; see #23",
		entry.Key, strings.Join(entry.Changes, ", "))
}
