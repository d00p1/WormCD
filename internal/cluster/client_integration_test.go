package cluster

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// TestIntegrationPing talks to a real Docker daemon. It is skipped unless
// WORMCD_INTEGRATION is set, so unit tests stay hermetic.
func TestIntegrationPing(t *testing.T) {
	if os.Getenv("WORMCD_INTEGRATION") == "" {
		t.Skip("set WORMCD_INTEGRATION=1 to run against a Docker daemon")
	}

	endpoint := os.Getenv("WORMCD_DOCKER_ENDPOINT")
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}

	cli, err := New(Config{
		Name:     "integration",
		Endpoint: endpoint,
		CAFile:   os.Getenv("WORMCD_DOCKER_CA"),
		CertFile: os.Getenv("WORMCD_DOCKER_CERT"),
		KeyFile:  os.Getenv("WORMCD_DOCKER_KEY"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := cli.Ping(ctx, client.PingOptions{})
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Logf("connected to Docker API %s", res.APIVersion)
}
