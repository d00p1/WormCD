package swarm

import (
	"context"
	"fmt"
	"strings"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// Prune removes the managed networks, configs and secrets of scope that are not
// in keep. It never removes volumes: they hold data, so an unwanted volume is
// reported as skipped and must be removed by a person.
//
// An object that a service in the cluster still references is also skipped,
// not removed: Docker refuses to remove it, and it becomes prunable once the
// service has moved to the new revision. Every skipped object is listed in the
// result with its reason.
//
// Services are not pruned yet (see #18); a service of scope that is not in keep
// is reported as an error instead of being left behind silently.
func (r *Runtime) Prune(ctx context.Context, scope runtime.Scope, keep runtime.Set) (runtime.PruneResult, error) {
	var result runtime.PruneResult
	if err := validateScope(scope); err != nil {
		return result, err
	}

	services, err := r.api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return result, fmt.Errorf("list services: %w", err)
	}
	for _, item := range filterByScope(services.Items, func(s swarm.Service) map[string]string { return s.Spec.Labels }, scope) {
		if !keep.Has(desired.KindService, item.Spec.Name) {
			return result, fmt.Errorf("service %s: pruning services is not implemented yet (see #18)", item.Spec.Name)
		}
	}
	referenced := serviceReferences(services.Items)

	steps := []pruneStep{
		networkPrune(r),
		configPrune(r),
		secretPrune(r),
		volumePrune(r),
	}
	for _, step := range steps {
		if err := step(ctx, scope, keep, referenced, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// pruneStep prunes one kind of object. Every kind shares the same loop in
// pruneKind; only the hooks it is built from differ.
type pruneStep func(ctx context.Context, scope runtime.Scope, keep runtime.Set, referenced references, result *runtime.PruneResult) error

// pruneKind builds the step for one kind. list returns every object of the
// kind, name is the key that keep uses, refNames are the forms a service may
// reference the object by, and remove deletes it. A nil remove means the kind
// is never removed automatically, and retain is the reason it is reported.
func pruneKind[T any](
	kind desired.Kind,
	list func(context.Context) ([]T, error),
	labels func(T) map[string]string,
	name func(T) string,
	refNames func(T) []string,
	remove func(context.Context, T) error,
	retain string,
) pruneStep {
	return func(ctx context.Context, scope runtime.Scope, keep runtime.Set, referenced references, result *runtime.PruneResult) error {
		items, err := list(ctx)
		if err != nil {
			return err
		}
		for _, item := range filterByScope(items, labels, scope) {
			itemName := name(item)
			key := desired.Key{Kind: kind, Name: itemName}.String()
			switch {
			case keep.Has(kind, itemName):
			case referenced.has(kind, refNames(item)...):
				result.Skipped = append(result.Skipped, runtime.Skip{Key: key, Reason: "in use by a service"})
			case remove == nil:
				result.Skipped = append(result.Skipped, runtime.Skip{Key: key, Reason: retain})
			default:
				if err := remove(ctx, item); err != nil {
					return fmt.Errorf("remove %s %s: %w", strings.ToLower(string(kind)), itemName, err)
				}
				result.Removed = append(result.Removed, key)
			}
		}
		return nil
	}
}

func networkPrune(r *Runtime) pruneStep {
	return pruneKind(desired.KindNetwork,
		func(ctx context.Context) ([]network.Summary, error) {
			res, err := r.api.NetworkList(ctx, client.NetworkListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list networks: %w", err)
			}
			return res.Items, nil
		},
		func(n network.Summary) map[string]string { return n.Labels },
		func(n network.Summary) string { return n.Name },
		// Services attach networks by ID, so both forms are checked.
		func(n network.Summary) []string { return []string{n.ID, n.Name} },
		func(ctx context.Context, n network.Summary) error {
			_, err := r.api.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			return err
		},
		"",
	)
}

func configPrune(r *Runtime) pruneStep {
	return pruneKind(desired.KindConfig,
		func(ctx context.Context) ([]swarm.Config, error) {
			res, err := r.api.ConfigList(ctx, client.ConfigListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list configs: %w", err)
			}
			return res.Items, nil
		},
		func(c swarm.Config) map[string]string { return c.Spec.Labels },
		func(c swarm.Config) string { return c.Spec.Name },
		func(c swarm.Config) []string { return []string{c.Spec.Name} },
		func(ctx context.Context, c swarm.Config) error {
			_, err := r.api.ConfigRemove(ctx, c.ID, client.ConfigRemoveOptions{})
			return err
		},
		"",
	)
}

func secretPrune(r *Runtime) pruneStep {
	return pruneKind(desired.KindSecret,
		func(ctx context.Context) ([]swarm.Secret, error) {
			res, err := r.api.SecretList(ctx, client.SecretListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list secrets: %w", err)
			}
			return res.Items, nil
		},
		func(s swarm.Secret) map[string]string { return s.Spec.Labels },
		func(s swarm.Secret) string { return s.Spec.Name },
		func(s swarm.Secret) []string { return []string{s.Spec.Name} },
		func(ctx context.Context, s swarm.Secret) error {
			_, err := r.api.SecretRemove(ctx, s.ID, client.SecretRemoveOptions{})
			return err
		},
		"",
	)
}

func volumePrune(r *Runtime) pruneStep {
	return pruneKind(desired.KindVolume,
		func(ctx context.Context) ([]volume.Volume, error) {
			res, err := r.api.VolumeList(ctx, client.VolumeListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list volumes: %w", err)
			}
			return res.Items, nil
		},
		func(v volume.Volume) map[string]string { return v.Labels },
		func(v volume.Volume) string { return v.Name },
		func(v volume.Volume) []string { return []string{v.Name} },
		nil,
		"volumes hold data and are never removed automatically; remove it manually if the data is no longer needed",
	)
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
