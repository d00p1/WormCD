package swarm

import (
	"context"
	"fmt"

	"github.com/d00p1/wormcd/internal/runtime"
	"github.com/moby/moby/api/types/swarm"
)

// Health reports whether the services of scope run their desired tasks. A scope
// with no managed object at all is not healthy: nothing was applied, or it was
// deleted. A scope with objects but no service has nothing to run, so it is
// healthy.
func (r *Runtime) Health(ctx context.Context, scope runtime.Scope) (runtime.Health, error) {
	observed, err := r.Inspect(ctx, scope)
	if err != nil {
		return runtime.Health{}, err
	}
	if nothingDeployed(observed) {
		return runtime.Health{Healthy: false, Problems: []string{"no objects found for the target"}}, nil
	}

	statuses, err := r.serviceStatuses(ctx, nil, func(s swarm.Service) bool {
		return inScope(s.Spec.Labels, scope)
	})
	if err != nil {
		return runtime.Health{}, err
	}

	var problems []string
	for _, status := range statuses {
		for _, problem := range status.problems() {
			problems = append(problems, fmt.Sprintf("service %s: %s", status.Name, problem))
		}
	}
	return runtime.Health{Healthy: len(problems) == 0, Problems: problems}, nil
}

func nothingDeployed(observed runtime.Observed) bool {
	return len(observed.Services) == 0 && len(observed.Networks) == 0 && len(observed.Configs) == 0 &&
		len(observed.Secrets) == 0 && len(observed.Volumes) == 0
}
