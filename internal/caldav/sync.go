package caldav

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
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

func nullString(value string) *sql.NullString {
	return &sql.NullString{String: value, Valid: value != ""}
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
type metadataUpdate struct {
	href      string
	messageID int64
	metadata  sql.NullString
}

// batch is one calendar read ready to persist. live holds every event that
// stays live afterward: new and changed events, unchanged archived events, and
// archived events of resources that could not be decoded.
type batch struct {
	items           []prepared
	skipped         []skippedResource
	metadataUpdates []metadataUpdate
	live            map[string]struct{}
	retryFull       bool
}

func prepare(ctx context.Context, st *store.Store, plan carddav.MemberPlan, inventory, history map[string]store.MessageMetadataRecord, zone, account string) (batch, error) {
	b := batch{live: map[string]struct{}{}}
	accepted := map[string]string{}
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
			accepted[key] = member.Href
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
	// A source conversation ID different from the message ID identifies an
	// exception. Include historical exceptions when an accepted resource
	// contains their master, but omits them.
	storedMembers := map[string]map[string]bool{}
	for key, record := range history {
		if _, present := b.live[key]; present || !record.SourceConversationID.Valid || record.SourceConversationID.String == key {
			continue
		}
		var meta calevent.CalDAVMetadata
		if err := json.Unmarshal([]byte(record.Metadata.String), &meta); err != nil {
			return b, fmt.Errorf("CalDAV archived resource metadata: %w", err)
		}
		if skipped[meta.Href] || !plan.ReplaceAll && !touched[meta.Href] || meta.OverrideRemoved {
			continue
		}
		masterID := record.SourceConversationID.String
		if _, masterPresent := accepted[masterID]; !masterPresent {
			// A previous run may have committed the master snapshot before
			// failing to mark its removed overrides. Recover from that snapshot
			// even if the provider has since deleted the resource.
			members, loaded := storedMembers[masterID]
			if !loaded {
				master, exists := history[masterID]
				if !exists {
					continue
				}
				raw, err := st.GetMessageRawContext(ctx, master.ID)
				if err != nil {
					return b, fmt.Errorf("read archived CalDAV master: %w", err)
				}
				events, err := calevent.ParseCalDAVResource(ctx, raw, zone)
				if err != nil {
					return b, fmt.Errorf("parse archived CalDAV master: %w", err)
				}
				members = map[string]bool{}
				for _, event := range events {
					members[event.Event.ProviderEventID] = true
				}
				storedMembers[masterID] = members
			}
			if !members[masterID] || members[key] {
				continue
			}
		}
		if masterRecord, exists := inventory[masterID]; exists {
			var masterMeta calevent.CalDAVMetadata
			if err := json.Unmarshal([]byte(masterRecord.Metadata.String), &masterMeta); err != nil {
				return b, fmt.Errorf("CalDAV archived master metadata: %w", err)
			}
			if skipped[masterMeta.Href] {
				continue
			}
		}
		meta.OverrideRemoved = true
		encoded, err := json.Marshal(meta)
		if err != nil {
			return b, fmt.Errorf("encode removed CalDAV override metadata: %w", err)
		}
		href := accepted[masterID]
		if href == "" {
			href = meta.Href
		}
		b.metadataUpdates = append(b.metadataUpdates, metadataUpdate{
			href:      href,
			messageID: record.ID,
			metadata:  sql.NullString{String: string(encoded), Valid: true},
		})
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
				cleanup, runID, "CalDAV sync failed", checkpoint,
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
	history, err := st.SourceMessageMetadataContext(ctx, src.ID, false)
	if err != nil {
		return result, err
	}
	b, err := prepare(ctx, st, plan, inventory, history, opts.TimeZone, ownerEmail)
	if err != nil {
		return result, err
	}
	limit := opts.MaxEvents
	if limit == 0 {
		limit = config.DefaultCalDAVMaxEvents
	}
	if len(b.live) > limit {
		return result, fmt.Errorf("CalDAV calendar %q has %d live events; increase max_events (currently %d)", cal.DisplayName, len(b.live), limit)
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
		if retErr != nil && !finalized && result.Events > 0 {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer cancel()
			retErr = errors.Join(retErr, finalize(cleanup))
		}
	}()
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
	resources := map[string]batch{}
	for _, p := range b.items {
		r := resources[p.member.Href]
		r.items = append(r.items, p)
		resources[p.member.Href] = r
	}
	for _, update := range b.metadataUpdates {
		r := resources[update.href]
		r.metadataUpdates = append(r.metadataUpdates, update)
		resources[update.href] = r
	}
	for _, href := range slices.Sorted(maps.Keys(resources)) {
		r := resources[href]
		if err := st.WithCalendarResourceTransactionContext(ctx, func(ctx context.Context) error {
			for _, p := range r.items {
				if err := persistEvent(ctx, st, src.ID, ownerEmail, p.event, p.member); err != nil {
					return err
				}
				if err := st.ClearMessageDeletedFromSourceContext(ctx, src.ID, p.event.Event.ProviderEventID); err != nil {
					return err
				}
			}
			for _, update := range r.metadataUpdates {
				if err := st.SetMessageMetadataContext(ctx, update.messageID, update.metadata); err != nil {
					return fmt.Errorf("mark removed CalDAV override: %w", err)
				}
			}
			return nil
		}); err != nil {
			checkpoint.ErrorsCount++
			return result, err
		}
		for _, p := range r.items {
			result.Events++
			checkpoint.MessagesProcessed++
			if _, exists := existing[p.event.Event.ProviderEventID]; exists {
				checkpoint.MessagesUpdated++
			} else {
				checkpoint.MessagesAdded++
			}
		}
		if err := st.UpdateSyncCheckpointContext(ctx, runID, checkpoint); err != nil {
			return result, fmt.Errorf("checkpoint CalDAV sync progress: %w", err)
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
	return calevent.CalDAVMetadata{EventID: e.ProviderEventID, Href: member.Href, ETag: member.ETag, AccountEmail: account, TimeZone: e.CalendarTimeZone, Status: e.Status, AllDay: e.AllDay}
}

func persistEvent(ctx context.Context, st *store.Store, sourceID int64, account string, item calevent.CalDAVEvent, member carddav.Member) error {
	e := item.Event
	raw := member.Body
	meta, err := json.Marshal(eventMetadata(item, member, account))
	if err != nil {
		return err
	}
	organizer, attendees := calsync.MapParticipants(gcalmodel.Person{Email: item.OrganizerEmail, DisplayName: item.OrganizerName}, item.ExplicitAttendees, true)
	var people []store.ParticipantPersistData
	fromIndex := -1
	var toIndexes []int
	var toNames, toEmails []string
	if organizer.Email != "" {
		fromIndex = len(people)
		people = append(people, store.ParticipantPersistData{EmailAddress: organizer.Email, DisplayName: organizer.DisplayName, Domain: calsync.EmailDomain(organizer.Email)})
	}
	for _, a := range attendees {
		toIndexes = append(toIndexes, len(people))
		toNames = append(toNames, a.DisplayName)
		toEmails = append(toEmails, a.Email)
		people = append(people, store.ParticipantPersistData{EmailAddress: a.Email, DisplayName: a.DisplayName, Domain: calsync.EmailDomain(a.Email)})
	}
	bodyEvent := gcal.Event{Summary: item.Summary, Description: item.Description, Location: item.Location}
	if e.AllDay {
		bodyEvent.Start.Date, bodyEvent.End.Date = e.StartDate, e.EndDate
	} else {
		zoneName := e.EventTimeZone
		if zoneName == "" {
			zoneName = e.CalendarTimeZone
		}
		zone := time.UTC
		if zoneName != "" {
			zone, err = time.LoadLocation(zoneName)
			if err != nil {
				return fmt.Errorf("CalDAV body time zone: %w", err)
			}
		}
		if e.Start != nil {
			bodyEvent.Start.DateTime = e.Start.In(zone)
		}
		if e.End != nil {
			bodyEvent.End.DateTime = e.End.In(zone)
		}
	}
	for _, a := range e.Attendees {
		bodyEvent.Attendees = append(bodyEvent.Attendees, gcal.Attendee{DisplayName: a.DisplayName})
	}
	body := calsync.SerializeBody(bodyEvent)
	sent := sql.NullTime{}
	if e.AllDay {
		startDate := e.StartDate
		if startDate == "" && e.Original != nil && e.Original.AllDay {
			startDate = e.Original.Date
		}
		if startDate != "" {
			parsed, err := time.Parse(time.DateOnly, startDate)
			if err != nil {
				return fmt.Errorf("parse CalDAV all-day start date: %w", err)
			}
			sent = sql.NullTime{Time: parsed, Valid: true}
		}
	} else {
		startAt := e.Start
		if startAt == nil && e.Original != nil && !e.Original.AllDay {
			startAt = &e.Original.Instant
		}
		if startAt != nil {
			sent = sql.NullTime{Time: *startAt, Valid: true}
		}
	}
	isFromMe := item.OrganizerEmail != "" && strings.EqualFold(item.OrganizerEmail, account)
	_, err = st.PersistMessageWithParticipantsContext(ctx, people, func(ids []int64) *store.MessagePersistData {
		sender := sql.NullInt64{}
		var fromIDs []int64
		var fromNames, fromEmails []string
		if fromIndex >= 0 {
			sender = sql.NullInt64{Int64: ids[fromIndex], Valid: true}
			fromIDs = []int64{ids[fromIndex]}
			fromNames = []string{item.OrganizerName}
			fromEmails = []string{item.OrganizerEmail}
		}
		toIDs := make([]int64, len(toIndexes))
		for i, index := range toIndexes {
			toIDs[i] = ids[index]
		}
		conversation := e.ProviderEventID
		title := item.Summary
		if e.SeriesProviderID != "" {
			conversation = e.SeriesProviderID
		}
		if e.Kind == "exception" {
			title = ""
		}
		return &store.MessagePersistData{Message: &store.Message{SourceID: sourceID, SourceMessageID: e.ProviderEventID, MessageType: gcal.MessageTypeCalendarEvent, Subject: *nullString(item.Summary), Snippet: *nullString(calsync.Snippet(body)), SentAt: sent, SenderID: sender, IsFromMe: isFromMe, IdentityDerivedIsFromMe: isFromMe, SizeEstimate: int64(len(raw))}, Conversation: &store.ConversationPersistData{SourceConversationID: conversation, ConversationType: "calendar", Title: title}, Metadata: nullString(string(meta)), BodyText: *nullString(body), RawMIME: raw, RawFormat: calevent.CalDAVRawFormat, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: fromIDs, DisplayNames: fromNames, EmailAddresses: fromEmails}, {Type: "to", ParticipantIDs: toIDs, DisplayNames: toNames, EmailAddresses: toEmails}}, FTS: &store.FTSDoc{Subject: item.Summary, Body: body, FromAddr: item.OrganizerEmail, ToAddrs: strings.Join(toEmails, " ")}}
	})
	if err != nil {
		return fmt.Errorf("persist CalDAV event: %w", err)
	}
	return nil
}
