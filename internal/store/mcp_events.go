package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	// Stored capture state, read under the clock lock. Another process can
	// change it while this one keeps its in-memory runtime.
	enabled     bool
	fingerprint string
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
	config      MCPEventsConfig
	kinds       map[string]map[string]bool
	fingerprint string
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
	r.fingerprint = hex.EncodeToString(sum[:])
	return r, r.fingerprint
}

func (s *Store) mcpIdentityFence(ctx context.Context, tx contextQuerier) error {
	if _, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key,value) VALUES (?,'0') ON CONFLICT (key) DO NOTHING`), identityRevisionKey); err != nil {
		return err
	}
	if s.IsPostgreSQL() {
		// FOR UPDATE also excludes disabled writers holding FOR KEY SHARE.
		_, err := tx.ExecContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key=? FOR UPDATE`), identityRevisionKey)
		return err
	}
	_, err := tx.ExecContext(ctx, s.Rebind(`UPDATE archive_metadata SET value=value WHERE key=?`), identityRevisionKey)
	return err
}

func (s *Store) beginMCPWriterTx(ctx context.Context, opts *sql.TxOptions, runtime *mcpRuntime) (*loggedTx, error) {
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*loggedTx, error) {
		_ = tx.Rollback()
		return nil, err
	}
	if s.IsPostgreSQL() && !runtime.config.Enabled {
		// KEY SHARE excludes capture changes but lets ordinary identity updates
		// proceed without two writers deadlocking on a SHARE-to-write upgrade.
		var revision string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=? FOR KEY SHARE`, identityRevisionKey).Scan(&revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fail(err)
		}
		if err == nil {
			var enabled bool
			if err := tx.QueryRowContext(ctx, `SELECT enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&enabled); err != nil {
				return fail(err)
			}
			if !enabled {
				return tx, nil
			}
		}
		// Restart before any mutation or source lock instead of upgrading a
		// shared lock held by competing writers. Recheck capture exclusively.
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		tx, err = s.db.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
	}
	// Identity precedes source, sync-generation, and Events clock locks.
	if err := s.mcpIdentityFence(ctx, tx.Tx); err != nil {
		return fail(err)
	}
	return tx, nil
}

func (s *Store) mcpClockLock(ctx context.Context, tx contextRowQuerier) (MCPEventClock, error) {
	var clock MCPEventClock
	err := tx.QueryRowContext(ctx, `UPDATE mcp_event_clock SET head_seq=head_seq WHERE singleton=1 RETURNING head_seq,pruned_through_seq,capture_epoch,enabled,coverage_fingerprint`).Scan(&clock.HeadSeq, &clock.PrunedThroughSeq, &clock.Epoch, &clock.enabled, &clock.fingerprint)
	return clock, err
}

// mcpFenceWriterTx runs under the identity fence shared with ConfigureMCPEvents.
// A disabled writer may outlive a daemon restart that enables capture. Its next
// mutation must stop capture in the same transaction, so an uncaptured commit
// cannot leave subscriptions claiming uninterrupted coverage. When capture is
// already off, the clock stays untouched.
func (s *Store) mcpFenceWriterTx(ctx context.Context, tx *sql.Tx, runtime *mcpRuntime) error {
	if runtime.config.Enabled {
		_, err := s.mcpClockLock(ctx, tx)
		return mcpSafeError(err)
	}
	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&enabled); err != nil {
		return mcpSafeError(err)
	}
	if !enabled {
		return nil
	}
	_, err := s.mcpChangeCaptureTx(ctx, tx, runtime)
	return mcpSafeError(err)
}

func (s *Store) mcpChangeCaptureTx(ctx context.Context, tx *sql.Tx, runtime *mcpRuntime) (MCPEventClock, error) {
	now := mcpTime(time.Now().UTC())
	var clock MCPEventClock
	//nolint:gosec // The SQL is static; Rebind only changes placeholders.
	err := tx.QueryRowContext(ctx, s.Rebind(`UPDATE mcp_event_clock SET capture_epoch=capture_epoch+1,epoch_started_at=?,coverage_fingerprint=?,enabled=? WHERE singleton=1 RETURNING head_seq,pruned_through_seq,capture_epoch,enabled,coverage_fingerprint`), now, runtime.fingerprint, runtime.config.Enabled).Scan(&clock.HeadSeq, &clock.PrunedThroughSeq, &clock.Epoch, &clock.enabled, &clock.fingerprint)
	if err != nil {
		return MCPEventClock{}, err
	}
	// Active subscriptions keep their old-epoch cursors. The next delivery
	// pass or refresh sees the epoch mismatch, resets to the head and reports
	// the gap. Delivery checks require a current-epoch cursor, so nothing is
	// sent from the old epoch in the meantime.
	return clock, nil
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
		if clock.fingerprint != fingerprint {
			var err error
			result, err = root.mcpChangeCaptureTx(ctx, tx, runtime)
			if err != nil {
				return err
			}
		}
		if cfg.Principal == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, root.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='principal_revoked',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=? WHERE principal_id<>? AND stop_reason<>'principal_revoked'`), mcpTime(time.Now().UTC()), cfg.Principal)
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
	var enabled, active, revocable bool
	err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT head_seq, pruned_through_seq, capture_epoch, enabled,
		EXISTS (SELECT 1 FROM mcp_event_subscriptions WHERE state='active'),
		EXISTS (SELECT 1 FROM mcp_event_subscriptions WHERE principal_id<>? AND stop_reason<>'principal_revoked')
		FROM mcp_event_clock WHERE singleton=1`), principal).Scan(&clock.HeadSeq, &clock.PrunedThroughSeq, &clock.Epoch, &enabled, &active, &revocable)
	if err != nil {
		return MCPEventClock{}, false, fmt.Errorf("read Events capture state: %w", err)
	}
	revocable = revocable && principal != ""
	return clock, !enabled && !active && !revocable, nil
}

func (s *Store) ValidateMCPEventScope(ctx context.Context, family, scopeKind string, scopeID int64, kinds []string) (string, int64, error) {
	return s.validateMCPEventScope(ctx, s.DB(), s.mcpRoot().mcpConfig.Load(), family, scopeKind, scopeID, kinds)
}

func (s *Store) validateMCPEventScope(ctx context.Context, db SQLReader, r *mcpRuntime, family, scopeKind string, scopeID int64, kinds []string) (string, int64, error) {
	wantScope := "conversation"
	if family == "msgvault.calendar_event_changed" {
		wantScope = "source"
	}
	if scopeKind != wantScope {
		return "", 0, mcpStoreError("wrong_scope_type")
	}
	var sourceType string
	var sourceID int64
	var err error
	if scopeKind == "source" {
		err = db.QueryRowContext(ctx, s.Rebind(`SELECT source_type,id FROM sources WHERE id=?`), scopeID).Scan(&sourceType, &sourceID)
	} else {
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
	return sourceType, sourceID, nil
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
	now := mcpTime(time.Now().UTC())
	_, err := db.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='scope_removed',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=?,last_outcome=CASE WHEN state='active' AND expires_at>? THEN 'terminate_pending' ELSE last_outcome END WHERE source_id=? AND stop_reason<>'scope_removed'`), now, now, sourceID)
	return mcpSafeError(err)
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
	now := mcpTime(time.Now().UTC())
	_, err := db.ExecContext(ctx, s.Rebind(`UPDATE mcp_event_subscriptions SET state='stopped',stop_reason='scope_removed',generation=generation+1,pending_seq=NULL,pending_envelope=NULL,pending_generation=NULL,attempt_count=0,next_attempt_at=NULL,updated_at=?,last_outcome=CASE WHEN state='active' AND expires_at>? THEN 'terminate_pending' ELSE last_outcome END WHERE scope_kind='conversation' AND scope_id IN (`+targets+`) AND stop_reason<>'scope_removed'`), append([]any{now, now}, args...)...)
	return mcpSafeError(err)
}
