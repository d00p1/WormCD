package reconcile

// Event types written to the events table by Sync and Rollback.
const (
	EventSyncStarted       = "sync.started"
	EventSyncSucceeded     = "sync.succeeded"
	EventSyncFailed        = "sync.failed"
	EventRollbackStarted   = "rollback.started"
	EventRollbackSucceeded = "rollback.succeeded"
	EventRollbackFailed    = "rollback.failed"
)

// eventPayload is the JSON payload of a sync or rollback event. Stage names the
// step that failed (render, runtime, inspect, apply, prune, health, revision).
type eventPayload struct {
	Message string `json:"message"`
	Stage   string `json:"stage,omitempty"`
	Error   string `json:"error,omitempty"`
}
