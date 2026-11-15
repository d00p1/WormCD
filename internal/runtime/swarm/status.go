package swarm

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"time"

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
// now. Docker has no revision number on tasks, so the task spec is compared
// with the service's task template, as swarmkit's IsTaskDirty does.
//
// The daemon does not store the template as it was sent: it drops fields equal
// to defaults from the stored task spec (Runtime "container", a 10s stop grace
// period, an empty DNSConfig, an empty Placement, the default restart policy)
// and adds some to both specs (Init, Isolation, the resolved image digest).
// Both specs are therefore canonicalized before the comparison; the
// integration tests in #22 cover a real daemon's normalization.
//
// A task's creation time is not compared with the service's update time: the
// daemon bumps the service version and UpdatedAt when it refreshes the
// service's runtime status, so a current task can look older than the last
// update. What the task runs is its spec.
func isCurrent(task swarm.Task, service swarm.Service) bool {
	return canonicalSpec(task.Spec) == canonicalSpec(service.Spec.TaskTemplate)
}

// Defaults the daemon drops from the task spec it stores. They match the
// daemon's own defaults, so removing them from both specs loses nothing.
const (
	defaultStopGraceNanos = int64(10 * time.Second)
	defaultRestartDelay   = int64(5 * time.Second)
)

// canonicalSpec is the JSON encoding of spec with daemon-defaulted fields
// removed, so a stored task spec and the service's task template compare
// equal when they describe the same task.
func canonicalSpec(spec swarm.TaskSpec) string {
	data, err := json.Marshal(spec)
	if err != nil {
		return fmt.Sprintf("%#v", spec)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return string(data)
	}
	if m["Runtime"] == "container" {
		delete(m, "Runtime")
	}
	if c, ok := m["ContainerSpec"].(map[string]any); ok {
		if c["StopGracePeriod"] == float64(defaultStopGraceNanos) {
			delete(c, "StopGracePeriod")
		}
		if dns, ok := c["DNSConfig"].(map[string]any); ok && len(dns) == 0 {
			delete(c, "DNSConfig")
		}
	}
	if p, ok := m["Placement"].(map[string]any); ok && len(p) == 0 {
		delete(m, "Placement")
	}
	if rp, ok := m["RestartPolicy"].(map[string]any); ok && isDefaultRestartPolicy(rp) {
		delete(m, "RestartPolicy")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return string(data)
	}
	return string(out)
}

// isDefaultRestartPolicy reports whether rp encodes the daemon's default
// restart policy (condition any, delay 5s, no attempt or window limit). The
// omitted zero fields are not in the map.
func isDefaultRestartPolicy(rp map[string]any) bool {
	if rp["Condition"] != "any" {
		return false
	}
	if len(rp) == 1 {
		return true
	}
	return len(rp) == 2 && rp["Delay"] == float64(defaultRestartDelay)
}

// pending says why the service has not converged on a version newer than
// after, or is empty when it has.
func (s serviceStatus) pending(after uint64) string {
	if s.Version <= after {
		return "update not applied yet"
	}
	return s.blocker()
}

// blocker says what keeps the service from its desired state right now, or is
// empty when nothing does. An update that Docker paused or rolled back is not a
// blocker: updateFailure reports it.
func (s serviceStatus) blocker() string {
	if reason := s.shortfall(); reason != "" {
		return reason
	}
	switch {
	case s.Starting > 0:
		return fmt.Sprintf("%d tasks starting", s.Starting)
	case s.Update == swarm.UpdateStateUpdating:
		return "update in progress"
	}
	return ""
}

// updateFailure says why Docker stopped the update, or is empty when it did not.
// A paused update waits for a person; a rolled-back one runs the previous spec.
func (s serviceStatus) updateFailure() string {
	var what string
	switch s.Update {
	case swarm.UpdateStatePaused:
		what = "update paused"
	case swarm.UpdateStateRollbackStarted:
		what = "update rolling back"
	case swarm.UpdateStateRollbackPaused:
		what = "update rollback paused"
	case swarm.UpdateStateRollbackCompleted:
		what = "update rolled back"
	default:
		return ""
	}
	return what + detail(s.UpdateMessage)
}

// problems lists why the service is not healthy right now.
func (s serviceStatus) problems() []string {
	var out []string
	if reason := s.updateFailure(); reason != "" {
		out = append(out, reason)
	}
	out = append(out, s.Failures...)
	if reason := s.blocker(); reason != "" {
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
		case isFailed(task.Status.State):
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

// isFailed reports whether a task ended in an error. Swarm reports a task that
// cannot be started as rejected.
func isFailed(state swarm.TaskState) bool {
	return state == swarm.TaskStateFailed || state == swarm.TaskStateRejected
}

func taskFailure(task swarm.Task) string {
	return fmt.Sprintf("task %s %s", task.ID, task.Status.State) + detail(cmp.Or(task.Status.Err, task.Status.Message))
}
