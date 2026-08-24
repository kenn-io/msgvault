package document

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestStatusServiceInspectsAndResetsFailures(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	wantStatus := store.DocumentVectorGenerationStatus{
		GenerationID: 7, State: store.DocumentVectorGenerationBuilding,
		Pending: 2, Retryable: 1, Terminal: 3, ReadyLive: 4, Obsolete: 5, CleanupPending: 2,
	}
	wantReset := store.DocumentVectorFailureResetResult{Scanned: 3, Reset: 2, Exhausted: true}
	ledger := &fakeDocumentVectorStatusLedger{status: wantStatus, reset: wantReset}
	service := NewStatusService(ledger)

	status, err := service.Inspect(t.Context(), 7, "", 25)
	requirements.NoError(err)
	assertions.Equal(wantStatus, status)
	resetAt := time.Date(2026, time.August, 20, 23, 30, 0, 987654000, time.UTC)
	reset, err := service.ResetFailures(t.Context(), 7, "", 25, resetAt)
	requirements.NoError(err)
	assertions.Equal(wantReset, reset)
	assertions.Equal(resetAt, ledger.resetAt)
}

type fakeDocumentVectorStatusLedger struct {
	status  store.DocumentVectorGenerationStatus
	reset   store.DocumentVectorFailureResetResult
	resetAt time.Time
}

func (l *fakeDocumentVectorStatusLedger) GetDocumentVectorGenerationStatus(context.Context, int64, string, int) (store.DocumentVectorGenerationStatus, error) {
	return l.status, nil
}

func (l *fakeDocumentVectorStatusLedger) ResetDocumentVectorFailures(_ context.Context, _ int64, _ string, _ int, now time.Time) (store.DocumentVectorFailureResetResult, error) {
	l.resetAt = now
	return l.reset, nil
}
