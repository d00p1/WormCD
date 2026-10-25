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
	"github.com/d00p1/wormcd/internal/reconcile"
	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/d00p1/wormcd/internal/runtime/swarm"
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
	namespace := desired.TargetNamespace(app, target)
	labels := desired.Labels(app, target)
	scope := runtime.Scope{Application: app, Target: target}

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
	manifest := fmt.Sprintf(`apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: %s
spec:
  compose: compose.yaml
  targets:
    - name: %s
      cluster: local
`, app, target)
	if err := os.WriteFile(filepath.Join(appDir, "wormcd.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	networkName := namespace + "_default"
	createdNetwork, err := api.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
		Driver: "overlay",
		Labels: labels,
	})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	cleanupDiff(t, "network", func(ctx context.Context) error {
		_, err := api.NetworkRemove(ctx, createdNetwork.ID, client.NetworkRemoveOptions{})
		return err
	})

	image := "ghcr.io/d00p1/wormcd-demo:v1"
	webName := namespace + "_web"
	web := createDiffService(t, ctx, api, webName, image, createdNetwork.ID, labels)
	cleanupDiff(t, "service", func(ctx context.Context) error {
		_, err := api.ServiceRemove(ctx, web.ID, client.ServiceRemoveOptions{})
		return err
	})
	waitForServiceSettled(t, ctx, api, web.ID)

	oldName := namespace + "_old"
	old := createDiffService(t, ctx, api, oldName, image, createdNetwork.ID, labels)
	oldRemoved := false
	cleanupDiff(t, "service", func(ctx context.Context) error {
		if oldRemoved {
			return nil
		}
		_, err := api.ServiceRemove(ctx, old.ID, client.ServiceRemoveOptions{})
		return err
	})
	waitForServiceSettled(t, ctx, api, old.ID)

	args := diffArgs(cfg, app, target, root)

	// A matching service is Noop; the extra service is Delete.
	before := diffSnapshot(t, ctx, api, scope)
	plan, err := diffPlan(t, args)
	requireExitCode(t, err, 2)
	requireAction(t, plan, "Network/"+networkName, reconcile.Noop)
	requireAction(t, plan, "Service/"+webName, reconcile.Noop)
	requireAction(t, plan, "Service/"+oldName, reconcile.Delete)
	if after := diffSnapshot(t, ctx, api, scope); after != before {
		t.Fatalf("diff mutated the cluster\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// A manual image change must show up as Update with the changed field.
	setDiffServiceImage(t, ctx, api, web.ID, "ghcr.io/d00p1/wormcd-demo:manual")
	before = diffSnapshot(t, ctx, api, scope)
	plan, err = diffPlan(t, args)
	requireExitCode(t, err, 2)
	webEntry := requireAction(t, plan, "Service/"+webName, reconcile.Update)
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
	setDiffServiceImage(t, ctx, api, web.ID, image)

	before = diffSnapshot(t, ctx, api, scope)
	plan, err = diffPlan(t, args)
	requireExitCode(t, err, 0)
	if plan.HasChanges() {
		t.Fatalf("plan after cleanup has changes: %+v", plan.Entries)
	}
	for _, entry := range plan.Entries {
		if entry.Action != reconcile.Noop {
			t.Fatalf("%s: action = %s, want Noop", entry.Key, entry.Action)
		}
	}
	if after := diffSnapshot(t, ctx, api, scope); after != before {
		t.Fatalf("diff mutated the cluster\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func createDiffService(
	t *testing.T,
	ctx context.Context,
	api *client.Client,
	name, image, networkID string,
	labels map[string]string,
) swarmtypes.Service {
	t.Helper()

	zero := uint64(0)
	created, err := api.ServiceCreate(ctx, client.ServiceCreateOptions{
		Spec: swarmtypes.ServiceSpec{
			Annotations: swarmtypes.Annotations{Name: name, Labels: labels},
			TaskTemplate: swarmtypes.TaskSpec{
				ContainerSpec: &swarmtypes.ContainerSpec{Image: image},
				Networks:      []swarmtypes.NetworkAttachmentConfig{{Target: networkID}},
			},
			Mode: swarmtypes.ServiceMode{Replicated: &swarmtypes.ReplicatedService{Replicas: &zero}},
		},
	})
	if err != nil {
		t.Fatalf("create service %s: %v", name, err)
	}

	inspected, err := api.ServiceInspect(ctx, created.ID, client.ServiceInspectOptions{})
	if err != nil {
		t.Fatalf("inspect service %s: %v", name, err)
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

func diffArgs(cfg cluster.Config, app, target, root string) []string {
	args := []string{
		"--app", app + "/" + target,
		"--root", root,
		"--output", "json",
		"--endpoint", cfg.Endpoint,
	}
	if cfg.CAFile != "" {
		args = append(args, "--ca", cfg.CAFile)
	}
	if cfg.CertFile != "" {
		args = append(args, "--cert", cfg.CertFile)
	}
	if cfg.KeyFile != "" {
		args = append(args, "--key", cfg.KeyFile)
	}
	return args
}

func diffPlan(t *testing.T, args []string) (reconcile.Plan, error) {
	t.Helper()

	var out bytes.Buffer
	err := runDiffTo(args, &out)
	if out.Len() == 0 {
		return reconcile.Plan{}, err
	}

	var plan reconcile.Plan
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

func requireAction(t *testing.T, plan reconcile.Plan, key string, action reconcile.Action) reconcile.Entry {
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
	return reconcile.Entry{}
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
	versions := make(map[string]uint64)
	for _, service := range services.Items {
		if service.Spec.Labels[desired.LabelApplication] != scope.Application ||
			service.Spec.Labels[desired.LabelTarget] != scope.Target {
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
		Name:     "integration",
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
