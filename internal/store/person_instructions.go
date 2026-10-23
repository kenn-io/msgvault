package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

var (
	ErrInstructionNotFound         = errors.New("instruction not found")
	ErrInstructionRevisionConflict = errors.New("instruction revision conflict")
	ErrInstructionInvalid          = errors.New("invalid instruction")
	ErrInstructionTransition       = personfacts.ErrInstructionTransition
	ErrInstructionDuplicate        = errors.New("duplicate live instruction")
)

type PersonInstruction struct {
	ID                  int64                              `json:"id"`
	PersonID            int64                              `json:"person_id"`
	InstructionKey      string                             `json:"instruction_key"`
	Kind                personfacts.InstructionKind        `json:"kind"`
	ScopeKey            string                             `json:"scope_key"`
	ContactPointID      *int64                             `json:"contact_point_id,omitzero"`
	Text                string                             `json:"text"`
	NormalizedText      string                             `json:"normalized_text"`
	ValueText           string                             `json:"value_text"`
	Origin              personfacts.InstructionOrigin      `json:"origin"`
	Status              personfacts.InstructionStatus      `json:"status"`
	ReviewState         personfacts.InstructionReviewState `json:"review_state"`
	Adopted             bool                               `json:"adopted"`
	StatedAt            *time.Time                         `json:"stated_at,omitzero"`
	ValidFrom           time.Time                          `json:"valid_from"`
	ValidUntil          *time.Time                         `json:"valid_until,omitzero"`
	SupersedesID        *int64                             `json:"supersedes_id,omitzero"`
	SourceObservationID *int64                             `json:"source_observation_id,omitzero"`
	GenerationID        *int64                             `json:"generation_id,omitzero"`
	ConfirmedAt         *time.Time                         `json:"confirmed_at,omitzero"`
	ConfirmedBy         *string                            `json:"confirmed_by,omitzero"`
	Revision            int64                              `json:"revision"`
	CreatedAt           time.Time                          `json:"created_at"`
	UpdatedAt           time.Time                          `json:"updated_at"`
	ContentTrust        string                             `json:"content_trust"`
	VisibleToAgents     bool                               `json:"visible_to_agents"`
	ScopeMissing        bool                               `json:"scope_missing"`
}
type PersonInstructionInput struct {
	PersonID        int64
	Kind            personfacts.InstructionKind
	ContactPointID  *int64
	ValueText, Text string
	ValidUntil      *time.Time
	Actor           string
}
type PersonInstructionDecision struct {
	InstructionID, ExpectedRevision int64
	Action                          personfacts.InstructionAction
	Reason                          string
	Edit                            *PersonInstructionEdit
	Actor                           string
}
type PersonInstructionEdit struct {
	Kind      *personfacts.InstructionKind `json:"kind,omitzero"`
	Text      *string                      `json:"text,omitzero"`
	ValueText *string                      `json:"value,omitzero"`
}
type PersonInstructionFollowOn struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}
type PersonInstructionDecisionResult struct {
	Instruction PersonInstruction           `json:"instruction"`
	FollowOns   []PersonInstructionFollowOn `json:"follow_ons"`
}
type PersonInstructionFilter struct {
	Statuses       []personfacts.InstructionStatus
	IncludeExpired bool
	AsOf           time.Time
}

const personInstructionSelect = `SELECT i.id,i.person_id,i.instruction_key,i.kind,i.scope_key,i.contact_point_id,
 i.original_text,i.normalized_text,i.value_text,i.origin,i.status,i.review_state,i.adopted,i.stated_at,i.valid_from,i.valid_until,
 i.supersedes_id,i.source_observation_id,i.generation_id,i.confirmed_at,i.confirmed_by,i.revision,i.created_at,i.updated_at,
 (i.scope_key <> '' AND NOT EXISTS(SELECT 1 FROM person_contact_points cp
 WHERE cp.id=i.contact_point_id AND cp.person_id=i.person_id AND cp.superseded_at IS NULL AND cp.active_until IS NULL))
 FROM person_instructions i`

func scanPersonInstruction(sc scanner, asOf time.Time) (*PersonInstruction, error) {
	var i PersonInstruction
	err := sc.Scan(&i.ID, &i.PersonID, &i.InstructionKey, &i.Kind, &i.ScopeKey, &i.ContactPointID, &i.Text, &i.NormalizedText, &i.ValueText, &i.Origin, &i.Status, &i.ReviewState, &i.Adopted, &i.StatedAt, &i.ValidFrom, &i.ValidUntil, &i.SupersedesID, &i.SourceObservationID, &i.GenerationID, &i.ConfirmedAt, &i.ConfirmedBy, &i.Revision, &i.CreatedAt, &i.UpdatedAt, &i.ScopeMissing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInstructionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan person instruction: %w", err)
	}
	i.ContentTrust = "unconfirmed"
	if i.Origin == personfacts.InstructionOriginOwner {
		i.ContentTrust = "owner_entered"
	} else if i.ConfirmedAt != nil {
		i.ContentTrust = "person_statement_owner_confirmed"
	}
	i.VisibleToAgents = i.Status == personfacts.InstructionStatusActive && (i.Origin == personfacts.InstructionOriginOwner || i.ConfirmedAt != nil) && !asOf.Before(i.ValidFrom) && (i.ValidUntil == nil || asOf.Before(*i.ValidUntil))
	return &i, nil
}

func (s *Store) GetPersonInstructionContext(ctx context.Context, id int64) (*PersonInstruction, error) {
	return scanPersonInstruction(s.db.QueryRowContext(ctx, personInstructionSelect+` WHERE i.id=?`, id), time.Now().UTC())
}
func listPersonInstructionRows(ctx context.Context, q contextRowsQuerier, query string, asOf time.Time, args ...any) ([]PersonInstruction, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person instructions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []PersonInstruction{}
	for rows.Next() {
		i, err := scanPersonInstruction(rows, asOf)
		if err != nil {
			return nil, err
		}
		result = append(result, *i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person instructions: %w", err)
	}
	return result, nil
}
func (s *Store) ListPersonInstructionsContext(ctx context.Context, personID int64, f PersonInstructionFilter) ([]PersonInstruction, error) {
	if _, err := s.GetPersonContext(ctx, personID); err != nil {
		return nil, err
	}
	if f.AsOf.IsZero() {
		f.AsOf = time.Now().UTC()
	}
	f.AsOf = f.AsOf.UTC()
	query := personInstructionSelect + ` WHERE i.person_id=?`
	args := []any{personID}
	if len(f.Statuses) > 0 {
		query += ` AND i.status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Statuses)), ",") + `)`
		for _, status := range f.Statuses {
			switch status {
			case personfacts.InstructionStatusProposed, personfacts.InstructionStatusActive, personfacts.InstructionStatusSuspended, personfacts.InstructionStatusSuperseded, personfacts.InstructionStatusExpired, personfacts.InstructionStatusRevoked, personfacts.InstructionStatusRejected:
			default:
				return nil, fmt.Errorf("%w: unknown-status", ErrInstructionInvalid)
			}
			args = append(args, status)
		}
	} else {
		query += ` AND i.status='active' AND (i.origin='owner' OR i.confirmed_at IS NOT NULL) AND i.valid_from<=?`
		args = append(args, f.AsOf)
	}
	if !f.IncludeExpired {
		query += ` AND (i.valid_until IS NULL OR i.valid_until>?)`
		args = append(args, f.AsOf)
	}
	return listPersonInstructionRows(ctx, s.db, query+` ORDER BY i.kind,i.scope_key,i.id`, f.AsOf, args...)
}
func (s *Store) ListPersonInstructionReviewContext(ctx context.Context, limit int) ([]PersonInstruction, error) {
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("%w: limit must be 1–200", ErrInstructionInvalid)
	}
	return listPersonInstructionRows(ctx, s.db, personInstructionSelect+` WHERE i.status IN ('proposed','suspended') OR (i.status='active' AND i.review_state<>'none') ORDER BY i.updated_at,i.id LIMIT ?`, time.Now().UTC(), limit)
}

func (s *Store) lockPersonInstructionsTx(ctx context.Context, tx *loggedTx, personID int64) error {
	if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-instructions", personID); err != nil {
		return err
	}
	query := `SELECT 1 FROM persons WHERE id=?`
	if s.IsPostgreSQL() {
		query += ` FOR KEY SHARE`
	}
	var exists int
	if err := tx.QueryRowContext(ctx, query, personID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ErrPersonNotFound
	} else if err != nil {
		return fmt.Errorf("verify instruction person: %w", err)
	}
	return nil
}

type personInstructionScopeIdentity struct {
	AddressKind     string  `json:"address_kind"`
	ServiceID       *int64  `json:"service_id"`
	ScopeKind       *string `json:"scope_kind"`
	ScopeValue      *string `json:"scope_value"`
	NormalizedValue string  `json:"normalized_value"`
}

func instructionScopeAddress(scopeKey string) string {
	var scope personInstructionScopeIdentity
	if err := json.Unmarshal([]byte(scopeKey), &scope); err == nil && scope.AddressKind != "" {
		return scope.NormalizedValue
	}
	if _, address, ok := strings.Cut(scopeKey, ":"); ok {
		return address
	}
	return ""
}

func instructionScopeTx(ctx context.Context, tx *loggedTx, personID int64, contactID *int64) (string, string, error) {
	if contactID == nil {
		return "", "", nil
	}
	var scope personInstructionScopeIdentity
	var serviceID sql.NullInt64
	var scopeKind, scopeValue sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT address_kind,service_id,scope_kind,scope_value,normalized_value FROM person_contact_points WHERE id=? AND person_id=? AND superseded_at IS NULL AND active_until IS NULL`, *contactID, personID).Scan(&scope.AddressKind, &serviceID, &scopeKind, &scopeValue, &scope.NormalizedValue)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: unsupported-target", ErrInstructionInvalid)
	}
	if err != nil {
		return "", "", fmt.Errorf("load instruction scope: %w", err)
	}
	if serviceID.Valid {
		scope.ServiceID = &serviceID.Int64
	}
	if scopeKind.Valid {
		scope.ScopeKind = &scopeKind.String
	}
	if scopeValue.Valid {
		scope.ScopeValue = &scopeValue.String
	}
	encoded, err := json.Marshal(scope)
	if err != nil {
		return "", "", fmt.Errorf("encode instruction scope: %w", err)
	}
	return string(encoded), scope.NormalizedValue, nil
}
func appendPersonInstructionEventTx(ctx context.Context, tx *loggedTx, i *PersonInstruction, action personfacts.InstructionAction, actor string, detail any) error {
	data, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("marshal instruction event: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_instruction_events(person_id,instruction_id,instruction_key,action,actor,detail_json) VALUES (?,?,?,?,?,?)`, i.PersonID, i.ID, i.InstructionKey, action, actor, string(data))
	if err != nil {
		return fmt.Errorf("append instruction event: %w", err)
	}
	return nil
}
func instructionLiveKeyExistsTx(ctx context.Context, tx *loggedTx, personID int64, key string, exceptID int64) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_instructions WHERE person_id=? AND instruction_key=? AND id<>? AND status IN ('proposed','active','suspended'))`, personID, key, exceptID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check live instruction key: %w", err)
	}
	return exists, nil
}
func expirePersonInstructionKeyTx(ctx context.Context, tx *loggedTx, personID int64, key string, exceptID int64, now time.Time) error {
	rows, err := listPersonInstructionRows(ctx, tx, personInstructionSelect+` WHERE i.person_id=? AND i.instruction_key=? AND i.id<>? AND i.status='active' AND i.valid_until<=? ORDER BY i.id`, now, personID, key, exceptID, now)
	if err != nil {
		return err
	}
	for _, row := range rows {
		next, err := personfacts.NextInstructionState(personfacts.InstructionState{Status: row.Status, ReviewState: row.ReviewState, Origin: row.Origin, Confirmed: row.ConfirmedAt != nil, Adopted: row.Adopted}, personfacts.InstructionActionExpire, false)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_instructions SET status=?,review_state=?,revision=revision+1,updated_at=? WHERE id=?`, next.Status, next.ReviewState, now, row.ID); err != nil {
			return fmt.Errorf("expire person instruction key: %w", err)
		}
		if err := appendPersonInstructionEventTx(ctx, tx, &row, personfacts.InstructionActionExpire, "system:expiry", map[string]string{"reason": "valid_until"}); err != nil {
			return err
		}
	}
	return nil
}
func supersedePersonInstructionTx(ctx context.Context, tx *loggedTx, i *PersonInstruction, actor string, at time.Time) error {
	if i.Status != personfacts.InstructionStatusActive {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_instructions SET status='superseded',review_state='none',revision=revision+1,updated_at=? WHERE id=?`, at, i.ID); err != nil {
		return fmt.Errorf("supersede instruction: %w", err)
	}
	return appendPersonInstructionEventTx(ctx, tx, i, personfacts.InstructionActionSupersede, actor, map[string]any{})
}
func (s *Store) AddPersonInstructionContext(ctx context.Context, in PersonInstructionInput) (*PersonInstruction, error) {
	in.Actor = strings.TrimSpace(in.Actor)
	in.ValidUntil = personFactPortableTimePointer(in.ValidUntil)
	if in.Actor == "" {
		return nil, fmt.Errorf("%w: actor-required", ErrInstructionInvalid)
	}
	return retryContendedWrite(ctx, s, "add person instruction", func() (*PersonInstruction, error) {
		var result *PersonInstruction
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockPersonInstructionsTx(ctx, tx, in.PersonID); err != nil {
				return err
			}
			scope, address, err := instructionScopeTx(ctx, tx, in.PersonID, in.ContactPointID)
			if err != nil {
				return err
			}
			now := personFactPortableTime(time.Now().UTC())
			if failure := personfacts.ValidateInstructionText(personfacts.InstructionText{Kind: in.Kind, ScopeKey: scope, ScopeAddress: address, ValueText: in.ValueText, Text: in.Text, ValidFrom: now, ValidUntil: in.ValidUntil}); failure != nil {
				return fmt.Errorf("%w: %w", ErrInstructionInvalid, failure)
			}
			key := personfacts.InstructionKey(in.Kind, scope, in.ValueText, in.Text)
			if err := expirePersonInstructionKeyTx(ctx, tx, in.PersonID, key, 0, now); err != nil {
				return err
			}
			duplicate, err := instructionLiveKeyExistsTx(ctx, tx, in.PersonID, key, 0)
			if err != nil {
				return err
			}
			if duplicate {
				return ErrInstructionDuplicate
			}
			var supersedes *int64
			if personfacts.SingleValuedInstructionKind(in.Kind) {
				previous, err := listPersonInstructionRows(ctx, tx, personInstructionSelect+` WHERE i.person_id=? AND i.kind=? AND i.scope_key=? AND i.status='active' ORDER BY i.id`, now, in.PersonID, in.Kind, scope)
				if err != nil {
					return err
				}
				for _, old := range previous {
					if err := supersedePersonInstructionTx(ctx, tx, &old, in.Actor, now); err != nil {
						return err
					}
					supersedes = &old.ID
				}
			}
			var id int64
			err = tx.QueryRowContext(ctx, `INSERT INTO person_instructions(person_id,instruction_key,kind,scope_key,contact_point_id,original_text,normalized_text,value_text,origin,status,valid_from,valid_until,supersedes_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,'owner','active',?,?,?,?,?) RETURNING id`, in.PersonID, key, in.Kind, scope, in.ContactPointID, in.Text, personfacts.NormalizeInstructionText(in.Text), strings.TrimSpace(in.ValueText), now, in.ValidUntil, supersedes, now, now).Scan(&id)
			if err != nil {
				if s.dialect.IsConflictError(err) {
					return ErrInstructionDuplicate
				}
				return fmt.Errorf("insert person instruction: %w", err)
			}
			result, err = scanPersonInstruction(tx.QueryRowContext(ctx, personInstructionSelect+` WHERE i.id=?`, id), now)
			if err != nil {
				return err
			}
			return appendPersonInstructionEventTx(ctx, tx, result, personfacts.InstructionActionPropose, in.Actor, map[string]string{"origin": "owner", "text": in.Text})
		})
		return result, err
	})
}

func (s *Store) DecidePersonInstructionContext(ctx context.Context, d PersonInstructionDecision) (*PersonInstructionDecisionResult, error) {
	d.Actor = strings.TrimSpace(d.Actor)
	if d.Actor == "" {
		return nil, fmt.Errorf("%w: actor-required", ErrInstructionInvalid)
	}
	switch d.Action {
	case personfacts.InstructionActionConfirm, personfacts.InstructionActionReject, personfacts.InstructionActionUnsure, personfacts.InstructionActionRevoke, personfacts.InstructionActionAdopt:
	default:
		return nil, ErrInstructionTransition
	}
	if d.Edit != nil && d.Action != personfacts.InstructionActionConfirm {
		return nil, fmt.Errorf("%w: edit-requires-confirm", ErrInstructionInvalid)
	}
	return retryContendedWrite(ctx, s, "decide person instruction", func() (*PersonInstructionDecisionResult, error) {
		var result *PersonInstructionDecisionResult
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			// This read holds no instruction row lock. Person KEY SHARE must precede
			// all writes so a concurrent merge can finish without a lock inversion.
			var personID int64
			err := tx.QueryRowContext(ctx, `SELECT person_id FROM person_instructions WHERE id=?`, d.InstructionID).Scan(&personID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInstructionNotFound
			}
			if err != nil {
				return fmt.Errorf("find instruction person: %w", err)
			}
			if err := s.lockPersonInstructionsTx(ctx, tx, personID); err != nil {
				return err
			}
			now := personFactPortableTime(time.Now().UTC())
			current, err := scanPersonInstruction(tx.QueryRowContext(ctx, personInstructionSelect+` WHERE i.id=?`, d.InstructionID), now)
			if err != nil {
				return err
			}
			if current.PersonID != personID {
				return ErrInstructionNotFound
			}
			if current.Revision != d.ExpectedRevision {
				return ErrInstructionRevisionConflict
			}
			if (d.Action == personfacts.InstructionActionConfirm || d.Action == personfacts.InstructionActionAdopt) && current.ValidUntil != nil && !now.Before(*current.ValidUntil) {
				return fmt.Errorf("%w: expired", ErrInstructionInvalid)
			}
			next, err := personfacts.NextInstructionState(personfacts.InstructionState{Status: current.Status, ReviewState: current.ReviewState, Origin: current.Origin, Confirmed: current.ConfirmedAt != nil, Adopted: current.Adopted}, d.Action, true)
			if err != nil {
				return err
			}
			if d.Edit != nil {
				old := *current
				if d.Edit.Kind != nil {
					current.Kind = *d.Edit.Kind
				}
				if d.Edit.Text != nil {
					current.Text = *d.Edit.Text
				}
				if d.Edit.ValueText != nil {
					current.ValueText = strings.TrimSpace(*d.Edit.ValueText)
				}
				address := instructionScopeAddress(current.ScopeKey)
				if failure := personfacts.ValidateInstructionText(personfacts.InstructionText{Kind: current.Kind, ScopeKey: current.ScopeKey, ScopeAddress: address, ValueText: current.ValueText, Text: current.Text, ValidFrom: current.ValidFrom, ValidUntil: current.ValidUntil}); failure != nil {
					return fmt.Errorf("%w: %w", ErrInstructionInvalid, failure)
				}
				current.NormalizedText = personfacts.NormalizeInstructionText(current.Text)
				current.InstructionKey = personfacts.InstructionKey(current.Kind, current.ScopeKey, current.ValueText, current.Text)
				if err := expirePersonInstructionKeyTx(ctx, tx, personID, current.InstructionKey, current.ID, now); err != nil {
					return err
				}
				exists, err := instructionLiveKeyExistsTx(ctx, tx, personID, current.InstructionKey, current.ID)
				if err != nil {
					return err
				}
				if exists {
					return ErrInstructionDuplicate
				}
				if err := appendPersonInstructionEventTx(ctx, tx, &old, personfacts.InstructionActionEdit, d.Actor, map[string]string{
					"new_key": current.InstructionKey, "old_text": old.Text, "new_text": current.Text,
				}); err != nil {
					return err
				}
			}
			// Adoption is an owner authorization too. A suspended extraction row can
			// lack a prior confirmation; record that authorization to satisfy the
			// active-row invariant while retaining its extraction provenance.
			if d.Action == personfacts.InstructionActionAdopt && current.Origin == personfacts.InstructionOriginExtraction && current.ConfirmedAt == nil {
				current.ConfirmedAt = &now
				current.ConfirmedBy = &d.Actor
			}
			if d.Action == personfacts.InstructionActionConfirm {
				current.ConfirmedAt = &now
				current.ConfirmedBy = &d.Actor
				if current.SupersedesID != nil {
					old, err := scanPersonInstruction(tx.QueryRowContext(ctx, personInstructionSelect+` WHERE i.id=? AND i.person_id=?`, *current.SupersedesID, personID), now)
					if err != nil && !errors.Is(err, ErrInstructionNotFound) {
						return err
					}
					if old != nil {
						if err := supersedePersonInstructionTx(ctx, tx, old, d.Actor, now); err != nil {
							return err
						}
					}
				}
			}
			if (d.Action == personfacts.InstructionActionConfirm || d.Action == personfacts.InstructionActionAdopt) && personfacts.SingleValuedInstructionKind(current.Kind) {
				// Owner selection wins over every currently active alternative,
				// including one added after this proposal was first recorded.
				alternatives, err := listPersonInstructionRows(ctx, tx, personInstructionSelect+` WHERE i.person_id=? AND i.kind=? AND i.scope_key=? AND i.status='active' AND i.id<>? ORDER BY i.id`, now, personID, current.Kind, current.ScopeKey, current.ID)
				if err != nil {
					return err
				}
				for _, old := range alternatives {
					if err := supersedePersonInstructionTx(ctx, tx, &old, d.Actor, now); err != nil {
						return err
					}
				}
			}
			updated, err := tx.ExecContext(ctx, `UPDATE person_instructions SET kind=?,original_text=?,normalized_text=?,value_text=?,instruction_key=?,status=?,review_state=?,adopted=?,confirmed_at=?,confirmed_by=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, current.Kind, current.Text, current.NormalizedText, current.ValueText, current.InstructionKey, next.Status, next.ReviewState, next.Adopted, current.ConfirmedAt, current.ConfirmedBy, now, current.ID, d.ExpectedRevision)
			if err != nil {
				if s.dialect.IsConflictError(err) {
					return ErrInstructionDuplicate
				}
				return fmt.Errorf("decide person instruction: %w", err)
			}
			count, err := updated.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrInstructionRevisionConflict
			}
			current, err = scanPersonInstruction(tx.QueryRowContext(ctx, personInstructionSelect+` WHERE i.id=?`, current.ID), now)
			if err != nil {
				return err
			}
			if err := appendPersonInstructionEventTx(ctx, tx, current, d.Action, d.Actor, map[string]string{"reason": strings.TrimSpace(d.Reason)}); err != nil {
				return err
			}
			followOns := []PersonInstructionFollowOn{}
			if d.Action == personfacts.InstructionActionConfirm || d.Action == personfacts.InstructionActionAdopt {
				switch current.Kind {
				case personfacts.InstructionKindAddressAs:
					followOns = append(followOns, PersonInstructionFollowOn{Kind: "save_nickname", Value: current.ValueText})
				case personfacts.InstructionKindPreferChannel:
					followOns = append(followOns, PersonInstructionFollowOn{Kind: "set_primary_channel", Value: current.ValueText})
				default:
					// Other instruction kinds have no v1 follow-on.
				}
			}
			result = &PersonInstructionDecisionResult{Instruction: *current, FollowOns: followOns}
			return nil
		})
		return result, err
	})
}
