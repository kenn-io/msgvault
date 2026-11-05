package caldav

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

type Options struct {
	Name, BaseURL, Username, TimeZone string
	Calendars                         []string
	MaxEvents                         int
	Full                              bool
}
type Result struct {
	Calendars, Events, Skipped int
	Removed                    int64
}

type sourceConfig struct {
	CalendarID   string `json:"calendar_id"`
	TimeZone     string `json:"time_zone"`
	AccountEmail string `json:"account_email,omitempty"`
}

func sourceIdentifier(name, collection string) (string, error) {
	u, err := url.Parse(collection)
	if err != nil {
		return "", fmt.Errorf("parse CalDAV collection URL: %w", err)
	}
	raw, err := json.Marshal([2]string{name, carddav.CollectionURLIdentity(u)})
	return string(raw), err
}

func accountEmail(username string) string {
	email, err := search.ParseAccountAddress(username)
	if err != nil {
		return ""
	}
	return email
}

// Sync discovers and archives the selected calendars. It never writes to the
// provider. A failed run preserves its checkpoint; the next run reads a full
// view and skips events already persisted with matching metadata and times.
func (c *Client) Sync(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	configured := make([]*url.URL, len(opts.Calendars))
	for i, raw := range opts.Calendars {
		u, err := url.Parse(raw)
		if err != nil {
			return Result{}, fmt.Errorf("parse configured CalDAV calendar URL: %w", err)
		}
		configured[i] = u
	}
	budget := carddav.NewBudget(256 << 20)
	discoveryCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	calendars, err := c.discover(discoveryCtx, opts.BaseURL, budget)
	cancel()
	if err != nil {
		return Result{}, fmt.Errorf("discover CalDAV calendars: %w", err)
	}
	var result Result
	var syncErrors []error
	found := make([]bool, len(configured))
	for _, cal := range calendars {
		selected := len(configured) == 0
		for i, u := range configured {
			if carddav.SameCollectionURL(u, cal.URL) || cal.DiscoveryAliasURL != nil && carddav.SameCollectionURL(u, cal.DiscoveryAliasURL) {
				selected, found[i] = true, true
			}
		}
		if !selected {
			continue
		}
		r, err := c.syncCalendar(ctx, st, cal, opts, carddav.NewBudget(256<<20))
		result.Events += r.Events
		result.Skipped += r.Skipped
		result.Removed += r.Removed
		if err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("archive CalDAV calendar %q (%s): %w", cal.DisplayName, cal.URL, err))
			continue
		}
		result.Calendars++
	}
	for _, ok := range found {
		if !ok {
			syncErrors = append(syncErrors, errors.New("configured CalDAV calendar was not discovered"))
			break
		}
	}
	return result, errors.Join(syncErrors...)
}

type prepared struct {
	event  calevent.CalDAVEvent
	member carddav.Member
}
type skippedResource struct {
	href string
	err  error
}

// batch is one calendar read ready to persist. live holds every event that
// stays live afterward: new and changed events, unchanged archived events, and
// archived events of resources that could not be decoded.
type batch struct {
	items     []prepared
	skipped   []skippedResource
	live      map[string]struct{}
	retryFull bool
}

func prepare(ctx context.Context, st *store.Store, plan carddav.MemberPlan, inventory map[string]store.MessageMetadataRecord, zone, account string) (batch, error) {
	b := batch{live: map[string]struct{}{}}
	touched := map[string]bool{}
	for _, href := range plan.RemovedHrefs {
		touched[href] = true
	}
	for _, member := range plan.Members {
		touched[member.Href] = true
	}
	skipped := map[string]bool{}
	type decodedResource struct {
		member carddav.Member
		events []calevent.CalDAVEvent
	}
	var decoded []decodedResource
	members := slices.Clone(plan.Members)
	slices.SortFunc(members, func(a, b carddav.Member) int { return strings.Compare(a.Href, b.Href) })
	for _, member := range members {
		events, err := calevent.ParseCalDAVResource(ctx, member.Body, zone)
		if err != nil {
			skipped[member.Href] = true
			b.skipped = append(b.skipped, skippedResource{href: member.Href, err: err})
			continue
		}
		decoded = append(decoded, decodedResource{member: member, events: events})
	}
	// Unchanged and unreadable resources retain ownership before accepting updates.
	for key, record := range inventory {
		var meta calevent.CalDAVMetadata
		if err := json.Unmarshal([]byte(record.Metadata.String), &meta); err != nil {
			return b, fmt.Errorf("CalDAV archived resource metadata: %w", err)
		}
		if skipped[meta.Href] || !plan.ReplaceAll && !touched[meta.Href] {
			b.live[key] = struct{}{}
		}
	}
	for _, resource := range decoded {
		member, events := resource.member, resource.events
		duplicate := false
		for _, event := range events {
			if _, exists := b.live[event.Event.ProviderEventID]; exists {
				duplicate = true
				break
			}
		}
		if duplicate {
			b.retryFull = true
			skipped[member.Href] = true
			b.skipped = append(b.skipped, skippedResource{href: member.Href, err: errors.New("duplicate CalDAV event identity across resources")})
			continue
		}
		for _, event := range events {
			key := event.Event.ProviderEventID
			b.live[key] = struct{}{}
			record, exists := inventory[key]
			if exists {
				var meta calevent.CalDAVMetadata
				if err := json.Unmarshal([]byte(record.Metadata.String), &meta); err != nil {
					return b, fmt.Errorf("CalDAV archived resource metadata: %w", err)
				}
				if meta == eventMetadata(event, member, account) {
					matches, err := st.CalendarEventTimesMatchContext(ctx, record.ID, event.Event)
					if err != nil {
						return b, err
					}
					if matches {
						continue
					}
				}
			}
			b.items = append(b.items, prepared{event: event, member: member})
		}
	}
	for key, record := range inventory {
		var meta calevent.CalDAVMetadata
		if err := json.Unmarshal([]byte(record.Metadata.String), &meta); err != nil {
			return b, fmt.Errorf("CalDAV archived resource metadata: %w", err)
		}
		if skipped[meta.Href] {
			b.live[key] = struct{}{}
		}
	}
	return b, nil
}

func (c *Client) syncCalendar(ctx context.Context, base *store.Store, cal carddav.DiscoveredBook, opts Options, budget *carddav.Budget) (result Result, retErr error) {
	identifier, err := sourceIdentifier(opts.Name, cal.URL.String())
	if err != nil {
		return result, fmt.Errorf("CalDAV source identity: %w", err)
	}
	ownerEmail := accountEmail(opts.Username)
	src, err := base.GetOrCreateSourceContext(ctx, calevent.CalDAVSourceType, identifier)
	if err != nil {
		return result, err
	}
	if ownerEmail != "" {
		if err := base.AddAccountIdentityContext(ctx, src.ID, ownerEmail, "account-email"); err != nil {
			return result, fmt.Errorf("confirm CalDAV account identity: %w", err)
		}
	}
	runID, err := base.StartSyncContext(ctx, src.ID, calevent.CalDAVSourceType)
	if err != nil {
		return result, err
	}
	st := base.ScopedToSync(src.ID, runID)
	checkpoint := &store.Checkpoint{}
	defer func() {
		if retErr != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			retErr = errors.Join(retErr, st.FailSyncWithCheckpointContext(
				cleanup, runID, retErr.Error(), checkpoint,
			))
		}
	}()
	// Read the checkpoint after acquiring source execution ownership.
	src, err = base.GetOrCreateSourceContext(ctx, calevent.CalDAVSourceType, identifier)
	if err != nil {
		return result, err
	}
	checkpoint.PageToken = src.SyncCursor.String
	var previous sourceConfig
	if src.SyncConfig.Valid {
		if err := json.Unmarshal([]byte(src.SyncConfig.String), &previous); err != nil {
			return result, fmt.Errorf("CalDAV source config: %w", err)
		}
	}
	current := sourceConfig{CalendarID: ownerEmail, TimeZone: opts.TimeZone, AccountEmail: ownerEmail}
	configJSON, err := json.Marshal(current)
	if err != nil {
		return result, err
	}
	finalize := func(ctx context.Context) error {
		var repairErr, statsErr error
		if _, err := base.RepairAccountAttributionContext(ctx, src.ID, nil); err != nil {
			repairErr = fmt.Errorf("repair CalDAV account attribution: %w", err)
		}
		if err := st.RecomputeConversationStatsContext(ctx, src.ID); err != nil {
			statsErr = fmt.Errorf("recompute CalDAV conversation statistics: %w", err)
		}
		return errors.Join(repairErr, statsErr)
	}
	finalized := false
	defer func() {
		if retErr != nil && !finalized {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			retErr = errors.Join(retErr, finalize(cleanup))
		}
	}()
	if err := st.UpdateSourceSyncConfigContext(ctx, src.ID, string(configJSON)); err != nil {
		return result, err
	}
	if err := st.UpdateSourceDisplayNameContext(ctx, src.ID, cal.DisplayName); err != nil && !errors.Is(err, store.ErrSourceSettingsInvalid) {
		return result, err
	}
	token := src.SyncCursor.String
	forceFull := opts.Full || previous != current
	if !forceFull {
		previous, err := base.GetLatestSyncContext(ctx, src.ID, runID)
		switch {
		case errors.Is(err, store.ErrSyncRunNotFound):
		case err != nil:
			return result, fmt.Errorf("read previous CalDAV sync: %w", err)
		case previous.Status != store.SyncStatusCompleted:
			forceFull = true // A delta cannot report an event created and deleted since the failed run.
		}
	}
	if forceFull {
		token = ""
	}
	pullCtx, cancelPull := context.WithTimeout(ctx, 5*time.Minute)
	plan, err := c.pull(pullCtx, cal, token, budget)
	cancelPull()
	if err != nil {
		return result, err
	}
	ctx, cancelPersist := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelPersist()
	inventory, err := st.SourceMessageMetadataContext(ctx, src.ID, true)
	if err != nil {
		return result, err
	}
	b, err := prepare(ctx, st, plan, inventory, opts.TimeZone, ownerEmail)
	if err != nil {
		return result, err
	}
	limit := opts.MaxEvents
	if len(b.live) > limit {
		return result, fmt.Errorf("CalDAV calendar %q has %d live events; increase max_events (currently %d)", cal.DisplayName, len(b.live), limit)
	}
	for _, skip := range b.skipped {
		slog.WarnContext(ctx, "Skipping CalDAV resource", "href", skip.href, "error", skip.err)
		result.Skipped++
		checkpoint.ErrorsCount++
		if err := st.RecordSyncRunItemContext(ctx, store.SyncRunItem{SyncRunID: runID, SourceMessageID: skip.href, Phase: "ingest", Status: store.SyncRunItemStatusError, ErrorKind: "calendar_ingest_error", ErrorMessage: skip.err.Error()}); err != nil {
			return result, err
		}
	}
	keys := make([]string, 0, len(b.items))
	for _, p := range b.items {
		keys = append(keys, p.event.Event.ProviderEventID)
	}
	existing, err := st.MessageExistsBatchContext(ctx, src.ID, keys)
	if err != nil {
		return result, err
	}
	// prepare keeps components of each resource contiguous in href order.
	for i, p := range b.items {
		if err := persistEvent(ctx, st, src.ID, ownerEmail, p.event, p.member); err != nil {
			checkpoint.ErrorsCount++
			return result, err
		}
		result.Events++
		checkpoint.MessagesProcessed++
		if _, exists := existing[p.event.Event.ProviderEventID]; exists {
			checkpoint.MessagesUpdated++
		} else {
			checkpoint.MessagesAdded++
		}
		if err := st.ClearMessageDeletedFromSourceContext(ctx, src.ID, p.event.Event.ProviderEventID); err != nil {
			checkpoint.ErrorsCount++
			return result, err
		}
		if i+1 == len(b.items) || b.items[i+1].member.Href != p.member.Href {
			if err := st.UpdateSyncCheckpointContext(ctx, runID, checkpoint); err != nil {
				return result, fmt.Errorf("checkpoint CalDAV sync progress: %w", err)
			}
		}
	}
	result.Removed, err = st.ReconcileSourceMessageSnapshot(ctx, src.ID, b.live)
	if err != nil {
		return result, err
	}
	if err := finalize(ctx); err != nil {
		return result, err
	}
	finalized = true
	if err := st.UpdateSyncCheckpointContext(ctx, runID, checkpoint); err != nil {
		return result, err
	}
	if b.retryFull {
		// A duplicate may become the only remaining copy without changing again.
		// Clear the cursor so the next run enumerates it even without a delta.
		plan.NextSyncToken = ""
	}
	if err := st.CompleteSyncAndUpdateSourceCursorContext(ctx, runID, src.ID, plan.NextSyncToken); err != nil {
		return result, err
	}
	return result, nil
}

func eventMetadata(item calevent.CalDAVEvent, member carddav.Member, account string) calevent.CalDAVMetadata {
	e := item.Event
	meta := calevent.CalDAVMetadata{EventID: e.ProviderEventID, Href: member.Href, ETag: member.ETag, AccountEmail: account, Status: e.Status, ICalUID: item.ICalUID, Sequence: item.Sequence}
	meta.SetEventTimes(e)
	return meta
}

func persistEvent(ctx context.Context, st *store.Store, sourceID int64, account string, item calevent.CalDAVEvent, member carddav.Member) error {
	e := item.Event
	raw := member.Body
	meta, err := json.Marshal(eventMetadata(item, member, account))
	if err != nil {
		return err
	}
	ev, err := item.MessageEvent()
	if err != nil {
		return err
	}
	body := calevent.SerializeBody(ev)
	ev.Organizer, ev.Attendees = calsync.MapParticipants(gcalmodel.Person{Email: item.OrganizerEmail, DisplayName: item.OrganizerName}, item.ExplicitAttendees, true)
	conversation := e.ProviderEventID
	if e.SeriesProviderID != "" {
		conversation = e.SeriesProviderID
	}
	data := &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: e.ProviderEventID, SizeEstimate: int64(len(raw))},
		Conversation: &store.ConversationPersistData{SourceConversationID: conversation},
		Metadata:     new(store.NullIfEmpty(string(meta))),
		BodyText:     store.NullIfEmpty(body),
		RawMIME:      raw, RawFormat: calevent.CalDAVRawFormat,
	}
	var people []store.ParticipantPersistData
	fromIndex := -1
	if ev.Organizer.Email != "" {
		fromIndex = len(people)
		people = append(people, store.ParticipantPersistData{EmailAddress: ev.Organizer.Email, DisplayName: ev.Organizer.DisplayName, Domain: calsync.EmailDomain(ev.Organizer.Email)})
	}
	var toIndexes []int
	for _, a := range ev.Attendees {
		if a.Email == "" {
			continue
		}
		toIndexes = append(toIndexes, len(people))
		people = append(people, store.ParticipantPersistData{EmailAddress: a.Email, DisplayName: a.DisplayName, Domain: calsync.EmailDomain(a.Email)})
	}
	_, err = st.PersistMessageWithParticipantsContext(ctx, people, func(ids []int64) *store.MessagePersistData {
		senderID := int64(0)
		if fromIndex >= 0 {
			senderID = ids[fromIndex]
		}
		toIDs := make([]int64, len(toIndexes))
		for i, index := range toIndexes {
			toIDs[i] = ids[index]
		}
		return calsync.MapEventMessage(ev, account, data, senderID, toIDs)
	})
	if err != nil {
		return fmt.Errorf("persist CalDAV event: %w", err)
	}
	return nil
}
