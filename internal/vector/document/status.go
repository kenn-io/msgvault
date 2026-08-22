package document

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type FailureDiagnostic = store.DocumentVectorFailureDiagnostic
type GenerationStatus = store.DocumentVectorGenerationStatus
type FailureResetResult = store.DocumentVectorFailureResetResult

// StatusLedger is the authoritative inspection and manual-recovery seam.
type StatusLedger interface {
	GetDocumentVectorGenerationStatus(ctx context.Context, generationID int64, afterToken string, limit int) (store.DocumentVectorGenerationStatus, error)
	ResetDocumentVectorFailures(ctx context.Context, generationID int64, afterToken string, limit int, now time.Time) (store.DocumentVectorFailureResetResult, error)
}

var _ StatusLedger = (*store.Store)(nil)

// StatusService exposes bounded, non-PII generation diagnostics and explicit
// failed-publication recovery without exposing store implementation details.
type StatusService struct{ ledger StatusLedger }

func NewStatusService(ledger StatusLedger) *StatusService { return &StatusService{ledger: ledger} }

func (s *StatusService) Inspect(
	ctx context.Context, generationID GenerationID, afterToken string, limit int,
) (GenerationStatus, error) {
	if s == nil || s.ledger == nil {
		return GenerationStatus{}, errors.New("document vector status ledger is required")
	}
	return s.ledger.GetDocumentVectorGenerationStatus(ctx, int64(generationID), afterToken, limit)
}

func (s *StatusService) ResetFailures(
	ctx context.Context, generationID GenerationID, afterToken string, limit int, now time.Time,
) (FailureResetResult, error) {
	if s == nil || s.ledger == nil {
		return FailureResetResult{}, errors.New("document vector status ledger is required")
	}
	return s.ledger.ResetDocumentVectorFailures(ctx, int64(generationID), afterToken, limit, now)
}
