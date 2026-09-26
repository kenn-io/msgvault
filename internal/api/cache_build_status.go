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
// Published means the check completed with a usable cache; it may reuse an
// unchanged publication. The daemon retains the last 100 completed jobs.
type CacheBuildStatus struct {
	JobID      string     `json:"job_id"`
	Status     string     `json:"status"`
	AcceptedAt time.Time  `json:"accepted_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// CacheBuildAccepted is returned when a query must wait for a cache check
// before it can return rows.
type CacheBuildAccepted struct {
	Status string                `json:"status"`
	JobID  string                `json:"job_id"`
	Cache  *query.CacheFreshness `json:"cache,omitempty"`
}
