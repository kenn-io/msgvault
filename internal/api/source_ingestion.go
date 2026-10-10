package api

import (
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

// sourceProviderIngestion reports evidence of gaps, never provider completeness.
// LatestSync includes hydrated per-item errors; a completed run can contain
// rejected items even when its aggregate error counter is zero.
func sourceProviderIngestion(latest *SyncRunStatus) *inboxcontrol.ProviderIngestion {
	result := inboxcontrol.UnknownProviderIngestion()
	if latest == nil {
		return result
	}
	if latest.Status == store.SyncStatusFailed || latest.ErrorsCount > 0 || len(latest.ItemErrors) > 0 {
		result.Status, result.Reason = "partial", "sync_reported_errors"
	} else if latest.Status == store.SyncStatusRunning {
		result.Reason = "sync_in_progress"
	}
	return result
}
