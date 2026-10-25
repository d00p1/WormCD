package swarm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

func TestInspect(t *testing.T) {
	labels := map[string]string{
		desired.LabelApplication: "web",
		desired.LabelTarget:      "prod",
		"team":                   "platform",
	}

	fake := &fakeAPI{
		services: client.ServiceListResult{Items: []swarm.Service{
			{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name:   "wormcd_web_prod_web",
						Labels: labels,
					},
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{
							Image:           "ghcr.io/d00p1/web:v1",
							Command:         []string{"/entrypoint.sh"},
							Args:            []string{"/app/server", "--verbose"},
							Env:             []string{"APP_ENV=prod", "LOG_LEVEL=info"},
							Hostname:        "web",
							User:            "1000",
							Dir:             "/app",
							StopSignal:      "SIGTERM",
							StopGracePeriod: durationPtr(10 * time.Second),
							ReadOnly:        true,
							Mounts: []mount.Mount{
								{Type: mount.TypeVolume, Source: "wormcd_web_prod_data", Target: "/var/lib/web"},
								{Type: mount.TypeBind, Source: "/etc/hosts", Target: "/etc/hosts", ReadOnly: true},
							},
							Configs: []*swarm.ConfigReference{{
								ConfigName: "wormcd_web_prod_nginx_abc",
								File: &swarm.ConfigReferenceFileTarget{
									Name: "/etc/nginx/nginx.conf",
									UID:  "1000",
									GID:  "1000",
									Mode: 0o444,
								},
							}},
							Secrets: []*swarm.SecretReference{{
								SecretName: "prod_tls_key",
								File: &swarm.SecretReferenceFileTarget{
									Name: "/run/secrets/tls.key",
									Mode: 0o444,
								},
							}},
							Healthcheck: &container.HealthConfig{
								Test:     []string{"CMD", "curl", "-f", "http://localhost/"},
								Interval: 10 * time.Second,
								Timeout:  2 * time.Second,
								Retries:  3,
							},
						},
						Resources: &swarm.ResourceRequirements{
							Limits: &swarm.Limit{
								NanoCPUs:    500_000_000,
								MemoryBytes: 128 << 20,
								Pids:        100,
							},
							Reservations: &swarm.Resources{
								NanoCPUs:    250_000_000,
								MemoryBytes: 64 << 20,
							},
						},
						Placement: &swarm.Placement{
							Constraints: []string{"node.role == worker"},
							MaxReplicas: 2,
							Preferences: []swarm.PlacementPreference{{
								Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.zone"},
							}},
						},
						RestartPolicy: &swarm.RestartPolicy{
							Condition:   swarm.RestartPolicyConditionOnFailure,
							Delay:       durationPtr(10 * time.Second),
							MaxAttempts: uint64Ptr(3),
						},
						Networks: []swarm.NetworkAttachmentConfig{
							{Target: "net1", Aliases: []string{"web"}},
							{Target: "net3"},
						},
					},
					Mode: swarm.ServiceMode{
						Replicated: &swarm.ReplicatedService{Replicas: uint64Ptr(3)},
					},
					EndpointSpec: &swarm.EndpointSpec{
						Mode: swarm.ResolutionModeDNSRR,
						Ports: []swarm.PortConfig{{
							TargetPort:    80,
							PublishedPort: 8080,
							Protocol:      network.TCP,
							PublishMode:   swarm.PortConfigPublishModeIngress,
						}},
					},
					UpdateConfig: &swarm.UpdateConfig{
						Parallelism:     2,
						Delay:           time.Second,
						FailureAction:   swarm.UpdateFailureActionContinue,
						Monitor:         3 * time.Second,
						MaxFailureRatio: 0.25,
						Order:           swarm.UpdateOrderStartFirst,
					},
					RollbackConfig: &swarm.UpdateConfig{
						Parallelism:   1,
						FailureAction: swarm.UpdateFailureActionPause,
						Monitor:       5 * time.Second,
						Order:         swarm.UpdateOrderStopFirst,
					},
				},
			},
			{
				Spec: swarm.ServiceSpec{
					Annotations: swarm.Annotations{
						Name: "other_target",
						Labels: map[string]string{
							desired.LabelApplication: "web",
							desired.LabelTarget:      "staging",
						},
					},
					TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "x:v1"}},
				},
			},
		}},
		networks: client.NetworkListResult{Items: []network.Summary{
			{Network: network.Network{
				ID:         "net1",
				Name:       "wormcd_web_prod_default",
				Driver:     "overlay",
				Attachable: true,
				Options:    map[string]string{"com.docker.network.driver.mtu": "1450"},
				Labels:     labels,
			}},
			{Network: network.Network{
				ID:     "net2",
				Name:   "unrelated",
				Labels: map[string]string{desired.LabelApplication: "web", desired.LabelTarget: "staging"},
			}},
			{Network: network.Network{ID: "net3", Name: "shared_net"}},
		}},
		configs: client.ConfigListResult{Items: []swarm.Config{
			{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "wormcd_web_prod_nginx_abc", Labels: labels}}},
			{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "unrelated"}}},
		}},
		secrets: client.SecretListResult{Items: []swarm.Secret{
			{Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "managed_secret", Labels: labels}}},
			{Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "prod_tls_key"}}},
		}},
		volumes: client.VolumeListResult{Items: []volume.Volume{
			{Name: "wormcd_web_prod_data", Driver: "local", Options: map[string]string{"type": "tmpfs"}, Labels: labels},
			{Name: "unrelated", Driver: "local"},
		}},
	}

	observed, err := newRuntime(fake).Inspect(context.Background(), runtime.Scope{Application: "web", Target: "prod"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if observed.Application != "web" || observed.Target != "prod" || observed.Namespace != "wormcd_web_prod" {
		t.Fatalf("scope = %s/%s/%s", observed.Application, observed.Target, observed.Namespace)
	}
	if len(observed.Services) != 1 {
		t.Fatalf("services = %d, want 1 (the other target must be skipped)", len(observed.Services))
	}

	service := observed.Services[0]
	if service.Name != "wormcd_web_prod_web" || service.Image != "ghcr.io/d00p1/web:v1" {
		t.Errorf("service = %s %s", service.Name, service.Image)
	}
	if service.Mode != desired.ModeReplicated || service.Replicas != 3 {
		t.Errorf("mode/replicas = %s/%d", service.Mode, service.Replicas)
	}
	if service.EndpointMode != "dnsrr" {
		t.Errorf("endpoint mode = %q, want dnsrr", service.EndpointMode)
	}
	if service.Command == nil || !slices.Equal(*service.Command, []string{"/app/server", "--verbose"}) {
		t.Errorf("command = %v", service.Command)
	}
	if service.Entrypoint == nil || !slices.Equal(*service.Entrypoint, []string{"/entrypoint.sh"}) {
		t.Errorf("entrypoint = %v", service.Entrypoint)
	}
	if service.Env["APP_ENV"] != "prod" || service.Env["LOG_LEVEL"] != "info" {
		t.Errorf("env = %v", service.Env)
	}
	if service.Hostname != "web" || service.User != "1000" || service.WorkingDir != "/app" {
		t.Errorf("hostname/user/workdir = %q/%q/%q", service.Hostname, service.User, service.WorkingDir)
	}
	if service.StopSignal != "SIGTERM" || service.StopGracePeriod != 10*time.Second || !service.ReadOnly {
		t.Errorf("stop/readonly = %q/%s/%v", service.StopSignal, service.StopGracePeriod, service.ReadOnly)
	}
	if len(service.Ports) != 1 || service.Ports[0].Target != 80 || service.Ports[0].Published != 8080 {
		t.Errorf("ports = %+v", service.Ports)
	}
	if len(service.Networks) != 2 {
		t.Fatalf("networks = %+v, want two", service.Networks)
	}
	networkNames := []string{service.Networks[0].Name, service.Networks[1].Name}
	slices.Sort(networkNames)
	if !slices.Equal(networkNames, []string{"shared_net", "wormcd_web_prod_default"}) {
		t.Errorf("network names = %v", networkNames)
	}
	if len(service.Mounts) != 2 {
		t.Fatalf("mounts = %+v, want two", service.Mounts)
	}
	mountsByTarget := make(map[string]desired.Mount, len(service.Mounts))
	for _, item := range service.Mounts {
		mountsByTarget[item.Target] = item
	}
	if got := mountsByTarget["/var/lib/web"]; got.Type != desired.MountVolume || got.Source != "wormcd_web_prod_data" {
		t.Errorf("volume mount = %+v", got)
	}
	if got := mountsByTarget["/etc/hosts"]; got.Type != desired.MountBind || !got.ReadOnly {
		t.Errorf("bind mount = %+v", got)
	}
	if len(service.Configs) != 1 || service.Configs[0].Source != "wormcd_web_prod_nginx_abc" ||
		service.Configs[0].Target != "/etc/nginx/nginx.conf" || service.Configs[0].Mode != 0o444 {
		t.Errorf("configs = %+v", service.Configs)
	}
	if len(service.Secrets) != 1 || service.Secrets[0].Source != "prod_tls_key" ||
		service.Secrets[0].Target != "/run/secrets/tls.key" || service.Secrets[0].Mode != 0o444 {
		t.Errorf("secrets = %+v", service.Secrets)
	}
	if service.Resources.Limits.NanoCPUs != 500_000_000 ||
		service.Resources.Limits.MemoryBytes != 128<<20 ||
		service.Resources.Limits.Pids != 100 {
		t.Errorf("limits = %+v", service.Resources.Limits)
	}
	if service.Resources.Reservations.NanoCPUs != 250_000_000 ||
		service.Resources.Reservations.MemoryBytes != 64<<20 {
		t.Errorf("reservations = %+v", service.Resources.Reservations)
	}
	if len(service.Placement.Constraints) != 1 || service.Placement.MaxReplicas != 2 ||
		len(service.Placement.Preferences) != 1 || service.Placement.Preferences[0].Spread != "node.labels.zone" {
		t.Errorf("placement = %+v", service.Placement)
	}
	if service.Restart.Condition != "on-failure" || service.Restart.Delay != 10*time.Second || service.Restart.MaxAttempts != 3 {
		t.Errorf("restart = %+v", service.Restart)
	}
	if service.Healthcheck == nil || len(service.Healthcheck.Test) != 4 ||
		service.Healthcheck.Interval != 10*time.Second ||
		service.Healthcheck.Timeout != 2*time.Second ||
		service.Healthcheck.Retries != 3 {
		t.Errorf("healthcheck = %+v", service.Healthcheck)
	}
	if service.Update.Parallelism != 2 || service.Update.FailureAction != "continue" ||
		service.Update.Monitor != 3*time.Second || service.Update.MaxFailureRatio != 0.25 ||
		service.Update.Order != "start-first" {
		t.Errorf("update = %+v", service.Update)
	}
	if service.Rollback.Parallelism != 1 || service.Rollback.FailureAction != "pause" ||
		service.Rollback.Monitor != 5*time.Second || service.Rollback.Order != "stop-first" {
		t.Errorf("rollback defaults = %+v", service.Rollback)
	}

	if len(observed.Networks) != 1 || observed.Networks[0].Name != "wormcd_web_prod_default" {
		t.Fatalf("observed networks = %+v, want only the labelled network", observed.Networks)
	}
	if observed.Networks[0].DriverOpts["com.docker.network.driver.mtu"] != "1450" || !observed.Networks[0].Attachable {
		t.Errorf("network = %+v", observed.Networks[0])
	}
	if len(observed.Configs) != 1 || observed.Configs[0].Name != "wormcd_web_prod_nginx_abc" {
		t.Errorf("observed configs = %+v", observed.Configs)
	}
	if len(observed.Secrets) != 1 || observed.Secrets[0].Name != "managed_secret" {
		t.Errorf("observed secrets = %+v (external secrets must be excluded)", observed.Secrets)
	}
	if len(observed.Volumes) != 1 || observed.Volumes[0].Name != "wormcd_web_prod_data" {
		t.Errorf("observed volumes = %+v", observed.Volumes)
	}
}

func TestInspectRejectsUnsupportedServices(t *testing.T) {
	labels := map[string]string{
		desired.LabelApplication: "web",
		desired.LabelTarget:      "prod",
	}

	tests := []struct {
		name string
		spec swarm.ServiceSpec
		want string
	}{
		{
			name: "job",
			spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: "job", Labels: labels},
				Mode:        swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{}},
			},
			want: "job modes are not supported",
		},
		{
			name: "no container",
			spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: "no_container", Labels: labels},
			},
			want: "container spec is missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeAPI{services: client.ServiceListResult{Items: []swarm.Service{{Spec: tt.spec}}}}
			_, err := newRuntime(fake).Inspect(context.Background(), runtime.Scope{Application: "web", Target: "prod"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestInspectRejectsEmptyScope(t *testing.T) {
	_, err := newRuntime(&fakeAPI{}).Inspect(context.Background(), runtime.Scope{})
	if err == nil || !strings.Contains(err.Error(), "application and target are required") {
		t.Fatalf("error = %v, want a scope validation error", err)
	}
}

func TestInspectListError(t *testing.T) {
	r := newRuntime(&fakeAPI{err: errors.New("boom")})
	_, err := r.Inspect(context.Background(), runtime.Scope{Application: "web", Target: "prod"})
	if err == nil || !strings.Contains(err.Error(), "list services: boom") {
		t.Fatalf("error = %v, want a list-services error", err)
	}
}

func TestKind(t *testing.T) {
	if got := newRuntime(&fakeAPI{}).Kind(); got != runtime.KindSwarm {
		t.Fatalf("kind = %q, want %q", got, runtime.KindSwarm)
	}
}

func TestMapUpdateDefaults(t *testing.T) {
	got := mapUpdate(nil)
	want := desired.UpdateConfig{
		Parallelism:   1,
		FailureAction: "pause",
		Monitor:       5 * time.Second,
		Order:         "stop-first",
	}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
}

type fakeAPI struct {
	services client.ServiceListResult
	networks client.NetworkListResult
	configs  client.ConfigListResult
	secrets  client.SecretListResult
	volumes  client.VolumeListResult
	err      error
}

func (f *fakeAPI) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return f.services, f.err
}

func (f *fakeAPI) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return f.networks, f.err
}

func (f *fakeAPI) ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error) {
	return f.configs, f.err
}

func (f *fakeAPI) SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error) {
	return f.secrets, f.err
}

func (f *fakeAPI) VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
	return f.volumes, f.err
}

func uint64Ptr(value uint64) *uint64 { return &value }

func durationPtr(value time.Duration) *time.Duration { return &value }
