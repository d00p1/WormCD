package swarm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/cluster"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/client"
)

// TestIntegrationInspect is read-only: it skips unless the daemon is a Swarm
// manager, and inspects a scope that cannot exist. Enable it with
// WORMCD_INTEGRATION=1.
func TestIntegrationInspect(t *testing.T) {
	if os.Getenv("WORMCD_INTEGRATION") == "" {
		t.Skip("set WORMCD_INTEGRATION=1 to run against a Docker daemon")
	}

	endpoint := os.Getenv("WORMCD_DOCKER_ENDPOINT")
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}

	api, err := cluster.New(cluster.Config{Name: "integration", Endpoint: endpoint})
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	t.Cleanup(func() {
		if err := api.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := api.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info.Info.Swarm.ControlAvailable {
		t.Skip("Docker daemon is not a Swarm manager")
	}

	observed, err := New(api).Inspect(ctx, runtime.Scope{
		Application: "__wormcd_missing__",
		Target:      "__wormcd_missing__",
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(observed.Services) != 0 || len(observed.Networks) != 0 ||
		len(observed.Configs) != 0 || len(observed.Secrets) != 0 || len(observed.Volumes) != 0 {
		t.Fatalf("inspect of a missing scope returned objects: %+v", observed)
	}
}
