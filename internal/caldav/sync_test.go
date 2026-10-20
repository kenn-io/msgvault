package caldav

import (
	"database/sql"
	"encoding/binary"
	"encoding/json/v2"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type syncFixture struct {
	sync.Mutex

	raw                     map[string]string
	reported                map[string]bool
	deleted                 []string
	token                   string
	failFetch, invalidToken bool
	forceGET                bool
	forceSnapshot           bool
	incompleteSnapshot      bool
	explicitCollectionPort  bool
}

func archiveFixture(t *testing.T) (*Client, *syncFixture) {
	t.Helper()
	assertions := assert.New(t)
	f := &syncFixture{raw: map[string]string{"a.ics": calendarBody}, token: "t1"}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.Lock()
		defer f.Unlock()
		if body, ok := discoveryResponse(r); ok {
			if r.URL.Path == "/home/" && f.explicitCollectionPort {
				body = strings.Replace(body, `<D:href>/home/cal/</D:href>`, `<D:href>http://203.0.113.1:80/home/cal/</D:href>`, 1)
			}
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
			var report struct {
				Token string `xml:"sync-token"`
			}
			if !assertions.NoError(xml.Unmarshal(req, &report)) {
				return
			}
			var responses strings.Builder
			for name := range f.raw {
				if report.Token != "" && f.reported != nil && !f.reported[name] {
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
		if f.forceGET && strings.Contains(string(req), "calendar-multiget") {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
			raw, ok := f.raw[strings.TrimPrefix(r.URL.Path, "/home/cal/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"`+f.token+`"`)
			_, _ = io.WriteString(w, raw)
			return
		}
		var responses strings.Builder
		for name, raw := range f.raw {
			if strings.Contains(string(req), "/home/cal/"+name) {
				responses.WriteString(strings.Replace(data("/home/cal/"+name, raw), `"v1"`, `"`+f.token+`"`, 1))
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

func TestSyncCanonicalizesDefaultPortForSourceIdentity(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)

	first, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(1, first.Events)

	f.Lock()
	f.explicitCollectionPort = true
	f.Unlock()
	second, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Zero(second.Events)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM sources WHERE source_type='caldav'`))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_events`))
}

func compound() string {
	return strings.Replace(calendarBody, "END:VEVENT\r\nEND:VCALENDAR", "RRULE:FREQ=WEEKLY;COUNT=3\r\nATTENDEE;CUTYPE=ROOM:mailto:room@example.org\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:example@example.org\r\nRECURRENCE-ID:20270321T130000Z\r\nDTSTART:20270321T150000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
}

func TestSyncFloatingEventsSurviveConfiguredTimeZoneChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = strings.NewReplacer(
		"20270314T130000Z", "20270314T090000",
		"20270321T130000Z", "20270321T090000",
		"20270321T150000Z", "20270321T110000",
	).Replace(compound())
	opts := syncOpts()
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)
	ids := map[string]int64{}
	for _, step := range []struct{ zone, master, exception, original string }{
		{"UTC", "2027-03-14T09:00:00Z", "2027-03-21T11:00:00Z", "2027-03-21T09:00:00Z"},
		{"America/New_York", "2027-03-14T13:00:00Z", "2027-03-21T15:00:00Z", "2027-03-21T13:00:00Z"},
	} {
		if step.zone != "UTC" {
			opts.TimeZone = step.zone
			_, err = c.Sync(t.Context(), st, opts)
			require.NoError(err)
		}
		for kind, want := range map[string]string{"series": step.master, "exception": step.exception} {
			var id int64
			var start time.Time
			var original sql.NullTime
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_id, start_at, original_start_at FROM calendar_events WHERE kind=?`), kind).Scan(&id, &start, &original))
			if step.zone == "UTC" {
				ids[kind] = id
			}
			assert.Equal(ids[kind], id)
			assert.Equal(want, start.UTC().Format(time.RFC3339))
			if kind == "exception" {
				require.True(original.Valid)
				assert.Equal(step.original, original.Time.UTC().Format(time.RFC3339))
			}
		}
		assert.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages`))
		assert.Zero(count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
	}
}

func TestSyncFloatingTimeZoneChangeRetriesMessageTimeAfterFetchFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "20270314T130000Z", "20270314T090000", 1)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)
	var id int64
	var sent time.Time
	require.NoError(st.DB().QueryRow(`SELECT id, sent_at FROM messages`).Scan(&id, &sent))
	assert.Equal("2027-03-14T09:00:00Z", sent.UTC().Format(time.RFC3339))

	opts.TimeZone = "America/New_York"
	f.Lock()
	f.failFetch = true
	f.Unlock()
	_, err = c.Sync(t.Context(), st, opts)
	require.Error(err)
	var start time.Time
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT start_at FROM calendar_events WHERE message_id = ?`), id).Scan(&start))
	assert.Equal("2027-03-14T13:00:00Z", start.UTC().Format(time.RFC3339))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sent_at FROM messages WHERE id = ?`), id).Scan(&sent))
	assert.Equal("2027-03-14T13:00:00Z", sent.UTC().Format(time.RFC3339))

	f.Lock()
	f.failFetch = false
	f.Unlock()
	_, err = c.Sync(t.Context(), st, opts)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sent_at FROM messages WHERE id = ?`), id).Scan(&sent))
	assert.Equal("2027-03-14T13:00:00Z", sent.UTC().Format(time.RFC3339))
}

func TestSyncFloatingTimeZoneChangeUpdatesMessageTimeForSkippedResource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "20270314T130000Z", "20270314T090000", 1)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)

	var id int64
	var sent time.Time
	require.NoError(st.DB().QueryRow(`SELECT id, sent_at FROM messages`).Scan(&id, &sent))
	assert.Equal("2027-03-14T09:00:00Z", sent.UTC().Format(time.RFC3339))

	f.Lock()
	f.raw["a.ics"] = strings.Replace(f.raw["a.ics"], "UID:example@example.org", "UID:example@example.org\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270314T090000", 1)
	f.token = "t2"
	f.Unlock()
	opts.TimeZone = "America/New_York"
	result, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, result.Skipped)
	assert.Zero(result.Events)

	var start time.Time
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT start_at FROM calendar_events WHERE message_id = ?`), id).Scan(&start))
	assert.Equal("2027-03-14T13:00:00Z", start.UTC().Format(time.RFC3339))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sent_at FROM messages WHERE id = ?`), id).Scan(&sent))
	assert.Equal("2027-03-14T13:00:00Z", sent.UTC().Format(time.RFC3339))
	assert.Equal("t2", sourceToken(t, st))
}

func TestSyncCalDAVRecurrenceIdentitySurvivesTimeZoneUpdate(t *testing.T) {
	// A fresh process lets ZONEINFO take effect before time.LoadLocation's first call.
	mode := os.Getenv("MSGVAULT_TEST_RECURRENCE_ZONE")
	if mode == "" {
		executable, err := os.Executable()
		require.NoError(t, err)
		for _, mode := range []string{"explicit", "floating"} {
			t.Run(mode, func(t *testing.T) {
				cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSyncCalDAVRecurrenceIdentitySurvivesTimeZoneUpdate$")
				cmd.Env = append(os.Environ(), "MSGVAULT_TEST_RECURRENCE_ZONE="+mode, "ZONEINFO="+t.TempDir())
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
			})
		}
		return
	}
	assertions, requirements := assert.New(t), require.New(t)
	zoneDir := filepath.Join(os.Getenv("ZONEINFO"), "Test")
	requirements.NoError(os.MkdirAll(zoneDir, 0o700))
	writeZone := func(offset int32) {
		// Minimal TZif v1 file with one fixed-offset local time type.
		data := make([]byte, 54)
		copy(data, "TZif")
		binary.BigEndian.PutUint32(data[36:40], 1) // type count
		binary.BigEndian.PutUint32(data[40:44], 4) // abbreviation bytes
		binary.BigEndian.PutUint32(data[44:48], uint32(offset))
		copy(data[50:], "TST\x00")
		requirements.NoError(os.WriteFile(filepath.Join(zoneDir, "Calendar"), data, 0o600))
	}
	writeZone(-4 * 3600)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	rid := "RECURRENCE-ID:20270321T090000"
	if mode == "explicit" {
		rid = "RECURRENCE-ID;TZID=Test/Calendar:20270321T090000"
	}
	f.raw["a.ics"] = strings.Replace(compound(), "RECURRENCE-ID:20270321T130000Z", rid, 1)
	masterStart := "DTSTART:20270314T090000"
	start := "DTSTART:20270321T110000"
	if mode == "explicit" {
		masterStart = "DTSTART;TZID=Test/Calendar:20270314T090000"
		start = "DTSTART;TZID=Test/Calendar:20270321T110000"
	}
	f.raw["a.ics"] = strings.Replace(f.raw["a.ics"], "DTSTART:20270314T130000Z", masterStart, 1)
	f.raw["a.ics"] = strings.Replace(f.raw["a.ics"], "DTSTART:20270321T150000Z", start, 1)
	opts := syncOpts()
	opts.TimeZone = "Test/Calendar"
	_, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	var messageID int64
	var key, metadata string
	requirements.NoError(st.DB().QueryRow(`SELECT m.id,m.source_message_id,m.metadata FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind='exception'`).Scan(&messageID, &key, &metadata))
	raw, err := st.GetMessageRaw(messageID)
	requirements.NoError(err)
	before, ok, err := calevent.Decode(calevent.CalDAVRawFormat, raw, []byte(metadata), opts.TimeZone)
	requirements.NoError(err)
	requirements.True(ok)
	requirements.NotNil(before.Original)
	assertions.Equal("20270321T130000Z", before.Original.Key())

	writeZone(-5 * 3600)
	// Evict the one-resource decode cache, as reopening an archive would do.
	_, _, err = calevent.Decode(calevent.CalDAVRawFormat, []byte(calendarBody), []byte(`{"event_id":"[\"example@example.org\",\"\"]"}`), "UTC")
	requirements.NoError(err)
	after, ok, err := calevent.Decode(calevent.CalDAVRawFormat, raw, []byte(metadata), opts.TimeZone)
	if assertions.NoError(err) && assertions.True(ok) && assertions.NotNil(after.Original) {
		assertions.Equal("20270321T140000Z", after.Original.Key())
	}
	opts.Full = true
	_, err = c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages`))
	var refreshedID int64
	var refreshedKey string
	requirements.NoError(st.DB().QueryRow(`SELECT m.id,m.source_message_id FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind='exception' AND m.deleted_from_source_at IS NULL`).Scan(&refreshedID, &refreshedKey))
	assertions.Equal(messageID, refreshedID)
	assertions.Equal(key, refreshedKey)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), time.Date(2027, 3, 21, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 22, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("2027-03-21T16:00:00Z", page.Occurrences[0].Start.UTC().Format(time.RFC3339))
	assertions.Equal("2027-03-21T17:00:00Z", page.Occurrences[0].End.UTC().Format(time.RFC3339))
	unchanged, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Zero(unchanged.Events)
}

func TestSyncArchivesUnsupportedRecurrencePeriod(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(compound(), "RRULE:FREQ=WEEKLY;COUNT=3", "RRULE:FREQ=WEEKLY;COUNT=3\r\nRDATE;TZID=America/New_York;VALUE=PERIOD:20270328T090000/PT1H", 1)
	st := testutil.NewTestStore(t)
	result, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, result.Events)
	assertions.Zero(result.Skipped)
	rows, err := st.DB().Query(`SELECT m.id,m.metadata,e.kind FROM messages m JOIN calendar_events e ON e.message_id=m.id ORDER BY e.kind`)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	var kinds []string
	for rows.Next() {
		var id int64
		var metadata, kind string
		requirements.NoError(rows.Scan(&id, &metadata, &kind))
		kinds = append(kinds, kind)
		raw, err := st.GetMessageRaw(id)
		requirements.NoError(err)
		assertions.Equal(f.raw["a.ics"], string(raw))
		event, ok, err := calevent.Decode(calevent.CalDAVRawFormat, raw, []byte(metadata), "UTC")
		requirements.NoError(err)
		assertions.True(ok)
		assertions.Equal(kind, event.Kind)
		if kind == "series" {
			assertions.Contains(event.Recurrence, "RDATE;TZID=America/New_York;VALUE=PERIOD:20270328T090000/PT1H")
		}
	}
	requirements.NoError(rows.Err())
	requirements.NoError(rows.Close())
	assertions.Equal([]string{"exception", "series"}, kinds)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	requirements.Len(page.Unexpandable, 1)
	assertions.Equal("unsupported recurrence", page.Unexpandable[0].Reason)
}

func TestSyncResourceDelete(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = compound()
	result, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, result.Events)
	assertions.Equal("t1", sourceToken(t, st))
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM calendar_events `))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_event_attendees WHERE role='room'`))
	var id int64
	requirements.NoError(st.DB().QueryRow(`SELECT m.id FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind='series'`).Scan(&id))
	raw, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Contains(string(raw), "RECURRENCE-ID")
	f.Lock()
	f.raw = map[string]string{}
	f.deleted = []string{"a.ics"}
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`))
	raw, err = st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.NotEmpty(raw)
}

func TestSyncRecomputesConversationStats(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(compound(), "DTSTART:20270321T150000Z\r\nDURATION:PT1H", "DTSTART:20270321T150000Z\r\nDURATION:PT1H\r\nSUMMARY:Later meeting", 1)
	st := testutil.NewTestStore(t)
	result, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(2, result.Events)

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
	for _, tc := range []struct {
		name            string
		postgresTrigger string
		sqliteTrigger   string
		wantError       string
		persistFails    bool
	}{
		{
			name:            "persist of the second event fails",
			postgresTrigger: `CREATE FUNCTION reject_caldav_exception() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='exception' THEN RAISE EXCEPTION 'calendar write blocked'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_caldav_exception BEFORE INSERT ON calendar_events FOR EACH ROW EXECUTE FUNCTION reject_caldav_exception()`,
			sqliteTrigger:   `CREATE TRIGGER reject_caldav_exception BEFORE INSERT ON calendar_events WHEN NEW.kind='exception' BEGIN SELECT RAISE(ABORT,'calendar write blocked'); END`,
			wantError:       "calendar write blocked",
			persistFails:    true,
		},
		{
			name:            "clearing the tombstone fails",
			postgresTrigger: `CREATE FUNCTION reject_caldav_clear() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'calendar clear blocked'; END $$; CREATE TRIGGER reject_caldav_clear BEFORE UPDATE OF deleted_from_source_at ON messages FOR EACH ROW EXECUTE FUNCTION reject_caldav_clear()`,
			sqliteTrigger:   `CREATE TRIGGER reject_caldav_clear BEFORE UPDATE OF deleted_from_source_at ON messages BEGIN SELECT RAISE(ABORT,'calendar clear blocked'); END`,
			wantError:       "calendar clear blocked",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			c, f := archiveFixture(t)
			st := testutil.NewTestStore(t)
			if tc.persistFails {
				f.raw["a.ics"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
				_, err := c.Sync(t.Context(), st, syncOpts())
				require.NoError(err)
				f.Lock()
				f.token = "t2"
				f.raw["a.ics"] = calendarBody
				f.raw["b.ics"] = strings.ReplaceAll(compound(), "example@example.org", "series@example.org")
				f.Unlock()
			}
			trigger := tc.sqliteTrigger
			if st.IsPostgreSQL() {
				trigger = tc.postgresTrigger
			}
			_, err := st.DB().Exec(trigger)
			require.NoError(err)

			result, err := c.Sync(t.Context(), st, syncOpts())
			require.ErrorContains(err, tc.wantError)
			committed := 0
			if tc.persistFails {
				committed = 1
			}
			assert.Equal(committed, result.Events)
			assert.Equal(committed, count(t, st, `SELECT COUNT(*) FROM calendar_events`))
			assert.Equal(committed, count(t, st, `SELECT COUNT(*) FROM conversations WHERE message_count = 1`))
			if tc.persistFails {
				assert.Equal("t1", sourceToken(t, st))
				assert.Zero(count(t, st, `SELECT COUNT(*) FROM calendar_events WHERE kind = 'series'`))
				assert.Zero(count(t, st, `SELECT COUNT(*) FROM calendar_events WHERE kind = 'exception'`))
				_, total, err := st.SearchMessagesQuery(search.Parse("account:owner@example.org"), 0, 10)
				require.NoError(err)
				assert.Equal(int64(1), total)
			} else {
				assert.Zero(count(t, st, `SELECT COUNT(*) FROM messages WHERE account_path = 'calendar' AND account_address = 'owner@example.org'`))
			}
			var sourceID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
			run, err := st.GetLatestSyncContext(t.Context(), sourceID, 0)
			require.NoError(err)
			assert.Equal(store.SyncStatusFailed, run.Status)
			assert.Equal(int64(committed), run.MessagesProcessed)
			assert.Equal(int64(committed), run.MessagesAdded)
			assert.Zero(run.MessagesUpdated)
			assert.Equal(int64(1), run.ErrorsCount)
		})
	}
}

func TestSyncContinuesAfterCalendarFailures(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == "PROPFIND" && r.URL.Path == "/home/" {
			var calendars strings.Builder
			calendars.WriteString(prop("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`))
			for _, name := range []string{"A", "B", "C"} {
				calendars.WriteString(prop("/home/"+name+"/", `<D:displayname>`+name+`</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:supported-report-set><D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report><D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report></D:supported-report-set>`))
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(calendars.String()))
			return
		}
		if body, ok := discoveryResponse(r); ok {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
			return
		}
		var collection string
		switch r.URL.Path {
		case "/home/A/":
			collection = "/home/A/"
		case "/home/B/":
			collection = "/home/B/"
		case "/home/C/":
			collection = "/home/C/"
		default:
			http.NotFound(w, r)
			return
		}
		req, err := io.ReadAll(r.Body)
		if !assertions.NoError(err) {
			return
		}
		var response string
		if strings.Contains(string(req), "sync-collection") {
			response = prop(collection+"a.ics", `<D:getetag>"v1"</D:getetag>`) + `<D:sync-token>t1</D:sync-token>`
		} else {
			raw := compound()
			if r.URL.Path == "/home/B/" {
				raw = calendarBody
			}
			response = data(collection+"a.ics", raw)
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, multi(response))
	})
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.MaxEvents = 1
	result, err := c.Sync(t.Context(), st, opts)
	requirements.Error(err)
	assertions.Contains(err.Error(), `"A" (http://203.0.113.1/home/A/)`)
	assertions.Contains(err.Error(), `"C" (http://203.0.113.1/home/C/)`)
	assertions.Equal(1, result.Calendars)
	assertions.Equal(1, result.Events)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_events`))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM sources WHERE display_name = 'B' AND sync_cursor = 't1'`))
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM sync_runs WHERE status = 'completed'`))
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM sync_runs WHERE status = 'failed'`))
}

func TestSyncInvalidToken(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	f.Lock()
	f.invalidToken = true
	f.token = "t2"
	f.raw = map[string]string{}
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal("t2", sourceToken(t, st))
}

func TestSyncCorrectsTimezoneAfterFailedFirstSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name                string
		failedFirstSnapshot bool
	}{
		{"established source allows change", false},
		{"failed first snapshot allows change", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			c, f := archiveFixture(t)
			st := testutil.NewTestStore(t)
			f.forceSnapshot = true
			f.incompleteSnapshot = tc.failedFirstSnapshot
			_, err := c.Sync(t.Context(), st, syncOpts())
			if tc.failedFirstSnapshot {
				require.ErrorContains(err, "omitted the collection")
				assert.Zero(count(t, st, `SELECT COUNT(*) FROM messages`))
			} else {
				require.NoError(err)
			}
			var config string
			require.NoError(st.DB().QueryRow(`SELECT sync_config FROM sources WHERE source_type='caldav'`).Scan(&config))
			var stored sourceConfig
			require.NoError(json.Unmarshal([]byte(config), &stored))
			assert.Equal("UTC", stored.TimeZone)

			f.Lock()
			f.incompleteSnapshot = false
			f.Unlock()
			opts := syncOpts()
			opts.TimeZone = "America/New_York"
			_, err = c.Sync(t.Context(), st, opts)
			require.NoError(err)
			assert.Positive(count(t, st, `SELECT COUNT(*) FROM calendar_events`))
			require.NoError(st.DB().QueryRow(`SELECT sync_config FROM sources WHERE source_type='caldav'`).Scan(&config))
			require.NoError(json.Unmarshal([]byte(config), &stored))
			assert.Equal("America/New_York", stored.TimeZone)
		})
	}
}

func TestSyncSelectsRedirectedDiscoveryAlias(t *testing.T) {
	assert := assert.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/home/" {
			http.Redirect(w, r, "/canonical/home/", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/canonical/home/" {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(prop("./", `<D:resourcetype><D:collection/></D:resourcetype>`)+prop("cal/", `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:supported-report-set><D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report><D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report></D:supported-report-set>`)))
			return
		}
		if body, ok := discoveryResponse(r); ok {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
			return
		}
		if r.URL.Path != "/canonical/home/cal/" || r.Method != "REPORT" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		if strings.Contains(string(body), "sync-collection") {
			_, _ = io.WriteString(w, multi(prop("/canonical/home/cal/a.ics", `<D:getetag>"v1"</D:getetag>`)+`<D:sync-token>t1</D:sync-token>`))
		} else {
			assert.Contains(string(body), "calendar-multiget")
			_, _ = io.WriteString(w, multi(data("/canonical/home/cal/a.ics", calendarBody)))
		}
	})
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.Calendars = []string{"http://203.0.113.1:80/home/cal/"}
	result, err := c.Sync(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(1, result.Calendars)
	assert.Equal(1, result.Events)
	assert.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_events`))
}

func TestSyncMoveIdentity(t *testing.T) {
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
	var metadata string
	requirements.NoError(st.DB().QueryRow(`SELECT metadata FROM messages WHERE deleted_from_source_at IS NULL`).Scan(&metadata))
	assertions.Contains(metadata, "/b.ics")
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
	assert.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.provider_event_id='["second@example.org",""]'  AND m.deleted_from_source_at IS NULL`))

	// The server's delta omits the resource created and deleted between retries.
	f.Lock()
	delete(f.raw, "b.ics")
	f.reported = map[string]bool{"a.ics": true}
	f.token = "t3"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	assert.Equal("t3", sourceToken(t, st))
	assert.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.provider_event_id='["second@example.org",""]'  AND m.deleted_from_source_at IS NOT NULL`))
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
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:long-description@example.org\r\nDTSTART:20270314T130000Z\r\nSUMMARY:Long description\r\nATTENDEE;CN=Zephyrguest:mailto:guest@example.org\r\nDESCRIPTION:" + folded.String() + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	c, f := archiveFixture(t)
	f.raw["a.ics"] = raw
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE message_type='calendar_event'`).Scan(&messageID))
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	for _, term := range []string{"Zephyrguest", "caldavtailneedle"} {
		assert.Contains(body, term)
		matches, total, err := st.SearchMessagesContext(t.Context(), term, 0, 10)
		require.NoError(err)
		assert.Equal(int64(1), total)
		require.Len(matches, 1)
		assert.Equal(messageID, matches[0].ID)
	}
}

func TestSyncConfirmsOwnerMailboxForActivityClassification(t *testing.T) {
	for _, username := range []string{"owner@example.org", "calendar-login"} {
		t.Run(username, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c, f := archiveFixture(t)
			st := testutil.NewTestStore(t)
			f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:owner@example.org\r\nEND:VEVENT", 1)
			collectionURL, err := url.Parse("http://203.0.113.1/home/cal/")
			require.NoError(err)
			identifier, err := sourceIdentifier("personal", collectionURL.String())
			require.NoError(err)
			src, err := st.GetOrCreateSourceContext(t.Context(), "caldav", identifier)
			require.NoError(err)
			require.NoError(st.AddAccountIdentityContext(t.Context(), src.ID, "preferred@example.org", "manual"))

			opts := syncOpts()
			opts.Username = username
			_, err = c.Sync(t.Context(), st, opts)
			require.NoError(err)
			var sourceID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
			var isFromMe, sourceIsFromMe, identityIsFromMe bool
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT is_from_me, source_is_from_me, identity_is_from_me FROM messages WHERE message_type = 'calendar_event'`).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe))
			assert.Equal(username == "owner@example.org", isFromMe)
			assert.False(sourceIsFromMe)
			assert.Equal(username == "owner@example.org", identityIsFromMe)

			identities, err := st.ListAccountIdentitiesContext(t.Context(), sourceID)
			require.NoError(err)
			wantIdentities := 1
			if username == "owner@example.org" {
				wantIdentities = 2
			}
			assert.Len(identities, wantIdentities)
			var confirmedOwner, preservedPreference bool
			for _, identity := range identities {
				if identity.Address == "owner@example.org" && strings.Contains(identity.SourceSignal, "account-email") {
					confirmedOwner = true
				}
				if identity.Address == "preferred@example.org" && strings.Contains(identity.SourceSignal, "manual") {
					preservedPreference = true
				}
			}
			assert.Equal(username == "owner@example.org", confirmedOwner)
			assert.True(preservedPreference)
		})
	}
}

func TestSyncRepairsCalendarAttributionOnIngestAndResync(t *testing.T) {
	require := require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:organizer@example.org\r\nATTENDEE:mailto:guest@example.net\r\nEND:VEVENT", 1)

	assertCalendarAttribution := func() {
		var sourceID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM sources WHERE source_type = 'caldav'`).Scan(&sourceID))
		var address sql.NullString
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT account_address FROM messages WHERE message_type = 'calendar_event'`).Scan(&address))
		assert.Equal(t, sql.NullString{String: "owner@example.org", Valid: true}, address)
		rows, total, err := st.SearchMessagesQuery(search.Parse("account:owner@example.org"), 0, 10)
		require.NoError(err)
		assert.Equal(t, int64(1), total)
		assert.Len(t, rows, 1)
		var attributed, pendingRows int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND message_type = 'calendar_event' AND account_path = 'calendar'`), sourceID).Scan(&attributed))
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND message_type = 'calendar_event' AND account_path IS NULL`), sourceID).Scan(&pendingRows))
		assert.Equal(t, 1, attributed)
		assert.Zero(t, pendingRows)

		accounts, err := st.ListVirtualAccountsContext(t.Context())
		require.NoError(err)
		var virtualPending, owned, unattributed int64
		for _, account := range accounts[sourceID] {
			virtualPending += account.PendingCount
			if account.AccountAddress == "owner@example.org" {
				owned += account.MessageCount
			}
			if account.Unattributed {
				unattributed += account.MessageCount
			}
		}
		assert.Zero(t, virtualPending)
		assert.Equal(t, int64(1), owned)
		assert.Zero(t, unattributed)
	}

	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	assertCalendarAttribution()
	for _, tc := range []struct{ email, wantDomain string }{
		{email: "organizer@example.org", wantDomain: "example.org"},
		{email: "guest@example.net", wantDomain: "example.net"},
	} {
		var domain string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT domain FROM participants WHERE email_address = ?`), tc.email).Scan(&domain))
		assert.Equal(t, tc.wantDomain, domain)
	}

	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET account_path = NULL WHERE message_type = 'calendar_event'`)
	require.NoError(err)

	f.Lock()
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
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
	require.ErrorContains(err, "omitted the collection")
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

func TestSyncBoundlessCancellationPreservesArchivedEvent(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	first, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Equal(1, first.Events)

	before := occurrences(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	requirements.Len(before, 1)
	assertions.Equal("Example meeting", before[0].Subject)

	f.Lock()
	f.raw["a.ics"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:example@example.org\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	f.token = "t2"
	f.Unlock()
	result, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	assertions.Zero(result.Events)
	assertions.Equal(1, result.Skipped)

	after := occurrences(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	requirements.Len(after, 1)
	assertions.Equal(before[0].Start.UTC().Format(time.RFC3339), after[0].Start.UTC().Format(time.RFC3339))
	assertions.Equal(before[0].Subject, after[0].Subject)
	assertions.Equal(before[0].Status, after[0].Status)
	assertions.False(after[0].DeletedFromSource)
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

func TestSyncDeltaSkipsDuplicateOnUntouchedResource(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uniqueSibling bool
	}{
		{"unique sibling skips whole resource", true},
		{"untouched resource adopted after deletion", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			c, f := archiveFixture(t)
			st := testutil.NewTestStore(t)
			if !tc.uniqueSibling {
				_, err := c.Sync(t.Context(), st, syncOpts())
				requirements.NoError(err)
			}
			f.Lock()
			f.raw["b.ics"] = calendarBody
			if tc.uniqueSibling {
				f.raw["b.ics"] = strings.Replace(calendarBody, "BEGIN:VEVENT", "BEGIN:VEVENT\r\nUID:unique-sibling\r\nDTSTART:20270314T130000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT", 1)
			}
			if !tc.uniqueSibling {
				f.reported = map[string]bool{"b.ics": true}
			}
			f.token = "t2"
			f.Unlock()
			result, err := c.Sync(t.Context(), st, syncOpts())
			requirements.NoError(err)
			assertions.Equal(1, result.Skipped)
			assertions.Empty(sourceToken(t, st))
			if tc.uniqueSibling {
				assertions.Equal(1, result.Events)
				assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE subject = 'Example meeting'`))
				assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages`))
				assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM sync_run_items WHERE status = 'error' AND source_message_id = 'http://203.0.113.1:80/home/cal/b.ics'`))
				var status string
				requirements.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
				assertions.Equal("completed", status)
				return
			}
			// The server reports only the deletion, since b.ics has not changed.
			f.Lock()
			delete(f.raw, "a.ics")
			f.deleted = []string{"a.ics"}
			f.reported = map[string]bool{}
			f.token = "t3"
			f.Unlock()
			_, err = c.Sync(t.Context(), st, syncOpts())
			requirements.NoError(err)
			assertions.Equal("t3", sourceToken(t, st))
			assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
			var metadata string
			requirements.NoError(st.DB().QueryRow(`SELECT metadata FROM messages WHERE deleted_from_source_at IS NULL`).Scan(&metadata))
			assertions.Contains(metadata, "/b.ics")
		})
	}
}

func TestSyncSkipsUnparseableResource(t *testing.T) {
	emptyAllDay := strings.Replace(calendarBody, "DTSTART:20270314T130000Z\r\nDURATION:PT1H", "DTSTART;VALUE=DATE:20270314\r\nDTEND;VALUE=DATE:20270314", 1)
	zeroDurationAllDay := strings.Replace(calendarBody, "DTSTART:20270314T130000Z\r\nDURATION:PT1H", "DTSTART;VALUE=DATE:20270314\r\nDURATION:P0D", 1)
	rangeEdit := strings.Replace(calendarBody, "UID:example@example.org", "UID:range@example.org\r\nRECURRENCE-ID;RANGE=THISANDFUTURE:20270314T130000Z", 1)
	rangeEdit = strings.Replace(rangeEdit, "BEGIN:VEVENT", "BEGIN:VEVENT\r\nUID:skipped-sibling@example.org\r\nDTSTART:20270314T130000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT", 1)
	valid := strings.Replace(calendarBody, "UID:example@example.org", "UID:valid@example.org", 1)
	for _, tc := range []struct {
		name           string
		full, forceGET bool
		invalid        string
		unreadableHref string
	}{
		{name: "delta", invalid: emptyAllDay},
		{name: "full", full: true, invalid: emptyAllDay},
		{name: "zero all-day duration", invalid: zeroDurationAllDay},
		{name: "zero all-day duration full", full: true, invalid: zeroDurationAllDay},
		{name: "invalid UID", forceGET: true, invalid: strings.Replace(calendarBody, "UID:example@example.org", "UID:bad\xff", 1)},
		{name: "unreadable before duplicate", invalid: "unreadable calendar", unreadableHref: "a.ics"},
		{name: "unreadable after duplicate", invalid: "unreadable calendar", unreadableHref: "z.ics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			c, f := archiveFixture(t)
			href := "a.ics"
			if tc.unreadableHref != "" {
				href = tc.unreadableHref
				f.raw = map[string]string{href: calendarBody}
			}
			st := testutil.NewTestStore(t)
			_, err := c.Sync(t.Context(), st, syncOpts())
			requirements.NoError(err)
			var id int64
			var oldMetadata string
			requirements.NoError(st.DB().QueryRow(`SELECT id, metadata FROM messages`).Scan(&id, &oldMetadata))
			f.Lock()
			f.raw = map[string]string{href: tc.invalid, "range.ics": rangeEdit, "valid.ics": valid}
			if tc.unreadableHref != "" {
				f.raw = map[string]string{href: tc.invalid, "m.ics": strings.Replace(calendarBody, "Example meeting", "Conflicting meeting", 1)}
			}
			f.forceGET = tc.forceGET
			f.token = "t2"
			f.Unlock()
			opts := syncOpts()
			opts.Full = tc.full
			result, err := c.Sync(t.Context(), st, opts)
			requirements.NoError(err)
			assertions.Equal(2, result.Skipped)
			wantEvents, wantLive, wantToken := 1, 2, "t2"
			if tc.unreadableHref != "" {
				wantEvents, wantLive, wantToken = 0, 1, ""
				raw, err := st.GetMessageRaw(id)
				requirements.NoError(err)
				assertions.Equal(calendarBody, string(raw))
			}
			assertions.Equal(wantEvents, result.Events)
			assertions.Zero(result.Removed)
			assertions.Equal(wantLive, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
			var metadata string
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata FROM messages WHERE source_message_id = ?`), `["example@example.org",""]`).Scan(&metadata))
			assertions.Equal(oldMetadata, metadata)
			var sourceID int64
			requirements.NoError(st.DB().QueryRow(`SELECT id FROM sources WHERE source_type='caldav'`).Scan(&sourceID))
			run, err := st.GetLatestSyncContext(t.Context(), sourceID, 0)
			requirements.NoError(err)
			assertions.Equal(store.SyncStatusCompleted, run.Status)
			assertions.Equal(int64(2), run.ErrorsCount)
			var errorItems int
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM sync_run_items WHERE sync_run_id = ? AND status = 'error'`), run.ID).Scan(&errorItems))
			assertions.Equal(2, errorItems)
			assertions.Equal(wantToken, sourceToken(t, st))
		})
	}
}

func TestSyncBoundsResultingLiveEvents(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	opts := syncOpts()
	opts.MaxEvents = 1
	c, f := archiveFixture(t)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	// Replacing the only event keeps the calendar at the bound.
	f.Lock()
	f.raw = map[string]string{"b.ics": strings.Replace(calendarBody, "UID:example@example.org", "UID:second@example.org", 1)}
	f.deleted = []string{"a.ics"}
	f.token = "t2"
	f.Unlock()
	result, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Equal(int64(1), result.Removed)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
	f.Lock()
	f.raw["c.ics"] = strings.Replace(calendarBody, "UID:example@example.org", "UID:third@example.org", 1)
	f.deleted = nil
	f.token = "t3"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, opts)
	requirements.ErrorContains(err, `CalDAV calendar "Example calendar" has 2 live events; increase max_events (currently 1)`)
	assertions.Equal("t2", sourceToken(t, st))
	opts.MaxEvents = 2
	_, err = c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Equal(2, count(t, st, `SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NULL`))
	assertions.Equal("t3", sourceToken(t, st))
}

func occurrences(t *testing.T, st *store.Store, from, to string) []store.CalendarOccurrence {
	t.Helper()
	requirements := require.New(t)
	start, err := time.Parse(time.RFC3339, from)
	requirements.NoError(err)
	end, err := time.Parse(time.RFC3339, to)
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), start, end)
	requirements.NoError(err)
	return page.Occurrences
}

func TestSyncCancelledCalDAVOverrideIsNotInherited(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(compound(), "DTSTART:20270321T150000Z\r\nDURATION:PT1H", "STATUS:CANCELLED", 1)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	got := occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
	requirements.Len(got, 1)
	assertions.Equal("cancelled", got[0].Status)
	assertions.Empty(got[0].Subject)
	assertions.Empty(got[0].Attendees)
}

func TestSyncRemovedOverrideRestoresOriginalOccurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = compound()
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	got := occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
	requirements.Len(got, 1)
	assertions.Equal("2027-03-21T15:00:00Z", got[0].Start.UTC().Format(time.RFC3339))

	f.Lock()
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "RRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT", 1)
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	got = occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
	requirements.Len(got, 1)
	assertions.Equal("2027-03-21T13:00:00Z", got[0].Start.UTC().Format(time.RFC3339))
	assertions.Equal(got[0].SeriesMessageID, got[0].MessageID)
	assertions.False(got[0].DeletedFromSource)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM messages m JOIN calendar_events e ON e.message_id = m.id WHERE e.kind = 'exception' AND m.deleted_from_source_at IS NOT NULL`))

	f.Lock()
	delete(f.raw, "a.ics")
	f.deleted = []string{"a.ics"}
	f.token = "t3"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	got = occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
	requirements.Len(got, 1)
	assertions.Equal("2027-03-21T13:00:00Z", got[0].Start.UTC().Format(time.RFC3339))
	assertions.Equal(got[0].SeriesMessageID, got[0].MessageID)
	assertions.True(got[0].DeletedFromSource)
}

func TestSyncResourceDeletionPreservesActiveOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantStart, wantStatus string
	}{
		{name: "moved", body: compound(), wantStart: "2027-03-21T15:00:00Z", wantStatus: "confirmed"},
		{name: "cancelled", body: strings.Replace(compound(), "DTSTART:20270321T150000Z\r\nDURATION:PT1H", "STATUS:CANCELLED", 1), wantStart: "2027-03-21T13:00:00Z", wantStatus: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			c, f := archiveFixture(t)
			f.raw["a.ics"] = tc.body
			st := testutil.NewTestStore(t)
			_, err := c.Sync(t.Context(), st, syncOpts())
			requirements.NoError(err)

			f.Lock()
			delete(f.raw, "a.ics")
			f.deleted = []string{"a.ics"}
			f.token = "t2"
			f.Unlock()
			_, err = c.Sync(t.Context(), st, syncOpts())
			requirements.NoError(err)

			got := occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
			requirements.Len(got, 1)
			assertions.Equal(tc.wantStart, got[0].Start.UTC().Format(time.RFC3339))
			assertions.Equal(tc.wantStatus, got[0].Status)
			assertions.NotEqual(got[0].SeriesMessageID, got[0].MessageID)
			assertions.True(got[0].DeletedFromSource)
		})
	}
}

func TestSyncCivilDayDurationKeepsWallTimeAcrossDST(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "DTSTART:20270314T130000Z\r\nDURATION:PT1H", "DTSTART;TZID=America/New_York:20270313T090000\r\nDURATION:P1D\r\nRRULE:FREQ=WEEKLY;COUNT=2", 1)
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	requirements.NoError(err)
	got := occurrences(t, st, "2027-03-13T00:00:00Z", "2027-03-22T00:00:00Z")
	requirements.Len(got, 2)
	assertions.Equal("2027-03-21T13:00:00Z", got[1].End.UTC().Format(time.RFC3339))
	// The series bound must reach the later occurrence's wall-clock end.
	assertions.Len(occurrences(t, st, "2027-03-21T12:30:00Z", "2027-03-21T12:45:00Z"), 1)
}

func TestSyncRecipientMembership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:recipients@example.org\r\nDTSTART:20270314T130000Z\r\nORGANIZER:mailto:organizer@example.org\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	rows, total, err := st.SearchMessagesQuery(search.Parse("to:organizer@example.org"), 0, 10)
	require.NoError(err)
	assert.Zero(total)
	assert.Empty(rows)
	f.Lock()
	f.raw["a.ics"] = strings.Replace(f.raw["a.ics"], "END:VEVENT", "ATTENDEE:mailto:guest@example.org\r\nATTENDEE:mailto:organizer@example.org\r\nATTENDEE;CUTYPE=ROOM:mailto:room@example.org\r\nEND:VEVENT", 1)
	f.token = "t2"
	f.Unlock()
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	for _, email := range []string{"guest@example.org", "organizer@example.org"} {
		rows, total, err = st.SearchMessagesQuery(search.Parse("to:"+email), 0, 10)
		require.NoError(err)
		assert.EqualValues(1, total)
		assert.Len(rows, 1)
	}
	rows, total, err = st.SearchMessagesQuery(search.Parse("to:room@example.org"), 0, 10)
	require.NoError(err)
	assert.Zero(total)
	assert.Empty(rows)
	assert.Zero(count(t, st, `SELECT COUNT(*) FROM participants WHERE email_address='room@example.org'`))
}

func TestSyncOrganizerMatchingNonhumanAttendeeIsDropped(t *testing.T) {
	for _, cutype := range []string{"ROOM", "RESOURCE"} {
		t.Run(cutype, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			c, f := archiveFixture(t)
			f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ORGANIZER:mailto:Room@Example.org\r\nATTENDEE;CUTYPE="+cutype+":mailto:room@example.org\r\nATTENDEE:mailto:guest@example.org\r\nEND:VEVENT", 1)
			st := testutil.NewTestStore(t)
			_, err := c.Sync(t.Context(), st, syncOpts())
			requirements.NoError(err)
			rows, total, err := st.SearchMessagesQuery(search.Parse("to:guest@example.org"), 0, 10)
			requirements.NoError(err)
			assertions.EqualValues(1, total)
			assertions.Len(rows, 1)
			for _, query := range []string{"to:room@example.org", "from:room@example.org"} {
				rows, total, err = st.SearchMessagesQuery(search.Parse(query), 0, 10)
				requirements.NoError(err)
				assertions.Zero(total, query)
				assertions.Empty(rows, query)
			}
			assertions.Zero(count(t, st, `SELECT COUNT(*) FROM participants WHERE email_address='room@example.org'`))
			assertions.Zero(count(t, st, `SELECT COUNT(*) FROM messages WHERE sender_id IS NOT NULL`))
		})
	}
}

func TestSyncBodyUsesEventClock(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			c, f := archiveFixture(t)
			start := "DTSTART:20270314T090000"
			if explicit {
				start = "DTSTART;TZID=America/New_York:20270314T090000"
			}
			f.raw["a.ics"] = strings.Replace(calendarBody, "DTSTART:20270314T130000Z", start, 1)
			st := testutil.NewTestStore(t)
			opts := syncOpts()
			opts.TimeZone = "America/New_York"
			_, err := c.Sync(t.Context(), st, opts)
			requirements.NoError(err)
			var id int64
			var snippet, body string
			requirements.NoError(st.DB().QueryRow(`SELECT id, snippet FROM messages`).Scan(&id, &snippet))
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
			assertions.Equal("Example meeting\nWhen: 2027-03-14 09:00 - 2027-03-14 10:00", body)
			assertions.Equal(body, snippet)
		})
	}
}

func TestSyncUnchangedFullReadSkipsWrites(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, _ := archiveFixture(t)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.Full = true
	_, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	// An old modification marker makes another event write observable without a clock wait.
	_, err = st.DB().Exec(`UPDATE messages SET last_modified = '2000-01-01 00:00:00'`)
	requirements.NoError(err)
	var before string
	requirements.NoError(st.DB().QueryRow(`SELECT last_modified FROM messages`).Scan(&before))
	result, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Zero(result.Events)
	var after string
	requirements.NoError(st.DB().QueryRow(`SELECT last_modified FROM messages`).Scan(&after))
	assertions.Equal(before, after)
}

func TestSyncUsernameChangeRefreshesUnreportedEvents(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "ATTENDEE:mailto:owner@example.org\r\nATTENDEE:mailto:other@example.org\r\nEND:VEVENT", 1)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	_, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	f.Lock()
	f.reported = map[string]bool{}
	f.Unlock()
	opts.Username = "other@example.org"
	result, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Equal(1, result.Events)
	var meta string
	requirements.NoError(st.DB().QueryRow(`SELECT metadata FROM messages`).Scan(&meta))
	var metadata calevent.CalDAVMetadata
	requirements.NoError(json.Unmarshal([]byte(meta), &metadata))
	assertions.Equal("other@example.org", metadata.AccountEmail)
	assertions.Equal(1, count(t, st, `SELECT COUNT(*) FROM calendar_event_attendees WHERE email='other@example.org' AND is_copy_self`))
	assertions.Zero(count(t, st, `SELECT COUNT(*) FROM calendar_event_attendees WHERE email='owner@example.org' AND is_copy_self`))
}

func TestSyncSeriesBecomesStandalone(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			require := require.New(t)
			c, f := archiveFixture(t)
			f.raw["a.ics"] = compound()
			if cancelled {
				f.raw["a.ics"] = strings.Replace(compound(), "DTSTART:20270321T150000Z\r\nDURATION:PT1H", "STATUS:CANCELLED", 1)
			}
			st := testutil.NewTestStore(t)
			_, err := c.Sync(t.Context(), st, syncOpts())
			require.NoError(err)
			f.raw["a.ics"], f.token = calendarBody, "t2"
			_, err = c.Sync(t.Context(), st, syncOpts())
			require.NoError(err)
			from, err := time.Parse(time.RFC3339, "2027-03-14T00:00:00Z")
			require.NoError(err)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 9))
			require.NoError(err)
			require.Len(page.Occurrences, 1)
			require.Empty(page.Unexpandable)
			require.Equal("2027-03-14T13:00:00Z", page.Occurrences[0].Start.UTC().Format(time.RFC3339))
		})
	}
}

func TestSyncRestoredResourceDropsHistoricalOverride(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			require := require.New(t)
			c, f := archiveFixture(t)
			f.raw["a.ics"] = compound()
			if cancelled {
				f.raw["a.ics"] = strings.Replace(compound(), "DTSTART:20270321T150000Z\r\nDURATION:PT1H", "STATUS:CANCELLED", 1)
			}
			st := testutil.NewTestStore(t)
			_, err := c.Sync(t.Context(), st, syncOpts())
			require.NoError(err)
			delete(f.raw, "a.ics")
			f.deleted, f.token = []string{"a.ics"}, "t2"
			_, err = c.Sync(t.Context(), st, syncOpts())
			require.NoError(err)
			f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "RRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT", 1)
			f.deleted, f.token = nil, "t3"
			opts := syncOpts()
			opts.MaxEvents = 1
			_, err = c.Sync(t.Context(), st, opts)
			require.NoError(err)
			delete(f.raw, "a.ics")
			f.deleted, f.token = []string{"a.ics"}, "t4"
			_, err = c.Sync(t.Context(), st, opts)
			require.NoError(err)
			got := occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
			require.Len(got, 1)
			require.Equal("2027-03-21T13:00:00Z", got[0].Start.UTC().Format(time.RFC3339))
			require.Equal("confirmed", got[0].Status)
			require.Equal(got[0].SeriesMessageID, got[0].MessageID)
			require.True(got[0].DeletedFromSource)
		})
	}
}

func TestSyncDeletionAndFloatingEventTimeZoneChange(t *testing.T) {
	require := require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.Replace(calendarBody, "20270314T130000Z", "20270314T090000", 1)
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)
	delete(f.raw, "a.ics")
	f.deleted, f.token = []string{"a.ics"}, "t2"
	opts.TimeZone = "America/New_York"
	_, err = c.Sync(t.Context(), st, opts)
	require.NoError(err)
	got := occurrences(t, st, "2027-03-14T00:00:00Z", "2027-03-15T00:00:00Z")
	require.Len(got, 1)
	require.True(got[0].DeletedFromSource)
	require.Equal("2027-03-14T13:00:00Z", got[0].Start.UTC().Format(time.RFC3339))
	var sent, lastMessage time.Time
	require.NoError(st.DB().QueryRow(`SELECT sent_at FROM messages`).Scan(&sent))
	require.Equal("2027-03-14T13:00:00Z", sent.UTC().Format(time.RFC3339))
	require.NoError(st.DB().QueryRow(`SELECT last_message_at FROM conversations`).Scan(&lastMessage))
	require.Equal("2027-03-14T13:00:00Z", lastMessage.UTC().Format(time.RFC3339))
}

func TestSyncResourceUpdateIsAtomic(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(strconv.FormatBool(deleted), func(t *testing.T) {
			require := require.New(t)
			c, f := archiveFixture(t)
			f.raw["a.ics"] = compound()
			st := testutil.NewTestStore(t)
			_, err := c.Sync(t.Context(), st, syncOpts())
			require.NoError(err)
			if st.IsPostgreSQL() {
				_, err = st.DB().Exec(`CREATE FUNCTION reject_caldav_exception() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='exception' THEN RAISE EXCEPTION 'exception blocked'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_caldav_exception BEFORE INSERT ON calendar_events FOR EACH ROW EXECUTE FUNCTION reject_caldav_exception()`)
			} else {
				_, err = st.DB().Exec(`CREATE TRIGGER reject_caldav_exception BEFORE INSERT ON calendar_events WHEN NEW.kind='exception' BEGIN SELECT RAISE(ABORT,'exception blocked'); END`)
			}
			require.NoError(err)
			f.raw["a.ics"], f.token = strings.NewReplacer("Example meeting", "Updated meeting", "20270321T150000Z", "20270321T160000Z").Replace(compound()), "t2"
			_, err = c.Sync(t.Context(), st, syncOpts())
			require.ErrorContains(err, "exception blocked")
			require.Equal("t1", sourceToken(t, st))
			for kind, want := range map[string]string{"series": "2027-03-14T13:00:00Z", "exception": "2027-03-21T15:00:00Z"} {
				var id int64
				var sent, start time.Time
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT m.id, m.sent_at, e.start_at FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind=?`), kind).Scan(&id, &sent, &start))
				require.Equal(want, sent.UTC().Format(time.RFC3339))
				require.Equal(want, start.UTC().Format(time.RFC3339))
				raw, err := st.GetMessageRawContext(t.Context(), id)
				require.NoError(err)
				require.Equal(compound(), string(raw))
			}
			if st.IsPostgreSQL() {
				_, err = st.DB().Exec(`DROP TRIGGER reject_caldav_exception ON calendar_events; DROP FUNCTION reject_caldav_exception()`)
			} else {
				_, err = st.DB().Exec(`DROP TRIGGER reject_caldav_exception`)
			}
			require.NoError(err)
			want := "2027-03-21T16:00:00Z"
			if deleted {
				delete(f.raw, "a.ics")
				f.deleted = []string{"a.ics"}
				want = "2027-03-21T15:00:00Z"
			}
			_, err = c.Sync(t.Context(), st, syncOpts())
			require.NoError(err)
			got := occurrences(t, st, "2027-03-14T00:00:00Z", "2027-03-22T00:00:00Z")
			require.Len(got, 2)
			require.Equal(map[bool]string{false: "Updated meeting", true: "Example meeting"}[deleted], got[0].Subject)
			require.Equal(want, got[1].Start.UTC().Format(time.RFC3339))
			require.Equal(deleted, got[1].DeletedFromSource)
			require.Equal("t2", sourceToken(t, st))
		})
	}
}

func TestSyncDeletedSeriesRecoversInterruptedOverrideRemoval(t *testing.T) {
	require := require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = compound()
	st := testutil.NewTestStore(t)
	_, err := c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	// Fail the progress write after the resource snapshot commits.
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION reject_caldav_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.messages_processed > 0 THEN RAISE EXCEPTION 'progress blocked'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_caldav_progress BEFORE UPDATE ON sync_runs FOR EACH ROW EXECUTE FUNCTION reject_caldav_progress()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER reject_caldav_progress BEFORE UPDATE ON sync_runs WHEN NEW.messages_processed > 0 BEGIN SELECT RAISE(ABORT,'progress blocked'); END`)
	}
	require.NoError(err)
	f.raw["a.ics"] = strings.Replace(calendarBody, "END:VEVENT", "RRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT", 1)
	f.token = "t2"
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.ErrorContains(err, "progress blocked")
	var masterID int64
	require.NoError(st.DB().QueryRow(`SELECT message_id FROM calendar_events WHERE kind='series'`).Scan(&masterID))
	raw, err := st.GetMessageRawContext(t.Context(), masterID)
	require.NoError(err)
	require.Equal(f.raw["a.ics"], string(raw))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER reject_caldav_progress ON sync_runs; DROP FUNCTION reject_caldav_progress()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER reject_caldav_progress`)
	}
	require.NoError(err)
	delete(f.raw, "a.ics")
	f.deleted, f.token = []string{"a.ics"}, "t3"
	_, err = c.Sync(t.Context(), st, syncOpts())
	require.NoError(err)
	got := occurrences(t, st, "2027-03-21T00:00:00Z", "2027-03-22T00:00:00Z")
	require.Len(got, 1)
	require.Equal("2027-03-21T13:00:00Z", got[0].Start.UTC().Format(time.RFC3339))
	require.Equal(got[0].SeriesMessageID, got[0].MessageID)
	require.True(got[0].DeletedFromSource)
}

func TestSyncDeletedFloatingEventMessageTimeZoneChange(t *testing.T) {
	require := require.New(t)
	c, f := archiveFixture(t)
	f.raw["a.ics"] = strings.NewReplacer("20270314T130000Z", "20270314T090000", "20270321T130000Z", "20270321T090000", "20270321T150000Z", "20270321T110000").Replace(compound())
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	_, err := c.Sync(t.Context(), st, opts)
	require.NoError(err)
	// Retain an obsolete exception and its raw snapshot, then delete the resource.
	f.raw["a.ics"] = strings.Replace(strings.Replace(calendarBody, "20270314T130000Z", "20270314T090000", 1), "END:VEVENT", "RRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT", 1)
	f.token = "t2"
	_, err = c.Sync(t.Context(), st, opts)
	require.NoError(err)
	delete(f.raw, "a.ics")
	f.deleted, f.token = []string{"a.ics"}, "t3"
	_, err = c.Sync(t.Context(), st, opts)
	require.NoError(err)
	var before string
	require.NoError(st.DB().QueryRow(`SELECT metadata FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind='exception'`).Scan(&before))
	var meta calevent.CalDAVMetadata
	require.NoError(json.Unmarshal([]byte(before), &meta))
	require.True(meta.OverrideRemoved)
	opts.TimeZone = "America/New_York"
	_, err = c.Sync(t.Context(), st, opts)
	require.NoError(err)
	for kind, want := range map[string]string{"series": "2027-03-14T13:00:00Z", "exception": "2027-03-21T15:00:00Z"} {
		var sent, start time.Time
		var deleted bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT m.sent_at, e.start_at, m.deleted_from_source_at IS NOT NULL FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind=?`), kind).Scan(&sent, &start, &deleted))
		require.Equal(want, start.UTC().Format(time.RFC3339))
		require.Equal(want, sent.UTC().Format(time.RFC3339))
		require.True(deleted)
	}
	var after string
	require.NoError(st.DB().QueryRow(`SELECT metadata FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE e.kind='exception'`).Scan(&after))
	require.Equal(before, after)
}
