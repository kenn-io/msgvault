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

// PersonAppreciationProjection contains the exact current version and the
// revalidated, undismissed contribution snapshots that may be shown from it.
type PersonAppreciationProjection struct {
	Brief         PersonBrief
	Appreciations []peoplesweep.BriefAppreciationMemory
}

func (s *Store) currentAppreciationsTx(ctx context.Context, tx *loggedTx, personID int64, profile peoplesweep.ProviderProfile, includeDismissed bool) (PersonAppreciationProjection, error) {
	brief, err := scanPersonBrief(tx.QueryRowContext(ctx, `SELECT `+personBriefColumns+` FROM person_briefs WHERE person_id = ? AND status = ?`, personID, PersonBriefStatusCurrent))
	if errors.Is(err, sql.ErrNoRows) {
		return PersonAppreciationProjection{}, ErrPersonBriefNotFound
	}
	if err != nil {
		return PersonAppreciationProjection{}, err
	}
	projection := PersonAppreciationProjection{Brief: brief, Appreciations: []peoplesweep.BriefAppreciationMemory{}}
	if brief.ProgramVersion != "v3" {
		return projection, nil
	}
	var boundary peoplesweep.BriefBoundary
	if err := json.Unmarshal(brief.Boundary, &boundary); err != nil || len(boundary.AppreciationMemory) > 3 {
		return PersonAppreciationProjection{}, errors.New("invalid appreciation brief boundary")
	}
	result := peoplesweep.BriefResult{Boundary: boundary, ValidationProfile: profile}
	_, err = s.revalidateAppreciationMemoryTx(ctx, tx, personID, &result, includeDismissed)
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
		var proofID sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT thanks_brief_id FROM person_appreciation_state WHERE contribution_key = ?`, memory.Suggestion.Key).Scan(&proofID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return PersonAppreciationProjection{}, err
		}
		if proofID.Valid {
			var raw, programVersion string
			err := tx.QueryRowContext(ctx, `SELECT CAST(boundary_json AS TEXT), program_version FROM person_briefs WHERE id = ?`, proofID.Int64).Scan(&raw, &programVersion)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return PersonAppreciationProjection{}, err
			}
			if err == nil && programVersion == "v3" {
				var old peoplesweep.BriefBoundary
				if err := json.Unmarshal([]byte(raw), &old); err != nil || len(old.AppreciationMemory) > 3 {
					return PersonAppreciationProjection{}, errors.New("invalid appreciation proof boundary")
				}
				for _, proof := range old.AppreciationMemory {
					if proof.Suggestion.Key != memory.Suggestion.Key || proof.Suggestion.State != "already_thanked" {
						continue
					}
					proofResult := peoplesweep.BriefResult{Boundary: peoplesweep.BriefBoundary{AppreciationMemory: []peoplesweep.BriefAppreciationMemory{proof}}, ValidationProfile: profile}
					validated, err := s.revalidateAppreciationMemoryTx(ctx, tx, personID, &proofResult, true)
					if err != nil {
						return PersonAppreciationProjection{}, err
					}
					if supported, ok := validated[proof.Suggestion.Key]; ok && supported.Suggestion.State == "already_thanked" {
						memory.Suggestion.State, memory.Suggestion.PriorThanks = "already_thanked", "evidenced"
						memory.Suggestion.Thanked, memory.OwnerRef, memory.OwnerRawSHA256 = supported.Suggestion.Thanked, supported.OwnerRef, supported.OwnerRawSHA256
					}
					break
				}
			}
		}
		projection.Appreciations = append(projection.Appreciations, memory)
	}
	return projection, nil
}

func (s *Store) ProjectPersonAppreciationsContext(ctx context.Context, personID int64, profile peoplesweep.ProviderProfile) (PersonAppreciationProjection, error) {
	var projection PersonAppreciationProjection
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		var err error
		projection, err = s.currentAppreciationsTx(ctx, tx, personID, profile, false)
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
		projection, err := s.currentAppreciationsTx(ctx, tx, personID, profile, true)
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
