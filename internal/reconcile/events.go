package reconcile

// EventType names a kind of event in the events table.
type EventType string

// Event types written by Sync and Rollback.
const (
	EventSyncStarted       EventType = "sync.started"
	EventSyncSucceeded     EventType = "sync.succeeded"
	EventSyncFailed        EventType = "sync.failed"
	EventRollbackStarted   EventType = "rollback.started"
	EventRollbackSucceeded EventType = "rollback.succeeded"
	EventRollbackFailed    EventType = "rollback.failed"
)

// Stage names the step of a run that failed or started. It appears in the
// failure event payload and in the error text.
type Stage string

const (
	stageStart    Stage = "start"
	stageTarget   Stage = "target"
	stageRender   Stage = "render"
	stageRuntime  Stage = "runtime"
	stageInspect  Stage = "inspect"
	stageApply    Stage = "apply"
	stagePrune    Stage = "prune"
	stageHealth   Stage = "health"
	stageRevision Stage = "revision"
	stageStatus   Stage = "status"
)

// eventPayload is the JSON payload of a sync or rollback event.
type eventPayload struct {
	Message string `json:"message"`
	Stage   Stage  `json:"stage,omitempty"`
	Error   string `json:"error,omitempty"`
}
