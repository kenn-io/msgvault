package caldav

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncFixture struct {
	sync.Mutex

	raw                     map[string]string
	reported                map[string]bool
	deleted                 []string
	token                   string
	failFetch, invalidToken bool
	forceSnapshot           bool
	incompleteSnapshot      bool
}

func archiveFixture(t *testing.T) (*Client, *syncFixture) {
	t.Helper()
	assertions := assert.New(t)
	f := &syncFixture{raw: map[string]string{"a.ics": calendarBody}, token: "t1"}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.Lock()
		defer f.Unlock()
		if body, ok := discoveryResponse(r); ok {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
			return
		}
		req, err := io.ReadAll(r.Body)
		if !assertions.NoError(err) {
			http.Error(w, "fixture read failed", http.StatusBadRequest)
			return
		}
		if strings.Contains(string(req), "sync-collection") {
			if f.forceSnapshot {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if f.invalidToken && strings.Contains(string(req), ">t1<") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`)
				return
			}
			var responses strings.Builder
			for name := range f.raw {
				if f.reported != nil && !f.reported[name] {
					continue
				}
				responses.WriteString(prop("/home/cal/"+name, `<D:getetag>"updated"</D:getetag>`))
			}
			for _, name := range f.deleted {
				responses.WriteString(`<D:response><D:href>/home/cal/` + name + `</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`)
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(responses.String()+`<D:sync-token>`+f.token+`</D:sync-token>`))
			return
		}
		if r.Method == "PROPFIND" && f.forceSnapshot {
			var responses strings.Builder
			if !f.incompleteSnapshot {
				responses.WriteString(prop("/home/cal/", `<D:getetag>"collection"</D:getetag><D:resourcetype><D:collection/></D:resourcetype>`))
				for name := range f.raw {
					responses.WriteString(prop("/home/cal/"+name, `<D:getetag>"updated"</D:getetag><D:resourcetype/>`))
				}
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(responses.String()))
			return
		}
		if f.failFetch {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var responses strings.Builder
		for name, raw := range f.raw {
			if strings.Contains(string(req), "/home/cal/"+name) {
				responses.WriteString(data("/home/cal/"+name, raw))
			}
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, multi(responses.String()))
	})
	return c, f
}
func syncOpts() Options {
	return Options{Name: "personal", BaseURL: "http://203.0.113.1/dav", Username: "owner@example.org", TimeZone: "UTC"}
}
func sourceToken(t *testing.T, st *store.Store) string {
	t.Helper()
	requirements := require.New(t)
	var token string
	requirements.NoError(st.DB().QueryRow(`SELECT COALESCE(sync_cursor,'') FROM sources WHERE source_type='caldav'`).Scan(&token))
	return token
}
func count(t *testing.T, st *store.Store, sql string) int {
	t.Helper()
	requirements := require.New(t)
	var n int
	requirements.NoError(st.DB().QueryRow(sql).Scan(&n))
	return n
}
func compound() string {
	return strings.Replace(calendarBody, "END:VEVENT\r\nEND:VCALENDAR", "RRULE:FREQ=WEEKLY;COUNT=3\r\nATTENDEE;CUTYPE=ROOM:mailto:room@example.org\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:example@example.org\r\nRECURRENCE-ID:20270321T130000Z\r\nDTSTART:20270321T150000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
}
func TestSyncCompoundUpdatesDeletesAndRestores(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = compound()
	result, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, result.Events)
	assertions.Equal("t1", sourceToken(t, st))
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM calendar_events WHERE provider='caldav'`))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_event_attendees WHERE role='room'`))
	assertions.Zero(count(t, st, `SELECT COUNT(*) FROM participants WHERE email_address='room@example.org'`))
	var id int64
	var meta, namespace string
	requirements.NoError(st.DB().QueryRow(`SELECT m.id,m.metadata,e.namespace FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind='series'`).Scan(&id, &meta, &namespace))
	raw, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Contains(string(raw), "RECURRENCE-ID")
	e, ok, err := calevent.Decode(calevent.CalDAVRawFormat, raw, []byte(meta), calevent.SourceContext{CalendarID: namespace, CalendarTimeZone: "UTC"})
	requirements.NoError(err)
	requirements.True(ok)
	assertions.Equal("series", e.Kind)
	f.Lock()
	f.raw["a.ics"] = calendarBody
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
	f.Lock()
	f.raw = map[string]string{}
	f.deleted = []string{"a.ics"}
	f.token = "t3"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
	raw, err = st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.NotEmpty(raw)
	f.Lock()
	f.raw["b.ics"] = calendarBody
	f.deleted = nil
	f.token = "t4"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages`))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages`))
}

func TestSyncRecomputesConversationStats(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(compound(), "DTSTART:20270321T150000Z\r\nDURATION:PT1H", "DTSTART:20270321T150000Z\r\nDURATION:PT1H\r\nSUMMARY:Later meeting", 1)
	st := testutil.NewTestStore(t)
	result, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	requirements.Equal(2, result.Events)

	var messageCount int
	var lastMessageAt, sentAt sql.NullTime
	var preview, snippet sql.NullString
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT c.message_count, c.last_message_at, c.last_message_preview, m.sent_at, m.snippet
		FROM conversations c JOIN messages m ON m.conversation_id = c.id
		WHERE m.message_type = 'calendar_event'
		ORDER BY m.sent_at DESC LIMIT 1`)).Scan(&messageCount, &lastMessageAt, &preview, &sentAt, &snippet))
	assertions.Equal(2, messageCount)
	requirements.True(lastMessageAt.Valid)
	requirements.True(sentAt.Valid)
	assertions.Equal(sentAt.Time, lastMessageAt.Time)
	requirements.True(preview.Valid)
	assertions.Equal(snippet.String, preview.String)
	assertions.Equal("Later meeting", preview.String)
}

func TestSyncRunCountersTrackAddedAndUpdatedEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = compound()

	result, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	assert.Equal(2, result.Events)
	var sourceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
	run, err := st.GetLatestSyncContext(t.Context(), sourceID, 0)
	require.NoError(err)
	assert.Equal(int64(2), run.MessagesProcessed)
	assert.Equal(int64(2), run.MessagesAdded)
	assert.Zero(run.MessagesUpdated)

	f.Lock()
	f.token = "t2"
	f.raw["a.ics"] = strings.Replace(compound(), "SUMMARY:Example meeting", "SUMMARY:Updated meeting", 1)
	f.Unlock()
	result, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	assert.Equal(2, result.Events)
	run, err = st.GetLatestSyncContext(t.Context(), sourceID, 0)
	require.NoError(err)
	assert.Equal(int64(2), run.MessagesProcessed)
	assert.Zero(run.MessagesAdded)
	assert.Equal(int64(2), run.MessagesUpdated)
}

func TestSyncRunCountersPreservePartialWritesOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	var sourceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))

	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_caldav_exception() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='exception' THEN RAISE EXCEPTION 'calendar write blocked'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_caldav_exception BEFORE INSERT ON calendar_events FOR EACH ROW EXECUTE FUNCTION reject_caldav_exception()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_caldav_exception BEFORE INSERT ON calendar_events WHEN NEW.kind='exception' BEGIN SELECT RAISE(ABORT,'calendar write blocked'); END`)
	}
	require.NoError(err)
	f.Lock()
	f.token = "t2"
	f.raw["a.ics"] = compound()
	f.Unlock()

	result, err := c.Sync(t.Context(), st, syncOpts())
	require.ErrorContains(err, "calendar write blocked")
	assert.Equal(1, result.Events)
	assert.Equal("t1", sourceToken(t, st))
	run, err := st.GetLatestSyncContext(t.Context(), sourceID, 0)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.Equal(int64(1), run.MessagesProcessed)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Zero(run.MessagesUpdated)
	assert.Equal(int64(1), run.ErrorsCount)
	assert.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_events WHERE kind = 'series'`))
	assert.Zero(count(t, st, `SELECT COUNT(*) FROM calendar_events WHERE kind = 'exception'`))
}

func TestSyncFailureKeepsCursorOnFetchOrDecodeFailure(t *testing.T) {
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	for _, failure := range []string{"fetch", "decode"} {
		t.Run(failure, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f.Lock()
			f.token = "t2"
			f.failFetch = failure == "fetch"
			if failure == "decode" {
				f.raw["a.ics"] = "invalid"
			}
			f.Unlock()
			_, err := c.Sync(t.Context(), st, syncOpts())
			requirements.Error(err)
			assertions.Equal("t1", sourceToken(t, st))
			f.Lock()
			f.failFetch = false
			f.raw["a.ics"] = calendarBody
			f.Unlock()
		})
	}
}
func TestSyncInvalidTokenTasksAndTimezone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	f.Lock()
	f.invalidToken = true
	f.token = "t2"
	f.raw = map[string]string{"task.ics": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"}
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal("t2", sourceToken(t, st))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
	opts := syncOpts()
	opts.TimeZone = "America/New_York"
	_, err = c.Sync(t.Context(), st, opts)
	requirements.ErrorContains(err, "time_zone")
}

func TestSyncRejectsLocalTimeZone(t *testing.T) {
	c, _ := archiveFixture(t)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.TimeZone = "Local"

	_, err := c.Sync(t.Context(), st, opts)
	require.ErrorContains(t, err, "Local")
	assert.Zero(t, count(t, st, `SELECT COUNT(*) FROM sources WHERE source_type = 'caldav'`))
}

func TestSyncWriteFailureKeepsCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_caldav_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'calendar write blocked'; END $$; CREATE TRIGGER reject_caldav_write BEFORE INSERT OR UPDATE ON calendar_events FOR EACH ROW EXECUTE FUNCTION reject_caldav_write()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_caldav_write BEFORE UPDATE ON calendar_events BEGIN SELECT RAISE(ABORT,'calendar write blocked'); END`)
	}
	requirements.NoError(err)
	f.Lock()
	f.token = "t2"
	f.raw["a.ics"] = strings.ReplaceAll(calendarBody, "20270314", "20270315")
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.Error(err)
	assertions.Equal("t1", sourceToken(t, st))
}

func TestSyncMoveAndDuplicateIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	f.Lock()
	f.raw = map[string]string{"b.ics": calendarBody}
	f.deleted = []string{"a.ics"}
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
	f.Lock()
	f.raw["c.ics"] = calendarBody
	f.deleted = nil
	f.token = "t3"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.ErrorContains(err, "duplicate")
	assertions.Equal("t2", sourceToken(t, st))
}
func TestSyncAmplificationFailsBeforeWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n")
	for i := range 100 {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:amplified-%d@example.org\r\nDTSTART:20270314T130000Z\r\nDESCRIPTION:%s\r\nEND:VEVENT\r\n", i, strings.Repeat("x", 8000))
	}
	b.WriteString("END:VCALENDAR\r\n")
	f.raw["a.ics"] = b.String()
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.ErrorContains(err, "amplification")
	assertions.Zero(count(t, st, `SELECT COUNT(*) FROM messages`))
	assertions.Empty(sourceToken(t, st))
}
func TestSyncFailedDeltaRetryTakesFullSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	f.Lock()
	f.raw["b.ics"] = strings.Replace(calendarBody, "UID:example@example.org", "UID:second@example.org", 1)
	f.token = "t2"
	f.Unlock()
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_caldav_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.sync_cursor='t2' THEN RAISE EXCEPTION 'completion blocked'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_caldav_completion BEFORE UPDATE ON sources FOR EACH ROW EXECUTE FUNCTION reject_caldav_completion()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_caldav_completion BEFORE UPDATE ON sources WHEN NEW.sync_cursor='t2' BEGIN SELECT RAISE(ABORT,'completion blocked'); END`)
	}
	require.NoError(err)
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.Error(err)
	assert.Equal("t1", sourceToken(t, st))
	assert.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.ical_uid='second@example.org' AND m.deleted_from_source_at IS NULL`))

	// The server's delta omits the resource created and deleted between retries.
	f.Lock()
	delete(f.raw, "b.ics")
	f.reported = map[string]bool{"a.ics": true}
	f.token = "t3"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	assert.Equal("t3", sourceToken(t, st))
	assert.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.ical_uid='second@example.org' AND m.deleted_from_source_at IS NOT NULL`))
}

func TestSyncPreservesLongDescriptionInBodyAndFTS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	description := strings.Repeat("beforeword ", 380) + "caldavtailneedle"
	folded := strings.Builder{}
	for start := 0; start < len(description); start += 70 {
		if start > 0 {
			folded.WriteString("\r\n ")
		}
		folded.WriteString(description[start:min(start+70, len(description))])
	}
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:long-description@example.org\r\nDTSTART:20270314T130000Z\r\nSUMMARY:Long description\r\nDESCRIPTION:" + folded.String() + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	c, f := archiveFixture(t)
	f.raw["a.ics"] = raw
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE message_type='calendar_event'`).Scan(&messageID))
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	assert.Contains(body, "caldavtailneedle")

	var projectedDescription string
	var truncated bool
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT description, description_truncated FROM calendar_events WHERE message_id = ?`), messageID).Scan(&projectedDescription, &truncated))
	assert.True(truncated)
	assert.LessOrEqual(len(projectedDescription), 4096)
	assert.NotContains(projectedDescription, "caldavtailneedle")

	matches, total, err := st.SearchMessagesContext(t.Context(), "caldavtailneedle", 0, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	require.Len(matches, 1)
	assert.Equal(messageID, matches[0].ID)
}

func TestSyncArchivesGenericConferenceURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	joinURL := "https://video.example.org/conference/room/alpha"
	raw := strings.Replace(calendarBody, "SUMMARY:Example meeting", "SUMMARY:Example meeting\r\nCONFERENCE:"+joinURL, 1)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = raw
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE message_type='calendar_event'`).Scan(&messageID))
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	assert.Contains(body, "Join: "+joinURL)
	var storedURL string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT join_url FROM calendar_events WHERE message_id = ?`), messageID).Scan(&storedURL))
	assert.Equal(joinURL, storedURL)
}

func TestSyncPersistsParticipantDomains(t *testing.T) {
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:organizer@example.org\r\nATTENDEE:mailto:guest@example.net\r\nEND:VEVENT", 1)

	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(t, err)
	for _, tc := range []struct{ email, wantDomain string }{
		{email: "organizer@example.org", wantDomain: "example.org"},
		{email: "guest@example.net", wantDomain: "example.net"},
	} {
		var domain string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT domain FROM participants WHERE email_address = ?`), tc.email).Scan(&domain))
		assert.Equal(t, tc.wantDomain, domain)
	}
}

func TestSyncConfirmsOwnerMailboxForActivityClassification(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:organizer@example.org\r\nATTENDEE:mailto:owner@example.org\r\nEND:VEVENT", 1)
	src, err := st.GetOrCreateSourceContext(t.Context(), "caldav", sourceIdentifier("personal", "http://203.0.113.1/home/cal/"))
	require.NoError(err)
	require.NoError(st.AddAccountIdentityContext(t.Context(), src.ID, "preferred@example.org", "manual"))

	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	var sourceID, messageID, organizerID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE message_type = 'calendar_event'`).Scan(&messageID))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM participants WHERE email_address = 'organizer@example.org'`).Scan(&organizerID))
	organizerPerson, created, err := st.CreatePersonFromParticipant(organizerID)
	require.NoError(err)
	require.True(created)

	identities, err := st.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	var confirmedOwner, preservedPreference bool
	for _, identity := range identities {
		if identity.Address == "owner@example.org" && strings.Contains(identity.SourceSignal, "account-email") {
			confirmedOwner = true
		}
		if identity.Address == "preferred@example.org" && strings.Contains(identity.SourceSignal, "manual") {
			preservedPreference = true
		}
	}
	assert.True(confirmedOwner)
	assert.True(preservedPreference)

	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	classification := store.ClassifyActivityCandidate(candidates[0], store.DefaultMaxDirectActivityCounterparts)
	assert.Equal(store.DirectionInbound, classification.Direction)
	require.Len(classification.Persons, 1)
	assert.Equal(organizerPerson.ID, classification.Persons[0].PersonID)
	assert.Equal(store.RoleOrganizer, classification.Persons[0].Role)
	assert.Equal(store.EvidenceDirect, classification.Persons[0].Evidence)
}

func TestSyncOwnerOrganizerAttributionTracksIdentityRemoval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:owner@example.org\r\nEND:VEVENT", 1)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	var sourceID, messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE message_type = 'calendar_event'`).Scan(&messageID))
	readAttribution := func() (isFromMe, sourceIsFromMe, identityIsFromMe bool) {
		t.Helper()
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT is_from_me, source_is_from_me, identity_is_from_me FROM messages WHERE id = ?`), messageID).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe))
		return isFromMe, sourceIsFromMe, identityIsFromMe
	}

	isFromMe, sourceIsFromMe, identityIsFromMe := readAttribution()
	assert.True(isFromMe)
	assert.False(sourceIsFromMe)
	assert.True(identityIsFromMe)

	removed, err := st.RemoveAccountIdentity(sourceID, "owner@example.org")
	require.NoError(err)
	require.Equal(int64(1), removed)
	isFromMe, sourceIsFromMe, identityIsFromMe = readAttribution()
	assert.False(isFromMe)
	assert.False(sourceIsFromMe)
	assert.False(identityIsFromMe)
}

func TestSyncDoesNotConfirmNonMailboxUsername(t *testing.T) {
	require := require.New(t)
	c, _ := archiveFixture(t)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.Username = "calendar-login"
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)
	var sourceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
	identities, err := st.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	assert.Empty(t, identities)
}

func TestSyncRepairsCalendarAttributionOnIngestAndResync(t *testing.T) {
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:organizer@example.org\r\nATTENDEE:mailto:guest@example.net\r\nEND:VEVENT", 1)

	assertCalendarAttribution := func() {
		var sourceID int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
		var attributed, pendingRows int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND message_type = 'calendar_event' AND account_path = 'calendar'`), sourceID).Scan(&attributed))
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND message_type = 'calendar_event' AND account_path IS NULL`), sourceID).Scan(&pendingRows))
		assert.Equal(t, 1, attributed)
		assert.Zero(t, pendingRows)

		accounts, err := st.ListVirtualAccountsContext(t.Context())
		require.NoError(t, err)
		var virtualPending int64
		for _, account := range accounts[sourceID] {
			virtualPending += account.PendingCount
		}
		assert.Zero(t, virtualPending)
	}

	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(t, err)
	assertCalendarAttribution()

	f.Lock()
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(t, err)
	assertCalendarAttribution()
}

func TestSyncIncompleteSnapshotDoesNotTombstoneEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	f.Lock()
	f.forceSnapshot = true
	f.incompleteSnapshot = true
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.ErrorContains(err, "collection response")
	assert.Zero(count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
	assert.Equal("t1", sourceToken(t, st))

	f.Lock()
	f.incompleteSnapshot = false
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	assert.Zero(count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
}

func TestSyncCancelledEventIsActivityIneligible(t *testing.T) {
	require := require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "SUMMARY:Example meeting", "SUMMARY:Example meeting\r\nSTATUS:CANCELLED", 1)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE message_type='calendar_event'`).Scan(&messageID))
	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.False(t, candidates[0].Eligible)
}

func TestSyncSparseTimedCancellationPreservesOriginalStartOnResync(t *testing.T) {
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT\r\nEND:VCALENDAR", "RRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:example@example.org\r\nRECURRENCE-ID:20270321T130000Z\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
	st := testutil.NewTestStore(t)

	assertOriginalStart := func() {
		t.Helper()
		var sentAt sql.NullTime
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT m.sent_at FROM messages m JOIN calendar_events e ON e.message_id = m.id WHERE e.kind = 'exception'`).Scan(&sentAt))
		require.True(t, sentAt.Valid)
		assert.Equal(t, time.Date(2027, 3, 21, 13, 0, 0, 0, time.UTC), sentAt.Time.UTC())
	}

	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(t, err)
	assertOriginalStart()

	f.Lock()
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(t, err)
	assertOriginalStart()
}

func TestSyncArchivesAllDayDateForActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := strings.Replace(calendarBody, "DTSTART:20270314T130000Z\r\nDURATION:PT1H", "DTSTART;VALUE=DATE:20270314\r\nDTEND;VALUE=DATE:20270315", 1)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = raw
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.TimeZone = "America/Los_Angeles"
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)

	var messageID int64
	var sentAt time.Time
	var rawMetadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id, sent_at, metadata FROM messages WHERE message_type='calendar_event'`).Scan(&messageID, &sentAt, &rawMetadata))
	assert.Equal(time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC), sentAt.UTC())
	var metadata struct {
		AllDay bool `json:"all_day"`
	}
	require.NoError(json.Unmarshal([]byte(rawMetadata), &metadata))
	assert.True(metadata.AllDay)

	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{messageID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.True(candidates[0].DateOnly)
}

func TestSyncDeltaRejectsDuplicateOnUntouchedResource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	f.Lock()
	f.raw["b.ics"] = calendarBody
	f.reported = map[string]bool{"b.ics": true}
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.ErrorContains(err, "duplicate")
	assertions.Equal("t1", sourceToken(t, st))
	// Removing the old resource makes this a legitimate move, in either order.
	f.Lock()
	delete(f.raw, "a.ics")
	f.deleted = []string{"a.ics"}
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal("t2", sourceToken(t, st))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
}
