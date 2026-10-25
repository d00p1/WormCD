package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/distribution/reference"
)

type normalizer struct {
	app       string
	target    string
	namespace string
	baseDir   string

	networkNames map[string]string
	configNames  map[string]string
	secretNames  map[string]string
	volumeNames  map[string]string

	out *desired.Desired
}

// swarmVXLANDriverOpt is assigned by Swarm when an overlay network is
// created. Letting it through would make the desired state depend on a runtime
// detail and produce permanent drift.
const swarmVXLANDriverOpt = "com.docker.network.driver.overlay.vxlanid_list"

func normalize(project *types.Project, app string, target Target, namespace, baseDir string) (*desired.Desired, error) {
	if err := checkProject(project); err != nil {
		return nil, err
	}

	n := &normalizer{
		app:          app,
		target:       target.Name,
		namespace:    namespace,
		baseDir:      baseDir,
		networkNames: map[string]string{},
		configNames:  map[string]string{},
		secretNames:  map[string]string{},
		volumeNames:  map[string]string{},
		out: &desired.Desired{
			Application: app,
			Target:      target.Name,
			Namespace:   namespace,
		},
	}

	if err := n.networks(project); err != nil {
		return nil, err
	}
	if err := n.configs(project); err != nil {
		return nil, err
	}
	if err := n.secrets(project); err != nil {
		return nil, err
	}
	if err := n.volumes(project); err != nil {
		return nil, err
	}
	if err := n.services(project); err != nil {
		return nil, err
	}

	n.out.Sort()
	return n.out, nil
}

func (n *normalizer) networks(project *types.Project) error {
	for _, key := range sortedKeys(project.Networks) {
		network, err := n.network(key, project.Networks[key])
		if err != nil {
			return err
		}
		n.networkNames[key] = network.Name
		n.out.Networks = append(n.out.Networks, network)
	}
	return nil
}

func (n *normalizer) network(key string, cfg types.NetworkConfig) (desired.Network, error) {
	if bool(cfg.External) {
		if cfg.Driver != "" || len(cfg.DriverOpts) > 0 || cfg.Internal || cfg.Attachable ||
			len(cfg.Labels) > 0 || !zeroIPAM(cfg.Ipam) || cfg.EnableIPv4 != nil || cfg.EnableIPv6 != nil {
			return desired.Network{}, fmt.Errorf("network %q: external networks only support name", key)
		}
		name := firstNonEmpty(cfg.Name, key)
		return desired.Network{Name: name, External: true}, nil
	}

	if _, ok := cfg.DriverOpts[swarmVXLANDriverOpt]; ok {
		return desired.Network{}, fmt.Errorf("network %q: driver option %q is assigned by Swarm", key, swarmVXLANDriverOpt)
	}

	if !zeroIPAM(cfg.Ipam) {
		return desired.Network{}, fmt.Errorf("network %q: ipam is not supported in v0", key)
	}
	if cfg.EnableIPv4 != nil || cfg.EnableIPv6 != nil {
		return desired.Network{}, fmt.Errorf("network %q: enable_ipv4/enable_ipv6 are not supported in v0", key)
	}

	driver := cfg.Driver
	if driver == "" {
		driver = "overlay"
	}
	name := firstNonEmpty(cfg.Name, n.namespace+"_"+key)
	return desired.Network{
		Name:       name,
		Driver:     driver,
		DriverOpts: map[string]string(cfg.DriverOpts),
		Attachable: cfg.Attachable,
		Internal:   cfg.Internal,
		Labels:     desired.MergeLabels(cfg.Labels, n.app, n.target),
	}, nil
}

func zeroIPAM(cfg types.IPAMConfig) bool {
	return cfg.Driver == "" && len(cfg.Config) == 0 && len(cfg.Options) == 0
}

func (n *normalizer) ensureDefaultNetwork() {
	if _, ok := n.networkNames["default"]; ok {
		return
	}
	n.networkNames["default"] = n.namespace + "_default"
	n.out.Networks = append(n.out.Networks, desired.Network{
		Name:   n.namespace + "_default",
		Driver: "overlay",
		Labels: desired.Labels(n.app, n.target),
	})
}

func (n *normalizer) configs(project *types.Project) error {
	for _, key := range sortedKeys(project.Configs) {
		config, err := n.config(key, project.Configs[key])
		if err != nil {
			return err
		}
		n.configNames[key] = config.Name
		n.out.Configs = append(n.out.Configs, config)
	}
	return nil
}

func (n *normalizer) config(key string, cfg types.ConfigObjConfig) (desired.Config, error) {
	if cfg.Driver != "" || len(cfg.DriverOpts) > 0 || cfg.TemplateDriver != "" {
		return desired.Config{}, fmt.Errorf("config %q: drivers are not supported", key)
	}

	if bool(cfg.External) {
		if cfg.File != "" || cfg.Environment != "" || cfg.Content != "" || len(cfg.Labels) > 0 {
			return desired.Config{}, fmt.Errorf("config %q: external configs only support name", key)
		}
		return desired.Config{Name: firstNonEmpty(cfg.Name, key), Source: key, External: true}, nil
	}

	if cfg.Environment != "" {
		return desired.Config{}, fmt.Errorf("config %q: environment source is not supported", key)
	}

	var data []byte
	var file string
	switch {
	case cfg.File != "" && cfg.Content != "":
		return desired.Config{}, fmt.Errorf("config %q: file and content are mutually exclusive", key)
	case cfg.File != "":
		path, err := n.resolveResource(cfg.File)
		if err != nil {
			return desired.Config{}, fmt.Errorf("config %q: %w", key, err)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return desired.Config{}, fmt.Errorf("config %q: %w", key, err)
		}
		if rel, err := filepath.Rel(n.baseDir, path); err == nil && !strings.HasPrefix(rel, "..") {
			file = rel
		} else {
			file = cfg.File
		}
	case cfg.Content != "":
		data = []byte(cfg.Content)
	default:
		return desired.Config{}, fmt.Errorf("config %q: one of file or content is required", key)
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])[:12]

	base := firstNonEmpty(cfg.Name, n.namespace+"_"+key)
	return desired.Config{
		Name:   base + "_" + hash,
		Source: key,
		File:   file,
		Hash:   hash,
		Labels: desired.MergeLabels(cfg.Labels, n.app, n.target),
		Data:   data,
	}, nil
}

func (n *normalizer) secrets(project *types.Project) error {
	for _, key := range sortedKeys(project.Secrets) {
		secret, err := n.secret(key, project.Secrets[key])
		if err != nil {
			return err
		}
		n.secretNames[key] = secret.Name
		n.out.Secrets = append(n.out.Secrets, secret)
	}
	return nil
}

func (n *normalizer) secret(key string, cfg types.SecretConfig) (desired.Secret, error) {
	if cfg.Driver != "" || len(cfg.DriverOpts) > 0 || cfg.TemplateDriver != "" {
		return desired.Secret{}, fmt.Errorf("secret %q: drivers are not supported", key)
	}
	if !bool(cfg.External) {
		return desired.Secret{}, fmt.Errorf("secret %q: v0 supports external Docker secrets only", key)
	}
	if cfg.File != "" || cfg.Environment != "" || cfg.Content != "" || len(cfg.Labels) > 0 {
		return desired.Secret{}, fmt.Errorf("secret %q: external secrets only support name", key)
	}
	return desired.Secret{Name: firstNonEmpty(cfg.Name, key), Source: key, External: true}, nil
}

func (n *normalizer) volumes(project *types.Project) error {
	for _, key := range sortedKeys(project.Volumes) {
		volume, err := n.volume(key, project.Volumes[key])
		if err != nil {
			return err
		}
		n.volumeNames[key] = volume.Name
		n.out.Volumes = append(n.out.Volumes, volume)
	}
	return nil
}

func (n *normalizer) volume(key string, cfg types.VolumeConfig) (desired.Volume, error) {
	if bool(cfg.External) {
		if cfg.Driver != "" || len(cfg.DriverOpts) > 0 || len(cfg.Labels) > 0 {
			return desired.Volume{}, fmt.Errorf("volume %q: external volumes only support name", key)
		}
		name := firstNonEmpty(cfg.Name, key)
		return desired.Volume{Name: name, External: true}, nil
	}
	driver := cfg.Driver
	if driver == "" {
		driver = "local"
	}
	name := firstNonEmpty(cfg.Name, n.namespace+"_"+key)
	return desired.Volume{
		Name:       name,
		Driver:     driver,
		DriverOpts: map[string]string(cfg.DriverOpts),
		Labels:     desired.MergeLabels(cfg.Labels, n.app, n.target),
	}, nil
}

func (n *normalizer) services(project *types.Project) error {
	for _, key := range sortedKeys(project.Services) {
		service, err := n.service(key, project.Services[key])
		if err != nil {
			return err
		}
		n.out.Services = append(n.out.Services, service)
	}
	return nil
}

func (n *normalizer) service(key string, s types.ServiceConfig) (desired.Service, error) {
	if fields := unsupportedServiceFields(s); len(fields) > 0 {
		return desired.Service{}, fmt.Errorf("services.%s: unsupported fields: %s", key, strings.Join(fields, ", "))
	}
	if s.Image == "" {
		return desired.Service{}, fmt.Errorf("service %q: image is required", key)
	}
	if err := validateImage(s.Image); err != nil {
		return desired.Service{}, fmt.Errorf("service %q: %w", key, err)
	}

	deploy := s.Deploy
	if deploy == nil {
		deploy = &types.DeployConfig{}
	}
	if deploy.EndpointMode != "" && deploy.EndpointMode != "vip" && deploy.EndpointMode != "dnsrr" {
		return desired.Service{}, fmt.Errorf("service %q: deploy.endpoint_mode %q is not supported", key, deploy.EndpointMode)
	}
	endpointMode := deploy.EndpointMode
	if endpointMode == "" {
		endpointMode = desired.DefaultEndpointMode
	}

	mode, replicas, err := serviceMode(key, s, deploy)
	if err != nil {
		return desired.Service{}, err
	}
	env, err := serviceEnv(key, s.Environment)
	if err != nil {
		return desired.Service{}, err
	}
	ports, err := servicePorts(key, s.Ports)
	if err != nil {
		return desired.Service{}, err
	}
	networks, err := n.serviceNetworks(key, s.Networks)
	if err != nil {
		return desired.Service{}, err
	}
	mounts, err := n.serviceMounts(key, s.Volumes)
	if err != nil {
		return desired.Service{}, err
	}
	configs, err := n.serviceConfigs(key, s.Configs)
	if err != nil {
		return desired.Service{}, err
	}
	secrets, err := n.serviceSecrets(key, s.Secrets)
	if err != nil {
		return desired.Service{}, err
	}
	healthcheck, err := serviceHealthcheck(key, s.HealthCheck)
	if err != nil {
		return desired.Service{}, err
	}
	resources, err := serviceResources(key, deploy.Resources)
	if err != nil {
		return desired.Service{}, err
	}
	restart, err := serviceRestart(key, deploy.RestartPolicy)
	if err != nil {
		return desired.Service{}, err
	}
	update, err := serviceUpdate(key, "update_config", deploy.UpdateConfig)
	if err != nil {
		return desired.Service{}, err
	}
	rollback, err := serviceUpdate(key, "rollback_config", deploy.RollbackConfig)
	if err != nil {
		return desired.Service{}, err
	}

	labels := make(map[string]string, len(s.Labels)+len(deploy.Labels)+2)
	for k, v := range s.Labels {
		labels[k] = v
	}
	for k, v := range deploy.Labels {
		labels[k] = v
	}

	service := desired.Service{
		Name:            n.namespace + "_" + key,
		Image:           s.Image,
		Mode:            mode,
		Replicas:        replicas,
		EndpointMode:    endpointMode,
		Command:         commandPtr(s.Command),
		Entrypoint:      commandPtr(s.Entrypoint),
		Env:             env,
		Labels:          desired.MergeLabels(labels, n.app, n.target),
		Hostname:        s.Hostname,
		User:            s.User,
		WorkingDir:      s.WorkingDir,
		StopSignal:      s.StopSignal,
		StopGracePeriod: durationOf(s.StopGracePeriod),
		ReadOnly:        s.ReadOnly,
		Ports:           ports,
		Networks:        networks,
		Mounts:          mounts,
		Configs:         configs,
		Secrets:         secrets,
		Resources:       resources,
		Placement:       servicePlacement(deploy.Placement),
		Restart:         restart,
		Healthcheck:     healthcheck,
		Update:          update,
		Rollback:        rollback,
	}
	return service, nil
}

func serviceMode(key string, s types.ServiceConfig, deploy *types.DeployConfig) (desired.ServiceMode, uint64, error) {
	mode := desired.ModeReplicated
	replicas := desired.DefaultReplicas

	switch deploy.Mode {
	case "", "replicated":
	case "global":
		mode = desired.ModeGlobal
		replicas = 0
	default:
		return "", 0, fmt.Errorf("service %q: deploy.mode %q is not supported", key, deploy.Mode)
	}

	if mode == desired.ModeGlobal {
		if deploy.Replicas != nil {
			return "", 0, fmt.Errorf("service %q: deploy.replicas cannot be used with global mode", key)
		}
		if s.Scale != nil && *s.Scale != 0 {
			return "", 0, fmt.Errorf("service %q: scale cannot be used with global mode", key)
		}
		return mode, 0, nil
	}

	if s.Scale != nil && deploy.Replicas != nil && *s.Scale != *deploy.Replicas {
		return "", 0, fmt.Errorf("service %q: scale and deploy.replicas disagree", key)
	}
	switch {
	case s.Scale != nil:
		if *s.Scale < 0 {
			return "", 0, fmt.Errorf("service %q: scale cannot be negative", key)
		}
		replicas = uint64(*s.Scale)
	case deploy.Replicas != nil:
		if *deploy.Replicas < 0 {
			return "", 0, fmt.Errorf("service %q: deploy.replicas cannot be negative", key)
		}
		replicas = uint64(*deploy.Replicas)
	}
	return mode, replicas, nil
}

func serviceEnv(key string, env types.MappingWithEquals) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == nil {
			return nil, fmt.Errorf("service %q: environment %q has no value; set it explicitly or in a .env file", key, k)
		}
		out[k] = *v
	}
	return out, nil
}

func servicePorts(key string, ports []types.ServicePortConfig) ([]desired.Port, error) {
	out := make([]desired.Port, 0, len(ports))
	for _, port := range ports {
		if port.HostIP != "" {
			return nil, fmt.Errorf("service %q: port host_ip is not supported", key)
		}
		if port.AppProtocol != "" {
			return nil, fmt.Errorf("service %q: port app_protocol is not supported", key)
		}
		if port.Target == 0 {
			return nil, fmt.Errorf("service %q: port target is required", key)
		}

		published := uint32(0)
		if port.Published != "" {
			if strings.Contains(port.Published, "-") {
				return nil, fmt.Errorf("service %q: published port ranges are not supported", key)
			}
			value, err := strconv.ParseUint(port.Published, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("service %q: published port %q: %w", key, port.Published, err)
			}
			published = uint32(value)
		}

		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		switch protocol {
		case "tcp", "udp", "sctp":
		default:
			return nil, fmt.Errorf("service %q: port protocol %q is not supported", key, protocol)
		}

		mode := port.Mode
		if mode == "" {
			mode = "ingress"
		}
		if mode != "ingress" && mode != "host" {
			return nil, fmt.Errorf("service %q: port mode %q is not supported", key, mode)
		}

		out = append(out, desired.Port{
			Name:      port.Name,
			Target:    port.Target,
			Published: published,
			Protocol:  protocol,
			Mode:      mode,
		})
	}
	return out, nil
}

func (n *normalizer) serviceNetworks(key string, networks map[string]*types.ServiceNetworkConfig) ([]desired.ServiceNetwork, error) {
	if len(networks) == 0 {
		n.ensureDefaultNetwork()
		return []desired.ServiceNetwork{{Name: n.networkNames["default"]}}, nil
	}

	out := make([]desired.ServiceNetwork, 0, len(networks))
	for _, name := range sortedKeys(networks) {
		networkName, ok := n.networkNames[name]
		if !ok {
			return nil, fmt.Errorf("service %q: network %q is not declared", key, name)
		}
		cfg := networks[name]
		if cfg != nil {
			if len(cfg.DriverOpts) > 0 || cfg.GatewayPriority != 0 || cfg.InterfaceName != "" ||
				cfg.Ipv4Address != "" || cfg.Ipv6Address != "" || len(cfg.LinkLocalIPs) > 0 ||
				cfg.MacAddress != "" || cfg.Priority != 0 || cfg.UseServiceNameAlias != nil {
				return nil, fmt.Errorf("service %q: network %q: static addresses and driver options are not supported", key, name)
			}
		}
		attachment := desired.ServiceNetwork{Name: networkName}
		if cfg != nil {
			attachment.Aliases = append([]string(nil), cfg.Aliases...)
		}
		out = append(out, attachment)
	}
	return out, nil
}

func (n *normalizer) serviceMounts(key string, volumes []types.ServiceVolumeConfig) ([]desired.Mount, error) {
	out := make([]desired.Mount, 0, len(volumes))
	for _, volume := range volumes {
		if volume.Consistency != "" {
			return nil, fmt.Errorf("service %q: volume consistency is not supported", key)
		}
		if volume.Target == "" {
			return nil, fmt.Errorf("service %q: volume target is required", key)
		}

		mount := desired.Mount{Target: volume.Target, ReadOnly: volume.ReadOnly}
		switch volume.Type {
		case "volume":
			mount.Type = desired.MountVolume
			mount.Source = volume.Source
			if volume.Source != "" {
				name, ok := n.volumeNames[volume.Source]
				if !ok {
					return nil, fmt.Errorf("service %q: volume %q is not declared", key, volume.Source)
				}
				mount.Source = name
			}
			if volume.Volume != nil && (volume.Volume.NoCopy || volume.Volume.Subpath != "" || len(volume.Volume.Labels) > 0) {
				return nil, fmt.Errorf("service %q: volume options nocopy, subpath and labels are not supported", key)
			}
		case "bind":
			mount.Type = desired.MountBind
			if volume.Source == "" {
				return nil, fmt.Errorf("service %q: bind mount source is required", key)
			}
			mount.Source = volume.Source
			if !filepath.IsAbs(mount.Source) {
				mount.Source = filepath.Join(n.baseDir, mount.Source)
			}
			if volume.Bind != nil && (volume.Bind.SELinux != "" || volume.Bind.Propagation != "" || volume.Bind.Recursive != "") {
				return nil, fmt.Errorf("service %q: bind options selinux, propagation and recursive are not supported", key)
			}
		case "tmpfs":
			return nil, fmt.Errorf("service %q: tmpfs mounts are not supported", key)
		case "image":
			return nil, fmt.Errorf("service %q: image mounts are not supported", key)
		default:
			return nil, fmt.Errorf("service %q: mount type %q is not supported", key, volume.Type)
		}
		out = append(out, mount)
	}
	return out, nil
}

func (n *normalizer) serviceConfigs(key string, refs []types.ServiceConfigObjConfig) ([]desired.ConfigRef, error) {
	out := make([]desired.ConfigRef, 0, len(refs))
	for _, ref := range refs {
		name, ok := n.configNames[ref.Source]
		if !ok {
			return nil, fmt.Errorf("service %q: config %q is not declared", key, ref.Source)
		}
		target := ref.Target
		if target == "" {
			target = "/" + ref.Source
		}
		mode := uint32(0o444)
		if ref.Mode != nil {
			mode = uint32(*ref.Mode)
		}
		out = append(out, desired.ConfigRef{
			Source: name,
			Target: target,
			UID:    ref.UID,
			GID:    ref.GID,
			Mode:   mode,
		})
	}
	return out, nil
}

func (n *normalizer) serviceSecrets(key string, refs []types.ServiceSecretConfig) ([]desired.SecretRef, error) {
	out := make([]desired.SecretRef, 0, len(refs))
	for _, ref := range refs {
		name, ok := n.secretNames[ref.Source]
		if !ok {
			return nil, fmt.Errorf("service %q: secret %q is not declared", key, ref.Source)
		}
		target := ref.Target
		if target == "" {
			target = "/run/secrets/" + ref.Source
		}
		mode := uint32(0o444)
		if ref.Mode != nil {
			mode = uint32(*ref.Mode)
		}
		out = append(out, desired.SecretRef{
			Source: name,
			Target: target,
			UID:    ref.UID,
			GID:    ref.GID,
			Mode:   mode,
		})
	}
	return out, nil
}

func serviceHealthcheck(key string, cfg *types.HealthCheckConfig) (*desired.Healthcheck, error) {
	if cfg == nil || cfg.Disable {
		return nil, nil
	}
	if len(cfg.Test) == 0 {
		return nil, fmt.Errorf("service %q: healthcheck.test is required", key)
	}
	if cfg.StartInterval != nil {
		return nil, fmt.Errorf("service %q: healthcheck.start_interval is not supported", key)
	}

	defaults := desired.DefaultHealthcheck()
	defaults.Test = append([]string(nil), cfg.Test...)
	out := &defaults
	if cfg.Interval != nil {
		out.Interval = time.Duration(*cfg.Interval)
	}
	if cfg.Timeout != nil {
		out.Timeout = time.Duration(*cfg.Timeout)
	}
	if cfg.Retries != nil {
		out.Retries = int(*cfg.Retries)
	}
	if cfg.StartPeriod != nil {
		out.StartPeriod = time.Duration(*cfg.StartPeriod)
	}
	return out, nil
}

func serviceResources(key string, resources types.Resources) (desired.Resources, error) {
	limits, err := resourceLimits(key, "limits", resources.Limits)
	if err != nil {
		return desired.Resources{}, err
	}
	reservations, err := resourceLimits(key, "reservations", resources.Reservations)
	if err != nil {
		return desired.Resources{}, err
	}
	return desired.Resources{Limits: limits, Reservations: reservations}, nil
}

func resourceLimits(key, kind string, resource *types.Resource) (desired.ResourceLimits, error) {
	if resource == nil {
		return desired.ResourceLimits{}, nil
	}
	if len(resource.Devices) > 0 || len(resource.GenericResources) > 0 {
		return desired.ResourceLimits{}, fmt.Errorf("service %q: deploy.resources.%s devices and generic_resources are not supported", key, kind)
	}
	return desired.ResourceLimits{
		NanoCPUs:    int64(math.Round(float64(resource.NanoCPUs) * 1e9)),
		MemoryBytes: int64(resource.MemoryBytes),
		Pids:        resource.Pids,
	}, nil
}

func servicePlacement(placement types.Placement) desired.Placement {
	out := desired.Placement{
		Constraints: append([]string(nil), placement.Constraints...),
		MaxReplicas: placement.MaxReplicas,
	}
	for _, preference := range placement.Preferences {
		out.Preferences = append(out.Preferences, desired.PlacementPreference{Spread: preference.Spread})
	}
	return out
}

func serviceRestart(key string, policy *types.RestartPolicy) (desired.RestartPolicy, error) {
	out := desired.DefaultRestartPolicy()
	if policy != nil {
		if policy.Condition != "" {
			out.Condition = policy.Condition
		}
		if policy.Delay != nil {
			out.Delay = time.Duration(*policy.Delay)
		}
		if policy.MaxAttempts != nil {
			out.MaxAttempts = *policy.MaxAttempts
		}
		if policy.Window != nil {
			out.Window = time.Duration(*policy.Window)
		}
	}
	switch out.Condition {
	case "none", "on-failure", "any":
	default:
		return desired.RestartPolicy{}, fmt.Errorf("service %q: restart condition %q is not supported", key, out.Condition)
	}
	return out, nil
}

func serviceUpdate(key, field string, cfg *types.UpdateConfig) (desired.UpdateConfig, error) {
	out := desired.DefaultUpdateConfig()
	if cfg != nil {
		if cfg.Parallelism != nil {
			out.Parallelism = *cfg.Parallelism
		}
		out.Delay = time.Duration(cfg.Delay)
		if cfg.FailureAction != "" {
			out.FailureAction = cfg.FailureAction
		}
		if cfg.Monitor != 0 {
			out.Monitor = time.Duration(cfg.Monitor)
		}
		out.MaxFailureRatio = cfg.MaxFailureRatio
		if cfg.Order != "" {
			out.Order = cfg.Order
		}
	}

	switch out.FailureAction {
	case "continue", "pause", "rollback":
	default:
		return desired.UpdateConfig{}, fmt.Errorf("service %q: deploy.%s.failure_action %q is not supported", key, field, out.FailureAction)
	}
	if out.Order != "stop-first" && out.Order != "start-first" {
		return desired.UpdateConfig{}, fmt.Errorf("service %q: deploy.%s.order %q is not supported", key, field, out.Order)
	}
	return out, nil
}

func validateImage(image string) error {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("image %q: %w", image, err)
	}
	if reference.IsNameOnly(named) {
		return fmt.Errorf("image %q must be pinned to a tag or digest", image)
	}
	return nil
}

func commandPtr(command types.ShellCommand) *[]string {
	if command == nil {
		return nil
	}
	out := make([]string, len(command))
	copy(out, command)
	return &out
}

func durationOf(duration *types.Duration) time.Duration {
	if duration == nil {
		return 0
	}
	return time.Duration(*duration)
}

func (n *normalizer) resolveResource(path string) (string, error) {
	if filepath.IsAbs(path) {
		if err := ensureInside(n.baseDir, path); err != nil {
			return "", err
		}
		return path, nil
	}
	return resolveLocal(n.baseDir, path)
}

func ensureInside(baseDir, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseDir, abs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q is outside the application directory", path)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
