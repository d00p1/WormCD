package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/store"
)

func TestRenderedSpecIsCanonicalAfterSort(t *testing.T) {
	build := func(reverse bool) desired.Desired {
		services := []desired.Service{
			{
				Name:     "web",
				Image:    "web:1",
				Mode:     desired.ModeReplicated,
				Replicas: 2,
				Networks: []desired.ServiceNetwork{{Name: "front"}, {Name: "back"}},
				Ports: []desired.Port{
					{Target: 80, Published: 80, Protocol: "tcp", Mode: "ingress"},
					{Target: 53, Published: 53, Protocol: "udp", Mode: "host"},
				},
			},
			{Name: "api", Image: "api:1", Mode: desired.ModeGlobal},
		}
		networks := []desired.Network{{Name: "front", Driver: "overlay"}, {Name: "back", Driver: "overlay"}}
		volumes := []desired.Volume{{Name: "data", Driver: "local"}, {Name: "cache", Driver: "local"}}
		if reverse {
			slices.Reverse(services)
			slices.Reverse(networks)
			slices.Reverse(volumes)
			slices.Reverse(services[1].Networks)
			slices.Reverse(services[1].Ports)
		}
		return desired.Desired{
			Application: "web",
			Target:      "prod",
			Namespace:   desired.TargetNamespace("web", "prod"),
			Services:    services,
			Networks:    networks,
			Volumes:     volumes,
		}
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "wormcd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}

	forward := build(false)
	reversed := build(true)
	forwardJSON, err := forward.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	reversedJSON, err := reversed.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !bytes.Equal(forwardJSON, reversedJSON) {
		t.Fatalf("canonical specs differ:\n%s\n%s", forwardJSON, reversedJSON)
	}

	number, err := st.AppendRevision(ctx, "web", "prod", store.NewRevision{Commit: "abc123", RenderedSpec: &reversed})
	if err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}
	rev, err := st.Revision(ctx, "web", "prod", number)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if !bytes.Equal(rev.RenderedSpec, forwardJSON) {
		t.Errorf("stored spec differs from canonical render:\n got: %s\nwant: %s", rev.RenderedSpec, forwardJSON)
	}
}

func TestRenderedRevisionRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "wormcd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.EnsureTarget(ctx, "web", "prod", ""); err != nil {
		t.Fatalf("EnsureTarget: %v", err)
	}

	command := []string{"serve", "--port", "8080"}
	original := desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   desired.TargetNamespace("web", "prod"),
		Services: []desired.Service{
			{
				Name:         "web",
				Image:        "registry.example/web@sha256:abc",
				Mode:         desired.ModeReplicated,
				Replicas:     3,
				EndpointMode: "vip",
				Command:      &command,
				Env:          map[string]string{"LOG": "info"},
				Ports: []desired.Port{
					{Target: 53, Published: 53, Protocol: "udp", Mode: "host"},
					{Target: 8080, Published: 80, Protocol: "tcp", Mode: "ingress"},
				},
				Networks: []desired.ServiceNetwork{{Name: "front", Aliases: []string{"web"}}},
				Mounts:   []desired.Mount{{Type: desired.MountVolume, Source: "data", Target: "/data"}},
				Configs:  []desired.ConfigRef{{Source: "app-config", Target: "/etc/app.yaml", Mode: 0o440}},
				Resources: desired.Resources{
					Limits: desired.ResourceLimits{NanoCPUs: 500_000_000, MemoryBytes: 256 << 20},
				},
				Restart: desired.RestartPolicy{
					Condition:   "on-failure",
					Delay:       5 * time.Second,
					MaxAttempts: 3,
					Window:      time.Minute,
				},
				Healthcheck: &desired.Healthcheck{
					Test:     []string{"CMD", "true"},
					Interval: 10 * time.Second,
					Retries:  2,
				},
			},
		},
		Networks: []desired.Network{{Name: "front", Driver: "overlay", Attachable: true}},
		Configs: []desired.Config{
			{Name: "app-config", Source: "configs/config.yaml", Hash: "h1", Data: []byte("key: value\n")},
		},
		Secrets: []desired.Secret{{Name: "db-password", Source: "db-password", External: true}},
		Volumes: []desired.Volume{{Name: "data", Driver: "local"}},
	}

	number, err := st.AppendRevision(ctx, "web", "prod", store.NewRevision{Commit: "abc123", RenderedSpec: &original})
	if err != nil {
		t.Fatalf("AppendRevision: %v", err)
	}

	rev, err := st.Revision(ctx, "web", "prod", number)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	var got desired.Desired
	if err := json.Unmarshal(rev.RenderedSpec, &got); err != nil {
		t.Fatalf("unmarshal rendered spec: %v", err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("rendered spec mismatch\n got: %+v\nwant: %+v", got, original)
	}
}
