package swarm

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

const (
	testNetwork   = "wormcd_web_prod_default"
	testVolume    = "wormcd_web_prod_data"
	testConfig    = "wormcd_web_prod_nginx_abc123"
	testOldConfig = "wormcd_web_prod_nginx_old999"
)

func testDesired() desired.Desired {
	labels := desired.Labels("web", "prod")
	return desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Networks: []desired.Network{{
			Name:       testNetwork,
			Driver:     "overlay",
			Attachable: true,
		}},
		Configs: []desired.Config{{
			Name:   testConfig,
			Source: "nginx",
			Hash:   "abc123",
			Data:   []byte("server {}"),
			Labels: labels,
		}},
		Secrets: []desired.Secret{{Name: "prod_tls", Source: "tls", External: true}},
		Volumes: []desired.Volume{{
			Name:       testVolume,
			Driver:     "local",
			DriverOpts: map[string]string{"type": "none"},
			Labels:     labels,
		}},
	}
}

func testPlan(desiredState desired.Desired, entries ...runtime.Entry) runtime.Plan {
	plan := runtime.Plan{
		Application: desiredState.Application,
		Target:      desiredState.Target,
		Namespace:   desiredState.Namespace,
		Desired:     desiredState,
	}
	for _, entry := range entries {
		plan.Add(entry)
	}
	return plan
}

func create(kind desired.Kind, name string) runtime.Entry {
	return runtime.Entry{Key: desired.Key{Kind: kind, Name: name}.String(), Action: runtime.Create}
}

func oldConfigObject() swarm.Config {
	return swarm.Config{
		ID: "id-" + testOldConfig,
		Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{
			Name:   testOldConfig,
			Labels: desired.Labels("web", "prod"),
		}},
	}
}

func TestApplyCreatesManagedObjects(t *testing.T) {
	fake := &fakeAPI{configs: client.ConfigListResult{Items: []swarm.Config{oldConfigObject()}}}
	plan := testPlan(testDesired(),
		create(desired.KindNetwork, testNetwork),
		create(desired.KindConfig, testConfig),
		create(desired.KindVolume, testVolume),
		runtime.Entry{Key: desired.Key{Kind: desired.KindConfig, Name: testOldConfig}.String(), Action: runtime.Delete},
	)

	result, err := newRuntime(fake).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantCalls := []string{
		"NetworkCreate:" + testNetwork,
		"ConfigCreate:" + testConfig,
		"VolumeCreate:" + testVolume,
	}
	if !slices.Equal(fake.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v (the old config must wait for Prune)", fake.calls, wantCalls)
	}
	wantCreated := []string{"Network/" + testNetwork, "Config/" + testConfig, "Volume/" + testVolume}
	if !slices.Equal(result.Created, wantCreated) {
		t.Fatalf("created = %v, want %v", result.Created, wantCreated)
	}

	owned := desired.Labels("web", "prod")
	if got := fake.networks.Items[0].Labels; !maps.Equal(got, owned) {
		t.Errorf("network labels = %v, want ownership labels", got)
	}
	if got := fake.volumes.Items[0].Labels; !maps.Equal(got, owned) {
		t.Errorf("volume labels = %v, want ownership labels", got)
	}
	if got := fake.volumes.Items[0].Options; got["type"] != "none" {
		t.Errorf("volume options = %v", got)
	}
	newest := fake.configs.Items[len(fake.configs.Items)-1]
	if newest.Spec.Name != testConfig || string(newest.Spec.Data) != "server {}" || !maps.Equal(newest.Spec.Labels, owned) {
		t.Errorf("config = %+v", newest.Spec)
	}

	observed, err := newRuntime(fake).Inspect(context.Background(), runtime.Scope{Application: "web", Target: "prod"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(observed.Networks) != 1 || len(observed.Volumes) != 1 || len(observed.Configs) != 2 {
		t.Fatalf("observed after apply = %d networks, %d volumes, %d configs",
			len(observed.Networks), len(observed.Volumes), len(observed.Configs))
	}
}

func TestApplyIsNoopWhenInSync(t *testing.T) {
	fake := &fakeAPI{}
	first := testPlan(testDesired(),
		create(desired.KindNetwork, testNetwork),
		create(desired.KindConfig, testConfig),
		create(desired.KindVolume, testVolume),
	)
	if _, err := newRuntime(fake).Apply(context.Background(), first); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	fake.calls = nil
	noop := testPlan(testDesired(),
		runtime.Entry{Key: "Network/" + testNetwork, Action: runtime.Noop},
		runtime.Entry{Key: "Config/" + testConfig, Action: runtime.Noop},
		runtime.Entry{Key: "Volume/" + testVolume, Action: runtime.Noop},
	)
	result, err := newRuntime(fake).Apply(context.Background(), noop)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(fake.calls) != 0 || len(result.Created) != 0 {
		t.Fatalf("re-applying an in-sync plan mutated the cluster: calls = %v, result = %+v", fake.calls, result)
	}
}

func TestApplyRejectsServiceChangesBeforeMutating(t *testing.T) {
	fake := &fakeAPI{}
	plan := testPlan(testDesired(),
		create(desired.KindNetwork, testNetwork),
		create(desired.KindService, "wormcd_web_prod_web"),
	)

	_, err := newRuntime(fake).Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "#18") {
		t.Fatalf("error = %v, want a not-implemented error for services", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none: the plan must be rejected before any change", fake.calls)
	}
}

func TestApplyRejectsImmutableUpdates(t *testing.T) {
	tests := []struct {
		name  string
		entry runtime.Entry
		want  string
	}{
		{
			name:  "config",
			entry: runtime.Entry{Key: "Config/" + testConfig, Action: runtime.Update, Changes: []string{"labels.team"}},
			want:  "configs and secrets are immutable",
		},
		{
			name:  "network",
			entry: runtime.Entry{Key: "Network/" + testNetwork, Action: runtime.Update, Changes: []string{"driver"}},
			want:  "in-place update is not supported",
		},
		{
			name:  "volume",
			entry: runtime.Entry{Key: "Volume/" + testVolume, Action: runtime.Update, Changes: []string{"driverOpts"}},
			want:  "in-place update is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeAPI{}
			_, err := newRuntime(fake).Apply(context.Background(), testPlan(testDesired(), tt.entry))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("calls = %v, want none", fake.calls)
			}
		})
	}
}

func TestApplyRejectsManagedSecret(t *testing.T) {
	desiredState := testDesired()
	desiredState.Secrets = []desired.Secret{{Name: "managed", Source: "managed"}}
	fake := &fakeAPI{}

	_, err := newRuntime(fake).Apply(context.Background(), testPlan(desiredState, create(desired.KindSecret, "managed")))
	if err == nil || !strings.Contains(err.Error(), "managed secrets are not supported") {
		t.Fatalf("error = %v, want a managed-secret error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}

func TestApplyRejectsPlanForAnotherTarget(t *testing.T) {
	desiredState := testDesired()
	desiredState.Target = "staging"
	plan := testPlan(desiredState, create(desired.KindNetwork, testNetwork))
	plan.Target = "prod"
	fake := &fakeAPI{}

	_, err := newRuntime(fake).Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "belongs to web/staging") {
		t.Fatalf("error = %v, want a target mismatch error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}

func TestApplyStopsOnAPIError(t *testing.T) {
	fake := &fakeAPI{err: errors.New("boom")}
	_, err := newRuntime(fake).Apply(context.Background(), testPlan(testDesired(),
		create(desired.KindNetwork, testNetwork),
		create(desired.KindVolume, testVolume),
	))
	if err == nil || !strings.Contains(err.Error(), "create network "+testNetwork+": boom") {
		t.Fatalf("error = %v, want a create-network error", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %v, want the volume not to be attempted after the failure", fake.calls)
	}
}

func TestHealthReportsNotImplementedAsUnhealthy(t *testing.T) {
	health, err := newRuntime(&fakeAPI{}).Health(context.Background(), runtime.Scope{Application: "web", Target: "prod"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Healthy {
		t.Fatal("Health reported healthy, want not healthy until services are implemented")
	}
	if !slices.Equal(health.Problems, []string{"not implemented (see #18)"}) {
		t.Fatalf("problems = %v, want the not-implemented problem", health.Problems)
	}
}
