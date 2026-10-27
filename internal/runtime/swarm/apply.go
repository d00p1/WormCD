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

// Apply creates the networks, volumes, configs and secrets that a plan asks
// for. Entries with Noop or Delete actions do nothing here: deletions belong to
// Prune, which runs after the services have moved off the old objects.
//
// Configs and secrets are immutable. A content change arrives as a new name
// (the hash is part of it), so an Update on them is an error, not a silent
// in-place edit. Service changes are not applied until #18.
func (r *Runtime) Apply(ctx context.Context, plan runtime.Plan) (runtime.Result, error) {
	if err := validateScope(runtime.Scope{Application: plan.Application, Target: plan.Target}); err != nil {
		return runtime.Result{}, err
	}
	if plan.Desired.Application != plan.Application || plan.Desired.Target != plan.Target {
		return runtime.Result{}, fmt.Errorf("plan desired state belongs to %s/%s, not %s/%s",
			plan.Desired.Application, plan.Desired.Target, plan.Application, plan.Target)
	}

	entries := make(map[string]runtime.Entry, len(plan.Entries))
	for _, entry := range plan.Entries {
		entries[entry.Key] = entry
		if isService(entry.Key) && entry.Action != runtime.Noop {
			return runtime.Result{}, fmt.Errorf("%s: %s is not implemented yet (see #18)", entry.Key, entry.Action)
		}
	}

	var result runtime.Result
	for _, network := range plan.Desired.Networks {
		if !network.Managed() {
			continue
		}
		key := desired.Key{Kind: desired.KindNetwork, Name: network.Name}.String()
		switch entries[key].Action {
		case runtime.Create:
			_, err := r.api.NetworkCreate(ctx, network.Name, client.NetworkCreateOptions{
				Driver:     network.Driver,
				Options:    network.DriverOpts,
				Internal:   network.Internal,
				Attachable: network.Attachable,
				Labels:     desired.MergeLabels(network.Labels, plan.Application, plan.Target),
			})
			if err != nil {
				return result, fmt.Errorf("create network %s: %w", network.Name, err)
			}
			result.Created = append(result.Created, key)
		case runtime.Update:
			return result, inPlaceUpdateError(entries[key])
		}
	}

	for _, config := range plan.Desired.Configs {
		if !config.Managed() {
			continue
		}
		key := desired.Key{Kind: desired.KindConfig, Name: config.Name}.String()
		switch entries[key].Action {
		case runtime.Create:
			_, err := r.api.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: swarm.ConfigSpec{
				Annotations: swarm.Annotations{
					Name:   config.Name,
					Labels: desired.MergeLabels(config.Labels, plan.Application, plan.Target),
				},
				Data: config.Data,
			}})
			if err != nil {
				return result, fmt.Errorf("create config %s: %w", config.Name, err)
			}
			result.Created = append(result.Created, key)
		case runtime.Update:
			return result, immutableError(entries[key])
		}
	}

	for _, secret := range plan.Desired.Secrets {
		if !secret.Managed() {
			continue
		}
		key := desired.Key{Kind: desired.KindSecret, Name: secret.Name}.String()
		switch entries[key].Action {
		case runtime.Create:
			// The model carries no secret payload: v0 only references external
			// secrets, so a managed one cannot be created yet.
			return result, fmt.Errorf("create secret %s: managed secrets are not supported in v0", secret.Name)
		case runtime.Update:
			return result, immutableError(entries[key])
		}
	}

	for _, volume := range plan.Desired.Volumes {
		if !volume.Managed() {
			continue
		}
		key := desired.Key{Kind: desired.KindVolume, Name: volume.Name}.String()
		switch entries[key].Action {
		case runtime.Create:
			_, err := r.api.VolumeCreate(ctx, client.VolumeCreateOptions{
				Name:       volume.Name,
				Driver:     volume.Driver,
				DriverOpts: volume.DriverOpts,
				Labels:     desired.MergeLabels(volume.Labels, plan.Application, plan.Target),
			})
			if err != nil {
				return result, fmt.Errorf("create volume %s: %w", volume.Name, err)
			}
			result.Created = append(result.Created, key)
		case runtime.Update:
			return result, inPlaceUpdateError(entries[key])
		}
	}

	return result, nil
}

// Health is not implemented yet; it arrives with service support (#18).
func (r *Runtime) Health(context.Context, runtime.Scope) (runtime.Health, error) {
	return runtime.Health{}, fmt.Errorf("health: not implemented yet (see #18)")
}

func isService(key string) bool {
	return strings.HasPrefix(key, string(desired.KindService)+"/")
}

func immutableError(entry runtime.Entry) error {
	return fmt.Errorf("%s: configs and secrets are immutable (%s); a content change must use a new name",
		entry.Key, strings.Join(entry.Changes, ", "))
}

func inPlaceUpdateError(entry runtime.Entry) error {
	return fmt.Errorf("%s: in-place update is not supported (%s); remove the object and create it again",
		entry.Key, strings.Join(entry.Changes, ", "))
}
