package swarm

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// fastConvergence keeps the convergence wait short enough for tests.
var fastConvergence = convergence{Interval: time.Millisecond, Timeout: time.Second}

// withService adds the test service to the desired state. It references the
// test config and volume, so Apply has to resolve real IDs for it.
func withService(desiredState desired.Desired, image string) desired.Desired {
	desiredState.Services = []desired.Service{{
		Name:         testService,
		Image:        image,
		Mode:         desired.ModeReplicated,
		Replicas:     1,
		EndpointMode: desired.DefaultEndpointMode,
		Labels:       desired.Labels("web", "prod"),
		Networks:     []desired.ServiceNetwork{{Name: testNetwork}},
		Mounts:       []desired.Mount{{Type: desired.MountVolume, Source: testVolume, Target: "/data"}},
		Configs:      []desired.ConfigRef{{Source: testConfig, Target: "/etc/nginx/nginx.conf", Mode: 0o444}},
		Restart:      desired.DefaultRestartPolicy(),
		Update:       desired.DefaultUpdateConfig(),
		Rollback:     desired.DefaultUpdateConfig(),
	}}
	return desiredState
}

func serviceKey() string {
	return desired.Key{Kind: desired.KindService, Name: testService}.String()
}

func updateEntry(changes ...string) runtime.Entry {
	return runtime.Entry{Key: serviceKey(), Action: runtime.Update, Changes: changes}
}

// runningService is a service already in the cluster at version 7, with its
// tasks running.
func runningService(image string) swarm.Service {
	return swarm.Service{
		ID:   "id-" + testService,
		Meta: swarm.Meta{Version: swarm.Version{Index: 7}},
		Spec: swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: testService, Labels: desired.Labels("web", "prod")},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image}},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: uint64Ptr(1)}},
		},
	}
}

// clusterWithService holds the objects the service needs, plus the service
// itself when given.
func clusterWithService(service *swarm.Service) *fakeAPI {
	fake := &fakeAPI{
		configs: client.ConfigListResult{Items: []swarm.Config{{
			ID:   "id-" + testConfig,
			Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: testConfig, Labels: desired.Labels("web", "prod")}},
		}}},
	}
	if service != nil {
		fake.services.Items = []swarm.Service{*service}
		fake.startTasks(*service)
	}
	return fake
}

func TestApplyCreatesServiceAfterItsDependencies(t *testing.T) {
	fake := &fakeAPI{}
	plan := testPlan(withService(testDesired(), "web:v1"),
		create(desired.KindNetwork, testNetwork),
		create(desired.KindConfig, testConfig),
		create(desired.KindVolume, testVolume),
		create(desired.KindService, testService),
	)

	result, err := newRuntime(fake).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	wantCalls := []string{
		"NetworkCreate:" + testNetwork,
		"ConfigCreate:" + testConfig,
		"VolumeCreate:" + testVolume,
		"ServiceCreate:" + testService,
	}
	if !slices.Equal(fake.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", fake.calls, wantCalls)
	}
	if !slices.Contains(result.Created, serviceKey()) {
		t.Fatalf("created = %v, want %s", result.Created, serviceKey())
	}

	if len(fake.services.Items) != 1 {
		t.Fatalf("services = %d, want 1", len(fake.services.Items))
	}
	service := fake.services.Items[0]
	if service.Spec.TaskTemplate.ContainerSpec.Image != "web:v1" {
		t.Errorf("image = %q", service.Spec.TaskTemplate.ContainerSpec.Image)
	}
	refs := service.Spec.TaskTemplate.ContainerSpec.Configs
	if len(refs) != 1 || refs[0].ConfigID != "id-"+testConfig {
		t.Errorf("config refs = %+v, want the ID of the config created by this Apply", refs)
	}
}

// TestApplyUpdatesServiceWithItsVersion checks that ServiceUpdate receives the
// version read when Apply runs; the fake rejects any other version.
func TestApplyUpdatesServiceWithItsVersion(t *testing.T) {
	existing := runningService("web:v1")
	fake := clusterWithService(&existing)
	plan := completePlan(withService(testDesired(), "web:v2"), updateEntry("image"))

	result, err := newRuntime(fake).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(fake.calls, "ServiceUpdate:"+existing.ID) {
		t.Fatalf("calls = %v, want a ServiceUpdate of %s", fake.calls, existing.ID)
	}
	if !slices.Contains(result.Updated, serviceKey()) {
		t.Fatalf("updated = %v, want %s", result.Updated, serviceKey())
	}
	service := fake.services.Items[0]
	if service.Spec.TaskTemplate.ContainerSpec.Image != "web:v2" {
		t.Errorf("image = %q, want web:v2", service.Spec.TaskTemplate.ContainerSpec.Image)
	}
	if service.Version.Index != 8 {
		t.Errorf("version = %d, want 8 after one update", service.Version.Index)
	}
}

func TestApplyNoopServiceMakesNoCalls(t *testing.T) {
	existing := runningService("web:v1")
	fake := clusterWithService(&existing)
	plan := completePlan(withService(testDesired(), "web:v1"), runtime.Entry{Key: serviceKey(), Action: runtime.Noop})

	if _, err := newRuntime(fake).Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "Service") {
			t.Fatalf("calls = %v, want no service call for a Noop", fake.calls)
		}
	}
}

func TestApplyFailsWhenNewServiceTasksFail(t *testing.T) {
	fake := clusterWithService(nil)
	fake.tasksFor = failedTasks("pull access denied for web:broken")
	plan := testPlan(withService(testDesired(), "web:broken"),
		create(desired.KindNetwork, testNetwork),
		create(desired.KindConfig, testConfig),
		create(desired.KindVolume, testVolume),
		create(desired.KindService, testService),
	)
	r := newRuntime(fake)
	r.converge = fastConvergence

	_, err := r.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("error = %v, want the task failure", err)
	}
	if !strings.Contains(err.Error(), "service "+testService) {
		t.Fatalf("error = %v, want the service named", err)
	}
}

func TestApplyFailsWhenUpdatePausesOnBrokenImage(t *testing.T) {
	existing := runningService("web:v1")
	fake := clusterWithService(&existing)
	fake.tasksFor = failedTasks("pull access denied for web:broken")
	plan := completePlan(withService(testDesired(), "web:broken"), updateEntry("image"))
	r := newRuntime(fake)
	r.converge = fastConvergence

	_, err := r.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "update paused") {
		t.Fatalf("error = %v, want the paused update", err)
	}
	if fake.services.Items[0].UpdateStatus.State != swarm.UpdateStatePaused {
		t.Errorf("update state = %s, want paused", fake.services.Items[0].UpdateStatus.State)
	}
}

// TestApplyWaitsForConvergence lets the tasks start only after a few polls. Apply
// must keep polling and succeed once they run.
func TestApplyWaitsForConvergence(t *testing.T) {
	fake := clusterWithService(nil)
	fake.tasksFor = pendingTasks
	polls := 0
	fake.onTaskList = func() {
		polls++
		if polls < 3 {
			return
		}
		for i := range fake.tasks {
			fake.tasks[i].Status.State = swarm.TaskStateRunning
		}
	}
	plan := completePlan(withService(testDesired(), "web:v1"), create(desired.KindService, testService))
	r := newRuntime(fake)
	r.converge = fastConvergence

	if _, err := r.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if polls < 3 {
		t.Fatalf("polls = %d, want Apply to wait for the tasks", polls)
	}
}

func TestApplyTimesOutWhenServiceDoesNotConverge(t *testing.T) {
	fake := clusterWithService(nil)
	fake.tasksFor = pendingTasks
	plan := completePlan(withService(testDesired(), "web:v1"), create(desired.KindService, testService))
	r := newRuntime(fake)
	r.converge = convergence{Interval: time.Millisecond, Timeout: 20 * time.Millisecond}

	_, err := r.Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "not converged (0 of 1 tasks running)") {
		t.Fatalf("error = %v, want a not-converged timeout", err)
	}
}

func TestApplyRefusesToUpdateServiceWithoutOwnership(t *testing.T) {
	foreign := runningService("web:v1")
	foreign.Spec.Labels = nil
	fake := clusterWithService(&foreign)
	plan := completePlan(withService(testDesired(), "web:v2"), updateEntry("image"))

	_, err := newRuntime(fake).Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "not owned by web/prod") {
		t.Fatalf("error = %v, want an ownership error", err)
	}
	if slices.ContainsFunc(fake.calls, func(call string) bool { return strings.HasPrefix(call, "ServiceUpdate") }) {
		t.Fatalf("calls = %v, want no ServiceUpdate of a foreign service", fake.calls)
	}
}

func TestApplyRefusesServiceModeChangeBeforeMutating(t *testing.T) {
	existing := runningService("web:v1")
	fake := clusterWithService(&existing)
	plan := completePlan(withService(testDesired(), "web:v1"), updateEntry("mode", "replicas"))

	_, err := newRuntime(fake).Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "mode cannot change in place") {
		t.Fatalf("error = %v, want a mode change refusal", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}

func TestApplyRefusesUndeclaredServiceReferenceBeforeMutating(t *testing.T) {
	desiredState := withService(testDesired(), "web:v1")
	desiredState.Services[0].Configs = []desired.ConfigRef{{Source: "wormcd_web_prod_missing", Target: "/x"}}
	fake := &fakeAPI{}
	plan := completePlan(desiredState, create(desired.KindService, testService))

	_, err := newRuntime(fake).Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "config wormcd_web_prod_missing is not declared") {
		t.Fatalf("error = %v, want an undeclared reference error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}

func TestApplyRejectsServiceMissingFromPlan(t *testing.T) {
	fake := &fakeAPI{}
	plan := testPlan(withService(testDesired(), "web:v1"),
		runtime.Entry{Key: "Network/" + testNetwork, Action: runtime.Noop},
		runtime.Entry{Key: "Config/" + testConfig, Action: runtime.Noop},
		runtime.Entry{Key: "Volume/" + testVolume, Action: runtime.Noop},
	)

	_, err := newRuntime(fake).Apply(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), serviceKey()+": missing from the plan") {
		t.Fatalf("error = %v, want a missing-entry error", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}
