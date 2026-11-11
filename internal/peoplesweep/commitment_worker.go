package peoplesweep

import (
	"context"
	"errors"
	"slices"
	"time"
)

// CommitmentStore supplies the bounded separately consented conversation lane.
type CommitmentStore interface {
	HasActivePersonCommitmentConsent(context.Context, string) (bool, error)
	PersonCommitmentsDue(context.Context, int64, time.Time, time.Duration, time.Duration) (bool, error)
	LoadPersonCommitmentWindow(context.Context, int64, ProviderProfile, CommitmentConfig, time.Time, string) (CommitmentWindow, error)
	EnsurePersonSweepWork(context.Context, int64, bool) (bool, error)
}

type CommitmentResult struct {
	Window CommitmentWindow
	Output ParsedCommitments
	Config CommitmentConfig
}

func (w *Worker) commitmentDue(ctx context.Context, personID int64, profile ProviderProfile, now time.Time) (bool, error) {
	if !w.Config.Commitments.Enabled || !profile.AllowSensitive || !slices.Contains(profile.AllowedSources, SourceConversationText) {
		return false, nil
	}
	store, ok := w.Store.(CommitmentStore)
	if !ok {
		return false, nil
	}
	active, err := store.HasActivePersonCommitmentConsent(ctx, profile.Fingerprint)
	if err != nil || !active {
		return false, err
	}
	return store.PersonCommitmentsDue(ctx, personID, now, w.Config.Commitments.MinInterval, w.Config.Commitments.Window)
}

func (w *Worker) runCommitmentCall(
	ctx context.Context, call briefCall, accounting *sweepCallAccounting,
	reservations *[]BudgetReservation,
) (*CommitmentResult, FailureClass, Lease, error) {
	lease := call.lease
	store, ok := w.Store.(CommitmentStore)
	if !ok {
		return nil, FailurePolicy, lease, nil
	}
	timezone := w.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	window, err := store.LoadPersonCommitmentWindow(ctx, lease.PersonID, call.profile, w.Config.Commitments, call.resolvedAt, timezone)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	if len(window.Sources) == 0 {
		return &CommitmentResult{Window: window, Config: w.Config.Commitments}, "", lease, nil
	}
	request, err := commitmentRequest(window, w.Config.Commitments.MaxOutputTokens)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	batch := PacketBatch{Ordinal: call.batchOrdinal, Request: request}

	prepared, err := w.Runner.PrepareStructured(ctx, batch.Request)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	estimate, err := EstimateWireTokenReservation(
		prepared.WireRequest(), batch.Request.MaxOutputTokens)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	estimatedCost, err := EstimateCostMicroUSD(estimate, w.Config.Budgets)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	execution, err := w.Runner.BeginStructuredExecution(ctx, prepared)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	preparedCall, err := execution.PrimaryCall(prepared)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}

	reservation, err := w.Store.ReservePersonSweepBudget(ctx, BudgetReservationRequest{
		RunID: call.runID, AttemptID: call.attemptID, BatchOrdinal: call.batchOrdinal,
		CallOrdinal: 0, Purpose: ProviderCallPurposeCommitment, PersonID: lease.PersonID,
		ProviderFingerprint: call.profile.Fingerprint,
		UTCDate:             call.resolvedAt.UTC().Format(time.DateOnly),
		InputHash:           prepared.WireSHA256(),
		ItemCount:           len(window.Sources),
		EstimatedRequests:   1, EstimatedInputTokens: estimate.InputTokens,
		EstimatedOutputTokens: estimate.OutputTokens, EstimatedCostMicroUSD: estimatedCost,
		Budget: w.Config.Budgets,
	})
	if err != nil {
		// Budget exhaustion defers commitment extraction to the next run; the reservation
		// rolled back, so there is nothing to reconcile.
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	*reservations = append(*reservations, reservation)

	current := sweepProviderCall{batch: batch, prepared: prepared, estimate: estimate,
		estimatedCost: estimatedCost, reservation: reservation, callOrdinal: 0,
		purpose: ProviderCallPurposeCommitment}
	for {
		renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
		if renewErr != nil {
			return nil, "", lease, renewErr
		}
		if renewed == nil {
			return nil, "", lease, ErrLeaseLost
		}
		lease = *renewed
		started := w.now()
		marked := false
		updated, response, runErr := w.runPreparedWithLeaseHeartbeat(ctx, lease,
			func(markCtx context.Context) error {
				if markErr := w.Store.MarkPersonSweepBudgetStarted(
					markCtx, current.reservation, lease); markErr != nil {
					return markErr
				}
				marked = true
				return nil
			}, preparedCall)
		lease = updated
		if !marked {
			if errors.Is(runErr, ErrPersonSweepConsentRevoked) {
				return nil, FailurePolicy, lease, nil
			}
			_ = w.Store.ReleasePersonSweepBudget(ctx, current.reservation)
			return nil, "", lease, briefMarkFailure(runErr)
		}
		latency := max(time.Duration(0), w.now().Sub(started))
		if structuredResponseCompleted(response) {
			if recordErr := accounting.record(current, response, latency); recordErr != nil {
				return nil, briefFailureClass(recordErr), lease, briefFatalError(ctx, recordErr)
			}
		}

		var failure *ValidationFailure
		if runErr != nil {
			if !errors.As(runErr, &failure) || current.callOrdinal != 0 || failure.repair {
				return nil, briefFailureClass(runErr), lease, briefFatalError(ctx, runErr)
			}
		} else {
			parsed, parseErr := ParseCommitments(response.Output, window)
			if parseErr == nil {
				return &CommitmentResult{Window: window, Output: parsed, Config: w.Config.Commitments}, "", lease, nil
			}

			if current.callOrdinal != 0 {
				return nil, FailureInvalidOutput, lease, nil
			}
			semanticFailure, semanticErr := execution.SemanticValidationFailure(response)
			if semanticErr != nil {
				return nil, briefFailureClass(semanticErr), lease, briefFatalError(ctx, semanticErr)
			}
			failure = &semanticFailure
		}

		repair, repairErr := execution.PrepareRepair(*failure)
		if repairErr != nil {
			return nil, briefFailureClass(repairErr), lease, briefFatalError(ctx, repairErr)
		}
		repairCall, repairErr := execution.RepairCall(repair)
		if repairErr != nil {
			return nil, briefFailureClass(repairErr), lease, briefFatalError(ctx, repairErr)
		}
		repairEstimate, estimateErr := EstimateWireTokenReservation(
			repair.WireRequest(), batch.Request.MaxOutputTokens)
		if estimateErr != nil {
			return nil, briefFailureClass(estimateErr), lease, briefFatalError(ctx, estimateErr)
		}
		repairCost, estimateErr := EstimateCostMicroUSD(repairEstimate, w.Config.Budgets)
		if estimateErr != nil {
			return nil, briefFailureClass(estimateErr), lease, briefFatalError(ctx, estimateErr)
		}
		repairReservation, reserveErr := w.Store.ReservePersonSweepBudget(ctx, BudgetReservationRequest{
			RunID: call.runID, AttemptID: call.attemptID, BatchOrdinal: call.batchOrdinal,
			CallOrdinal: 1, Purpose: ProviderCallPurposeCommitmentRepair, PersonID: lease.PersonID,
			ProviderFingerprint: call.profile.Fingerprint,
			UTCDate:             call.resolvedAt.UTC().Format(time.DateOnly),
			InputHash:           repair.WireSHA256(),
			ItemCount:           len(window.Sources),
			EstimatedRequests:   1, EstimatedInputTokens: repairEstimate.InputTokens,
			EstimatedOutputTokens: repairEstimate.OutputTokens,
			EstimatedCostMicroUSD: repairCost, Budget: w.Config.Budgets,
		})
		if reserveErr != nil {
			return nil, briefFailureClass(reserveErr), lease, briefFatalError(ctx, reserveErr)
		}
		*reservations = append(*reservations, repairReservation)
		current = sweepProviderCall{batch: batch, prepared: repair, estimate: repairEstimate,
			estimatedCost: repairCost, reservation: repairReservation, callOrdinal: 1,
			purpose: ProviderCallPurposeCommitmentRepair}
		preparedCall = repairCall
	}
}
