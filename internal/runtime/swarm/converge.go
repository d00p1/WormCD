package swarm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Defaults for the convergence wait: Apply polls a service every
// defaultConvergeInterval and gives up after defaultConvergeTimeout.
const (
	defaultConvergeInterval = 2 * time.Second
	defaultConvergeTimeout  = 10 * time.Minute
)

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
		if failures := status.failures(); len(failures) > 0 {
			return fmt.Errorf("service %s: %s", name, strings.Join(failures, "; "))
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("service %s: not converged (%s): %w", name, pending, ctx.Err())
		case <-time.After(r.converge.Interval):
		}
	}
}
