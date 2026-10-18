package api

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
)

func TestSuccessfulSyncNeverProvesProviderCompleteness(t *testing.T) {
	for _, count := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		evidence := sourceProviderIngestion(&SyncRunStatus{Status: store.SyncStatusCompleted, ErrorsCount: count, MessagesProcessed: count, MessagesAdded: count, MessagesUpdated: count})
		assert.Contains(t, []string{"unknown", "partial"}, evidence.Status)
	}
}

func FuzzSuccessfulSyncNeverProvesProviderCompleteness(f *testing.F) {
	f.Add(int64(0), int64(1), int64(1), int64(0), "", "")
	f.Add(int64(math.MaxInt64), int64(math.MinInt64), int64(-1), int64(math.MaxInt64), "\x00", "malformed")
	f.Fuzz(func(t *testing.T, errors, processed, added, updated int64, started, completed string) {
		evidence := sourceProviderIngestion(&SyncRunStatus{Status: store.SyncStatusCompleted, ErrorsCount: errors, MessagesProcessed: processed, MessagesAdded: added, MessagesUpdated: updated, StartedAt: started, CompletedAt: &completed})
		assert.Contains(t, []string{"unknown", "partial"}, evidence.Status)
	})
}

func TestReportedSyncErrorsRemainPartialAcrossRunMetadata(t *testing.T) {
	for _, status := range []string{"", store.SyncStatusRunning, store.SyncStatusCompleted, store.SyncStatusFailed, "unknown-status"} {
		assert.Equal(t, "partial", sourceProviderIngestion(&SyncRunStatus{Status: status, ErrorsCount: 1}).Status)
	}
}

func FuzzReportedSyncErrorsRemainPartialAcrossRunMetadata(f *testing.F) {
	f.Add(uint64(0), "completed", int64(0), "")
	f.Add(uint64(math.MaxUint64), "running", int64(math.MinInt64), "\x00")
	f.Fuzz(func(t *testing.T, positiveErrors uint64, status string, processed int64, completed string) {
		// Construct the complete positive int64 domain required by this property.
		errors := int64(positiveErrors%math.MaxInt64) + 1
		assert.Equal(t, "partial", sourceProviderIngestion(&SyncRunStatus{Status: status, ErrorsCount: errors, MessagesProcessed: processed, CompletedAt: &completed}).Status)
	})
}

func FuzzRecordedItemErrorsRemainPartialWithoutAggregateCounter(f *testing.F) {
	f.Add("completed", "", "", int64(0))
	f.Add("running", "\x00", "synthetic parser refusal", int64(math.MaxInt64))
	f.Fuzz(func(t *testing.T, status, kind, message string, added int64) {
		assert.Equal(t, "partial", sourceProviderIngestion(&SyncRunStatus{Status: status, ItemErrors: []SyncRunItemStatus{{ErrorKind: kind, ErrorMessage: message}}, MessagesAdded: added}).Status)
	})
}
