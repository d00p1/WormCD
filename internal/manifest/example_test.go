package manifest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleDeployRepo keeps examples/deploy-repo renderable. The example is
// documentation agents copy from, so a broken example must fail the build.
func TestExampleDeployRepo(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "deploy-repo")

	cases := []struct {
		target    string
		namespace string
		replicas  uint64
		secret    string
		image     string
	}{
		{target: "staging", namespace: "wormcd_web_staging", replicas: 2, secret: "staging_tls_key", image: ":v1"},
		{target: "prod", namespace: "wormcd_web_prod", replicas: 3, secret: "prod_tls_key", image: "@sha256:"},
	}

	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			state, err := Load(context.Background(), Options{App: "web/" + tc.target, Root: root})
			if err != nil {
				t.Fatalf("load example: %v", err)
			}

			if state.Application != "web" || state.Target != tc.target || state.Namespace != tc.namespace {
				t.Fatalf("state = %s/%s (%s), want web/%s (%s)",
					state.Application, state.Target, state.Namespace, tc.target, tc.namespace)
			}

			serviceName := tc.namespace + "_web"
			if len(state.Services) != 1 || state.Services[0].Name != serviceName {
				t.Fatalf("services = %+v, want one %q", state.Services, serviceName)
			}
			service := state.Services[0]
			if service.Replicas != tc.replicas {
				t.Errorf("replicas = %d, want %d", service.Replicas, tc.replicas)
			}
			if !strings.Contains(service.Image, tc.image) {
				t.Errorf("image = %q, want it to contain %q", service.Image, tc.image)
			}

			if len(state.Secrets) != 1 || state.Secrets[0].Name != tc.secret {
				t.Errorf("secrets = %+v, want one %q", state.Secrets, tc.secret)
			}
			if len(state.Configs) != 1 || !strings.HasPrefix(state.Configs[0].Name, tc.namespace+"_app_config_") {
				t.Errorf("configs = %+v, want one content-addressed app_config", state.Configs)
			}
		})
	}
}
