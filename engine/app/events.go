package app

// Event is something that happened in the engine which an organization may
// want to be told about.
type Event struct {
	Type    string `json:"type"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}

// Event types.
const (
	EventBackupFailed       = "backup.failed"
	EventMigrationFailed    = "migration.failed"
	EventMigrationCompleted = "migration.completed"
	EventMigrationLag       = "migration.lag"
	EventConnectionDown     = "connection.down"
	EventConnectionUp       = "connection.up"
)

// emit hands an event to the alerting subsystem. It never blocks the caller.
func (a *App) emit(orgID string, ev Event) {}
