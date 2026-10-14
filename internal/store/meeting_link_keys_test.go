package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func linkKeyValues(t *testing.T, st *Store, id int64) []string {
	t.Helper()
	rows, err := st.db.Query(`SELECT kind || ':' || value FROM meeting_link_keys WHERE message_id = ? ORDER BY ordinal`, id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, rows.Err())
	return keys
}

func TestMetadataOnlyKeyChangeRelinks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	other, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("other", "other@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, id := projectionFixture(t, st, "keys", "granola_json", `{"summary_text":"same"}`)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"platform":"granola","calendar_event_id":"series_20270314T100000Z","scheduled_start":"2027-03-14T10:00:00Z"}`, Valid: true}))
	assertions.Equal([]string{"google_event_id:series_20270314T100000Z"}, linkKeyValues(t, st, id))
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), id)))
	_, before := readProjection(t, st, id)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"platform":"granola","calendar_event_id":"other"}`, Valid: true}))
	assertions.Equal([]string{"google_event_id:other"}, linkKeyValues(t, st, id))
	assertions.Equal([]OccurrenceRef{{MessageID: other}}, linkTargets(memberLinks(t, st, OwnerScope(), id)))
	_, after := readProjection(t, st, id)
	assertions.Equal(before, after)
}

func TestKeyRemovalUnlinks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	target, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("event", "event@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, id := projectionFixture(t, st, "remove", "granola_json", `{"calendar_event":{"calendar_event_id":"event"}}`)
	requirements.NotEmpty(linkKeyValues(t, st, id))
	assertions.Equal([]OccurrenceRef{{MessageID: target}}, linkTargets(memberLinks(t, st, OwnerScope(), id)))
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"summary_text":"same"}`), "granola_json"))
	assertions.Empty(linkKeyValues(t, st, id))
	assertions.Empty(memberLinks(t, st, OwnerScope(), id))
}

func TestMessageTypeChangeDropsKeys(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	data, id := projectionFixture(t, st, "type", "granola_json", `{"calendar_event":{"calendar_event_id":"event"}}`)
	require.NotEmpty(t, linkKeyValues(t, st, id))
	data.Message.MessageType = "email"
	_, err := st.UpsertMessage(data.Message)
	require.NoError(t, err)
	assert.Empty(t, linkKeyValues(t, st, id))
}

func TestMuesliOpaqueIDNeverLinks(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "opaque", "muesli_json", `{"calendar_event_id":"opaque","calendar_series_id":"opaque"}`)
	require.NoError(t, st.SetMessageMetadata(id, sql.NullString{String: `{"platform":"muesli","calendar_event_id":"opaque","calendar_source":"google"}`, Valid: true}))
	assert.Empty(t, linkKeyValues(t, st, id))
}

func TestLegacyCirclebackAndGranolaKeysBackfill(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "legacy", "granola_json", `{}`)
	_, err := st.db.Exec(`UPDATE messages SET metadata = ? WHERE id = ?`, `{"platform":"granola","calendar_event_id":"series_20270314"}`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM meeting_link_key_state WHERE message_id = ?`, id)
	requirements.NoError(err)
	_, circle := projectionFixture(t, st, "legacy-circle", "circleback_json", `{}`)
	_, err = st.db.Exec(`UPDATE messages SET metadata = ? WHERE id = ?`, `{"platform":"circleback","meeting_url":"https://meet.google.com/abc-defg-hij","scheduled_start":"2027-03-14T10:00:00Z"}`, circle)
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM meeting_link_key_state WHERE message_id = ?`, circle)
	requirements.NoError(err)
	requirements.NoError(st.backfillMeetingLinkKeysContext(context.Background()))
	assertions.Equal([]string{"join_key:meet:abc-defg-hij"}, linkKeyValues(t, st, circle))
	assertions.Equal([]string{"google_event_id:series_20270314"}, linkKeyValues(t, st, id))
}

func TestCirclebackKeyProjectionV3BackfillsSupportedURLAndSchedule(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "circleback-v2", "circleback_json", `{"meeting":{"url":"https://unsupported.example/room","meetingUrl":"https://meet.google.com/abc-defg-hij","startTime":"not-a-time","date":"2027-03-14T10:00:00Z"}}`)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"platform":"circleback","meeting_url":"https://unsupported.example/room","start":"not-a-time"}`, Valid: true}))

	// Recreate a v2 row that lacked a supported key so InitSchema must rebuild it.
	_, err := st.db.Exec(`DELETE FROM meeting_link_keys WHERE message_id = ?`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_link_key_state SET key_version = 2 WHERE message_id = ?`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE applied_migrations SET version = 2 WHERE name = ?`, migrationMeetingLinkKeysV1)
	requirements.NoError(err)

	requirements.NoError(st.InitSchemaContext(t.Context()))
	var scheduledAt sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT scheduled_at FROM meeting_link_keys WHERE message_id = ?`, id).Scan(&scheduledAt))
	requirements.True(scheduledAt.Valid)
	assertions.Equal([]string{"join_key:meet:abc-defg-hij"}, linkKeyValues(t, st, id))
	wantSchedule := time.Date(2027, time.March, 14, 10, 0, 0, 0, time.UTC)
	assertions.True(scheduledAt.Time.Equal(wantSchedule), "schedule should preserve the same instant across backend location representations")
	var keyVersion, migrationVersion int
	requirements.NoError(st.db.QueryRow(`SELECT key_version FROM meeting_link_key_state WHERE message_id = ?`, id).Scan(&keyVersion))
	requirements.NoError(st.db.QueryRow(`SELECT version FROM applied_migrations WHERE name = ?`, migrationMeetingLinkKeysV1).Scan(&migrationVersion))
	assertions.Equal(3, keyVersion)
	assertions.Equal(3, migrationVersion)
	version, err := st.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(SchemaVersion, version)
}

func TestKeyBackfillTwiceNoWrites(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "no-writes", "granola_json", `{"calendar_event":{"calendar_event_id":"event"}}`)
	requirements.NoError(st.backfillMeetingLinkKeysContext(context.Background()))
	if st.IsPostgreSQL() {
		_, err := st.db.Exec(`CREATE FUNCTION reject_link_keys() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unexpected key write'; END $$`)
		requirements.NoError(err)
		_, err = st.db.Exec(`CREATE TRIGGER reject_link_keys BEFORE INSERT OR UPDATE OR DELETE ON meeting_link_keys FOR EACH ROW EXECUTE FUNCTION reject_link_keys()`)
		requirements.NoError(err)
	} else {
		_, err := st.db.Exec(`CREATE TRIGGER reject_link_keys BEFORE INSERT ON meeting_link_keys BEGIN SELECT RAISE(ABORT, 'unexpected key write'); END`)
		requirements.NoError(err)
	}
	requirements.NoError(st.backfillMeetingLinkKeysContext(context.Background()))
	assert.Equal(t, []string{"google_event_id:event"}, linkKeyValues(t, st, id))
}

func TestKeyBackfillDoesNotRewriteContent(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "keys-only", "granola_json", `{"calendar_event":{"calendar_event_id":"event"}}`)
	_, err := st.db.Exec(`DELETE FROM meeting_link_key_state WHERE message_id = ?`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_details SET projection_version = 1 WHERE message_id = ?`, id)
	requirements.NoError(err)
	rejectMeetingProjectionWrites(t, st)
	requirements.NoError(st.backfillMeetingLinkKeysContext(context.Background()))
	assert.Equal(t, []string{"google_event_id:event"}, linkKeyValues(t, st, id))
}

func TestMeetingLinkKeysTwentyScheduledNotRecording(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "twenty-key", "twenty_json", `{"calendar_event":{"iCalUid":"meeting@example.org","startsAt":"2027-03-14T10:00:00Z"},"recording":{"startedAt":"2027-03-14T10:05:00Z"}}`)
	assertions.Equal([]string{"ical_uid:meeting@example.org"}, linkKeyValues(t, st, id))
	var at sql.NullTime
	requirements.NoError(st.db.QueryRow(`SELECT scheduled_at FROM meeting_link_keys WHERE message_id = ?`, id).Scan(&at))
	requirements.True(at.Valid)
	assertions.Equal("2027-03-14T10:00:00Z", at.Time.UTC().Format(time.RFC3339))
}

func TestCirclebackLinkKeysRawAndLegacyMetadata(t *testing.T) {
	for _, tc := range []struct{ name, raw, metadata, want string }{
		{"raw", `{"meeting":{"meetingUrl":"https://meet.google.com/ABC-defg-hij","startTime":"2027-03-14T10:00:00Z"}}`, "", "join_key:meet:abc-defg-hij"},
		{"legacy", `{}`, `{"platform":"circleback","meeting_url":"https://team.zoom.us/j/12345?pwd=example","scheduled_start":"2027-03-14T10:00:00Z"}`, "join_key:zoom:12345"},
		{"join-url", `{}`, `{"platform":"circleback","join_url":"https://meet.google.com/abc-defg-hij"}`, "join_key:meet:abc-defg-hij"},
		{"lookalike", `{"meeting":{"meetingUrl":"https://evilzoom.us/j/12345"}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			_, id := projectionFixture(t, st, "circle-key", "circleback_json", tc.raw)
			if tc.metadata != "" {
				require.NoError(t, st.SetMessageMetadata(id, sql.NullString{String: tc.metadata, Valid: true}))
			}
			if tc.want == "" {
				assert.Empty(t, linkKeyValues(t, st, id))
			} else {
				assert.Equal(t, []string{tc.want}, linkKeyValues(t, st, id))
			}
		})
	}
}

func TestCirclebackUsesFirstSupportedJoinURL(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	targetRaw := `{"id":"google-meeting","iCalUID":"meeting@example.org","hangoutLink":"https://meet.google.com/abc-defg-hij","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	target, _ := calendarLinkFixture(t, st, "primary@example.com", targetRaw)
	_, note := projectionFixture(t, st, "circleback-url-fallback", "circleback_json", `{"meeting":{"url":"https://unsupported.example/room","meetingUrl":"https://meet.google.com/abc-defg-hij","date":"2027-03-14T10:00:00Z"}}`)
	requirements.NoError(st.SetMessageMetadata(note, sql.NullString{String: `{"platform":"circleback","meeting_url":"https://unsupported.example/room","scheduled_start":"2027-03-14T10:00:00Z"}`, Valid: true}))

	assertions.Equal([]string{"join_key:meet:abc-defg-hij"}, linkKeyValues(t, st, note))
	assertions.Equal([]OccurrenceRef{{MessageID: target}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}
