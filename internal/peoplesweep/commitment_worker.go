package peoplesweep

import (
	"context"
	"encoding/json/jsontext"
	"slices"
	"time"
)

// CommitmentStore supplies the bounded separately consented conversation lane.
type CommitmentStore interface {
	HasActivePersonCommitmentConsent(ctx context.Context, fingerprint string) (bool, error)
	PersonCommitmentsDue(ctx context.Context, personID int64, now time.Time, interval, window time.Duration, scopeFingerprint string) (bool, error)
	LoadPersonCommitmentWindow(ctx context.Context, personID int64, profile ProviderProfile, cfg CommitmentConfig, now time.Time, timezone string) (CommitmentWindow, error)
	EnsurePersonSweepWork(ctx context.Context, personID int64, force bool) (bool, error)
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
	return store.PersonCommitmentsDue(ctx, personID, now, w.Config.Commitments.MinInterval, w.Config.Commitments.Window, CommitmentScopeFingerprint(profile, w.Config.Commitments))
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

	var result *CommitmentResult
	parse := func(output jsontext.Value) (FailureClass, error) {
		parsed, err := ParseCommitments(output, window)
		if err != nil {
			return "", err
		}
		result = &CommitmentResult{Window: window, Output: parsed, Config: w.Config.Commitments}
		return "", nil
	}
	class, lease, err := w.runOptionalCall(ctx, call, batch, len(window.Sources), ProviderCallPurposeCommitment, ProviderCallPurposeCommitmentRepair, parse, accounting, reservations)
	return result, class, lease, err
}
