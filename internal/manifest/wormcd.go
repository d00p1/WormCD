package manifest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

const (
	// APIVersion is the only manifest API version v0 understands.
	APIVersion = "wormcd.dev/v1alpha1"
	// KindApplication is the only manifest kind v0 understands.
	KindApplication = "Application"
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Application is the content of a wormcd.yaml file.
type Application struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata identifies an application.
type Metadata struct {
	Name string `yaml:"name"`
}

// Spec describes where the application lives and where it deploys.
type Spec struct {
	Compose string   `yaml:"compose"`
	Targets []Target `yaml:"targets"`
}

// Target is one deployable environment of an application.
type Target struct {
	Name       string      `yaml:"name"`
	Cluster    string      `yaml:"cluster"`
	Overlay    string      `yaml:"overlay,omitempty"`
	SyncPolicy *SyncPolicy `yaml:"syncPolicy,omitempty"`
}

// SyncPolicy controls reconciliation for a target. It is parsed in v0 and
// enforced in M3.
type SyncPolicy struct {
	Automated bool `yaml:"automated"`
	Prune     bool `yaml:"prune"`
	SelfHeal  bool `yaml:"selfHeal"`
}

func loadApplication(path string) (*Application, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var app Application
	if err := dec.Decode(&app); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", path, ErrInvalidManifest, err)
	}

	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s: %w: multiple YAML documents are not supported", path, ErrInvalidManifest)
		}
		return nil, fmt.Errorf("%s: %w: %w", path, ErrInvalidManifest, err)
	}
	return &app, nil
}

func (a *Application) validate() error {
	if a.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion = %q, want %q", a.APIVersion, APIVersion)
	}
	if a.Kind != KindApplication {
		return fmt.Errorf("kind = %q, want %q", a.Kind, KindApplication)
	}
	if !nameRE.MatchString(a.Metadata.Name) {
		return fmt.Errorf("metadata.name %q must be lowercase alphanumeric with dashes", a.Metadata.Name)
	}
	if a.Spec.Compose == "" {
		return fmt.Errorf("spec.compose is required")
	}
	if !filepath.IsLocal(a.Spec.Compose) {
		return fmt.Errorf("spec.compose %q must be a relative path inside the application directory", a.Spec.Compose)
	}
	if len(a.Spec.Targets) == 0 {
		return fmt.Errorf("spec.targets must contain at least one target")
	}

	seen := make(map[string]bool, len(a.Spec.Targets))
	for i, t := range a.Spec.Targets {
		field := fmt.Sprintf("spec.targets[%d]", i)
		if !nameRE.MatchString(t.Name) {
			return fmt.Errorf("%s.name %q must be lowercase alphanumeric with dashes", field, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s.name %q is duplicated", field, t.Name)
		}
		seen[t.Name] = true
		if t.Cluster == "" {
			return fmt.Errorf("%s.cluster is required", field)
		}
		if t.Overlay != "" && !filepath.IsLocal(t.Overlay) {
			return fmt.Errorf("%s.overlay %q must be a relative path inside the application directory", field, t.Overlay)
		}
	}
	return nil
}

func (a *Application) selectTarget(name string) (Target, error) {
	if name != "" {
		for _, t := range a.Spec.Targets {
			if t.Name == name {
				return t, nil
			}
		}
		return Target{}, fmt.Errorf("%w: target %q not found (available: %s)", ErrNotDeclared, name, a.targetNames())
	}
	if len(a.Spec.Targets) == 1 {
		return a.Spec.Targets[0], nil
	}
	return Target{}, fmt.Errorf("%w: target is required (available: %s)", ErrInvalidManifest, a.targetNames())
}

func (a *Application) targetNames() string {
	names := make([]string, 0, len(a.Spec.Targets))
	for _, t := range a.Spec.Targets {
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}
