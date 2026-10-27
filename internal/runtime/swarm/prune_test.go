package swarm

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

const (
	testService     = "wormcd_web_prod_web"
	testBusyConfig  = "wormcd_web_prod_nginx_busy"
	testUsedNetwork = "wormcd_web_prod_used"
)

func testPruneScope() runtime.Scope {
	return runtime.Scope{Application: "web", Target: "prod"}
}

func testPruneFake() *fakeAPI {
	labels := desired.Labels("web", "prod")
	staging := desired.Labels("web", "staging")
	return &fakeAPI{
		services: client.ServiceListResult{Items: []swarm.Service{{
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{Name: testService, Labels: labels},
				TaskTemplate: swarm.TaskSpec{
					ContainerSpec: &swarm.ContainerSpec{
						Image: "web:v1",
						Mounts: []mount.Mount{
							{Type: mount.TypeVolume, Source: testVolume, Target: "/data"},
						},
						Configs: []*swarm.ConfigReference{{ConfigName: testBusyConfig}},
					},
					Networks: []swarm.NetworkAttachmentConfig{{Target: "id-" + testUsedNetwork}},
				},
			},
		}}},
		networks: client.NetworkListResult{Items: []network.Summary{
			{Network: network.Network{ID: "id-" + testNetwork, Name: testNetwork, Labels: labels}},
			{Network: network.Network{ID: "id-old", Name: "wormcd_web_prod_old", Labels: labels}},
			{Network: network.Network{ID: "id-" + testUsedNetwork, Name: testUsedNetwork, Labels: labels}},
			{Network: network.Network{ID: "id-staging", Name: "wormcd_web_staging_default", Labels: staging}},
			{Network: network.Network{ID: "id-shared", Name: "shared"}},
		}},
		configs: client.ConfigListResult{Items: []swarm.Config{
			{ID: "id-" + testConfig, Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: testConfig, Labels: labels}}},
			{ID: "id-" + testOldConfig, Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: testOldConfig, Labels: labels}}},
			{ID: "id-" + testBusyConfig, Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: testBusyConfig, Labels: labels}}},
		}},
		secrets: client.SecretListResult{Items: []swarm.Secret{
			{ID: "id-managed", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "managed_old", Labels: labels}}},
			{ID: "id-external", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "prod_tls"}}},
		}},
		volumes: client.VolumeListResult{Items: []volume.Volume{
			{Name: testVolume, Labels: labels},
			{Name: "wormcd_web_prod_scratch", Labels: labels},
			{Name: "unrelated"},
		}},
	}
}

func testKeep() runtime.Set {
	return runtime.Set{
		{Kind: desired.KindService, Name: testService}: {},
		{Kind: desired.KindNetwork, Name: testNetwork}: {},
		{Kind: desired.KindConfig, Name: testConfig}:   {},
		{Kind: desired.KindVolume, Name: testVolume}:   {},
	}
}

func TestPruneRemovesUnkeptManagedObjects(t *testing.T) {
	fake := testPruneFake()

	result, err := newRuntime(fake).Prune(context.Background(), testPruneScope(), testKeep())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	want := []string{
		"NetworkRemove:id-old",
		"ConfigRemove:id-" + testOldConfig,
		"SecretRemove:id-managed",
	}
	if !slices.Equal(fake.calls, want) {
		t.Fatalf("calls = %v, want %v", fake.calls, want)
	}
	wantRemoved := []string{"Network/wormcd_web_prod_old", "Config/" + testOldConfig, "Secret/managed_old"}
	if !slices.Equal(result.Removed, wantRemoved) {
		t.Fatalf("removed = %v, want %v", result.Removed, wantRemoved)
	}

	fake.calls = nil
	if _, err := newRuntime(fake).Prune(context.Background(), testPruneScope(), testKeep()); err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("second Prune mutated the cluster: %v", fake.calls)
	}
}

func TestPruneNeverRemovesVolumes(t *testing.T) {
	fake := testPruneFake()

	result, err := newRuntime(fake).Prune(context.Background(), testPruneScope(), testKeep())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, call := range fake.calls {
		if strings.HasPrefix(call, "VolumeRemove:") {
			t.Fatalf("Prune removed a volume: %s", call)
		}
	}
	if len(fake.volumes.Items) != 3 {
		t.Fatalf("volumes = %d, want all 3 to remain", len(fake.volumes.Items))
	}
	want := runtime.Skip{Key: "Volume/wormcd_web_prod_scratch", Reason: "volumes hold data and are never removed automatically; remove it manually if the data is no longer needed"}
	if !slices.Contains(result.Skipped, want) {
		t.Fatalf("skipped = %+v, want %+v", result.Skipped, want)
	}
}

func TestPruneReportsObjectsInUseByServices(t *testing.T) {
	fake := testPruneFake()

	result, err := newRuntime(fake).Prune(context.Background(), testPruneScope(), testKeep())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, call := range fake.calls {
		if strings.Contains(call, testBusyConfig) || strings.Contains(call, testUsedNetwork) {
			t.Fatalf("Prune removed an object that a service still uses: %s", call)
		}
	}
	for _, key := range []string{"Config/" + testBusyConfig, "Network/" + testUsedNetwork} {
		if !slices.Contains(result.Skipped, runtime.Skip{Key: key, Reason: "in use by a service"}) {
			t.Fatalf("skipped = %+v, want %s reported as in use", result.Skipped, key)
		}
	}
}

func TestPruneRejectsUnkeptService(t *testing.T) {
	fake := testPruneFake()
	keep := testKeep()
	delete(keep, desired.Key{Kind: desired.KindService, Name: testService})

	_, err := newRuntime(fake).Prune(context.Background(), testPruneScope(), keep)
	if err == nil || !strings.Contains(err.Error(), "#18") {
		t.Fatalf("error = %v, want a not-implemented error for services", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}

func TestPruneRejectsEmptyScope(t *testing.T) {
	_, err := newRuntime(&fakeAPI{}).Prune(context.Background(), runtime.Scope{}, testKeep())
	if err == nil || !strings.Contains(err.Error(), "application and target are required") {
		t.Fatalf("error = %v, want a scope validation error", err)
	}
}
