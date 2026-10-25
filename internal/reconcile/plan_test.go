package reconcile

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/d00p1/wormcd/internal/runtime"
)

func TestDiffInSync(t *testing.T) {
	labels := desired.Labels("web", "prod")
	want := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services: []desired.Service{{
			Name:         "wormcd_web_prod_web",
			Image:        "ghcr.io/d00p1/web:v1",
			Mode:         desired.ModeReplicated,
			Replicas:     1,
			EndpointMode: "vip",
			Labels:       labels,
			Networks:     []desired.ServiceNetwork{{Name: "wormcd_web_prod_default"}},
			// The renderer returns allocated empty slices; the runtime
			// reader returns nil. This must not look like drift.
			Ports:    []desired.Port{},
			Mounts:   []desired.Mount{},
			Configs:  []desired.ConfigRef{},
			Secrets:  []desired.SecretRef{},
			Restart:  desired.DefaultRestartPolicy(),
			Update:   desired.DefaultUpdateConfig(),
			Rollback: desired.DefaultUpdateConfig(),
		}},
		Networks: []desired.Network{{
			Name:   "wormcd_web_prod_default",
			Driver: "overlay",
			Labels: labels,
		}},
		Configs: []desired.Config{{
			Name:   "wormcd_web_prod_nginx_abc",
			Source: "nginx",
			Hash:   "abc",
			Data:   []byte("server {}"),
			Labels: labels,
		}},
		Volumes: []desired.Volume{{
			Name:   "wormcd_web_prod_data",
			Driver: "local",
			Labels: labels,
		}},
		// External objects are referenced, not managed.
		Secrets: []desired.Secret{{Name: "prod_tls", Source: "tls", External: true}},
	}

	have := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services: []desired.Service{{
			Name:         "wormcd_web_prod_web",
			Image:        "ghcr.io/d00p1/web:v1",
			Mode:         desired.ModeReplicated,
			Replicas:     1,
			EndpointMode: "vip",
			Labels:       labels,
			Networks:     []desired.ServiceNetwork{{Name: "wormcd_web_prod_default"}},
			Restart:      desired.DefaultRestartPolicy(),
			Update:       desired.DefaultUpdateConfig(),
			Rollback:     desired.DefaultUpdateConfig(),
		}},
		Networks: []desired.Network{{
			Name:   "wormcd_web_prod_default",
			Driver: "overlay",
			Labels: labels,
		}},
		Configs: []desired.Config{{
			Name:   "wormcd_web_prod_nginx_abc",
			Labels: labels,
		}},
		Volumes: []desired.Volume{{
			Name:   "wormcd_web_prod_data",
			Driver: "local",
			Labels: labels,
		}},
	}

	plan := Diff(want, have)
	if plan.HasChanges() {
		t.Fatalf("plan has changes: %+v", plan.Entries)
	}
	if plan.Summary != (Summary{Noop: 4}) {
		t.Fatalf("summary = %+v, want 4 noop", plan.Summary)
	}

	keys := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		keys = append(keys, entry.Key)
		if entry.Action != Noop {
			t.Errorf("%s: action = %s, want Noop (%v)", entry.Key, entry.Action, entry.Changes)
		}
	}
	wantKeys := []string{
		"Network/wormcd_web_prod_default",
		"Config/wormcd_web_prod_nginx_abc",
		"Volume/wormcd_web_prod_data",
		"Service/wormcd_web_prod_web",
	}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
	if plan.Application != "web" || plan.Target != "prod" || plan.Namespace != "wormcd_web_prod" {
		t.Errorf("scope = %s/%s/%s", plan.Application, plan.Target, plan.Namespace)
	}
}

func TestDiffActions(t *testing.T) {
	labels := desired.Labels("web", "prod")
	base := desired.Service{
		Image:        "ghcr.io/d00p1/web:v1",
		Mode:         desired.ModeReplicated,
		Replicas:     1,
		EndpointMode: "vip",
		Labels:       labels,
		Networks:     []desired.ServiceNetwork{{Name: "wormcd_web_prod_default"}},
		Restart:      desired.DefaultRestartPolicy(),
		Update:       desired.DefaultUpdateConfig(),
		Rollback:     desired.DefaultUpdateConfig(),
	}

	wantWeb := base
	wantWeb.Name = "wormcd_web_prod_web"

	wantAPI := base
	wantAPI.Name = "wormcd_web_prod_api"

	haveWeb := wantWeb
	haveWeb.Image = "ghcr.io/d00p1/web:v2"

	haveOld := base
	haveOld.Name = "wormcd_web_prod_old"

	want := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services:    []desired.Service{wantWeb, wantAPI},
		Networks: []desired.Network{{
			Name:   "wormcd_web_prod_default",
			Driver: "overlay",
			Labels: labels,
		}},
		// A changed immutable config is a new object with a new name.
		Configs: []desired.Config{{
			Name:   "wormcd_web_prod_nginx_new",
			Source: "nginx",
			Labels: labels,
		}},
	}
	have := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services:    []desired.Service{haveWeb, haveOld},
		Networks: []desired.Network{{
			Name:   "wormcd_web_prod_default",
			Driver: "overlay",
			Labels: labels,
		}},
		Configs: []desired.Config{{
			Name:   "wormcd_web_prod_nginx_old",
			Labels: labels,
		}},
	}

	plan := Diff(want, have)
	if !plan.HasChanges() {
		t.Fatal("plan has no changes")
	}
	if plan.Summary != (Summary{Create: 2, Update: 1, Delete: 2, Noop: 1}) {
		t.Fatalf("summary = %+v, want 2 create, 1 update, 2 delete, 1 noop", plan.Summary)
	}

	got := make(map[string]Entry, len(plan.Entries))
	for _, entry := range plan.Entries {
		got[entry.Key] = entry
	}

	web := got["Service/wormcd_web_prod_web"]
	if web.Action != Update || !slices.Equal(web.Changes, []string{"image"}) {
		t.Errorf("web = %+v, want Update [image]", web)
	}
	if entry := got["Service/wormcd_web_prod_api"]; entry.Action != Create {
		t.Errorf("api = %+v, want Create", entry)
	}
	if entry := got["Service/wormcd_web_prod_old"]; entry.Action != Delete {
		t.Errorf("old = %+v, want Delete", entry)
	}
	if entry := got["Network/wormcd_web_prod_default"]; entry.Action != Noop {
		t.Errorf("network = %+v, want Noop", entry)
	}
	if entry := got["Config/wormcd_web_prod_nginx_new"]; entry.Action != Create {
		t.Errorf("new config = %+v, want Create", entry)
	}
	if entry := got["Config/wormcd_web_prod_nginx_old"]; entry.Action != Delete {
		t.Errorf("old config = %+v, want Delete", entry)
	}
}

func TestDiffStableOrder(t *testing.T) {
	labels := desired.Labels("web", "prod")
	want := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services: []desired.Service{
			{Name: "wormcd_web_prod_zeta", Image: "img:v1"},
			{Name: "wormcd_web_prod_alpha", Image: "img:v1"},
		},
		Networks: []desired.Network{
			{Name: "wormcd_web_prod_z", Driver: "overlay", Labels: labels},
			{Name: "wormcd_web_prod_a", Driver: "overlay", Labels: labels},
		},
		Volumes: []desired.Volume{
			{Name: "wormcd_web_prod_z", Driver: "local", Labels: labels},
			{Name: "wormcd_web_prod_a", Driver: "local", Labels: labels},
		},
	}

	plan := Diff(want, runtime.Observed{})
	keys := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		keys = append(keys, entry.Key)
	}
	wantKeys := []string{
		"Network/wormcd_web_prod_a",
		"Network/wormcd_web_prod_z",
		"Volume/wormcd_web_prod_a",
		"Volume/wormcd_web_prod_z",
		"Service/wormcd_web_prod_alpha",
		"Service/wormcd_web_prod_zeta",
	}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
}

func TestDiffSkipsExternalObjects(t *testing.T) {
	want := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Networks:    []desired.Network{{Name: "shared", External: true}},
		Volumes:     []desired.Volume{{Name: "shared", External: true}},
		Configs:     []desired.Config{{Name: "shared", External: true}},
		Secrets:     []desired.Secret{{Name: "shared", External: true}},
	}

	plan := Diff(want, runtime.Observed{})
	if len(plan.Entries) != 0 {
		t.Fatalf("entries = %+v, want none for external objects", plan.Entries)
	}
	if plan.HasChanges() {
		t.Fatal("external objects must not produce a change")
	}
}

func TestDiffServiceMapChanges(t *testing.T) {
	labels := desired.Labels("web", "prod")
	want := desired.Service{
		Name:   "wormcd_web_prod_web",
		Image:  "img:v1",
		Env:    map[string]string{"A": "1", "B": "2"},
		Labels: labels,
	}
	have := want
	have.Env = map[string]string{"A": "1", "B": "3", "C": "4"}
	have.Labels = map[string]string{
		desired.LabelApplication: "web",
		desired.LabelTarget:      "prod",
		"team":                   "platform",
	}

	changes := serviceChanges(want, have)
	wantChanges := []string{"env.B", "env.C", "labels.team"}
	if !slices.Equal(changes, wantChanges) {
		t.Fatalf("changes = %v, want %v", changes, wantChanges)
	}
}

func TestDiffServiceDefaults(t *testing.T) {
	want := desired.Service{
		Name: "s",
		// Nil collections from the renderer.
	}
	have := desired.Service{
		Name:       "s",
		Ports:      []desired.Port{},
		Networks:   []desired.ServiceNetwork{},
		Mounts:     []desired.Mount{},
		Configs:    []desired.ConfigRef{},
		Secrets:    []desired.SecretRef{},
		Env:        map[string]string{},
		Labels:     map[string]string{},
		Command:    &[]string{},
		Entrypoint: &[]string{},
	}
	if changes := serviceChanges(want, have); len(changes) != 0 {
		t.Fatalf("changes = %v, want none", changes)
	}
}

func TestDiffNetworkOptions(t *testing.T) {
	want := desired.Network{
		Name:       "n",
		Driver:     "overlay",
		DriverOpts: map[string]string{"com.docker.network.driver.mtu": "1450"},
	}
	have := want
	have.DriverOpts = map[string]string{}
	if changes := networkChanges(want, have); !slices.Equal(changes, []string{"driverOpts"}) {
		t.Fatalf("changes = %v, want [driverOpts]", changes)
	}

	have.DriverOpts = map[string]string{"com.docker.network.driver.mtu": "1400"}
	if changes := networkChanges(want, have); !slices.Equal(changes, []string{"driverOpts"}) {
		t.Fatalf("changes = %v, want [driverOpts]", changes)
	}

	have.DriverOpts = map[string]string{"com.docker.network.driver.mtu": "1450"}
	if changes := networkChanges(want, have); len(changes) != 0 {
		t.Fatalf("changes = %v, want none", changes)
	}
}

func TestPlanJSON(t *testing.T) {
	plan := Diff(&desired.Desired{Application: "web", Target: "prod", Namespace: "wormcd_web_prod"}, runtime.Observed{})

	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"application":"web","target":"prod","namespace":"wormcd_web_prod","summary":{"create":0,"update":0,"delete":0,"noop":0},"entries":[]}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}
