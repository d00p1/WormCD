package swarm

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/d00p1/wormcd/internal/desired"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// serviceStatus is what Apply and Health read about one service: its version,
// its update state, how many of its desired tasks run, and the tasks that
// failed.
type serviceStatus struct {
	Name    string
	Version uint64
	Update  swarm.UpdateState
	// UpdateMessage explains a paused or rolled-back update.
	UpdateMessage string
	Desired       uint64
	DesiredKnown  bool
	Running       uint64
	// Failures are failed or rejected tasks that are meant to run.
	Failures []string
	// RevisionFailures are failed tasks of the image the service runs now,
	// whatever their desired state. Swarm replaces a failed task at once and
	// marks it shut down, so a crash loop never shows as a failed running task.
	RevisionFailures []string
}

// pending says why the service has not converged on a version newer than
// after, or is empty when it has.
func (s serviceStatus) pending(after uint64) string {
	switch {
	case s.Version <= after:
		return "update not applied yet"
	case s.Update != "" && s.Update != swarm.UpdateStateCompleted:
		return "update " + string(s.Update)
	}
	return s.shortfall()
}

// failures lists the update and task errors that stop a service from
// converging.
func (s serviceStatus) failures() []string {
	var out []string
	switch s.Update {
	case swarm.UpdateStatePaused, swarm.UpdateStateRollbackStarted,
		swarm.UpdateStateRollbackPaused, swarm.UpdateStateRollbackCompleted:
		out = append(out, "update "+string(s.Update)+detail(s.UpdateMessage))
	}
	return append(out, s.Failures...)
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
// service list carries the desired task count; the tasks of each service give
// the running count and the failures.
func (r *Runtime) serviceStatuses(ctx context.Context, filters client.Filters, keep func(swarm.Service) bool) ([]serviceStatus, error) {
	list, err := r.api.ServiceList(ctx, client.ServiceListOptions{Filters: filters, Status: true})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	var out []serviceStatus
	for _, service := range list.Items {
		if !keep(service) {
			continue
		}
		status, err := r.readStatus(ctx, service)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}

func (r *Runtime) readStatus(ctx context.Context, service swarm.Service) (serviceStatus, error) {
	status := serviceStatus{Name: service.Spec.Name, Version: service.Version.Index}
	if service.UpdateStatus != nil {
		status.Update = service.UpdateStatus.State
		status.UpdateMessage = service.UpdateStatus.Message
	}
	status.Desired, status.DesiredKnown = desiredTasks(service)

	image := ""
	if containerSpec := service.Spec.TaskTemplate.ContainerSpec; containerSpec != nil {
		image = containerSpec.Image
	}
	tasks, err := r.api.TaskList(ctx, client.TaskListOptions{
		Filters: client.Filters{}.Add("service", service.ID),
	})
	if err != nil {
		return serviceStatus{}, fmt.Errorf("list tasks of service %s: %w", status.Name, err)
	}
	for _, task := range tasks.Items {
		failed := task.Status.State == swarm.TaskStateFailed || task.Status.State == swarm.TaskStateRejected
		switch {
		case task.DesiredState == swarm.TaskStateRunning && task.Status.State == swarm.TaskStateRunning:
			status.Running++
		case failed && task.DesiredState == swarm.TaskStateRunning:
			status.Failures = append(status.Failures, taskFailure(task))
		case failed && taskImage(task) == image:
			status.RevisionFailures = append(status.RevisionFailures, taskFailure(task))
		}
	}
	return status, nil
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

func taskImage(task swarm.Task) string {
	if containerSpec := task.Spec.ContainerSpec; containerSpec != nil {
		return containerSpec.Image
	}
	return ""
}

func taskFailure(task swarm.Task) string {
	return fmt.Sprintf("task %s %s", task.ID, task.Status.State) + detail(cmp.Or(task.Status.Err, task.Status.Message))
}

// waitConverged polls the service until it converges on a version newer than
// after. A failed update or task ends the wait at once, so a bad image is
// reported without waiting out the deadline.
func (r *Runtime) waitConverged(ctx context.Context, name string, after uint64) error {
	ctx, cancel := context.WithTimeout(ctx, r.converge.Timeout)
	defer cancel()

	var pending string
	for {
		statuses, err := r.serviceStatuses(ctx, client.Filters{}.Add("name", name), func(s swarm.Service) bool {
			return s.Spec.Name == name
		})
		if err != nil {
			return fmt.Errorf("check service %s: %w", name, err)
		}
		if len(statuses) != 1 {
			return fmt.Errorf("service %s: not found after apply", name)
		}
		status := statuses[0]
		pending = status.pending(after)
		if pending == "" {
			return nil
		}
		if failures := append(status.failures(), status.RevisionFailures...); len(failures) > 0 {
			return fmt.Errorf("service %s: %s", name, strings.Join(failures, "; "))
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("service %s: not converged (%s): %w", name, pending, ctx.Err())
		case <-time.After(r.converge.Interval):
		}
	}
}
