package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/d00p1/wormcd/internal/cluster"
	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/manifest"
	"github.com/d00p1/wormcd/internal/runtime"
)

func TestRunDiff(t *testing.T) {
	desiredState, clusterName, err := manifest.LoadWithCluster(context.Background(), manifest.Options{
		App:  "web/prod",
		Root: "testdata/stand",
	})
	if err != nil {
		t.Fatalf("load stand: %v", err)
	}
	if clusterName != "local" {
		t.Fatalf("cluster = %q, want local", clusterName)
	}

	t.Run("in sync", func(t *testing.T) {
		inspector := &fakeDiffInspector{observed: observedFromDesired(desiredState)}
		var out bytes.Buffer
		err := runDiffWith(
			[]string{"--app", "web/prod", "--root", "testdata/stand", "--output", "json"},
			&out,
			fakeConnector(t, "local", inspector),
		)
		if err != nil {
			t.Fatalf("runDiffWith: %v", err)
		}
		if inspector.calls != 1 {
			t.Fatalf("Inspect calls = %d, want 1", inspector.calls)
		}

		plan := decodePlan(t, out.Bytes())
		if plan.HasChanges() {
			t.Fatalf("plan has changes: %+v", plan.Entries)
		}
	})

	t.Run("drift", func(t *testing.T) {
		observed := observedFromDesired(desiredState)
		observed.Services[0].Image = "ghcr.io/d00p1/wormcd-demo:manual"
		inspector := &fakeDiffInspector{observed: observed}
		var out bytes.Buffer
		err := runDiffWith(
			[]string{"--app", "web/prod", "--root", "testdata/stand", "--output", "json"},
			&out,
			fakeConnector(t, "local", inspector),
		)

		var exit *exitError
		if !errors.As(err, &exit) || exit.code != 2 {
			t.Fatalf("error = %v, want exit code 2", err)
		}
		if inspector.calls != 1 {
			t.Fatalf("Inspect calls = %d, want 1", inspector.calls)
		}

		plan := decodePlan(t, out.Bytes())
		if !plan.HasChanges() {
			t.Fatal("plan has no changes")
		}
		entry := findPlanEntry(t, plan, "Service/"+desiredState.Services[0].Name)
		if entry.Action != runtime.Update {
			t.Fatalf("action = %s, want Update", entry.Action)
		}
	})
}

type fakeDiffInspector struct {
	observed runtime.Observed
	calls    int
}

func (f *fakeDiffInspector) Inspect(context.Context, runtime.Scope) (runtime.Observed, error) {
	f.calls++
	return f.observed, nil
}

func fakeConnector(
	t *testing.T,
	wantCluster string,
	inspector diffInspector,
) func(cluster.Config) (diffInspector, func(), error) {
	t.Helper()

	return func(cfg cluster.Config) (diffInspector, func(), error) {
		if cfg.Name != wantCluster {
			t.Errorf("cluster name = %q, want %q", cfg.Name, wantCluster)
		}
		return inspector, func() {}, nil
	}
}

func observedFromDesired(desiredState *desired.Desired) runtime.Observed {
	observed := runtime.Observed{
		Application: desiredState.Application,
		Target:      desiredState.Target,
		Namespace:   desiredState.Namespace,
	}
	for _, network := range desiredState.Networks {
		if network.Managed() {
			observed.Networks = append(observed.Networks, network)
		}
	}
	for _, config := range desiredState.Configs {
		if config.Managed() {
			observed.Configs = append(observed.Configs, config)
		}
	}
	for _, secret := range desiredState.Secrets {
		if secret.Managed() {
			observed.Secrets = append(observed.Secrets, secret)
		}
	}
	for _, volume := range desiredState.Volumes {
		if volume.Managed() {
			observed.Volumes = append(observed.Volumes, volume)
		}
	}
	observed.Services = append(observed.Services, desiredState.Services...)
	return observed
}

func decodePlan(t *testing.T, data []byte) runtime.Plan {
	t.Helper()

	var plan runtime.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, data)
	}
	return plan
}

func findPlanEntry(t *testing.T, plan runtime.Plan, key string) runtime.Entry {
	t.Helper()

	for _, entry := range plan.Entries {
		if entry.Key == key {
			return entry
		}
	}
	t.Fatalf("%s: not found in plan %+v", key, plan.Entries)
	return runtime.Entry{}
}
