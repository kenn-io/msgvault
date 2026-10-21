package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FullObservationWhere admits exact full-room support or independent evidence.
// Legacy chat observations with no support edges cannot borrow another room.
// The alias is a trusted SQL identifier, never caller-provided data.
func FullObservationWhere(alias string) string {
	return `(` + alias + `.source IN ('user','carddav_import','vcard_import') OR NOT EXISTS (SELECT 1 FROM sources observation_source WHERE observation_source.id=` + alias + `.source_id AND observation_source.source_type IN ('beeper','slack','slackdump','discord','teams')) OR EXISTS (SELECT 1 FROM participant_contact_observation_conversations observation_room JOIN conversations observation_conversation ON observation_conversation.id=observation_room.conversation_id WHERE observation_room.observation_id=` + alias + `.id AND observation_conversation.source_id=` + alias + `.source_id AND ` + FullConversationWhere("observation_conversation") + `))`
}

func fullObservationTx(ctx context.Context, tx *loggedTx, id int64) (bool, error) {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participant_contact_observations o WHERE o.id=? AND o.active_until IS NULL AND o.superseded_at IS NULL AND `+FullObservationWhere("o")+`)`, id).Scan(&eligible)
	return eligible, err
}

// ObservationFullEligibleContext is the producer-side admission check. Automatic
// candidate lookups apply the same predicate to the peer observations.
func (s *Store) ObservationFullEligibleContext(ctx context.Context, id int64) (bool, error) {
	var eligible bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participant_contact_observations o WHERE o.id=? AND o.active_until IS NULL AND o.superseded_at IS NULL AND `+FullObservationWhere("o")+`)`, id).Scan(&eligible)
	return eligible, err
}
func (s *Store) ListFullParticipantObservationsContext(ctx context.Context, pid int64) ([]ParticipantContactObservation, error) {
	return s.queryParticipantObservationsContext(ctx, participantObservationSelect+` WHERE o.participant_id=? AND o.active_until IS NULL AND o.superseded_at IS NULL AND `+FullObservationWhere("o")+` ORDER BY o.id`, pid)
}
func (s *Store) ConversationFullEligibleContext(ctx context.Context, id int64) (bool, error) {
	var eligible bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND `+FullConversationWhere("c")+`)`, id).Scan(&eligible)
	return eligible, err
}

func observationOriginFullTx(
	ctx context.Context,
	tx *loggedTx,
	sourceID, conversationID *int64,
) (bool, error) {
	if conversationID != nil {
		if sourceID == nil {
			return false, errors.New("observation conversation must belong to its source")
		}
		var belongs bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM conversations c WHERE c.id=? AND c.source_id=?
		)`, *conversationID, *sourceID).Scan(&belongs); err != nil {
			return false, err
		}
		if !belongs {
			return false, errors.New("observation conversation must belong to its source")
		}
		var eligible bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM conversations c
			WHERE c.id=? AND c.source_id=? AND `+FullConversationWhere("c")+`
		)`, *conversationID, *sourceID).Scan(&eligible)
		return eligible, err
	}
	if sourceID == nil {
		return true, nil
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sources s WHERE s.id=?
		  AND s.source_type NOT IN ('beeper','slack','slackdump','discord','teams')
	)`, *sourceID).Scan(&eligible)
	return eligible, err
}

func (s *Store) withPolicyAttributionTxContext(ctx context.Context, fn func(*loggedTx) error) error {
	if !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyExclusive)
		if err != nil {
			return err
		}
		defer release()
	}
	return s.withAttributionPreFenceTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		if s.IsPostgreSQL() {
			return s.LockPostgresConversationPolicyTx(ctx, tx.Tx, ConversationPolicyExclusive)
		}
		return nil
	}, fn)
}

func (s *Store) recordObservationConversationTx(ctx context.Context, tx *loggedTx, observation *ParticipantContactObservation, room *int64) (bool, error) {
	if room == nil {
		return false, nil
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE id=? AND source_id=?)`, *room, observation.SourceID).Scan(&valid); err != nil {
		return false, err
	}
	if !valid {
		return false, errors.New("observation conversation must belong to its source")
	}
	before, err := fullObservationTx(ctx, tx, observation.Envelope.ID)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO participant_contact_observation_conversations(observation_id,conversation_id) VALUES(?,?) ON CONFLICT(observation_id,conversation_id) DO NOTHING`, observation.Envelope.ID, *room)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	after, err := fullObservationTx(ctx, tx, observation.Envelope.ID)
	if err != nil {
		return false, err
	}
	if before != after {
		people, err := personIDsForParticipantsTx(ctx, tx, []int64{observation.ParticipantID})
		if err != nil {
			return false, err
		}
		if err = s.invalidateConversationPolicyPersonInputsTx(ctx, tx, people); err != nil {
			return false, err
		}
		if err = s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
			return false, err
		}
		if err = s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return false, err
		}
		if _, err = s.bumpIdentityRevisionContext(ctx, tx); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE person_contact_state SET dirty_at=`+s.dialect.Now()+` WHERE dirty_at IS NULL AND EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id=person_contact_state.person_id AND pp.participant_id=?)`, observation.ParticipantID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Automatic identity publication uses a shared policy fence before its normal
// identity/sync locks. Every check runs on this actual write transaction.
func (s *Store) withConversationPolicyIdentityTx(ctx context.Context, fn func(*loggedTx) error) error {
	if !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyShared)
		if err != nil {
			return err
		}
		defer release()
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
		if s.IsPostgreSQL() {
			if err := s.LockPostgresConversationPolicyTx(ctx, tx.Tx, ConversationPolicyShared); err != nil {
				return err
			}
		}
		return s.lockIdentityMutationTxContext(ctx, tx)
	}, fn)
}

func (s *Store) checkObservationCandidateInputTx(ctx context.Context, tx *loggedTx, input IdentityMatchCandidateInput, left, right int64, serviceID any) error {
	var eligible bool
	predicate := `peer.provider_user_id=?`
	peerScope := ""
	if input.Basis == IdentityMatchServiceScopeUsername {
		predicate = `peer.normalized_value=? AND peer.address_kind='username' AND peer.service_id=?`
		peerScope = ` AND NOT (
          ((origin.scope_kind IS NULL AND peer.scope_kind IS NULL) OR
           (origin.scope_kind IS NOT NULL AND peer.scope_kind IS NOT NULL AND origin.scope_kind=peer.scope_kind))
          AND ((origin.scope_value IS NULL AND peer.scope_value IS NULL) OR
           (origin.scope_value IS NOT NULL AND peer.scope_value IS NOT NULL AND origin.scope_value=peer.scope_value)))`
	}
	args := []any{input.ObservationID, left, right, input.NormalizedValue}
	if input.Basis == IdentityMatchServiceScopeUsername {
		args = append(args, serviceID)
	}
	args = append(args, left, right, input.NormalizedValue)
	if input.Basis == IdentityMatchServiceScopeUsername {
		args = append(args, serviceID)
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participant_contact_observations origin WHERE origin.id=? AND origin.participant_id IN (?,?)
    AND origin.active_until IS NULL AND origin.superseded_at IS NULL AND `+FullObservationWhere("origin")+` AND `+strings.ReplaceAll(predicate, "peer.", "origin.")+`
    AND EXISTS(SELECT 1 FROM participant_contact_observations peer WHERE peer.participant_id IN (?,?) AND peer.participant_id<>origin.participant_id
    AND peer.active_until IS NULL AND peer.superseded_at IS NULL AND `+FullObservationWhere("peer")+` AND `+predicate+peerScope+`))`, args...).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrConversationPolicyStale
	}
	return nil
}

// Existing candidate endpoints and value identify its observation pair; each
// observation must independently have live exact-room support. The stored
// source relation selects chat-generated candidates, never supplies support.
func (s *Store) checkAutomaticObservationCandidateTx(ctx context.Context, tx *loggedTx, candidate *IdentityMatchCandidate) error {
	if candidate.Source != ProvenanceArchiveObservation || candidate.LeftKind != IdentityMatchParticipant || candidate.RightKind != IdentityMatchParticipant {
		return nil
	}
	var stale bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM identity_match_candidates c WHERE c.id=?
    AND EXISTS(SELECT 1 FROM identity_match_candidate_sources cs JOIN sources src ON src.id=cs.source_id WHERE cs.candidate_id=c.id AND src.source_type IN ('beeper','slack','slackdump','discord','teams'))
    AND NOT `+currentCandidateObservationPairSQL+`)`, candidate.ID).Scan(&stale)
	if err != nil {
		return err
	}
	if stale {
		return ErrConversationPolicyStale
	}
	return nil
}

// Evidence retains the revision captured before gathering. Re-reading a newer
// revision at publication would incorrectly bless work from the old policy.
func (s *Store) checkIdentityMatchEvidenceAdmissionTx(ctx context.Context, tx *loggedTx, candidateID int64, input IdentityMatchEvidenceInput) error {
	admission := input.Admission
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS BIGINT) FROM archive_metadata WHERE key=?),0)`, conversationPolicyRevisionKey).Scan(&revision); err != nil {
		return err
	}
	if revision != admission.PolicyRevision {
		return ErrConversationPolicyStale
	}
	candidate, err := getIdentityMatchCandidateWithoutEvidenceTx(ctx, tx, candidateID)
	if err != nil {
		return err
	}
	if candidate.LeftKind != IdentityMatchParticipant || candidate.RightKind != IdentityMatchParticipant {
		return ErrConversationPolicyStale
	}
	var eligible bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM participant_contact_observations o WHERE o.id=? AND o.participant_id IN (?,?) AND o.active_until IS NULL AND o.superseded_at IS NULL AND `+FullObservationWhere("o")+`)`, admission.ObservationID, candidate.LeftID, candidate.RightID).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return ErrConversationPolicyStale
	}
	detail := ""
	if input.Detail != nil {
		detail = *input.Detail
	}
	pair := `SELECT EXISTS(SELECT 1 FROM participant_contact_observations a JOIN participant_contact_observations b ON TRUE
 WHERE a.participant_id=? AND b.participant_id=? AND a.active_until IS NULL AND a.superseded_at IS NULL
 AND b.active_until IS NULL AND b.superseded_at IS NULL AND ` + FullObservationWhere("a") + ` AND ` + FullObservationWhere("b")
	args := []any{candidate.LeftID, candidate.RightID}
	switch input.EvidenceKind {
	case "email", "phone":
		pair += ` AND a.address_kind=? AND b.address_kind=? AND a.normalized_value=? AND b.normalized_value=?`
		args = append(args, input.EvidenceKind, input.EvidenceKind, detail, detail)
	case "stable_provider_id":
		pair += ` AND a.provider_user_id=? AND b.provider_user_id=?`
		args = append(args, detail, detail)
	case "service_scope_username":
		var serviceID int64
		if candidate.ServiceSlug == nil {
			return ErrConversationPolicyStale
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM communication_services WHERE slug=?`, *candidate.ServiceSlug).Scan(&serviceID); err != nil {
			return err
		}
		return s.checkObservationCandidateInputTx(ctx, tx, IdentityMatchCandidateInput{ObservationID: admission.ObservationID, Basis: IdentityMatchServiceScopeUsername, NormalizedValue: candidate.NormalizedValue}, candidate.LeftID, candidate.RightID, serviceID)
	case "display_name":
		var leftName, rightName string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(a.display_name,''),COALESCE(b.display_name,''), (`+FullParticipantNameWhere("a.id", "a.display_name")+` AND `+FullParticipantNameWhere("b.id", "b.display_name")+`) FROM participants a JOIN participants b ON TRUE WHERE a.id=? AND b.id=?`, candidate.LeftID, candidate.RightID).Scan(&leftName, &rightName, &eligible); err != nil {
			return err
		}
		if !eligible || leftName == "" || !strings.EqualFold(leftName, rightName) || !strings.EqualFold(leftName, detail) {
			return ErrConversationPolicyStale
		}
	case "conversation_membership":
		ref := ""
		if input.EvidenceRef != nil {
			ref = *input.EvidenceRef
		}
		room, err := strconv.ParseInt(strings.TrimPrefix(ref, "conversation:"), 10, 64)
		if err != nil || !strings.HasPrefix(ref, "conversation:") {
			return ErrConversationPolicyStale
		}
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND `+FullConversationWhere("c")+` AND EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=c.id AND cp.participant_id=?) AND EXISTS(SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=c.id AND cp.participant_id=?))`, room, candidate.LeftID, candidate.RightID).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return ErrConversationPolicyStale
		}
		return nil
	default:
		return ErrConversationPolicyStale
	}
	err = tx.QueryRowContext(ctx, pair+`)`, args...).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return ErrConversationPolicyStale
	}
	return nil
}

// Reconcile only a room whose eligibility just became Full. Ordinary duplicate
// captures retain their no-op path, including all repeated roster replays.
func (s *Store) reconcilePromotedConversationConflictsTx(ctx context.Context, tx *loggedTx, roomID int64) error {
	rows, err := tx.QueryContext(ctx, participantObservationSelect+` WHERE o.active_until IS NULL AND o.superseded_at IS NULL
  AND EXISTS (SELECT 1 FROM participant_contact_observation_conversations support WHERE support.observation_id=o.id AND support.conversation_id=?)
  AND `+FullObservationWhere("o"), roomID)
	if err != nil {
		return fmt.Errorf("load promoted room observations: %w", err)
	}
	observations, err := scanParticipantContactObservations(rows)
	if err != nil {
		return err
	}
	for i := range observations {
		observation := &observations[i]
		var serviceID any
		if observation.ServiceSlug != nil {
			var id int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM communication_services WHERE slug=?`, *observation.ServiceSlug).Scan(&id); err != nil {
				return err
			}
			serviceID = id
		}
		if _, err := s.recordObservationConflictsTx(ctx, tx, observation, serviceID); err != nil {
			return err
		}
	}
	return nil
}

// A raw participant row can combine several providers after archival merges.
// Eligibility therefore belongs to the observed value, not to that whole row.
func participantChatProvenanceWhere(id string) string {
	// Keep sender and recipient checks separate so SQLite can use each index.
	const chatSources = `'beeper','slack','slackdump','discord','teams'`
	return `(EXISTS(SELECT 1 FROM conversation_participants policy_member JOIN conversations policy_room ON policy_room.id=policy_member.conversation_id JOIN sources policy_source ON policy_source.id=policy_room.source_id WHERE policy_member.participant_id=` + id + ` AND policy_source.source_type IN (` + chatSources + `)) OR EXISTS(SELECT 1 FROM participant_identifiers policy_identifier WHERE policy_identifier.participant_id=` + id + ` AND policy_identifier.identifier_type IN (` + chatSources + `)) OR EXISTS(SELECT 1 FROM participant_contact_observations policy_observation JOIN sources policy_source ON policy_source.id=policy_observation.source_id WHERE policy_observation.participant_id=` + id + ` AND policy_source.source_type IN (` + chatSources + `)) OR EXISTS(SELECT 1 FROM messages policy_message JOIN sources policy_source ON policy_source.id=policy_message.source_id WHERE policy_message.sender_id=` + id + ` AND policy_source.source_type IN (` + chatSources + `)) OR EXISTS(SELECT 1 FROM message_recipients policy_recipient JOIN messages policy_message ON policy_message.id=policy_recipient.message_id JOIN sources policy_source ON policy_source.id=policy_message.source_id WHERE policy_recipient.participant_id=` + id + ` AND policy_source.source_type IN (` + chatSources + `)))`
}

// FullParticipantValueWhere returns the SQL predicate for using one exact
// participant email or phone value. Chat-derived values need support in a
// Full room or independent curated evidence; an eligible room membership alone
// cannot authorize another value on a merged participant row.
func FullParticipantValueWhere(id, kind, value string) string {
	return `(NOT ` + participantChatProvenanceWhere(id) + ` OR EXISTS(SELECT 1 FROM participant_contact_observations value_observation WHERE value_observation.participant_id=` + id + ` AND value_observation.address_kind=` + kind + ` AND value_observation.normalized_value=LOWER(` + value + `) AND value_observation.active_until IS NULL AND value_observation.superseded_at IS NULL AND ` + FullObservationWhere("value_observation") + `) OR EXISTS(SELECT 1 FROM person_participants value_person JOIN person_contact_points value_contact ON value_contact.person_id=value_person.person_id WHERE value_person.participant_id=` + id + ` AND value_contact.address_kind=` + kind + ` AND value_contact.normalized_value=LOWER(` + value + `) AND value_contact.source IN ('user','carddav_import','vcard_import') AND value_contact.active_until IS NULL AND value_contact.superseded_at IS NULL) OR (` + kind + `='email' AND EXISTS(SELECT 1 FROM message_recipients value_recipient JOIN messages value_message ON value_message.id=value_recipient.message_id WHERE value_recipient.participant_id=` + id + ` AND LOWER(value_recipient.email_address)=LOWER(` + value + `) AND ` + LiveMessagesWhere("value_message", true) + ` AND ` + FullMessageWhere("value_message") + `)) OR (` + kind + `='phone' AND EXISTS(SELECT 1 FROM participant_identifiers native_phone WHERE native_phone.participant_id=` + id + ` AND native_phone.identifier_value=` + value + ` AND native_phone.identifier_type IN ('imessage','whatsapp','sms','synctech_sms','imazing_csv','google_voice','google-voice','google_messages','google-messages') AND ` + nativeParticipantIdentifierHistoryWhere("native_phone") + `)))`
}

// Identifier classification is persisted by the existing provider normalizers.
// Equal opaque provider IDs alone do not identify a namespace or an account.
func (s *Store) fullParticipantIdentifierWhere(alias string) string {
	return fullParticipantIdentifierWhere(alias, s.IsPostgreSQL())
}

// FullParticipantIdentifierWhereSQLite returns the SQL predicate used by
// participant identity projections sourced from a SQLite archive.
func FullParticipantIdentifierWhereSQLite(alias string) string {
	return fullParticipantIdentifierWhere(alias, false)
}

// FullParticipantIdentifierWherePostgres returns the participant identifier
// eligibility predicate for PostgreSQL archive projections.
func FullParticipantIdentifierWherePostgres(alias string) string {
	return fullParticipantIdentifierWhere(alias, true)
}

func fullParticipantIdentifierWhere(alias string, postgres bool) string {
	const observation = "identifier_observation"
	const source = "identifier_source"
	nullSafeEqual := "IS"
	if postgres {
		nullSafeEqual = "IS NOT DISTINCT FROM"
	}
	classification := observation + `.service_id ` + nullSafeEqual + ` ` + alias + `.service_id
 AND ` + observation + `.scope_kind ` + nullSafeEqual + ` ` + alias + `.scope_kind
 AND ` + observation + `.scope_value ` + nullSafeEqual + ` ` + alias + `.scope_value`
	// Beeper's identifier is its account/user fallback anchor; provider_user_id
	// may instead be a native provider key. The recorder's source_ref binds the
	// raw user to the source account. Rebuild the existing length-delimited anchor
	// using byte lengths, while SUBSTR indexes the textual source-ref prefix.
	prefix := `('beeper:' || ` + source + `.identifier || ':')`
	user := `SUBSTR(` + observation + `.source_ref,LENGTH(` + prefix + `)+1)`
	byteLength := func(value string) string {
		if postgres {
			return `OCTET_LENGTH(` + value + `)`
		}
		return `LENGTH(CAST(` + value + ` AS BLOB))`
	}
	anchor := `('beeper:' || CAST(` + byteLength(source+`.identifier`) + ` AS TEXT) || ':' || ` + source + `.identifier || ':' || CAST(` + byteLength(user) + ` AS TEXT) || ':' || ` + user + `)`
	beeper := alias + `.identifier_type='beeper' AND ` + source + `.source_type='beeper'
 AND SUBSTR(` + observation + `.source_ref,1,LENGTH(` + prefix + `))=` + prefix + `
 AND LENGTH(` + user + `)>0 AND ` + alias + `.identifier_value=` + anchor
	provider := alias + `.identifier_type<>'beeper' AND ` + observation + `.provider_user_id=` + alias + `.identifier_value
 AND (` + source + `.source_type=` + alias + `.identifier_type
 OR (` + source + `.source_type='slackdump' AND ` + alias + `.identifier_type='slack')
 OR EXISTS(SELECT 1 FROM communication_services identifier_service WHERE identifier_service.id=` + alias + `.service_id AND identifier_service.slug=` + source + `.source_type))`
	address := observation + `.normalized_value=` + alias + `.identifier_value AND ` + observation + `.address_kind=` + alias + `.identifier_type`
	return `(NOT ` + participantChatProvenanceWhere(alias+".participant_id") + ` OR ` + nativeParticipantIdentifierHistoryWhere(alias) + ` OR EXISTS(
 SELECT 1 FROM participant_contact_observations ` + observation + `
 LEFT JOIN sources ` + source + ` ON ` + source + `.id=` + observation + `.source_id
 WHERE ` + observation + `.participant_id=` + alias + `.participant_id
 AND ` + observation + `.active_until IS NULL AND ` + observation + `.superseded_at IS NULL
 AND ` + FullObservationWhere(observation) + `
 AND ((` + classification + ` AND ((` + beeper + `) OR (` + provider + `))) OR (` + address + `))))`
}

// Native import evidence is tied to one exact stored identifier and a message
// from the corresponding non-chat provider. This uses existing archive rows so
// older imports retain their independent phone/service support after a chat
// room becomes Cold.
func nativeParticipantIdentifierHistoryWhere(identifier string) string {
	typeValue := identifier + `.identifier_type`
	providerMatch := `(native_source.source_type=` + typeValue + `
 OR (native_source.source_type='apple_messages' AND ` + typeValue + `='imessage')
 OR (native_source.source_type IN ('synctech-sms','synctech_sms') AND ` + typeValue + `='sms')
 OR (native_source.source_type IN ('google_messages','google-messages') AND ` + typeValue + ` IN ('google_messages','google-messages'))
 OR EXISTS(SELECT 1 FROM communication_services native_service WHERE native_service.slug=native_source.source_type AND native_service.slug=` + typeValue + `))`
	return `EXISTS(SELECT 1 FROM messages native_message JOIN sources native_source ON native_source.id=native_message.source_id
	 WHERE native_source.source_type IN ('apple_messages','imessage','whatsapp','sms','synctech-sms','synctech_sms','google_messages','google-messages','imazing_csv','google_voice','google-voice')
 AND ` + providerMatch + `
 AND (native_message.sender_id=` + identifier + `.participant_id OR EXISTS(SELECT 1 FROM message_recipients native_recipient WHERE native_recipient.message_id=native_message.id AND native_recipient.participant_id=` + identifier + `.participant_id)))`
}

// FullParticipantNameWhere returns the SQL predicate for using one exact raw
// participant display name. Chat-derived names require a matching display
// value on a live Full message or a surviving Full-room name observation.
// Exact native and curated name observations remain independent support; a
// sender or roster ID alone cannot attest to the name stored on its shared row.
// Curated profile names are read separately from this fallback.
func FullParticipantNameWhere(id, value string) string {
	return `(NOT ` + participantChatProvenanceWhere(id) + ` OR EXISTS(
 SELECT 1 FROM participant_contact_observations name_observation
 WHERE name_observation.participant_id=` + id + ` AND name_observation.address_kind='display_name'
 AND name_observation.normalized_value=` + value + `
 AND name_observation.active_until IS NULL AND name_observation.superseded_at IS NULL
 AND ` + FullObservationWhere("name_observation") + `) OR EXISTS(
 SELECT 1 FROM message_recipients name_recipient JOIN messages name_message ON name_message.id=name_recipient.message_id
 WHERE name_recipient.participant_id=` + id + ` AND name_recipient.display_name=` + value + `
 AND ` + FullMessageWhere("name_message") + ` AND ` + LiveMessagesWhere("name_message", true) + `))`
}

// FullParticipantDisplaySQL renders the eligible display fallback for a
// participant row joined under alias. Keep embedding assembly and publication
// digest checks on the same value-level policy.
func FullParticipantDisplaySQL(alias string) string {
	name := `CASE WHEN ` + FullParticipantNameWhere(alias+`.id`, alias+`.display_name`) + ` THEN ` + alias + `.display_name END`
	email := `CASE WHEN ` + FullParticipantValueWhere(alias+`.id`, `'email'`, alias+`.email_address`) + ` THEN ` + alias + `.email_address END`
	phone := `CASE WHEN ` + FullParticipantValueWhere(alias+`.id`, `'phone'`, alias+`.phone_number`) + ` THEN ` + alias + `.phone_number END`
	return `COALESCE(NULLIF(TRIM(` + name + `), ''), NULLIF(` + email + `, ''), NULLIF(` + phone + `, ''), '')`
}

// Envelope support is shared by contextual blocks in every Full room. Revoke
// their stamps before removing support, inside the same write transaction.
// Selection fragments are fixed SQL supplied by Store mutation paths.
func invalidateEnvelopeContextWith(q querier, selection string, args ...any) error {
	_, err := q.Exec(envelopeContextInvalidationSQL(selection, ""), args...)
	return err
}

func invalidateEnvelopeContextWithIncomingParticipants(
	q querier, dialect Dialect, selection string, args []any, supportMessageID int64, participantIDs []int64,
) error {
	seen := make(map[int64]struct{}, len(participantIDs))
	uniqueIDs := make([]int64, 0, len(participantIDs))
	for _, id := range participantIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return invalidateEnvelopeContextWith(q, selection, args...)
	}
	encodedIDs, err := json.Marshal(uniqueIDs)
	if err != nil {
		return fmt.Errorf("encode incoming envelope participants: %w", err)
	}
	incomingParticipants := `SELECT CAST(value AS BIGINT) AS participant_id FROM json_each(?)`
	if dialect.DriverName() == postgresDriverName {
		incomingParticipants = `SELECT CAST(value AS BIGINT) AS participant_id
 FROM jsonb_array_elements_text(CAST(? AS JSONB)) AS incoming(value)`
	}
	queryArgs := []any{string(encodedIDs)}
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, supportMessageID)
	_, err = q.Exec(envelopeContextInvalidationSQL(selection, incomingParticipants), queryArgs...)
	return err
}

func envelopeContextInvalidationSQL(selection, incomingParticipants string) string {
	incomingCTE := ""
	incomingSelection := ""
	if incomingParticipants != "" {
		incomingCTE = `incoming_participants AS (` + incomingParticipants + `), `
		incomingSelection = ` UNION SELECT incoming.participant_id
 FROM incoming_participants incoming
 WHERE EXISTS(SELECT 1 FROM messages incoming_support WHERE incoming_support.id=? AND ` + LiveMessagesWhere("incoming_support", true) + ` AND ` + FullMessageWhere("incoming_support") + `)`
	}
	return `WITH ` + incomingCTE + `affected_participants AS (
 SELECT mr.participant_id FROM message_recipients mr
 JOIN messages support ON support.id=mr.message_id
 WHERE ` + selection + ` AND ` + LiveMessagesWhere("support", true) + ` AND ` + FullMessageWhere("support") + incomingSelection + `
 ), affected_rooms AS (
 SELECT conversation_id FROM conversation_participants WHERE participant_id IN (SELECT participant_id FROM affected_participants)
 UNION SELECT conversation_id FROM messages WHERE sender_id IN (SELECT participant_id FROM affected_participants)
 )
 UPDATE messages SET embed_gen=NULL, embedding_revision=embedding_revision+1
 WHERE message_type='beeper' AND conversation_id IN (SELECT conversation_id FROM affected_rooms)
 AND ` + FullMessageWhere("messages")
}

// tombstoneEnvelopeMessagesWith covers provider tombstones and dedup hiding.
// Keeping invalidation next to the UPDATE also covers compatibility writers.
func tombstoneEnvelopeMessagesWith(q querier, now, selection string, args ...any) error {
	if err := invalidateEnvelopeContextWith(q, selection, args...); err != nil {
		return err
	}
	_, err := q.Exec(`UPDATE messages SET deleted_from_source_at=`+now+` WHERE `+strings.ReplaceAll(selection, "support.", ""), args...)
	return err
}

func tombstoneSourceEnvelopesWith(q querier, now string, sourceID int64, ids []string) error {
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := []any{sourceID}
		for _, id := range chunk {
			args = append(args, id)
		}
		if err := tombstoneEnvelopeMessagesWith(q, now, "support.source_id=? AND support.source_message_id IN ("+sqlPlaceholders(len(chunk))+") AND support.deleted_from_source_at IS NULL", args...); err != nil {
			return err
		}
	}
	return nil
}

// Exact replays retain their existing contextual stamps. Compare in bounded
// chunks so large provider envelopes retain the insert path's parameter bound.
func messageRecipientEnvelopeUnchangedWith(q querier, messageID int64, rs RecipientSet) (bool, error) {
	rows := normalizedMessageRecipients(rs)
	var count int
	if err := q.QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type=?`, messageID, rs.Type).Scan(&count); err != nil {
		return false, err
	}
	if count != len(rows) {
		return false, nil
	}
	for start := 0; start < len(rows); start += 300 {
		chunk := rows[start:min(start+300, len(rows))]
		args := []any{messageID, rs.Type}
		pairs := make([]string, len(chunk))
		for i, row := range chunk {
			pairs[i] = "(CAST(? AS BIGINT),CAST(? AS TEXT),CAST(? AS TEXT))"
			args = append(args, row.participantID, row.displayName, row.email)
		}
		var matched int
		if err := q.QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type=? AND (participant_id,COALESCE(display_name,''),COALESCE(email_address,'')) IN (VALUES `+strings.Join(pairs, ",")+`)`, args...).Scan(&matched); err != nil {
			return false, err
		}
		if matched != len(chunk) {
			return false, nil
		}
	}
	return true, nil
}
