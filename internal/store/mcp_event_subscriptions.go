package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"
)

type MCPSubscription struct {
	ID, Principal, Name                                   string
	Arguments                                             []byte
	ScopeKind                                             string
	ScopeID, SourceID                                     int64
	CallbackURL                                           string
	SecretEnc, PreviousSecretEnc                          []byte
	PreviousSecretUntil                                   time.Time
	SecretRevision, VerifiedRevision                      int64
	VerifiedAt                                            time.Time
	Generation                                            int64
	State, StopReason                                     string
	ExpiresAt                                             time.Time
	CursorEpoch, CursorSeq, PendingSeq, PendingGeneration int64
	PendingEnvelope                                       []byte
	AttemptCount                                          int
	NextAttemptAt, FromMeWindowStart                      time.Time
	FromMeWindowCount, LoopGuardSkips, DeadLetterCount    int
	LastOutcome                                           string
	CreatedAt, UpdatedAt                                  time.Time
}

type MCPActivation struct {
	Subscription           MCPSubscription
	ExpectedGeneration     int64
	ExpectedState          string
	ExpectedSecretRevision int64
	Replay                 bool
	ReplayEpoch, ReplaySeq int64
	Now                    time.Time
}

type MCPDelivery struct {
	Subscription MCPSubscription
	Event        MCPEvent
}

const mcpSubscriptionColumns = `id,principal_id,name,arguments,scope_kind,scope_id,source_id,callback_url,secret_enc,previous_secret_enc,previous_secret_until,secret_revision,verified_revision,verified_at,generation,state,stop_reason,expires_at,cursor_epoch,cursor_seq,pending_seq,pending_envelope,pending_generation,attempt_count,next_attempt_at,from_me_window_start,from_me_window_count,loop_guard_skips,dead_letter_count,last_outcome,created_at,updated_at`
const mcpEventColumns = `seq,epoch,family,kind,scope_kind,scope_id,item_key,COALESCE(message_id,0),COALESCE(message_reference_seq,0),conversation_id,source_id,COALESCE(attachment_id,0),from_me,occurred_at,recorded_at,data`
const mcpSubscriptionActive = "active"

func scanMCPSubscription(row scanner) (*MCPSubscription, error) {
	var sub MCPSubscription
	var arguments string
	var previousUntil, due, window sql.NullString
	var verified, expires, created, updated string
	var pendingSeq, pendingGen sql.NullInt64
	err := row.Scan(&sub.ID, &sub.Principal, &sub.Name, &arguments, &sub.ScopeKind, &sub.ScopeID, &sub.SourceID, &sub.CallbackURL, &sub.SecretEnc, &sub.PreviousSecretEnc, &previousUntil, &sub.SecretRevision, &sub.VerifiedRevision, &verified, &sub.Generation, &sub.State, &sub.StopReason, &expires, &sub.CursorEpoch, &sub.CursorSeq, &pendingSeq, &sub.PendingEnvelope, &pendingGen, &sub.AttemptCount, &due, &window, &sub.FromMeWindowCount, &sub.LoopGuardSkips, &sub.DeadLetterCount, &sub.LastOutcome, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // A missing subscription is a valid absence state for lookup and first activation.
	}
	if err != nil {
		return nil, err
	}
	sub.Arguments = []byte(arguments)
	sub.PendingSeq = pendingSeq.Int64
	sub.PendingGeneration = pendingGen.Int64
	for _, field := range []struct {
		value  string
		target *time.Time
	}{{previousUntil.String, &sub.PreviousSecretUntil}, {due.String, &sub.NextAttemptAt}, {window.String, &sub.FromMeWindowStart}, {verified, &sub.VerifiedAt}, {expires, &sub.ExpiresAt}, {created, &sub.CreatedAt}, {updated, &sub.UpdatedAt}} {
		*field.target, err = mcpParseTime(field.value)
		if err != nil {
			return nil, err
		}
	}
	return &sub, nil
}

func scanMCPEvent(row scanner) (MCPEvent, error) {
	var event MCPEvent
	var occurred, recorded, data string
	err := row.Scan(&event.Seq, &event.Epoch, &event.Family, &event.Kind, &event.ScopeKind, &event.ScopeID, &event.ItemKey, &event.MessageID, &event.MessageReferenceSeq, &event.ConversationID, &event.SourceID, &event.AttachmentID, &event.FromMe, &occurred, &recorded, &data)
	if err != nil {
		return MCPEvent{}, err
	}
	event.Data = []byte(data)
	event.OccurredAt, err = mcpParseTime(occurred)
	if err != nil {
		return MCPEvent{}, err
	}
	event.RecordedAt, err = mcpParseTime(recorded)
	return event, err
}

func (s *Store) mcpSubscription(ctx context.Context, db cardDAVQueryer, id string) (*MCPSubscription, error) {
	return scanMCPSubscription(db.QueryRowContext(ctx, s.Rebind(`SELECT `+mcpSubscriptionColumns+` FROM mcp_event_subscriptions WHERE id=?`), id))
}

func (s *Store) saveMCPSubscription(ctx context.Context, tx *sql.Tx, sub *MCPSubscription) error {
	columns := strings.Split(mcpSubscriptionColumns, ",")
	updates := make([]string, 0, len(columns)-1)
	for _, column := range columns[1:] {
		updates = append(updates, column+"=excluded."+column)
	}
	var pending []byte
	if sub.PendingSeq > 0 {
		pending = sub.PendingEnvelope
	}
	_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_event_subscriptions (`+mcpSubscriptionColumns+`) VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")+`) ON CONFLICT (id) DO UPDATE SET `+strings.Join(updates, ",")), sub.ID, sub.Principal, sub.Name, string(sub.Arguments), sub.ScopeKind, sub.ScopeID, sub.SourceID, sub.CallbackURL, sub.SecretEnc, sub.PreviousSecretEnc, mcpOptionalTime(sub.PreviousSecretUntil), sub.SecretRevision, sub.VerifiedRevision, mcpTime(sub.VerifiedAt), sub.Generation, sub.State, sub.StopReason, mcpTime(sub.ExpiresAt), sub.CursorEpoch, sub.CursorSeq, mcpOptionalID(sub.PendingSeq), pending, mcpOptionalID(sub.PendingGeneration), sub.AttemptCount, mcpOptionalTime(sub.NextAttemptAt), mcpOptionalTime(sub.FromMeWindowStart), sub.FromMeWindowCount, sub.LoopGuardSkips, sub.DeadLetterCount, sub.LastOutcome, mcpTime(sub.CreatedAt), mcpTime(sub.UpdatedAt))
	return err
}

func clearMCPPending(sub *MCPSubscription) {
	sub.PendingSeq = 0
	sub.PendingGeneration = 0
	sub.PendingEnvelope = nil
	sub.AttemptCount = 0
	sub.NextAttemptAt = time.Time{}
}

func stopMCPSubscription(sub *MCPSubscription, state, reason string, now time.Time) {
	sub.State = state
	sub.StopReason = reason
	sub.Generation++
	sub.UpdatedAt = now
	clearMCPPending(sub)
}

func (s *Store) GetMCPSubscription(ctx context.Context, id string) (*MCPSubscription, error) {
	sub, err := s.mcpSubscription(ctx, s.DB(), id)
	return sub, mcpSafeError(err)
}

func (s *Store) ListMCPSubscriptions(ctx context.Context, principal string) ([]MCPSubscription, error) {
	return s.listMCPSubscriptions(ctx, &principal)
}

// ListMCPSubscriptionsForStartup supplies retained secrets for restore-key
// validation, including revoked owners. It is never exposed through the API.
func (s *Store) ListMCPSubscriptionsForStartup(ctx context.Context) ([]MCPSubscription, error) {
	return s.listMCPSubscriptions(ctx, nil)
}

func (s *Store) listMCPSubscriptions(ctx context.Context, principal *string) ([]MCPSubscription, error) {
	query := `SELECT ` + mcpSubscriptionColumns + ` FROM mcp_event_subscriptions`
	var args []any
	if principal != nil {
		query += ` WHERE principal_id=?`
		args = append(args, *principal)
	}
	query += ` ORDER BY created_at,id`
	rows, err := s.DB().QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, mcpSafeError(err)
	}
	defer func() { _ = rows.Close() }()
	subs := make([]MCPSubscription, 0)
	for rows.Next() {
		sub, err := scanMCPSubscription(rows)
		if err != nil {
			return nil, mcpSafeError(err)
		}
		subs = append(subs, *sub)
	}
	return subs, mcpSafeError(rows.Err())
}

// ActivateMCPSubscription applies a verified candidate only if the predecessor
// observed before its outbound challenge is still the same generation/state/key.
func (s *Store) ActivateMCPSubscription(ctx context.Context, a MCPActivation) (*MCPSubscription, bool, error) {
	root := s.mcpRoot()
	root.mcpConfigMu.Lock()
	defer root.mcpConfigMu.Unlock()
	input := a.Subscription
	if input.ID == "" || input.Principal == "" || input.Name == "" || input.ScopeID <= 0 || input.SourceID <= 0 || input.SecretRevision <= 0 || input.VerifiedRevision != input.SecretRevision || len(input.SecretEnc) == 0 || !a.Now.Before(input.ExpiresAt) || input.ExpiresAt.After(a.Now.Add(24*time.Hour)) {
		return nil, false, mcpStoreError("invalid_subscription")
	}
	r := root.mcpConfig.Load()
	if r == nil || !r.config.Enabled || input.Principal != r.config.Principal {
		return nil, false, mcpStoreError("subscription_unavailable")
	}
	var result *MCPSubscription
	truncated := false
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, clock MCPEventClock) error {
		var args struct {
			Kinds []string `json:"kinds"`
		}
		if err := json.Unmarshal(input.Arguments, &args); err != nil {
			return mcpStoreError("invalid_subscription")
		}
		var scopeKinds []string
		for _, kind := range args.Kinds {
			if kind != "*" {
				scopeKinds = append(scopeKinds, kind)
			}
		}
		_, sourceID, err := s.validateMCPEventScope(ctx, tx, r, input.Name, input.ScopeKind, input.ScopeID, scopeKinds)
		if err != nil {
			return err
		}
		if sourceID != input.SourceID {
			return mcpStoreError("unknown_scope")
		}
		old, err := s.mcpSubscription(ctx, tx, input.ID)
		if err != nil {
			return err
		}
		if old == nil {
			if a.ExpectedGeneration != 0 || a.ExpectedState != "" || a.ExpectedSecretRevision != 0 {
				return mcpStoreError("concurrent_update")
			}
		} else {
			if old.Principal != input.Principal || old.StopReason == "principal_revoked" || old.StopReason == "scope_removed" {
				return mcpStoreError("subscription_unavailable")
			}
			if old.Generation != a.ExpectedGeneration || old.State != a.ExpectedState || old.SecretRevision != a.ExpectedSecretRevision {
				return mcpStoreError("concurrent_update")
			}
		}
		if old == nil || old.State != mcpSubscriptionActive || !a.Now.Before(old.ExpiresAt) {
			var active int
			if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT COUNT(*) FROM mcp_event_subscriptions WHERE principal_id=? AND state='active' AND expires_at>? AND id<>?`), input.Principal, mcpTime(a.Now), input.ID).Scan(&active); err != nil {
				return err
			}
			if active >= 64 {
				return mcpStoreError("subscription_limit")
			}
		}
		input.State = mcpSubscriptionActive
		input.StopReason = ""
		input.UpdatedAt = a.Now
		if input.VerifiedAt.IsZero() {
			input.VerifiedAt = a.Now
		}
		if old == nil {
			input.Generation = 1
			input.CreatedAt = a.Now
			input.CursorEpoch = clock.Epoch
			input.CursorSeq = clock.HeadSeq
			clearMCPPending(&input)
		} else {
			// The service supplies only new verification/secret/TTL fields. All
			// durable delivery state comes from the locked predecessor.
			candidate := *old
			candidate.ExpiresAt = input.ExpiresAt
			candidate.SecretEnc = input.SecretEnc
			candidate.PreviousSecretEnc = input.PreviousSecretEnc
			candidate.PreviousSecretUntil = input.PreviousSecretUntil
			candidate.SecretRevision = input.SecretRevision
			candidate.VerifiedRevision = input.VerifiedRevision
			candidate.VerifiedAt = input.VerifiedAt
			candidate.UpdatedAt = a.Now
			candidate.State = mcpSubscriptionActive
			candidate.StopReason = ""
			input = candidate
			ordinaryRenewal := old.State == mcpSubscriptionActive && a.Now.Before(old.ExpiresAt) && old.CursorEpoch == clock.Epoch && old.CursorSeq >= clock.PrunedThroughSeq && !a.Replay
			if ordinaryRenewal {
				if old.SecretRevision != input.SecretRevision {
					input.Generation++
					if input.PendingSeq > 0 {
						input.PendingGeneration = input.Generation
					}
				}
			} else {
				input.Generation++
				clearMCPPending(&input)
				canResume := old.CursorEpoch == clock.Epoch && old.CursorSeq >= clock.PrunedThroughSeq && (old.State == "expired" || (old.State == mcpSubscriptionActive && !a.Now.Before(old.ExpiresAt))) && a.Now.Before(old.ExpiresAt.Add(24*time.Hour))
				if !canResume {
					input.CursorEpoch = clock.Epoch
					input.CursorSeq = clock.HeadSeq
				}
				if old.CursorEpoch != clock.Epoch || old.CursorSeq < clock.PrunedThroughSeq || old.StopReason == "capture_gap" || old.StopReason == "retention" {
					truncated = true
				}
			}
		}
		forceHead := old != nil && old.State == "stopped" && (old.StopReason == "retention" || old.StopReason == "capture_gap")
		if forceHead {
			input.CursorEpoch = clock.Epoch
			input.CursorSeq = clock.HeadSeq
			truncated = true
			clearMCPPending(&input)
		} else if a.Replay {
			if a.ReplayEpoch < 0 || a.ReplayEpoch > clock.Epoch || a.ReplaySeq > clock.HeadSeq || a.ReplaySeq < 0 {
				return mcpStoreError("invalid_cursor")
			}
			if a.ReplayEpoch != clock.Epoch || a.ReplaySeq < clock.PrunedThroughSeq {
				input.CursorEpoch = clock.Epoch
				input.CursorSeq = clock.HeadSeq
				truncated = true
			} else {
				input.CursorEpoch = a.ReplayEpoch
				input.CursorSeq = a.ReplaySeq
				truncated = false
			}
			clearMCPPending(&input)
		}
		if err := s.saveMCPSubscription(ctx, tx, &input); err != nil {
			return err
		}
		result = &input
		return nil
	})
	return result, truncated, err
}

func (s *Store) EndMCPSubscription(ctx context.Context, id, principal string, now time.Time) error {
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		sub, err := s.mcpSubscription(ctx, tx, id)
		if err != nil {
			return err
		}
		if sub == nil {
			return nil
		}
		if sub.Principal != principal {
			return mcpStoreError("subscription_unavailable")
		}
		if sub.State == "unsubscribed" {
			return nil
		}
		stopMCPSubscription(sub, "unsubscribed", "", now)
		return s.saveMCPSubscription(ctx, tx, sub)
	})
}

func mcpEventMatches(sub MCPSubscription, event MCPEvent) bool {
	if sub.Name != event.Family || sub.ScopeKind != event.ScopeKind || sub.ScopeID != event.ScopeID || sub.SourceID != event.SourceID {
		return false
	}
	var arguments struct {
		IncludeFromMe    bool     `json:"include_from_me"`
		IncludeReactions *bool    `json:"include_reactions"`
		Kinds            []string `json:"kinds"`
	}
	if err := json.Unmarshal(sub.Arguments, &arguments); err != nil {
		return false
	}
	if event.Family == "msgvault.message_archived" && event.FromMe && !arguments.IncludeFromMe {
		return false
	}
	if event.Kind == "reaction" && arguments.IncludeReactions != nil && !*arguments.IncludeReactions {
		return false
	}
	if len(arguments.Kinds) > 0 {
		for _, kind := range arguments.Kinds {
			if kind == "*" || kind == event.Kind {
				return true
			}
		}
		return false
	}
	return true
}

func mcpCanRead(sub *MCPSubscription, principal string, now time.Time) bool {
	return sub != nil && sub.Principal == principal && sub.State != "unsubscribed" && sub.StopReason != "scope_removed" && sub.StopReason != "principal_revoked" && now.Before(sub.ExpiresAt.Add(24*time.Hour))
}

func (s *Store) mcpAuthorizedEvent(ctx context.Context, db cardDAVQueryer, id string, seq int64, principal string, now time.Time) (MCPEvent, error) {
	sub, err := s.mcpSubscription(ctx, db, id)
	if err != nil {
		return MCPEvent{}, err
	}
	if !mcpCanRead(sub, principal, now) {
		return MCPEvent{}, mcpStoreError("event_unavailable")
	}
	event, err := scanMCPEvent(db.QueryRowContext(ctx, s.Rebind(`SELECT `+mcpEventColumns+` FROM mcp_event_log WHERE seq=?`), seq))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPEvent{}, mcpStoreError("event_unavailable")
	}
	if err != nil {
		return MCPEvent{}, err
	}
	// A physically retained older epoch or receipt below a pruning hole is
	// readable; delivery cursors and replay restrictions do not apply here.
	if !mcpEventMatches(*sub, event) {
		return MCPEvent{}, mcpStoreError("event_unavailable")
	}
	return event, nil
}

func (s *Store) GetMCPEvent(ctx context.Context, id string, seq int64, principal string, now time.Time) (MCPEvent, error) {
	event, err := s.mcpAuthorizedEvent(ctx, s.DB(), id, seq, principal, now)
	return event, mcpSafeError(err)
}

// ReadMCPEventMessage authorizes a retained reference and loads all requested
// message details in the same read-only snapshot. Physical ID reuse cannot
// replace the row between the guard and the body/recipient/attachment reads.
func (s *Store) ReadMCPEventMessage(ctx context.Context, id string, seq int64, principal string, messageID int64, now time.Time, read func(*sql.Tx) error) error {
	tx, err := s.DB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return mcpSafeError(err)
	}
	defer func() { _ = tx.Rollback() }()
	event, err := s.mcpAuthorizedEvent(ctx, tx, id, seq, principal, now)
	if err != nil {
		return mcpSafeError(err)
	}
	if messageID <= 0 || event.MessageID != messageID || event.MessageReferenceSeq <= 0 {
		return mcpStoreError("message_unavailable")
	}
	var reference int64
	err = tx.QueryRowContext(ctx, s.Rebind(`SELECT reference_seq FROM mcp_event_message_refs WHERE message_id=?`), messageID).Scan(&reference)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && reference != event.MessageReferenceSeq) {
		return mcpStoreError("message_unavailable")
	}
	if err != nil {
		return mcpSafeError(err)
	}
	if err = read(tx); err != nil {
		return mcpSafeError(err)
	}
	return mcpSafeError(tx.Commit())
}

func (s *Store) CheckMCPSubscription(ctx context.Context, id string, generation int64, now time.Time) error {
	sub, err := s.GetMCPSubscription(ctx, id)
	if err != nil {
		return err
	}
	r := s.mcpRoot().mcpConfig.Load()
	if sub == nil || sub.Generation != generation || sub.State != mcpSubscriptionActive || !now.Before(sub.ExpiresAt) || r == nil || !r.config.Enabled || r.config.Principal != sub.Principal {
		return mcpStoreError("subscription_inactive")
	}
	return nil
}

func (s *Store) mcpDeadLetter(ctx context.Context, tx *sql.Tx, sub *MCPSubscription, statusClass string, now time.Time) error {
	_, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO mcp_event_dead_letters (subscription_id,seq,attempts,last_status_class,failed_at) VALUES (?,?,?,?,?) ON CONFLICT (subscription_id,seq) DO NOTHING`), sub.ID, sub.PendingSeq, max(sub.AttemptCount, 1), statusClass, mcpTime(now))
	if err != nil {
		return err
	}
	sub.DeadLetterCount++
	sub.CursorSeq = sub.PendingSeq
	sub.LastOutcome = "dead_letter"
	sub.UpdatedAt = now
	clearMCPPending(sub)
	return nil
}

func (s *Store) PrepareMCPDelivery(ctx context.Context, id string, generation int64, now time.Time, envelope func(MCPSubscription, MCPEvent) ([]byte, error)) (*MCPDelivery, error) {
	var result *MCPDelivery
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, clock MCPEventClock) error {
		sub, err := s.mcpSubscription(ctx, tx, id)
		if err != nil {
			return err
		}
		if sub == nil || sub.Generation != generation || sub.State != mcpSubscriptionActive {
			return nil
		}
		if !now.Before(sub.ExpiresAt) {
			stopMCPSubscription(sub, "expired", "", now)
			return s.saveMCPSubscription(ctx, tx, sub)
		}
		if sub.CursorEpoch != clock.Epoch || sub.CursorSeq < clock.PrunedThroughSeq {
			reason := "capture_gap"
			if sub.CursorEpoch == clock.Epoch {
				reason = "retention"
			}
			stopMCPSubscription(sub, "stopped", reason, now)
			return s.saveMCPSubscription(ctx, tx, sub)
		}
		for range 256 {
			var event MCPEvent
			if sub.PendingSeq > 0 {
				if now.Before(sub.NextAttemptAt) {
					return nil
				}
				if sub.AttemptCount >= 12 {
					if err := s.mcpDeadLetter(ctx, tx, sub, "attempts_exhausted", now); err != nil {
						return err
					}
					if err := s.saveMCPSubscription(ctx, tx, sub); err != nil {
						return err
					}
					continue
				}
				event, err = scanMCPEvent(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+mcpEventColumns+` FROM mcp_event_log WHERE seq=?`), sub.PendingSeq))
				if errors.Is(err, sql.ErrNoRows) {
					stopMCPSubscription(sub, "stopped", "retention", now)
					return s.saveMCPSubscription(ctx, tx, sub)
				}
				if err != nil {
					return err
				}
			} else {
				event, err = scanMCPEvent(tx.QueryRowContext(ctx, s.Rebind(`SELECT `+mcpEventColumns+` FROM mcp_event_log WHERE epoch=? AND family=? AND scope_kind=? AND scope_id=? AND seq>? ORDER BY seq LIMIT 1`), sub.CursorEpoch, sub.Name, sub.ScopeKind, sub.ScopeID, sub.CursorSeq))
				if errors.Is(err, sql.ErrNoRows) {
					return s.saveMCPSubscription(ctx, tx, sub)
				}
				if err != nil {
					return err
				}
				if !mcpEventMatches(*sub, event) {
					sub.CursorSeq = event.Seq
					continue
				}
				if event.FromMe && (event.Kind == "message" || event.Kind == "reaction") {
					if sub.FromMeWindowStart.IsZero() || !now.Before(sub.FromMeWindowStart.Add(10*time.Minute)) {
						sub.FromMeWindowStart = now
						sub.FromMeWindowCount = 0
					}
					if sub.FromMeWindowCount >= 6 {
						sub.CursorSeq = event.Seq
						sub.LoopGuardSkips++
						sub.LastOutcome = "loop_guard"
						continue
					}
					sub.FromMeWindowCount++
				}
				body, err := envelope(*sub, event)
				if err != nil {
					return err
				}
				if len(body) == 0 {
					return mcpStoreError("invalid_envelope")
				}
				sub.PendingSeq = event.Seq
				sub.PendingGeneration = sub.Generation
				if len(body) > 262144 {
					if err := s.mcpDeadLetter(ctx, tx, sub, "payload_too_large", now); err != nil {
						return err
					}
					continue
				}
				sub.PendingEnvelope = append([]byte(nil), body...)
				sub.NextAttemptAt = now
			}
			sub.AttemptCount++
			sub.UpdatedAt = now
			if err := s.saveMCPSubscription(ctx, tx, sub); err != nil {
				return err
			}
			result = &MCPDelivery{Subscription: *sub, Event: event}
			return nil
		}
		return s.saveMCPSubscription(ctx, tx, sub)
	})
	return result, err
}

func mcpStatusClass(status int) string {
	if status >= 500 && status < 600 {
		return "http_5xx"
	}
	if status >= 400 && status < 500 {
		return "http_4xx"
	}
	if status >= 300 && status < 400 {
		return "http_3xx"
	}
	return "transport_error"
}

func (s *Store) FinishMCPDelivery(ctx context.Context, id string, generation, seq int64, now time.Time, status int, retryAt time.Time) error {
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, clock MCPEventClock) error {
		sub, err := s.mcpSubscription(ctx, tx, id)
		if err != nil {
			return err
		}
		if sub == nil || sub.Generation != generation || sub.State != mcpSubscriptionActive || sub.PendingSeq != seq || sub.PendingGeneration != generation {
			return nil
		}
		if !now.Before(sub.ExpiresAt) {
			stopMCPSubscription(sub, "expired", "", now)
			return s.saveMCPSubscription(ctx, tx, sub)
		}
		if sub.CursorEpoch != clock.Epoch || sub.CursorSeq < clock.PrunedThroughSeq {
			reason := "capture_gap"
			if sub.CursorEpoch == clock.Epoch {
				reason = "retention"
			}
			stopMCPSubscription(sub, "stopped", reason, now)
			return s.saveMCPSubscription(ctx, tx, sub)
		}
		switch {
		case status >= 200 && status < 300:
			sub.CursorSeq = seq
			sub.LastOutcome = "acknowledged"
			clearMCPPending(sub)
		case status == 410:
			stopMCPSubscription(sub, "gone", "", now)
			sub.LastOutcome = "gone"
		case status == 413 || sub.AttemptCount >= 12:
			if err := s.mcpDeadLetter(ctx, tx, sub, mcpStatusClass(status), now); err != nil {
				return err
			}
		default:
			if retryAt.IsZero() {
				retryAt = now.Add(time.Second)
			}
			sub.NextAttemptAt = retryAt
			sub.LastOutcome = "retry_" + mcpStatusClass(status)
		}
		sub.UpdatedAt = now
		return s.saveMCPSubscription(ctx, tx, sub)
	})
}

func (s *Store) PruneMCPEvents(ctx context.Context, now time.Time, retention time.Duration) error {
	if retention <= 0 {
		return mcpStoreError("invalid_retention")
	}
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, clock MCPEventClock) error {
		cutoff := mcpTime(now.Add(-retention))
		var deleted sql.NullInt64
		if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT MAX(seq) FROM mcp_event_log WHERE recorded_at<?`), cutoff).Scan(&deleted); err != nil {
			return err
		}
		floor := clock.PrunedThroughSeq
		if deleted.Valid && deleted.Int64 > floor {
			floor = deleted.Int64
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM mcp_event_log WHERE recorded_at<?`), cutoff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_clock SET pruned_through_seq=? WHERE singleton=1`), floor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='retention',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE state='active' AND cursor_seq<?`), mcpTime(now), floor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM mcp_event_dead_letters WHERE failed_at<?`), cutoff); err != nil {
			return err
		}
		// Keep receipt authorization through expiry grace and prevent a
		// candidate or worker from spanning a purge and a recreated generation.
		grace := mcpTime(now.Add(-24 * time.Hour))
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM mcp_event_dead_letters WHERE EXISTS (SELECT 1 FROM mcp_event_subscriptions s WHERE s.id=mcp_event_dead_letters.subscription_id AND s.state<>'active' AND s.expires_at<=? AND s.updated_at<=?)`), grace, grace); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM mcp_event_subscriptions WHERE state<>'active' AND expires_at<=? AND updated_at<=?`), grace, grace)
		return err
	})
}

func (s *Store) ExpireMCPSubscriptions(ctx context.Context, now time.Time) error {
	return s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		_, err := tx.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_subscriptions SET state='expired',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE state='active' AND expires_at<=?`), mcpTime(now), mcpTime(now))
		return err
	})
}
