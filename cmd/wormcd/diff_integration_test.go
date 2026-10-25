package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/cluster"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/manifest"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/runtime/swarm"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// TestIntegrationDiff is the end-to-end M0 stand: it creates a disposable
// labelled stack on a real Swarm manager, checks that a missing service shows
// up as Delete and a manual image change as Update, and verifies that diff
// leaves the cluster untouched. Enable it with WORMCD_INTEGRATION=1.
func TestIntegrationDiff(t *testing.T) {
	if os.Getenv("WORMCD_INTEGRATION") == "" {
		t.Skip("set WORMCD_INTEGRATION=1 to run against a Docker daemon")
	}

	cfg := diffIntegrationConfig()
	api, err := cluster.New(cfg)
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	t.Cleanup(func() {
		if err := api.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	root := writeDiffStand(t, app, target)

	state, err := manifest.Load(ctx, manifest.Options{App: app + "/" + target, Root: root})
	if err != nil {
		t.Fatalf("load stand: %v", err)
	}
	scope := runtime.Scope{Application: state.Application, Target: state.Target}

	// Create the objects a real apply would create, so the stand covers
	// networks, configs, external secrets, volumes and published ports.
	networkSpec := state.Networks[0]
	createdNetwork, err := api.NetworkCreate(ctx, networkSpec.Name, client.NetworkCreateOptions{
		Driver:     networkSpec.Driver,
		Labels:     networkSpec.Labels,
		Attachable: networkSpec.Attachable,
		Internal:   networkSpec.Internal,
	})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	cleanupDiff(t, "network", func(ctx context.Context) error {
		_, err := api.NetworkRemove(ctx, createdNetwork.ID, client.NetworkRemoveOptions{})
		return err
	})

	configSpec := state.Configs[0]
	createdConfig, err := api.ConfigCreate(ctx, client.ConfigCreateOptions{
		Spec: swarmtypes.ConfigSpec{
			Annotations: swarmtypes.Annotations{Name: configSpec.Name, Labels: configSpec.Labels},
			Data:        configSpec.Data,
		},
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	cleanupDiff(t, "config", func(ctx context.Context) error {
		_, err := api.ConfigRemove(ctx, createdConfig.ID, client.ConfigRemoveOptions{})
		return err
	})

	// Secrets are external in v0: the object exists in the cluster but carries
	// no WormCD labels, so Inspect must not plan it.
	secretSpec := state.Secrets[0]
	createdSecret, err := api.SecretCreate(ctx, client.SecretCreateOptions{
		Spec: swarmtypes.SecretSpec{
			Annotations: swarmtypes.Annotations{Name: secretSpec.Name},
			Data:        []byte("stand-secret"),
		},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	cleanupDiff(t, "secret", func(ctx context.Context) error {
		_, err := api.SecretRemove(ctx, createdSecret.ID, client.SecretRemoveOptions{})
		return err
	})

	volumeSpec := state.Volumes[0]
	if _, err := api.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:       volumeSpec.Name,
		Driver:     volumeSpec.Driver,
		DriverOpts: volumeSpec.DriverOpts,
		Labels:     volumeSpec.Labels,
	}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	cleanupDiff(t, "volume", func(ctx context.Context) error {
		_, err := api.VolumeRemove(ctx, volumeSpec.Name, client.VolumeRemoveOptions{Force: true})
		return err
	})

	webSpec := state.Services[0]
	web := createDiffService(t, ctx, api, webSpec, createdNetwork.ID, createdConfig.ID, createdSecret.ID)
	cleanupDiff(t, "service", func(ctx context.Context) error {
		_, err := api.ServiceRemove(ctx, web.ID, client.ServiceRemoveOptions{})
		return err
	})
	waitForServiceSettled(t, ctx, api, web.ID)

	oldSpec := webSpec
	oldSpec.Name = state.Namespace + "_old"
	oldSpec.Ports = nil
	oldSpec.EndpointMode = ""
	old := createDiffService(t, ctx, api, oldSpec, createdNetwork.ID, createdConfig.ID, createdSecret.ID)
	oldRemoved := false
	cleanupDiff(t, "service", func(ctx context.Context) error {
		if oldRemoved {
			return nil
		}
		_, err := api.ServiceRemove(ctx, old.ID, client.ServiceRemoveOptions{})
		return err
	})
	waitForServiceSettled(t, ctx, api, old.ID)

	inspector := swarm.New(api)

	// A matching service is Noop; the extra service is Delete.
	before := diffSnapshot(t, ctx, api, scope)
	plan, err := diffPlan(t, inspector, app, target, root)
	requireExitCode(t, err, 2)
	requireAction(t, plan, "Network/"+networkSpec.Name, runtime.Noop)
	requireAction(t, plan, "Config/"+configSpec.Name, runtime.Noop)
	requireAction(t, plan, "Volume/"+volumeSpec.Name, runtime.Noop)
	requireAction(t, plan, "Service/"+webSpec.Name, runtime.Noop)
	requireAction(t, plan, "Service/"+oldSpec.Name, runtime.Delete)
	if after := diffSnapshot(t, ctx, api, scope); after != before {
		t.Fatalf("diff mutated the cluster\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// A manual image change must show up as Update with the changed field.
	setDiffServiceImage(t, ctx, api, web.ID, "ghcr.io/d00p1/wormcd-demo:manual")
	before = diffSnapshot(t, ctx, api, scope)
	plan, err = diffPlan(t, inspector, app, target, root)
	requireExitCode(t, err, 2)
	webEntry := requireAction(t, plan, "Service/"+webSpec.Name, runtime.Update)
	if len(webEntry.Changes) != 1 || webEntry.Changes[0] != "image" {
		t.Fatalf("web changes = %v, want [image]", webEntry.Changes)
	}
	if after := diffSnapshot(t, ctx, api, scope); after != before {
		t.Fatalf("diff mutated the cluster\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// Removing the extra service and restoring the image leaves nothing to do.
	if _, err := api.ServiceRemove(ctx, old.ID, client.ServiceRemoveOptions{}); err != nil {
		t.Fatalf("remove old service: %v", err)
	}
	oldRemoved = true
	setDiffServiceImage(t, ctx, api, web.ID, webSpec.Image)

	before = diffSnapshot(t, ctx, api, scope)
	plan, err = diffPlan(t, inspector, app, target, root)
	requireExitCode(t, err, 0)
	if plan.HasChanges() {
		t.Fatalf("plan after cleanup has changes: %+v", plan.Entries)
	}
	for _, entry := range plan.Entries {
		if entry.Action != runtime.Noop {
			t.Fatalf("%s: action = %s, want Noop", entry.Key, entry.Action)
		}
	}
	if after := diffSnapshot(t, ctx, api, scope); after != before {
		t.Fatalf("diff mutated the cluster\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func writeDiffStand(t *testing.T, app, target string) string {
	t.Helper()

	root := t.TempDir()
	appDir := filepath.Join(root, "apps", "web")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("create app dir: %v", err)
	}

	compose, err := os.ReadFile(filepath.Join("testdata", "stand", "compose.yaml"))
	if err != nil {
		t.Fatalf("read fixture compose: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "compose.yaml"), compose, 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	application := fmt.Sprintf(`apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: %s
spec:
  compose: compose.yaml
  targets:
    - name: %s
      cluster: local
`, app, target)
	if err := os.WriteFile(filepath.Join(appDir, "wormcd.yaml"), []byte(application), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return root
}

func createDiffService(
	t *testing.T,
	ctx context.Context,
	api *client.Client,
	service desired.Service,
	networkID, configID, secretID string,
) swarmtypes.Service {
	t.Helper()

	container := &swarmtypes.ContainerSpec{Image: service.Image}
	for _, ref := range service.Configs {
		container.Configs = append(container.Configs, &swarmtypes.ConfigReference{
			ConfigID:   configID,
			ConfigName: ref.Source,
			File: &swarmtypes.ConfigReferenceFileTarget{
				Name: ref.Target,
				UID:  ref.UID,
				GID:  ref.GID,
				Mode: os.FileMode(ref.Mode),
			},
		})
	}
	for _, ref := range service.Secrets {
		container.Secrets = append(container.Secrets, &swarmtypes.SecretReference{
			SecretID:   secretID,
			SecretName: ref.Source,
			File: &swarmtypes.SecretReferenceFileTarget{
				Name: ref.Target,
				UID:  ref.UID,
				GID:  ref.GID,
				Mode: os.FileMode(ref.Mode),
			},
		})
	}
	for _, ref := range service.Mounts {
		container.Mounts = append(container.Mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   ref.Source,
			Target:   ref.Target,
			ReadOnly: ref.ReadOnly,
		})
	}

	replicas := service.Replicas
	spec := swarmtypes.ServiceSpec{
		Annotations: swarmtypes.Annotations{Name: service.Name, Labels: service.Labels},
		TaskTemplate: swarmtypes.TaskSpec{
			ContainerSpec: container,
			Networks:      []swarmtypes.NetworkAttachmentConfig{{Target: networkID}},
		},
		Mode: swarmtypes.ServiceMode{
			Replicated: &swarmtypes.ReplicatedService{Replicas: &replicas},
		},
	}
	if service.EndpointMode != "" || len(service.Ports) > 0 {
		endpoint := &swarmtypes.EndpointSpec{Mode: swarmtypes.ResolutionMode(service.EndpointMode)}
		for _, port := range service.Ports {
			endpoint.Ports = append(endpoint.Ports, swarmtypes.PortConfig{
				Name:          port.Name,
				TargetPort:    port.Target,
				PublishedPort: port.Published,
				Protocol:      network.IPProtocol(port.Protocol),
				PublishMode:   swarmtypes.PortConfigPublishMode(port.Mode),
			})
		}
		spec.EndpointSpec = endpoint
	}

	created, err := api.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec})
	if err != nil {
		t.Fatalf("create service %s: %v", service.Name, err)
	}

	inspected, err := api.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspect service %s: %v", service.Name, err)
	}
	return inspected.Service
}

// waitForServiceSettled waits until Docker stops applying its own post-create
// updates to the service spec. Only the service version is used: a diff must
// not change it, but creation itself may bump it asynchronously.
func waitForServiceSettled(t *testing.T, ctx context.Context, api *client.Client, serviceID string) {
	t.Helper()

	const (
		interval = 100 * time.Millisecond
		stable   = 3
	)

	last := uint64(0)
	unchanged := 0
	for {
		inspected, err := api.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err != nil {
			t.Fatalf("inspect service while waiting for it to settle: %v", err)
		}
		version := inspected.Service.Version.Index
		if version == last {
			unchanged++
			if unchanged >= stable {
				return
			}
		} else {
			last = version
			unchanged = 0
		}

		select {
		case <-ctx.Done():
			t.Fatalf("service did not settle: %v", ctx.Err())
		case <-time.After(interval):
		}
	}
}

func setDiffServiceImage(t *testing.T, ctx context.Context, api *client.Client, serviceID, image string) {
	t.Helper()

	inspected, err := api.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspect service: %v", err)
	}
	spec := inspected.Service.Spec
	spec.TaskTemplate.ContainerSpec.Image = image
	if _, err := api.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: inspected.Service.Version,
		Spec:    spec,
	}); err != nil {
		t.Fatalf("update service image: %v", err)
	}
}

func diffPlan(t *testing.T, inspector diffInspector, app, target, root string) (runtime.Plan, error) {
	t.Helper()

	var out bytes.Buffer
	err := runDiffWith(
		[]string{"--app", app + "/" + target, "--root", root, "--output", "json"},
		&out,
		func(cfg cluster.Config) (diffInspector, func(), error) {
			if cfg.Name != "local" {
				t.Errorf("cluster name = %q, want local", cfg.Name)
			}
			return inspector, func() {}, nil
		},
	)
	if out.Len() == 0 {
		return runtime.Plan{}, err
	}

	var plan runtime.Plan
	if decodeErr := json.Unmarshal(out.Bytes(), &plan); decodeErr != nil {
		t.Fatalf("decode plan: %v\noutput:\n%s", decodeErr, out.String())
	}
	return plan, err
}

func requireExitCode(t *testing.T, err error, code int) {
	t.Helper()

	if code == 0 {
		if err != nil {
			t.Fatalf("error = %v, want success", err)
		}
		return
	}

	var exit *exitError
	if !errors.As(err, &exit) || exit.code != code {
		t.Fatalf("error = %v, want exit code %d", err, code)
	}
}

func requireAction(t *testing.T, plan runtime.Plan, key string, action runtime.Action) runtime.Entry {
	t.Helper()

	for _, entry := range plan.Entries {
		if entry.Key == key {
			if entry.Action != action {
				t.Fatalf("%s: action = %s, want %s (%v)", key, entry.Action, action, entry.Changes)
			}
			return entry
		}
	}
	t.Fatalf("%s: not found in plan %+v", key, plan.Entries)
	return runtime.Entry{}
}

func diffSnapshot(t *testing.T, ctx context.Context, api *client.Client, scope runtime.Scope) string {
	t.Helper()

	observed, err := swarm.New(api).Inspect(ctx, scope)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	services, err := api.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	scoped := make(map[string]struct{}, len(observed.Services))
	for _, service := range observed.Services {
		scoped[service.Name] = struct{}{}
	}
	versions := make(map[string]uint64, len(scoped))
	for _, service := range services.Items {
		if _, ok := scoped[service.Spec.Name]; !ok {
			continue
		}
		versions[service.Spec.Name] = service.Version.Index
	}

	data, err := json.Marshal(struct {
		Observed        runtime.Observed
		ServiceVersions map[string]uint64
	}{
		Observed:        observed,
		ServiceVersions: versions,
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(data)
}

func diffIntegrationConfig() cluster.Config {
	endpoint := os.Getenv("WORMCD_DOCKER_ENDPOINT")
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	return cluster.Config{
		Name:     "local",
		Endpoint: endpoint,
		CAFile:   os.Getenv("WORMCD_DOCKER_CA"),
		CertFile: os.Getenv("WORMCD_DOCKER_CERT"),
		KeyFile:  os.Getenv("WORMCD_DOCKER_KEY"),
	}
}

func cleanupDiff(t *testing.T, what string, remove func(context.Context) error) {
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
