package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
)

var ErrBroadcastFrozen = errors.New("broadcast is frozen or revision changed")

type Broadcast struct {
	ID                                                      string
	SourceID                                                int64
	AdAccountID, Name                                       string
	MessageJSON                                             []byte
	MessageSHA256                                           string
	Revision, DailyBudgetCents, CampaignDays, MinGapSeconds int64
	State, CampaignID, PlanDigest, LastError                string
	CampaignReadyAt, CampaignEndsAt                         sql.NullTime
	BudgetWarning                                           bool
	CreatedAt, UpdatedAt                                    time.Time
}
type BroadcastRecipient struct {
	BroadcastID                               string
	SubscriptionID                            int64
	ParticipantID                             sql.NullInt64
	State, Reason, ErrorCode, TrackingID, MID string
	MessageID                                 sql.NullInt64
	AttemptedAt                               sql.NullTime
	RowRevision                               int64
}
type RecipientDecisionRow struct {
	SubscriptionID int64
	Eligible       bool
	Reason         string
}
type MarketingEvent struct {
	FailureCategory                                string
	SourceID                                       int64
	Kind, TrackingID, MID, CampaignID, Fingerprint string
	MessageID                                      sql.NullInt64
	At                                             time.Time
}

const broadcastColumns = `id,source_id,ad_account_id,name,message_json,message_sha256,revision,daily_budget_cents,campaign_days,min_conversation_gap_seconds,state,COALESCE(campaign_id,''),plan_digest,last_error,campaign_ready_at,campaign_ends_at,budget_warning,created_at,updated_at`
const broadcastRecipientColumns = `broadcast_id,subscription_id,participant_id,state,reason,error_code,COALESCE(tracking_id,''),COALESCE(mid,''),message_id,attempted_at,row_revision`

func scanBroadcast(row interface{ Scan(dest ...any) error }) (Broadcast, error) {
	var b Broadcast
	err := row.Scan(&b.ID, &b.SourceID, &b.AdAccountID, &b.Name, &b.MessageJSON, &b.MessageSHA256, &b.Revision, &b.DailyBudgetCents, &b.CampaignDays, &b.MinGapSeconds, &b.State, &b.CampaignID, &b.PlanDigest, &b.LastError, &b.CampaignReadyAt, &b.CampaignEndsAt, &b.BudgetWarning, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}
func scanBroadcastRecipient(row interface{ Scan(dest ...any) error }) (BroadcastRecipient, error) {
	var r BroadcastRecipient
	err := row.Scan(&r.BroadcastID, &r.SubscriptionID, &r.ParticipantID, &r.State, &r.Reason, &r.ErrorCode, &r.TrackingID, &r.MID, &r.MessageID, &r.AttemptedAt, &r.RowRevision)
	return r, err
}
func broadcastMessageHash(message []byte) (string, error) {
	v := jsontext.Value(message)
	if len(v) == 0 || !v.IsValid() || v.Kind() != '{' || len(v) > 32<<10 {
		return "", errors.New("invalid broadcast message")
	}
	sum := sha256.Sum256(message)
	return hex.EncodeToString(sum[:]), nil
}
func (s *Store) CreateBroadcastContext(ctx context.Context, b Broadcast, ids []int64) (Broadcast, error) {
	hash, err := broadcastMessageHash(b.MessageJSON)
	if err != nil {
		return Broadcast{}, err
	}
	if len(ids) == 0 || len(ids) > 1000 || b.DailyBudgetCents <= 0 || b.CampaignDays < 1 || b.CampaignDays > 30 || b.MinGapSeconds < 0 {
		return Broadcast{}, errors.New("invalid broadcast")
	}
	var raw [16]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return Broadcast{}, fmt.Errorf("generate broadcast ID: %w", err)
	}
	b.ID = "broadcast-" + hex.EncodeToString(raw[:])
	b.Revision = 1
	b.State = "draft"
	b.MessageSHA256 = hash
	b.CreatedAt = time.Now().UTC()
	b.UpdatedAt = b.CreatedAt
	b.CampaignID = ""
	b.PlanDigest = ""
	b.CampaignReadyAt = sql.NullTime{}
	b.CampaignEndsAt = sql.NullTime{}
	b.BudgetWarning = false
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta_broadcasts(id,source_id,ad_account_id,name,message_json,message_sha256,revision,daily_budget_cents,campaign_days,min_conversation_gap_seconds,state,created_at,updated_at) VALUES (?,?,?,?,?,?,1,?,?,?,'draft',?,?)`, b.ID, b.SourceID, b.AdAccountID, b.Name, string(b.MessageJSON), hash, b.DailyBudgetCents, b.CampaignDays, b.MinGapSeconds, b.CreatedAt, b.UpdatedAt)
		if err != nil {
			return err
		}
		for _, id := range ids {
			var participant sql.NullInt64
			if err = tx.QueryRowContext(ctx, `SELECT participant_id FROM meta_marketing_subscriptions WHERE id=? AND source_id=?`, id, b.SourceID).Scan(&participant); err != nil {
				return errors.New("broadcast subscription does not belong to source")
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO meta_broadcast_recipients(broadcast_id,subscription_id,participant_id,state,updated_at) VALUES (?,?,?,'planned',?) ON CONFLICT DO NOTHING`, b.ID, id, participant, b.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Broadcast{}, err
	}
	return b, nil
}
func (s *Store) GetBroadcastContext(ctx context.Context, id string) (Broadcast, []BroadcastRecipient, error) {
	var b Broadcast
	out := []BroadcastRecipient{}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		b, err = scanBroadcast(tx.QueryRowContext(ctx, `SELECT `+broadcastColumns+` FROM meta_broadcasts WHERE id=?`, id))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+broadcastRecipientColumns+` FROM meta_broadcast_recipients WHERE broadcast_id=? ORDER BY subscription_id`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			r, err := scanBroadcastRecipient(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return b, out, err
}
func (s *Store) ListBroadcastsContext(ctx context.Context, sourceID int64) ([]Broadcast, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+broadcastColumns+` FROM meta_broadcasts WHERE source_id=? ORDER BY created_at,id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Broadcast{}
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Store) UpdateBroadcastMessageContext(ctx context.Context, id string, revision int64, message []byte) (Broadcast, error) {
	hash, err := broadcastMessageHash(message)
	if err != nil {
		return Broadcast{}, err
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcasts SET message_json=?,message_sha256=?,revision=revision+1,plan_digest='',state='draft',updated_at=? WHERE id=? AND revision=? AND state IN ('draft','planned')`, string(message), hash, time.Now().UTC(), id, revision)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrBroadcastFrozen
		}
		_, err = tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state='planned',reason='',row_revision=row_revision+1,updated_at=? WHERE broadcast_id=? AND state IN ('planned','refused')`, time.Now().UTC(), id)
		return err
	})
	if err != nil {
		return Broadcast{}, err
	}
	b, _, err := s.GetBroadcastContext(ctx, id)
	return b, err
}
func (s *Store) SavePlanContext(ctx context.Context, id string, revision int64, digest string, decisions []RecipientDecisionRow) error {
	if digest == "" {
		return errors.New("missing broadcast plan digest")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcasts SET state='planned',plan_digest=?,updated_at=? WHERE id=? AND revision=? AND state IN ('draft','planned')`, digest, time.Now().UTC(), id, revision)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrBroadcastFrozen
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta_broadcast_recipients WHERE broadcast_id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count != len(decisions) {
			return errors.New("broadcast plan recipient mismatch")
		}
		seen := map[int64]bool{}
		for _, d := range decisions {
			if seen[d.SubscriptionID] {
				return errors.New("duplicate broadcast plan recipient")
			}
			seen[d.SubscriptionID] = true
			state := "refused"
			if d.Eligible {
				state = "planned"
			}
			res, err = tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state=?,reason=?,row_revision=row_revision+1,updated_at=? WHERE broadcast_id=? AND subscription_id=? AND state IN ('planned','refused')`, state, d.Reason, time.Now().UTC(), id, d.SubscriptionID)
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
			if n != 1 {
				return errors.New("broadcast plan recipient mismatch")
			}
		}
		return nil
	})
}
func (s *Store) TransitionBroadcastContext(ctx context.Context, id, from, to, campaignID string) error {
	return s.transitionBroadcast(ctx, id, from, to, campaignID, nil)
}

// BeginBroadcastCampaignContext freezes the requested end before the provider
// write, so adoption cannot extend an ambiguously created campaign.
func (s *Store) BeginBroadcastCampaignContext(ctx context.Context, id string, ends time.Time) error {
	if ends.IsZero() {
		return errors.New("invalid broadcast campaign end")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE meta_broadcasts SET state='campaign_pending',campaign_ends_at=?,updated_at=? WHERE id=? AND state='planned'`, ends.UTC(), time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return ErrBroadcastFrozen
	}
	return err
}

// TransitionBroadcastFailureContext records a closed failure category with the transition.
func (s *Store) TransitionBroadcastFailureContext(ctx context.Context, id, from, to, category string) error {
	switch category {
	case "permission_missing", "token_invalid", "rate_limited", "recipient_unavailable", "conversation_gap", "frequency_cap", "pacing", "spend_limit", "campaign_rejected", "send_rejected", "cancelled":
	default:
		return errors.New("invalid broadcast failure category")
	}
	return s.transitionBroadcast(ctx, id, from, to, "", &category)
}

func (s *Store) transitionBroadcast(ctx context.Context, id, from, to, campaignID string, category *string) error {
	if category == nil && (to == "sending" || to == "complete") {
		cleared := ""
		category = &cleared
	}
	allowed := map[string][]string{"planned": {"campaign_pending"}, "campaign_pending": {"planned", "campaign_unknown"}, "ready": {"sending"}, "sending": {"paused", "complete"}, "paused": {"sending"}}
	valid := false
	for _, next := range allowed[from] {
		if next == to {
			valid = true
		}
	}
	if !valid {
		return errors.New("invalid broadcast transition")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcasts SET state=?,campaign_id=COALESCE(?,campaign_id),last_error=COALESCE(?,last_error),campaign_ends_at=CASE WHEN ?='planned' THEN NULL ELSE campaign_ends_at END,updated_at=? WHERE id=? AND state=?`, to, marketingString(campaignID), category, to, time.Now().UTC(), id, from)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrBroadcastFrozen
		}
		return nil
	})
}
func (s *Store) SetCampaignContext(ctx context.Context, id, campaign string, ready, ends time.Time, warning bool) error {
	if campaign == "" || ready.IsZero() || !ends.After(ready) {
		return errors.New("invalid broadcast campaign")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcasts SET state='ready',campaign_id=?,campaign_ready_at=?,campaign_ends_at=?,budget_warning=?,last_error='',updated_at=? WHERE id=? AND state IN ('campaign_pending','campaign_unknown')`, campaign, ready.UTC(), ends.UTC(), warning, time.Now().UTC(), id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrBroadcastFrozen
		}
		return nil
	})
}

// RefuseBroadcastRecipientContext never rewrites a claimed or finished receipt.
func (s *Store) RefuseBroadcastRecipientContext(ctx context.Context, id string, subscriptionID int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state='refused',reason=?,updated_at=?,row_revision=row_revision+1 WHERE broadcast_id=? AND subscription_id=? AND state='planned'`, reason, time.Now().UTC(), id, subscriptionID)
	return err
}
func (s *Store) ClaimRecipientContext(ctx context.Context, id string, subID, revision int64, now time.Time) (bool, error) {
	claimed := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		// Consent changes take this same row lock. Acquire it before reading
		// status so a STOP committed after planning also prevents the claim.
		if _, err := tx.ExecContext(ctx, `UPDATE meta_marketing_subscriptions SET updated_at=updated_at WHERE id=?`, subID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state='dispatching',attempted_at=?,updated_at=?,row_revision=row_revision+1 WHERE broadcast_id=? AND subscription_id=? AND row_revision=? AND state='planned' AND EXISTS(SELECT 1 FROM meta_broadcasts b WHERE b.id=broadcast_id AND b.state IN ('ready','sending','paused')) AND EXISTS(SELECT 1 FROM meta_marketing_subscriptions s WHERE s.id=subscription_id AND s.status='active')`, now.UTC(), now.UTC(), id, subID, revision)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		claimed = n == 1
		return err
	})
	return claimed, err
}
func (s *Store) FinishRecipientContext(ctx context.Context, id string, subID int64, state, reason, code, tracking string) error {
	if state != "accepted" && state != "failed" && state != "remote_unknown" {
		return errors.New("invalid broadcast recipient result")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		// Signed events can settle the row before the HTTP call returns. Preserve
		// that conclusive state and tracking ID when committing the call result.
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state=CASE WHEN state='dispatching' THEN ? ELSE state END,reason=CASE WHEN state='dispatching' THEN ? ELSE reason END,error_code=CASE WHEN state='dispatching' THEN ? ELSE error_code END,tracking_id=COALESCE(tracking_id,?),updated_at=?,row_revision=row_revision+1 WHERE broadcast_id=? AND subscription_id=? AND state IN ('dispatching','accepted','delivered','read','delivery_failed')`, state, reason, code, marketingString(tracking), time.Now().UTC(), id, subID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("broadcast recipient is not dispatching")
		}
		return nil
	})
}
func (s *Store) ExpireAbandonedClaimsContext(ctx context.Context, id string, olderThan time.Time) (int, error) {
	n := int64(0)
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state='remote_unknown',updated_at=?,row_revision=row_revision+1 WHERE broadcast_id=? AND state='dispatching' AND attempted_at<?`, time.Now().UTC(), id, olderThan.UTC())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}
func findMarketingRecipient(ctx context.Context, tx *loggedTx, ev MarketingEvent) (BroadcastRecipient, bool, error) {
	// Resolve exactly one receipt. Missing tracking IDs can fall back to mid
	// or campaign/fingerprint; never guess when an adopted campaign is shared.
	var predicates []string
	var values [][]any
	if ev.TrackingID != "" {
		predicates = append(predicates, "tracking_id=?")
		values = append(values, []any{ev.TrackingID})
	}
	if ev.MID != "" {
		predicates = append(predicates, "mid=?")
		values = append(values, []any{ev.MID})
	}
	if ev.CampaignID != "" && ev.Fingerprint != "" {
		predicates = append(predicates, `EXISTS(SELECT 1 FROM meta_broadcasts b WHERE b.id=r.broadcast_id AND b.campaign_id=?) AND EXISTS(SELECT 1 FROM meta_marketing_subscriptions s WHERE s.id=r.subscription_id AND s.token_fingerprint=?)`)
		values = append(values, []any{ev.CampaignID, ev.Fingerprint})
	}
	var row BroadcastRecipient
	matched := false
	if ev.SourceID <= 0 {
		return row, false, errors.New("missing marketing event source")
	}
	scope := ` AND EXISTS(SELECT 1 FROM meta_broadcasts b WHERE b.id=r.broadcast_id AND b.source_id=?)`
	scopeArgs := []any{ev.SourceID}
	for i, predicate := range predicates {
		rows, err := tx.QueryContext(ctx, `SELECT `+broadcastRecipientColumns+` FROM meta_broadcast_recipients r WHERE `+predicate+scope+` LIMIT 2`, append(values[i], scopeArgs...)...)
		if err != nil {
			return row, false, err
		}
		count := 0
		for rows.Next() {
			row, err = scanBroadcastRecipient(rows)
			if err != nil {
				_ = rows.Close()
				return row, false, err
			}
			count++
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return row, false, err
		}
		if count > 1 {
			return row, false, errors.New("ambiguous marketing event receipt")
		}
		if count == 1 {
			matched = true
			break
		}
	}
	return row, matched, nil
}

// FindMarketingEventRecipientContext resolves a receipt before archiving its echo.
func (s *Store) FindMarketingEventRecipientContext(ctx context.Context, ev MarketingEvent) (BroadcastRecipient, bool, error) {
	var row BroadcastRecipient
	var found bool
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		row, found, err = findMarketingRecipient(ctx, tx, ev)
		return err
	})
	return row, found, err
}

func (s *Store) ApplyMarketingEventContext(ctx context.Context, ev MarketingEvent) (bool, error) {
	if ev.Kind == "delivery_failed" && ev.FailureCategory == "" {
		ev.FailureCategory = "send_rejected"
	}

	switch ev.FailureCategory {
	case "", "send_rejected", "recipient_unavailable", "frequency_cap":
	default:
		return false, errors.New("invalid marketing delivery failure category")
	}

	switch ev.Kind {
	case "echo", "delivered", "read", "click", "delivery_failed":
	default:
		return false, errors.New("invalid marketing event kind")
	}
	matched := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		row, found, err := findMarketingRecipient(ctx, tx, ev)
		if err != nil || !found {
			return err
		}
		matched = true
		if _, err := tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET updated_at=updated_at WHERE broadcast_id=? AND subscription_id=?`, row.BroadcastID, row.SubscriptionID); err != nil {
			return err
		}
		// Read state again under the row lock for monotonic concurrent events.
		row, err = scanBroadcastRecipient(tx.QueryRowContext(ctx, `SELECT `+broadcastRecipientColumns+` FROM meta_broadcast_recipients WHERE broadcast_id=? AND subscription_id=?`, row.BroadcastID, row.SubscriptionID))
		if err != nil {
			return err
		}
		state := row.State
		reason := row.Reason
		tracking := row.TrackingID
		if tracking == "" {
			tracking = ev.TrackingID
		}
		mid := row.MID
		message := row.MessageID
		switch ev.Kind {
		case "echo":
			if state == "dispatching" || state == "remote_unknown" {
				state = "accepted"
			}
			if ev.MID != "" {
				mid = ev.MID
			}
			if ev.MessageID.Valid {
				message = ev.MessageID
			}
		case "delivered":
			if state == "dispatching" || state == "accepted" || state == "remote_unknown" {
				state = "delivered"
			}
		case "read":
			if state == "dispatching" || state == "accepted" || state == "remote_unknown" || state == "delivered" {
				state = "read"
			}
		case "delivery_failed":
			if state == "dispatching" || state == "accepted" || state == "remote_unknown" || state == "delivered" {
				state = "delivery_failed"
				if ev.FailureCategory != "" {
					reason = ev.FailureCategory
				}
			}
		case "click":
			if state == "dispatching" || state == "remote_unknown" {
				state = "accepted"
			}
		}
		if ev.At.IsZero() {
			ev.At = time.Now().UTC()
		}
		_, err = tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state=?,reason=?,tracking_id=?,mid=?,message_id=?,updated_at=?,row_revision=row_revision+1 WHERE broadcast_id=? AND subscription_id=?`, state, reason, marketingString(tracking), marketingString(mid), message, ev.At.UTC(), row.BroadcastID, row.SubscriptionID)
		return err
	})
	return matched, err
}
func (s *Store) LastAcceptedSendContext(ctx context.Context, ids []int64) (map[int64]time.Time, error) {
	out := map[int64]time.Time{}
	for _, id := range ids {
		var at time.Time
		err := s.db.QueryRowContext(ctx, `SELECT attempted_at FROM meta_broadcast_recipients r WHERE state IN ('accepted','delivered','read','delivery_failed') AND attempted_at IS NOT NULL AND EXISTS (
 SELECT 1 FROM meta_marketing_subscriptions sent WHERE sent.id=r.subscription_id AND EXISTS (
  SELECT 1 FROM meta_marketing_subscriptions target WHERE target.id=? AND target.source_id=sent.source_id AND
  (sent.id=target.id OR (target.participant_id IS NOT NULL AND sent.participant_id=target.participant_id) OR (target.psid IS NOT NULL AND target.psid<>'' AND sent.psid=target.psid))
 )) ORDER BY attempted_at DESC LIMIT 1`, id).Scan(&at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			out[id] = at
		}
	}
	return out, nil
}
func (s *Store) CancelBroadcastContext(ctx context.Context, id string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE meta_broadcasts SET state='cancelled',updated_at=? WHERE id=? AND state<>'complete'`, time.Now().UTC(), id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrBroadcastFrozen
		}
		_, err = tx.ExecContext(ctx, `UPDATE meta_broadcast_recipients SET state='cancelled',updated_at=?,row_revision=row_revision+1 WHERE broadcast_id=? AND state='planned'`, time.Now().UTC(), id)
		return err
	})
}
