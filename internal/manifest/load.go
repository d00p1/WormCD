package manifest

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/d00p1/wormcd/internal/desired"
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
	file, target, err := resolveOptions(opts)
	if err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest path: %w", err)
	}

	app, err := loadApplication(abs)
	if err != nil {
		return nil, err
	}
	if err := app.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}

	selected, err := app.selectTarget(target)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}

	baseDir := filepath.Dir(abs)
	composePath, err := resolveLocal(baseDir, app.Spec.Compose)
	if err != nil {
		return nil, fmt.Errorf("%s: spec.compose: %w", abs, err)
	}

	configs := []string{composePath}
	if selected.Overlay != "" {
		overlayPath, err := resolveLocal(baseDir, selected.Overlay)
		if err != nil {
			return nil, fmt.Errorf("%s: target %q overlay: %w", abs, selected.Name, err)
		}
		configs = append(configs, overlayPath)
	}

	namespace := desired.Namespace(app.Metadata.Name, selected.Name)

	projectOptions, err := cli.NewProjectOptions(
		configs,
		cli.WithWorkingDirectory(baseDir),
		cli.WithName(namespace),
		cli.WithEnvFiles(),
		cli.WithDotEnv,
	)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}

	project, err := projectOptions.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}

	state, err := normalize(project, app.Metadata.Name, selected, namespace, baseDir)
	if err != nil && len(project.ComposeFiles) > 0 {
		return nil, fmt.Errorf("%s: %w", strings.Join(project.ComposeFiles, ", "), err)
	}
	return state, err
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

func findApplication(root, name string) (string, error) {
	var found []string
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
			return err
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
		return "", fmt.Errorf("application %q not found under %s", name, root)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("application %q is ambiguous: %s", name, strings.Join(found, ", "))
	}
}

func resolveLocal(baseDir, path string) (string, error) {
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("%q must be a relative path inside the application directory", path)
	}
	return filepath.Join(baseDir, path), nil
}
