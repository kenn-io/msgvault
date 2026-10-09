package store

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
	"time"
)

const deliverySourceEmailEvidenceQuery = `
SELECT EXISTS(
  SELECT 1
  FROM messages m
  WHERE m.sender_id=?
    AND m.source_id=?
    AND m.message_type='email'
    AND m.deleted_from_source_at IS NULL
) OR EXISTS(
  SELECT 1
  FROM message_recipients mr
  JOIN messages m ON m.id=mr.message_id
  WHERE mr.participant_id=?
    AND m.source_id=?
    AND m.message_type='email'
    AND m.deleted_from_source_at IS NULL
)`

// This resolver only consumes native evidence. Current email providers accept a
// reviewed mailbox; chat sending stays closed until both the native PR1161
// route producer and the source lifecycle contract are available to a sender.
func (s *Store) resolveDeliveryTargetTx(ctx context.Context, tx *loggedTx, state *DeliveryPolicyState, t *DeliveryTarget) (digest string, generation int64, reason string, err error) {
	invalid := func(r string) (string, int64, string, error) { return "", 0, r, nil }
	var sourceType, account string
	var config, oauth, googleUserID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT source_type,identifier,sync_config,oauth_app,google_user_id FROM sources WHERE id=?`, t.SourceID).Scan(&sourceType, &account, &config, &oauth, &googleUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("unknown_source")
	}
	if err != nil {
		return digest, generation, reason, err
	}
	if sourceType != t.SourceType || account != t.AccountID {
		return invalid("source_binding_changed")
	}
	// Lifecycle is optional on this base. When the native table arrives, a
	// history-only/merged/retired account can never authorize a provider send.
	present, tableErr := s.deliveryTableExistsTx(ctx, tx, "source_settings")
	if tableErr != nil {
		err = tableErr
		return digest, generation, reason, err
	}
	if present {
		var history bool
		var merged sql.NullInt64
		var retired sql.NullTime
		e := tx.QueryRowContext(ctx, `SELECT history_only,merged_into_source_id,retired_at FROM source_settings WHERE source_id=?`, t.SourceID).Scan(&history, &merged, &retired)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			err = e
			return digest, generation, reason, err
		}
		if history || merged.Valid || retired.Valid {
			return invalid("history_only_source")
		}
	}
	var marker int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM archive_metadata WHERE key=?`, BeeperReanchorMarkerKey(t.SourceID)).Scan(&marker); err != nil {
		return digest, generation, reason, err
	}
	if marker != 0 {
		return invalid("source_reanchor_required")
	}
	if (sourceType != "gmail" && sourceType != "imap") || t.Network != "email" || t.ConversationID != 0 || t.ProviderChatID != "" {
		return invalid("unsupported_route")
	}
	address, e := mail.ParseAddress(t.Endpoint)
	if e != nil || address.Address != t.Endpoint || strings.ContainsAny(t.Endpoint, "\r\n") {
		return invalid("invalid_endpoint")
	}
	// Equivalent-route checks use normalized mailbox keys. Native binding checks
	// below continue to compare the stored value and endpoint exactly.
	routeKey := deliveryEndpointRouteKey(*t)
	var ambiguous int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM persons p WHERE p.id<>? AND (EXISTS(SELECT 1 FROM person_contact_points cp WHERE cp.person_id=p.id AND cp.address_kind='email' AND LOWER(cp.normalized_value)=? AND cp.active_until IS NULL AND cp.superseded_at IS NULL AND (cp.active_from IS NULL OR cp.active_from<=CURRENT_TIMESTAMP)) OR EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id=p.id AND EXISTS(SELECT 1 FROM participants a WHERE a.id=pp.participant_id AND LOWER(a.email_address)=?)))`, state.PersonID, routeKey, routeKey).Scan(&ambiguous); err != nil {
		return digest, generation, reason, err
	}
	if ambiguous > 0 {
		return invalid("ambiguous_endpoint")
	}
	var endpointVersion, endpointGeneration int64
	var native any
	if t.ContactPointID > 0 {
		var personID int64
		var kind, value string
		var serviceID sql.NullInt64
		var scopeKind, scopeValue sql.NullString
		var activeFrom, activeUntil, superseded sql.NullTime
		e = tx.QueryRowContext(ctx, `SELECT person_id,address_kind,normalized_value,service_id,scope_kind,scope_value,active_from,active_until,superseded_at FROM person_contact_points WHERE id=?`, t.ContactPointID).Scan(&personID, &kind, &value, &serviceID, &scopeKind, &scopeValue, &activeFrom, &activeUntil, &superseded)
		if errors.Is(e, sql.ErrNoRows) {
			return invalid("detached_endpoint")
		}
		if e != nil {
			err = e
			return digest, generation, reason, err
		}
		if personID != state.PersonID || kind != "email" || value != t.Endpoint || serviceID.Valid || scopeKind.Valid || scopeValue.Valid || activeUntil.Valid || superseded.Valid {
			return invalid("detached_endpoint")
		}
		if activeFrom.Valid && activeFrom.Time.After(deliveryNow()) {
			return invalid("inactive_endpoint")
		}
		endpointVersion, endpointGeneration, err = deliveryVersion(ctx, tx, "contact_point", t.ContactPointID)
		if err != nil {
			return digest, generation, reason, err
		}
		native = []any{personID, kind, value}
	} else {
		var email sql.NullString
		// This legacy text is evidence, not the native link graph's numeric root.
		var canonical sql.NullString
		var bound int
		e = tx.QueryRowContext(ctx, `SELECT email_address,canonical_id FROM participants WHERE id=?`, t.ParticipantID).Scan(&email, &canonical)
		if errors.Is(e, sql.ErrNoRows) {
			return invalid("unknown_participant")
		}
		if e != nil {
			err = e
			return digest, generation, reason, err
		}
		if !email.Valid || email.String != t.Endpoint {
			return invalid("detached_endpoint")
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_participants WHERE person_id=? AND participant_id=?`, state.PersonID, t.ParticipantID).Scan(&bound); err != nil {
			return digest, generation, reason, err
		}
		if bound != 1 {
			return invalid("unbound_participant")
		}
		// Archive evidence establishes identity/source association only. Provider
		// credentials and actual delivery remain the caller's separate obligation.
		var evidence bool
		if err = tx.QueryRowContext(ctx, deliverySourceEmailEvidenceQuery, t.ParticipantID, t.SourceID, t.ParticipantID, t.SourceID).Scan(&evidence); err != nil {
			return digest, generation, reason, err
		}
		if !evidence {
			return invalid("missing_source_evidence")
		}
		var evidenceVersion, evidenceGeneration int64
		if err = tx.QueryRowContext(ctx, `SELECT version,generation FROM delivery_source_email_evidence WHERE participant_id=? AND source_id=? AND evidence_present=TRUE`, t.ParticipantID, t.SourceID).Scan(&evidenceVersion, &evidenceGeneration); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("missing_source_evidence")
			}
			return digest, generation, reason, err
		}
		endpointVersion, endpointGeneration, err = deliveryVersion(ctx, tx, "participant", t.ParticipantID)
		if err != nil {
			return digest, generation, reason, err
		}
		native = []any{t.ParticipantID, email.String, canonical}
		// Evidence is an identity/source association. Its generation fences
		// approvals without turning archive proof into delivery permission.
		sourceVersion, sourceGeneration, e := deliveryVersion(ctx, tx, "source", t.SourceID)
		if e != nil {
			err = e
			return digest, generation, reason, err
		}
		personVersion, personGeneration, e := deliveryVersion(ctx, tx, string(AttributeObjectPerson), state.PersonID)
		if e != nil {
			err = e
			return digest, generation, reason, err
		}
		generation = max(endpointGeneration, sourceGeneration, personGeneration, evidenceGeneration)
		digest, err = deliveryDigest([]any{state.PersonUID, state.PersonID, state.PersonRevision, state.IdentityRevision, personVersion, t, native, endpointVersion, sourceVersion, evidenceVersion, evidenceGeneration, config, oauth, googleUserID})
		return digest, generation, "", err
	}
	sourceVersion, sourceGeneration, e := deliveryVersion(ctx, tx, "source", t.SourceID)
	if e != nil {
		err = e
		return digest, generation, reason, err
	}
	personVersion, personGeneration, e := deliveryVersion(ctx, tx, string(AttributeObjectPerson), state.PersonID)
	if e != nil {
		err = e
		return digest, generation, reason, err
	}
	generation = max(endpointGeneration, sourceGeneration, personGeneration)
	digest, err = deliveryDigest([]any{state.PersonUID, state.PersonID, state.PersonRevision, state.IdentityRevision, personVersion, t, native, endpointVersion, sourceVersion, config, oauth, googleUserID})
	return digest, generation, "", err
}
func deliveryNow() time.Time { return time.Now().UTC() }
func (s *Store) deliveryTableExistsTx(ctx context.Context, tx *loggedTx, name string) (bool, error) {
	var count int
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`
	if s.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?`
	}
	err := tx.QueryRowContext(ctx, query, name).Scan(&count)
	return count != 0, err
}
