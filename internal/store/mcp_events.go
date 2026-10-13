package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// IngestMode identifies a provider phase, independently of its sync run.
type IngestMode string

const (
	IngestUnknown  IngestMode = "unknown"
	IngestLive     IngestMode = "live"
	IngestBackfill IngestMode = "backfill"
)

type IngestContext struct {
	Mode       IngestMode
	ObservedAt time.Time
}

type MCPEventCapability struct {
	Family     string
	SourceType string
	Kinds      []string
}

type MCPEventsConfig struct {
	Enabled      bool
	Principal    string
	Capabilities []MCPEventCapability
	Retention    time.Duration
}

type MCPEventClock struct {
	HeadSeq          int64
	PrunedThroughSeq int64
	Epoch            int64
}

type MCPEvent struct {
	Seq, Epoch                                                                      int64
	Family, Kind, ScopeKind                                                         string
	ScopeID, MessageID, MessageReferenceSeq, ConversationID, SourceID, AttachmentID int64
	ItemKey                                                                         string
	FromMe                                                                          bool
	OccurredAt, RecordedAt                                                          time.Time
	Data                                                                            []byte
}

type mcpRuntime struct {
	config MCPEventsConfig
	kinds  map[string]map[string]bool
}

type mcpWakeCallback struct{ wake func() }

type mcpStoreError string

func (e mcpStoreError) Error() string { return string(e) }

// mcpRedactedError prints only a fixed reason, but keeps the driver error
// reachable through errors.Is/As so busy and deadlock retry loops still work.
type mcpRedactedError struct{ cause error }

func (e mcpRedactedError) Error() string { return "events_storage_unavailable" }
func (e mcpRedactedError) Unwrap() error { return e.cause }

// Driver messages can contain callback URLs, ciphertext, or archive content.
// Only fixed reasons and cancellation cross the Events Store boundary.
func mcpSafeError(err error) error {
	if err == nil {
		return nil
	}
	if redacted, ok := errors.AsType[mcpRedactedError](err); ok {
		return redacted
	}
	if safe, ok := errors.AsType[mcpStoreError](err); ok {
		return safe
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return mcpRedactedError{cause: err}
}

const mcpTimeLayout = "2006-01-02T15:04:05.000000000Z"

func mcpTime(t time.Time) string { return t.UTC().Format(mcpTimeLayout) }
func mcpParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse event timestamp: %w", err)
	}
	return t, nil
}
func mcpOptionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return mcpTime(t)
}
func mcpOptionalID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) mcpRoot() *Store {
	if s.mcpBase != nil {
		return s.mcpBase
	}
	if s.syncBase != nil {
		return s.syncBase.mcpRoot()
	}
	return s
}

func (s *Store) captureMCPEnabled() bool {
	r := s.mcpRoot().mcpConfig.Load()
	return r != nil && r.config.Enabled
}

// MCPMessageCaptureEnabled lets native adapters avoid Events-only permission
// requests when this provider has no selected live message family. It is an
// advisory snapshot; capture and delivery repeat transactional authorization.
func (s *Store) MCPMessageCaptureEnabled(sourceType string) bool {
	r := s.mcpRoot().mcpConfig.Load()
	if r == nil || !r.config.Enabled {
		return false
	}
	for _, family := range []string{"msgvault.message_archived", mcpSourceMessageFamily, mcpInboxMessageFamily} {
		if len(r.kinds[family+"\x00"+sourceType]) > 0 {
			return true
		}
	}
	return false
}

func (s *Store) ingestContext() IngestContext { return s.mcpIngest }

// WithIngestContext returns an immutable view sharing the archive pool and
// Events configuration. It preserves any existing sync-generation fence.
func (s *Store) WithIngestContext(ingest IngestContext) *Store {
	view := s.ScopedToSync(0, 0)
	view.syncGeneration = s.syncGeneration
	view.syncBase = s.syncBase
	view.mcpBase = s.mcpRoot()
	view.mcpIngest = ingest
	// ScopedToSync views defer Directory refreshes; an ingest view keeps the
	// policy of the Store it was derived from.
	view.directoryProjectionReady = s.directoryProjectionReady
	return view
}

func (s *Store) SetMCPEventsWake(wake func()) {
	if wake == nil {
		s.mcpRoot().mcpWake.Store(nil)
		return
	}
	s.mcpRoot().mcpWake.Store(&mcpWakeCallback{wake: wake})
}
func (s *Store) wakeMCPEvents() {
	if wake := s.mcpRoot().mcpWake.Load(); wake != nil {
		wake.wake()
	}
}

func mcpNewRuntime(cfg MCPEventsConfig) (*mcpRuntime, string) {
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	r := &mcpRuntime{config: cfg, kinds: make(map[string]map[string]bool)}
	for _, cap := range cfg.Capabilities {
		key := cap.Family + "\x00" + cap.SourceType
		if r.kinds[key] == nil {
			r.kinds[key] = make(map[string]bool)
		}
		for _, kind := range cap.Kinds {
			r.kinds[key][kind] = true
		}
	}
	keys := make([]string, 0, len(r.kinds))
	for key := range r.kinds {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	r.config.Capabilities = nil
	for _, key := range keys {
		family, sourceType, _ := strings.Cut(key, "\x00")
		kinds := make([]string, 0, len(r.kinds[key]))
		for kind := range r.kinds[key] {
			kinds = append(kinds, kind)
		}
		slices.Sort(kinds)
		r.config.Capabilities = append(r.config.Capabilities, MCPEventCapability{Family: family, SourceType: sourceType, Kinds: kinds})
	}
	// Principal changes revoke readers but do not change capture coverage.
	data, _ := json.Marshal(struct {
		Enabled      bool
		Capabilities []MCPEventCapability
	}{cfg.Enabled, r.config.Capabilities})
	sum := sha256.Sum256(data)
	return r, hex.EncodeToString(sum[:])
}

func mcpEventRetention(runtime *mcpRuntime) time.Duration {
	if runtime == nil || runtime.config.Retention <= 0 {
		return 7 * 24 * time.Hour
	}
	return runtime.config.Retention
}

func (s *Store) mcpIdentityFence(ctx context.Context, tx contextQuerier) error {
	if _, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key,value) VALUES (?,'0') ON CONFLICT (key) DO NOTHING`), identityRevisionKey); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, s.Rebind(`UPDATE archive_metadata SET value=value WHERE key=?`), identityRevisionKey)
	return err
}

func (s *Store) mcpClockLock(ctx context.Context, tx contextRowQuerier) (MCPEventClock, error) {
	var clock MCPEventClock
	err := tx.QueryRowContext(ctx, `UPDATE mcp_event_clock SET head_seq=head_seq WHERE singleton=1 RETURNING head_seq,pruned_through_seq,capture_epoch`).Scan(&clock.HeadSeq, &clock.PrunedThroughSeq, &clock.Epoch)
	return clock, err
}

// Events control transactions use the raw driver only inside this boundary,
// so SQL diagnostics cannot print secrets or callback identifiers.
func (s *Store) withMCPEventTx(ctx context.Context, fn func(*sql.Tx, MCPEventClock) error) error {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return mcpSafeError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.mcpIdentityFence(ctx, tx); err != nil {
		return mcpSafeError(err)
	}
	if err = s.fenceSyncGenerationTx(ctx, &loggedTx{Tx: tx, rebind: s.Rebind}); err != nil {
		return mcpSafeError(err)
	}
	clock, err := s.mcpClockLock(ctx, tx)
	if err != nil {
		return mcpSafeError(err)
	}
	if err = fn(tx, clock); err != nil {
		return mcpSafeError(err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return mcpSafeError(err)
	}
	return nil
}

// ConfigureMCPEvents records coverage once at daemon startup before writers
// begin. A changed owner revokes old subscriptions even with unchanged coverage.
// A disabled configuration with an empty Principal means the owner is unknown
// to this process; it revokes nothing.
func (s *Store) ConfigureMCPEvents(ctx context.Context, cfg MCPEventsConfig) (MCPEventClock, error) {
	root := s.mcpRoot()
	root.mcpConfigMu.Lock()
	defer root.mcpConfigMu.Unlock()
	if cfg.Enabled && cfg.Principal == "" {
		return MCPEventClock{}, mcpStoreError("owner_required")
	}
	runtime, fingerprint := mcpNewRuntime(cfg)
	if !cfg.Enabled {
		clock, unchanged, err := root.mcpCaptureAlreadyDisabled(ctx, cfg.Principal)
		if err != nil {
			return MCPEventClock{}, mcpSafeError(err)
		}
		if unchanged {
			// Nothing was captured, no subscription can be waiting, and no
			// owner change needs revocation, so there is nothing to record.
			root.mcpConfig.Store(runtime)
			return clock, nil
		}
	}
	var result MCPEventClock
	err := root.withMCPEventTx(ctx, func(tx *sql.Tx, clock MCPEventClock) error {
		result = clock
		if err := root.adoptMCPResources(ctx, tx); err != nil {
			return err
		}
		var oldFingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT coverage_fingerprint FROM mcp_event_clock WHERE singleton=1`).Scan(&oldFingerprint); err != nil {
			return err
		}
		compatible, err := root.adoptMCPScopeCoverage(ctx, tx, runtime, oldFingerprint, fingerprint)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if compatible {
			if _, err := tx.ExecContext(ctx, root.Rebind(`UPDATE mcp_event_clock SET coverage_fingerprint=?,enabled=? WHERE singleton=1`), fingerprint, cfg.Enabled); err != nil {
				return err
			}
		} else if oldFingerprint != fingerprint {
			err := tx.QueryRowContext(ctx, root.Rebind(`UPDATE mcp_event_clock SET capture_epoch=capture_epoch+1,epoch_started_at=?,coverage_fingerprint=?,enabled=? WHERE singleton=1 RETURNING head_seq,pruned_through_seq,capture_epoch`), mcpTime(now), fingerprint, cfg.Enabled).Scan(&result.HeadSeq, &result.PrunedThroughSeq, &result.Epoch)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, root.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='capture_gap',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE state='active'`), mcpTime(now)); err != nil {
				return err
			}
		}
		if cfg.Principal == "" {
			return nil
		}
		_, err = tx.ExecContext(ctx, root.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='principal_revoked',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE principal_id<>? AND stop_reason<>'principal_revoked'`), mcpTime(now), cfg.Principal)
		return err
	})
	if err != nil {
		return MCPEventClock{}, err
	}
	root.mcpConfig.Store(runtime)
	return result, nil
}

// mcpCaptureAlreadyDisabled reports, without taking a write lock, whether the
// archive's stored capture state is already off with no active subscription
// and no subscription that principal would revoke.
func (s *Store) mcpCaptureAlreadyDisabled(ctx context.Context, principal string) (MCPEventClock, bool, error) {
	var clock MCPEventClock
	var enabled, active, revocable, adopted bool
	err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT head_seq, pruned_through_seq, capture_epoch, enabled,
		EXISTS (SELECT 1 FROM mcp_event_subscriptions WHERE state='active'),
		EXISTS (SELECT 1 FROM mcp_event_subscriptions WHERE principal_id<>? AND stop_reason<>'principal_revoked'),
		EXISTS (SELECT 1 FROM archive_metadata WHERE key=?)
		FROM mcp_event_clock WHERE singleton=1`), principal, mcpResourceAdoptionKey).Scan(&clock.HeadSeq, &clock.PrunedThroughSeq, &clock.Epoch, &enabled, &active, &revocable, &adopted)
	if err != nil {
		return MCPEventClock{}, false, fmt.Errorf("read Events capture state: %w", err)
	}
	revocable = revocable && principal != ""
	return clock, !enabled && !active && !revocable && adopted, nil
}

func (s *Store) ValidateMCPEventScope(ctx context.Context, family, scopeKind string, scopeID int64, kinds []string) (string, int64, error) {
	return s.validateMCPEventScope(ctx, s.DB(), s.mcpRoot().mcpConfig.Load(), family, scopeKind, scopeID, kinds)
}

func (s *Store) validateMCPEventScope(ctx context.Context, db SQLReader, r *mcpRuntime, family, scopeKind string, scopeID int64, kinds []string) (string, int64, error) {
	wantScope := mcpScopeConversation
	if family == "msgvault.calendar_event_changed" || family == mcpSourceMessageFamily {
		wantScope = mcpScopeSource
	}
	if family == mcpInboxMessageFamily {
		wantScope = mcpScopeInbox
	}
	if scopeKind != wantScope {
		return "", 0, mcpStoreError("wrong_scope_type")
	}
	var sourceType string
	var sourceID int64
	var err error
	switch scopeKind {
	case mcpScopeSource:
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT source_type,id FROM sources WHERE id=?`), scopeID).Scan(&sourceType, &sourceID)
	case mcpScopeInbox:
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT s.source_type,s.id FROM native_message_scopes n JOIN sources s ON s.id=n.source_id WHERE n.id=? AND n.present AND EXISTS (SELECT 1 FROM mcp_source_resources sr WHERE sr.source_id=n.source_id AND sr.generation=n.source_generation)`), scopeID).Scan(&sourceType, &sourceID)
	default:
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT s.source_type,s.id FROM conversations c JOIN sources s ON s.id=c.source_id WHERE c.id=?`), scopeID).Scan(&sourceType, &sourceID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, mcpStoreError("unknown_scope")
	}
	if err != nil {
		return "", 0, mcpSafeError(err)
	}
	if r == nil || !r.config.Enabled || len(r.kinds[family+"\x00"+sourceType]) == 0 {
		return "", 0, mcpStoreError("source_not_capable")
	}
	for _, kind := range kinds {
		if !r.kinds[family+"\x00"+sourceType][kind] {
			return "", 0, mcpStoreError("kind_not_capable")
		}
	}
	if family == "msgvault.message_archived" || family == mcpSourceMessageFamily || family == mcpInboxMessageFamily {
		allowed, err := s.mcpNativeMessageAccess(ctx, db, family, sourceID)
		if err != nil {
			return "", 0, err
		}
		if !allowed {
			return "", 0, mcpStoreError("native_authority_unavailable")
		}
	}
	// A source subscription accepts new chats; its source ID is never a
	// conversation ID. Each captured occurrence still validates the native chat.
	if scopeKind == mcpScopeConversation {
		if allowed, err := s.mcpNativeConversationAllowed(ctx, db, family, sourceID, scopeID); err != nil {
			return "", 0, mcpSafeError(err)
		} else if !allowed {
			return "", 0, mcpStoreError("source_not_capable")
		}
	}
	return sourceType, sourceID, nil
}

// mcpNativeConversationAllowed applies native Teams chat and Discord container boundaries without
// requiring capture to remain enabled for retained receipt reads. Subscription
// lifetime checks separately revoke removed scopes and sources.
func (s *Store) mcpNativeConversationAllowed(ctx context.Context, db SQLReader, family string, sourceID, conversationID int64) (bool, error) {
	if family != "msgvault.message_archived" && family != mcpSourceMessageFamily && family != mcpInboxMessageFamily {
		return true, nil
	}
	var allowed bool
	err := db.QueryRowContext(ctx, s.Rebind(`SELECT
		(NOT EXISTS (SELECT 1 FROM sources WHERE id=? AND source_type='teams')
		OR EXISTS (SELECT 1 FROM conversations c WHERE c.id=? AND c.source_id=?
			AND c.conversation_type IN ('direct_chat','group_chat') AND EXISTS (SELECT 1 FROM native_message_scopes n WHERE n.source_id=c.source_id AND n.scope_kind='teams_chat' AND n.provider_id=c.source_conversation_id AND n.present AND EXISTS (SELECT 1 FROM mcp_source_resources r WHERE r.source_id=n.source_id AND r.generation=n.source_generation))))
  AND (NOT EXISTS (SELECT 1 FROM sources WHERE id=? AND source_type='discord') OR EXISTS (SELECT 1 FROM conversations c WHERE c.id=? AND c.source_id=? AND EXISTS (SELECT 1 FROM native_message_scopes n WHERE n.source_id=c.source_id AND n.scope_kind IN ('discord_channel','discord_thread') AND n.provider_id=c.source_conversation_id AND n.present AND EXISTS (SELECT 1 FROM mcp_source_resources r WHERE r.source_id=n.source_id AND r.generation=n.source_generation)))) AND (NOT EXISTS(SELECT 1 FROM sources WHERE id=? AND source_type='matrix') OR EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND c.source_id=? AND EXISTS(SELECT 1 FROM native_message_scopes n WHERE n.source_id=c.source_id AND n.scope_kind='matrix_room' AND n.provider_id=c.source_conversation_id AND n.present AND EXISTS(SELECT 1 FROM mcp_source_resources r WHERE r.source_id=n.source_id AND r.generation=n.source_generation)))) AND (NOT EXISTS(SELECT 1 FROM sources WHERE id=? AND source_type='beeper') OR EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND c.source_id=? AND EXISTS(SELECT 1 FROM native_message_scopes n WHERE n.source_id=c.source_id AND n.scope_kind='beeper_member' AND n.provider_id=c.source_conversation_id AND n.present AND EXISTS(SELECT 1 FROM mcp_source_resources r WHERE r.source_id=n.source_id AND r.generation=n.source_generation))))`), sourceID, conversationID, sourceID, sourceID, conversationID, sourceID, sourceID, conversationID, sourceID, sourceID, conversationID, sourceID).Scan(&allowed)
	return allowed, err
}

// appendMCPEventTx runs after the complete archive mutation under the outer
// identity, sync-generation and Events clock fences. It never does network I/O.
func (s *Store) appendMCPEventTx(ctx context.Context, logged *loggedTx, event MCPEvent) error {
	if !s.captureMCPEnabled() || s.mcpIngest.Mode != IngestLive {
		return nil
	}
	tx := logged.Tx
	var sourceType string
	if err := logged.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id=?`, event.SourceID).Scan(&sourceType); err != nil {
		return mcpSafeError(err)
	}
	r := s.mcpRoot().mcpConfig.Load()
	if !r.kinds[event.Family+"\x00"+sourceType][event.Kind] {
		return nil
	}
	if allowed, err := s.mcpNativeMessageAccess(ctx, tx, event.Family, event.SourceID); err != nil {
		return mcpSafeError(err)
	} else if !allowed {
		return nil
	}
	if allowed, err := s.mcpNativeConversationAllowed(ctx, tx, event.Family, event.SourceID, event.ConversationID); err != nil {
		return mcpSafeError(err)
	} else if !allowed {
		return nil
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(event.Data, &object); err != nil || object == nil {
		return mcpStoreError("invalid_event_data")
	}
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT head_seq,capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&head, &event.Epoch); err != nil {
		return mcpSafeError(err)
	}
	newReference := false
	if event.MessageID > 0 {
		err := logged.QueryRowContext(ctx, `SELECT reference_seq FROM mcp_event_message_refs WHERE message_id=?`, event.MessageID).Scan(&event.MessageReferenceSeq)
		if errors.Is(err, sql.ErrNoRows) {
			event.MessageReferenceSeq = head + 1
			newReference = true
		} else if err != nil {
			return mcpSafeError(err)
		}
	}
	if event.Family == "msgvault.draft_changed" || event.Family == "msgvault.kata_issue_filed" || event.Family == "msgvault.attachment_processed" {
		var exists bool
		err := logged.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mcp_event_log WHERE family=? AND scope_kind=? AND scope_id=? AND item_key=?)`, event.Family, event.ScopeKind, event.ScopeID, event.ItemKey).Scan(&exists)
		if err != nil {
			return mcpSafeError(err)
		}
		if exists {
			return nil
		}
	}
	if err := tx.QueryRowContext(ctx, `UPDATE mcp_event_clock SET head_seq=head_seq+1 WHERE singleton=1 RETURNING head_seq`).Scan(&event.Seq); err != nil {
		return mcpSafeError(err)
	}
	if newReference {
		if _, err := logged.ExecContext(ctx, `INSERT INTO mcp_event_message_refs (message_id,reference_seq) VALUES (?,?)`, event.MessageID, event.MessageReferenceSeq); err != nil {
			return mcpSafeError(err)
		}
	}
	event.RecordedAt = time.Now().UTC()
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.mcpIngest.ObservedAt
		if event.OccurredAt.IsZero() {
			event.OccurredAt = event.RecordedAt
		}
	}
	_, err := logged.ExecContext(ctx, `INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,message_reference_seq,conversation_id,source_id,attachment_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.Seq, event.Epoch, event.Family, event.Kind, event.ScopeKind, event.ScopeID, event.ItemKey, mcpOptionalID(event.MessageID), mcpOptionalID(event.MessageReferenceSeq), event.ConversationID, event.SourceID, mcpOptionalID(event.AttachmentID), event.FromMe, mcpTime(event.OccurredAt), mcpTime(event.RecordedAt), string(event.Data))
	if err != nil {
		return mcpSafeError(err)
	}
	if err := s.bindMCPEventResource(ctx, tx, event, false); err != nil {
		return mcpSafeError(err)
	}
	if event.Kind == "message" && event.MessageID > 0 {
		_, err = logged.ExecContext(ctx, `INSERT INTO mcp_live_admissions (message_id,source_id,message_reference_seq,epoch,admitted_at) VALUES (?,?,?,?,?) ON CONFLICT (message_id) DO UPDATE SET source_id=excluded.source_id,message_reference_seq=excluded.message_reference_seq,epoch=excluded.epoch,admitted_at=excluded.admitted_at`, event.MessageID, event.SourceID, event.MessageReferenceSeq, event.Epoch, mcpTime(event.RecordedAt))
	}
	if err != nil {
		return mcpSafeError(err)
	}
	logged.mcpEventsWritten = true
	return nil
}

// Source cleanup uses stored source IDs, independently of archive rows that
// may already be deleted. Its caller owns identity and clock fences.
func (s *Store) removeMCPEventSource(ctx context.Context, db contextQuerier, sourceID int64) error {
	queries := []string{
		`DELETE FROM mcp_event_log WHERE source_id=?`,
		`DELETE FROM mcp_event_dead_letters WHERE EXISTS (SELECT 1 FROM mcp_event_subscriptions s WHERE s.id=mcp_event_dead_letters.subscription_id AND s.source_id=?)`,
	}
	for _, query := range queries {
		if _, err := db.ExecContext(ctx, s.Rebind(query), sourceID); err != nil {
			return mcpSafeError(err)
		}
	}
	_, err := db.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='scope_removed',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE source_id=? AND stop_reason<>'scope_removed'`), mcpTime(time.Now().UTC()), sourceID)
	return mcpSafeError(err)
}

// Source admission has no conversation yet; every occurrence validates its
// actual origin separately. A source ID is never used as a conversation ID.
func (s *Store) mcpSubscriptionTeamsChatAllowed(ctx context.Context, db SQLReader, sub *MCPSubscription) (bool, error) {
	if sub.ScopeKind != mcpScopeConversation {
		return true, nil
	}
	return s.mcpNativeConversationAllowed(ctx, db, sub.Name, sub.SourceID, sub.ScopeID)
}

func (s *Store) removeMCPEventConversations(ctx context.Context, db contextQuerier, targets string, args []any) error {
	queries := []string{
		`DELETE FROM mcp_event_log WHERE conversation_id IN (` + targets + `)`,
		`DELETE FROM mcp_event_dead_letters WHERE EXISTS (SELECT 1 FROM mcp_event_subscriptions s WHERE s.id=mcp_event_dead_letters.subscription_id AND s.scope_kind='conversation' AND s.scope_id IN (` + targets + `))`,
	}
	for _, query := range queries {
		if _, err := db.ExecContext(ctx, s.Rebind(query), args...); err != nil {
			return mcpSafeError(err)
		}
	}
	_, err := db.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='scope_removed',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE scope_kind='conversation' AND scope_id IN (`+targets+`) AND stop_reason<>'scope_removed'`), append([]any{mcpTime(time.Now().UTC())}, args...)...)
	return mcpSafeError(err)
}
