package swarm

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
)

func testServiceScope() runtime.Scope {
	return runtime.Scope{Application: "web", Target: "prod"}
}

func testReferenceIDs() referenceIDs {
	return referenceIDs{
		Configs: map[string]string{testConfig: "id-" + testConfig},
		Secrets: map[string]string{"prod_tls": "id-prod_tls"},
	}
}

// fullService sets every field WormCD manages, so the round trip below covers
// each conversion.
func fullService() desired.Service {
	return desired.Service{
		Name:            "web",
		Image:           "ghcr.io/example/web@sha256:abc",
		Mode:            desired.ModeReplicated,
		Replicas:        3,
		EndpointMode:    "dnsrr",
		Command:         &[]string{"serve", "--port", "8080"},
		Entrypoint:      &[]string{"/entry.sh"},
		Env:             map[string]string{"APP_ENV": "prod", "EMPTY": ""},
		Labels:          desired.MergeLabels(map[string]string{"team": "shop"}, "web", "prod"),
		Hostname:        "web-1",
		User:            "10001",
		WorkingDir:      "/srv",
		StopSignal:      "SIGTERM",
		StopGracePeriod: 20 * time.Second,
		ReadOnly:        true,
		Ports: []desired.Port{
			{Name: "http", Target: 8080, Published: 80, Protocol: "tcp", Mode: "ingress"},
			{Target: 9000, Published: 9000, Protocol: "udp", Mode: "host"},
		},
		Networks: []desired.ServiceNetwork{
			{Name: testNetwork, Aliases: []string{"web", "frontend"}},
			{Name: "wormcd_web_prod_back"},
		},
		Mounts: []desired.Mount{
			{Type: desired.MountVolume, Source: testVolume, Target: "/data"},
			{Type: desired.MountBind, Source: "/etc/hosts", Target: "/etc/hosts", ReadOnly: true},
		},
		Configs: []desired.ConfigRef{{Source: testConfig, Target: "/etc/nginx/nginx.conf", UID: "0", GID: "0", Mode: 0o440}},
		Secrets: []desired.SecretRef{{Source: "prod_tls", Target: "/run/secrets/tls", Mode: 0o400}},
		Resources: desired.Resources{
			Limits:       desired.ResourceLimits{NanoCPUs: 500_000_000, MemoryBytes: 256 << 20, Pids: 100},
			Reservations: desired.ResourceLimits{NanoCPUs: 250_000_000, MemoryBytes: 64 << 20},
		},
		Placement: desired.Placement{
			Constraints: []string{"node.role == worker"},
			Preferences: []desired.PlacementPreference{{Spread: "node.labels.zone"}},
			MaxReplicas: 2,
		},
		Restart: desired.RestartPolicy{
			Condition:   "on-failure",
			Delay:       10 * time.Second,
			MaxAttempts: 3,
			Window:      time.Minute,
		},
		Healthcheck: &desired.Healthcheck{
			Test:        []string{"CMD", "curl", "-f", "http://localhost/"},
			Interval:    10 * time.Second,
			Timeout:     2 * time.Second,
			StartPeriod: 5 * time.Second,
			Retries:     5,
		},
		Update: desired.UpdateConfig{
			Parallelism:     2,
			Delay:           time.Second,
			FailureAction:   "rollback",
			Monitor:         10 * time.Second,
			MaxFailureRatio: 0.25,
			Order:           "start-first",
		},
		Rollback: desired.UpdateConfig{
			Parallelism:   1,
			FailureAction: "pause",
			Monitor:       5 * time.Second,
			Order:         "stop-first",
		},
	}
}

// TestServiceSpecRoundTrip converts a fully populated desired service and maps
// the spec back with mapService. The result must equal the input, which is the
// guarantee that a converged service does not show up as drift.
func TestServiceSpecRoundTrip(t *testing.T) {
	service := fullService()
	spec, err := serviceSpec(service, testServiceScope(), testReferenceIDs())
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}

	got, err := mapService(swarm.Service{Spec: spec}, nil)
	if err != nil {
		t.Fatalf("mapService: %v", err)
	}
	if !reflect.DeepEqual(got, service) {
		t.Fatalf("round trip differs:\n got  %+v\n want %+v", got, service)
	}
}

func TestServiceSpecGlobalMode(t *testing.T) {
	service := desired.Service{
		Name:         "agent",
		Image:        "agent:1",
		Mode:         desired.ModeGlobal,
		EndpointMode: desired.DefaultEndpointMode,
	}
	spec, err := serviceSpec(service, testServiceScope(), referenceIDs{})
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	if spec.Mode.Global == nil || spec.Mode.Replicated != nil {
		t.Fatalf("mode = %+v, want global only", spec.Mode)
	}

	got, err := mapService(swarm.Service{Spec: spec}, nil)
	if err != nil {
		t.Fatalf("mapService: %v", err)
	}
	if got.Mode != desired.ModeGlobal || got.Replicas != 0 {
		t.Fatalf("mapped mode = %s replicas = %d, want global with no replicas", got.Mode, got.Replicas)
	}
}

func TestServiceSpecOwnershipLabelsWinOverServiceLabels(t *testing.T) {
	service := desired.Service{
		Name:   "web",
		Image:  "web:1",
		Mode:   desired.ModeReplicated,
		Labels: map[string]string{"team": "shop", desired.LabelTarget: "staging"},
	}
	spec, err := serviceSpec(service, testServiceScope(), referenceIDs{})
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	want := map[string]string{"team": "shop", desired.LabelApplication: "web", desired.LabelTarget: "prod"}
	if !maps.Equal(spec.Labels, want) {
		t.Fatalf("labels = %v, want %v", spec.Labels, want)
	}
}

func TestServiceSpecReferencesByID(t *testing.T) {
	spec, err := serviceSpec(fullService(), testServiceScope(), testReferenceIDs())
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	containerSpec := spec.TaskTemplate.ContainerSpec
	if len(containerSpec.Configs) != 1 || containerSpec.Configs[0].ConfigID != "id-"+testConfig || containerSpec.Configs[0].ConfigName != testConfig {
		t.Fatalf("config refs = %+v", containerSpec.Configs)
	}
	if len(containerSpec.Secrets) != 1 || containerSpec.Secrets[0].SecretID != "id-prod_tls" {
		t.Fatalf("secret refs = %+v", containerSpec.Secrets)
	}
}

func TestServiceSpecRejectsReferenceWithoutID(t *testing.T) {
	_, err := serviceSpec(fullService(), testServiceScope(), referenceIDs{})
	if err == nil || !strings.Contains(err.Error(), "config "+testConfig+" has no Docker ID") {
		t.Fatalf("error = %v, want a missing config ID error", err)
	}
}

func TestServiceSpecRefusesTmpfsMount(t *testing.T) {
	service := desired.Service{
		Name:   "web",
		Image:  "web:1",
		Mode:   desired.ModeReplicated,
		Mounts: []desired.Mount{{Type: desired.MountTmpfs, Target: "/scratch"}},
	}
	_, err := serviceSpec(service, testServiceScope(), referenceIDs{})
	if err == nil || !strings.Contains(err.Error(), "tmpfs") {
		t.Fatalf("error = %v, want a tmpfs refusal", err)
	}
}

func TestServiceSpecRefusesUnknownMode(t *testing.T) {
	_, err := serviceSpec(desired.Service{Name: "web", Mode: "replicatedJob"}, testServiceScope(), referenceIDs{})
	if err == nil || !strings.Contains(err.Error(), `mode "replicatedJob"`) {
		t.Fatalf("error = %v, want a mode refusal", err)
	}
}

func TestServiceSpecEnvIsSorted(t *testing.T) {
	service := desired.Service{
		Name:  "web",
		Image: "web:1",
		Mode:  desired.ModeReplicated,
		Env:   map[string]string{"ZETA": "1", "ALPHA": "2", "MID": "3"},
	}
	spec, err := serviceSpec(service, testServiceScope(), referenceIDs{})
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	want := []string{"ALPHA=2", "MID=3", "ZETA=1"}
	if got := spec.TaskTemplate.ContainerSpec.Env; !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

func TestServiceSpecLeavesUnsetOptionalsNil(t *testing.T) {
	service := desired.Service{Name: "web", Image: "web:1", Mode: desired.ModeReplicated}
	spec, err := serviceSpec(service, testServiceScope(), referenceIDs{})
	if err != nil {
		t.Fatalf("serviceSpec: %v", err)
	}
	containerSpec := spec.TaskTemplate.ContainerSpec
	if containerSpec.Healthcheck != nil || containerSpec.StopGracePeriod != nil || containerSpec.Mounts != nil ||
		containerSpec.Env != nil || containerSpec.Command != nil || containerSpec.Args != nil {
		t.Fatalf("container = %+v, want unset optionals to be nil", containerSpec)
	}
	if spec.TaskTemplate.Resources != nil || spec.TaskTemplate.Placement != nil {
		t.Fatalf("resources = %+v, placement = %+v, want nil", spec.TaskTemplate.Resources, spec.TaskTemplate.Placement)
	}
	if spec.EndpointSpec == nil || spec.EndpointSpec.Ports != nil {
		t.Fatalf("endpoint = %+v, want an endpoint without ports", spec.EndpointSpec)
	}
	if spec.TaskTemplate.RestartPolicy == nil || spec.TaskTemplate.RestartPolicy.MaxAttempts == nil {
		t.Fatalf("restart policy = %+v, want a fully set policy", spec.TaskTemplate.RestartPolicy)
	}
}

func TestServiceSpecMountsKeepTypes(t *testing.T) {
	mounts, err := mountSpecs([]desired.Mount{
		{Type: desired.MountVolume, Source: testVolume, Target: "/data"},
		{Type: desired.MountBind, Source: "/host", Target: "/container", ReadOnly: true},
	})
	if err != nil {
		t.Fatalf("mountSpecs: %v", err)
	}
	want := []mount.Mount{
		{Type: mount.TypeVolume, Source: testVolume, Target: "/data"},
		{Type: mount.TypeBind, Source: "/host", Target: "/container", ReadOnly: true},
	}
	if !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts = %+v, want %+v", mounts, want)
	}
}
