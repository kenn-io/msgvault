//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package api

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

// Hegel embeds its engine on these platforms. The native HTTP and Store tests
// exercise the same contracts on every platform, including Intel Macs.
func TestSuccessfulSyncNeverProvesProviderCompleteness(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		run := &SyncRunStatus{
			Status:            store.SyncStatusCompleted,
			ErrorsCount:       hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)),
			MessagesProcessed: hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)),
			MessagesAdded:     hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)),
			MessagesUpdated:   hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)),
			StartedAt:         hegel.Draw(ht, hegel.Text()),
			CompletedAt:       new(hegel.Draw(ht, hegel.Text())),
		}
		evidence := sourceProviderIngestion(run)
		assert.Contains(ht, []string{"unknown", "partial"}, evidence.Status)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestReportedSyncErrorsRemainPartialAcrossRunMetadata(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		run := &SyncRunStatus{
			Status:            hegel.Draw(ht, hegel.Text()),
			ErrorsCount:       hegel.Draw(ht, hegel.Integers[int64](1, math.MaxInt64)),
			MessagesProcessed: hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)),
			CompletedAt:       new(hegel.Draw(ht, hegel.Text())),
		}
		evidence := sourceProviderIngestion(run)
		assert.Equal(ht, "partial", evidence.Status)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestRecordedItemErrorsRemainPartialWithoutAggregateCounter(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		run := &SyncRunStatus{
			Status:        hegel.Draw(ht, hegel.Text()),
			ItemErrors:    []SyncRunItemStatus{{ErrorKind: hegel.Draw(ht, hegel.Text()), ErrorMessage: hegel.Draw(ht, hegel.Text())}},
			MessagesAdded: hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)),
		}
		assert.Equal(ht, "partial", sourceProviderIngestion(run).Status)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
