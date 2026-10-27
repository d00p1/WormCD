package reconcile

import (
	"reflect"
	"sort"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
)

// Diff compares desired and observed state for one target. It is a pure
// function: it never contacts a runtime, so it cannot mutate the cluster.
func Diff(desiredState *desired.Desired, observedState runtime.Observed) runtime.Plan {
	plan := runtime.Plan{
		Application: desiredState.Application,
		Target:      desiredState.Target,
		Namespace:   desiredState.Namespace,
		Entries:     make([]runtime.Entry, 0, 16),
		Desired:     *desiredState,
	}

	// Entries are grouped in dependency order, matching apply. Apply must
	// still delete services before their dependencies; the order here is
	// only about a stable, readable plan.
	diffObjects(&plan, networkKind, desiredState.Networks, observedState.Networks)
	diffObjects(&plan, configKind, desiredState.Configs, observedState.Configs)
	diffObjects(&plan, secretKind, desiredState.Secrets, observedState.Secrets)
	diffObjects(&plan, volumeKind, desiredState.Volumes, observedState.Volumes)
	diffObjects(&plan, serviceKind, desiredState.Services, observedState.Services)

	return plan
}

// objectKind groups everything the diff needs for one kind of object. A nil
// changes means the kind has no in-place updates: a matching name is a match.
type objectKind[T desired.Managed] struct {
	kind    desired.Kind
	name    func(T) string
	changes func(desiredObject, observedObject T) []string
}

var (
	networkKind = objectKind[desired.Network]{
		kind:    desired.KindNetwork,
		name:    networkName,
		changes: networkChanges,
	}
	// Config names carry a hash of the content, so a matching name already means
	// matching data. Labels are the one thing Docker can change in place
	// (ConfigUpdate), so they are the only config changes.
	configKind = objectKind[desired.Config]{
		kind:    desired.KindConfig,
		name:    configName,
		changes: configChanges,
	}
	// Managed secrets cannot be created in v0 (see swarm.Apply), so there is
	// nothing to compare.
	secretKind = objectKind[desired.Secret]{
		kind: desired.KindSecret,
		name: secretName,
	}
	volumeKind = objectKind[desired.Volume]{
		kind:    desired.KindVolume,
		name:    volumeName,
		changes: volumeChanges,
	}
	serviceKind = objectKind[desired.Service]{
		kind:    desired.KindService,
		name:    serviceName,
		changes: serviceChanges,
	}
)

func diffObjects[T desired.Managed](plan *runtime.Plan, objectType objectKind[T], desiredObjects, observedObjects []T) {
	desiredByName := make(map[string]T, len(desiredObjects))
	for _, object := range desiredObjects {
		if !object.Managed() {
			continue
		}
		desiredByName[objectType.name(object)] = object
	}

	observedByName := make(map[string]T, len(observedObjects))
	for _, object := range observedObjects {
		if !object.Managed() {
			continue
		}
		observedByName[objectType.name(object)] = object
	}

	names := make([]string, 0, len(desiredByName)+len(observedByName))
	for itemName := range desiredByName {
		names = append(names, itemName)
	}
	for itemName := range observedByName {
		if _, ok := desiredByName[itemName]; !ok {
			names = append(names, itemName)
		}
	}
	sort.Strings(names)

	for _, itemName := range names {
		desiredObject, inDesired := desiredByName[itemName]
		observedObject, inObserved := observedByName[itemName]

		entry := runtime.Entry{
			Key: desired.Key{Kind: objectType.kind, Name: itemName}.String(),
		}
		switch {
		case inDesired && inObserved:
			if objectType.changes != nil {
				entry.Changes = objectType.changes(desiredObject, observedObject)
			}
			if len(entry.Changes) == 0 {
				entry.Action = runtime.Noop
			} else {
				entry.Action = runtime.Update
			}
		case inDesired:
			entry.Action = runtime.Create
		default:
			entry.Action = runtime.Delete
		}
		plan.Add(entry)
	}
}

func networkName(network desired.Network) string { return network.Name }
func configName(config desired.Config) string    { return config.Name }
func secretName(secret desired.Secret) string    { return secret.Name }
func volumeName(volume desired.Volume) string    { return volume.Name }
func serviceName(service desired.Service) string {
	return service.Name
}

func networkChanges(desiredNetwork, observedNetwork desired.Network) []string {
	var out []string
	addChange(&out, "driver", desiredNetwork.Driver, observedNetwork.Driver)
	addChange(&out, "driverOpts",
		nonEmptyMap(desiredNetwork.DriverOpts),
		nonEmptyMap(observedNetwork.DriverOpts),
	)
	addChange(&out, "attachable", desiredNetwork.Attachable, observedNetwork.Attachable)
	addChange(&out, "internal", desiredNetwork.Internal, observedNetwork.Internal)
	addMapChanges(&out, "labels", desiredNetwork.Labels, observedNetwork.Labels)
	return out
}

func configChanges(desiredConfig, observedConfig desired.Config) []string {
	var out []string
	addMapChanges(&out, "labels", desiredConfig.Labels, observedConfig.Labels)
	return out
}

func volumeChanges(desiredVolume, observedVolume desired.Volume) []string {
	var out []string
	addChange(&out, "driver", desiredVolume.Driver, observedVolume.Driver)
	addChange(&out, "driverOpts",
		nonEmptyMap(desiredVolume.DriverOpts),
		nonEmptyMap(observedVolume.DriverOpts),
	)
	addMapChanges(&out, "labels", desiredVolume.Labels, observedVolume.Labels)
	return out
}

func serviceChanges(desiredService, observedService desired.Service) []string {
	desiredService = canonicalService(desiredService)
	observedService = canonicalService(observedService)

	var out []string
	addChange(&out, "image", desiredService.Image, observedService.Image)
	addChange(&out, "mode", desiredService.Mode, observedService.Mode)
	addChange(&out, "replicas", desiredService.Replicas, observedService.Replicas)
	addChange(&out, "endpointMode", desiredService.EndpointMode, observedService.EndpointMode)
	addChange(&out, "command", desiredService.Command, observedService.Command)
	addChange(&out, "entrypoint", desiredService.Entrypoint, observedService.Entrypoint)
	addMapChanges(&out, "env", desiredService.Env, observedService.Env)
	addMapChanges(&out, "labels", desiredService.Labels, observedService.Labels)
	addChange(&out, "hostname", desiredService.Hostname, observedService.Hostname)
	addChange(&out, "user", desiredService.User, observedService.User)
	addChange(&out, "workingDir", desiredService.WorkingDir, observedService.WorkingDir)
	addChange(&out, "stopSignal", desiredService.StopSignal, observedService.StopSignal)
	addChange(&out, "stopGracePeriod", desiredService.StopGracePeriod, observedService.StopGracePeriod)
	addChange(&out, "readOnly", desiredService.ReadOnly, observedService.ReadOnly)
	addChange(&out, "ports", desiredService.Ports, observedService.Ports)
	addChange(&out, "networks", desiredService.Networks, observedService.Networks)
	addChange(&out, "mounts", desiredService.Mounts, observedService.Mounts)
	addChange(&out, "configs", desiredService.Configs, observedService.Configs)
	addChange(&out, "secrets", desiredService.Secrets, observedService.Secrets)
	addChange(&out, "resources", desiredService.Resources, observedService.Resources)
	addChange(&out, "placement", desiredService.Placement, observedService.Placement)
	addChange(&out, "restart", desiredService.Restart, observedService.Restart)
	addChange(&out, "healthcheck", desiredService.Healthcheck, observedService.Healthcheck)
	addChange(&out, "update", desiredService.Update, observedService.Update)
	addChange(&out, "rollback", desiredService.Rollback, observedService.Rollback)
	return out
}

func addChange[T any](out *[]string, field string, desiredValue, observedValue T) {
	if !reflect.DeepEqual(desiredValue, observedValue) {
		*out = append(*out, field)
	}
}

func addMapChanges(out *[]string, field string, desiredValues, observedValues map[string]string) {
	keys := make(map[string]struct{}, len(desiredValues)+len(observedValues))
	for key := range desiredValues {
		keys[key] = struct{}{}
	}
	for key := range observedValues {
		keys[key] = struct{}{}
	}

	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)

	for _, key := range names {
		if desiredValues[key] != observedValues[key] {
			*out = append(*out, field+"."+key)
		}
	}
}

// canonicalService removes nil/empty differences that carry no meaning. The
// renderer and the runtime reader represent an empty collection differently
// (nil vs an allocated empty slice), and comparing those directly would report
// drift for every service.
func canonicalService(service desired.Service) desired.Service {
	service.Command = nonEmptyPtr(service.Command)
	service.Entrypoint = nonEmptyPtr(service.Entrypoint)
	service.Env = nonEmptyMap(service.Env)
	service.Labels = nonEmptyMap(service.Labels)
	service.Ports = nonEmptySlice(service.Ports)
	service.Networks = nonEmptySlice(service.Networks)
	service.Mounts = nonEmptySlice(service.Mounts)
	service.Configs = nonEmptySlice(service.Configs)
	service.Secrets = nonEmptySlice(service.Secrets)
	service.Placement.Constraints = nonEmptySlice(service.Placement.Constraints)
	service.Placement.Preferences = nonEmptySlice(service.Placement.Preferences)
	if service.Healthcheck != nil {
		service.Healthcheck.Test = nonEmptySlice(service.Healthcheck.Test)
	}
	return service
}

func nonEmptyPtr(values *[]string) *[]string {
	if values == nil || len(*values) == 0 {
		return nil
	}
	return values
}

func nonEmptySlice[T any](values []T) []T {
	if len(values) == 0 {
		return nil
	}
	return values
}

func nonEmptyMap[K comparable, V any](values map[K]V) map[K]V {
	if len(values) == 0 {
		return nil
	}
	return values
}
