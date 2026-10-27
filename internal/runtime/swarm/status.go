package swarm

import (
	"cmp"
	"context"
	"fmt"
	"reflect"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// serviceStatus is what Apply and Health read about one service. Only tasks of
// the revision the service runs now count (see isCurrent).
type serviceStatus struct {
	Name          string
	Version       uint64
	Update        swarm.UpdateState
	UpdateMessage string
	Desired       uint64
	DesiredKnown  bool
	// Running is the number of current tasks that run, and Starting the number
	// of current tasks that are meant to run but do not yet.
	Running  uint64
	Starting uint64
	// Failures are the current tasks that failed or were rejected, with their
	// errors. A crash loop shows here even though Swarm marks each failed task
	// as shut down.
	Failures []string
}

// isCurrent reports whether a task was made from the revision the service runs
// now. Docker has no revision number on tasks, so the task spec is compared with
// the service's task template, as swarmkit's IsTaskDirty does, and the task must
// not be older than the last update.
//
// This assumes the daemon stores the template as it was sent. If it normalized
// the spec, no task would match and Apply would never converge; the integration
// tests in #22 check that.
func isCurrent(task swarm.Task, service swarm.Service) bool {
	return reflect.DeepEqual(task.Spec, service.Spec.TaskTemplate) && !task.CreatedAt.Before(service.UpdatedAt)
}

// pending says why the service has not converged on a version newer than
// after, or is empty when it has.
func (s serviceStatus) pending(after uint64) string {
	if s.Version <= after {
		return "update not applied yet"
	}
	return s.progress()
}

// progress says what keeps the service from its desired state, or is empty when
// nothing does. A failed update is not progress: failures reports it.
func (s serviceStatus) progress() string {
	switch {
	case s.shortfall() != "":
		return s.shortfall()
	case s.Starting > 0:
		return fmt.Sprintf("%d tasks starting", s.Starting)
	case s.Update == swarm.UpdateStateUpdating:
		return "update in progress"
	}
	return ""
}

// failures lists the update and task errors that stop a service from
// converging, and that Health reports.
func (s serviceStatus) failures() []string {
	var out []string
	switch s.Update {
	case swarm.UpdateStatePaused, swarm.UpdateStateRollbackStarted,
		swarm.UpdateStateRollbackPaused, swarm.UpdateStateRollbackCompleted:
		out = append(out, "update "+string(s.Update)+detail(s.UpdateMessage))
	}
	return append(out, s.Failures...)
}

// problems lists why the service is not healthy right now.
func (s serviceStatus) problems() []string {
	out := s.failures()
	if reason := s.progress(); reason != "" {
		out = append(out, reason)
	}
	return out
}

// shortfall describes the desired tasks that do not run, or is empty when every
// desired task runs.
func (s serviceStatus) shortfall() string {
	switch {
	case !s.DesiredKnown:
		return "desired task count is unknown"
	case s.Running < s.Desired:
		return fmt.Sprintf("%d of %d tasks running", s.Running, s.Desired)
	}
	return ""
}

func detail(message string) string {
	if message == "" {
		return ""
	}
	return ": " + message
}

// serviceStatuses reads the status of every service that keep accepts. The
// service list carries the desired task count; one task list covers all of
// them.
func (r *Runtime) serviceStatuses(ctx context.Context, filters client.Filters, keep func(swarm.Service) bool) ([]serviceStatus, error) {
	list, err := r.api.ServiceList(ctx, client.ServiceListOptions{Filters: filters, Status: true})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	var services []swarm.Service
	ids := make([]string, 0, len(list.Items))
	for _, service := range list.Items {
		if keep(service) {
			services = append(services, service)
			ids = append(ids, service.ID)
		}
	}
	if len(services) == 0 {
		return nil, nil
	}

	tasks, err := r.api.TaskList(ctx, client.TaskListOptions{
		Filters: client.Filters{}.Add("service", ids...),
	})
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	byService := make(map[string][]swarm.Task, len(services))
	for _, task := range tasks.Items {
		byService[task.ServiceID] = append(byService[task.ServiceID], task)
	}

	out := make([]serviceStatus, 0, len(services))
	for _, service := range services {
		out = append(out, statusOf(service, byService[service.ID]))
	}
	return out, nil
}

func statusOf(service swarm.Service, tasks []swarm.Task) serviceStatus {
	status := serviceStatus{Name: service.Spec.Name, Version: service.Version.Index}
	if service.UpdateStatus != nil {
		status.Update = service.UpdateStatus.State
		status.UpdateMessage = service.UpdateStatus.Message
	}
	status.Desired, status.DesiredKnown = desiredTasks(service)

	for _, task := range tasks {
		if !isCurrent(task, service) {
			continue
		}
		switch {
		case task.Status.State == swarm.TaskStateFailed || task.Status.State == swarm.TaskStateRejected:
			status.Failures = append(status.Failures, taskFailure(task))
		case task.DesiredState != swarm.TaskStateRunning:
		case task.Status.State == swarm.TaskStateRunning:
			status.Running++
		default:
			status.Starting++
		}
	}
	return status
}

// desiredTasks is the number of tasks the service should run. Swarm reports it
// in the service status; a replicated service without that status uses its
// replica count, and any other service has no known count.
func desiredTasks(service swarm.Service) (uint64, bool) {
	if service.ServiceStatus != nil {
		return service.ServiceStatus.DesiredTasks, true
	}
	if replicated := service.Spec.Mode.Replicated; replicated != nil {
		if replicated.Replicas != nil {
			return *replicated.Replicas, true
		}
		return desired.DefaultReplicas, true
	}
	return 0, false
}

func taskFailure(task swarm.Task) string {
	return fmt.Sprintf("task %s %s", task.ID, task.Status.State) + detail(cmp.Or(task.Status.Err, task.Status.Message))
}
