// Package desired defines the normalized, runtime-agnostic model of an
// application target. The same types describe desired state (rendered from
// Compose) and observed state (read from a runtime), so reconcile can diff
// them directly. See ADR-0002 and ADR-0003.
package desired

import (
	"sort"
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

// Sort puts every collection in a deterministic order. Callers should invoke
// it before marshaling or diffing.
func (d *Desired) Sort() {
	SortServices(d.Services)
	SortNetworks(d.Networks)
	SortConfigs(d.Configs)
	SortSecrets(d.Secrets)
	SortVolumes(d.Volumes)
}

// SortServices orders services by name and each service's nested collections.
func SortServices(services []Service) {
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	for i := range services {
		services[i].sort()
	}
}

// SortNetworks orders networks by name.
func SortNetworks(networks []Network) {
	sort.Slice(networks, func(i, j int) bool { return networks[i].Name < networks[j].Name })
}

// SortConfigs orders configs by name.
func SortConfigs(configs []Config) {
	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
}

// SortSecrets orders secrets by name.
func SortSecrets(secrets []Secret) {
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })
}

// SortVolumes orders volumes by name.
func SortVolumes(volumes []Volume) {
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
}

func (s *Service) sort() {
	sort.Slice(s.Ports, func(i, j int) bool {
		a, b := s.Ports[i], s.Ports[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.Published < b.Published
	})
	sort.Slice(s.Networks, func(i, j int) bool { return s.Networks[i].Name < s.Networks[j].Name })
	sort.Slice(s.Mounts, func(i, j int) bool { return s.Mounts[i].Target < s.Mounts[j].Target })
	sort.Slice(s.Configs, func(i, j int) bool { return s.Configs[i].Target < s.Configs[j].Target })
	sort.Slice(s.Secrets, func(i, j int) bool { return s.Secrets[i].Target < s.Secrets[j].Target })
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
