package reconcile

import (
	"reflect"
	"sort"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
)

// Action is what apply must do with one object.
type Action string

const (
	Create Action = "Create"
	Update Action = "Update"
	Delete Action = "Delete"
	Noop   Action = "Noop"
)

// Entry is one object in a plan. Key is "Kind/name"; Kind and Name are
// repeated so machine consumers do not have to split it.
type Entry struct {
	Key     string       `json:"key"`
	Kind    desired.Kind `json:"kind"`
	Name    string       `json:"name"`
	Action  Action       `json:"action"`
	Changes []string     `json:"changes,omitempty"`
}

// Summary counts the actions in a plan.
type Summary struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Delete int `json:"delete"`
	Noop   int `json:"noop"`
}

// Plan is the ordered, read-only difference between desired and observed
// state. Entries are grouped by kind in apply order and sorted by name inside
// a kind, so the same inputs always produce the same plan.
type Plan struct {
	Application string  `json:"application"`
	Target      string  `json:"target"`
	Namespace   string  `json:"namespace"`
	Summary     Summary `json:"summary"`
	Entries     []Entry `json:"entries"`
}

// HasChanges reports whether applying the plan would mutate the cluster.
func (p Plan) HasChanges() bool {
	return p.Summary.Create > 0 || p.Summary.Update > 0 || p.Summary.Delete > 0
}

// Diff compares desired and observed state for one target. It is a pure
// function: it never contacts a runtime, so it cannot mutate the cluster.
func Diff(want *desired.Desired, have runtime.Observed) Plan {
	plan := Plan{
		Application: want.Application,
		Target:      want.Target,
		Namespace:   want.Namespace,
		Entries:     make([]Entry, 0, 16),
	}

	// Entries are grouped in dependency order, matching apply. Apply must
	// still delete services before their dependencies; the order here is
	// only about a stable, readable plan.
	diffObjects(&plan, desired.KindNetwork, want.Networks, have.Networks, networkName, networkManaged, networkChanges)
	diffObjects(&plan, desired.KindConfig, want.Configs, have.Configs, configName, configManaged, configChanges)
	diffObjects(&plan, desired.KindSecret, want.Secrets, have.Secrets, secretName, secretManaged, secretChanges)
	diffObjects(&plan, desired.KindVolume, want.Volumes, have.Volumes, volumeName, volumeManaged, volumeChanges)
	diffObjects(&plan, desired.KindService, want.Services, have.Services, serviceName, nil, serviceChanges)

	return plan
}

func diffObjects[T any](
	plan *Plan,
	kind desired.Kind,
	want, have []T,
	name func(T) string,
	managed func(T) bool,
	changes func(want, have T) []string,
) {
	desiredByName := make(map[string]T, len(want))
	for _, item := range want {
		if managed != nil && !managed(item) {
			continue
		}
		desiredByName[name(item)] = item
	}

	observedByName := make(map[string]T, len(have))
	for _, item := range have {
		observedByName[name(item)] = item
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
		wantItem, inDesired := desiredByName[itemName]
		haveItem, inObserved := observedByName[itemName]

		entry := Entry{
			Key:  desired.Key{Kind: kind, Name: itemName}.String(),
			Kind: kind,
			Name: itemName,
		}
		switch {
		case inDesired && inObserved:
			entry.Changes = changes(wantItem, haveItem)
			if len(entry.Changes) == 0 {
				entry.Action = Noop
			} else {
				entry.Action = Update
			}
		case inDesired:
			entry.Action = Create
		default:
			entry.Action = Delete
		}
		plan.add(entry)
	}
}

func (p *Plan) add(entry Entry) {
	p.Entries = append(p.Entries, entry)
	switch entry.Action {
	case Create:
		p.Summary.Create++
	case Update:
		p.Summary.Update++
	case Delete:
		p.Summary.Delete++
	case Noop:
		p.Summary.Noop++
	}
}

// External objects are referenced by a target but are not owned by it: WormCD
// must neither create nor delete them, so they never enter a plan.

func networkName(network desired.Network) string { return network.Name }
func networkManaged(network desired.Network) bool {
	return !network.External
}

func configName(config desired.Config) string { return config.Name }
func configManaged(config desired.Config) bool {
	return !config.External
}

func secretName(secret desired.Secret) string { return secret.Name }
func secretManaged(secret desired.Secret) bool {
	return !secret.External
}

func volumeName(volume desired.Volume) string { return volume.Name }
func volumeManaged(volume desired.Volume) bool {
	return !volume.External
}

func serviceName(service desired.Service) string { return service.Name }

func networkChanges(want, have desired.Network) []string {
	var out []string
	addChange(&out, "driver", want.Driver, have.Driver)
	addChange(&out, "driverOpts", nonEmptyMap(want.DriverOpts), nonEmptyMap(have.DriverOpts))
	addChange(&out, "attachable", want.Attachable, have.Attachable)
	addChange(&out, "internal", want.Internal, have.Internal)
	addMapChanges(&out, "labels", want.Labels, have.Labels)
	return out
}

func configChanges(want, have desired.Config) []string {
	var out []string
	addMapChanges(&out, "labels", want.Labels, have.Labels)
	return out
}

func secretChanges(want, have desired.Secret) []string {
	var out []string
	addMapChanges(&out, "labels", want.Labels, have.Labels)
	return out
}

func volumeChanges(want, have desired.Volume) []string {
	want.DriverOpts = nonEmptyMap(want.DriverOpts)
	have.DriverOpts = nonEmptyMap(have.DriverOpts)

	var out []string
	addChange(&out, "driver", want.Driver, have.Driver)
	addChange(&out, "driverOpts", want.DriverOpts, have.DriverOpts)
	addMapChanges(&out, "labels", want.Labels, have.Labels)
	return out
}

func serviceChanges(want, have desired.Service) []string {
	want = canonicalService(want)
	have = canonicalService(have)

	var out []string
	addChange(&out, "image", want.Image, have.Image)
	addChange(&out, "mode", want.Mode, have.Mode)
	addChange(&out, "replicas", want.Replicas, have.Replicas)
	addChange(&out, "endpointMode", want.EndpointMode, have.EndpointMode)
	addChange(&out, "command", want.Command, have.Command)
	addChange(&out, "entrypoint", want.Entrypoint, have.Entrypoint)
	addMapChanges(&out, "env", want.Env, have.Env)
	addMapChanges(&out, "labels", want.Labels, have.Labels)
	addChange(&out, "hostname", want.Hostname, have.Hostname)
	addChange(&out, "user", want.User, have.User)
	addChange(&out, "workingDir", want.WorkingDir, have.WorkingDir)
	addChange(&out, "stopSignal", want.StopSignal, have.StopSignal)
	addChange(&out, "stopGracePeriod", want.StopGracePeriod, have.StopGracePeriod)
	addChange(&out, "readOnly", want.ReadOnly, have.ReadOnly)
	addChange(&out, "ports", want.Ports, have.Ports)
	addChange(&out, "networks", want.Networks, have.Networks)
	addChange(&out, "mounts", want.Mounts, have.Mounts)
	addChange(&out, "configs", want.Configs, have.Configs)
	addChange(&out, "secrets", want.Secrets, have.Secrets)
	addChange(&out, "resources", want.Resources, have.Resources)
	addChange(&out, "placement", want.Placement, have.Placement)
	addChange(&out, "restart", want.Restart, have.Restart)
	addChange(&out, "healthcheck", want.Healthcheck, have.Healthcheck)
	addChange(&out, "update", want.Update, have.Update)
	addChange(&out, "rollback", want.Rollback, have.Rollback)
	return out
}

func addChange[T any](out *[]string, field string, want, have T) {
	if !reflect.DeepEqual(want, have) {
		*out = append(*out, field)
	}
}

func addMapChanges(out *[]string, field string, want, have map[string]string) {
	keys := make(map[string]struct{}, len(want)+len(have))
	for key := range want {
		keys[key] = struct{}{}
	}
	for key := range have {
		keys[key] = struct{}{}
	}

	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)

	for _, key := range names {
		if want[key] != have[key] {
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
