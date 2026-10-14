package caldav

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

type Options struct {
	Name, BaseURL, Username, TimeZone string
	Calendars                         []string
	Full                              bool
}
type Result struct {
	Calendars, Events int
	Removed           int64
}
type sourceConfig struct {
	CalendarID   string `json:"calendar_id"`
	TimeZone     string `json:"time_zone"`
	AccountEmail string `json:"account_email,omitempty"`
}

func sourceIdentifier(name, collection string) string {
	raw, _ := json.Marshal([2]string{name, collection})
	return string(raw)
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

func participantDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// Sync discovers and archives the selected calendars. It never writes to the
// provider. A failed run preserves its checkpoint; the next run requests a
// full view to reconcile any items persisted before the failure.
func (c *Client) Sync(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if opts.Name == "" {
		return Result{}, errors.New("CalDAV connection name required")
	}
	if opts.TimeZone == "" {
		opts.TimeZone = "UTC"
	}
	if opts.TimeZone == "Local" {
		return Result{}, errors.New("CalDAV time_zone cannot be Local")
	}
	if _, err := time.LoadLocation(opts.TimeZone); err != nil {
		return Result{}, fmt.Errorf("CalDAV time_zone: %w", err)
	}
	opts.Calendars = slices.Clone(opts.Calendars)
	for i, raw := range opts.Calendars {
		u, err := url.Parse(raw)
		if err != nil {
			return Result{}, fmt.Errorf("parse configured CalDAV calendar URL: %w", err)
		}
		opts.Calendars[i] = canonicalURL(u).String()
	}
	budget := carddav.NewBudget(256 << 20)
	calendars, err := c.discover(ctx, opts.BaseURL, budget)
	if err != nil {
		return Result{}, fmt.Errorf("discover CalDAV calendars: %w", err)
	}
	var result Result
	selected := map[string]bool{}
	for _, cal := range calendars {
		if len(opts.Calendars) > 0 && !slices.Contains(opts.Calendars, cal.URL.String()) {
			continue
		}
		selected[cal.URL.String()] = true
		r, err := c.syncCalendar(ctx, st, cal, opts, budget)
		result.Events += r.Events
		result.Removed += r.Removed
		if err != nil {
			return result, fmt.Errorf("archive CalDAV calendar: %w", err)
		}
		result.Calendars++
	}
	for _, requested := range opts.Calendars {
		if !selected[requested] {
			return result, errors.New("configured CalDAV calendar was not discovered")
		}
	}
	return result, nil
}
func (c *Client) syncCalendar(ctx context.Context, base *store.Store, cal Calendar, opts Options, budget *carddav.Budget) (result Result, retErr error) {
	identifier := sourceIdentifier(opts.Name, cal.URL.String())
	ownerEmail := accountEmail(opts.Username)
	src, err := base.GetOrCreateSourceContext(ctx, "caldav", identifier)
	if err != nil {
		return result, err
	}
	if ownerEmail != "" {
		if err := base.AddAccountIdentityContext(ctx, src.ID, ownerEmail, "account-email"); err != nil {
			return result, fmt.Errorf("confirm CalDAV account identity: %w", err)
		}
	}
	runID, err := base.StartSyncContext(ctx, src.ID, "caldav")
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
	src, err = base.GetOrCreateSourceContext(ctx, "caldav", identifier)
	if err != nil {
		return result, err
	}
	checkpoint.PageToken = src.SyncCursor.String
	var previous sourceConfig
	if src.SyncConfig.Valid {
		if err := json.Unmarshal([]byte(src.SyncConfig.String), &previous); err != nil {
			return result, fmt.Errorf("CalDAV source config: %w", err)
		}
		if previous.TimeZone != "" && previous.TimeZone != opts.TimeZone {
			return result, errors.New("changing CalDAV time_zone on an established source is unsupported")
		}
	}
	config, err := json.Marshal(sourceConfig{CalendarID: identifier, TimeZone: opts.TimeZone, AccountEmail: ownerEmail})
	if err != nil {
		return result, err
	}
	if err := st.UpdateSourceSyncConfigContext(ctx, src.ID, string(config)); err != nil {
		return result, err
	}
	if err := st.UpdateSourceDisplayNameContext(ctx, src.ID, cal.DisplayName); err != nil {
		return result, err
	}
	token := src.SyncCursor.String
	forceFull := opts.Full
	if !forceFull {
		previous, err := base.GetLatestSyncContext(ctx, src.ID, runID)
		switch {
		case errors.Is(err, store.ErrSyncRunNotFound):
		case err != nil:
			return result, fmt.Errorf("read previous CalDAV sync: %w", err)
		case previous.Status != store.SyncStatusCompleted:
			forceFull = true
		}
	}
	if forceFull {
		token = ""
	}
	p, err := c.pull(ctx, cal, token, budget)
	if err != nil {
		return result, err
	}
	inventory, err := st.SourceMessageMetadataLimitContext(ctx, src.ID, 20000)
	if err != nil {
		return result, err
	}
	type prepared struct {
		item       calevent.CalDAVEvent
		href, etag string
		raw        []byte
	}
	var items []prepared
	present := map[string]struct{}{}
	rawBytes := int64(0)
	sc := calevent.SourceContext{CalendarID: identifier, CalendarTimeZone: opts.TimeZone}
	hrefs := make([]string, 0, len(p.Resources))
	for href := range p.Resources {
		hrefs = append(hrefs, href)
	}
	slices.Sort(hrefs)
	for _, href := range hrefs {
		r := p.Resources[href]
		if r.Deleted {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		events, err := calevent.ParseCalDAVResource(r.Raw, sc)
		if err != nil {
			return result, fmt.Errorf("decode CalDAV resource: %w", err)
		}
		rawBytes += int64(len(r.Raw)) * int64(len(events))
		if rawBytes > 64<<20 {
			return result, errors.New("CalDAV raw amplification exceeds 64 MiB")
		}
		for _, event := range events {
			key := event.Event.ProviderEventID
			if _, duplicate := present[key]; duplicate {
				return result, errors.New("duplicate CalDAV event identity across resources")
			}
			present[key] = struct{}{}
			if len(present) > 20000 {
				return result, errors.New("CalDAV event limit exceeded")
			}
			items = append(items, prepared{item: event, href: href, etag: r.ETag, raw: r.Raw})
		}
	}
	// Historical members also count toward the bounded reconciliation inventory.
	total := len(inventory)
	for key := range present {
		if _, exists := inventory[key]; !exists {
			total++
		}
	}
	if total > 20000 {
		return result, errors.New("CalDAV archived event inventory limit exceeded")
	}
	if !p.Full {
		for _, item := range items {
			record, exists := inventory[item.item.Event.ProviderEventID]
			if !exists || record.DeletedFromSource {
				continue
			}
			var meta calevent.CalDAVMetadata
			if err := json.Unmarshal([]byte(record.Metadata.String), &meta); err != nil {
				return result, fmt.Errorf("CalDAV archived resource metadata: %w", err)
			}
			if _, touched := p.Resources[meta.Href]; !touched {
				return result, errors.New("duplicate CalDAV event identity on untouched resource")
			}
		}
	}
	for _, p := range items {
		if err := persistEvent(ctx, st, src.ID, opts.Username, p.item, p.href, p.etag, p.raw); err != nil {
			checkpoint.ErrorsCount++
			return result, err
		}
		result.Events++
		checkpoint.MessagesProcessed++
		if _, exists := inventory[p.item.Event.ProviderEventID]; exists {
			checkpoint.MessagesUpdated++
		} else {
			checkpoint.MessagesAdded++
		}
		if err := st.UpdateSyncCheckpointContext(ctx, runID, checkpoint); err != nil {
			return result, fmt.Errorf("checkpoint CalDAV sync progress: %w", err)
		}
	}
	if p.Full {
		result.Removed, err = st.ReconcileSourceMessageSnapshot(ctx, src.ID, present)
		if err != nil {
			return result, err
		}
	} else {
		// Membership comes from persisted metadata, including partial previous runs.
		// Collect all present identities before tombstoning, so a resource move
		// cannot delete the copy just archived at its new href.
		for key, record := range inventory {
			var meta calevent.CalDAVMetadata
			if err := json.Unmarshal([]byte(record.Metadata.String), &meta); err != nil {
				return result, fmt.Errorf("CalDAV archived resource metadata: %w", err)
			}
			if _, touched := p.Resources[meta.Href]; !touched {
				continue
			}
			if _, exists := present[key]; exists {
				continue
			}
			if err := st.MarkMessageDeletedContext(ctx, src.ID, key); err != nil {
				return result, err
			}
			if !record.DeletedFromSource {
				result.Removed++
			}
		}
	}
	if _, err := base.RepairAccountAttributionContext(ctx, src.ID, nil); err != nil {
		return result, fmt.Errorf("repair CalDAV account attribution: %w", err)
	}
	if err := st.RecomputeConversationStatsContext(ctx, src.ID); err != nil {
		return result, fmt.Errorf("recompute CalDAV conversation statistics: %w", err)
	}
	if err := st.CompleteSyncAndUpdateSourceCursorContext(ctx, runID, src.ID, p.Token); err != nil {
		return result, err
	}
	return result, nil
}

func persistEvent(ctx context.Context, st *store.Store, sourceID int64, account string, item calevent.CalDAVEvent, href, etag string, raw []byte) error {
	e := item.Event
	meta, err := json.Marshal(calevent.CalDAVMetadata{EventID: e.ProviderEventID, Href: href, ETag: etag, AccountEmail: account, Status: e.Status, AllDay: e.AllDay})
	if err != nil {
		return err
	}
	var people []store.ParticipantPersistData
	var fromIndex = -1
	var toIndexes []int
	var toNames, toEmails []string
	if e.OrganizerEmail != "" && !calevent.IsRoomAttendee(e.OrganizerEmail, false) {
		fromIndex = len(people)
		people = append(people, store.ParticipantPersistData{
			EmailAddress: e.OrganizerEmail,
			DisplayName:  item.OrganizerName,
			Domain:       participantDomain(e.OrganizerEmail),
		})
	}
	seen := map[string]bool{}
	for _, a := range e.Attendees {
		if a.Role == "room" || a.Email == "" || seen[a.Email] {
			continue
		}
		seen[a.Email] = true
		toIndexes = append(toIndexes, len(people))
		toNames = append(toNames, a.DisplayName)
		toEmails = append(toEmails, a.Email)
		people = append(people, store.ParticipantPersistData{
			EmailAddress: a.Email,
			DisplayName:  a.DisplayName,
			Domain:       participantDomain(a.Email),
		})
	}
	body := item.Summary
	if item.Description != "" {
		body += "\n\n" + item.Description
	}
	if e.Location != "" {
		body += "\nLocation: " + e.Location
	}
	if e.JoinURL != "" {
		body += "\nJoin: " + e.JoinURL
	}
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
	isFromMe := e.OrganizerEmail != "" && strings.EqualFold(e.OrganizerEmail, account)
	_, err = st.PersistMessageWithParticipantsContext(ctx, people, func(ids []int64) *store.MessagePersistData {
		sender := sql.NullInt64{}
		var fromIDs []int64
		var fromNames, fromEmails []string
		if fromIndex >= 0 {
			sender = sql.NullInt64{Int64: ids[fromIndex], Valid: true}
			fromIDs = []int64{ids[fromIndex]}
			fromNames = []string{item.OrganizerName}
			fromEmails = []string{e.OrganizerEmail}
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
		return &store.MessagePersistData{Message: &store.Message{SourceID: sourceID, SourceMessageID: e.ProviderEventID, MessageType: "calendar_event", Subject: *nullString(item.Summary), Snippet: *nullString(item.Summary), SentAt: sent, SenderID: sender, IsFromMe: isFromMe, IdentityDerivedIsFromMe: isFromMe, SizeEstimate: int64(len(raw))}, Conversation: &store.ConversationPersistData{SourceConversationID: conversation, ConversationType: "calendar", Title: title}, Metadata: nullString(string(meta)), BodyText: *nullString(body), RawMIME: raw, RawFormat: calevent.CalDAVRawFormat, Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: fromIDs, DisplayNames: fromNames, EmailAddresses: fromEmails}, {Type: "to", ParticipantIDs: toIDs, DisplayNames: toNames, EmailAddresses: toEmails}}, FTS: &store.FTSDoc{Subject: item.Summary, Body: body, FromAddr: e.OrganizerEmail, ToAddrs: strings.Join(toEmails, " ")}}
	})
	if err != nil {
		return fmt.Errorf("persist CalDAV event: %w", err)
	}
	return st.ClearMessageDeletedFromSourceContext(ctx, sourceID, e.ProviderEventID)
}
