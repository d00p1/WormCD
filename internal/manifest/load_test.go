package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
)

var updateGolden = flag.Bool("update", false, "update golden files")

const appWithOverlay = `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
      overlay: envs/prod.compose.yaml
`

func TestGoldenBasic(t *testing.T) {
	state := load(t, filepath.Join("testdata", "basic", "wormcd.yaml"), "prod")

	got, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "basic", "desired.golden.json")
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run go test -run TestGoldenBasic -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("rendered state does not match %s\n--- got ---\n%s", path, got)
	}
}

func TestLoadBasic(t *testing.T) {
	state := load(t, filepath.Join("testdata", "basic", "wormcd.yaml"), "prod")

	if state.Application != "web" || state.Target != "prod" {
		t.Fatalf("application/target = %s/%s, want web/prod", state.Application, state.Target)
	}
	if state.Namespace != "wormcd_web_prod" {
		t.Fatalf("namespace = %q, want wormcd_web_prod", state.Namespace)
	}

	if len(state.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(state.Services))
	}
	if got := state.Services[0].Name; got != "wormcd_web_prod_web" {
		t.Fatalf("first service = %q, want wormcd_web_prod_web", got)
	}
	if got := state.Services[1].Name; got != "wormcd_web_prod_worker" {
		t.Fatalf("second service = %q, want wormcd_web_prod_worker", got)
	}

	web := serviceByName(t, state, "wormcd_web_prod_web")
	if got, want := web.Image, "ghcr.io/d00p1/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"; got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
	if web.Replicas != 3 {
		t.Errorf("replicas = %d, want 3", web.Replicas)
	}
	if web.EndpointMode != "dnsrr" {
		t.Errorf("endpoint mode = %q, want dnsrr", web.EndpointMode)
	}
	if got := web.Env["APP_ENV"]; got != "prod" {
		t.Errorf("APP_ENV = %q, want prod", got)
	}
	if got := web.Env["LOG_LEVEL"]; got != "info" {
		t.Errorf("LOG_LEVEL = %q, want info (overlay must merge)", got)
	}
	if web.Command == nil || strings.Join(*web.Command, " ") != "/app/server --verbose" {
		t.Errorf("command = %v, want [/app/server --verbose]", web.Command)
	}
	if web.Entrypoint == nil || strings.Join(*web.Entrypoint, " ") != "/app/entrypoint.sh" {
		t.Errorf("entrypoint = %v, want [/app/entrypoint.sh]", web.Entrypoint)
	}
	if got := web.Labels[desired.LabelApplication]; got != "web" {
		t.Errorf("%s = %q, want web", desired.LabelApplication, got)
	}
	if got := web.Labels[desired.LabelTarget]; got != "prod" {
		t.Errorf("%s = %q, want prod", desired.LabelTarget, got)
	}

	if len(web.Ports) != 1 || web.Ports[0].Target != 80 || web.Ports[0].Published != 8080 {
		t.Errorf("ports = %+v, want 8080->80", web.Ports)
	}
	if web.Ports[0].Protocol != "tcp" || web.Ports[0].Mode != "ingress" {
		t.Errorf("port protocol/mode = %s/%s, want tcp/ingress", web.Ports[0].Protocol, web.Ports[0].Mode)
	}

	if len(web.Networks) != 1 || web.Networks[0].Name != "wormcd_web_prod_default" {
		t.Errorf("web networks = %+v, want default", web.Networks)
	}
	worker := serviceByName(t, state, "wormcd_web_prod_worker")
	if len(worker.Networks) != 1 || worker.Networks[0].Name != "wormcd_web_prod_backend" {
		t.Errorf("worker networks = %+v, want backend", worker.Networks)
	}

	if len(web.Mounts) != 1 || web.Mounts[0].Source != "wormcd_web_prod_data" || web.Mounts[0].Target != "/var/lib/web" {
		t.Errorf("mounts = %+v, want data:/var/lib/web", web.Mounts)
	}
	if len(web.Configs) != 1 {
		t.Fatalf("configs = %+v, want one", web.Configs)
	}
	if !strings.HasPrefix(web.Configs[0].Source, "wormcd_web_prod_nginx_") {
		t.Errorf("config source = %q, want namespaced hashed name", web.Configs[0].Source)
	}
	if web.Configs[0].Target != "/etc/nginx/nginx.conf" {
		t.Errorf("config target = %q", web.Configs[0].Target)
	}
	if len(web.Secrets) != 1 || web.Secrets[0].Source != "prod_tls_key" || web.Secrets[0].Target != "/run/secrets/tls.key" {
		t.Errorf("secrets = %+v, want prod_tls_key at /run/secrets/tls.key", web.Secrets)
	}

	if got, want := web.Resources.Limits.NanoCPUs, int64(500_000_000); got != want {
		t.Errorf("cpu limit = %d, want %d", got, want)
	}
	if got, want := web.Resources.Limits.MemoryBytes, int64(128*1024*1024); got != want {
		t.Errorf("memory limit = %d, want %d", got, want)
	}
	if got, want := web.Resources.Reservations.NanoCPUs, int64(250_000_000); got != want {
		t.Errorf("cpu reservation = %d, want %d", got, want)
	}

	if web.Restart.Condition != "on-failure" || web.Restart.Delay != 10*time.Second || web.Restart.MaxAttempts != 3 {
		t.Errorf("restart = %+v", web.Restart)
	}
	if len(web.Placement.Constraints) != 1 || web.Placement.Constraints[0] != "node.role == worker" {
		t.Errorf("placement = %+v", web.Placement)
	}
	if web.Healthcheck == nil || web.Healthcheck.Interval != 10*time.Second || web.Healthcheck.Retries != 3 {
		t.Errorf("healthcheck = %+v", web.Healthcheck)
	}

	if len(state.Networks) != 2 {
		t.Errorf("networks = %+v, want default and backend", state.Networks)
	}
	if len(state.Volumes) != 1 || state.Volumes[0].Name != "wormcd_web_prod_data" || state.Volumes[0].Driver != "local" {
		t.Errorf("volumes = %+v", state.Volumes)
	}
	if len(state.Configs) != 1 || len(state.Configs[0].Hash) != 12 {
		t.Errorf("configs = %+v, want one hash-addressed config", state.Configs)
	}
	if len(state.Secrets) != 1 || state.Secrets[0].Name != "prod_tls_key" || !state.Secrets[0].External {
		t.Errorf("secrets = %+v, want external prod_tls_key", state.Secrets)
	}
}

func TestConfigContentChangesName(t *testing.T) {
	compose := `services:
  web:
    image: ghcr.io/d00p1/web:v1
    configs:
      - source: app
configs:
  app:
    file: app.conf
`
	firstPath := writeApp(t, "", map[string]string{
		"compose.yaml": compose,
		"app.conf":     "one",
	})
	secondPath := writeApp(t, "", map[string]string{
		"compose.yaml": compose,
		"app.conf":     "two",
	})

	first := load(t, firstPath, "")
	second := load(t, secondPath, "")

	if first.Configs[0].Name == second.Configs[0].Name {
		t.Fatalf("config name did not change: %q", first.Configs[0].Name)
	}
	want := sha256.Sum256([]byte("one"))
	if got, wantHash := first.Configs[0].Hash, hex.EncodeToString(want[:])[:12]; got != wantHash {
		t.Errorf("hash = %q, want %q", got, wantHash)
	}
}

func TestConfigLabelChangeKeepsName(t *testing.T) {
	compose := func(team string) string {
		return `services:
  web:
    image: ghcr.io/d00p1/web:v1
    configs:
      - source: app
configs:
  app:
    file: app.conf
    labels:
      team: ` + team + `
`
	}
	firstPath := writeApp(t, "", map[string]string{
		"compose.yaml": compose("a"),
		"app.conf":     "one",
	})
	secondPath := writeApp(t, "", map[string]string{
		"compose.yaml": compose("b"),
		"app.conf":     "one",
	})

	first := load(t, firstPath, "")
	second := load(t, secondPath, "")

	if first.Configs[0].Name != second.Configs[0].Name {
		t.Fatalf("config name changed with labels: %q -> %q", first.Configs[0].Name, second.Configs[0].Name)
	}
	if got := second.Configs[0].Labels["team"]; got != "b" {
		t.Errorf("labels[team] = %q, want %q", got, "b")
	}
}

func TestLoadRejectsUnsupportedField(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    build: .
`,
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "build") {
		t.Fatalf("error = %v, want a build rejection", err)
	}
	if !strings.Contains(err.Error(), "services.web") {
		t.Fatalf("error = %v, want the YAML path services.web", err)
	}
}

func TestLoadRejectsUntaggedImage(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web
`,
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "pinned to a tag or digest") {
		t.Fatalf("error = %v, want an image pinning error", err)
	}
}

func TestLoadRequiresExternalSecrets(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
secrets:
  token:
    file: token.txt
`,
		"token.txt": "secret",
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "external Docker secrets only") {
		t.Fatalf("error = %v, want an external-secret rejection", err)
	}
}

func TestLoadRejectsPathTraversal(t *testing.T) {
	path := writeApp(t, `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: ../compose.yaml
  targets:
    - name: prod
      cluster: prod-1
`, nil)
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "relative path inside") {
		t.Fatalf("error = %v, want a path traversal error", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeApp(t, `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
  unexpected: value
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
`, map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
`,
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("error = %v, want an unknown-field error", err)
	}
}

func TestLoadSelectsSingleTarget(t *testing.T) {
	state := load(t, filepath.Join("testdata", "basic", "wormcd.yaml"), "")
	if state.Target != "prod" {
		t.Fatalf("target = %q, want prod", state.Target)
	}
}

func TestLoadByApp(t *testing.T) {
	state, err := Load(context.Background(), Options{
		App:  "web/prod",
		Root: filepath.Join("testdata"),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Application != "web" || state.Target != "prod" {
		t.Fatalf("application/target = %s/%s, want web/prod", state.Application, state.Target)
	}
}

func TestLoadWithCluster(t *testing.T) {
	state, clusterName, err := LoadWithCluster(context.Background(), Options{
		App:  "web/prod",
		Root: filepath.Join("testdata"),
	})
	if err != nil {
		t.Fatalf("LoadWithCluster: %v", err)
	}
	if state.Application != "web" || state.Target != "prod" {
		t.Fatalf("application/target = %s/%s, want web/prod", state.Application, state.Target)
	}
	if clusterName != "prod-1" {
		t.Fatalf("cluster = %q, want prod-1", clusterName)
	}
}

func TestLoadByAppNotFound(t *testing.T) {
	_, err := Load(context.Background(), Options{
		App:  "missing/prod",
		Root: filepath.Join("testdata"),
	})
	if err == nil || !strings.Contains(err.Error(), `application "missing" not found`) {
		t.Fatalf("error = %v, want an application-not-found error", err)
	}
	if !strings.Contains(err.Error(), "invalid manifests") {
		t.Fatalf("error = %v, want the invalid manifest to be reported", err)
	}
}

func TestLoadErrorsWrapInvalidManifest(t *testing.T) {
	ctx := context.Background()
	if _, err := Load(ctx, Options{App: "broken/prod", Root: filepath.Join("testdata")}); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("broken manifest: err = %v, want ErrInvalidManifest", err)
	}
	path := writeApp(t, "kind: Application\n", nil)
	if _, err := Load(ctx, Options{File: path}); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("wrong apiVersion: err = %v, want ErrInvalidManifest", err)
	}
	if _, err := Load(ctx, Options{App: "missing/prod", Root: t.TempDir()}); !errors.Is(err, ErrNotDeclared) || errors.Is(err, ErrInvalidManifest) {
		t.Errorf("undeclared application: err = %v, want ErrNotDeclared only", err)
	}
}

func TestLoadReadErrorIsNotInvalidManifest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := Load(context.Background(), Options{App: "web/prod", Root: missing})
	if err == nil || errors.Is(err, ErrInvalidManifest) || errors.Is(err, ErrNotDeclared) {
		t.Fatalf("err = %v, want a read error that is neither invalid nor undeclared", err)
	}
}

func TestLoadBaseOnly(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    ports:
      - "8080:80"
`,
	})
	state := load(t, path, "")

	if state.Services[0].Replicas != 1 {
		t.Errorf("replicas = %d, want the default 1", state.Services[0].Replicas)
	}
	if state.Services[0].EndpointMode != "vip" {
		t.Errorf("endpoint mode = %q, want the default vip", state.Services[0].EndpointMode)
	}
	if len(state.Services[0].Ports) != 1 {
		t.Errorf("ports = %+v, want one", state.Services[0].Ports)
	}
}

func TestOverlayMergesLists(t *testing.T) {
	path := writeApp(t, appWithOverlay, map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    ports:
      - "8080:80"
`,
		"envs/prod.compose.yaml": `services:
  web:
    ports:
      - "9090:90"
`,
	})
	state := load(t, path, "prod")

	ports := state.Services[0].Ports
	if len(ports) != 2 {
		t.Fatalf("ports = %+v, want two (overlay must append)", ports)
	}
	if ports[0].Target != 80 || ports[1].Target != 90 {
		t.Fatalf("ports = %+v, want targets 80 and 90", ports)
	}
}

func TestLoadRejectsOverrideTags(t *testing.T) {
	tests := []struct {
		name    string
		overlay string
		want    string
	}{
		{
			name: "reset",
			overlay: `services:
  web:
    ports: !reset []
`,
			want: "!reset",
		},
		{
			name: "override",
			overlay: `services:
  web:
    ports: !override
      - "9090:90"
`,
			want: "!override",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeApp(t, appWithOverlay, map[string]string{
				"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    ports:
      - "8080:80"
`,
				"envs/prod.compose.yaml": tt.overlay,
			})
			_, err := Load(context.Background(), Options{File: path, Target: "prod"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want a %s rejection", err, tt.want)
			}
		})
	}
}

func TestManifestValidation(t *testing.T) {
	valid := `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
`
	tests := []struct {
		name string
		app  string
		want string
	}{
		{
			name: "api version",
			app:  strings.Replace(valid, "wormcd.dev/v1alpha1", "wormcd.dev/v2", 1),
			want: "apiVersion",
		},
		{
			name: "kind",
			app:  strings.Replace(valid, "kind: Application", "kind: Deployment", 1),
			want: "kind",
		},
		{
			name: "compose",
			app:  strings.Replace(valid, "  compose: compose.yaml\n", "", 1),
			want: "spec.compose is required",
		},
		{
			name: "name",
			app:  strings.Replace(valid, "name: web", "name: Web", 1),
			want: "metadata.name",
		},
		{
			name: "duplicate target",
			app: `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
    - name: prod
      cluster: prod-2
`,
			want: "duplicated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeApp(t, tt.app, nil)
			_, err := Load(context.Background(), Options{File: path})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadUsesDotEnvOnly(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:${TAG}
`,
		".env": "TAG=v1\n",
	})
	t.Setenv("TAG", "v9")

	state := load(t, path, "")
	if got, want := state.Services[0].Image, "ghcr.io/d00p1/web:v1"; got != want {
		t.Fatalf("image = %q, want %q (OS environment must not leak in)", got, want)
	}
}

func TestLoadRejectsFileAndApp(t *testing.T) {
	_, err := Load(context.Background(), Options{
		File: "testdata/basic/wormcd.yaml",
		App:  "web/prod",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want a mutual-exclusion error", err)
	}
}

func TestLoadRejectsMissingTarget(t *testing.T) {
	path := writeApp(t, `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: staging
      cluster: staging-1
    - name: prod
      cluster: prod-1
`, map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
`,
	})
	_, err := Load(context.Background(), Options{File: path, Target: "nope"})
	if err == nil || !strings.Contains(err.Error(), `target "nope" not found`) {
		t.Fatalf("error = %v, want an unknown-target error", err)
	}
}

func TestLoadRejectsEnvironmentWithoutValue(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    environment:
      - FOO
`,
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "has no value") {
		t.Fatalf("error = %v, want an environment value error", err)
	}
}

func TestLoadRejectsProfiles(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    profiles: [debug]
`,
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "profiles are not supported") {
		t.Fatalf("error = %v, want a profile rejection", err)
	}
}

func TestLoadRejectsConfigOutsideAppDir(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    configs: [app]
configs:
  app:
    file: ../outside.conf
`,
		"app.conf": "inside",
	})
	_, err := Load(context.Background(), Options{File: path})
	if err == nil || !strings.Contains(err.Error(), "outside the application directory") {
		t.Fatalf("error = %v, want an outside-directory error", err)
	}
}

func TestExternalNetworkAndVolume(t *testing.T) {
	path := writeApp(t, "", map[string]string{
		"compose.yaml": `services:
  web:
    image: ghcr.io/d00p1/web:v1
    networks: [shared]
    volumes: [shared:/data]
networks:
  shared:
    external: true
    name: shared_net
volumes:
  shared:
    external: true
    name: shared_data
`,
	})
	state := load(t, path, "")

	if len(state.Networks) != 1 || state.Networks[0].Name != "shared_net" || !state.Networks[0].External {
		t.Fatalf("networks = %+v, want external shared_net", state.Networks)
	}
	if len(state.Volumes) != 1 || state.Volumes[0].Name != "shared_data" || !state.Volumes[0].External {
		t.Fatalf("volumes = %+v, want external shared_data", state.Volumes)
	}
	web := state.Services[0]
	if web.Networks[0].Name != "shared_net" {
		t.Errorf("service network = %q, want shared_net", web.Networks[0].Name)
	}
	if web.Mounts[0].Source != "shared_data" {
		t.Errorf("mount source = %q, want shared_data", web.Mounts[0].Source)
	}
}

func load(t *testing.T, path, target string) *desired.Desired {
	t.Helper()
	state, err := Load(context.Background(), Options{File: path, Target: target})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return state
}

func serviceByName(t *testing.T, state *desired.Desired, name string) desired.Service {
	t.Helper()
	for _, service := range state.Services {
		if service.Name == name {
			return service
		}
	}
	t.Fatalf("service %q not found", name)
	return desired.Service{}
}

func writeApp(t *testing.T, app string, files map[string]string) string {
	t.Helper()
	if app == "" {
		app = `apiVersion: wormcd.dev/v1alpha1
kind: Application
metadata:
  name: web
spec:
  compose: compose.yaml
  targets:
    - name: prod
      cluster: prod-1
`
	}

	dir := t.TempDir()
	if files == nil {
		files = map[string]string{}
	}
	files["wormcd.yaml"] = app
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "wormcd.yaml")
}
