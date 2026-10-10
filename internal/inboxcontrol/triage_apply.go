package inboxcontrol

import (
	"context"

	"go.kenn.io/msgvault/internal/emailtags"
)

// ApplyTriage returns results in proposal order, including durable receipts when
// a later item fails. Empty entries have not been dispatched. The complete signed
// envelope is authenticated before looking up receipts; replay requires current
// authority but does not require a fresh preview or live provider access.
func (s *Service) ApplyTriage(ctx context.Context, proposal TriageProposal, principal Principal, gate func(context.Context) (func(), error)) ([]Result, error) {
	// IssuedAt is itself authenticated. Verifying at that instant checks signature,
	// caller, complete envelope and bounded TTL without expiring durable replay.
	if err := VerifyTriageProposal(s.Key, proposal, principal, proposal.IssuedAt); err != nil {
		return nil, err
	}
	if s.Ledger == nil {
		return nil, ErrUnavailable
	}
	results := make([]Result, len(proposal.Items))
	targets := make([]Target, len(proposal.Items))
	pending := false
	var replayError error
	for i, item := range proposal.Items {
		targets[i] = *item.Request.Target
		if err := s.authorize(ctx, principal, item.Request); err != nil {
			return results, err
		}
		intentHash, err := triageItemIntentHash(item)
		if err != nil {
			return results, err
		}
		receipt, err := s.Ledger.LookupInboxReceipt(ctx, principal.ID, proposal.Source.SourceID, item.Request.IdempotencyKey)
		if err != nil {
			return results, ErrInternal
		}
		if receipt == nil {
			pending = true
			continue
		}
		result, err := s.replay(ctx, receipt, intentHash, principal)
		if err != nil {
			return results, err
		}
		results[i] = *result
		if err := triageReceiptOutcome(receipt.Status); err != nil && replayError == nil {
			replayError = err
		}
	}
	// An unknown/failed/reconcile-only dispatch is never retried, including after
	// process restart. Keep all authorized receipts visible to the caller.
	if replayError != nil {
		return results, replayError
	}
	if !pending {
		return results, nil
	}
	if err := VerifyTriageProposal(s.Key, proposal, principal, s.now()); err != nil {
		return results, err
	}
	store, ok := s.Ledger.(TriageSnapshotStore)
	evidence, hasEvidence := s.Ledger.(TriageEvidenceStore)
	if !ok || !hasEvidence || s.Resolve == nil {
		return results, ErrUnavailable
	}
	done, err := s.acquire(ctx, proposal.Source.SourceID, gate)
	if err != nil {
		return results, err
	}
	defer done()
	// Receipt claims may have completed while waiting for the source lease. Re-read
	// every key inside that lease before validating the original archive revision.
	for i, item := range proposal.Items {
		if err := s.authorize(ctx, principal, item.Request); err != nil {
			return results, err
		}
		receipt, err := s.Ledger.LookupInboxReceipt(ctx, principal.ID, proposal.Source.SourceID, item.Request.IdempotencyKey)
		if err != nil {
			return results, ErrInternal
		}
		if receipt == nil {
			continue
		}
		hash, err := triageItemIntentHash(item)
		if err != nil {
			return results, err
		}
		result, err := s.replay(ctx, receipt, hash, principal)
		if err != nil {
			return results, err
		}
		results[i] = *result
		if err := triageReceiptOutcome(receipt.Status); err != nil && replayError == nil {
			replayError = err
		}
	}
	if replayError != nil {
		return results, replayError
	}
	pending = false
	for _, result := range results {
		pending = pending || result.Receipt == nil
	}
	if !pending {
		return results, nil
	}
	baseline, err := store.InboxTriageSnapshot(ctx, proposal.Source, targets)
	if err != nil {
		return results, safePreflightError(err)
	}
	if baseline == nil || baseline.Source != proposal.Source || len(baseline.Candidates) != len(targets) || baseline.MappingRevision != proposal.MappingRevision || baseline.ArchiveRevision != proposal.ArchiveRevision || baseline.IncomingWatermark != proposal.IncomingWatermark {
		return results, ErrPlanChanged
	}
	for i, item := range proposal.Items {
		current, err := triageArchiveFingerprint(baseline.Candidates[i].State)
		if err != nil {
			return results, ErrUnavailable
		}
		expected, err := triageArchiveFingerprint(*item.Request.Expected)
		if err != nil || current != expected {
			return results, ErrPlanChanged
		}
	}
	// Reuse Control unchanged inside the already-held exact source lease. Its
	// receipt claim, native observation, token check, dispatch and readback remain
	// the only write path. These scoped no-op acquisitions cannot escape this call.
	inner := *s
	held := true
	defer func() { held = false }()
	heldGate := func(ctx context.Context) (func(), error) {
		if !held {
			return nil, ErrInternal
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	inner.AcquireSource = func(ctx context.Context, id int64) (func(), error) {
		if id != proposal.Source.SourceID {
			return nil, ErrDenied
		}
		return heldGate(ctx)
	}
	for i, item := range proposal.Items {
		if results[i].Receipt != nil {
			continue
		}
		// Keep freshness errors distinct from authorization failures, and close
		// native catalog/archive read races after Control's live observation.
		guard := func(ctx context.Context, request Request) error {
			if request.Operation != OpTags || request.Target == nil || *request.Target != *item.Request.Target {
				return ErrDenied
			}
			return s.checkTriageApplyGuards(ctx, proposal, baseline, targets, item, principal, store, evidence)
		}
		inner.beforeClaim = guard
		if err := guard(ctx, item.Request); err != nil {
			return results, err
		}

		request := item.Request
		hash, err := triageItemIntentHash(item)
		if err != nil {
			return results, err
		}
		stateHash, err := planFingerprint(*request.Expected, item.Projected)
		if err != nil {
			return results, err
		}
		request.PreviewToken, err = SignPreview(s.Key, PreviewClaims{PrincipalID: principal.ID, IntentHash: hash, StateHash: stateHash, IssuedAt: proposal.IssuedAt, ExpiresAt: proposal.ExpiresAt})
		if err != nil {
			return results, err
		}
		result, err := inner.Control(ctx, request, principal, heldGate)
		if result != nil {
			results[i] = *result
		}
		if err != nil {
			return results, err
		}
		if result == nil || result.Receipt == nil || result.After == nil || result.Receipt.Status != StatusVerified {
			return results, ErrOutcomeUnknown
		}
		// Reconciliation legitimately changes the source archive revision. Advance
		// our baseline only after proving unchanged mapping/arrivals/other targets and
		// the exact independently verified observation for this item. Sync remains
		// excluded by the source lease throughout the batch.
		current, err := store.InboxTriageSnapshot(ctx, proposal.Source, targets)
		if err != nil {
			return results, safePreflightError(err)
		}
		if current == nil || current.Source != baseline.Source || current.MappingRevision != baseline.MappingRevision || current.IncomingWatermark != baseline.IncomingWatermark || len(current.Candidates) != len(targets) {
			return results, ErrPlanChanged
		}
		for j, candidate := range current.Candidates {
			expected := baseline.Candidates[j].State
			if j == i {
				expected = *result.After
			}
			want, err := SemanticFingerprint(expected)
			if err != nil {
				return results, ErrUnavailable
			}
			got, err := SemanticFingerprint(candidate.State)
			if err != nil || want != got {
				return results, ErrPlanChanged
			}
		}
		baseline = current
	}
	return results, nil
}

func triageItemIntentHash(item TriageProposalItem) (string, error) {
	intent := item.Request
	intent.DryRun, intent.Expected, intent.PreviewToken, intent.IdempotencyKey = true, nil, "", ""
	return IntentFingerprint(intent)
}

func triageReceiptOutcome(status Status) error {
	switch status {
	case StatusVerified:
		return nil
	case StatusFailed:
		return ErrNoWrite
	case StatusReconcileOnly:
		return ErrReconcileOnly
	default:
		return ErrOutcomeUnknown
	}
}

func (s *Service) checkTriageApplyGuards(ctx context.Context, proposal TriageProposal, baseline *TriageSnapshot, targets []Target, item TriageProposalItem, principal Principal, store TriageSnapshotStore, evidence TriageEvidenceStore) error {
	if err := VerifyTriageProposal(s.Key, proposal, principal, s.now()); err != nil {
		return err
	}
	sourceRequest := Request{Operation: OpGetCapabilities, Source: &proposal.Source, DryRun: true}
	if err := s.authorize(ctx, principal, sourceRequest); err != nil {
		return err
	}
	if err := evidence.ValidateInboxTriageEvidence(ctx, *item.Request.Target, item.EvidenceMessageIDs); err != nil {
		return safePreflightError(err)
	}
	tags, err := s.triageNativeCatalog(ctx, proposal.Source, principal)
	if err != nil {
		return err
	}
	for _, id := range item.Request.Tags.Add {
		matches := 0
		for _, tag := range tags {
			if emailtags.Contains([]string{tag.ID}, id, proposal.Source.SourceType == sourceTypeIMAP) {
				matches++
			}
		}
		if matches != 1 {
			return ErrDenied
		}
	}
	current, err := store.InboxTriageSnapshot(ctx, proposal.Source, targets)
	if err != nil {
		return safePreflightError(err)
	}
	if current == nil || current.Source != baseline.Source || current.MappingRevision != baseline.MappingRevision || current.ArchiveRevision != baseline.ArchiveRevision || current.IncomingWatermark != baseline.IncomingWatermark || len(current.Candidates) != len(targets) {
		return ErrPlanChanged
	}
	for i, candidate := range current.Candidates {
		want, err := SemanticFingerprint(baseline.Candidates[i].State)
		if err != nil {
			return ErrUnavailable
		}
		got, err := SemanticFingerprint(candidate.State)
		if err != nil || want != got {
			return ErrPlanChanged
		}
	}
	if err := s.authorize(ctx, principal, sourceRequest); err != nil {
		return err
	}
	// Catalog and Store reads can outlive the preview. Expiry is checked again
	// at the end of preflight, before Control commits a dispatch claim.
	return VerifyTriageProposal(s.Key, proposal, principal, s.now())
}
