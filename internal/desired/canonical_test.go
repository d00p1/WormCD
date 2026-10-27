package desired

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// shuffled returns the same state with every sortable collection in reverse
// order, including the nested ones of the first service.
func shuffled() Desired {
	services := []Service{
		{
			Name:     "web",
			Image:    "web:1",
			Mode:     ModeReplicated,
			Replicas: 2,
			Env:      map[string]string{"ZETA": "z", "ALPHA": "a", "MID": "m"},
			Networks: []ServiceNetwork{{Name: "front"}, {Name: "back"}},
			Ports: []Port{
				{Target: 80, Published: 80, Protocol: "tcp", Mode: "ingress"},
				{Target: 53, Published: 53, Protocol: "udp", Mode: "host"},
			},
			Mounts: []Mount{
				{Type: MountVolume, Source: "data", Target: "/data"},
				{Type: MountBind, Source: "/etc/x", Target: "/etc/x"},
			},
			Configs: []ConfigRef{
				{Source: "b", Target: "/b", Mode: 0o440},
				{Source: "a", Target: "/a", Mode: 0o440},
			},
			Secrets: []SecretRef{
				{Source: "pw", Target: "/run/pw", Mode: 0o400},
				{Source: "key", Target: "/run/key", Mode: 0o400},
			},
		},
		{Name: "api", Image: "api:1", Mode: ModeGlobal},
	}
	networks := []Network{{Name: "front", Driver: "overlay"}, {Name: "back", Driver: "overlay"}}
	configs := []Config{{Name: "b-cfg", Source: "b"}, {Name: "a-cfg", Source: "a"}}
	secrets := []Secret{{Name: "pw", Source: "pw"}, {Name: "key", Source: "key"}}
	volumes := []Volume{{Name: "data", Driver: "local"}, {Name: "cache", Driver: "local"}}

	slices.Reverse(services)
	slices.Reverse(networks)
	slices.Reverse(configs)
	slices.Reverse(secrets)
	slices.Reverse(volumes)
	slices.Reverse(services[1].Networks)
	slices.Reverse(services[1].Ports)

	return Desired{
		Application: "web",
		Target:      "prod",
		Namespace:   TargetNamespace("web", "prod"),
		Services:    services,
		Networks:    networks,
		Configs:     configs,
		Secrets:     secrets,
		Volumes:     volumes,
	}
}

func TestCanonicalDoesNotMutateInput(t *testing.T) {
	d := shuffled()
	want := shuffled()

	if _, err := Canonical(&d); err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("Canonical changed its input\n got: %+v\nwant: %+v", d, want)
	}
}

func TestCanonicalIgnoresInputOrder(t *testing.T) {
	d := shuffled()
	reversed, err := Canonical(&d)
	if err != nil {
		t.Fatalf("Canonical(shuffled): %v", err)
	}

	ordered := shuffled()
	ordered.Sort()
	want, err := Canonical(&ordered)
	if err != nil {
		t.Fatalf("Canonical(sorted): %v", err)
	}
	if !bytes.Equal(reversed, want) {
		t.Errorf("shuffled input differs from sorted input:\n%s\n%s", reversed, want)
	}
}

func TestCanonicalIsRepeatable(t *testing.T) {
	d := shuffled()
	first, err := Canonical(&d)
	if err != nil {
		t.Fatalf("first Canonical: %v", err)
	}
	second, err := Canonical(&d)
	if err != nil {
		t.Fatalf("second Canonical: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("repeated Canonical differs:\n%s\n%s", first, second)
	}
}

func TestCanonicalSortsMapKeys(t *testing.T) {
	d := shuffled()
	raw, err := Canonical(&d)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if !strings.Contains(string(raw), `"env":{"ALPHA":"a","MID":"m","ZETA":"z"}`) {
		t.Errorf("map keys are not sorted in %s", raw)
	}
}

func TestCanonicalEmptyDesired(t *testing.T) {
	var d Desired
	if _, err := Canonical(&d); err != nil {
		t.Fatalf("Canonical(zero): %v", err)
	}
}
