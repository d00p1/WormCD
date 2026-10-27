package swarm

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeAPI is an in-memory Docker. Creates and removals change what the list
// calls return, so tests can check the cluster state after Apply and Prune.
// Every mutating call is recorded in calls; err is returned by every call.
type fakeAPI struct {
	services client.ServiceListResult
	networks client.NetworkListResult
	configs  client.ConfigListResult
	secrets  client.SecretListResult
	volumes  client.VolumeListResult
	err      error

	calls []string
}

func (f *fakeAPI) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return f.services, f.err
}

func (f *fakeAPI) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return f.networks, f.err
}

func (f *fakeAPI) ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error) {
	return f.configs, f.err
}

func (f *fakeAPI) ConfigInspect(_ context.Context, name string, _ client.ConfigInspectOptions) (client.ConfigInspectResult, error) {
	if f.err != nil {
		return client.ConfigInspectResult{}, f.err
	}
	for _, config := range f.configs.Items {
		if config.Spec.Name == name {
			return client.ConfigInspectResult{Config: config}, nil
		}
	}
	return client.ConfigInspectResult{}, fmt.Errorf("config %s not found", name)
}

func (f *fakeAPI) SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error) {
	return f.secrets, f.err
}

func (f *fakeAPI) VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
	return f.volumes, f.err
}

func (f *fakeAPI) NetworkCreate(_ context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.calls = append(f.calls, "NetworkCreate:"+name)
	if f.err != nil {
		return client.NetworkCreateResult{}, f.err
	}
	id := "id-" + name
	f.networks.Items = append(f.networks.Items, network.Summary{Network: network.Network{
		ID:         id,
		Name:       name,
		Driver:     options.Driver,
		Options:    options.Options,
		Internal:   options.Internal,
		Attachable: options.Attachable,
		Labels:     options.Labels,
	}})
	return client.NetworkCreateResult{ID: id}, nil
}

func (f *fakeAPI) NetworkRemove(_ context.Context, id string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.calls = append(f.calls, "NetworkRemove:"+id)
	if f.err != nil {
		return client.NetworkRemoveResult{}, f.err
	}
	f.networks.Items = slices.DeleteFunc(f.networks.Items, func(n network.Summary) bool { return n.ID == id })
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeAPI) ConfigCreate(_ context.Context, options client.ConfigCreateOptions) (client.ConfigCreateResult, error) {
	name := options.Spec.Name
	f.calls = append(f.calls, "ConfigCreate:"+name)
	if f.err != nil {
		return client.ConfigCreateResult{}, f.err
	}
	id := "id-" + name
	f.configs.Items = append(f.configs.Items, swarm.Config{
		ID:   id,
		Meta: swarm.Meta{Version: swarm.Version{Index: 1}},
		Spec: options.Spec,
	})
	return client.ConfigCreateResult{ID: id}, nil
}

// ConfigUpdate mirrors Docker: the version must match the stored object, the
// data must not change, and only labels are replaced.
func (f *fakeAPI) ConfigUpdate(_ context.Context, name string, options client.ConfigUpdateOptions) (client.ConfigUpdateResult, error) {
	f.calls = append(f.calls, "ConfigUpdate:"+name)
	if f.err != nil {
		return client.ConfigUpdateResult{}, f.err
	}
	for i, config := range f.configs.Items {
		if config.Spec.Name != name {
			continue
		}
		if options.Version.Index != config.Version.Index {
			return client.ConfigUpdateResult{}, fmt.Errorf("config %s: version %d is out of date (have %d)",
				name, options.Version.Index, config.Version.Index)
		}
		if !bytes.Equal(options.Spec.Data, config.Spec.Data) {
			return client.ConfigUpdateResult{}, fmt.Errorf("config %s: data cannot be changed", name)
		}
		config.Spec.Labels = options.Spec.Labels
		config.Version.Index++
		f.configs.Items[i] = config
		return client.ConfigUpdateResult{}, nil
	}
	return client.ConfigUpdateResult{}, fmt.Errorf("config %s not found", name)
}

func (f *fakeAPI) ConfigRemove(_ context.Context, id string, _ client.ConfigRemoveOptions) (client.ConfigRemoveResult, error) {
	f.calls = append(f.calls, "ConfigRemove:"+id)
	if f.err != nil {
		return client.ConfigRemoveResult{}, f.err
	}
	f.configs.Items = slices.DeleteFunc(f.configs.Items, func(c swarm.Config) bool { return c.ID == id })
	return client.ConfigRemoveResult{}, nil
}

func (f *fakeAPI) SecretCreate(_ context.Context, options client.SecretCreateOptions) (client.SecretCreateResult, error) {
	name := options.Spec.Name
	f.calls = append(f.calls, "SecretCreate:"+name)
	if f.err != nil {
		return client.SecretCreateResult{}, f.err
	}
	id := "id-" + name
	f.secrets.Items = append(f.secrets.Items, swarm.Secret{ID: id, Spec: options.Spec})
	return client.SecretCreateResult{ID: id}, nil
}

func (f *fakeAPI) SecretRemove(_ context.Context, id string, _ client.SecretRemoveOptions) (client.SecretRemoveResult, error) {
	f.calls = append(f.calls, "SecretRemove:"+id)
	if f.err != nil {
		return client.SecretRemoveResult{}, f.err
	}
	f.secrets.Items = slices.DeleteFunc(f.secrets.Items, func(s swarm.Secret) bool { return s.ID == id })
	return client.SecretRemoveResult{}, nil
}

func (f *fakeAPI) VolumeCreate(_ context.Context, options client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	f.calls = append(f.calls, "VolumeCreate:"+options.Name)
	if f.err != nil {
		return client.VolumeCreateResult{}, f.err
	}
	created := volume.Volume{
		Name:    options.Name,
		Driver:  options.Driver,
		Options: options.DriverOpts,
		Labels:  options.Labels,
	}
	f.volumes.Items = append(f.volumes.Items, created)
	return client.VolumeCreateResult{Volume: created}, nil
}

func (f *fakeAPI) VolumeRemove(_ context.Context, name string, _ client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.calls = append(f.calls, "VolumeRemove:"+name)
	if f.err != nil {
		return client.VolumeRemoveResult{}, f.err
	}
	f.volumes.Items = slices.DeleteFunc(f.volumes.Items, func(v volume.Volume) bool { return v.Name == name })
	return client.VolumeRemoveResult{}, nil
}
