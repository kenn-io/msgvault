package calsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const testAccount = "alice@example.com"

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newSyncer(t *testing.T, mock *gcal.MockAPI, opts Options) (*Syncer, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	if opts.AccountEmail == "" {
		opts.AccountEmail = testAccount
	}
	s := New(mock, st, opts).WithLogger(quietLogger())
	return s, st
}

func TestRegisterCalendars_NormalizesAccountEmailInSourceIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	s, st := newSyncer(t, m, Options{AccountEmail: "Alice.Example@Example.COM"})

	_, err := s.RegisterCalendars(context.Background())
	require.NoError(err)

	src, err := st.GetSourceByIdentifier("alice.example@example.com/primary")
	require.NoError(err)
	assert.Equal("gcal", src.SourceType)

	var cfg sourceConfig
	require.True(src.SyncConfig.Valid, "SyncConfig")
	require.NoError(json.Unmarshal([]byte(src.SyncConfig.String), &cfg))
	assert.Equal("alice.example@example.com", cfg.AccountEmail)
}

func TestRegisterCalendars_ReusesMixedCaseCalendarSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	s, st := newSyncer(t, m, Options{AccountEmail: "Alice.Example@Example.COM"})

	oldSrc, err := st.GetOrCreateSource(gcal.SourceType, "Alice.Example@Example.COM/primary")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncCursor(oldSrc.ID, "cursor-1"))
	require.NoError(st.UpdateSourceSyncConfig(oldSrc.ID,
		`{"account_email":"Alice.Example@Example.COM","calendar_id":"primary"}`))

	_, err = s.RegisterCalendars(context.Background())
	require.NoError(err)

	src, err := st.GetSourceByIdentifier("alice.example@example.com/primary")
	require.NoError(err)
	assert.Equal(oldSrc.ID, src.ID, "existing source row must be reused")
	assert.Equal("cursor-1", src.SyncCursor.String, "existing cursor state must be preserved")

	sources, err := st.ListSources(gcal.SourceType)
	require.NoError(err)
	assert.Len(sources, 1, "normalization must not create a duplicate gcal source")
}

// --- read-back helpers (direct SQL through the real store) ---

type msgRow struct {
	id                int64
	convID            int64
	mtype             string
	subject           sql.NullString
	sentAt            sql.NullTime
	senderID          sql.NullInt64
	isFromMe          bool
	snippet           sql.NullString
	metadata          sql.NullString
	deletedFromSource sql.NullTime
}

func getMsg(t *testing.T, st *store.Store, sourceID int64, smid string) (msgRow, bool) {
	t.Helper()
	var m msgRow
	err := st.DB().QueryRow(st.Rebind(`
		SELECT id, conversation_id, message_type, subject, sent_at, sender_id,
		       is_from_me, snippet, metadata, deleted_from_source_at
		FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, smid).
		Scan(&m.id, &m.convID, &m.mtype, &m.subject, &m.sentAt, &m.senderID,
			&m.isFromMe, &m.snippet, &m.metadata, &m.deletedFromSource)
	if err == sql.ErrNoRows {
		return msgRow{}, false
	}
	require.NoError(t, err)
	return m, true
}

func primarySource(t *testing.T, st *store.Store) *store.Source {
	t.Helper()
	src, err := st.GetSourceByIdentifier(testAccount + "/primary")
	require.NoError(t, err)
	return src
}

func countMessages(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), sourceID).Scan(&n))
	return n
}

func bodyText(t *testing.T, st *store.Store, msgID int64) string {
	t.Helper()
	var body sql.NullString
	err := st.DB().QueryRow(
		st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), msgID).Scan(&body)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return body.String
}

func rawFormat(t *testing.T, st *store.Store, msgID int64) string {
	t.Helper()
	var f string
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT raw_format FROM message_raw WHERE message_id = ?`), msgID).Scan(&f))
	return f
}

func recipientEmails(t *testing.T, st *store.Store, msgID int64, typ string) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT p.email_address FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = ?
		ORDER BY p.email_address`), msgID, typ)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var e sql.NullString
		require.NoError(t, rows.Scan(&e))
		out = append(out, e.String)
	}
	require.NoError(t, rows.Err())
	return out
}

func conversationTitle(t *testing.T, st *store.Store, convID int64) string {
	t.Helper()
	var title sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT title FROM conversations WHERE id = ?`), convID).Scan(&title))
	return title.String
}

func parseMeta(t *testing.T, m msgRow) map[string]any {
	t.Helper()
	require.True(t, m.metadata.Valid, "metadata should be set")
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(m.metadata.String), &out))
	return out
}

// timedEvent builds a representative timed event.
func timedEvent(id, summary string, attendees ...gcal.Attendee) gcal.Event {
	return gcal.Event{
		ID:        id,
		Status:    gcal.StatusConfirmed,
		Summary:   summary,
		Location:  "Room 1",
		HTMLLink:  "https://cal/" + id,
		Organizer: gcal.Person{Email: testAccount, DisplayName: "Alice", Self: true},
		Start:     gcal.EventDateTime{DateTime: time.Date(2024, 5, 1, 16, 0, 0, 0, time.UTC), TimeZone: "UTC"},
		End:       gcal.EventDateTime{DateTime: time.Date(2024, 5, 1, 16, 30, 0, 0, time.UTC)},
		Attendees: attendees,
	}
}

func TestFull_PersistsEventsAsMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", Summary: "Personal", AccessRole: "owner", Primary: true, TimeZone: "UTC"}}
	ev := timedEvent("e1", "Sprint standup",
		gcal.Attendee{Email: "bob@example.com", DisplayName: "Bob", ResponseStatus: "accepted"})
	m.FullEvents["primary"] = [][]gcal.Event{{ev}}
	m.FullSyncToken["primary"] = "TOKEN1"

	s, st := newSyncer(t, m, Options{})

	// Pre-existing email contact for Bob — the attendee must dedupe to it.
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)

	res, err := s.Full(context.Background())
	require.NoError(err)
	assert.Equal(1, res.CalendarsSynced)
	assert.Equal(1, res.EventsAdded)

	src := primarySource(t, st)
	require.NotNil(src)
	assert.Equal("TOKEN1", src.SyncCursor.String, "final sync token persisted as cursor")

	row, ok := getMsg(t, st, src.ID, "e1")
	require.True(ok, "event e1 should be persisted")
	assert.Equal(gcal.MessageTypeCalendarEvent, row.mtype)
	assert.Equal("Sprint standup", row.subject.String)
	require.True(row.sentAt.Valid)
	assert.Equal(time.Date(2024, 5, 1, 16, 0, 0, 0, time.UTC), row.sentAt.Time.UTC())
	assert.True(row.isFromMe, "organizer is the account → is_from_me")
	assert.False(row.deletedFromSource.Valid, "must not be soft-deleted")

	meta := parseMeta(t, row)
	assert.Equal("confirmed", meta["status"])
	assert.Equal(false, meta["all_day"])
	assert.Equal("primary", meta["calendar_id"])
	assert.Equal(testAccount, meta["account_email"])
	assert.NotEmpty(meta["end"], "interval end stored in metadata")

	assert.Equal("gcal_json", rawFormat(t, st, row.id))

	// Organizer is 'from'; attendee is 'to' and deduped with the email contact.
	assert.Equal([]string{testAccount}, recipientEmails(t, st, row.id, "from"))
	assert.Equal([]string{"bob@example.com"}, recipientEmails(t, st, row.id, "to"))
	toID := recipientParticipantID(t, st, row.id, "to")
	assert.Equal(bobID, toID, "attendee must reuse the existing email contact participant")

	// Body carries the display name + summary but NOT the raw attendee email.
	body := bodyText(t, st, row.id)
	assert.Contains(body, "Sprint standup")
	assert.Contains(body, "Bob")
	assert.NotContains(body, "bob@example.com", "raw attendee email must not be in body_text")

	// FTS to_addr indexes the raw attendee email (SQLite vtable).
	if st.FTS5Available() && !st.IsPostgreSQL() {
		var toAddr, ftsBody string
		require.NoError(st.DB().QueryRow(
			`SELECT to_addr, body FROM messages_fts WHERE message_id = ?`, row.id).Scan(&toAddr, &ftsBody))
		assert.Contains(toAddr, "bob@example.com", "attendee email reaches FTS via to_addr")
		assert.NotContains(ftsBody, "bob@example.com", "attendee email must not be double-encoded in FTS body")
	}
}

func TestFull_ClearsBodyWhenEventBodyBecomesEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", Summary: "Personal", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{
		{
			ID:          "e1",
			Status:      gcal.StatusConfirmed,
			Summary:     "Planning",
			Description: "Bring notes",
			Organizer:   gcal.Person{Email: testAccount, Self: true},
		},
	}}
	m.FullSyncToken["primary"] = "TOKEN1"

	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)
	row, ok := getMsg(t, st, src.ID, "e1")
	require.True(ok, "event e1 should be persisted")
	assert.Contains(bodyText(t, st, row.id), "Bring notes")

	m.FullEvents["primary"] = [][]gcal.Event{{
		{
			ID:        "e1",
			Status:    gcal.StatusConfirmed,
			Organizer: gcal.Person{Email: testAccount, Self: true},
		},
	}}
	m.FullSyncToken["primary"] = "TOKEN2"

	_, err = s.Full(context.Background())
	require.NoError(err)

	row, ok = getMsg(t, st, src.ID, "e1")
	require.True(ok, "event e1 should still be persisted")
	assert.Empty(bodyText(t, st, row.id), "body_text must be cleared when the event body becomes empty")
	assert.False(row.snippet.Valid, "snippet should clear with an empty event body")
}

func recipientParticipantID(t *testing.T, st *store.Store, msgID int64, typ string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT participant_id FROM message_recipients
		WHERE message_id = ? AND recipient_type = ?`), msgID, typ).Scan(&id))
	return id
}

func TestFull_IdempotentReRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{
		{timedEvent("e1", "One"), timedEvent("e2", "Two")},
	}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})

	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)
	first, _ := getMsg(t, st, src.ID, "e1")
	assert.Equal(2, countMessages(t, st, src.ID))

	// Re-run: no duplicate rows, stable ids.
	_, err = s.Full(context.Background())
	require.NoError(err)
	assert.Equal(2, countMessages(t, st, src.ID), "re-run must not duplicate rows")
	again, _ := getMsg(t, st, src.ID, "e1")
	assert.Equal(first.id, again.id, "message id stable across re-sync")
}

func TestFull_ReusesMixedCaseCalendarSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Meeting")}}
	m.FullSyncToken["primary"] = "T1"
	s, st := newSyncer(t, m, Options{AccountEmail: "Alice.Example@Example.COM"})

	oldSrc, err := st.GetOrCreateSource(gcal.SourceType, "Alice.Example@Example.COM/primary")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(oldSrc.ID,
		`{"account_email":"Alice.Example@Example.COM","calendar_id":"primary"}`))

	res, err := s.Full(context.Background())
	require.NoError(err)
	assert.Equal(1, res.CalendarsSynced)
	assert.Equal(1, res.EventsAdded)

	src, err := st.GetSourceByIdentifier("alice.example@example.com/primary")
	require.NoError(err)
	assert.Equal(oldSrc.ID, src.ID, "full sync must reuse the existing source row")
	assert.Equal("T1", src.SyncCursor.String, "cursor should advance on the reused source")
	assert.Equal(1, countMessages(t, st, src.ID), "event should be stored under the reused source")

	sources, err := st.ListSources(gcal.SourceType)
	require.NoError(err)
	assert.Len(sources, 1, "full sync must not create a duplicate gcal source")
}

func TestFull_AccessRoleFilter(t *testing.T) {
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{
		{ID: "primary", AccessRole: "owner"},
		{ID: "team", AccessRole: "writer"},
		{ID: "holidays", AccessRole: "reader"},
		{ID: "busy", AccessRole: "freeBusyReader"},
	}
	for _, id := range []string{"primary", "team", "holidays", "busy"} {
		m.FullEvents[id] = [][]gcal.Event{{timedEvent("e-"+id, "Ev "+id)}}
		m.FullSyncToken[id] = "T-" + id
	}
	s, st := newSyncer(t, m, Options{})

	assert := assert.New(t)
	require := require.New(t)
	res, err := s.Full(context.Background())
	require.NoError(err)
	assert.Equal(2, res.CalendarsSynced, "default filter keeps owner+writer only")

	_, err = st.GetSourceByIdentifier(testAccount + "/holidays")
	require.ErrorIs(err, store.ErrSourceNotFound, "reader calendar must be skipped")
	_, err = st.GetSourceByIdentifier(testAccount + "/team")
	assert.NoError(err, "writer calendar must be synced")
}

func TestFull_InvalidMinAccessRoleRejected(t *testing.T) {
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	s, _ := newSyncer(t, m, Options{MinAccessRole: "wrtier"})

	_, err := s.Full(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min access role")
}

func TestRegisterCalendars_InvalidMinAccessRoleRejected(t *testing.T) {
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	s, _ := newSyncer(t, m, Options{MinAccessRole: "wrtier"})

	_, err := s.RegisterCalendars(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid min access role")
}

func TestIncremental_CancellationRetainsRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Lunch")}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)
	before, ok := getMsg(t, st, src.ID, "e1")
	require.True(ok)
	assert.Equal("Lunch", before.subject.String)

	// Incremental delta cancels e1 (delta carries only id + status, no summary).
	m.IncEvents["T1"] = [][]gcal.Event{{{ID: "e1", Status: gcal.StatusCancelled}}}
	m.IncNextToken["T1"] = "T2"

	res, err := s.Incremental(context.Background())
	require.NoError(err)
	assert.Equal(1, res.EventsCancelled)

	after, ok := getMsg(t, st, src.ID, "e1")
	require.True(ok, "cancelled event must be RETAINED, not deleted")
	assert.False(after.deletedFromSource.Valid, "deleted_from_source_at must stay NULL")
	assert.Equal("Lunch", after.subject.String, "original subject preserved (not wiped by empty delta)")
	assert.Equal("cancelled", parseMeta(t, after)["status"], "metadata.status flipped to cancelled")

	src2 := primarySource(t, st)
	assert.Equal("T2", src2.SyncCursor.String, "cursor advanced after incremental")
}

func TestIncremental_PersistsExplicitOAuthAppBinding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		initialApp sql.NullString
		requestApp string
		wantApp    string
		wantAppSet bool
		deltaID    string
		nextToken  string
	}{
		{
			name:       "sets named app",
			requestApp: "new-app",
			wantApp:    "new-app",
			wantAppSet: true,
			deltaID:    "named-delta",
			nextToken:  "T2",
		},
		{
			name:       "clears to default app",
			initialApp: sql.NullString{String: "old-app", Valid: true},
			requestApp: "",
			wantAppSet: false,
			deltaID:    "clear-delta",
			nextToken:  "T3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			m := gcal.NewMockAPI()
			m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
			m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Initial")}}
			m.FullSyncToken["primary"] = "T1"

			s, st := newSyncer(t, m, Options{})
			_, err := s.Full(context.Background())
			require.NoError(err)
			src := primarySource(t, st)
			if tc.initialApp.Valid {
				require.NoError(st.UpdateSourceOAuthApp(src.ID, tc.initialApp))
			}

			m.IncEvents["T1"] = [][]gcal.Event{{timedEvent(tc.deltaID, "Delta")}}
			m.IncNextToken["T1"] = tc.nextToken

			incremental := New(m, st, Options{
				AccountEmail: testAccount,
				OAuthApp:     tc.requestApp,
				OAuthAppSet:  true,
			}).WithLogger(quietLogger())

			res, err := incremental.Incremental(context.Background())
			require.NoError(err)
			assert.Equal(1, res.CalendarsSynced)

			src, err = st.GetSourceByIdentifier(testAccount + "/primary")
			require.NoError(err)
			assert.Equal(tc.nextToken, src.SyncCursor.String)
			assert.Equal(tc.wantAppSet, src.OAuthApp.Valid)
			assert.Equal(tc.wantApp, src.OAuthApp.String)
		})
	}
}

func TestIncremental_PersistsExplicitOAuthAppBindingBeforeSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requestApp string
		wantApp    string
		wantAppSet bool
	}{
		{
			name:       "sets named app",
			requestApp: "new-app",
			wantApp:    "new-app",
			wantAppSet: true,
		},
		{
			name:       "clears to default app",
			requestApp: "",
			wantAppSet: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			m := gcal.NewMockAPI()
			m.Calendars = []gcal.Calendar{
				{ID: "primary", AccessRole: "owner"},
				{ID: "holidays", AccessRole: "reader"},
			}
			m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Initial")}}
			m.FullEvents["holidays"] = [][]gcal.Event{{timedEvent("h1", "Holiday")}}
			m.FullSyncToken["primary"] = "P1"
			m.FullSyncToken["holidays"] = "H1"

			s, st := newSyncer(t, m, Options{AllCalendars: true})
			_, err := s.Full(context.Background())
			require.NoError(err)

			primary, err := st.GetSourceByIdentifier(testAccount + "/primary")
			require.NoError(err)
			holidays, err := st.GetSourceByIdentifier(testAccount + "/holidays")
			require.NoError(err)
			require.NoError(st.UpdateSourceOAuthApp(primary.ID, sql.NullString{String: "old-app", Valid: true}))
			require.NoError(st.UpdateSourceOAuthApp(holidays.ID, sql.NullString{String: "old-app", Valid: true}))

			m.IncEvents["P1"] = [][]gcal.Event{{timedEvent("e2", "Primary delta")}}
			m.IncNextToken["P1"] = "P2"

			incremental := New(m, st, Options{
				AccountEmail: testAccount,
				OAuthApp:     tc.requestApp,
				OAuthAppSet:  true,
				Calendars:    []string{"primary"},
			}).WithLogger(quietLogger())

			res, err := incremental.Incremental(context.Background())
			require.NoError(err)
			assert.Equal(1, res.CalendarsSynced)

			primary, err = st.GetSourceByIdentifier(testAccount + "/primary")
			require.NoError(err)
			holidays, err = st.GetSourceByIdentifier(testAccount + "/holidays")
			require.NoError(err)
			assert.Equal(tc.wantAppSet, primary.OAuthApp.Valid, "selected source binding")
			assert.Equal(tc.wantApp, primary.OAuthApp.String, "selected source binding")
			assert.Equal(tc.wantAppSet, holidays.OAuthApp.Valid, "skipped source binding")
			assert.Equal(tc.wantApp, holidays.OAuthApp.String, "skipped source binding")
			assert.Equal("H1", holidays.SyncCursor.String, "skipped calendar must not sync")
		})
	}
}

func TestRegisterCalendars_PersistsExplicitOAuthAppBindingBeforeSelection(t *testing.T) {
	m, st := seedCalendarOAuthBindingFixture(t)

	register := New(m, st, Options{
		AccountEmail: testAccount,
		OAuthApp:     "new-app",
		OAuthAppSet:  true,
		Calendars:    []string{"primary"},
	}).WithLogger(quietLogger())

	registered, err := register.RegisterCalendars(context.Background())
	require.NoError(t, err)
	require.Len(t, registered, 1)

	assertCalendarOAuthApp(t, st, "primary", sql.NullString{String: "new-app", Valid: true})
	assertCalendarOAuthApp(t, st, "holidays", sql.NullString{String: "new-app", Valid: true})
}

func TestFull_PersistsExplicitOAuthAppBindingBeforeSelection(t *testing.T) {
	m, st := seedCalendarOAuthBindingFixture(t)
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e2", "Primary resync")}}
	m.FullSyncToken["primary"] = "P2"

	full := New(m, st, Options{
		AccountEmail: testAccount,
		OAuthApp:     "",
		OAuthAppSet:  true,
		Calendars:    []string{"primary"},
	}).WithLogger(quietLogger())

	res, err := full.Full(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.CalendarsSynced)

	assertCalendarOAuthApp(t, st, "primary", sql.NullString{})
	assertCalendarOAuthApp(t, st, "holidays", sql.NullString{})
}

func seedCalendarOAuthBindingFixture(t *testing.T) (*gcal.MockAPI, *store.Store) {
	t.Helper()
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{
		{ID: "primary", AccessRole: "owner"},
		{ID: "holidays", AccessRole: "reader"},
	}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Initial")}}
	m.FullEvents["holidays"] = [][]gcal.Event{{timedEvent("h1", "Holiday")}}
	m.FullSyncToken["primary"] = "P1"
	m.FullSyncToken["holidays"] = "H1"

	s, st := newSyncer(t, m, Options{AllCalendars: true})
	_, err := s.Full(context.Background())
	require.NoError(t, err)

	primary, err := st.GetSourceByIdentifier(testAccount + "/primary")
	require.NoError(t, err)
	holidays, err := st.GetSourceByIdentifier(testAccount + "/holidays")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceOAuthApp(primary.ID, sql.NullString{String: "old-app", Valid: true}))
	require.NoError(t, st.UpdateSourceOAuthApp(holidays.ID, sql.NullString{String: "old-app", Valid: true}))

	return m, st
}

func assertCalendarOAuthApp(t *testing.T, st *store.Store, calendarID string, want sql.NullString) {
	t.Helper()
	src, err := st.GetSourceByIdentifier(testAccount + "/" + calendarID)
	require.NoError(t, err)
	assert.Equal(t, want.Valid, src.OAuthApp.Valid, "oauth_app validity for %s", calendarID)
	assert.Equal(t, want.String, src.OAuthApp.String, "oauth_app value for %s", calendarID)
}

func TestIncremental_410TriggersFullResync(t *testing.T) {
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "First")}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	assert := assert.New(t)
	require := require.New(t)
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)
	require.Equal("T1", src.SyncCursor.String)

	// The stored token is now stale (410), and a fresh full listing has a new event.
	m.GoneTokens["T1"] = true
	m.FullEvents["primary"] = [][]gcal.Event{{
		timedEvent("e1", "First"),
		timedEvent("e2", "Second"),
	}}
	m.FullSyncToken["primary"] = "T2"

	res, err := s.Incremental(context.Background())
	require.NoError(err, "410 should self-heal into a full resync, not error out")
	assert.GreaterOrEqual(res.CalendarsSynced, 1)

	assert.Equal(2, countMessages(t, st, src.ID), "full resync repopulated after 410")
	src2 := primarySource(t, st)
	assert.Equal("T2", src2.SyncCursor.String, "cursor replaced with the fresh token")
}

func TestFull_RecurringMasterAndCancelledOccurrence(t *testing.T) {
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}

	master := gcal.Event{
		ID:         "r1",
		Status:     gcal.StatusConfirmed,
		Summary:    "Weekly sync",
		Organizer:  gcal.Person{Email: testAccount, Self: true},
		Start:      gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 10, 0, 0, 0, time.UTC)},
		End:        gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 10, 30, 0, 0, time.UTC)},
		Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=TH"},
	}
	// A single cancelled occurrence: same series, a specific original start.
	cancelledOcc := gcal.Event{
		ID:                "r1_20240509T100000Z",
		Status:            gcal.StatusCancelled,
		RecurringEventID:  "r1",
		OriginalStartTime: gcal.EventDateTime{DateTime: time.Date(2024, 5, 9, 10, 0, 0, 0, time.UTC)},
	}
	m.FullEvents["primary"] = [][]gcal.Event{{master, cancelledOcc}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	assert := assert.New(t)
	require := require.New(t)
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)

	// Master stored under event.id with its recurrence in metadata.
	masterRow, ok := getMsg(t, st, src.ID, "r1")
	require.True(ok)
	mMeta := parseMeta(t, masterRow)
	rec, _ := mMeta["recurrence"].([]any)
	require.Len(rec, 1)
	assert.Equal("RRULE:FREQ=WEEKLY;BYDAY=TH", rec[0])
	assert.Equal("confirmed", mMeta["status"], "master is not affected by the occurrence cancellation")

	// Cancelled occurrence keyed recurringEventId|originalStartTime, flagged only itself.
	occRow, ok := getMsg(t, st, src.ID, "r1|2024-05-09T10:00:00Z")
	require.True(ok, "cancelled occurrence stored under its derived key")
	assert.Equal("cancelled", parseMeta(t, occRow)["status"])
	assert.NotEqual(masterRow.id, occRow.id, "occurrence is its own row")
	require.True(occRow.sentAt.Valid)
	assert.Equal(time.Date(2024, 5, 9, 10, 0, 0, 0, time.UTC), occRow.sentAt.Time.UTC(),
		"occurrence sorts at its original start time")
}

type captureEnqueuer struct{ ids []int64 }

func (c *captureEnqueuer) EnqueueMessages(_ context.Context, ids []int64) error {
	c.ids = append(c.ids, ids...)
	return nil
}

func TestFull_EnqueuesForEmbedding(t *testing.T) {
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{
		timedEvent("e1", "One"), timedEvent("e2", "Two"),
	}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	enq := &captureEnqueuer{}
	s.SetEmbedEnqueuer(enq)

	_, err := s.Full(context.Background())
	require.NoError(t, err)
	assert.Len(t, enq.ids, 2, "both events queued for embedding")

	src := primarySource(t, st)
	e1, _ := getMsg(t, st, src.ID, "e1")
	assert.Contains(t, enq.ids, e1.id)
}
