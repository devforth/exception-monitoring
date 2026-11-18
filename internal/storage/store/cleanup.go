package store

import "time"

// Statuses of a cleanup run.
const (
	CleanupRunning = "running"
	CleanupDone    = "done"
	CleanupFailed  = "failed"
)

// CleanupRun is one cleanup by size: what it aims at, where it is and what
// it did. The steps are those of docs/configuration.md (Retention).
type CleanupRun struct {
	Sequence   uint64        `json:"seq"`
	Status     string        `json:"status"`
	StartedAt  time.Time     `json:"started_at"`
	Duration   time.Duration `json:"duration"`
	Limit      int64         `json:"limit"`
	SizeBefore int64         `json:"size_before"` // on disk
	Compacted  int64         `json:"compacted"`   // on disk once compacted, before deleting
	SizeAfter  int64         `json:"size_after"`
	Goal       int64         `json:"goal"`               // of the byte counters
	Reached    int           `json:"reached"`            // the step it is at; once done, the last one that ran
	Position   []byte        `json:"position,omitempty"` // where that step goes on
	Steps      []CleanupStep `json:"steps"`
	Error      string        `json:"error,omitempty"`
}

// CleanupStep is a step that deleted something: bytes freed (before
// compression) and what went, per project.
type CleanupStep struct {
	Number   int                    `json:"number"` // as in docs/configuration.md
	Name     string                 `json:"name"`
	Freed    int64                  `json:"freed"`
	Duration time.Duration          `json:"duration"`
	Projects map[int]*CleanupCounts `json:"projects"`
}

// CleanupCounts is what a step did to one project.
type CleanupCounts struct {
	Logs    int `json:"logs,omitempty"`
	Events  int `json:"events,omitempty"`
	Thinned int `json:"thinned,omitempty"`
	Issues  int `json:"issues,omitempty"`
}
