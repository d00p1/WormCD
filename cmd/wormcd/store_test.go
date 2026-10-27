package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/store"
)

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
					{Target: 8080, Published: 80, Protocol: "tcp", Mode: "ingress"},
					{Target: 53, Published: 53, Protocol: "udp", Mode: "host"},
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

	original.Sort()
	spec, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	number, err := st.AppendRevision(ctx, "web", "prod", store.NewRevision{Commit: "abc123", RenderedSpec: spec})
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
