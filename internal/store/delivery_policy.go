package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// DeliveryPolicy is an application gate for an explicit send. It grants no
// provider credentials, confirmation exemption, or permission to edit policies.
type DeliveryPolicy string

const (
	DeliveryDraftOnly   DeliveryPolicy = "draft_only"
	DeliverySendAllowed DeliveryPolicy = "send_allowed"
)

var (
	ErrDeliveryPolicyInvalid          = errors.New("invalid delivery policy request")
	ErrDeliveryPolicyRevisionConflict = errors.New("delivery policy revision conflict")
	ErrDeliveryTargetChanged          = errors.New("delivery target changed; review current binding")
	ErrDeliveryPersonUnknown          = errors.New("unknown or retired canonical person UID")
)

// DeliveryTarget references native records; it is not a routing/discovery model.
// Every populated field is part of the exact method's authorization scope.
type DeliveryTarget struct {
	SourceID       int64  `json:"source_id"`
	SourceType     string `json:"source_type"`
	AccountID      string `json:"account_id"`
	Network        string `json:"network"`
	Endpoint       string `json:"endpoint"`
	ContactPointID int64  `json:"contact_point_id,omitempty"`
	ParticipantID  int64  `json:"participant_id,omitempty"`
	ConversationID int64  `json:"conversation_id,omitempty"`
	ProviderChatID string `json:"provider_chat_id,omitempty"`
}
type DeliveryPolicyQuery struct {
	PersonUID string          `json:"person_uid"`
	Target    *DeliveryTarget `json:"target,omitempty"`
}
type DeliveryPolicyState struct {
	PersonUID         string          `json:"person_uid"`
	PersonID          int64           `json:"person_id"`
	PersonRevision    int64           `json:"person_revision"`
	IdentityRevision  int64           `json:"identity_revision"`
	PolicyRevision    int64           `json:"policy_revision"`
	BindingDigest     string          `json:"binding_digest"`
	StoredPolicy      *DeliveryPolicy `json:"stored_policy"`
	EffectivePolicy   DeliveryPolicy  `json:"effective_policy"`
	InheritanceSource string          `json:"inheritance_source"`
	Reason            string          `json:"reason"`
	Scope             string          `json:"scope"`
	Target            *DeliveryTarget `json:"target,omitempty"`
}
type DeliveryPolicyWrite struct {
	Query                  DeliveryPolicyQuery `json:"query"`
	ExpectedRevision       int64               `json:"expected_revision"`
	ExpectedPersonRevision int64               `json:"expected_person_revision,omitempty"`
	BindingDigest          string              `json:"binding_digest,omitempty"`
	Policy                 DeliveryPolicy      `json:"policy,omitempty"`
	ScopeAcknowledgement   string              `json:"scope_acknowledgement,omitempty"`
	Actor                  string              `json:"-"` // supplied by the trusted service, never a wire field
	Reason                 string              `json:"reason"`
}
type DeliveryPolicyAudit struct {
	Actor        string          `json:"actor"`
	Reason       string          `json:"reason"`
	CreatedAt    time.Time       `json:"created_at"`
	Scope        string          `json:"scope"`
	BeforePolicy *DeliveryPolicy `json:"before_policy"`
	AfterPolicy  *DeliveryPolicy `json:"after_policy"`
}
type DeliveryPolicyReceipt struct {
	Before DeliveryPolicyState `json:"before"`
	After  DeliveryPolicyState `json:"after"`
	Audit  DeliveryPolicyAudit `json:"audit"`
}

func deliveryStringValid(v string) bool {
	return v != "" && len(v) <= 1024 && strings.TrimSpace(v) == v && !strings.ContainsFunc(v, unicode.IsControl)
}
func ValidateDeliveryPolicyQuery(q DeliveryPolicyQuery) error {
	if !deliveryStringValid(q.PersonUID) {
		return ErrDeliveryPolicyInvalid
	}
	if q.Target == nil {
		return nil
	}
	t := q.Target
	if t.SourceID <= 0 || t.SourceID > 9007199254740991 || t.ContactPointID > 9007199254740991 || t.ParticipantID > 9007199254740991 || t.ConversationID > 9007199254740991 || !deliveryStringValid(t.SourceType) || !deliveryStringValid(t.AccountID) || !deliveryStringValid(t.Network) || !deliveryStringValid(t.Endpoint) || t.ContactPointID < 0 || t.ParticipantID < 0 || t.ConversationID < 0 || (t.ProviderChatID != "" && !deliveryStringValid(t.ProviderChatID)) {
		return ErrDeliveryPolicyInvalid
	}
	if (t.ContactPointID > 0) == (t.ParticipantID > 0) {
		return ErrDeliveryPolicyInvalid
	}
	return nil
}
func deliveryJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode delivery policy contract: %w", err)
	}
	return string(b), nil
}
func deliveryDigest(v any) (string, error) {
	encoded, err := deliveryJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}
func deliveryScope(q DeliveryPolicyQuery) (string, error) {
	if q.Target == nil {
		return "person_all_routes", nil
	}
	return deliveryDigest(q.Target)
}
func deliveryCopyTarget(t *DeliveryTarget) *DeliveryTarget {
	if t == nil {
		return nil
	}
	return new(*t)
}

// Policy mutations share the dispatch fence with provider admission.
func (s *Store) withDeliveryTx(ctx context.Context, f func(*loggedTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.IsPostgreSQL() {
		if err = s.enterDeliveryAdmissionExclusiveFenceContext(ctx, tx); err != nil {
			return err
		}
	} else if _, err = tx.ExecContext(ctx,
		`UPDATE delivery_admission_lock SET singleton=singleton WHERE singleton=1`,
	); err != nil {
		return fmt.Errorf("enter delivery gate: %w", err)
	}
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) GetDeliveryPolicyContext(ctx context.Context, q DeliveryPolicyQuery) (*DeliveryPolicyState, error) {
	if err := ValidateDeliveryPolicyQuery(q); err != nil {
		return nil, err
	}
	var state *DeliveryPolicyState
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		state, err = s.deliveryStateTx(ctx, tx, q)
		return err
	})
	return state, err
}
func deliveryVersion(ctx context.Context, tx *loggedTx, kind string, id int64) (version, generation int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT version,generation FROM delivery_binding_versions WHERE kind=? AND target_id=?`, kind, id).Scan(&version, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
func (s *Store) deliveryStateTx(ctx context.Context, tx *loggedTx, q DeliveryPolicyQuery) (*DeliveryPolicyState, error) {
	scope, err := deliveryScope(q)
	if err != nil {
		return nil, err
	}
	state := &DeliveryPolicyState{PersonUID: q.PersonUID, Target: deliveryCopyTarget(q.Target), PolicyRevision: 1, EffectivePolicy: DeliveryDraftOnly, InheritanceSource: "system_default", Scope: scope, Reason: "draft_required"}
	err = tx.QueryRowContext(ctx, `SELECT id,revision FROM persons WHERE vcard_uid=?`, q.PersonUID).Scan(&state.PersonID, &state.PersonRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeliveryPersonUnknown
	}
	if err != nil {
		return nil, err
	}
	// Retired aliases are never followed for authorization, including corrupt
	// alias/live-row collisions. Native merge/split operations retain provenance.
	var retired int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_uid_aliases WHERE retired_uid=?`, q.PersonUID).Scan(&retired); err != nil {
		return nil, err
	}
	if retired != 0 {
		return nil, ErrDeliveryPersonUnknown
	}
	state.IdentityRevision, err = readIdentityRevisionContext(ctx, tx)
	if err != nil {
		return nil, err
	}
	pv, _, err := deliveryVersion(ctx, tx, string(AttributeObjectPerson), state.PersonID)
	if err != nil {
		return nil, err
	}
	var policy DeliveryPolicy
	var approvedRevision, approvedPV, approvedGen, approvedIdentity int64
	var defaultExplicit bool
	err = tx.QueryRowContext(ctx, `SELECT policy,revision,person_revision,person_binding_version,approved_generation,default_explicit,identity_revision FROM delivery_policy_persons WHERE person_uid=?`, q.PersonUID).Scan(&policy, &state.PolicyRevision, &approvedRevision, &approvedPV, &approvedGen, &defaultExplicit, &approvedIdentity)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	hasDefault := err == nil && defaultExplicit
	if q.Target == nil {
		// A broader approval must describe the bindings reviewed before the
		// write, including accounts that can change without a person revision.
		var generation int64
		if err = tx.QueryRowContext(ctx, `SELECT generation FROM delivery_admission_lock WHERE singleton=1`).Scan(&generation); err != nil {
			return nil, err
		}
		state.BindingDigest, err = deliveryDigest([]any{q.PersonUID, state.PersonID, state.PersonRevision, state.IdentityRevision, pv, generation})
		if err != nil {
			return nil, err
		}
		if hasDefault {
			state.StoredPolicy = new(policy)
			state.InheritanceSource = "person_default"
			state.EffectivePolicy = policy
		}
		if policy == DeliverySendAllowed && (approvedRevision != state.PersonRevision || approvedPV != pv || approvedIdentity != state.IdentityRevision) {
			state.EffectivePolicy = DeliveryDraftOnly
			state.Reason = "binding_changed"
		}
		if state.EffectivePolicy == DeliverySendAllowed {
			state.Reason = "explicit_person_default"
		}
		return state, nil
	}
	digest, generation, reason, err := s.resolveDeliveryTargetTx(ctx, tx, state, q.Target)
	if err != nil {
		return nil, err
	}
	state.BindingDigest = digest
	var override DeliveryPolicy
	var savedDigest string
	err = tx.QueryRowContext(ctx, `SELECT policy,binding_digest FROM delivery_policy_overrides WHERE person_uid=? AND scope_key=?`, q.PersonUID, state.Scope).Scan(&override, &savedDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		state.StoredPolicy = new(override)
		state.InheritanceSource = "contact_method"
		state.EffectivePolicy = override
		if override == DeliverySendAllowed && savedDigest != digest {
			state.EffectivePolicy = DeliveryDraftOnly
			state.Reason = "binding_changed"
		}
	} else if hasDefault {
		state.InheritanceSource = "person_default"
		state.EffectivePolicy = policy
		if policy == DeliverySendAllowed && (approvedRevision != state.PersonRevision || approvedPV != pv || approvedIdentity != state.IdentityRevision || generation > approvedGen) {
			state.EffectivePolicy = DeliveryDraftOnly
			state.Reason = "binding_changed"
		}
	}
	if reason != "" {
		state.EffectivePolicy = DeliveryDraftOnly
		state.Reason = reason
	} else if state.EffectivePolicy == DeliverySendAllowed {
		state.Reason = "explicit_approval"
	}
	if state.EffectivePolicy == DeliverySendAllowed {
		restricted, err := deliveryHasEquivalentRestrictionTx(ctx, tx, q.PersonUID, *q.Target)
		if err != nil {
			return nil, err
		}
		if restricted {
			state.EffectivePolicy = DeliveryDraftOnly
			state.Reason = "equivalent_endpoint_restricted"
		}
	}
	return state, nil
}

func deliveryTargetsShareRoute(a, b DeliveryTarget) bool {
	return a.SourceID == b.SourceID &&
		a.SourceType == b.SourceType &&
		a.AccountID == b.AccountID &&
		a.Network == b.Network &&
		deliveryEndpointRouteKey(a) == deliveryEndpointRouteKey(b) &&
		a.ConversationID == b.ConversationID &&
		a.ProviderChatID == b.ProviderChatID
}

func deliveryEndpointRouteKey(target DeliveryTarget) string {
	if target.Network == "email" {
		return NormalizeIdentifierForCompare(target.Endpoint)
	}
	return target.Endpoint
}

func deliveryHasEquivalentRestrictionTx(
	ctx context.Context, tx *loggedTx, personUID string, target DeliveryTarget,
) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT target_json FROM delivery_policy_overrides WHERE person_uid=? AND policy=?`,
		personUID, DeliveryDraftOnly,
	)
	if err != nil {
		return false, fmt.Errorf("query restrictive delivery overrides: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return false, fmt.Errorf("scan restrictive delivery override: %w", err)
		}
		var stored DeliveryTarget
		if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
			return false, fmt.Errorf("decode restrictive delivery override target: %w", err)
		}
		if deliveryTargetsShareRoute(target, stored) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate restrictive delivery overrides: %w", err)
	}
	return false, nil
}

func (s *Store) SetDeliveryPolicyContext(ctx context.Context, w DeliveryPolicyWrite) (*DeliveryPolicyReceipt, error) {
	return s.writeDeliveryPolicy(ctx, w, false)
}
func (s *Store) ClearDeliveryPolicyContext(ctx context.Context, w DeliveryPolicyWrite) (*DeliveryPolicyReceipt, error) {
	return s.writeDeliveryPolicy(ctx, w, true)
}
func (s *Store) writeDeliveryPolicy(ctx context.Context, w DeliveryPolicyWrite, reset bool) (*DeliveryPolicyReceipt, error) {
	if err := ValidateDeliveryPolicyQuery(w.Query); err != nil {
		return nil, err
	}
	if (!reset && w.Policy != DeliveryDraftOnly && w.Policy != DeliverySendAllowed) || w.ExpectedRevision < 1 || w.ExpectedRevision > 9007199254740991 || !deliveryStringValid(w.Actor) || !deliveryStringValid(w.Reason) {
		return nil, ErrDeliveryPolicyInvalid
	}
	if !reset && w.Query.Target == nil && w.ScopeAcknowledgement != "person_all_routes" {
		return nil, fmt.Errorf("acknowledge person_all_routes: %w", ErrDeliveryPolicyInvalid)
	}
	targetJSON, err := deliveryJSON(w.Query.Target)
	if err != nil {
		return nil, err
	}
	var receipt *DeliveryPolicyReceipt
	err = s.withDeliveryTx(ctx, func(tx *loggedTx) error {
		before, err := s.deliveryStateTx(ctx, tx, w.Query)
		if err != nil {
			return err
		}
		if before.PolicyRevision != w.ExpectedRevision {
			return ErrDeliveryPolicyRevisionConflict
		}
		if !reset && w.Policy == DeliverySendAllowed {
			if before.PersonRevision != w.ExpectedPersonRevision || before.BindingDigest == "" || before.BindingDigest != w.BindingDigest {
				return ErrDeliveryTargetChanged
			}
			if w.Query.Target != nil {
				_, _, reason, err := s.resolveDeliveryTargetTx(ctx, tx, before, w.Query.Target)
				if err != nil {
					return err
				}
				if reason != "" {
					return ErrDeliveryTargetChanged
				}
			}
		}
		pv, _, err := deliveryVersion(ctx, tx, string(AttributeObjectPerson), before.PersonID)
		if err != nil {
			return err
		}
		var generation int64
		if err = tx.QueryRowContext(ctx, `SELECT generation FROM delivery_admission_lock WHERE singleton=1`).Scan(&generation); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO delivery_policy_persons(person_uid,policy,revision) VALUES (?,'draft_only',1) ON CONFLICT(person_uid) DO NOTHING`, w.Query.PersonUID); err != nil {
			return err
		}
		if w.Query.Target == nil {
			next := w.Policy
			if reset {
				next = DeliveryDraftOnly
			}
			_, err = tx.ExecContext(ctx, `UPDATE delivery_policy_persons SET policy=?,revision=revision+1,person_revision=?,person_binding_version=?,approved_generation=?,default_explicit=?,identity_revision=? WHERE person_uid=?`, next, before.PersonRevision, pv, generation, !reset, before.IdentityRevision, w.Query.PersonUID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE delivery_policy_persons SET revision=revision+1 WHERE person_uid=?`, w.Query.PersonUID)
			if err != nil {
				return err
			}
			if reset {
				_, err = tx.ExecContext(ctx, `DELETE FROM delivery_policy_overrides WHERE person_uid=? AND scope_key=?`, w.Query.PersonUID, before.Scope)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO delivery_policy_overrides(person_uid,scope_key,target_json,policy,binding_digest) VALUES (?,?,?,?,?) ON CONFLICT(person_uid,scope_key) DO UPDATE SET policy=excluded.policy,binding_digest=excluded.binding_digest,target_json=excluded.target_json`, w.Query.PersonUID, before.Scope, targetJSON, w.Policy, before.BindingDigest)
			}
		}
		if err != nil {
			return err
		}
		after, err := s.deliveryStateTx(ctx, tx, w.Query)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, `INSERT INTO delivery_policy_audit(person_uid,revision,scope_key,target_json,before_policy,after_policy,actor,reason,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, w.Query.PersonUID, after.PolicyRevision, before.Scope, targetJSON, before.StoredPolicy, after.StoredPolicy, w.Actor, w.Reason, now)
		if err != nil {
			return err
		}
		receipt = &DeliveryPolicyReceipt{Before: *before, After: *after, Audit: DeliveryPolicyAudit{Actor: w.Actor, Reason: w.Reason, CreatedAt: now, Scope: before.Scope, BeforePolicy: before.StoredPolicy, AfterPolicy: after.StoredPolicy}}
		return nil
	})
	return receipt, err
}
