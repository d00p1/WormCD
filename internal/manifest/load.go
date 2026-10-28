package manifest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/d00p1/wormcd/internal/desired"
	"go.yaml.in/yaml/v4"
)

// Options selects the application manifest and target to render.
type Options struct {
	// File is the path to wormcd.yaml. Mutually exclusive with App.
	File string
	// App selects an application as "name/target" and discovers its
	// wormcd.yaml under Root. Mutually exclusive with File.
	App string
	// Root is the deploy repository root used by App. Defaults to ".".
	Root string
	// Target is a target name. It may be omitted when the application has
	// exactly one target. Not used with App, where the target comes from App.
	Target string
}

// Load reads a wormcd.yaml, loads its Compose file with the target overlay and
// returns the normalized desired state.
func Load(ctx context.Context, opts Options) (*desired.Desired, error) {
	state, _, err := LoadWithCluster(ctx, opts)
	return state, err
}

// LoadWithCluster is Load plus the cluster name declared by the selected
// target. The name is not part of the desired state: cluster registration is a
// control-plane concern, not a runtime object.
func LoadWithCluster(ctx context.Context, opts Options) (*desired.Desired, string, error) {
	file, target, err := resolveOptions(opts)
	if err != nil {
		return nil, "", err
	}

	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, "", fmt.Errorf("resolve manifest path: %w", err)
	}

	app, err := loadApplication(abs)
	if err != nil {
		return nil, "", err
	}
	if err := app.validate(); err != nil {
		return nil, "", fmt.Errorf("%s: %w: %w", abs, ErrInvalidManifest, err)
	}

	selected, err := app.selectTarget(target)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", abs, err)
	}

	baseDir := filepath.Dir(abs)
	composePath, err := resolveLocal(baseDir, app.Spec.Compose)
	if err != nil {
		return nil, "", fmt.Errorf("%s: spec.compose: %w", abs, err)
	}

	configs := []string{composePath}
	if selected.Overlay != "" {
		overlayPath, err := resolveLocal(baseDir, selected.Overlay)
		if err != nil {
			return nil, "", fmt.Errorf("%s: target %q overlay: %w", abs, selected.Name, err)
		}
		configs = append(configs, overlayPath)
	}

	namespace := desired.TargetNamespace(app.Metadata.Name, selected.Name)

	for _, path := range configs {
		if err := rejectOverrideTags(path); err != nil {
			return nil, "", err
		}
	}

	projectOptions, err := cli.NewProjectOptions(
		configs,
		cli.WithWorkingDirectory(baseDir),
		cli.WithName(namespace),
		cli.WithEnvFiles(),
		cli.WithDotEnv,
	)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w: %w", abs, ErrInvalidManifest, err)
	}

	project, err := projectOptions.LoadProject(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("%s: %w", abs, err)
		}
		return nil, "", fmt.Errorf("%s: %w: %w", abs, ErrInvalidManifest, err)
	}

	state, err := normalize(project, app.Metadata.Name, selected, namespace, baseDir)
	if err != nil {
		if len(project.ComposeFiles) > 0 {
			return nil, "", fmt.Errorf("%s: %w: %w", strings.Join(project.ComposeFiles, ", "), ErrInvalidManifest, err)
		}
		return nil, "", fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	return state, selected.Cluster, nil
}

// Declares reports whether the manifests under opts.Root declare the application
// and target that opts.App names. It reads the manifests and does not render the
// Compose files. An undeclared name wraps ErrNotDeclared.
func Declares(opts Options) error {
	file, target, err := resolveOptions(opts)
	if err != nil {
		return err
	}
	app, err := loadApplication(file)
	if err != nil {
		return err
	}
	_, err = app.selectTarget(target)
	return err
}

func resolveOptions(opts Options) (file, target string, err error) {
	switch {
	case opts.File != "" && opts.App != "":
		return "", "", fmt.Errorf("file and app are mutually exclusive")
	case opts.File != "":
		return opts.File, opts.Target, nil
	case opts.App != "":
		if opts.Target != "" {
			return "", "", fmt.Errorf("target cannot be used with app; use name/target")
		}
		name, targetName, ok := strings.Cut(opts.App, "/")
		if !ok || name == "" || targetName == "" {
			return "", "", fmt.Errorf("app %q must be in name/target form", opts.App)
		}
		root := opts.Root
		if root == "" {
			root = "."
		}
		found, err := findApplication(root, name)
		if err != nil {
			return "", "", err
		}
		return found, targetName, nil
	default:
		return "", "", fmt.Errorf("either file or app is required")
	}
}

// ErrInvalidManifest is the cause of an error in the manifests themselves: they
// do not parse, do not validate, reference a path outside the application, or
// do not render. Errors of reading the checkout, such as a missing root, do not
// wrap it.
var ErrInvalidManifest = errors.New("invalid manifest")

// ErrNotDeclared is returned when the application or the target is not in the
// manifests under the root. The manifests are the source of truth, so an
// undeclared name is not an invalid manifest.
var ErrNotDeclared = errors.New("not declared in the manifests")

func findApplication(root, name string) (string, error) {
	var found []string
	var invalid []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "wormcd.yaml" {
			return nil
		}
		app, err := loadApplication(path)
		if err != nil {
			invalid = append(invalid, err.Error())
			return nil
		}
		if app.Metadata.Name == name {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		if len(invalid) > 0 {
			return "", fmt.Errorf("%w: application %q not found under %s (invalid manifests: %s)", ErrInvalidManifest, name, root, strings.Join(invalid, "; "))
		}
		return "", fmt.Errorf("%w: application %q not found under %s", ErrNotDeclared, name, root)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%w: application %q is ambiguous: %s", ErrInvalidManifest, name, strings.Join(found, ", "))
	}
}

func rejectOverrideTags(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("%s: %w: %w", path, ErrInvalidManifest, err)
	}
	return walkTags(&document, path)
}

func walkTags(node *yaml.Node, path string) error {
	if node.Tag == "!reset" || node.Tag == "!override" {
		return fmt.Errorf("%w: %s:%d:%d: %s tags are not supported; use merge-only overlays", ErrInvalidManifest, path, node.Line, node.Column, node.Tag)
	}
	for _, child := range node.Content {
		if err := walkTags(child, path); err != nil {
			return err
		}
	}
	return nil
}

func resolveLocal(baseDir, path string) (string, error) {
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("%w: %q must be a relative path inside the application directory", ErrInvalidManifest, path)
	}
	return filepath.Join(baseDir, path), nil
}
