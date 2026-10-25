// Package swarm implements the Runtime interface for Docker Swarm.
// See ADR-0003 and issue #3.
package swarm

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// api is the subset of the Docker SDK used by Inspect. It exists so tests can
// use a fake without a live daemon.
type api interface {
	ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error)
	NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error)
	ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error)
	SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error)
	VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error)
}

// Runtime reads Swarm state through the Docker Engine API.
type Runtime struct {
	api api
}

// New returns a Swarm runtime backed by a Docker client.
func New(api *client.Client) *Runtime {
	return newRuntime(api)
}

func newRuntime(api api) *Runtime {
	return &Runtime{api: api}
}

// Kind returns the runtime kind.
func (r *Runtime) Kind() runtime.Kind {
	return runtime.KindSwarm
}

// Inspect returns the observed state owned by scope. It only performs
// list operations and never mutates the cluster.
func (r *Runtime) Inspect(ctx context.Context, scope runtime.Scope) (runtime.Observed, error) {
	if scope.Application == "" || scope.Target == "" {
		return runtime.Observed{}, fmt.Errorf("scope application and target are required")
	}

	services, err := r.api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return runtime.Observed{}, fmt.Errorf("list services: %w", err)
	}
	networks, err := r.api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return runtime.Observed{}, fmt.Errorf("list networks: %w", err)
	}
	configs, err := r.api.ConfigList(ctx, client.ConfigListOptions{})
	if err != nil {
		return runtime.Observed{}, fmt.Errorf("list configs: %w", err)
	}
	secrets, err := r.api.SecretList(ctx, client.SecretListOptions{})
	if err != nil {
		return runtime.Observed{}, fmt.Errorf("list secrets: %w", err)
	}
	volumes, err := r.api.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return runtime.Observed{}, fmt.Errorf("list volumes: %w", err)
	}

	observed := runtime.Observed{
		Application: scope.Application,
		Target:      scope.Target,
		Namespace:   desired.TargetNamespace(scope.Application, scope.Target),
	}

	networkNames := make(map[string]string, len(networks.Items))
	for _, item := range networks.Items {
		networkNames[item.ID] = item.Name
	}
	for _, item := range filterByScope(networks.Items, func(n network.Summary) map[string]string { return n.Labels }, scope) {
		driver := item.Driver
		if driver == "" {
			driver = "overlay"
		}
		observed.Networks = append(observed.Networks, desired.Network{
			Name:       item.Name,
			Driver:     driver,
			DriverOpts: maps.Clone(item.Options),
			Attachable: item.Attachable,
			Internal:   item.Internal,
			Labels:     maps.Clone(item.Labels),
		})
	}

	for _, item := range filterByScope(services.Items, func(s swarm.Service) map[string]string { return s.Spec.Labels }, scope) {
		mapped, err := mapService(item, networkNames)
		if err != nil {
			return runtime.Observed{}, err
		}
		observed.Services = append(observed.Services, mapped)
	}

	for _, item := range filterByScope(configs.Items, func(c swarm.Config) map[string]string { return c.Spec.Labels }, scope) {
		observed.Configs = append(observed.Configs, desired.Config{
			Name:   item.Spec.Name,
			Labels: maps.Clone(item.Spec.Labels),
		})
	}

	for _, item := range filterByScope(secrets.Items, func(s swarm.Secret) map[string]string { return s.Spec.Labels }, scope) {
		observed.Secrets = append(observed.Secrets, desired.Secret{
			Name:   item.Spec.Name,
			Labels: maps.Clone(item.Spec.Labels),
		})
	}

	for _, item := range filterByScope(volumes.Items, func(v volume.Volume) map[string]string { return v.Labels }, scope) {
		driver := item.Driver
		if driver == "" {
			driver = "local"
		}
		observed.Volumes = append(observed.Volumes, desired.Volume{
			Name:       item.Name,
			Driver:     driver,
			DriverOpts: maps.Clone(item.Options),
			Labels:     maps.Clone(item.Labels),
		})
	}

	observed.Sort()
	return observed, nil
}

func inScope(labels map[string]string, scope runtime.Scope) bool {
	return labels[desired.LabelApplication] == scope.Application &&
		labels[desired.LabelTarget] == scope.Target
}

func filterByScope[T any](items []T, labels func(T) map[string]string, scope runtime.Scope) []T {
	out := make([]T, 0, len(items))
	for _, item := range items {
		if inScope(labels(item), scope) {
			out = append(out, item)
		}
	}
	return out
}

func mapService(service swarm.Service, networkNames map[string]string) (desired.Service, error) {
	spec := service.Spec
	if spec.Mode.ReplicatedJob != nil || spec.Mode.GlobalJob != nil {
		return desired.Service{}, fmt.Errorf("service %q: job modes are not supported", spec.Name)
	}
	containerSpec := spec.TaskTemplate.ContainerSpec
	if containerSpec == nil {
		return desired.Service{}, fmt.Errorf("service %q: container spec is missing", spec.Name)
	}

	out := desired.Service{
		Name:         spec.Name,
		Image:        containerSpec.Image,
		Mode:         desired.ModeReplicated,
		Replicas:     desired.DefaultReplicas,
		EndpointMode: desired.DefaultEndpointMode,
		Command:      stringsPtr(containerSpec.Args),
		Entrypoint:   stringsPtr(containerSpec.Command),
		Env:          envMap(containerSpec.Env),
		Labels:       maps.Clone(spec.Labels),
		Hostname:     containerSpec.Hostname,
		User:         containerSpec.User,
		WorkingDir:   containerSpec.Dir,
		StopSignal:   containerSpec.StopSignal,
		ReadOnly:     containerSpec.ReadOnly,
		Ports:        nil,
		Networks:     mapNetworks(spec.TaskTemplate.Networks, networkNames),
		Mounts:       mapMounts(containerSpec.Mounts),
		Configs:      mapConfigs(containerSpec.Configs),
		Secrets:      mapSecrets(containerSpec.Secrets),
		Resources:    mapResources(spec.TaskTemplate.Resources),
		Placement:    mapPlacement(spec.TaskTemplate.Placement),
		Restart:      mapRestart(spec.TaskTemplate.RestartPolicy),
		Healthcheck:  mapHealthcheck(containerSpec.Healthcheck),
		Update:       mapUpdate(spec.UpdateConfig),
		Rollback:     mapUpdate(spec.RollbackConfig),
	}
	if containerSpec.StopGracePeriod != nil {
		out.StopGracePeriod = *containerSpec.StopGracePeriod
	}

	switch {
	case spec.Mode.Global != nil:
		out.Mode = desired.ModeGlobal
		out.Replicas = 0
	case spec.Mode.Replicated != nil:
		out.Mode = desired.ModeReplicated
		if spec.Mode.Replicated.Replicas != nil {
			out.Replicas = *spec.Mode.Replicated.Replicas
		}
	default:
		// Keep the replicated defaults set above.
	}

	if spec.EndpointSpec != nil {
		if spec.EndpointSpec.Mode != "" {
			out.EndpointMode = string(spec.EndpointSpec.Mode)
		}
		out.Ports = mapPorts(spec.EndpointSpec.Ports)
	}

	return out, nil
}

func stringsPtr(values []string) *[]string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	return &out
}

func envMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		out[key] = value
	}
	return out
}

func mapPorts(ports []swarm.PortConfig) []desired.Port {
	if len(ports) == 0 {
		return nil
	}
	out := make([]desired.Port, 0, len(ports))
	for _, port := range ports {
		protocol := string(port.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		mode := string(port.PublishMode)
		if mode == "" {
			mode = "ingress"
		}
		out = append(out, desired.Port{
			Name:      port.Name,
			Target:    port.TargetPort,
			Published: port.PublishedPort,
			Protocol:  protocol,
			Mode:      mode,
		})
	}
	return out
}

func mapNetworks(attachments []swarm.NetworkAttachmentConfig, names map[string]string) []desired.ServiceNetwork {
	if len(attachments) == 0 {
		return nil
	}
	out := make([]desired.ServiceNetwork, 0, len(attachments))
	for _, attachment := range attachments {
		name := names[attachment.Target]
		if name == "" {
			name = attachment.Target
		}
		out = append(out, desired.ServiceNetwork{
			Name:    name,
			Aliases: append([]string(nil), attachment.Aliases...),
		})
	}
	return out
}

func mapMounts(mounts []mount.Mount) []desired.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]desired.Mount, 0, len(mounts))
	for _, item := range mounts {
		var kind desired.MountType
		switch item.Type {
		case mount.TypeVolume:
			kind = desired.MountVolume
		case mount.TypeBind:
			kind = desired.MountBind
		case mount.TypeTmpfs:
			kind = desired.MountTmpfs
		default:
			continue
		}
		out = append(out, desired.Mount{
			Type:     kind,
			Source:   item.Source,
			Target:   item.Target,
			ReadOnly: item.ReadOnly,
		})
	}
	return out
}

func mapConfigs(refs []*swarm.ConfigReference) []desired.ConfigRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]desired.ConfigRef, 0, len(refs))
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		target := "/" + ref.ConfigName
		var uid, gid string
		mode := uint32(0o444)
		if ref.File != nil {
			target = ref.File.Name
			uid = ref.File.UID
			gid = ref.File.GID
			mode = uint32(ref.File.Mode)
		}
		out = append(out, desired.ConfigRef{
			Source: ref.ConfigName,
			Target: target,
			UID:    uid,
			GID:    gid,
			Mode:   mode,
		})
	}
	return out
}

func mapSecrets(refs []*swarm.SecretReference) []desired.SecretRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]desired.SecretRef, 0, len(refs))
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		target := "/run/secrets/" + ref.SecretName
		var uid, gid string
		mode := uint32(0o444)
		if ref.File != nil {
			target = ref.File.Name
			uid = ref.File.UID
			gid = ref.File.GID
			mode = uint32(ref.File.Mode)
		}
		out = append(out, desired.SecretRef{
			Source: ref.SecretName,
			Target: target,
			UID:    uid,
			GID:    gid,
			Mode:   mode,
		})
	}
	return out
}

func mapHealthcheck(healthcheck *container.HealthConfig) *desired.Healthcheck {
	if healthcheck == nil || len(healthcheck.Test) == 0 {
		return nil
	}
	if len(healthcheck.Test) == 1 && healthcheck.Test[0] == "NONE" {
		return nil
	}

	defaults := desired.DefaultHealthcheck()
	defaults.Test = append([]string(nil), healthcheck.Test...)
	out := &defaults
	if healthcheck.Interval != 0 {
		out.Interval = healthcheck.Interval
	}
	if healthcheck.Timeout != 0 {
		out.Timeout = healthcheck.Timeout
	}
	if healthcheck.StartPeriod != 0 {
		out.StartPeriod = healthcheck.StartPeriod
	}
	if healthcheck.Retries != 0 {
		out.Retries = healthcheck.Retries
	}
	return out
}

func mapResources(requirements *swarm.ResourceRequirements) desired.Resources {
	if requirements == nil {
		return desired.Resources{}
	}
	var out desired.Resources
	if requirements.Limits != nil {
		out.Limits = desired.ResourceLimits{
			NanoCPUs:    requirements.Limits.NanoCPUs,
			MemoryBytes: requirements.Limits.MemoryBytes,
			Pids:        requirements.Limits.Pids,
		}
	}
	if requirements.Reservations != nil {
		out.Reservations = desired.ResourceLimits{
			NanoCPUs:    requirements.Reservations.NanoCPUs,
			MemoryBytes: requirements.Reservations.MemoryBytes,
		}
	}
	return out
}

func mapPlacement(placement *swarm.Placement) desired.Placement {
	if placement == nil {
		return desired.Placement{}
	}
	out := desired.Placement{
		Constraints: append([]string(nil), placement.Constraints...),
		MaxReplicas: placement.MaxReplicas,
	}
	for _, preference := range placement.Preferences {
		if preference.Spread == nil {
			continue
		}
		out.Preferences = append(out.Preferences, desired.PlacementPreference{
			Spread: preference.Spread.SpreadDescriptor,
		})
	}
	return out
}

func mapRestart(policy *swarm.RestartPolicy) desired.RestartPolicy {
	out := desired.DefaultRestartPolicy()
	if policy == nil {
		return out
	}
	if policy.Condition != "" {
		out.Condition = string(policy.Condition)
	}
	if policy.Delay != nil {
		out.Delay = *policy.Delay
	}
	if policy.MaxAttempts != nil {
		out.MaxAttempts = *policy.MaxAttempts
	}
	if policy.Window != nil {
		out.Window = *policy.Window
	}
	return out
}

func mapUpdate(config *swarm.UpdateConfig) desired.UpdateConfig {
	out := desired.DefaultUpdateConfig()
	if config == nil {
		return out
	}
	out.Parallelism = config.Parallelism
	out.Delay = config.Delay
	if config.FailureAction != "" {
		out.FailureAction = string(config.FailureAction)
	}
	if config.Monitor != 0 {
		out.Monitor = config.Monitor
	}
	out.MaxFailureRatio = config.MaxFailureRatio
	if config.Order != "" {
		out.Order = string(config.Order)
	}
	return out
}
