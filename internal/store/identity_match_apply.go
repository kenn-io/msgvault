package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// ErrIdentityMatchEndpointUnsupported reports that a candidate cannot be
// applied automatically because its endpoints are not two participants.
// Participant-to-person and observation-to-contact-point candidates are
// representable and reviewable, but binding a participant to a curated person
// is a different write path than the participant link forest, and this PR
// deliberately does not automate it.
var ErrIdentityMatchEndpointUnsupported = errors.New(
	"identity match endpoint kind cannot be applied automatically")

// ErrIdentityMatchNotAccepted reports that an accepted-match recovery read
// became stale before its participant-link transaction acquired the shared
// identity mutation lock.
var ErrIdentityMatchNotAccepted = errors.New(
	"identity match candidate is no longer accepted")

var (
	errIdentityMatchSnapshotStale      = errors.New("identity match candidate snapshot is stale")
	errIdentityMatchEndpointsCollapsed = errors.New("identity match candidate endpoints collapsed")
	errIdentityMatchAlreadyConnected   = errors.New("identity match candidate endpoints are already connected")
)

// GetIdentityMatchCandidateContext loads one candidate with its evidence.
// Callers use it to read existing evidence before adding more (PR 3's
// AddIdentityMatchEvidence is insert-only, so an unguarded writer would
// duplicate evidence on every re-import) and to report a decision's outcome.
func (s *Store) GetIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64,
) (*IdentityMatchCandidate, error) {
	candidates, err := s.identityMatchCandidatesByIDContext(ctx, []int64{candidateID})
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("candidate %d: %w", candidateID, ErrIdentityMatchNotFound)
	}
	return &candidates[0], nil
}

// AcceptIdentityMatchCandidateContext accepts a candidate and applies it.
//
// The acceptance POLICY lives in DecideIdentityMatchCandidateContext: a
// stable provider/Beeper ID basis may be accepted by any actor, every other
// basis only by decidedBy == "user". This function adds the APPLICATION step
// PR 3 deliberately left out, and returns the identity revision after it.
//
// Accept and link are two transactions. The link transaction uses the same
// identity-mutation row lock, link-forest connectivity check, and curated
// person validation as LinkParticipants, with the caller's context on every
// statement. That is resumable rather than atomic on purpose: a crash between
// them leaves an accepted candidate with no link, and
// ApplyAcceptedIdentityMatchesContext finishes the job on the next import.
//
// ErrAlreadyLinked means the pair is already connected, which is success:
// re-accepting is idempotent. ErrPersonBindingConflict means the two clusters
// are curated as different durable people; the candidate is recorded as a
// conflict for review and the error is returned. Neither ever merges profiles.
func (s *Store) AcceptIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64, decidedBy string, notes *string,
) (*IdentityMatchCandidate, int64, error) {
	candidate, err := s.GetIdentityMatchCandidateContext(ctx, candidateID)
	if err != nil {
		return nil, 0, err
	}
	if candidate.LeftKind == IdentityMatchCardDAVResource &&
		candidate.RightKind == IdentityMatchPerson {
		return s.acceptCardDAVIdentityMatchCandidateContext(
			ctx, candidateID, decidedBy, notes,
		)
	}
	if candidate.LeftKind != IdentityMatchParticipant || candidate.RightKind != IdentityMatchParticipant {
		return nil, 0, fmt.Errorf("candidate %d (%s to %s): %w",
			candidateID, candidate.LeftKind, candidate.RightKind,
			ErrIdentityMatchEndpointUnsupported)
	}

	accepted := candidate
	beforeTransition := candidate
	transitioned := false
	if candidate.State != IdentityMatchStateAccepted ||
		(decidedBy == string(ProvenanceUser) &&
			(candidate.DecidedBy == nil || *candidate.DecidedBy != string(ProvenanceUser))) {
		if s.identityMatchAcceptBeforeDecisionHook != nil {
			s.identityMatchAcceptBeforeDecisionHook()
		}
		accepted, beforeTransition, err = s.decideIdentityMatchCandidateContext(
			ctx, candidateID, IdentityMatchStateAccepted, decidedBy, notes)
		if err != nil {
			return nil, 0, err
		}
		transitioned = true
	}

	applied, revision, _, err := s.applyAcceptedIdentityMatchCandidateContext(
		ctx, accepted, decidedBy)
	if err != nil {
		if transitioned && decidedBy == string(ProvenanceUser) &&
			errors.Is(err, ErrPersonBindingConflict) {
			if restoreErr := s.restoreIdentityMatchDecisionAfterBindingConflictContext(
				ctx, beforeTransition, accepted,
			); restoreErr != nil {
				return nil, 0, errors.Join(err, restoreErr)
			}
		}
		return nil, 0, err
	}
	return applied, revision, nil
}

// restoreIdentityMatchDecisionAfterBindingConflictContext makes the user
// accept path compare-and-set from the caller's perspective. If a person
// binding appeared between an API preflight and application, the merge offer
// must not consume the candidate. Recovery of a previously accepted pending
// decision still records a conflict through the normal resume path.
func (s *Store) restoreIdentityMatchDecisionAfterBindingConflictContext(
	ctx context.Context, before, accepted *IdentityMatchCandidate,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if accepted.DecidedAt == nil {
			return errors.New("restore identity match decision: accepted decision has no timestamp")
		}
		result, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
			state = ?, decided_by = ?, decided_at = ?, notes = ?,
			application_pending = ?, observation_conflict_origin = ?,
			pre_conflict_state = ?,
			updated_at = ?
			WHERE id = ? AND state = 'conflict' AND application_pending = FALSE
			  AND notes = ?`,
			before.State, before.DecidedBy, before.DecidedAt, before.Notes,
			before.applicationPending, before.conflictState.observationOrigin,
			before.conflictState.preConflictState, before.UpdatedAt, before.ID,
			"accepted match spans two durable person profiles; not applied",
		)
		if err != nil {
			return fmt.Errorf("restore identity match decision after binding conflict: %w", err)
		}
		if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
			return fmt.Errorf("count restored identity match decision: %w", rowsErr)
		} else if changed != 1 {
			return errors.New("restore identity match decision: candidate changed concurrently")
		}
		return nil
	})
}

// ResumeAcceptedIdentityMatchCandidateContext completes the link half of one
// already-accepted candidate without rewriting its original decision fields.
// The boolean reports whether this call inserted a new participant link.
func (s *Store) ResumeAcceptedIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64,
) (*IdentityMatchCandidate, int64, bool, error) {
	candidate, err := s.GetIdentityMatchCandidateContext(ctx, candidateID)
	if err != nil {
		return nil, 0, false, err
	}
	if candidate.State != IdentityMatchStateAccepted {
		return nil, 0, false, ErrInvalidIdentityMatchState
	}
	if !candidate.applicationPending {
		revision, err := readIdentityRevisionContext(ctx, s.db)
		if err != nil {
			return nil, 0, false, err
		}
		return candidate, revision, false, nil
	}
	decidedBy := "system"
	if candidate.DecidedBy != nil {
		decidedBy = *candidate.DecidedBy
	}
	applied, revision, linked, err := s.applyAcceptedIdentityMatchCandidateContext(
		ctx, candidate, decidedBy)
	if err != nil {
		return nil, 0, false, err
	}
	return applied, revision, linked, nil
}

func (s *Store) applyAcceptedIdentityMatchCandidateContext(
	ctx context.Context, accepted *IdentityMatchCandidate, decidedBy string,
) (*IdentityMatchCandidate, int64, bool, error) {
	current := *accepted
	var revision int64
	var linked bool
	var err error
	for {
		var refreshed *IdentityMatchCandidate
		revision, linked, err = s.linkParticipantsContextGuardedOwned(
			ctx, current.LeftID, current.RightID,
			current.ID,
			func(ctx context.Context, tx *loggedTx) error {
				loaded, loadErr := getIdentityMatchCandidateWithoutEvidenceTx(ctx, tx, current.ID)
				if errors.Is(loadErr, ErrIdentityMatchNotFound) {
					survivingID, collapsed, found, redirectErr :=
						identityMatchCandidateRedirectTx(ctx, tx, current.ID)
					if redirectErr != nil {
						return redirectErr
					}
					if collapsed {
						return errIdentityMatchEndpointsCollapsed
					}
					if found {
						loaded, loadErr = getIdentityMatchCandidateWithoutEvidenceTx(
							ctx, tx, survivingID)
						if loadErr != nil {
							return loadErr
						}
						loaded.Evidence, loadErr = loadCandidateEvidenceTx(ctx, tx, survivingID)
						if loadErr != nil {
							return loadErr
						}
						refreshed = loaded
						return errIdentityMatchSnapshotStale
					}
					edges, edgeErr := s.loadLinkEdgesTxContext(ctx, tx)
					if edgeErr != nil {
						return edgeErr
					}
					if _, connected := componentOf(current.LeftID, edges)[current.RightID]; connected {
						return errIdentityMatchAlreadyConnected
					}
					return loadErr
				}
				if loadErr != nil {
					return loadErr
				}
				if loaded.State != IdentityMatchStateAccepted {
					return fmt.Errorf("candidate %d state is %s: %w",
						current.ID, loaded.State, ErrIdentityMatchNotAccepted)
				}
				if loaded.LeftKind != current.LeftKind || loaded.LeftID != current.LeftID ||
					loaded.RightKind != current.RightKind || loaded.RightID != current.RightID {
					refreshed = loaded
					return errIdentityMatchSnapshotStale
				}
				loaded.Evidence = current.Evidence
				current = *loaded
				if _, updateErr := tx.ExecContext(ctx, `UPDATE identity_match_candidates
					SET application_pending = FALSE WHERE id = ?`, current.ID); updateErr != nil {
					return fmt.Errorf("complete identity match application: %w", updateErr)
				}
				return nil
			},
		)
		if errors.Is(err, errIdentityMatchSnapshotStale) {
			current = *refreshed
			continue
		}
		break
	}
	switch {
	case err == nil:
	case errors.Is(err, errIdentityMatchEndpointsCollapsed),
		errors.Is(err, errIdentityMatchAlreadyConnected):
		// A merge record confirms that the endpoints became one participant,
		// or the current graph confirms that another identity edge already
		// satisfies the assertion. No candidate deletion is otherwise success.
		revision, err = readIdentityRevisionContext(ctx, s.db)
		if err != nil {
			return nil, 0, false, err
		}
		return &current, revision, false, nil
	case errors.Is(err, ErrAlreadyLinked):
		// Already connected through other identities: the assertion holds.
		revision, err = readIdentityRevisionContext(ctx, s.db)
		if err != nil {
			return nil, 0, false, err
		}
	case errors.Is(err, ErrPersonBindingConflict):
		conflictNote := "accepted match spans two durable person profiles; not applied"
		if _, decideErr := s.DecideIdentityMatchCandidateContext(
			ctx, current.ID, IdentityMatchStateConflict, decidedBy, &conflictNote,
		); decideErr != nil {
			return nil, 0, false, errors.Join(err, decideErr)
		}
		return nil, 0, false, err
	default:
		return nil, 0, false, err
	}
	// current was either loaded before the identity lock or refreshed under it
	// after a concurrent participant merge rewrote the endpoints. Returning this
	// accepted snapshot avoids an unlocked post-commit reload: a merge can
	// legitimately contract and delete the candidate immediately after the link
	// transaction releases the lock.
	return &current, revision, linked, nil
}

// ApplyAcceptedIdentityMatchesContext links accepted participant candidates
// whose acceptance transaction left durable application work, bounded by
// limit. Successful application clears that state in the link transaction,
// so steady-state imports do not scan accepted history or load its evidence
// and link graph.
//
// A candidate that now spans two durable people is recorded as a conflict and
// skipped rather than failing the whole pass, so one contested pair cannot
// block the rest.
func (s *Store) ApplyAcceptedIdentityMatchesContext(ctx context.Context, limit int) (int, error) {
	applicationLimit := observationLookupLimit(limit)
	candidates, err := s.listPendingAcceptedIdentityMatchCandidatesContext(ctx, applicationLimit)
	if err != nil {
		return 0, err
	}
	applied := 0
	for i := range candidates {
		candidate := &candidates[i]
		decidedBy := "system"
		if candidate.DecidedBy != nil {
			decidedBy = *candidate.DecidedBy
		}
		_, _, linked, err := s.applyAcceptedIdentityMatchCandidateContext(
			ctx, candidate, decidedBy,
		)
		if err != nil {
			switch {
			case errors.Is(err, ErrIdentityMatchNotAccepted):
				continue
			case errors.Is(err, ErrPersonBindingConflict):
				slog.Warn("accepted identity match could not be applied",
					"candidate_id", candidate.ID, "error", err)
				continue
			case errors.Is(err, ErrParticipantNotFound):
				note := "accepted match could not be applied: " + err.Error()
				if _, decideErr := s.DecideIdentityMatchCandidateContext(
					ctx, candidate.ID, IdentityMatchStateConflict, decidedBy, &note,
				); decideErr != nil {
					return applied, decideErr
				}
				slog.Warn("accepted identity match could not be applied",
					"candidate_id", candidate.ID, "error", err)
				continue
			default:
				return applied, err
			}
		}
		if linked {
			applied++
		}
	}
	return applied, nil
}
