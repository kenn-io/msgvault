// Package mscalendar archives the calendars of a Microsoft 365 or Outlook.com
// mailbox through Microsoft Graph. Each calendar is one source. A series is
// stored once, with its edited occurrences; readers expand the rest.
package mscalendar

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
)

// SourceType is the sources.source_type value for a Graph calendar.
const SourceType = "mscalendar"

const (
	messageType      = "calendar_event"
	conversationType = "calendar"
	eventSelect      = "id,type,seriesMasterId,subject,body,start,end,isAllDay,isCancelled," +
		"originalStart,originalStartTimeZone,iCalUId,attendees,organizer,responseStatus," +
		"showAs,sensitivity,location,onlineMeeting,recurrence,webLink"
)

// Client adds the calendar endpoints to the shared Graph transport.
type Client struct {
	*msgraph.Client
}

// NewClient creates a calendar Client. Every request asks for immutable IDs,
// UTC times, and plain-text bodies.
func NewClient(baseURL string, token msgraph.TokenFunc, qps float64) *Client {
	c := msgraph.NewClient(baseURL, token, qps)
	c.Headers = map[string]string{"Prefer": `IdType="ImmutableId", outlook.timezone="UTC", outlook.body-content-type="text"`}
	return &Client{c}
}

// Calendar is one calendar of the mailbox.
type Calendar struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefaultCalendar"`
	Owner     struct {
		Address string `json:"address"`
	} `json:"owner"`
	// Zone is the mailbox time zone, set by Register.
	Zone string `json:"-"`
}

// ListCalendars returns the calendars the mailbox owns: those with the same
// owner as its default calendar. The owner is the primary address, which can
// differ from the alias the account was added with. Shared calendars are left
// out, because their events belong to another mailbox.
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	var all []Calendar
	_, err := msgraph.PageThrough(ctx, c.Client, "/me/calendars?$select=id,name,owner,isDefaultCalendar", func(page []Calendar) {
		all = append(all, page...)
	})
	if err != nil {
		return nil, err
	}
	owner := ""
	for _, cal := range all {
		if cal.IsDefault {
			owner = calevent.NormalizeEmail(cal.Owner.Address)
		}
	}
	if owner == "" {
		// Without an owner nothing can be classified, and treating every
		// calendar as gone would mark the whole archive deleted.
		return nil, errors.New("graph listed no default calendar with an owner")
	}
	var out []Calendar
	for _, cal := range all {
		if calevent.NormalizeEmail(cal.Owner.Address) == owner {
			out = append(out, cal)
		}
	}
	return out, nil
}

// MailboxTimeZone returns the mailbox time zone as an IANA name.
func (c *Client) MailboxTimeZone(ctx context.Context) (string, error) {
	body, err := c.GetRaw(ctx, "/me/mailboxSettings/timeZone")
	if err != nil {
		return "", err
	}
	var v struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("decode mailbox time zone: %w", err)
	}
	return calevent.GraphZone(v.Value), nil
}

// Summary reports what one sync did.
type Summary struct {
	Calendars int
	Events    int
	Errors    int
	Duration  time.Duration
}

type sourceConfig struct {
	AccountEmail    string `json:"account_email"`
	CalendarID      string `json:"calendar_id"`
	CalendarSummary string `json:"calendar_summary,omitempty"`
	TimeZone        string `json:"time_zone,omitempty"`
	// Primary marks the default calendar. Account attribution maps it to
	// account_email, as for a Google primary calendar.
	Primary bool `json:"primary,omitzero"`
}

// Register creates or refreshes one source per owned calendar.
func Register(ctx context.Context, st *store.Store, c *Client, email string) ([]*store.Source, []Calendar, error) {
	email = calevent.NormalizeEmail(email)
	zone, err := c.MailboxTimeZone(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read mailbox time zone: %w", err)
	}
	cals, err := c.ListCalendars(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list calendars: %w", err)
	}
	sources := make([]*store.Source, 0, len(cals))
	kept := make([]Calendar, 0, len(cals))
	for _, cal := range cals {
		cal.Zone = zone
		src, err := st.GetOrCreateSource(SourceType, email+"/"+cal.ID)
		if errors.Is(err, store.ErrSourceRetired) {
			// The user merged this calendar into another source.
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		// ListCalendars keeps only calendars of the signed-in mailbox, so
		// every one is attributed to it as a primary calendar.
		cfg, err := json.Marshal(sourceConfig{AccountEmail: email, CalendarID: cal.ID, CalendarSummary: cal.Name, TimeZone: zone, Primary: true}, json.Deterministic(true))
		if err != nil {
			return nil, nil, err
		}
		if err := st.UpdateSourceSyncConfigContext(ctx, src.ID, string(cfg)); err != nil {
			return nil, nil, err
		}
		if err := st.UpdateSourceDisplayName(src.ID, email+" / "+cal.Name); err != nil {
			return nil, nil, err
		}
		// Graph confirmed the signed-in mailbox, which owns this calendar.
		for _, address := range []string{email, calevent.NormalizeEmail(cal.Owner.Address)} {
			if err := st.AddAccountIdentityContext(ctx, src.ID, address, "account-email"); err != nil {
				return nil, nil, fmt.Errorf("confirm calendar account identity: %w", err)
			}
		}
		sources = append(sources, src)
		kept = append(kept, cal)
	}
	return sources, kept, nil
}

// Sync lists every event of every owned calendar. Graph v1.0 has no delta for
// a whole calendar, so each run lists everything; known events are upserted.
// ponytail: full listing each run, add calendarView delta if mailboxes grow
// past a few thousand events.
func Sync(ctx context.Context, st *store.Store, c *Client, email string, log *slog.Logger) (*Summary, error) {
	start := time.Now()
	email = calevent.NormalizeEmail(email)
	sources, cals, err := Register(ctx, st, c, email)
	if err != nil {
		return nil, err
	}
	sum := &Summary{Calendars: len(cals)}
	for i, src := range sources {
		if err := syncCalendar(ctx, st, c, src, cals[i], email, log, sum); err != nil {
			return sum, fmt.Errorf("calendar %s: %w", cals[i].Name, err)
		}
	}
	// A calendar that the mailbox no longer owns was deleted or moved away.
	// Its events stay in the archive, marked deleted.
	listed := map[int64]bool{}
	for _, src := range sources {
		listed[src.ID] = true
	}
	known, err := st.GetSourcesByTypeAndAccount(SourceType, email)
	if err != nil {
		return sum, err
	}
	for _, src := range known {
		if listed[src.ID] || src.MergedIntoSourceID != 0 {
			continue
		}
		if err := retireCalendar(ctx, st, src); err != nil {
			return sum, fmt.Errorf("calendar %s: %w", src.Identifier, err)
		}
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

func retireCalendar(ctx context.Context, st *store.Store, src *store.Source) (err error) {
	syncID, err := st.StartSync(src.ID, "full")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = st.FailSync(syncID, err.Error())
		}
	}()
	if _, err := st.ScopedToSync(src.ID, syncID).ReconcileSourceMessageSnapshot(ctx, src.ID, map[string]struct{}{}); err != nil {
		return err
	}
	return st.CompleteSync(syncID, "")
}

func syncCalendar(ctx context.Context, st *store.Store, c *Client, src *store.Source, cal Calendar, email string, log *slog.Logger, sum *Summary) (err error) {
	syncID, err := st.StartSync(src.ID, "full")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = st.FailSync(syncID, err.Error())
		}
	}()
	scoped := st.ScopedToSync(src.ID, syncID)
	ec := &eventContext{calendarID: cal.ID, email: email, mailbox: calevent.NormalizeEmail(cal.Owner.Address), zone: cal.Zone}
	seen := map[string]struct{}{}
	errorsBefore, eventsBefore := sum.Errors, sum.Events
	persist := func(raw []byte) {
		id, perr := persistEvent(ctx, scoped, src.ID, ec, raw)
		if perr == nil {
			// An event that returns after a deletion is current again.
			perr = scoped.ClearMessageDeletedFromSourceContext(ctx, src.ID, id)
		}
		if perr != nil {
			sum.Errors++
			log.Warn("store calendar event failed", "calendar", cal.Name, "error", perr)
			return
		}
		seen[id] = struct{}{}
		sum.Events++
	}

	var masters []string
	listURL := "/me/calendars/" + url.PathEscape(cal.ID) + "/events?$top=100&$select=" + eventSelect
	_, err = msgraph.PageThrough(ctx, c.Client, listURL, func(page []jsontext.Value) {
		for _, raw := range page {
			var head struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &head) == nil && head.Type == "seriesMaster" {
				masters = append(masters, head.ID)
				continue
			}
			persist(raw)
		}
	})
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	// A series is stored from its detail read, which carries the cancelled
	// occurrences and the edited ones.
	fetched := map[string]bool{}
	// Cancelled occurrences, as series ID and original local date, and the
	// zone of those dates.
	type cancelledKey struct{ series, date string }
	cancelled := map[cancelledKey]bool{}
	seriesLoc := map[string]*time.Location{}
	for _, id := range masters {
		fetched[id] = true
		raw, err := c.GetRaw(ctx, "/me/events/"+url.PathEscape(id)+"?$select="+eventSelect+
			",cancelledOccurrences,exceptionOccurrences&$expand=exceptionOccurrences($select="+eventSelect+")")
		if err != nil {
			return fmt.Errorf("read series: %w", err)
		}
		persist(raw)
		var detail struct {
			Exceptions []jsontext.Value `json:"exceptionOccurrences"`
			Next       string           `json:"exceptionOccurrences@odata.nextLink"`
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			return fmt.Errorf("decode series: %w", err)
		}
		// An expanded collection can page. A partial list would make the
		// missing edits look undone.
		if detail.Next != "" {
			if _, err := msgraph.PageThrough(ctx, c.Client, detail.Next, func(page []jsontext.Value) {
				detail.Exceptions = append(detail.Exceptions, page...)
			}); err != nil {
				return fmt.Errorf("read series edits: %w", err)
			}
		}
		master, err := calevent.DecodeGraphEvent(raw)
		if err != nil {
			return fmt.Errorf("decode series: %w", err)
		}
		ec.seriesAllDay, ec.seriesZone = &master.IsAllDay, calevent.GraphZone(master.OriginalStartTimeZone)
		if master.Recurrence != nil && master.Recurrence.Range.RecurrenceTimeZone != "" {
			ec.seriesZone = calevent.GraphZone(master.Recurrence.Range.RecurrenceTimeZone)
		}
		ec.seriesZone = cmp.Or(ec.seriesZone, ec.zone)
		if loc, err := time.LoadLocation(ec.seriesZone); err == nil {
			seriesLoc[master.ID] = loc
		}
		for _, id := range master.CancelledOccurrences {
			// Graph names a cancelled occurrence OID.<item id>.<original local date>.
			cancelled[cancelledKey{master.ID, id[strings.LastIndex(id, ".")+1:]}] = true
		}
		for _, exc := range detail.Exceptions {
			persist(exc)
		}
		ec.seriesAllDay, ec.seriesZone = nil, ""
	}
	if n := sum.Errors - errorsBefore; n > 0 {
		return fmt.Errorf("%d events could not be stored", n)
	}
	// The listing was complete, so an archived event it did not return was
	// deleted in Outlook. The archive keeps it, marked deleted.
	// An archived edit that its fetched series no longer lists was undone in
	// Outlook, for example by an edit to the whole series. It is not deleted.
	exceptions, err := st.CalendarSeriesEditsContext(ctx, src.ID)
	if err != nil {
		return err
	}
	var undone []string
	for id, series := range exceptions {
		if _, ok := seen[id]; !ok && fetched[series] {
			undone = append(undone, id)
		}
	}
	stored, err := st.StoredMessagesContext(ctx, src.ID, calevent.MSGraphRawFormat, undone)
	if err != nil {
		return err
	}
	ec.superseded = true
	for _, id := range undone {
		m, ok := stored[id]
		if !ok || len(m.Raw) == 0 {
			continue
		}
		// An edit whose occurrence was cancelled was deleted, not undone.
		// Reconciliation marks it deleted.
		series := exceptions[id]
		if g, err := calevent.DecodeGraphEvent(m.Raw); err == nil && seriesLoc[series] != nil {
			if original, err := time.Parse(time.RFC3339, g.OriginalStart); err == nil &&
				cancelled[cancelledKey{series, original.In(seriesLoc[series]).Format(time.DateOnly)}] {
				continue
			}
		}
		persist(m.Raw)
	}
	ec.superseded = false
	if n := sum.Errors - errorsBefore; n > 0 {
		return fmt.Errorf("%d events could not be stored", n)
	}
	if _, err := scoped.ReconcileSourceMessageSnapshot(ctx, src.ID, seen); err != nil {
		return err
	}
	if err := scoped.UpdateSyncCheckpointContext(ctx, syncID, &store.Checkpoint{MessagesProcessed: int64(sum.Events - eventsBefore)}); err != nil {
		return err
	}
	return scoped.CompleteSyncAndPreserveSourceCursorContext(ctx, syncID, src.ID, "")
}

// eventContext is what an event snapshot does not say about itself.
type eventContext struct {
	calendarID, email, mailbox, zone string
	// Set while storing a series' exceptions.
	seriesAllDay *bool
	seriesZone   string
	superseded   bool
}

func persistEvent(ctx context.Context, st *store.Store, sourceID int64, ec *eventContext, raw []byte) (string, error) {
	calendarID, email := ec.calendarID, ec.email
	g, err := calevent.DecodeGraphEvent(raw)
	if err != nil {
		return "", err
	}
	if g.ID == "" {
		return "", errors.New("event has no id")
	}
	organizer := calevent.NormalizeEmail(g.Organizer.EmailAddress.Address)
	participants := []store.ParticipantPersistData{}
	if organizer != "" {
		participants = append(participants, participant(organizer, g.Organizer.EmailAddress.Name))
	}
	var toNames, toEmails []string
	for _, a := range g.Attendees {
		addr := calevent.NormalizeEmail(a.EmailAddress.Address)
		// Rooms are resources, not people.
		if addr == "" || calevent.IsRoomAttendee(addr, a.Type == "resource") {
			continue
		}
		participants = append(participants, participant(addr, a.EmailAddress.Name))
		toNames, toEmails = append(toNames, a.EmailAddress.Name), append(toEmails, addr)
	}

	series := g.SeriesMasterID
	if series == "" {
		series = g.ID
	}
	title := g.Subject
	if g.Type == "exception" {
		// Only the series sets the shared conversation title.
		title = ""
	}
	var sentAt sql.NullTime
	if g.Start.DateTime != "" {
		t, err := calevent.GraphDateTime(g.Start)
		if g.IsAllDay {
			// As for Google, an all-day event is placed at midnight UTC of its date.
			t, err = time.Parse(time.DateOnly, calevent.GraphAllDayDate(g.Start, g.OriginalStartTimeZone, ec.zone))
		}
		if err == nil {
			sentAt = sql.NullTime{Time: t, Valid: true}
		}
	}
	body := serializeBody(g, sentAt, ec.zone)
	metaValue := buildMetadata(g, calendarID, email, ec.zone)
	if ec.mailbox != email {
		metaValue.MailboxEmail = ec.mailbox
	}
	if g.Type == "exception" {
		metaValue.SeriesAllDay, metaValue.SeriesTimeZone = ec.seriesAllDay, ec.seriesZone
	}
	metaValue.Superseded = ec.superseded
	meta, err := json.Marshal(metaValue, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	// Google metadata carries the series rule; keep the same key.
	if ev, ok, derr := calevent.Decode(calevent.MSGraphRawFormat, raw, meta, ec.zone); derr == nil && ok && len(ev.Recurrence) > 0 {
		metaValue.Recurrence = ev.Recurrence
		meta, err = json.Marshal(metaValue, json.Deterministic(true))
	}
	if err != nil {
		return "", err
	}
	metadata := sql.NullString{String: string(meta), Valid: true}
	fromMe := g.ResponseStatus.Response == "organizer" || organizer == email

	// Resolve participants first, as the Google sync does, so the message
	// write below takes the source's account attribution lock.
	ids := make([]int64, 0, len(participants))
	for _, p := range participants {
		id, err := st.EnsureParticipant(p.EmailAddress, p.DisplayName, p.Domain)
		if err != nil {
			return "", fmt.Errorf("participant: %w", err)
		}
		ids = append(ids, id)
	}
	var from []store.RecipientSet
	toIDs := ids
	if organizer != "" {
		from = []store.RecipientSet{{Type: "from", ParticipantIDs: ids[:1], DisplayNames: []string{g.Organizer.EmailAddress.Name}, EmailAddresses: []string{organizer}}}
		toIDs = ids[1:]
	} else {
		from = []store.RecipientSet{{Type: "from"}}
	}
	msg := &store.Message{
		SourceID:        sourceID,
		SourceMessageID: g.ID,
		MessageType:     messageType,
		SentAt:          sentAt,
		IsFromMe:        fromMe,
		Subject:         sql.NullString{String: g.Subject, Valid: g.Subject != ""},
		Snippet:         sql.NullString{String: calsync.Snippet(body), Valid: body != ""},
		SizeEstimate:    int64(len(body)),
	}
	if organizer != "" {
		msg.SenderID = sql.NullInt64{Int64: ids[0], Valid: true}
	}
	_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
		Message:      msg,
		Conversation: &store.ConversationPersistData{SourceConversationID: "event:" + series, ConversationType: conversationType, Title: title},
		Metadata:     &metadata,
		BodyText:     sql.NullString{String: body, Valid: body != ""},
		RawMIME:      raw,
		RawFormat:    calevent.MSGraphRawFormat,
		Recipients: append(from, store.RecipientSet{Type: "to", ParticipantIDs: toIDs,
			DisplayNames: toNames, EmailAddresses: toEmails}),
		FTS: &store.FTSDoc{Subject: g.Subject, Body: body, FromAddr: organizer, ToAddrs: strings.Join(toEmails, " ")},
	})
	return g.ID, err
}

func participant(email, name string) store.ParticipantPersistData {
	_, domain, _ := strings.Cut(email, "@")
	return store.ParticipantPersistData{EmailAddress: email, DisplayName: name, Domain: domain}
}

// eventMetadata keeps the keys that Google events store, so readers of
// messages.metadata see both providers the same way.
type eventMetadata struct {
	Status            string   `json:"status,omitempty"`
	AllDay            bool     `json:"all_day"`
	Start             string   `json:"start,omitempty"`
	End               string   `json:"end,omitempty"`
	TimeZone          string   `json:"time_zone,omitempty"`
	Recurrence        []string `json:"recurrence,omitempty"`
	RecurringEventID  string   `json:"recurring_event_id,omitempty"`
	OriginalStartTime string   `json:"original_start_time,omitempty"`
	ICalUID           string   `json:"ical_uid,omitempty"`
	HTMLLink          string   `json:"html_link,omitempty"`
	JoinURL           string   `json:"join_url,omitempty"`
	Transparency      string   `json:"transparency,omitempty"`
	Visibility        string   `json:"visibility,omitempty"`
	OrganizerEmail    string   `json:"organizer_email,omitempty"`
	CalendarID        string   `json:"calendar_id,omitempty"`
	AccountEmail      string   `json:"account_email,omitempty"`
	MailboxEmail      string   `json:"mailbox_email,omitempty"`
	SeriesAllDay      *bool    `json:"series_all_day,omitempty"`
	SeriesTimeZone    string   `json:"series_time_zone,omitempty"`
	Superseded        bool     `json:"superseded,omitzero"`
}

func buildMetadata(g calevent.GraphEvent, calendarID, email, zone string) eventMetadata {
	m := eventMetadata{
		Status: "confirmed", AllDay: g.IsAllDay, TimeZone: calevent.GraphZone(g.OriginalStartTimeZone),
		RecurringEventID: g.SeriesMasterID, OriginalStartTime: g.OriginalStart, ICalUID: g.ICalUID,
		HTMLLink: g.WebLink, Transparency: "opaque", Visibility: "default",
		OrganizerEmail: calevent.NormalizeEmail(g.Organizer.EmailAddress.Address), CalendarID: calendarID, AccountEmail: email,
	}
	if g.IsCancelled {
		m.Status = "cancelled"
	}
	if g.ShowAs == "free" {
		m.Transparency = "transparent"
	}
	if g.Sensitivity == "private" || g.Sensitivity == "confidential" {
		m.Visibility = "private"
	}
	if g.OnlineMeeting != nil {
		m.JoinURL = g.OnlineMeeting.JoinURL
	}
	if g.IsAllDay {
		m.Start = calevent.GraphAllDayDate(g.Start, g.OriginalStartTimeZone, zone)
		m.End = calevent.GraphAllDayDate(g.End, g.OriginalStartTimeZone, zone)
	} else {
		m.Start, m.End = rfc3339(g.Start), rfc3339(g.End)
	}
	return m
}

func rfc3339(v struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}) string {
	t, err := calevent.GraphDateTime(v)
	if err != nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// serializeBody matches the Google body: title, time, location, description
// and attendee names. Addresses reach full-text search through ToAddrs only.
func serializeBody(g calevent.GraphEvent, start sql.NullTime, zone string) string {
	var b strings.Builder
	line := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			b.WriteString(s + "\n")
		}
	}
	line(g.Subject)
	if start.Valid {
		if g.IsAllDay {
			line("When: " + calevent.GraphAllDayDate(g.Start, g.OriginalStartTimeZone, zone) + " (all day)")
		} else if end, err := calevent.GraphDateTime(g.End); err == nil {
			// Show the clock of the event's zone, as the Google body does.
			// Only a missing or unknown zone falls back to the mailbox's.
			loc, err := time.LoadLocation(calevent.GraphZone(g.OriginalStartTimeZone))
			if err != nil || g.OriginalStartTimeZone == "" {
				if loc, err = calevent.GraphLocation("", zone); err != nil {
					loc = time.UTC
				}
			}
			line("When: " + start.Time.In(loc).Format("2006-01-02 15:04") + " - " + end.In(loc).Format("2006-01-02 15:04"))
		}
	}
	if g.Location.DisplayName != "" {
		line("Location: " + g.Location.DisplayName)
	}
	line(g.Body.Content)
	var names []string
	for _, a := range g.Attendees {
		if a.EmailAddress.Name != "" && a.Type != "resource" {
			names = append(names, a.EmailAddress.Name)
		}
	}
	if len(names) > 0 {
		line("Attendees: " + strings.Join(names, ", "))
	}
	return strings.TrimSpace(b.String())
}
