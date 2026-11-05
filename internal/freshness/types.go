// Package freshness reports source currency for source inventory and timeline views.
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
	SyncCommand          string     `json:"sync_command"`
}

type Report struct {
	AsOf              time.Time         `json:"as_of"`
	StaleAfterSeconds int64             `json:"stale_after_seconds"`
	Warnings          []string          `json:"warnings"`
	Sources           []SourceFreshness `json:"sources"`
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

type Inputs struct {
	Sources    []SourceInventoryItem
	StaleAfter time.Duration
	Now        time.Time
}
