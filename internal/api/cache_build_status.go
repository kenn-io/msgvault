package api

import (
	"time"

	"go.kenn.io/msgvault/internal/query"
)

const (
	CacheBuildQueued    = "queued"
	CacheBuildRunning   = "running"
	CacheBuildPublished = "published"
	CacheBuildFailed    = "failed"
)

// CacheBuildStatus is the status of a daemon-owned analytics cache refresh.
type CacheBuildStatus struct {
	JobID      string     `json:"job_id"`
	Status     string     `json:"status"`
	AcceptedAt time.Time  `json:"accepted_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// CacheBuildAccepted is returned when a query must wait for a new publication
// before it can return rows from the requested generation.
type CacheBuildAccepted struct {
	Status string                `json:"status"`
	JobID  string                `json:"job_id"`
	Cache  *query.CacheFreshness `json:"cache,omitempty"`
}
