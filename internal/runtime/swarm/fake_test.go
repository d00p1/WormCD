package swarm

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeAPI is an in-memory Docker. Creates and removals change what the list
// calls return, so tests can check the cluster state after Apply and Prune.
// Every mutating call is recorded in calls; err is returned by every call.
//
// A service create or update starts the tasks of a revision: tasksFor decides
// what they look like, and by default every desired task runs. An update does
// not finish by itself. Like Swarm's updater, the fake settles an update when
// the List calls run: a failed new task pauses it, and once the new tasks
// run, the old tasks are shut down and the update completes. Old tasks keep
// running until then.
type fakeAPI struct {
	services client.ServiceListResult
	networks client.NetworkListResult
	configs  client.ConfigListResult
	secrets  client.SecretListResult
	volumes  client.VolumeListResult
	tasks    []swarm.Task
	tasksFor func(swarm.Service) []swarm.Task
	// onTaskList runs before every TaskList call, so a test can let tasks
	// progress between two polls.
	onTaskList func()
	// vanishOnRemove makes ServiceRemove report NotFound without removing
	// anything, as if another client removed the service first.
	vanishOnRemove bool
	err            error

	calls []string
}

// ServiceList fills in the task counts when the caller asks for status, as
// Docker does.
func (f *fakeAPI) ServiceList(_ context.Context, options client.ServiceListOptions) (client.ServiceListResult, error) {
	if f.err != nil {
		return client.ServiceListResult{}, f.err
	}
	f.settle()
	if !options.Status {
		return f.services, nil
	}
	out := client.ServiceListResult{Items: make([]swarm.Service, 0, len(f.services.Items))}
	for _, service := range f.services.Items {
		desiredCount, _ := desiredTasks(service)
		var running uint64
		for _, task := range f.tasks {
			if task.ServiceID == service.ID && task.DesiredState == swarm.TaskStateRunning && task.Status.State == swarm.TaskStateRunning {
				running++
			}
		}
		service.ServiceStatus = &swarm.ServiceStatus{RunningTasks: running, DesiredTasks: desiredCount}
		out.Items = append(out.Items, service)
	}
	return out, nil
}

func (f *fakeAPI) TaskList(_ context.Context, options client.TaskListOptions) (client.TaskListResult, error) {
	if f.onTaskList != nil {
		f.onTaskList()
	}
	if f.err != nil {
		return client.TaskListResult{}, f.err
	}
	f.settle()
	var out client.TaskListResult
	for _, task := range f.tasks {
		if !filterHas(options.Filters, "service", task.ServiceID) {
			continue
		}
		if !filterHas(options.Filters, "desired-state", string(task.DesiredState)) {
			continue
		}
		out.Items = append(out.Items, task)
	}
	return out, nil
}

// filterHas reports whether the filter lets value through: a missing key
// matches everything.
func filterHas(filters client.Filters, key, value string) bool {
	values, ok := filters[key]
	return !ok || values[value]
}

func (f *fakeAPI) ServiceInspect(_ context.Context, name string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	if f.err != nil {
		return client.ServiceInspectResult{}, f.err
	}
	for _, service := range f.services.Items {
		if service.Spec.Name == name {
			return client.ServiceInspectResult{Service: service}, nil
		}
	}
	return client.ServiceInspectResult{}, cerrdefs.ErrNotFound.WithMessage("service " + name + " not found")
}

// ServiceCreate stores the service with version 1 and starts its tasks.
func (f *fakeAPI) ServiceCreate(_ context.Context, options client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	name := options.Spec.Name
	f.calls = append(f.calls, "ServiceCreate:"+name)
	if f.err != nil {
		return client.ServiceCreateResult{}, f.err
	}
	service := swarm.Service{ID: "id-" + name, Meta: swarm.Meta{Version: swarm.Version{Index: 1}, UpdatedAt: time.Now()}, Spec: options.Spec}
	f.services.Items = append(f.services.Items, service)
	f.startTasks(service)
	return client.ServiceCreateResult{ID: service.ID}, nil
}

// ServiceUpdate mirrors Docker: the version must match the stored service. The
// update is started, not finished: its tasks are added, and the old ones keep
// running until settle retires them.
func (f *fakeAPI) ServiceUpdate(_ context.Context, id string, options client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	f.calls = append(f.calls, "ServiceUpdate:"+id)
	if f.err != nil {
		return client.ServiceUpdateResult{}, f.err
	}
	for i, service := range f.services.Items {
		if service.ID != id {
			continue
		}
		if options.Version.Index != service.Version.Index {
			return client.ServiceUpdateResult{}, fmt.Errorf("service %s: version %d is out of date (have %d)",
				service.Spec.Name, options.Version.Index, service.Version.Index)
		}
		service.Spec = options.Spec
		service.Version.Index++
		service.UpdatedAt = time.Now()
		service.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateUpdating}
		f.services.Items[i] = service
		f.startTasks(service)
		return client.ServiceUpdateResult{}, nil
	}
	return client.ServiceUpdateResult{}, fmt.Errorf("service %s not found", id)
}

// ServiceRemove removes the service and its tasks.
func (f *fakeAPI) ServiceRemove(_ context.Context, id string, _ client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	f.calls = append(f.calls, "ServiceRemove:"+id)
	if f.err != nil {
		return client.ServiceRemoveResult{}, f.err
	}
	if f.vanishOnRemove || !slices.ContainsFunc(f.services.Items, func(s swarm.Service) bool { return s.ID == id }) {
		return client.ServiceRemoveResult{}, cerrdefs.ErrNotFound.WithMessage("service " + id + " not found")
	}
	f.services.Items = slices.DeleteFunc(f.services.Items, func(s swarm.Service) bool { return s.ID == id })
	f.tasks = slices.DeleteFunc(f.tasks, func(t swarm.Task) bool { return t.ServiceID == id })
	return client.ServiceRemoveResult{}, nil
}

// startTasks adds the tasks of the service's current revision. Tasks of earlier
// revisions are left alone.
func (f *fakeAPI) startTasks(service swarm.Service) {
	tasks := f.defaultTasks(service)
	if f.tasksFor != nil {
		tasks = f.tasksFor(service)
	}
	now := time.Now()
	for i := range tasks {
		if tasks[i].CreatedAt.IsZero() {
			tasks[i].CreatedAt = now
		}
	}
	f.tasks = append(f.tasks, tasks...)
}

// settle plays Swarm's updater for every service with an update in progress. A
// failed task of the new revision pauses the update. Once every desired task of
// the new revision runs, the tasks of older revisions are shut down and the update
// completes.
func (f *fakeAPI) settle() {
	for i, service := range f.services.Items {
		if service.UpdateStatus == nil || service.UpdateStatus.State != swarm.UpdateStateUpdating {
			continue
		}
		var running uint64
		var failed bool
		for _, task := range f.tasks {
			if task.ServiceID != service.ID || !isCurrent(task, service) {
				continue
			}
			switch {
			case task.Status.State == swarm.TaskStateFailed || task.Status.State == swarm.TaskStateRejected:
				failed = true
			case task.DesiredState == swarm.TaskStateRunning && task.Status.State == swarm.TaskStateRunning:
				running++
			}
		}
		if failed {
			service.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStatePaused, Message: "task failed"}
			f.services.Items[i] = service
			continue
		}
		if desiredCount, _ := desiredTasks(service); running < desiredCount {
			continue
		}
		for j, task := range f.tasks {
			if task.ServiceID == service.ID && !isCurrent(task, service) {
				f.tasks[j].DesiredState = swarm.TaskStateShutdown
				f.tasks[j].Status.State = swarm.TaskStateShutdown
			}
		}
		service.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateCompleted}
		f.services.Items[i] = service
	}
}

// defaultTasks runs every desired task of the service's spec.
func (f *fakeAPI) defaultTasks(service swarm.Service) []swarm.Task {
	count, _ := desiredTasks(service)
	tasks := make([]swarm.Task, 0, count)
	for i := range count {
		tasks = append(tasks, swarm.Task{
			ID:           fmt.Sprintf("%s-task-%d", service.ID, i),
			ServiceID:    service.ID,
			DesiredState: swarm.TaskStateRunning,
			Status:       swarm.TaskStatus{State: swarm.TaskStateRunning},
			Spec:         service.Spec.TaskTemplate,
		})
	}
	return tasks
}

// pendingTasks is a task list in which every task waits for a node.
func pendingTasks(service swarm.Service) []swarm.Task {
	return []swarm.Task{{
		ID:           service.ID + "-task-0",
		ServiceID:    service.ID,
		DesiredState: swarm.TaskStateRunning,
		Status:       swarm.TaskStatus{State: swarm.TaskStatePending},
		Spec:         service.Spec.TaskTemplate,
	}}
}

// failedTasks is a task list in which every task of the service failed.
func failedTasks(message string) func(swarm.Service) []swarm.Task {
	return func(service swarm.Service) []swarm.Task {
		return []swarm.Task{{
			ID:           service.ID + "-task-0",
			ServiceID:    service.ID,
			DesiredState: swarm.TaskStateRunning,
			Status:       swarm.TaskStatus{State: swarm.TaskStateFailed, Err: message},
			Spec:         service.Spec.TaskTemplate,
		}}
	}
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

func (f *fakeAPI) SecretInspect(_ context.Context, name string, _ client.SecretInspectOptions) (client.SecretInspectResult, error) {
	if f.err != nil {
		return client.SecretInspectResult{}, f.err
	}
	for _, secret := range f.secrets.Items {
		if secret.Spec.Name == name {
			return client.SecretInspectResult{Secret: secret}, nil
		}
	}
	return client.SecretInspectResult{}, fmt.Errorf("secret %s not found", name)
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
