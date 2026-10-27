package swarm

import (
	"context"
	"fmt"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// Prune removes the managed networks, volumes, configs and secrets of scope
// that are not in keep. Objects that a service in the cluster still references
// are skipped rather than failing: Docker refuses to remove them, and they
// become prunable once the service has moved to the new revision.
//
// Services are not pruned yet (see #18); a service of scope that is not in keep
// is reported as an error instead of being left behind silently.
func (r *Runtime) Prune(ctx context.Context, scope runtime.Scope, keep runtime.Set) error {
	if err := validateScope(scope); err != nil {
		return err
	}

	services, err := r.api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	for _, item := range filterByScope(services.Items, func(s swarm.Service) map[string]string { return s.Spec.Labels }, scope) {
		if !keep.Has(desired.KindService, item.Spec.Name) {
			return fmt.Errorf("service %s: pruning services is not implemented yet (see #18)", item.Spec.Name)
		}
	}
	referenced := serviceReferences(services.Items)

	networks, err := r.api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, item := range filterByScope(networks.Items, func(n network.Summary) map[string]string { return n.Labels }, scope) {
		if keep.Has(desired.KindNetwork, item.Name) || referenced.has(desired.KindNetwork, item.ID, item.Name) {
			continue
		}
		if _, err := r.api.NetworkRemove(ctx, item.ID, client.NetworkRemoveOptions{}); err != nil {
			return fmt.Errorf("remove network %s: %w", item.Name, err)
		}
	}

	configs, err := r.api.ConfigList(ctx, client.ConfigListOptions{})
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}
	for _, item := range filterByScope(configs.Items, func(c swarm.Config) map[string]string { return c.Spec.Labels }, scope) {
		if keep.Has(desired.KindConfig, item.Spec.Name) || referenced.has(desired.KindConfig, item.Spec.Name) {
			continue
		}
		if _, err := r.api.ConfigRemove(ctx, item.ID, client.ConfigRemoveOptions{}); err != nil {
			return fmt.Errorf("remove config %s: %w", item.Spec.Name, err)
		}
	}

	secrets, err := r.api.SecretList(ctx, client.SecretListOptions{})
	if err != nil {
		return fmt.Errorf("list secrets: %w", err)
	}
	for _, item := range filterByScope(secrets.Items, func(s swarm.Secret) map[string]string { return s.Spec.Labels }, scope) {
		if keep.Has(desired.KindSecret, item.Spec.Name) || referenced.has(desired.KindSecret, item.Spec.Name) {
			continue
		}
		if _, err := r.api.SecretRemove(ctx, item.ID, client.SecretRemoveOptions{}); err != nil {
			return fmt.Errorf("remove secret %s: %w", item.Spec.Name, err)
		}
	}

	volumes, err := r.api.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	for _, item := range filterByScope(volumes.Items, func(v volume.Volume) map[string]string { return v.Labels }, scope) {
		if keep.Has(desired.KindVolume, item.Name) || referenced.has(desired.KindVolume, item.Name) {
			continue
		}
		if _, err := r.api.VolumeRemove(ctx, item.Name, client.VolumeRemoveOptions{}); err != nil {
			return fmt.Errorf("remove volume %s: %w", item.Name, err)
		}
	}

	return nil
}

// references holds the objects that services in the cluster point at. Keys
// use the same desired.Key shape as keep, so both can be matched directly.
type references map[desired.Key]struct{}

func serviceReferences(services []swarm.Service) references {
	refs := make(references)
	for _, service := range services {
		spec := service.Spec
		for _, attachment := range spec.TaskTemplate.Networks {
			refs.add(desired.KindNetwork, attachment.Target)
		}
		containerSpec := spec.TaskTemplate.ContainerSpec
		if containerSpec == nil {
			continue
		}
		for _, item := range containerSpec.Mounts {
			if item.Type == mount.TypeVolume {
				refs.add(desired.KindVolume, item.Source)
			}
		}
		for _, ref := range containerSpec.Configs {
			if ref != nil {
				refs.add(desired.KindConfig, ref.ConfigName)
			}
		}
		for _, ref := range containerSpec.Secrets {
			if ref != nil {
				refs.add(desired.KindSecret, ref.SecretName)
			}
		}
	}
	return refs
}

func (refs references) add(kind desired.Kind, name string) {
	refs[desired.Key{Kind: kind, Name: name}] = struct{}{}
}

func (refs references) has(kind desired.Kind, names ...string) bool {
	for _, name := range names {
		if _, ok := refs[desired.Key{Kind: kind, Name: name}]; ok {
			return true
		}
	}
	return false
}
