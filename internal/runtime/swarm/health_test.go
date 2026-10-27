package swarm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

func healthScope() runtime.Scope {
	return runtime.Scope{Application: "web", Target: "prod"}
}

// replicatedService is a running service with the given replica count, whose
// tasks are all running.
func replicatedService(replicas uint64) swarm.Service {
	service := runningService("web:v1")
	service.Spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}}
	return service
}

func healthFake(services ...swarm.Service) *fakeAPI {
	fake := &fakeAPI{}
	fake.services.Items = services
	for _, service := range services {
		fake.startTasks(service)
	}
	return fake
}

func TestHealthReportsConvergedService(t *testing.T) {
	service := replicatedService(2)
	health, err := newRuntime(healthFake(service)).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Healthy || len(health.Problems) != 0 {
		t.Fatalf("health = %+v, want healthy with no problems", health)
	}
}

func TestHealthReportsMissingReplicas(t *testing.T) {
	service := replicatedService(3)
	fake := healthFake(service)
	fake.tasks[0].Status.State = swarm.TaskStatePending

	health, err := newRuntime(fake).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	want := []string{"service " + testService + ": 2 of 3 tasks running"}
	if health.Healthy || !slices.Equal(health.Problems, want) {
		t.Fatalf("health = %+v, want unhealthy with %v", health, want)
	}
}

func TestHealthReportsFailedTasks(t *testing.T) {
	service := replicatedService(1)
	fake := healthFake(service)
	fake.tasks = failedTasks("pull access denied for web:v1")(service)

	health, err := newRuntime(fake).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Healthy {
		t.Fatal("Health reported healthy with a failed task")
	}
	joined := strings.Join(health.Problems, "\n")
	if !strings.Contains(joined, "pull access denied for web:v1") || !strings.Contains(joined, "service "+testService+":") {
		t.Fatalf("problems = %v, want the task error named by its service", health.Problems)
	}
}

func TestHealthReportsPausedUpdate(t *testing.T) {
	fake := healthFake(replicatedService(1))
	fake.services.Items[0].UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStatePaused, Message: "task failed to start"}

	health, err := newRuntime(fake).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Healthy || !slices.ContainsFunc(health.Problems, func(p string) bool {
		return strings.Contains(p, "update paused: task failed to start")
	}) {
		t.Fatalf("health = %+v, want the paused update reported", health)
	}
}

func TestHealthReportsUpdateInProgress(t *testing.T) {
	fake := healthFake(replicatedService(1))
	fake.services.Items[0].UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateUpdating}

	health, err := newRuntime(fake).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Healthy || !slices.ContainsFunc(health.Problems, func(p string) bool {
		return strings.HasSuffix(p, "update in progress")
	}) {
		t.Fatalf("health = %+v, want an update in progress reported", health)
	}
}

// TestHealthIgnoresOtherTargets checks that a broken service of another target
// does not make this one unhealthy.
func TestHealthIgnoresOtherTargets(t *testing.T) {
	healthy := replicatedService(1)
	other := replicatedService(1)
	other.Spec.Name = "wormcd_web_staging_web"
	other.Spec.Labels = desired.Labels("web", "staging")
	other.ID = "id-other"
	fake := healthFake(healthy, other)
	fake.tasks = append(fake.tasks, failedTasks("boom")(other)...)

	health, err := newRuntime(fake).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Healthy {
		t.Fatalf("health = %+v, want healthy: the broken service is another target's", health)
	}
}

func TestHealthOfEmptyScopeIsNotHealthy(t *testing.T) {
	health, err := newRuntime(&fakeAPI{}).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Healthy || !slices.Equal(health.Problems, []string{"no objects found for the target"}) {
		t.Fatalf("health = %+v, want not healthy: nothing is deployed", health)
	}
}

func TestHealthOfTargetWithoutServicesIsHealthy(t *testing.T) {
	networks := testPruneFake().networks
	fake := &fakeAPI{networks: client.NetworkListResult{Items: networks.Items[:1]}}

	health, err := newRuntime(fake).Health(context.Background(), healthScope())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Healthy || len(health.Problems) != 0 {
		t.Fatalf("health = %+v, want healthy: the target has objects and no service to run", health)
	}
}

func TestHealthReturnsAPIError(t *testing.T) {
	_, err := newRuntime(&fakeAPI{err: errors.New("daemon down")}).Health(context.Background(), healthScope())
	if err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("error = %v, want the API error", err)
	}
}
