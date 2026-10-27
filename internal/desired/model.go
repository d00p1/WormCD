// Package desired defines the normalized, runtime-agnostic model of an
// application target. The same types describe desired state (rendered from
// Compose) and observed state (read from a runtime), so reconcile can diff
// them directly. See ADR-0002 and ADR-0003.
package desired

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Kind identifies the type of a runtime object.
type Kind string

const (
	KindService Kind = "Service"
	KindNetwork Kind = "Network"
	KindConfig  Kind = "Config"
	KindSecret  Kind = "Secret"
	KindVolume  Kind = "Volume"
)

// Key uniquely identifies an object inside one target. The diff engine
// (issue #4) uses it as the unit of planning.
type Key struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
}

// String renders the key as "Kind/name".
func (k Key) String() string { return string(k.Kind) + "/" + k.Name }

// Desired is the fully rendered state of one Application target.
type Desired struct {
	Application string    `json:"application"`
	Target      string    `json:"target"`
	Namespace   string    `json:"namespace"`
	Services    []Service `json:"services"`
	Networks    []Network `json:"networks"`
	Configs     []Config  `json:"configs"`
	Secrets     []Secret  `json:"secrets"`
	Volumes     []Volume  `json:"volumes"`
}

// ServiceMode is the normalized scheduling mode. Runtimes map it onto their
// own primitive; a runtime without a global mode can ignore it.
type ServiceMode string

const (
	ModeReplicated ServiceMode = "replicated"
	ModeGlobal     ServiceMode = "global"
)

// Service is a normalized container service.
type Service struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Mode            ServiceMode       `json:"mode"`
	Replicas        uint64            `json:"replicas"`
	EndpointMode    string            `json:"endpointMode"`
	Command         *[]string         `json:"command,omitempty"`
	Entrypoint      *[]string         `json:"entrypoint,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Hostname        string            `json:"hostname,omitempty"`
	User            string            `json:"user,omitempty"`
	WorkingDir      string            `json:"workingDir,omitempty"`
	StopSignal      string            `json:"stopSignal,omitempty"`
	StopGracePeriod time.Duration     `json:"stopGracePeriod,omitempty"`
	ReadOnly        bool              `json:"readOnly,omitempty"`
	Ports           []Port            `json:"ports,omitempty"`
	Networks        []ServiceNetwork  `json:"networks,omitempty"`
	Mounts          []Mount           `json:"mounts,omitempty"`
	Configs         []ConfigRef       `json:"configs,omitempty"`
	Secrets         []SecretRef       `json:"secrets,omitempty"`
	Resources       Resources         `json:"resources"`
	Placement       Placement         `json:"placement"`
	Restart         RestartPolicy     `json:"restart"`
	Healthcheck     *Healthcheck      `json:"healthcheck,omitempty"`
	Update          UpdateConfig      `json:"update"`
	Rollback        UpdateConfig      `json:"rollback"`
}

// ServiceNetwork attaches a service to a network.
type ServiceNetwork struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// Port is a published service port. Mode is the normalized publish mode
// (ingress or host); runtimes without routing mesh use host.
type Port struct {
	Name      string `json:"name,omitempty"`
	Target    uint32 `json:"target"`
	Published uint32 `json:"published"`
	Protocol  string `json:"protocol"`
	Mode      string `json:"mode"`
}

// MountType is the type of a service mount.
type MountType string

const (
	MountVolume MountType = "volume"
	MountBind   MountType = "bind"
	// MountTmpfs is only produced by runtime inspection: the renderer rejects
	// tmpfs mounts in v0, but an out-of-band tmpfs mount must still be visible
	// as drift.
	MountTmpfs MountType = "tmpfs"
)

// Mount is a service volume mount.
type Mount struct {
	Type     MountType `json:"type"`
	Source   string    `json:"source,omitempty"`
	Target   string    `json:"target"`
	ReadOnly bool      `json:"readOnly,omitempty"`
}

// ConfigRef attaches a config to a service.
type ConfigRef struct {
	Source string `json:"source"`
	Target string `json:"target"`
	UID    string `json:"uid,omitempty"`
	GID    string `json:"gid,omitempty"`
	Mode   uint32 `json:"mode"`
}

// SecretRef attaches a secret to a service.
type SecretRef struct {
	Source string `json:"source"`
	Target string `json:"target"`
	UID    string `json:"uid,omitempty"`
	GID    string `json:"gid,omitempty"`
	Mode   uint32 `json:"mode"`
}

// Resources holds service resource limits and reservations.
type Resources struct {
	Limits       ResourceLimits `json:"limits"`
	Reservations ResourceLimits `json:"reservations"`
}

// ResourceLimits is the normalized subset of resource constraints WormCD manages.
type ResourceLimits struct {
	NanoCPUs    int64 `json:"nanoCPUs,omitempty"`
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	Pids        int64 `json:"pids,omitempty"`
}

// Placement holds scheduling constraints.
type Placement struct {
	Constraints []string              `json:"constraints,omitempty"`
	Preferences []PlacementPreference `json:"preferences,omitempty"`
	MaxReplicas uint64                `json:"maxReplicas,omitempty"`
}

// PlacementPreference is a spread preference.
type PlacementPreference struct {
	Spread string `json:"spread"`
}

// RestartPolicy is the normalized restart policy.
type RestartPolicy struct {
	Condition   string        `json:"condition"`
	Delay       time.Duration `json:"delay"`
	MaxAttempts uint64        `json:"maxAttempts"`
	Window      time.Duration `json:"window"`
}

// Healthcheck is the normalized container healthcheck.
type Healthcheck struct {
	Test        []string      `json:"test"`
	Interval    time.Duration `json:"interval"`
	Timeout     time.Duration `json:"timeout"`
	StartPeriod time.Duration `json:"startPeriod"`
	Retries     int           `json:"retries"`
}

// UpdateConfig is the normalized rollout policy. Runtimes without rolling
// updates ignore it.
type UpdateConfig struct {
	Parallelism     uint64        `json:"parallelism"`
	Delay           time.Duration `json:"delay"`
	FailureAction   string        `json:"failureAction"`
	Monitor         time.Duration `json:"monitor"`
	MaxFailureRatio float32       `json:"maxFailureRatio"`
	Order           string        `json:"order"`
}

// Network is a Docker network.
type Network struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	DriverOpts map[string]string `json:"driverOpts,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	External   bool              `json:"external,omitempty"`
}

// Config is a Docker config object. Data is serialized so a stored revision
// can be re-applied without a fresh git checkout (see design 0001, section 6).
type Config struct {
	Name     string            `json:"name"`
	Source   string            `json:"source"`
	File     string            `json:"file,omitempty"`
	Hash     string            `json:"hash,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	External bool              `json:"external,omitempty"`
	Data     []byte            `json:"data,omitempty"`
}

// Secret is an external Docker secret.
type Secret struct {
	Name     string            `json:"name"`
	Source   string            `json:"source"`
	Labels   map[string]string `json:"labels,omitempty"`
	External bool              `json:"external"`
}

// Volume is a Docker volume.
type Volume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	DriverOpts map[string]string `json:"driverOpts,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	External   bool              `json:"external,omitempty"`
}

// Sort puts every collection in a deterministic order, in place. To get the
// canonical JSON of a state without changing it, use Canonical.
func (d *Desired) Sort() {
	SortServices(d.Services)
	SortNetworks(d.Networks)
	SortConfigs(d.Configs)
	SortSecrets(d.Secrets)
	SortVolumes(d.Volumes)
}

// Every sort compares a full key, so elements that compare equal are
// identical in their encoding and the output does not depend on input order.

// SortServices orders services by name and each service's nested collections.
func SortServices(services []Service) {
	slices.SortFunc(services, func(a, b Service) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), compareEncoded(a, b))
	})
	for i := range services {
		services[i].sort()
	}
}

// SortNetworks orders networks by name.
func SortNetworks(networks []Network) {
	slices.SortFunc(networks, func(a, b Network) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), compareEncoded(a, b))
	})
}

// SortConfigs orders configs by name.
func SortConfigs(configs []Config) {
	slices.SortFunc(configs, func(a, b Config) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), compareEncoded(a, b))
	})
}

// SortSecrets orders secrets by name.
func SortSecrets(secrets []Secret) {
	slices.SortFunc(secrets, func(a, b Secret) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), compareEncoded(a, b))
	})
}

// SortVolumes orders volumes by name.
func SortVolumes(volumes []Volume) {
	slices.SortFunc(volumes, func(a, b Volume) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), compareEncoded(a, b))
	})
}

func (s *Service) sort() {
	slices.SortFunc(s.Ports, comparePort)
	slices.SortFunc(s.Networks, compareServiceNetwork)
	slices.SortFunc(s.Mounts, compareMount)
	slices.SortFunc(s.Configs, compareConfigRef)
	slices.SortFunc(s.Secrets, compareSecretRef)
}

func comparePort(a, b Port) int {
	return cmp.Or(
		cmp.Compare(a.Target, b.Target),
		strings.Compare(a.Protocol, b.Protocol),
		cmp.Compare(a.Published, b.Published),
		strings.Compare(a.Mode, b.Mode),
		strings.Compare(a.Name, b.Name),
	)
}

func compareServiceNetwork(a, b ServiceNetwork) int {
	return cmp.Or(
		strings.Compare(a.Name, b.Name),
		slices.Compare(a.Aliases, b.Aliases),
	)
}

func compareMount(a, b Mount) int {
	return cmp.Or(
		strings.Compare(a.Target, b.Target),
		strings.Compare(string(a.Type), string(b.Type)),
		strings.Compare(a.Source, b.Source),
		compareBool(a.ReadOnly, b.ReadOnly),
	)
}

func compareConfigRef(a, b ConfigRef) int {
	return cmp.Or(
		strings.Compare(a.Target, b.Target),
		strings.Compare(a.Source, b.Source),
		strings.Compare(a.UID, b.UID),
		strings.Compare(a.GID, b.GID),
		cmp.Compare(a.Mode, b.Mode),
	)
}

func compareSecretRef(a, b SecretRef) int {
	return cmp.Or(
		strings.Compare(a.Target, b.Target),
		strings.Compare(a.Source, b.Source),
		strings.Compare(a.UID, b.UID),
		strings.Compare(a.GID, b.GID),
		cmp.Compare(a.Mode, b.Mode),
	)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}

// compareEncoded orders two values by their JSON encoding. It is the last
// tie-break for elements that agree on every key compared before it.
func compareEncoded(a, b any) int {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Compare(ja, jb)
}

// TargetNamespace is the prefix WormCD gives to every managed runtime object
// of a target. It is stable and derived only from the application and target
// names.
func TargetNamespace(application, target string) string {
	return sanitize("wormcd_" + application + "_" + target)
}

func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
