package calsync

// End-to-end integration test driving the REAL gcal.Client over a loopback
// httptest.Server (not the in-memory mock) against byte-realistic Google
// Calendar API v3 JSON, through the real calsync pipeline into a real store.
// It exercises everything except the literal TCP-to-Google and OAuth token
// (the parts that require interactive consent): HTTP client unmarshal,
// pagination, the rate limiter, persist, metadata, FTS, recipients, raw bytes,
// and the incremental create/cancel cycle.
//
// Fixtures follow the documented wire shapes (verified against
// developers.google.com/workspace/calendar/api/v3/reference/events and
// .../guides/sync): a cancelled exception instance carries only id +
// recurringEventId + originalStartTime; a deleted standalone carries only id +
// status=cancelled; nextSyncToken appears only on the final page; incremental
// responses always include cancellations and never carry timeMin/timeMax.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type calFakeServer struct {
	calendarListJSON string
	fullPages        map[string]string // keyed by incoming pageToken ("" = first page)
	incPages         map[string]string // keyed by incoming syncToken
}

func (cs *calFakeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/users/me/calendarList":
			_, _ = w.Write([]byte(cs.calendarListJSON))
		case strings.HasSuffix(r.URL.Path, "/events"):
			q := r.URL.Query()
			assert.Equal(t, "false", q.Get("singleEvents"), "must request masters")
			assert.Equal(t, "true", q.Get("showDeleted"), "must include cancellations")
			if tok := q.Get("syncToken"); tok != "" {
				assert.Empty(t, q.Get("timeMin"), "timeMin must not accompany syncToken")
				assert.Empty(t, q.Get("timeMax"), "timeMax must not accompany syncToken")
				body, ok := cs.incPages[tok]
				if !assert.Truef(t, ok, "no incremental page seeded for syncToken %q", tok) {
					return
				}
				_, _ = w.Write([]byte(body))
				return
			}
			body, ok := cs.fullPages[q.Get("pageToken")]
			if !assert.Truef(t, ok, "no full page seeded for pageToken %q", q.Get("pageToken")) {
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			assert.Failf(t, "unexpected request path", "%q", r.URL.Path)
		}
	}))
}

func realCalClient(t *testing.T, srv *httptest.Server) gcal.API {
	t.Helper()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})
	return gcal.NewClient(ts, gcal.WithBaseURL(srv.URL), gcal.WithHTTPClient(srv.Client()))
}

type persistedEvent struct {
	id       int64
	mtype    string
	subject  string
	sentAt   sql.NullTime
	isFromMe bool
	meta     map[string]any
	deleted  bool
	body     string
	to       []string
	rawFmt   string
}

func loadEvent(t *testing.T, st *store.Store, sourceID int64, smid string) (persistedEvent, bool) {
	t.Helper()
	var p persistedEvent
	var subj, metaStr sql.NullString
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(st.Rebind(`
		SELECT id, message_type, subject, sent_at, is_from_me, metadata, deleted_from_source_at
		FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, smid).
		Scan(&p.id, &p.mtype, &subj, &p.sentAt, &p.isFromMe, &metaStr, &deletedAt)
	if err == sql.ErrNoRows {
		return persistedEvent{}, false
	}
	require.NoError(t, err)
	p.subject = subj.String
	p.deleted = deletedAt.Valid
	if metaStr.Valid {
		_ = json.Unmarshal([]byte(metaStr.String), &p.meta)
	}
	var body sql.NullString
	_ = st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), p.id).Scan(&body)
	p.body = body.String
	_ = st.DB().QueryRow(st.Rebind(`SELECT raw_format FROM message_raw WHERE message_id = ?`), p.id).Scan(&p.rawFmt)
	p.to = recipientEmails(t, st, p.id, "to")
	return p, true
}

func TestRealClient_DozenEvents_FullThenIncremental(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const account = "alice@example.com"

	cs := &calFakeServer{
		calendarListJSON: `{"items":[
			{"id":"alice@example.com","summary":"Alice","timeZone":"America/Los_Angeles","accessRole":"owner","primary":true},
			{"id":"holidays@group.v.calendar.google.com","summary":"US Holidays","accessRole":"reader"}
		]}`,
		fullPages: map[string]string{},
		incPages:  map[string]string{},
	}

	cs.fullPages[""] = `{"items":[
		{"id":"evt_timed","status":"confirmed","summary":"Sprint Standup","location":"Room 4",
		 "htmlLink":"https://www.google.com/calendar/event?eid=abc","hangoutLink":"https://meet.google.com/abc-defg-hij",
		 "iCalUID":"evt_timed@google.com","sequence":2,"transparency":"opaque","visibility":"default",
		 "organizer":{"email":"alice@example.com","displayName":"Alice Account","self":true},
		 "start":{"dateTime":"2024-05-01T09:00:00-07:00","timeZone":"America/Los_Angeles"},
		 "end":{"dateTime":"2024-05-01T09:30:00-07:00","timeZone":"America/Los_Angeles"},
		 "attendees":[
			{"email":"alice@example.com","displayName":"Alice Account","responseStatus":"accepted","organizer":true,"self":true},
			{"email":"bob@example.com","displayName":"Bob Builder","responseStatus":"accepted"},
			{"email":"carol@example.com","displayName":"Carol Singer","responseStatus":"tentative"}
		 ]},
		{"id":"evt_allday","status":"confirmed","summary":"Company Offsite",
		 "organizer":{"email":"alice@example.com","self":true},
		 "start":{"date":"2024-06-10"},"end":{"date":"2024-06-13"}},
		{"id":"evt_tentative","status":"tentative","summary":"Maybe Lunch",
		 "organizer":{"email":"dave@example.com","displayName":"Dave External"},
		 "start":{"dateTime":"2024-05-03T12:00:00Z"},"end":{"dateTime":"2024-05-03T13:00:00Z"},
		 "attendees":[{"email":"alice@example.com","self":true,"responseStatus":"needsAction"}]},
		{"id":"evt_unicode","status":"confirmed","summary":"Café résumé 会議 — plan",
		 "description":"Discuss the Ωmega rollout.","location":"Zürich",
		 "organizer":{"email":"alice@example.com","self":true},
		 "start":{"dateTime":"2024-05-04T15:00:00Z"},"end":{"dateTime":"2024-05-04T16:00:00Z"}}
	],"nextPageToken":"PAGE2"}`

	cs.fullPages["PAGE2"] = `{"items":[
		{"id":"evt_master","status":"confirmed","summary":"Weekly 1:1","location":"Office",
		 "organizer":{"email":"alice@example.com","self":true},
		 "start":{"dateTime":"2024-05-06T10:00:00Z"},"end":{"dateTime":"2024-05-06T10:30:00Z"},
		 "recurrence":["RRULE:FREQ=WEEKLY;BYDAY=MO","EXDATE;TZID=UTC:20240520T100000Z"],
		 "attendees":[{"email":"bob@example.com","displayName":"Bob Builder","responseStatus":"accepted"}]},
		{"id":"evt_master_20240513T100000Z","status":"confirmed","summary":"Weekly 1:1 (moved)",
		 "organizer":{"email":"alice@example.com","self":true},
		 "recurringEventId":"evt_master",
		 "originalStartTime":{"dateTime":"2024-05-13T10:00:00Z"},
		 "start":{"dateTime":"2024-05-13T14:00:00Z"},"end":{"dateTime":"2024-05-13T14:30:00Z"}},
		{"id":"evt_master_20240527T100000Z","status":"cancelled","recurringEventId":"evt_master",
		 "originalStartTime":{"dateTime":"2024-05-27T10:00:00Z"}},
		{"id":"evt_solo","status":"confirmed","summary":"Focus block",
		 "organizer":{"email":"alice@example.com","self":true},
		 "start":{"dateTime":"2024-05-07T13:00:00Z"},"end":{"dateTime":"2024-05-07T15:00:00Z"}}
	],"nextSyncToken":"SYNC_AFTER_FULL"}`

	srv := cs.start(t)
	defer srv.Close()

	st := testutil.NewTestStore(t)
	// Scope to the primary calendar so the fake server's page keying is
	// unambiguous; the access-role filter itself is covered by a mock test.
	syncer := New(realCalClient(t, srv), st, Options{
		AccountEmail: account,
		Calendars:    []string{account},
	}).WithLogger(quietLogger())

	res, err := syncer.Full(context.Background())
	require.NoError(err)

	src, err := st.GetSourceByIdentifier(account + "/" + account)
	require.NoError(err)
	require.NotNil(src)
	assert.Equal("SYNC_AFTER_FULL", src.SyncCursor.String, "final sync token persisted as cursor")

	type expect struct {
		smid     string
		subject  string
		isFromMe bool
		allDay   any
		status   string
		toEmails []string
	}
	cases := []expect{
		// Google lists the organizer (Alice, self) among attendees too, so she is
		// faithfully stored as both 'from' and a 'to' recipient — the full invite
		// list is preserved rather than silently dropping self.
		{"evt_timed", "Sprint Standup", true, false, "confirmed", []string{"alice@example.com", "bob@example.com", "carol@example.com"}},
		{"evt_allday", "Company Offsite", true, true, "confirmed", nil},
		{"evt_tentative", "Maybe Lunch", false, false, "tentative", nil},
		{"evt_unicode", "Café résumé 会議 — plan", true, false, "confirmed", nil},
		{"evt_master", "Weekly 1:1", true, false, "confirmed", []string{"bob@example.com"}},
		{"evt_master|2024-05-13T10:00:00Z", "Weekly 1:1 (moved)", true, false, "confirmed", nil},
		{"evt_master|2024-05-27T10:00:00Z", "", false, false, "cancelled", nil},
		{"evt_solo", "Focus block", true, false, "confirmed", nil},
	}

	t.Logf("=== persisted calendar events (real client over loopback) ===")
	t.Logf("%-36s %-24s %-7s %-7s %-9s", "source_message_id", "subject", "from_me", "all_day", "status")
	for _, c := range cases {
		p, ok := loadEvent(t, st, src.ID, c.smid)
		require.Truef(ok, "event %s should be persisted", c.smid)
		t.Logf("%-36s %-24s %-7v %-7v %-9s", c.smid, truncateRunes(p.subject, 24), p.isFromMe, p.meta["all_day"], p.meta["status"])

		assert.Equalf(gcal.MessageTypeCalendarEvent, p.mtype, "%s message_type", c.smid)
		if c.subject != "" {
			assert.Equalf(c.subject, p.subject, "%s subject", c.smid)
		}
		assert.Equalf(c.isFromMe, p.isFromMe, "%s is_from_me", c.smid)
		assert.Equalf(c.status, p.meta["status"], "%s metadata.status", c.smid)
		assert.Equalf(c.allDay, p.meta["all_day"], "%s metadata.all_day", c.smid)
		assert.Equalf("gcal_json", p.rawFmt, "%s raw_format", c.smid)
		assert.Falsef(p.deleted, "%s must not be soft-deleted", c.smid)
		if c.toEmails != nil {
			assert.Equalf(c.toEmails, p.to, "%s to-recipients", c.smid)
			for _, e := range c.toEmails {
				assert.NotContainsf(p.body, e, "%s body must not contain raw attendee email", c.smid)
			}
		}
	}

	// Cancelled occurrence retained, sorts at its ORIGINAL start time.
	cancelled, ok := loadEvent(t, st, src.ID, "evt_master|2024-05-27T10:00:00Z")
	require.True(ok)
	require.True(cancelled.sentAt.Valid)
	assert.Equal(time.Date(2024, 5, 27, 10, 0, 0, 0, time.UTC), cancelled.sentAt.Time.UTC())

	// All-day start normalized to midnight UTC.
	allday, _ := loadEvent(t, st, src.ID, "evt_allday")
	require.True(allday.sentAt.Valid)
	assert.Equal(time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC), allday.sentAt.Time.UTC())

	// Master keeps RRULE + EXDATE.
	master, _ := loadEvent(t, st, src.ID, "evt_master")
	rec, _ := master.meta["recurrence"].([]any)
	assert.Len(rec, 2, "RRULE + EXDATE preserved in metadata")
	assert.Equal("https://meet.google.com/abc-defg-hij", func() any {
		ev, _ := loadEvent(t, st, src.ID, "evt_timed")
		return ev.meta["hangout_link"]
	}(), "hangout link preserved in metadata")

	assert.Equal(7, res.EventsAdded)
	assert.Equal(1, res.EventsCancelled)

	// ---- INCREMENTAL: create a new event + cancel an existing standalone ----
	cs.incPages["SYNC_AFTER_FULL"] = `{"items":[
		{"id":"evt_new","status":"confirmed","summary":"Newly created",
		 "organizer":{"email":"alice@example.com","self":true},
		 "start":{"dateTime":"2024-05-10T08:00:00Z"},"end":{"dateTime":"2024-05-10T08:30:00Z"},
		 "attendees":[{"email":"erin@example.com","displayName":"Erin Eve","responseStatus":"accepted"}]},
		{"id":"evt_solo","status":"cancelled"}
	],"nextSyncToken":"SYNC_AFTER_INC"}`

	res2, err := syncer.Incremental(context.Background())
	require.NoError(err)
	assert.Equal(1, res2.EventsAdded)
	assert.Equal(1, res2.EventsCancelled)

	newEv, ok := loadEvent(t, st, src.ID, "evt_new")
	require.True(ok, "newly created event should be ingested by incremental")
	assert.Equal("Newly created", newEv.subject)
	assert.Equal([]string{"erin@example.com"}, newEv.to)

	solo, ok := loadEvent(t, st, src.ID, "evt_solo")
	require.True(ok, "cancelled event must be retained, not deleted")
	assert.False(solo.deleted, "deleted_from_source_at must stay NULL")
	assert.Equal("Focus block", solo.subject, "original subject preserved despite empty cancellation delta")
	assert.Equal("cancelled", solo.meta["status"])

	src2, _ := st.GetSourceByIdentifier(account + "/" + account)
	assert.Equal("SYNC_AFTER_INC", src2.SyncCursor.String, "cursor advanced after incremental")

	// A subsequent empty incremental is a clean no-op.
	cs.incPages["SYNC_AFTER_INC"] = `{"items":[],"nextSyncToken":"SYNC_AFTER_INC"}`
	_, err = syncer.Incremental(context.Background())
	require.NoError(err)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
