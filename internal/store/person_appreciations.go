package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

var ErrPersonAppreciationNotFound = errors.New("eligible person appreciation not found")

// ErrPersonAppreciationVersionUnsupported prevents treating a newer brief as empty.
var ErrPersonAppreciationVersionUnsupported = errors.New("unsupported person appreciation brief version")

// PersonAppreciationProjection contains the exact current version and the
// revalidated, undismissed contribution snapshots that may be shown from it.
type PersonAppreciationProjection struct {
	Brief         PersonBrief
	Appreciations []peoplesweep.BriefAppreciationMemory
}

func (s *Store) currentAppreciationsTx(ctx context.Context, tx *loggedTx, personID int64, profile peoplesweep.ProviderProfile, includeDismissed, lock bool) (PersonAppreciationProjection, error) {
	brief, err := scanPersonBrief(tx.QueryRowContext(ctx, `SELECT `+personBriefColumns+` FROM person_briefs WHERE person_id = ? AND status = ?`, personID, PersonBriefStatusCurrent))
	if errors.Is(err, sql.ErrNoRows) {
		return PersonAppreciationProjection{}, ErrPersonBriefNotFound
	}
	if err != nil {
		return PersonAppreciationProjection{}, err
	}
	projection := PersonAppreciationProjection{Brief: brief, Appreciations: []peoplesweep.BriefAppreciationMemory{}}
	switch brief.ProgramVersion {
	case "v1", "v2":
		return projection, nil
	case peoplesweep.BriefProgramVersion:
	default:
		return PersonAppreciationProjection{}, fmt.Errorf("%w: %q", ErrPersonAppreciationVersionUnsupported, brief.ProgramVersion)
	}
	var boundary peoplesweep.BriefBoundary
	if err := json.Unmarshal(brief.Boundary, &boundary); err != nil || !peoplesweep.ValidBriefAppreciationMemory(boundary.AppreciationMemory) {
		return PersonAppreciationProjection{}, errors.New("invalid appreciation brief boundary")
	}
	result := peoplesweep.BriefResult{Boundary: boundary, ValidationProfile: profile}
	_, err = s.revalidateAppreciationMemoryTx(ctx, tx, personID, &result, includeDismissed, false, lock)
	if err != nil {
		return PersonAppreciationProjection{}, err
	}
	keys := make([]string, 0, len(result.Boundary.AppreciationMemory))
	for _, memory := range result.Boundary.AppreciationMemory {
		keys = append(keys, memory.Suggestion.Key)
	}
	states, err := getPersonAppreciationStates(ctx, tx, keys)
	if err != nil {
		return PersonAppreciationProjection{}, err
	}
	for _, memory := range result.Boundary.AppreciationMemory {
		// Only the immutable pointer proves prior thanks. A null or deleted
		// pointer is unknown even if the current snapshot once carried text.
		memory.Suggestion.State, memory.Suggestion.PriorThanks = "suggested", "unknown"
		memory.Suggestion.Thanked, memory.OwnerRef, memory.OwnerRawSHA256 = nil, nil, ""
		// The sparse ledger names exactly one immutable brief. Never search a
		// recent version window, and never transfer proof across source keys.
		if proofID := states[memory.Suggestion.Key].ThanksBriefID; proofID != nil {
			proof, ok, err := s.validAppreciationProofTx(ctx, tx, personID, memory.Suggestion.Key, *proofID, profile, lock)
			if err != nil {
				return PersonAppreciationProjection{}, err
			}
			if ok {
				memory.Suggestion.State, memory.Suggestion.PriorThanks = "already_thanked", "evidenced"
				memory.Suggestion.Thanked, memory.OwnerRef, memory.OwnerRawSHA256 = proof.Suggestion.Thanked, proof.OwnerRef, proof.OwnerRawSHA256
			}
		}

		projection.Appreciations = append(projection.Appreciations, memory)
	}
	return projection, nil
}

// validAppreciationProofTx checks the exact immutable proof under the caller's
// snapshot or locks. Unsupported evidence is not proof; corrupt boundaries and
// operational failures are returned to the caller.
func (s *Store) validAppreciationProofTx(ctx context.Context, tx *loggedTx, personID int64, key string, proofID int64, profile peoplesweep.ProviderProfile, lock bool) (peoplesweep.BriefAppreciationMemory, bool, error) {
	var raw, version string
	err := tx.QueryRowContext(ctx, `SELECT CAST(boundary_json AS TEXT), program_version FROM person_briefs WHERE id = ?`, proofID).Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) || err == nil && version != "v3" {
		return peoplesweep.BriefAppreciationMemory{}, false, nil
	}
	if err != nil {
		return peoplesweep.BriefAppreciationMemory{}, false, err
	}
	var boundary peoplesweep.BriefBoundary
	if err := json.Unmarshal([]byte(raw), &boundary); err != nil || !peoplesweep.ValidBriefAppreciationMemory(boundary.AppreciationMemory) {
		return peoplesweep.BriefAppreciationMemory{}, false, errors.New("invalid appreciation proof boundary")
	}
	for _, proof := range boundary.AppreciationMemory {
		if proof.Suggestion.Key != key || proof.Suggestion.State != "already_thanked" {
			continue
		}
		result := peoplesweep.BriefResult{Boundary: peoplesweep.BriefBoundary{AppreciationMemory: []peoplesweep.BriefAppreciationMemory{proof}}, ValidationProfile: profile}
		validated, err := s.revalidateAppreciationMemoryTx(ctx, tx, personID, &result, true, true, lock)
		if err != nil {
			return peoplesweep.BriefAppreciationMemory{}, false, err
		}
		supported, ok := validated[key]
		return supported, ok && supported.Suggestion.State == "already_thanked", nil
	}
	return peoplesweep.BriefAppreciationMemory{}, false, nil
}

func (s *Store) ProjectPersonAppreciationsContext(ctx context.Context, personID int64, profile peoplesweep.ProviderProfile) (PersonAppreciationProjection, error) {
	var projection PersonAppreciationProjection
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		projection, err = s.currentAppreciationsTx(ctx, tx, personID, profile, false, false)
		return err
	})
	if err != nil {
		return PersonAppreciationProjection{}, fmt.Errorf("project person appreciations: %w", err)
	}
	return projection, nil
}

func (s *Store) DismissPersonAppreciationContext(ctx context.Context, personID int64, key, actor string, at time.Time, profile peoplesweep.ProviderProfile) (PersonAppreciationState, error) {
	if !validAppreciationKey(key) {
		return PersonAppreciationState{}, ErrInvalidPersonAppreciationKey
	}
	if strings.TrimSpace(actor) == "" || at.IsZero() {
		return PersonAppreciationState{}, ErrInvalidPersonAppreciationAudit
	}
	var state PersonAppreciationState
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		projection, err := s.currentAppreciationsTx(ctx, tx, personID, profile, true, true)
		if err != nil {
			return err
		}
		for _, memory := range projection.Appreciations {
			if memory.Suggestion.Key != key {
				continue
			}
			var uid string
			if err := tx.QueryRowContext(ctx, `SELECT vcard_uid FROM persons WHERE id = ?`, personID).Scan(&uid); err != nil {
				return err
			}
			state, err = recordPersonAppreciationDismissalTx(ctx, tx, key, uid, actor, at)
			return err
		}
		return ErrPersonAppreciationNotFound
	})
	if err != nil {
		return PersonAppreciationState{}, fmt.Errorf("dismiss person appreciation: %w", err)
	}
	return state, nil
}
