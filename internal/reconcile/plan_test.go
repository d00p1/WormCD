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
	desiredState := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services:    []desired.Service{testService("wormcd_web_prod_web")},
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

	observedState := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services:    []desired.Service{testService("wormcd_web_prod_web")},
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

	plan := Diff(desiredState, observedState)
	if plan.HasChanges() {
		t.Fatalf("plan has changes: %+v", plan.Entries)
	}
	if plan.Summary != (runtime.Summary{Noop: 4}) {
		t.Fatalf("summary = %+v, want 4 noop", plan.Summary)
	}

	keys := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		keys = append(keys, entry.Key)
		if entry.Action != runtime.Noop {
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

	desiredWeb := testService("wormcd_web_prod_web")
	desiredAPI := testService("wormcd_web_prod_api")

	observedWeb := desiredWeb
	observedWeb.Image = "ghcr.io/d00p1/web:v2"
	observedOld := testService("wormcd_web_prod_old")

	desiredState := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services:    []desired.Service{desiredWeb, desiredAPI},
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
	observedState := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Services:    []desired.Service{observedWeb, observedOld},
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

	plan := Diff(desiredState, observedState)
	if !plan.HasChanges() {
		t.Fatal("plan has no changes")
	}
	if plan.Summary != (runtime.Summary{Create: 2, Update: 1, Delete: 2, Noop: 1}) {
		t.Fatalf("summary = %+v, want 2 create, 1 update, 2 delete, 1 noop", plan.Summary)
	}

	got := make(map[string]runtime.Entry, len(plan.Entries))
	for _, entry := range plan.Entries {
		got[entry.Key] = entry
	}

	web := got["Service/wormcd_web_prod_web"]
	if web.Action != runtime.Update || !slices.Equal(web.Changes, []string{"image"}) {
		t.Errorf("web = %+v, want Update [image]", web)
	}
	if entry := got["Service/wormcd_web_prod_api"]; entry.Action != runtime.Create {
		t.Errorf("api = %+v, want Create", entry)
	}
	if entry := got["Service/wormcd_web_prod_old"]; entry.Action != runtime.Delete {
		t.Errorf("old = %+v, want Delete", entry)
	}
	if entry := got["Network/wormcd_web_prod_default"]; entry.Action != runtime.Noop {
		t.Errorf("network = %+v, want Noop", entry)
	}
	if entry := got["Config/wormcd_web_prod_nginx_new"]; entry.Action != runtime.Create {
		t.Errorf("new config = %+v, want Create", entry)
	}
	if entry := got["Config/wormcd_web_prod_nginx_old"]; entry.Action != runtime.Delete {
		t.Errorf("old config = %+v, want Delete", entry)
	}
}

func TestDiffStableOrder(t *testing.T) {
	labels := desired.Labels("web", "prod")
	desiredState := &desired.Desired{
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

	plan := Diff(desiredState, runtime.Observed{})
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
	desiredState := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Networks:    []desired.Network{{Name: "shared", External: true}},
		Volumes:     []desired.Volume{{Name: "shared", External: true}},
		Configs:     []desired.Config{{Name: "shared", External: true}},
		Secrets:     []desired.Secret{{Name: "shared", External: true}},
	}

	plan := Diff(desiredState, runtime.Observed{})
	if len(plan.Entries) != 0 {
		t.Fatalf("entries = %+v, want none for external objects", plan.Entries)
	}
	if plan.HasChanges() {
		t.Fatal("external objects must not produce a change")
	}
}

func TestDiffSkipsExternalObservedObjects(t *testing.T) {
	desiredState := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Networks:    []desired.Network{{Name: "shared", External: true}},
	}
	observedState := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Networks:    []desired.Network{{Name: "shared", External: true}},
	}

	plan := Diff(desiredState, observedState)
	if len(plan.Entries) != 0 {
		t.Fatalf("entries = %+v, want external observed objects to be ignored", plan.Entries)
	}
}

func TestDiffServiceMapChanges(t *testing.T) {
	desiredService := testService("wormcd_web_prod_web")
	desiredService.Env = map[string]string{"A": "1", "B": "2"}

	observedService := desiredService
	observedService.Env = map[string]string{"A": "1", "B": "3", "C": "4"}
	observedService.Labels = map[string]string{
		desired.LabelApplication: "web",
		desired.LabelTarget:      "prod",
		"team":                   "platform",
	}

	changes := serviceChanges(desiredService, observedService)
	wantChanges := []string{"env.B", "env.C", "labels.team"}
	if !slices.Equal(changes, wantChanges) {
		t.Fatalf("changes = %v, want %v", changes, wantChanges)
	}
}

func TestDiffServiceDefaults(t *testing.T) {
	desiredService := desired.Service{Name: "s"}
	observedService := desired.Service{
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
	if changes := serviceChanges(desiredService, observedService); len(changes) != 0 {
		t.Fatalf("changes = %v, want none", changes)
	}
}

func TestDiffNetworkOptions(t *testing.T) {
	desiredNetwork := desired.Network{
		Name:       "n",
		Driver:     "overlay",
		DriverOpts: map[string]string{"com.docker.network.driver.mtu": "1450"},
	}
	observedNetwork := desiredNetwork
	observedNetwork.DriverOpts = map[string]string{}
	if changes := networkChanges(desiredNetwork, observedNetwork); !slices.Equal(changes, []string{"driverOpts"}) {
		t.Fatalf("changes = %v, want [driverOpts]", changes)
	}

	observedNetwork.DriverOpts = map[string]string{"com.docker.network.driver.mtu": "1400"}
	if changes := networkChanges(desiredNetwork, observedNetwork); !slices.Equal(changes, []string{"driverOpts"}) {
		t.Fatalf("changes = %v, want [driverOpts]", changes)
	}

	observedNetwork.DriverOpts = map[string]string{"com.docker.network.driver.mtu": "1450"}
	if changes := networkChanges(desiredNetwork, observedNetwork); len(changes) != 0 {
		t.Fatalf("changes = %v, want none", changes)
	}
}

func TestPlanJSON(t *testing.T) {
	plan := Diff(
		&desired.Desired{Application: "web", Target: "prod", Namespace: "wormcd_web_prod"},
		runtime.Observed{},
	)

	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"application":"web","target":"prod","namespace":"wormcd_web_prod","summary":{"create":0,"update":0,"delete":0,"noop":0},"entries":[]}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}

func testService(name string) desired.Service {
	return desired.Service{
		Name:         name,
		Image:        "ghcr.io/d00p1/web:v1",
		Mode:         desired.ModeReplicated,
		Replicas:     1,
		EndpointMode: "vip",
		Labels:       desired.Labels("web", "prod"),
		Networks:     []desired.ServiceNetwork{{Name: "wormcd_web_prod_default"}},
		// The renderer returns allocated empty slices; the runtime reader
		// returns nil. This must not look like drift.
		Ports:    []desired.Port{},
		Mounts:   []desired.Mount{},
		Configs:  []desired.ConfigRef{},
		Secrets:  []desired.SecretRef{},
		Restart:  desired.DefaultRestartPolicy(),
		Update:   desired.DefaultUpdateConfig(),
		Rollback: desired.DefaultUpdateConfig(),
	}
}

func TestDiffUpdatesConfigLabelsInPlace(t *testing.T) {
	desiredState := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Configs: []desired.Config{{
			Name:   "wormcd_web_prod_nginx_abc",
			Labels: map[string]string{desired.LabelApplication: "web", desired.LabelTarget: "prod", "team": "b"},
		}},
	}
	observedState := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Configs: []desired.Config{{
			Name:   "wormcd_web_prod_nginx_abc",
			Labels: map[string]string{desired.LabelApplication: "web", desired.LabelTarget: "prod", "team": "a"},
		}},
	}

	plan := Diff(desiredState, observedState)
	if plan.Summary != (runtime.Summary{Update: 1}) {
		t.Fatalf("summary = %+v, want one Update and no Create or Delete", plan.Summary)
	}
	entry := plan.Entries[0]
	if entry.Action != runtime.Update || !slices.Equal(entry.Changes, []string{"labels.team"}) {
		t.Fatalf("config = %+v, want Update [labels.team]", entry)
	}
}

func TestDiffNeverUpdatesSecrets(t *testing.T) {
	desiredState := &desired.Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Secrets: []desired.Secret{{
			Name:   "wormcd_web_prod_tls",
			Labels: desired.Labels("web", "prod"),
		}},
	}
	observedState := runtime.Observed{
		Application: "web",
		Target:      "prod",
		Namespace:   "wormcd_web_prod",
		Secrets: []desired.Secret{{
			Name:   "wormcd_web_prod_tls",
			Labels: map[string]string{desired.LabelApplication: "web", desired.LabelTarget: "prod", "team": "a"},
		}},
	}

	plan := Diff(desiredState, observedState)
	if plan.Summary.Update != 0 {
		t.Fatalf("summary = %+v, want no Update: secrets have no in-place update in v0", plan.Summary)
	}
}
