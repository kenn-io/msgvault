// Package freshness reports source and projection currency without implying
// that a recently run job has applied every archive change. Its JSON contract
// is shared by loops and person context surfaces.
package freshness

import "time"

type SourceFreshness struct {
	SourceID             *int64     `json:"source_id"`
	SourceType           string     `json:"source_type"`
	Identifier           string     `json:"identifier"`
	Scheduling           string     `json:"scheduling"`
	LastSuccessfulSyncAt *time.Time `json:"last_successful_sync_at"`
	LastSyncErrors       int64      `json:"last_sync_errors"`
	Outcome              string     `json:"outcome"`
	Reason               string     `json:"reason,omitempty"`
	AgeSeconds           *int64     `json:"age_seconds"`
	Stale                bool       `json:"stale"`
	SyncCommand          string     `json:"sync_command"`
}

// ProjectionFreshness.ComputedAt is the certified archive frontier, never the
// wall-clock end of a pass which may have left queued work behind.
type ProjectionFreshness struct {
	ComputedAt       *time.Time     `json:"computed_at"`
	LastSuccessAt    *time.Time     `json:"last_success_at,omitempty"`
	AgeSeconds       *int64         `json:"age_seconds"`
	Stale            bool           `json:"stale"`
	BackfillComplete *bool          `json:"backfill_complete,omitempty"`
	Extra            map[string]any `json:"extra,omitempty"`
}

type Report struct {
	AsOf              time.Time                       `json:"as_of"`
	StaleAfterSeconds int64                           `json:"stale_after_seconds"`
	Stale             bool                            `json:"stale"`
	StaleReasons      []string                        `json:"stale_reasons"`
	Warnings          []string                        `json:"warnings"`
	Sources           []SourceFreshness               `json:"sources"`
	Projections       map[string]*ProjectionFreshness `json:"projections"`
}

// SourceInventoryItem is factual input: a configured account can have no
// SourceID or completed sync yet. Scheduling is scheduled, manual, static or
// disabled; Outcome describes the latest run independently of last success.
type SourceInventoryItem struct {
	SourceID                           *int64
	SourceType, Identifier, Scheduling string
	LastSuccessfulSyncAt               *time.Time
	LastSyncErrors                     int64
	Outcome, SyncCommand               string
}

type ProjectionState struct {
	ComputedAt, LastSuccessAt *time.Time
	BackfillComplete          *bool
	Extra                     map[string]any
}

type Inputs struct {
	Sources     []SourceInventoryItem
	Projections map[string]ProjectionState
	StaleAfter  time.Duration
	Now         time.Time
}
