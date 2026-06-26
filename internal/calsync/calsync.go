// Package calsync orchestrates read-only Google Calendar sync, mirroring
// internal/sync for Gmail. It enumerates an account's calendars, full-syncs and
// incrementally syncs their events via internal/gcal, and persists each event as
// a messages row (message_type=calendar_event) through the canonical store write
// path plus the messages.metadata helper. Calendars are sources keyed on a
// natural per-calendar identifier, decoupled from the OAuth account/token key
// which lives in sync_config.account_email.
package calsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

const (
	accessRoleOwner  = "owner"
	accessRoleWriter = "writer"
	accessRoleReader = "reader"
)

const calendarFullCheckpointKind = "gcal_full_v1"

// EmbedEnqueuer matches sync.EmbedEnqueuer: nil disables vector enqueue.
type EmbedEnqueuer interface {
	EnqueueMessages(ctx context.Context, messageIDs []int64) error
}

// Options configures a calendar sync run.
type Options struct {
	// AccountEmail is the OAuth account that owns the token; it keys token
	// lookup and is stored in each source's sync_config. Never the source
	// identifier.
	AccountEmail string
	// OAuthApp is the named OAuth app binding to persist on new sources (""=default).
	OAuthApp string
	// Calendars restricts sync to these calendar IDs (empty = access-role filter).
	Calendars []string
	// AllCalendars includes reader/freeBusyReader calendars (default: owner+writer).
	AllCalendars bool
	// MinAccessRole overrides the default minimum access role ("writer").
	MinAccessRole string
	// TimeMin/TimeMax bound a full sync (RFC3339). Full-sync only; ignored on
	// incremental (the API rejects them with a syncToken).
	TimeMin string
	TimeMax string
	// Limit caps events ingested per calendar (0 = unlimited).
	Limit int
	// NoResume forces a fresh full sync instead of resuming an interrupted one.
	NoResume bool
}

// Result summarizes a sync run.
type Result struct {
	CalendarsSynced int
	EventsAdded     int
	EventsCancelled int
	InsertedIDs     []int64
}

// Syncer runs calendar syncs against a gcal.API and a store.Store.
type Syncer struct {
	client gcal.API
	store  *store.Store
	opts   Options
	logger *slog.Logger
	enq    EmbedEnqueuer
}

// New builds a Syncer.
func New(client gcal.API, st *store.Store, opts Options) *Syncer {
	opts.AccountEmail = normalizeAccountEmail(opts.AccountEmail)
	return &Syncer{
		client: client,
		store:  st,
		opts:   opts,
		logger: slog.Default(),
	}
}

// WithLogger sets the structured logger and returns the Syncer for chaining.
func (s *Syncer) WithLogger(l *slog.Logger) *Syncer {
	if l != nil {
		s.logger = l
	}
	return s
}

// SetEmbedEnqueuer wires the optional vector-search enqueuer (nil = disabled).
func (s *Syncer) SetEmbedEnqueuer(e EmbedEnqueuer) { s.enq = e }

// Full enumerates calendars and runs a full sync of each. A per-calendar
// failure is logged and does not abort the remaining calendars; the first such
// error is returned after all calendars are attempted.
func (s *Syncer) Full(ctx context.Context) (Result, error) {
	if err := ValidateMinAccessRole(s.opts.MinAccessRole); err != nil {
		return Result{}, err
	}
	cals, err := s.listCalendars(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("enumerate calendars: %w", err)
	}

	var result Result
	var firstErr error
	for _, cal := range cals {
		if !s.includeCalendar(cal) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := s.syncCalendarFull(ctx, cal, &result); err != nil {
			s.logger.Error("calendar full sync failed", "calendar", cal.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		result.CalendarsSynced++
	}
	s.enqueue(ctx, result.InsertedIDs)
	return result, firstErr
}

// RegisterCalendars enumerates the account's calendars, applies the selection
// filter, and creates/updates a source row per selected calendar WITHOUT syncing
// events. Used by `add-calendar`, where the calendarList.list call also doubles
// as a smoke test that the calendar scope was actually granted. Returns the
// registered calendars.
func (s *Syncer) RegisterCalendars(ctx context.Context) ([]gcal.Calendar, error) {
	if err := ValidateMinAccessRole(s.opts.MinAccessRole); err != nil {
		return nil, err
	}
	cals, err := s.listCalendars(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate calendars: %w", err)
	}
	var registered []gcal.Calendar
	for _, cal := range cals {
		if !s.includeCalendar(cal) {
			continue
		}
		src, err := s.getOrCreateCalendarSource(cal)
		if err != nil {
			return nil, fmt.Errorf("get/create source for %s: %w", cal.ID, err)
		}
		if err := s.store.UpdateSourceSyncConfig(src.ID, s.sourceConfigJSON(cal)); err != nil {
			return nil, fmt.Errorf("write sync config for %s: %w", cal.ID, err)
		}
		if s.opts.OAuthApp != "" {
			if err := s.store.UpdateSourceOAuthApp(src.ID, sql.NullString{String: s.opts.OAuthApp, Valid: true}); err != nil {
				return nil, fmt.Errorf("write oauth app for %s: %w", cal.ID, err)
			}
		}
		registered = append(registered, cal)
	}
	return registered, nil
}

// listCalendars paginates calendarList.list.
func (s *Syncer) listCalendars(ctx context.Context) ([]gcal.Calendar, error) {
	var all []gcal.Calendar
	pageToken := ""
	for {
		page, err := s.client.ListCalendars(ctx, pageToken)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return all, nil
}

// syncCalendarFull full-syncs one calendar, persisting events and advancing the
// stored syncToken cursor only after the final page succeeds. Complete,
// unbounded full syncs resume from a checkpointed pageToken unless NoResume is
// set. Bounded/limited runs deliberately do not checkpoint page tokens because
// replaying that token under a later unbounded request can skip or corrupt the
// traversal.
func (s *Syncer) syncCalendarFull(ctx context.Context, cal gcal.Calendar, result *Result) error {
	src, err := s.getOrCreateCalendarSource(cal)
	if err != nil {
		return fmt.Errorf("get/create source: %w", err)
	}
	if err := s.store.UpdateSourceSyncConfig(src.ID, s.sourceConfigJSON(cal)); err != nil {
		return fmt.Errorf("write sync config: %w", err)
	}
	if s.opts.OAuthApp != "" {
		if err := s.store.UpdateSourceOAuthApp(src.ID, sql.NullString{String: s.opts.OAuthApp, Valid: true}); err != nil {
			return fmt.Errorf("write oauth app: %w", err)
		}
	}

	// Resume an interrupted run from its checkpoint, then ALWAYS StartSync. We do
	// NOT reuse the prior run's id: StartSync is the only path that takes the
	// source row's writer lock and supersedes other 'running' runs, so going
	// through it serializes concurrent/overlapping full syncs (a manual run racing
	// the daemon) instead of two callers sharing — and clobbering — one sync_run
	// row. The prior run's counters are carried forward so a resumed run's stats
	// stay accurate (UpdateSyncCheckpoint overwrites counters absolutely).
	var resumePageToken string
	var priorProcessed, priorAdded, priorUpdated int64
	resumeEligible := s.fullSyncResumeEligible()
	if resumeEligible {
		if active, _ := s.store.GetActiveSync(src.ID); active != nil && active.Status == store.SyncStatusRunning {
			if pageToken, ok := decodeCalendarFullCheckpoint(active.CursorBefore); ok {
				resumePageToken = pageToken
				priorProcessed = active.MessagesProcessed
				priorAdded = active.MessagesAdded
				priorUpdated = active.MessagesUpdated
				s.logger.Info("resuming interrupted calendar sync", "calendar", cal.ID, "page_token", resumePageToken)
			} else {
				s.logger.Info("ignoring legacy calendar sync checkpoint; restarting full sync", "calendar", cal.ID)
			}
		}
	}
	syncID, err := s.store.StartSync(src.ID, "full")
	if err != nil {
		return fmt.Errorf("start sync: %w", err)
	}

	cp := store.Checkpoint{
		PageToken:         resumePageToken,
		MessagesProcessed: priorProcessed,
		MessagesAdded:     priorAdded,
		MessagesUpdated:   priorUpdated,
	}
	pageToken := resumePageToken
	finalToken := ""
	ingested := 0
	limitHit := false

	fail := func(e error) error {
		_ = s.store.FailSync(syncID, e.Error())
		return e
	}

	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		page, err := s.client.ListEvents(ctx, cal.ID, gcal.EventsListParams{
			SingleEvents: false,
			ShowDeleted:  true,
			MaxResults:   2500,
			TimeMin:      s.opts.TimeMin,
			TimeMax:      s.opts.TimeMax,
			PageToken:    pageToken,
		})
		if err != nil {
			return fail(fmt.Errorf("events.list: %w", err))
		}

		for i := range page.Items {
			if s.opts.Limit > 0 && ingested >= s.opts.Limit {
				limitHit = true
				break
			}
			ev := page.Items[i]
			added, cancelled, err := s.persistOne(src.ID, cal, ev, result)
			if err != nil {
				return fail(fmt.Errorf("persist event %s: %w", ev.ID, err))
			}
			ingested++
			cp.MessagesProcessed++
			if added {
				cp.MessagesAdded++
			}
			if cancelled {
				cp.MessagesUpdated++
			}
		}

		if page.NextSyncToken != "" {
			finalToken = page.NextSyncToken
		}
		pageToken = page.NextPageToken
		cp.PageToken = pageToken
		if resumeEligible {
			checkpoint := cp
			checkpoint.PageToken = encodeCalendarFullCheckpoint(pageToken)
			if err := s.store.UpdateSyncCheckpoint(syncID, &checkpoint); err != nil {
				return fail(fmt.Errorf("checkpoint: %w", err))
			}
		}
		if pageToken == "" || limitHit {
			break
		}
	}

	// A partial traversal must NOT establish an incremental baseline: a --limit
	// run deliberately skips events, and a --after/--before (TimeMin/TimeMax) run
	// only sees a time window. In both cases — on a single-page calendar, where
	// the API returns events AND the final nextSyncToken together — the cursor
	// would advance past un-ingested events, and the next incremental sync (which
	// carries no time bounds) would never see them (silent data loss). Only a
	// complete, unbounded traversal advances the cursor.
	if limitHit || s.opts.TimeMin != "" || s.opts.TimeMax != "" {
		finalToken = ""
	}
	if finalToken != "" {
		if err := s.store.UpdateSourceSyncCursor(src.ID, finalToken); err != nil {
			return fail(fmt.Errorf("update cursor: %w", err))
		}
	}
	if err := s.store.CompleteSync(syncID, finalToken); err != nil {
		return fail(fmt.Errorf("complete sync: %w", err))
	}
	if err := s.store.RecomputeConversationStats(src.ID); err != nil {
		s.logger.Warn("recompute conversation stats failed", "calendar", cal.ID, "error", err)
	}
	_ = s.store.CheckpointWAL()
	s.logger.Info("calendar full sync complete",
		"calendar", cal.ID, "events_processed", cp.MessagesProcessed,
		"events_added", cp.MessagesAdded, "events_cancelled", cp.MessagesUpdated)
	return nil
}

func (s *Syncer) fullSyncResumeEligible() bool {
	return !s.opts.NoResume &&
		s.opts.Limit == 0 &&
		s.opts.TimeMin == "" &&
		s.opts.TimeMax == ""
}

type calendarFullCheckpoint struct {
	Kind      string `json:"kind"`
	PageToken string `json:"page_token"`
}

func encodeCalendarFullCheckpoint(pageToken string) string {
	b, err := json.Marshal(calendarFullCheckpoint{
		Kind:      calendarFullCheckpointKind,
		PageToken: pageToken,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeCalendarFullCheckpoint(raw sql.NullString) (string, bool) {
	if !raw.Valid || raw.String == "" {
		return "", false
	}
	var cp calendarFullCheckpoint
	if err := json.Unmarshal([]byte(raw.String), &cp); err != nil {
		return "", false
	}
	if cp.Kind != calendarFullCheckpointKind {
		return "", false
	}
	return cp.PageToken, true
}

// persistOne routes an event to ingest or cancellation handling and updates the
// run result. Returns (added, cancelled).
func (s *Syncer) persistOne(sourceID int64, cal gcal.Calendar, ev gcal.Event, result *Result) (bool, bool, error) {
	if ev.IsCancelled() {
		id, inserted, err := s.flagCancelled(sourceID, cal, ev)
		if err != nil {
			return false, false, err
		}
		result.EventsCancelled++
		if id != 0 {
			result.InsertedIDs = append(result.InsertedIDs, id)
		}
		return inserted, true, nil
	}
	id, err := s.ingestEvent(sourceID, cal, ev)
	if err != nil {
		return false, false, err
	}
	result.EventsAdded++
	result.InsertedIDs = append(result.InsertedIDs, id)
	return true, false, nil
}

// enqueue forwards ingested ids to the embed worker if vector search is enabled.
func (s *Syncer) enqueue(ctx context.Context, ids []int64) {
	if s.enq == nil || len(ids) == 0 {
		return
	}
	if err := s.enq.EnqueueMessages(ctx, ids); err != nil {
		s.logger.Warn("enqueue events for embedding failed", "count", len(ids), "error", err)
	}
}

// includeCalendar applies the calendar selection filter.
func (s *Syncer) includeCalendar(cal gcal.Calendar) bool {
	if cal.Deleted {
		return false
	}
	if len(s.opts.Calendars) > 0 {
		return slices.Contains(s.opts.Calendars, cal.ID)
	}
	if s.opts.AllCalendars {
		return true
	}
	return accessRoleRank(cal.AccessRole) >= accessRoleRank(s.minAccessRole())
}

func (s *Syncer) minAccessRole() string {
	if s.opts.MinAccessRole != "" {
		return s.opts.MinAccessRole
	}
	return accessRoleWriter
}

// ValidateMinAccessRole checks the user-supplied minimum role. Calendar access
// roles below reader, such as freeBusyReader, may still appear on calendars but
// are not accepted as minimum-selection flags because the CLI advertises
// owner|writer|reader only.
func ValidateMinAccessRole(role string) error {
	if role == "" {
		return nil
	}
	switch role {
	case accessRoleOwner, accessRoleWriter, accessRoleReader:
		return nil
	default:
		return fmt.Errorf("invalid min access role %q (expected owner|writer|reader)", role)
	}
}

// accessRoleRank orders calendar access roles so a minimum can be enforced.
func accessRoleRank(role string) int {
	switch role {
	case accessRoleOwner:
		return 3
	case accessRoleWriter:
		return 2
	case accessRoleReader:
		return 1
	default: // freeBusyReader, unknown
		return 0
	}
}

// sourceIdentifier is the natural per-calendar key, scoped by account to avoid
// collisions when two accounts subscribe to the same shared calendar ID.
func (s *Syncer) sourceIdentifier(cal gcal.Calendar) string {
	return s.opts.AccountEmail + "/" + cal.ID
}

func (s *Syncer) getOrCreateCalendarSource(cal gcal.Calendar) (*store.Source, error) {
	identifier := s.sourceIdentifier(cal)

	sources, err := s.store.GetSourcesByTypeAndAccount(gcal.SourceType, s.opts.AccountEmail)
	if err != nil {
		return nil, fmt.Errorf("find existing calendar sources: %w", err)
	}

	var migrate *store.Source
	for _, src := range sources {
		cfg := parseSourceConfig(src.SyncConfig)
		if cfg.CalendarID != cal.ID {
			continue
		}
		if src.Identifier == identifier {
			return src, nil
		}
		if migrate == nil {
			migrate = src
		}
	}
	if migrate != nil {
		if err := s.store.UpdateSourceIdentifier(migrate.ID, identifier); err != nil {
			return nil, fmt.Errorf("migrate calendar source identifier: %w", err)
		}
		migrate.Identifier = identifier
		return migrate, nil
	}

	return s.store.GetOrCreateSource(gcal.SourceType, identifier)
}

func normalizeAccountEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// sourceConfigJSON builds the sync_config payload. account_email is the token
// key; the rest is descriptive.
func (s *Syncer) sourceConfigJSON(cal gcal.Calendar) string {
	return buildSourceConfigJSON(sourceConfig{
		AccountEmail:    s.opts.AccountEmail,
		CalendarID:      cal.ID,
		CalendarSummary: cal.Summary,
		AccessRole:      cal.AccessRole,
		Primary:         cal.Primary,
		TimeZone:        cal.TimeZone,
	})
}

// emailDomain returns the lowercased domain part of an email, or "".
func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}
