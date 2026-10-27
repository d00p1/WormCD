package swarm

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

// referenceIDs holds the Docker IDs of the configs and secrets a service
// references, keyed by name. Docker takes a reference by ID, so Apply looks the
// IDs up before it converts the service.
type referenceIDs struct {
	Configs map[string]string
	Secrets map[string]string
}

// serviceSpec converts a desired service into the spec Docker expects. It is
// the inverse of mapService: a spec converted from a desired service reads back
// as the same desired fields, so a converged service is not reported as drift.
// The service gets the ownership labels of scope on top of its own labels.
func serviceSpec(service desired.Service, scope runtime.Scope, ids referenceIDs) (swarm.ServiceSpec, error) {
	fail := func(err error) (swarm.ServiceSpec, error) {
		return swarm.ServiceSpec{}, fmt.Errorf("service %s: %w", service.Name, err)
	}
	mode, err := serviceMode(service)
	if err != nil {
		return fail(err)
	}
	mounts, err := mountSpecs(service.Mounts)
	if err != nil {
		return fail(err)
	}
	configs, err := configRefSpecs(service.Configs, ids.Configs)
	if err != nil {
		return fail(err)
	}
	secrets, err := secretRefSpecs(service.Secrets, ids.Secrets)
	if err != nil {
		return fail(err)
	}

	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   service.Name,
			Labels: desired.MergeLabels(service.Labels, scope.Application, scope.Target),
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:           service.Image,
				Command:         derefStrings(service.Entrypoint),
				Args:            derefStrings(service.Command),
				Env:             envList(service.Env),
				Hostname:        service.Hostname,
				User:            service.User,
				Dir:             service.WorkingDir,
				StopSignal:      service.StopSignal,
				StopGracePeriod: optionalDuration(service.StopGracePeriod),
				ReadOnly:        service.ReadOnly,
				Mounts:          mounts,
				Configs:         configs,
				Secrets:         secrets,
				Healthcheck:     healthcheckSpec(service.Healthcheck),
			},
			Resources:     resourceSpec(service.Resources),
			RestartPolicy: restartPolicySpec(service.Restart),
			Placement:     placementSpec(service.Placement),
			Networks:      networkSpecs(service.Networks),
		},
		Mode:           mode,
		UpdateConfig:   updateSpec(service.Update),
		RollbackConfig: updateSpec(service.Rollback),
		EndpointSpec: &swarm.EndpointSpec{
			Mode:  swarm.ResolutionMode(service.EndpointMode),
			Ports: portSpecs(service.Ports),
		},
	}, nil
}

func serviceMode(service desired.Service) (swarm.ServiceMode, error) {
	switch service.Mode {
	case desired.ModeReplicated:
		replicas := service.Replicas
		return swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}, nil
	case desired.ModeGlobal:
		return swarm.ServiceMode{Global: &swarm.GlobalService{}}, nil
	default:
		return swarm.ServiceMode{}, fmt.Errorf("mode %q is not supported", service.Mode)
	}
}

// mountSpecs converts volume and bind mounts. A tmpfs mount is refused: the
// renderer rejects it in v0, so one reaching Apply is a bug, not user input.
func mountSpecs(mounts []desired.Mount) ([]mount.Mount, error) {
	out := make([]mount.Mount, 0, len(mounts))
	for _, item := range mounts {
		var kind mount.Type
		switch item.Type {
		case desired.MountVolume:
			kind = mount.TypeVolume
		case desired.MountBind:
			kind = mount.TypeBind
		default:
			return nil, fmt.Errorf("mount %s: type %q is not supported", item.Target, item.Type)
		}
		out = append(out, mount.Mount{
			Type:     kind,
			Source:   item.Source,
			Target:   item.Target,
			ReadOnly: item.ReadOnly,
		})
	}
	return emptyToNil(out), nil
}

func configRefSpecs(refs []desired.ConfigRef, ids map[string]string) ([]*swarm.ConfigReference, error) {
	out := make([]*swarm.ConfigReference, 0, len(refs))
	for _, ref := range refs {
		id, ok := ids[ref.Source]
		if !ok {
			return nil, fmt.Errorf("config %s has no Docker ID", ref.Source)
		}
		out = append(out, &swarm.ConfigReference{
			ConfigID:   id,
			ConfigName: ref.Source,
			File: &swarm.ConfigReferenceFileTarget{
				Name: ref.Target,
				UID:  ref.UID,
				GID:  ref.GID,
				Mode: os.FileMode(ref.Mode),
			},
		})
	}
	return emptyToNil(out), nil
}

func secretRefSpecs(refs []desired.SecretRef, ids map[string]string) ([]*swarm.SecretReference, error) {
	out := make([]*swarm.SecretReference, 0, len(refs))
	for _, ref := range refs {
		id, ok := ids[ref.Source]
		if !ok {
			return nil, fmt.Errorf("secret %s has no Docker ID", ref.Source)
		}
		out = append(out, &swarm.SecretReference{
			SecretID:   id,
			SecretName: ref.Source,
			File: &swarm.SecretReferenceFileTarget{
				Name: ref.Target,
				UID:  ref.UID,
				GID:  ref.GID,
				Mode: os.FileMode(ref.Mode),
			},
		})
	}
	return emptyToNil(out), nil
}

// envList writes the environment as KEY=VALUE pairs in key order, so the same
// desired state always produces the same spec.
func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(env))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func portSpecs(ports []desired.Port) []swarm.PortConfig {
	out := make([]swarm.PortConfig, 0, len(ports))
	for _, port := range ports {
		out = append(out, swarm.PortConfig{
			Name:          port.Name,
			Protocol:      network.IPProtocol(port.Protocol),
			TargetPort:    port.Target,
			PublishedPort: port.Published,
			PublishMode:   swarm.PortConfigPublishMode(port.Mode),
		})
	}
	return emptyToNil(out)
}

func networkSpecs(networks []desired.ServiceNetwork) []swarm.NetworkAttachmentConfig {
	out := make([]swarm.NetworkAttachmentConfig, 0, len(networks))
	for _, item := range networks {
		out = append(out, swarm.NetworkAttachmentConfig{
			Target:  item.Name,
			Aliases: slices.Clone(item.Aliases),
		})
	}
	return emptyToNil(out)
}

func healthcheckSpec(healthcheck *desired.Healthcheck) *container.HealthConfig {
	if healthcheck == nil {
		return nil
	}
	return &container.HealthConfig{
		Test:        slices.Clone(healthcheck.Test),
		Interval:    healthcheck.Interval,
		Timeout:     healthcheck.Timeout,
		StartPeriod: healthcheck.StartPeriod,
		Retries:     healthcheck.Retries,
	}
}

func resourceSpec(resources desired.Resources) *swarm.ResourceRequirements {
	var out swarm.ResourceRequirements
	if resources.Limits != (desired.ResourceLimits{}) {
		out.Limits = &swarm.Limit{
			NanoCPUs:    resources.Limits.NanoCPUs,
			MemoryBytes: resources.Limits.MemoryBytes,
			Pids:        resources.Limits.Pids,
		}
	}
	if resources.Reservations != (desired.ResourceLimits{}) {
		out.Reservations = &swarm.Resources{
			NanoCPUs:    resources.Reservations.NanoCPUs,
			MemoryBytes: resources.Reservations.MemoryBytes,
		}
	}
	if out.Limits == nil && out.Reservations == nil {
		return nil
	}
	return &out
}

func restartPolicySpec(policy desired.RestartPolicy) *swarm.RestartPolicy {
	delay := policy.Delay
	maxAttempts := policy.MaxAttempts
	window := policy.Window
	return &swarm.RestartPolicy{
		Condition:   swarm.RestartPolicyCondition(policy.Condition),
		Delay:       &delay,
		MaxAttempts: &maxAttempts,
		Window:      &window,
	}
}

func placementSpec(placement desired.Placement) *swarm.Placement {
	if len(placement.Constraints) == 0 && len(placement.Preferences) == 0 && placement.MaxReplicas == 0 {
		return nil
	}
	out := &swarm.Placement{
		Constraints: slices.Clone(placement.Constraints),
		MaxReplicas: placement.MaxReplicas,
	}
	for _, preference := range placement.Preferences {
		out.Preferences = append(out.Preferences, swarm.PlacementPreference{
			Spread: &swarm.SpreadOver{SpreadDescriptor: preference.Spread},
		})
	}
	return out
}

func updateSpec(config desired.UpdateConfig) *swarm.UpdateConfig {
	return &swarm.UpdateConfig{
		Parallelism:     config.Parallelism,
		Delay:           config.Delay,
		FailureAction:   swarm.FailureAction(config.FailureAction),
		Monitor:         config.Monitor,
		MaxFailureRatio: config.MaxFailureRatio,
		Order:           swarm.UpdateOrder(config.Order),
	}
}

// derefStrings copies an optional list. A nil pointer and an empty list both
// mean "not set" in the desired model.
func derefStrings(values *[]string) []string {
	if values == nil {
		return nil
	}
	return slices.Clone(*values)
}

func optionalDuration(value time.Duration) *time.Duration {
	if value == 0 {
		return nil
	}
	return &value
}

func emptyToNil[T any](items []T) []T {
	if len(items) == 0 {
		return nil
	}
	return items
}
