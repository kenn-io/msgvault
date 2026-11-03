package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrWalkInProgress = errors.New("walk_in_progress")

type MarketingSubscription struct {
	ID, SourceID                                              int64
	ParticipantID                                             sql.NullInt64
	PSID                                                      sql.NullString
	Token                                                     string `json:"-"`
	Fingerprint, TopicTitle, Timezone, Status, Reoptin        string
	TokenExpiryAt, NextEligibleAt, OptedInAt, LastConfirmedAt sql.NullTime
	LastListWalkID                                            sql.NullInt64
}
type ListedSubscription struct {
	Token, PSID, TopicTitle, Timezone, Reoptin string
	ParticipantID                              sql.NullInt64
	Created, Expiry, NextEligible, At          time.Time
}
type ConsentWebhook struct {
	PSID, Token, Title, Timezone, UserTokenStatus, Status string
	Expiry, At                                            time.Time
	EvidenceRef                                           string
}
type SubscriptionFilter struct {
	ParticipantIDs, SubscriptionIDs []int64
	Status                          string
}
type ConsentEvent struct {
	ID, SubscriptionID                int64
	Kind, EvidenceSource, EvidenceRef string
	OccurredAt                        time.Time
	Detail                            map[string]any
	WalkComplete                      bool
}

func TokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
func marketingTime(t time.Time) sql.NullTime  { return sql.NullTime{Time: t.UTC(), Valid: !t.IsZero()} }
func marketingString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

const marketingSubscriptionColumns = `id,source_id,participant_id,psid,token,token_fingerprint,topic_title,timezone,status,reoptin,token_expiry_at,next_eligible_at,opted_in_at,last_confirmed_at,last_list_walk_id`

func scanMarketingSubscription(row interface{ Scan(dest ...any) error }) (MarketingSubscription, error) {
	var v MarketingSubscription
	err := row.Scan(&v.ID, &v.SourceID, &v.ParticipantID, &v.PSID, &v.Token, &v.Fingerprint, &v.TopicTitle, &v.Timezone, &v.Status, &v.Reoptin, &v.TokenExpiryAt, &v.NextEligibleAt, &v.OptedInAt, &v.LastConfirmedAt, &v.LastListWalkID)
	return v, err
}
func (s *Store) StartListWalkContext(ctx context.Context, sourceID int64, now time.Time) (int64, error) {
	var id int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		// A write first serializes SQLite writers; the partial unique index also
		// enforces single flight under PostgreSQL concurrent inserts.
		_, err := tx.ExecContext(ctx, `UPDATE meta_marketing_list_walks SET state='abandoned',finished_at=? WHERE source_id=? AND state='running' AND started_at<?`, now.UTC(), sourceID, now.Add(-time.Hour).UTC())
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO meta_marketing_list_walks(source_id,state,started_at) VALUES (?,'running',?) ON CONFLICT DO NOTHING RETURNING id`, sourceID, now.UTC()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWalkInProgress
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("start marketing list walk: %w", err)
	}
	return id, nil
}

// FinishListWalkContext commits completion, confirmation and absence together.
// Partial walks never establish freshness or mark unseen subscriptions absent.
func (s *Store) FinishListWalkContext(ctx context.Context, walkID int64, state string, requests, listed int, category string) (int, error) {
	if state != "complete" && state != "partial" {
		return 0, errors.New("invalid marketing walk state")
	}
	count := 0
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		now := time.Now().UTC()
		var sourceID int64
		var started time.Time
		err := tx.QueryRowContext(ctx, `UPDATE meta_marketing_list_walks SET state=?,finished_at=?,requests=?,listed=?,error_category=? WHERE id=? AND state='running' RETURNING source_id,started_at`, state, now, requests, listed, category, walkID).Scan(&sourceID, &started)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("marketing walk is not running")
		}
		if err != nil || state != "complete" {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET last_confirmed_at=?,updated_at=? WHERE last_list_walk_id=?`, now, now, walkID); err != nil {
			return err
		}
		count, err = markUnlistedAbsent(ctx, tx, sourceID, walkID, started, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
func (s *Store) UpsertListedSubscriptionContext(ctx context.Context, sourceID, walkID int64, in ListedSubscription) (MarketingSubscription, error) {
	var sub MarketingSubscription
	if in.Token == "" {
		return sub, errors.New("missing marketing subscription token")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var ok int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM meta_marketing_list_walks WHERE id=? AND source_id=? AND state='running'`, walkID, sourceID).Scan(&ok); err != nil {
			return errors.New("marketing walk does not belong to source or is not running")
		}
		now := in.At.UTC()
		if now.IsZero() {
			now = time.Now().UTC()
		}
		err := tx.QueryRowContext(ctx, `INSERT INTO meta_marketing_subscriptions(source_id,participant_id,psid,token,token_fingerprint,topic_title,timezone,status,reoptin,token_expiry_at,next_eligible_at,opted_in_at,last_list_walk_id,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,'active',?,?,?,?,?,?,?) ON CONFLICT(source_id,token_fingerprint) DO UPDATE SET participant_id=COALESCE(excluded.participant_id,meta_marketing_subscriptions.participant_id),psid=COALESCE(excluded.psid,meta_marketing_subscriptions.psid),topic_title=excluded.topic_title,timezone=excluded.timezone,reoptin=excluded.reoptin,token_expiry_at=excluded.token_expiry_at,next_eligible_at=CASE WHEN meta_marketing_subscriptions.next_eligible_at IS NULL OR meta_marketing_subscriptions.next_eligible_at<excluded.next_eligible_at THEN excluded.next_eligible_at ELSE meta_marketing_subscriptions.next_eligible_at END,last_list_walk_id=excluded.last_list_walk_id,last_confirmed_at=CASE WHEN meta_marketing_subscriptions.status='absent' THEN NULL ELSE meta_marketing_subscriptions.last_confirmed_at END,updated_at=excluded.updated_at,status=CASE WHEN meta_marketing_subscriptions.status='absent' THEN 'active' ELSE meta_marketing_subscriptions.status END
 RETURNING id`, sourceID, in.ParticipantID, marketingString(in.PSID), in.Token, TokenFingerprint(in.Token), in.TopicTitle, in.Timezone, in.Reoptin, marketingTime(in.Expiry), marketingTime(in.NextEligible), marketingTime(in.Created), walkID, now, now).Scan(&sub.ID)
		if err != nil {
			return errors.New("persist marketing subscription failed")
		}
		if err = insertConsentEvent(ctx, tx, sub.ID, "listed", "token_list", strconv.FormatInt(walkID, 10), now, map[string]any{"token_fingerprint": TokenFingerprint(in.Token)}); err != nil {
			return err
		}
		sub, err = scanMarketingSubscription(tx.QueryRowContext(ctx, `SELECT `+marketingSubscriptionColumns+` FROM meta_marketing_subscriptions WHERE id=?`, sub.ID))
		return err
	})
	return sub, err
}
func markUnlistedAbsent(ctx context.Context, tx *loggedTx, sourceID, walkID int64, started, now time.Time) (int, error) {
	count := 0
	rows, err := tx.QueryContext(ctx, `SELECT id FROM meta_marketing_subscriptions WHERE source_id=? AND status='active' AND (last_list_walk_id IS NULL OR last_list_walk_id<?)`, sourceID, walkID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		// Serialize with signed consent events before inspecting their evidence.
		if _, err = tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET updated_at=updated_at WHERE id=?`, id); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET status='absent',last_confirmed_at=NULL,updated_at=? WHERE id=? AND status='active' AND (last_list_walk_id IS NULL OR last_list_walk_id<?) AND NOT EXISTS(SELECT 1 FROM meta_marketing_consent_events e WHERE e.subscription_id=meta_marketing_subscriptions.id AND e.kind IN ('optin','resume') AND e.occurred_at>=?)`, now, id, walkID, started)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if err = insertConsentEvent(ctx, tx, id, "absent", "token_list", strconv.FormatInt(walkID, 10), now, nil); err != nil {
			return 0, err
		}
		count++
	}
	return count, nil
}
func (s *Store) PushNextEligibleContext(ctx context.Context, id int64, at time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET next_eligible_at=CASE WHEN next_eligible_at IS NULL OR next_eligible_at<? THEN ? ELSE next_eligible_at END,updated_at=? WHERE id=?`, at.UTC(), at.UTC(), time.Now().UTC(), id)
		return err
	})
}
func insertConsentEvent(ctx context.Context, tx *loggedTx, id int64, kind, source, ref string, at time.Time, detail map[string]any) error {
	data, err := json.Marshal(detail)
	if err != nil {
		return errors.New("invalid consent event detail")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta_marketing_consent_events(subscription_id,kind,occurred_at,evidence_source,evidence_ref,detail) VALUES (?,?,?,?,?,?) ON CONFLICT(subscription_id,kind,evidence_ref) DO NOTHING`, id, kind, at.UTC(), source, ref, string(data))
	return err
}
func (s *Store) ApplyConsentWebhookContext(ctx context.Context, sourceID int64, in ConsentWebhook) (MarketingSubscription, error) {
	var sub MarketingSubscription
	kind, status := "optin", "active"
	switch in.Status {
	case "":
	case "STOP NOTIFICATIONS", "STOP_NOTIFICATIONS":
		kind, status = "stop", "stopped"
	case "RESUME NOTIFICATIONS", "RESUME_NOTIFICATIONS":
		kind = "resume"
	default:
		return sub, errors.New("invalid marketing consent status")
	}
	if in.Token == "" || in.EvidenceRef == "" || in.At.IsZero() {
		return sub, errors.New("invalid marketing consent event")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx, `INSERT INTO meta_marketing_subscriptions(source_id,psid,token,token_fingerprint,topic_title,timezone,status,token_expiry_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(source_id,token_fingerprint) DO NOTHING RETURNING id`, sourceID, marketingString(in.PSID), in.Token, TokenFingerprint(in.Token), in.Title, in.Timezone, status, marketingTime(in.Expiry), now, now).Scan(&sub.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return errors.New("persist marketing subscription failed")
		}
		sub, err = scanMarketingSubscription(tx.QueryRowContext(ctx, `SELECT `+marketingSubscriptionColumns+` FROM meta_marketing_subscriptions WHERE source_id=? AND token_fingerprint=?`, sourceID, TokenFingerprint(in.Token)))
		if err != nil {
			return err
		}
		// Serialize consent mutations on the subscription before inspecting evidence.
		if _, err = tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET updated_at=updated_at WHERE id=?`, sub.ID); err != nil {
			return err
		}
		var replay bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meta_marketing_consent_events WHERE subscription_id=? AND kind=? AND evidence_ref=?)`, sub.ID, kind, in.EvidenceRef).Scan(&replay); err != nil {
			return err
		}
		if replay {
			return nil
		}
		var newest sql.NullTime
		if err = tx.QueryRowContext(ctx, `SELECT occurred_at FROM meta_marketing_consent_events WHERE subscription_id=? AND kind IN ('optin','resume','stop','local_unsubscribe') ORDER BY occurred_at DESC,id DESC LIMIT 1`, sub.ID).Scan(&newest); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !newest.Valid || in.At.After(newest.Time) {
			_, err = tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET status=?,psid=COALESCE(?,psid),topic_title=?,timezone=?,token_expiry_at=?,opted_in_at=CASE WHEN ?='active' THEN ? ELSE opted_in_at END,updated_at=? WHERE id=?`, status, marketingString(in.PSID), in.Title, in.Timezone, marketingTime(in.Expiry), status, in.At.UTC(), now, sub.ID)
			if err != nil {
				return err
			}
		}
		if err = insertConsentEvent(ctx, tx, sub.ID, kind, "webhook", in.EvidenceRef, in.At, map[string]any{"token_fingerprint": sub.Fingerprint}); err != nil {
			return err
		}
		sub, err = scanMarketingSubscription(tx.QueryRowContext(ctx, `SELECT `+marketingSubscriptionColumns+` FROM meta_marketing_subscriptions WHERE id=?`, sub.ID))
		return err
	})
	return sub, err
}
func (s *Store) RecordConsentEventContext(ctx context.Context, id int64, kind, source, ref string, detail map[string]any) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var token string
		if err := tx.QueryRowContext(ctx, `SELECT token FROM meta_marketing_subscriptions WHERE id=?`, id).Scan(&token); err != nil {
			return err
		}
		// Detail is a small closed projection, not a channel for provider text.
		clean := map[string]any{"token_fingerprint": TokenFingerprint(token)}
		for _, key := range []string{"title", "receipt_id", "error_category"} {
			if v, ok := detail[key].(string); ok {
				clean[key] = strings.ReplaceAll(v, token, TokenFingerprint(token))
			}
		}
		return insertConsentEvent(ctx, tx, id, kind, source, ref, time.Now().UTC(), clean)
	})
}
func (s *Store) SetSubscriptionStatusContext(ctx context.Context, id int64, status, kind, source, ref string) error {
	switch status {
	case "active", "stopped", "unsubscribed", "absent":
	default:
		return errors.New("invalid marketing subscription status")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET status=?,updated_at=? WHERE id=?`, status, now, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return sql.ErrNoRows
		}
		return insertConsentEvent(ctx, tx, id, kind, source, ref, now, nil)
	})
}
func (s *Store) ListMarketingSubscriptionsContext(ctx context.Context, sourceID int64, f SubscriptionFilter) ([]MarketingSubscription, error) {
	q := `SELECT ` + marketingSubscriptionColumns + ` FROM meta_marketing_subscriptions WHERE source_id=?`
	args := []any{sourceID}
	if f.Status != "" {
		q += ` AND status=?`
		args = append(args, f.Status)
	}
	if len(f.ParticipantIDs)+len(f.SubscriptionIDs) > 0 {
		var terms []string
		for _, v := range f.ParticipantIDs {
			terms = append(terms, "participant_id=?")
			args = append(args, v)
		}
		for _, v := range f.SubscriptionIDs {
			terms = append(terms, "id=?")
			args = append(args, v)
		}
		q += ` AND (` + strings.Join(terms, " OR ") + `)`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []MarketingSubscription{}
	for rows.Next() {
		v, err := scanMarketingSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) LatestConsentEventsContext(ctx context.Context, ids []int64) (map[int64][]ConsentEvent, error) {
	out := map[int64][]ConsentEvent{}
	for _, id := range ids {
		rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.kind,e.occurred_at,e.evidence_source,e.evidence_ref,e.detail,
 EXISTS(SELECT 1 FROM meta_marketing_list_walks w WHERE CAST(w.id AS TEXT)=e.evidence_ref AND w.state='complete' AND e.kind='listed')
 FROM meta_marketing_consent_events e WHERE e.subscription_id=? ORDER BY e.occurred_at DESC,e.id DESC`, id)
		// Use a guarded textual comparison instead of casting webhook evidence IDs.
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			v := ConsentEvent{SubscriptionID: id}
			var detail string
			if err = rows.Scan(&v.ID, &v.Kind, &v.OccurredAt, &v.EvidenceSource, &v.EvidenceRef, &detail, &v.WalkComplete); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err = json.Unmarshal([]byte(detail), &v.Detail); err != nil {
				_ = rows.Close()
				return nil, errors.New("invalid consent event detail")
			}
			out[id] = append(out[id], v)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Store) SetSubscriptionParticipantContext(ctx context.Context, id, participantID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET participant_id=?,updated_at=? WHERE id=?`, participantID, time.Now().UTC(), id)
		return err
	})
}
