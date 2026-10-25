package desired

import "time"

// Defaults shared by the manifest renderer and runtime readers, so desired and
// observed state cannot drift on values Swarm fills in implicitly.
const (
	DefaultReplicas     uint64 = 1
	DefaultEndpointMode        = "vip"
)

// DefaultRestartPolicy returns Swarm's restart defaults.
func DefaultRestartPolicy() RestartPolicy {
	return RestartPolicy{Condition: "any", Delay: 5 * time.Second}
}

// DefaultUpdateConfig returns Swarm's update defaults.
func DefaultUpdateConfig() UpdateConfig {
	return UpdateConfig{
		Parallelism:   1,
		FailureAction: "pause",
		Monitor:       5 * time.Second,
		Order:         "stop-first",
	}
}

// DefaultHealthcheck returns Swarm's healthcheck defaults.
func DefaultHealthcheck() Healthcheck {
	return Healthcheck{
		Interval: 30 * time.Second,
		Timeout:  30 * time.Second,
		Retries:  3,
	}
}
