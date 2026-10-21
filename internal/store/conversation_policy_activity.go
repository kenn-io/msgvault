package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const conversationPolicyActivityDirtyPrefix = "conversation_policy_activity_dirty:"

// ConversationLastActivityContext attributes activity from envelope/provider
// metadata, including cold history. The signal's time, not its parent's time,
// must be no later than now. Bodies and display names are never inputs.
func (s *Store) ConversationLastActivityContext(ctx context.Context, id int64, now time.Time) (time.Time, error) {
	return s.conversationLastActivityWith(ctx, s.db, id, now)
}

func (s *Store) conversationLastActivityWith(ctx context.Context, q accountIdentityQuerier, id int64, now time.Time) (time.Time, error) {
	if now.IsZero() {
		return time.Time{}, errors.New("policy time must be set")
	}
	var sourceID int64
	var sourceType, identifier, providerID, conversationType, metadata, failure string
	err := q.QueryRowContext(ctx, `SELECT c.source_id,src.source_type,src.identifier,COALESCE(c.source_conversation_id,''),c.conversation_type,COALESCE(CAST(c.metadata AS TEXT),''),COALESCE(srf.failure,'')
 FROM conversations c JOIN sources src ON src.id=c.source_id LEFT JOIN source_messaging_route_failures srf ON srf.source_id=c.source_id WHERE c.id=?`, id).Scan(&sourceID, &sourceType, &identifier, &providerID, &conversationType, &metadata, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	identities, err := conversationPolicySelfIdentitiesWith(ctx, q, sourceID)
	if err != nil {
		return time.Time{}, err
	}
	var selfIDs []int64
	for _, identity := range identities {
		selfIDs = append(selfIDs, identity.ParticipantIDs...)
	}
	// Provider-captured self proof has exact account/chat/network bindings.
	// Roster completeness describes size; it does not withdraw an independently
	// attributed self participant when the provider cannot count all members.
	if sourceType == sourceTypeBeeper && failure == "" {
		var m routeMetadata
		if json.Unmarshal([]byte(metadata), &m) == nil && m.Route != nil {
			e := m.Route
			route := MessagingRoute{AccountID: identifier, ProviderChatID: providerID, ConversationType: conversationType}
			checkRouteBindings(&route, e)
			// These room/roster failures preserve already attributed self IDs.
			// Account, chat, network and unknown proof failures never do.
			selfProofIntact := e.Failure == "" || e.Failure == "membership_fetch_failed" || e.Failure == "source_messages_deleted"
			selfProofBounded := len(e.SelfParticipantIDs) <= maxRouteEvidenceIDs &&
				(e.SelfParticipantIDsComplete || !e.Truncated || len(e.SelfParticipantIDs) < maxRouteEvidenceIDs)
			if len(route.Reasons) == 0 && selfProofIntact && selfProofBounded && !e.Merged && !e.ObservedAt.IsZero() && !e.ObservedAt.After(now.Add(time.Minute)) &&
				ValidatePersonMessagingRouteQuery(PersonMessagingRouteQuery{PersonUID: "activity", Network: e.Network}) == nil && e.Network != "" {
				for _, self := range e.SelfParticipantIDs {
					if self > 0 {
						selfIDs = append(selfIDs, self)
					}
				}
			}
		}
	}
	slices.Sort(selfIDs)
	selfIDs = slices.Compact(selfIDs)
	selfSQL := "SELECT CAST(NULL AS BIGINT) WHERE FALSE"
	args := make([]any, 0, len(selfIDs)+3)
	if len(selfIDs) > 0 {
		values := make([]string, len(selfIDs))
		for i, id := range selfIDs {
			values[i] = "(CAST(? AS BIGINT))"
			args = append(args, id)
		}
		selfSQL = "VALUES " + strings.Join(values, ",")
	}
	own := func(alias string) string {
		identity := strings.ReplaceAll(messageIdentityAttributionMatch, "messages.", alias+".")
		return `(COALESCE(` + alias + `.source_is_from_me,FALSE) OR ` + identity + ` OR (
   NOT EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=` + alias + `.id AND ` + usableFromEnvelope + `)
   AND (` + alias + `.sender_id IN (SELECT id FROM self_participants) OR EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=` + alias + `.id AND mr.recipient_type='from' AND mr.participant_id IN (SELECT id FROM self_participants)))))`
	}
	args = append(args, id, sourceID)
	query := `WITH self_participants(id) AS (` + selfSQL + `), signals AS (
 SELECT COALESCE(m.sent_at,m.received_at,m.internal_date) AS signal_time FROM messages m
 WHERE m.conversation_id=? AND m.source_id=?
 AND (` + own("m") + ` OR EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='mention' AND mr.participant_id IN (SELECT id FROM self_participants))
 OR EXISTS(SELECT 1 FROM messages parent WHERE parent.id=m.reply_to_message_id AND parent.source_id=m.source_id AND parent.conversation_id=m.conversation_id AND ` + own("parent") + `))) `
	if s.IsPostgreSQL() {
		var activity nullableTimestamp
		err = q.QueryRowContext(ctx, query+`SELECT MAX(signal_time) FROM signals WHERE signal_time<=?`, append(args, now)...).Scan(&activity)
		if err != nil {
			return time.Time{}, fmt.Errorf("attribute conversation %d activity: %w", id, err)
		}
		return activity.Time.UTC(), nil
	}
	// SQLite stores offset-bearing text. Parse only qualifying metadata and
	// compare exact instants, avoiding lexical order and date-function rounding.
	rows, err := q.QueryContext(ctx, query+`SELECT signal_time FROM signals`, args...)
	if err != nil {
		return time.Time{}, fmt.Errorf("attribute conversation %d activity: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var latest time.Time
	for rows.Next() {
		var activity nullableTimestamp
		if err = rows.Scan(&activity); err != nil {
			return time.Time{}, err
		}
		if activity.Valid && !activity.Time.After(now) && activity.Time.After(latest) {
			latest = activity.Time.UTC()
		}
	}
	return latest, rows.Err()
}

// Claim keys before scanning, never after it. On PostgreSQL, a writer that
// re-dirties a claimed key waits until commit and its new key survives. SQLite
// already serializes these writes. Cancellation rolls claims and scans back.
func (s *Store) drainConversationPolicyActivityDirtyTx(ctx context.Context, tx *loggedTx) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `DELETE FROM archive_metadata WHERE key LIKE 'conversation_policy_activity_dirty:%' RETURNING key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(key, conversationPolicyActivityDirtyPrefix), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid activity dirty key: %w", err)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, rows.Err()
}
func (s *Store) clearConversationPolicyActivityDirtyTx(ctx context.Context, tx *loggedTx, ids []int64) error {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = conversationPolicyActivityDirtyPrefix + strconv.FormatInt(id, 10)
	}
	return execInChunksContext(ctx, tx, keys, nil, `DELETE FROM archive_metadata WHERE key IN (%s)`)
}
func (s *Store) refreshConversationActivityTx(ctx context.Context, tx *loggedTx, ids []int64, now time.Time) error {
	if ids == nil {
		rows, err := tx.QueryContext(ctx, `SELECT c.id FROM conversations c JOIN sources src ON src.id=c.source_id JOIN conversation_policy policy ON policy.conversation_id=c.id WHERE src.source_type IN ('beeper','slack','slackdump','discord','teams') ORDER BY c.id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		var supported []int64
		if err := queryInChunksContext(ctx, tx, ids, nil, `SELECT c.id FROM conversations c JOIN sources src ON src.id=c.source_id JOIN conversation_policy policy ON policy.conversation_id=c.id WHERE c.id IN (%s) AND src.source_type IN ('beeper','slack','slackdump','discord','teams') ORDER BY c.id`, func(rows *loggedRows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			supported = append(supported, id)
			return nil
		}); err != nil {
			return err
		}
		ids = supported
	}
	for _, id := range ids {
		activity, err := s.conversationLastActivityWith(ctx, tx, id, now)
		if err != nil {
			return err
		}
		var value any
		if !activity.IsZero() {
			value = activity
		}
		_, err = tx.ExecContext(ctx, `UPDATE conversation_policy SET last_activity_at=? WHERE conversation_id=?`, value, id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) conversationPolicyActivityExpiryStartTx(
	ctx context.Context, tx *loggedTx, now time.Time, window time.Duration,
) (time.Time, error) {
	var raw string
	err := tx.QueryRowContext(ctx, s.Rebind("SELECT value FROM archive_metadata WHERE key=?"), conversationPolicyActivityExpiryKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return now.Add(-window).UTC(), nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read conversation activity expiry checkpoint: %w", err)
	}
	checkpoint, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse conversation activity expiry checkpoint: %w", err)
	}
	if checkpoint.After(now) {
		return now.Add(-window).UTC(), nil
	}
	return checkpoint.UTC(), nil
}

// conversationPolicyActivityIDsInIntervalTx finds existing chat-policy rooms
// with message timestamps that crossed during an expiry interval. A missing
// checkpoint starts at the activity window boundary; later passes scan only
// timestamps since the previous full expiry. The activity scanner applies the
// exact own-message, mention, and reply rules before policy state changes.
func (s *Store) conversationPolicyActivityIDsInIntervalTx(
	ctx context.Context, tx *loggedTx, since, now time.Time,
) ([]int64, error) {
	since = since.UTC()
	now = now.UTC()
	const timestamp = "COALESCE(m.sent_at,m.received_at,m.internal_date)"
	const conversations = "SELECT c.id FROM conversations c" +
		" JOIN sources src ON src.id=c.source_id" +
		" JOIN conversation_policy policy ON policy.conversation_id=c.id"
	const supported = "src.source_type IN ('beeper','slack','slackdump','discord','teams')"
	var ids []int64
	readIDs := func(query string, args ...any) error {
		rows, err := tx.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}
	if s.IsPostgreSQL() {
		// PostgreSQL timestamps have microsecond precision. Round the interval
		// inward so sub-microsecond bounds cannot admit an event outside it.
		start := since.Truncate(time.Microsecond)
		if start.Before(since) {
			start = start.Add(time.Microsecond)
		}
		end := now.Truncate(time.Microsecond)
		if start.After(end) {
			return nil, nil
		}
		query := conversations + " WHERE " + supported +
			" AND EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.source_id=c.source_id AND " + timestamp + " >= ? AND " + timestamp + " <= ?)" +
			" ORDER BY c.id"
		if err := readIDs(query, start, end); err != nil {
			return nil, fmt.Errorf("find conversation activity in expiry interval: %w", err)
		}
		return ids, nil
	}

	// SQLite's julianday has millisecond precision. Find rooms in the safe
	// interior with EXISTS, then parse only timestamps near either boundary so
	// the precision cushion cannot admit activity just outside the interval.
	interior := conversations + " WHERE " + supported +
		" AND EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.source_id=c.source_id AND julianday(" + timestamp + ") >= julianday(?) + (1.0 / 86400.0) AND julianday(" + timestamp + ") <= julianday(?) - (1.0 / 86400.0))" +
		" ORDER BY c.id"
	if err := readIDs(interior, since, now); err != nil {
		return nil, fmt.Errorf("find interior conversation activity in expiry interval: %w", err)
	}
	edgeQuery := "SELECT c.id," + timestamp + " FROM conversations c" +
		" JOIN sources src ON src.id=c.source_id" +
		" JOIN conversation_policy policy ON policy.conversation_id=c.id" +
		" JOIN messages m ON m.conversation_id=c.id AND m.source_id=c.source_id" +
		" WHERE " + supported + " AND ((julianday(" + timestamp + ") >= julianday(?) - (1.0 / 86400.0) AND julianday(" + timestamp + ") <= julianday(?) + (1.0 / 86400.0)) OR (julianday(" + timestamp + ") >= julianday(?) - (1.0 / 86400.0) AND julianday(" + timestamp + ") <= julianday(?) + (1.0 / 86400.0)))" +
		" ORDER BY c.id"
	rows, err := tx.QueryContext(ctx, s.Rebind(edgeQuery), since, since, now, now)
	if err != nil {
		return nil, fmt.Errorf("find boundary conversation activity in expiry interval: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var activity nullableTimestamp
		if err := rows.Scan(&id, &activity); err != nil {
			return nil, err
		}
		if activity.Valid && !activity.Time.Before(since) && !activity.Time.After(now) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

type conversationPolicySelfIdentity struct {
	SourceID       int64
	Address        string
	ParticipantIDs []int64
}

// conversationPolicySelfIdentitiesWith resolves existing self authority on the
// caller's transaction. Slack's persisted workspace/account owner is the same
// authority used for source_is_from_me; it needs no rich profile admission.
func conversationPolicySelfIdentitiesWith(ctx context.Context, q accountIdentityQuerier, sourceID int64) ([]conversationPolicySelfIdentity, error) {
	confirmed, err := listAccountIdentitiesWith(ctx, q, sourceID)
	if err != nil {
		return nil, err
	}
	var identities []conversationPolicySelfIdentity
	for _, identity := range confirmed {
		resolved, err := resolveAccountIdentityWith(ctx, q, sourceID, identity.Address)
		if err != nil {
			return nil, err
		}
		identities = append(identities, conversationPolicySelfIdentity{SourceID: sourceID, Address: NormalizeIdentifierForCompare(resolved.Identifier), ParticipantIDs: resolved.ParticipantIDs})
	}
	var sourceType, owner string
	if err := q.QueryRowContext(ctx, `SELECT source_type,identifier FROM sources WHERE id=?`, sourceID).Scan(&sourceType, &owner); err != nil {
		return nil, err
	}
	if sourceType == "slack" || sourceType == "slackdump" {
		workspace, account, valid := strings.Cut(owner, ":")
		if valid && workspace != "" && account != "" && !strings.Contains(account, ":") {
			identity := conversationPolicySelfIdentity{SourceID: sourceID, Address: "slack:" + owner}
			var pid int64
			err := q.QueryRowContext(ctx, `SELECT participant_id FROM participant_identifiers WHERE identifier_type='slack' AND identifier_value=?`, owner).Scan(&pid)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				identity.ParticipantIDs = []int64{pid}
			}
			identities = append(identities, identity)
		}
	}
	return identities, nil
}

// conversationPolicySelfFingerprintTx is derived from the existing confirmed
// account identities and provider owner mappings, never a second self registry.
// Global identity revisions can change for unrelated people/link operations.
func (s *Store) conversationPolicySelfFingerprintTx(ctx context.Context, tx *loggedTx) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sources WHERE source_type IN ('beeper','slack','slackdump','discord','teams') ORDER BY id`)
	if err != nil {
		return "", err
	}
	var sourceIDs []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		sourceIDs = append(sourceIDs, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return "", err
	}
	var identities []conversationPolicySelfIdentity
	for _, sourceID := range sourceIDs {
		resolved, err := conversationPolicySelfIdentitiesWith(ctx, tx, sourceID)
		if err != nil {
			return "", err
		}
		identities = append(identities, resolved...)
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
