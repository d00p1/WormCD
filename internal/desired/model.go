// Package desired defines the normalized, runtime-agnostic model of an
// application target. The same types describe desired state (rendered from
// Compose) and observed state (read from a runtime), so reconcile can diff
// them directly. See ADR-0002 and ADR-0003.
package desired

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
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

// Sort puts every collection in a deterministic order, in place. If any element
// that needs a tie-break cannot be encoded, Sort returns an error and changes
// nothing. To get the canonical JSON of a state without changing it, use
// Canonical.
func (d *Desired) Sort() error {
	services, err := planServices(d.Services)
	if err != nil {
		return err
	}
	networks, err := planNetworks(d.Networks)
	if err != nil {
		return err
	}
	configs, err := planConfigs(d.Configs)
	if err != nil {
		return err
	}
	secrets, err := planSecrets(d.Secrets)
	if err != nil {
		return err
	}
	volumes, err := planVolumes(d.Volumes)
	if err != nil {
		return err
	}
	services()
	networks()
	configs()
	secrets()
	volumes()
	return nil
}

// Every sort compares a full key, so elements that compare equal are
// identical in their encoding and the output does not depend on input order.

// SortServices orders services by name and each service's nested collections.
func SortServices(services []Service) error {
	sortServices, err := planServices(services)
	if err != nil {
		return err
	}
	sortServices()
	return nil
}

// SortNetworks orders networks by name.
func SortNetworks(networks []Network) error {
	sortNetworks, err := planNetworks(networks)
	if err != nil {
		return err
	}
	sortNetworks()
	return nil
}

// SortConfigs orders configs by name.
func SortConfigs(configs []Config) error {
	sortConfigs, err := planConfigs(configs)
	if err != nil {
		return err
	}
	sortConfigs()
	return nil
}

// SortSecrets orders secrets by name.
func SortSecrets(secrets []Secret) error {
	sortSecrets, err := planSecrets(secrets)
	if err != nil {
		return err
	}
	sortSecrets()
	return nil
}

// SortVolumes orders volumes by name.
func SortVolumes(volumes []Volume) error {
	sortVolumes, err := planVolumes(volumes)
	if err != nil {
		return err
	}
	sortVolumes()
	return nil
}

// planNetworks, planConfigs, planSecrets and planVolumes are planSort for
// each kind, named by its Name field. Sort and the Sort* functions share them.
func planNetworks(networks []Network) (func(), error) {
	return planSort(networks, KindNetwork, func(n Network) string { return n.Name })
}

func planConfigs(configs []Config) (func(), error) {
	return planSort(configs, KindConfig, func(c Config) string { return c.Name })
}

func planSecrets(secrets []Secret) (func(), error) {
	return planSort(secrets, KindSecret, func(s Secret) string { return s.Name })
}

func planVolumes(volumes []Volume) (func(), error) {
	return planSort(volumes, KindVolume, func(v Volume) string { return v.Name })
}

// planSort encodes the elements whose names are shared, the only ones the
// tie-break compares, and returns the sort. The sort cannot fail, so callers
// can plan several collections and only then reorder any of them.
func planSort[T any](elems []T, kind Kind, name func(T) string) (func(), error) {
	shared := make(map[string]int, len(elems))
	for _, e := range elems {
		shared[name(e)]++
	}
	type entry struct {
		elem T
		name string
		enc  []byte
	}
	entries := make([]entry, len(elems))
	for i, e := range elems {
		n := name(e)
		entries[i] = entry{elem: e, name: n}
		if shared[n] > 1 {
			enc, err := json.Marshal(e)
			if err != nil {
				return nil, fmt.Errorf("desired: cannot sort %s %q: %w", kind, n, err)
			}
			entries[i].enc = enc
		}
	}
	return func() {
		slices.SortFunc(entries, func(a, b entry) int {
			if c := strings.Compare(a.name, b.name); c != 0 {
				return c
			}
			return bytes.Compare(a.enc, b.enc)
		})
		for i := range entries {
			elems[i] = entries[i].elem
		}
	}, nil
}

// planServices is planSort for services, followed by the nested sorts of each
// service.
func planServices(services []Service) (func(), error) {
	sortServices, err := planSort(services, KindService, func(s Service) string { return s.Name })
	if err != nil {
		return nil, err
	}
	return func() {
		sortServices()
		for i := range services {
			services[i].sort()
		}
	}, nil
}

func (s *Service) sort() {
	slices.SortFunc(s.Ports, comparePort)
	slices.SortFunc(s.Networks, compareServiceNetwork)
	slices.SortFunc(s.Mounts, compareMount)
	slices.SortFunc(s.Configs, func(a, b ConfigRef) int {
		return compareFileRef(fileRef(a), fileRef(b))
	})
	slices.SortFunc(s.Secrets, func(a, b SecretRef) int {
		return compareFileRef(fileRef(a), fileRef(b))
	})
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

// fileRef holds the fields that ConfigRef and SecretRef share, in the same
// order, so either converts to it.
type fileRef struct {
	Source string
	Target string
	UID    string
	GID    string
	Mode   uint32
}

func compareFileRef(a, b fileRef) int {
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

// ParseKey is the inverse of Key.String. It reports false for a string that is
// not a key of a known kind.
func ParseKey(s string) (Key, bool) {
	kind, name, ok := strings.Cut(s, "/")
	if !ok {
		return Key{}, false
	}
	switch Kind(kind) {
	case KindService, KindNetwork, KindConfig, KindSecret, KindVolume:
		return Key{Kind: Kind(kind), Name: name}, true
	}
	return Key{}, false
}
