package swarm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/cluster"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
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

// TestIntegrationInspectStack creates a disposable labelled stack, verifies
// that Inspect normalizes it, and removes everything again. It mutates the
// Swarm manager, so it only runs with WORMCD_INTEGRATION=1.
func TestIntegrationInspectStack(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	info, err := api.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info.Info.Swarm.ControlAvailable {
		t.Skip("Docker daemon is not a Swarm manager")
	}

	app := fmt.Sprintf("it%x", time.Now().UnixNano())
	target := "prod"
	namespace := desired.TargetNamespace(app, target)
	labels := desired.Labels(app, target)

	networkName := namespace + "_default"
	createdNetwork, err := api.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
		Driver:     "overlay",
		Attachable: true,
		Labels:     labels,
	})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	cleanup(t, "network", func(ctx context.Context) error {
		_, err := api.NetworkRemove(ctx, createdNetwork.ID, client.NetworkRemoveOptions{})
		return err
	})

	configName := namespace + "_app_000000000000"
	createdConfig, err := api.ConfigCreate(ctx, client.ConfigCreateOptions{
		Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: configName, Labels: labels},
			Data:        []byte("integration"),
		},
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	cleanup(t, "config", func(ctx context.Context) error {
		_, err := api.ConfigRemove(ctx, createdConfig.ID, client.ConfigRemoveOptions{})
		return err
	})

	secretName := namespace + "_token"
	createdSecret, err := api.SecretCreate(ctx, client.SecretCreateOptions{
		Spec: swarm.SecretSpec{
			Annotations: swarm.Annotations{Name: secretName, Labels: labels},
			Data:        []byte("integration"),
		},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	cleanup(t, "secret", func(ctx context.Context) error {
		_, err := api.SecretRemove(ctx, createdSecret.ID, client.SecretRemoveOptions{})
		return err
	})

	volumeName := namespace + "_data"
	if _, err := api.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   volumeName,
		Labels: labels,
	}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	cleanup(t, "volume", func(ctx context.Context) error {
		_, err := api.VolumeRemove(ctx, volumeName, client.VolumeRemoveOptions{Force: true})
		return err
	})

	serviceName := namespace + "_web"
	zero := uint64(0)
	createdService, err := api.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: serviceName, Labels: labels},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{
					Image: "ghcr.io/d00p1/wormcd-integration:test",
					Env:   []string{"APP_ENV=integration"},
					Configs: []*swarm.ConfigReference{{
						ConfigName: configName,
						File: &swarm.ConfigReferenceFileTarget{
							Name: "/app/config",
							Mode: 0o444,
						},
					}},
					Secrets: []*swarm.SecretReference{{
						SecretName: secretName,
						File: &swarm.SecretReferenceFileTarget{
							Name: "/run/secrets/token",
							Mode: 0o444,
						},
					}},
					Mounts: []mount.Mount{{
						Type:   mount.TypeVolume,
						Source: volumeName,
						Target: "/data",
					}},
				},
				Networks: []swarm.NetworkAttachmentConfig{{
					Target:  createdNetwork.ID,
					Aliases: []string{"web"},
				}},
				RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny},
			},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &zero}},
			EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP},
		},
	})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	cleanup(t, "service", func(ctx context.Context) error {
		_, err := api.ServiceRemove(ctx, createdService.ID, client.ServiceRemoveOptions{})
		return err
	})

	observed, err := New(api).Inspect(ctx, runtime.Scope{Application: app, Target: target})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if len(observed.Services) != 1 || len(observed.Networks) != 1 ||
		len(observed.Configs) != 1 || len(observed.Secrets) != 1 || len(observed.Volumes) != 1 {
		t.Fatalf("observed counts = services:%d networks:%d configs:%d secrets:%d volumes:%d",
			len(observed.Services), len(observed.Networks), len(observed.Configs),
			len(observed.Secrets), len(observed.Volumes))
	}

	service := observed.Services[0]
	if service.Name != serviceName || service.Replicas != 0 || service.EndpointMode != "vip" {
		t.Errorf("service = %s replicas=%d endpoint=%s", service.Name, service.Replicas, service.EndpointMode)
	}
	if service.Env["APP_ENV"] != "integration" {
		t.Errorf("env = %v", service.Env)
	}
	if len(service.Networks) != 1 || service.Networks[0].Name != networkName {
		t.Errorf("service networks = %+v, want %s", service.Networks, networkName)
	}
	if len(service.Configs) != 1 || service.Configs[0].Source != configName {
		t.Errorf("service configs = %+v, want %s", service.Configs, configName)
	}
	if len(service.Secrets) != 1 || service.Secrets[0].Source != secretName {
		t.Errorf("service secrets = %+v, want %s", service.Secrets, secretName)
	}
	if len(service.Mounts) != 1 || service.Mounts[0].Source != volumeName {
		t.Errorf("service mounts = %+v, want %s", service.Mounts, volumeName)
	}

	if observed.Networks[0].Name != networkName || observed.Configs[0].Name != configName ||
		observed.Secrets[0].Name != secretName || observed.Volumes[0].Name != volumeName {
		t.Errorf("observed objects do not match the created stack: %+v", observed)
	}
}

func cleanup(t *testing.T, what string, remove func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = remove(ctx); err == nil {
				return
			}
			time.Sleep(time.Second)
		}
		t.Errorf("remove %s: %v", what, err)
	})
}
