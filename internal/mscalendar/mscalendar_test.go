package mscalendar

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	single = `{"id":"single-1","type":"singleInstance","subject":"Review",
 "start":{"dateTime":"2027-01-06T15:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-06T16:00:00.0000000","timeZone":"UTC"},
 "organizer":{"emailAddress":{"name":"Me","address":"me@example.com"}},
 "responseStatus":{"response":"organizer"},
 "attendees":[
  {"type":"required","status":{"response":"accepted"},"emailAddress":{"name":"Pat","address":"pat@example.com"}},
  {"type":"resource","status":{"response":"accepted"},"emailAddress":{"name":"Room 1","address":"room1@example.com"}}]}`
	seriesHead = `{"id":"series-1","type":"seriesMaster","subject":"Standup"}`
	seriesBody = `"id":"series-1","type":"seriesMaster","subject":"Standup",
 "start":{"dateTime":"2027-01-05T14:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-05T14:30:00.0000000","timeZone":"UTC"},
 "originalStartTimeZone":"Eastern Standard Time",
 "organizer":{"emailAddress":{"name":"Me","address":"me@example.com"}},
 "recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["tuesday"],"firstDayOfWeek":"sunday"},
  "range":{"type":"numbered","numberOfOccurrences":4,"recurrenceTimeZone":"Eastern Standard Time"}},
 "cancelledOccurrences":["OID.AAMkSeries=.2027-01-12"]`
	exception = `{"id":"exc-1","type":"exception","seriesMasterId":"series-1","subject":"Standup (moved)",
 "originalStart":"2027-01-19T14:00:00Z","originalStartTimeZone":"Eastern Standard Time",
 "start":{"dateTime":"2027-01-19T16:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-19T16:30:00.0000000","timeZone":"UTC"}}`
)

// graphState changes what fakeGraph returns between syncs.
type graphState struct {
	dropSingle bool // the single event was deleted in Outlook
	badEvent   bool // the listing carries an event that cannot be stored
	noCalendar bool // the owned calendar was deleted in Outlook
	noOwner    bool // Graph lists calendars without a default owner
	undoEdit   bool // the moved occurrence was reset to the series
	pageEdits  bool // Graph pages the expanded edits
	dropSeries bool // the series was deleted in Outlook
	cancelEdit bool // the moved occurrence was cancelled
	customEdit bool // the moved occurrence has a custom Outlook zone
	second     bool // the mailbox owns a second, non-default calendar
	zone       string
	extra      string // more events in the owned calendar's first page
}

// fakeGraph serves one owned calendar and one shared calendar. Like Graph, it
// pages the event list and includes exceptions only when $expand asks for them.
func fakeGraph(t *testing.T, state *graphState) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/me/mailboxSettings/timeZone", func(w http.ResponseWriter, _ *http.Request) {
		zone := cmp.Or(state.zone, "Eastern Standard Time")
		_, _ = io.WriteString(w, `{"value":"`+zone+`"}`)
	})
	mux.HandleFunc("/me/calendars", func(w http.ResponseWriter, _ *http.Request) {
		if state.noOwner {
			_, _ = io.WriteString(w, `{"value":[{"id":"cal-own","name":"Calendar","owner":{}}]}`)
			return
		}
		if state.noCalendar {
			_, _ = io.WriteString(w, `{"value":[
 {"id":"cal-other","name":"Other","isDefaultCalendar":true,"owner":{"address":"Me@Example.com"}}]}`)
			return
		}
		second := ""
		if state.second {
			second = `{"id":"cal-other","name":"Other","owner":{"address":"Me@Example.com"}},`
		}
		_, _ = io.WriteString(w, `{"value":[`+second+`
 {"id":"cal-own","name":"Calendar","isDefaultCalendar":true,"owner":{"address":"Me@Example.com"}},
 {"id":"cal-shared","name":"Boss","owner":{"address":"boss@example.com"}}]}`)
	})
	var srv *httptest.Server
	mux.HandleFunc("/me/calendars/cal-own/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			if state.dropSeries {
				_, _ = io.WriteString(w, `{"value":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"value":[`+seriesHead+`]}`)
			return
		}
		first := single
		if state.dropSingle {
			first = `{"id":"single-2","type":"singleInstance","subject":"Other",
 "start":{"dateTime":"2027-03-01T15:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-03-01T16:00:00.0000000","timeZone":"UTC"}}`
		}
		if state.badEvent {
			first += `,{"type":"singleInstance"}`
		}
		first += state.extra
		_, _ = fmt.Fprintf(w, `{"value":[%s],"@odata.nextLink":"%s/me/calendars/cal-own/events?page=2"}`, first, srv.URL)
	})
	mux.HandleFunc("/me/calendars/cal-other/events", func(w http.ResponseWriter, _ *http.Request) {
		if state.second {
			_, _ = io.WriteString(w, `{"value":[{"id":"other-1","type":"singleInstance","subject":"Dentist",
 "start":{"dateTime":"2027-01-08T15:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-08T16:00:00.0000000","timeZone":"UTC"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"value":[]}`)
	})
	mux.HandleFunc("/me/calendars/cal-shared/events", func(http.ResponseWriter, *http.Request) {
		assert.Fail(t, "a shared calendar must not be read")
	})
	mux.HandleFunc("/me/events/series-1", func(w http.ResponseWriter, r *http.Request) {
		exceptions := ""
		switch {
		// Graph v1.0 returns the edits only when $select and $expand both name them.
		case !strings.HasPrefix(r.URL.Query().Get("$expand"), "exceptionOccurrences") ||
			!strings.Contains(","+r.URL.Query().Get("$select")+",", ",exceptionOccurrences,") || state.undoEdit:
		case state.cancelEdit:
			exceptions = `,"exceptionOccurrences":[]`
		case state.pageEdits:
			exceptions = `,"exceptionOccurrences":[],"exceptionOccurrences@odata.nextLink":"` + srv.URL + `/me/events/series-1/exceptionOccurrences"`
		case state.customEdit:
			exceptions = `,"exceptionOccurrences":[` + strings.Replace(exception, `"originalStartTimeZone":"Eastern Standard Time"`, `"originalStartTimeZone":"tzone://Microsoft/Custom"`, 1) + `]`
		default:
			exceptions = `,"exceptionOccurrences":[` + exception + `]`
		}
		body := seriesBody
		if state.cancelEdit {
			body = strings.Replace(body, `.2027-01-12"`, `.2027-01-12","OID.AAMkSeries=.2027-01-19"`, 1)
		}
		_, _ = io.WriteString(w, "{"+body+exceptions+"}")
	})
	mux.HandleFunc("/me/events/series-1/exceptionOccurrences", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[`+exception+`]}`)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSyncArchivesSeriesWithEditsAndCancellations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	srv := fakeGraph(t, &graphState{})
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "token", nil }, 100)
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	sum, err := Sync(ctx, st, c, "me@example.com", log)
	requirements.NoError(err)
	assertions.Equal(1, sum.Calendars)
	assertions.Equal(3, sum.Events)
	assertions.Equal(0, sum.Errors)

	// A second run changes nothing.
	_, err = Sync(ctx, st, c, "me@example.com", log)
	requirements.NoError(err)

	page, err := st.ListCalendarOccurrencesContext(ctx,
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	assertions.Empty(page.Unexpandable)
	var got []string
	for _, o := range page.Occurrences {
		got = append(got, o.Start.UTC().Format("01-02 15:04")+" "+o.Subject)
	}
	assertions.Equal([]string{
		"01-05 14:00 Standup",
		"01-06 15:00 Review",
		"01-19 16:00 Standup (moved)",
		"01-26 14:00 Standup",
	}, got)

	sources, err := st.GetSourcesByTypeAndAccount(SourceType, "me@example.com")
	requirements.NoError(err)
	requirements.Len(sources, 1)
	assertions.Equal("me@example.com/cal-own", sources[0].Identifier)
	assertions.True(sources[0].LastSyncAt.Valid, "a completed sync records when it finished")
	run, err := st.GetLastSuccessfulSync(sources[0].ID)
	requirements.NoError(err)
	requirements.NotNil(run)
	assertions.Equal(int64(3), run.MessagesProcessed)

	var metadata string
	requirements.NoError(st.DB().QueryRow(`SELECT metadata FROM messages WHERE source_message_id = 'series-1'`).Scan(&metadata))
	assertions.Contains(metadata, `"recurrence":["RRULE:FREQ=WEEKLY;INTERVAL=1;BYDAY=TU;WKST=SU;COUNT=4","EXDATE:20270112T090000"]`)

	var rooms int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address = 'room1@example.com'`).Scan(&rooms))
	assertions.Equal(0, rooms, "a room is not a participant")
}

// testSync runs Sync against fakeGraph with a store that persists across runs.
type testSync struct {
	t     *testing.T
	st    *store.Store
	state *graphState
	c     *Client
}

func newTestSync(t *testing.T) *testSync {
	t.Helper()
	state := &graphState{}
	srv := fakeGraph(t, state)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "token", nil }, 100)
	return &testSync{t: t, st: testutil.NewTestStore(t), state: state, c: c}
}

func (ts *testSync) run(email string) (*Summary, error) {
	return Sync(context.Background(), ts.st, ts.c, email, slog.New(slog.DiscardHandler))
}

// on lists the occurrences on one January 2027 day.
func (ts *testSync) on(day int) []string {
	ts.t.Helper()
	page, err := ts.st.ListCalendarOccurrencesContext(context.Background(),
		time.Date(2027, 1, day, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, day+1, 0, 0, 0, 0, time.UTC))
	require.NoError(ts.t, err)
	var out []string
	for _, o := range page.Occurrences {
		out = append(out, fmt.Sprintf("%s %s deleted=%v", o.Start.UTC().Format("01-02 15:04"), o.Subject, o.DeletedFromSource))
	}
	return out
}

func TestSyncMarksDeletedEventsAndClearsReturnedOnes(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ts := newTestSync(t)

	_, err := ts.run("me@example.com")
	requirements.NoError(err)
	assertions.Equal([]string{"01-06 15:00 Review deleted=false"}, ts.on(6))

	ts.state.dropSingle = true
	_, err = ts.run("me@example.com")
	requirements.NoError(err)
	assertions.Equal([]string{"01-06 15:00 Review deleted=true"}, ts.on(6), "the archive keeps a deleted event")

	ts.state.dropSingle = false
	_, err = ts.run("me@example.com")
	requirements.NoError(err)
	assertions.Equal([]string{"01-06 15:00 Review deleted=false"}, ts.on(6))
}

func TestSyncFailsWhenAnEventCannotBeStored(t *testing.T) {
	ts := newTestSync(t)
	_, err := ts.run("me@example.com")
	require.NoError(t, err)

	ts.state.badEvent, ts.state.dropSingle = true, true
	_, err = ts.run("me@example.com")
	require.ErrorContains(t, err, "1 events could not be stored")
	assert.Equal(t, []string{"01-06 15:00 Review deleted=false"}, ts.on(6), "a failed run marks nothing deleted")
}

func TestSyncRegistersCalendarsForAnAlias(t *testing.T) {
	ts := newTestSync(t)
	sum, err := ts.run("alias@example.com")
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Calendars, "ownership follows the default calendar, not the alias")
}

func TestSyncMarksEventsOfADeletedCalendar(t *testing.T) {
	ts := newTestSync(t)
	_, err := ts.run("me@example.com")
	require.NoError(t, err)

	ts.state.noCalendar = true
	_, err = ts.run("me@example.com")
	require.NoError(t, err)
	assert.Equal(t, []string{"01-06 15:00 Review deleted=true"}, ts.on(6))
}

func TestSyncWithoutAnOwnerKeepsEvents(t *testing.T) {
	ts := newTestSync(t)
	_, err := ts.run("me@example.com")
	require.NoError(t, err)

	ts.state.noOwner = true
	_, err = ts.run("me@example.com")
	require.ErrorContains(t, err, "no default calendar")
	assert.Equal(t, []string{"01-06 15:00 Review deleted=false"}, ts.on(6))
}

func TestSyncSkipsAMergedCalendar(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	state := &graphState{}
	srv := fakeGraph(t, state)
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "token", nil }, 100)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, err := Sync(ctx, st, c, "me@example.com", log)
	requirements.NoError(err)

	from, err := st.GetOrCreateSource(SourceType, "me@example.com/cal-own")
	requirements.NoError(err)
	into, err := st.GetOrCreateSource(SourceType, "me@example.com/cal-archive")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)

	sum, err := Sync(ctx, st, c, "me@example.com", log)
	requirements.NoError(err)
	assert.Equal(t, 0, sum.Calendars, "a merged calendar is not synced again")
}

func TestSyncKeepsAnUndoneEditOutOfTheSeries(t *testing.T) {
	requirements := require.New(t)
	ts := newTestSync(t)
	_, err := ts.run("me@example.com")
	requirements.NoError(err)

	ts.state.undoEdit = true
	for range 2 {
		_, err = ts.run("me@example.com")
		requirements.NoError(err)
		assert.Equal(t, []string{"01-19 14:00 Standup deleted=false"}, ts.on(19))
	}
	var deleted bool
	requirements.NoError(ts.st.DB().QueryRow(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_message_id = 'exc-1'`).Scan(&deleted))
	assert.False(t, deleted, "an undone edit is not a deletion")
}

func TestSyncMarksTheEditOfACancelledOccurrenceDeleted(t *testing.T) {
	requirements := require.New(t)
	ts := newTestSync(t)
	_, err := ts.run("me@example.com")
	requirements.NoError(err)

	ts.state.cancelEdit = true
	_, err = ts.run("me@example.com")
	requirements.NoError(err)
	var deleted bool
	requirements.NoError(ts.st.DB().QueryRow(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_message_id = 'exc-1'`).Scan(&deleted))
	assert.True(t, deleted, "a cancelled edit is deleted, not undone")
}

func TestSyncReadsPagedEdits(t *testing.T) {
	ts := newTestSync(t)
	ts.state.pageEdits = true
	_, err := ts.run("me@example.com")
	require.NoError(t, err)
	assert.Equal(t, []string{"01-19 16:00 Standup (moved) deleted=false"}, ts.on(19))
}

func TestSyncKeepsASeriesWithACustomZoneEdit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ts := newTestSync(t)
	ts.state.customEdit = true
	_, err := ts.run("me@example.com")
	requirements.NoError(err)
	page, err := ts.st.ListCalendarOccurrencesContext(context.Background(),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	// The edit carries its own instants, so its zone is not needed.
	assertions.Empty(page.Unexpandable)
	var got []string
	for _, o := range page.Occurrences {
		got = append(got, o.Start.UTC().Format("01-02 15:04")+" "+o.Subject)
	}
	assertions.Equal([]string{"01-05 14:00 Standup", "01-06 15:00 Review", "01-19 16:00 Standup (moved)", "01-26 14:00 Standup"}, got)
}

func TestSyncRestoresASeriesWithoutItsFormerEdit(t *testing.T) {
	requirements := require.New(t)
	ts := newTestSync(t)
	ts.state.dropSeries = true
	_, err := ts.run("me@example.com")
	requirements.NoError(err)
	ts.state.dropSeries = false
	_, err = ts.run("me@example.com")
	requirements.NoError(err)

	ts.state.dropSeries = true
	_, err = ts.run("me@example.com")
	requirements.NoError(err)
	ts.state.dropSeries, ts.state.undoEdit = false, true
	_, err = ts.run("me@example.com")
	requirements.NoError(err)
	assert.Equal(t, []string{"01-19 14:00 Standup deleted=false"}, ts.on(19))
}

func TestSyncStoresConvertedAllDayDatesInTheMailboxZone(t *testing.T) {
	requirements := require.New(t)
	ts := newTestSync(t)
	ts.state.zone = "W. Europe Standard Time"
	// Berlin midnight of 1 March, converted to UTC.
	ts.state.extra = `,{"id":"day-1","type":"singleInstance","subject":"Holiday","isAllDay":true,
 "originalStartTimeZone":"UTC",
 "start":{"dateTime":"2027-02-28T23:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-03-01T23:00:00.0000000","timeZone":"UTC"}}`
	_, err := ts.run("me@example.com")
	requirements.NoError(err)
	assertions := assert.New(t)
	var meta, body string
	var sentAt time.Time
	requirements.NoError(ts.st.DB().QueryRow(`SELECT m.metadata, b.body_text, m.sent_at FROM messages m
		JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = 'day-1'`).Scan(&meta, &body, &sentAt))
	assertions.Contains(meta, `"start":"2027-03-01"`)
	assertions.Contains(body, "When: 2027-03-01 (all day)")
	assertions.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), sentAt.UTC(), "a date search for 1 March finds it")
}

func TestSyncWritesTheEventClockIntoTheBody(t *testing.T) {
	requirements := require.New(t)
	ts := newTestSync(t)
	ts.state.extra = `,{"id":"utc-1","type":"singleInstance","subject":"Call","originalStartTimeZone":"UTC",
 "start":{"dateTime":"2027-01-06T15:00:00.0000000","timeZone":"UTC"},
 "end":{"dateTime":"2027-01-06T16:00:00.0000000","timeZone":"UTC"}}`
	_, err := ts.run("me@example.com")
	requirements.NoError(err)
	var body string
	requirements.NoError(ts.st.DB().QueryRow(`SELECT b.body_text FROM messages m
		JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = 'series-1'`).Scan(&body))
	assert.Contains(t, body, "When: 2027-01-05 09:00 - 2027-01-05 09:30", "the series runs in Eastern time")
	requirements.NoError(ts.st.DB().QueryRow(`SELECT b.body_text FROM messages m
		JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = 'utc-1'`).Scan(&body))
	assert.Contains(t, body, "When: 2027-01-06 15:00 - 2027-01-06 16:00", "an event created in UTC keeps the UTC clock")
}

func TestSyncAttributesEveryOwnedCalendarToTheAccount(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	srv := fakeGraph(t, &graphState{second: true})
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "token", nil }, 100)
	_, err := Sync(context.Background(), st, c, "me@example.com", slog.New(slog.DiscardHandler))
	requirements.NoError(err)
	var address string
	for _, id := range []string{"single-1", "other-1"} {
		requirements.NoError(st.DB().QueryRow(`SELECT COALESCE(account_address, '') FROM messages WHERE source_message_id = ?`, id).Scan(&address))
		assert.Equal(t, "me@example.com", address, id)
	}
}
