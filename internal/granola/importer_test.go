package granola

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// fakeAPI serves ListNotes from the notes it holds and GetNote from raw
// fixture bytes. Notes can be swapped mid-test to simulate server-side edits.
type fakeAPI struct {
	mu                  sync.Mutex
	notes               map[string][]byte // note ID -> full note JSON
	fullNotes           map[string][]byte // optional GET response override by listed note ID
	fail                map[string]bool   // note ID -> serve 404
	orderedIDs          []string
	respectUpdatedAfter bool
	respectCreatedAfter bool
	hasMore             bool
	cursor              string
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/notes" {
			summaries := make([]json.RawMessage, 0, len(f.notes))
			ids := f.orderedIDs
			if len(ids) == 0 {
				ids = make([]string, 0, len(f.notes))
				for id := range f.notes {
					ids = append(ids, id)
				}
			}
			var updatedAfter time.Time
			if raw := r.URL.Query().Get("updated_after"); raw != "" {
				var err error
				updatedAfter, err = time.Parse(time.RFC3339, raw)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			var createdAfter time.Time
			if raw := r.URL.Query().Get("created_after"); raw != "" {
				var err error
				createdAfter, err = time.Parse(time.RFC3339, raw)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			for _, id := range ids {
				raw := f.notes[id]
				if (f.respectUpdatedAfter && !updatedAfter.IsZero()) ||
					(f.respectCreatedAfter && !createdAfter.IsZero()) {
					var note NoteSummary
					if err := json.Unmarshal(raw, &note); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					if !note.UpdatedAt.After(updatedAfter) {
						continue
					}
					if f.respectCreatedAfter && !createdAfter.IsZero() && !note.CreatedAt.After(createdAfter) {
						continue
					}
				}
				summaries = append(summaries, raw)
			}
			resp, _ := json.Marshal(map[string]any{"notes": summaries, "hasMore": f.hasMore, "cursor": f.cursor})
			_, _ = w.Write(resp)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/notes/")
		if f.fail[id] {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		raw, ok := f.notes[id]
		if override, exists := f.fullNotes[id]; exists {
			raw, ok = override, true
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(raw)
	})
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return b
}

func noteFixtureAt(t *testing.T, id string, updatedAt time.Time) []byte {
	t.Helper()
	var note map[string]any
	require.NoError(t, json.Unmarshal(loadFixture(t, "note_no_calendar.json"), &note))
	note["id"] = id
	note["updated_at"] = updatedAt.UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(note)
	require.NoError(t, err)
	return b
}

func newTestImporter(t *testing.T, api *fakeAPI) (*Importer, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	st := testutil.NewTestStore(t)
	return NewImporter(st, NewClient(srv.URL, "grn_testkey")), st
}

func TestImport_RoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := &fakeAPI{notes: map[string][]byte{
		"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json"),
		"not_Zz98Yy87Xx76Wv": loadFixture(t, "note_no_calendar.json"),
	}}
	imp, st := newTestImporter(t, api)

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "alice@example.com",
		AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(2, sum.NotesProcessed)
	assert.EqualValues(2, sum.NotesAdded)
	assert.EqualValues(0, sum.Errors)

	// Message row: type, subject, sent_at from the scheduled start, organizer
	// bob is not the account holder.
	var subject, sentAt string
	var fromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT subject, sent_at, is_from_me FROM messages WHERE source_message_id = ?`),
		"not_Ab12Cd34Ef56Gh").Scan(&subject, &sentAt, &fromMe))
	assert.Equal("Quarterly Planning Review", subject)
	assert.Contains(sentAt, "2026-06-01")
	assert.Contains(sentAt, "15:00:00")
	assert.False(fromMe, "organizer bob is not the account identifier")

	// The ad-hoc note: title fallback, owner alice IS the account holder.
	var subject2 string
	var fromMe2 bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT subject, is_from_me FROM messages WHERE source_message_id = ?`),
		"not_Zz98Yy87Xx76Wv").Scan(&subject2, &fromMe2))
	assert.Equal("Meeting on 2026-06-02", subject2)
	assert.True(fromMe2, "note owner alice is the account identifier")

	// Body carries the summary markdown and offset-stamped transcript lines.
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?`), "not_Ab12Cd34Ef56Gh").Scan(&body))
	assert.Contains(body, "Agreed on **three priorities**")
	assert.Contains(body, "[00:00] Alice Smith: Let's get started with the quarterly review.")
	assert.Contains(body, "[01:11] Bob Jones: Sounds good. I have the budget numbers ready.")
	assert.Contains(body, "[1:01:32] Speaker C: The deadline for phase one is July fifteenth.")
	assert.Contains(body, "Attendees: Alice Smith, Bob Jones")
	assert.NotContains(body, "carol@example.com", "raw attendee emails stay out of the body")

	// Conversation: one per meeting, type "meeting".
	var convType, convTitle string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT c.conversation_type, c.title FROM conversations c
		JOIN messages m ON m.conversation_id = c.id
		WHERE m.source_message_id = ?`), "not_Ab12Cd34Ef56Gh").Scan(&convType, &convTitle))
	assert.Equal("meeting", convType)
	assert.Equal("Quarterly Planning Review", convTitle)

	// Recipients: from = organizer bob, to = all three attendees.
	var fromEmail string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT p.email_address FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ? AND mr.recipient_type = 'from'`),
		"not_Ab12Cd34Ef56Gh").Scan(&fromEmail))
	assert.Equal("bob@example.com", fromEmail)

	var toCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_message_id = ? AND mr.recipient_type = 'to'`),
		"not_Ab12Cd34Ef56Gh").Scan(&toCount))
	assert.Equal(3, toCount)

	// Metadata JSON.
	var msgID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "not_Ab12Cd34Ef56Gh").Scan(&msgID))
	metaNS, err := st.GetMessageMetadata(msgID)
	require.NoError(err)
	require.True(metaNS.Valid)
	var meta meetingMetadata
	require.NoError(json.Unmarshal([]byte(metaNS.String), &meta))
	assert.Equal("granola", meta.Platform)
	assert.Equal("not_Ab12Cd34Ef56Gh", meta.NoteID)
	assert.Equal("bob@example.com", meta.OrganizerEmail)
	assert.EqualValues(3600, meta.DurationSeconds)
	assert.Equal([]string{"Planning"}, meta.Folders)
	assert.Equal(3, meta.SegmentCount)

	// Raw archive preserves the verbatim response.
	raw, err := st.GetMessageRaw(msgID)
	require.NoError(err)
	assert.JSONEq(string(api.notes["not_Ab12Cd34Ef56Gh"]), string(raw))
	var rawFormat string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT raw_format FROM message_raw WHERE message_id = ?`), msgID).Scan(&rawFormat))
	assert.Equal(RawFormat, rawFormat)
}

func TestImport_TranscriptTimestampFallbacksRemainSearchable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdAt := time.Date(2026, 7, 14, 15, 0, 0, 0, time.UTC)
	firstTranscriptAt := createdAt.Add(2 * time.Minute)

	notes := []*Note{
		{
			NoteSummary: NoteSummary{
				ID: "sparse-transcript-times", Title: "Sparse transcript times",
				Owner:     User{Name: "Test User", Email: "user@example.com"},
				CreatedAt: createdAt, UpdatedAt: createdAt,
			},
			Transcript: []TranscriptSegment{
				{Speaker: Speaker{Name: "Untimed"}, Text: "No timestamp"},
				{Speaker: Speaker{Name: "Timed"}, Text: "First timestamp", StartTime: firstTranscriptAt},
				{Speaker: Speaker{Name: "Later"}, Text: "Thirty seconds later", StartTime: firstTranscriptAt.Add(30 * time.Second), EndTime: firstTranscriptAt.Add(45 * time.Second)},
			},
		},
		{
			NoteSummary: NoteSummary{
				ID: "missing-transcript-times", Title: "Missing transcript times",
				Owner:     User{Name: "Test User", Email: "user@example.com"},
				CreatedAt: createdAt.Add(time.Hour), UpdatedAt: createdAt.Add(time.Hour),
			},
			Transcript: []TranscriptSegment{{
				Speaker: Speaker{Name: "Untimed"}, Text: "Created-at fallback",
			}},
		},
	}
	api := &fakeAPI{notes: make(map[string][]byte, len(notes))}
	for _, note := range notes {
		raw, err := json.Marshal(note)
		require.NoError(err)
		api.notes[note.ID] = raw
	}
	imp, st := newTestImporter(t, api)

	_, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)

	var sparseSentAt time.Time
	var sparseBody, sparseMetadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.sent_at, mb.body_text, m.metadata
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_message_id = ?`), "sparse-transcript-times").Scan(
		&sparseSentAt, &sparseBody, &sparseMetadata,
	))
	assert.Equal(firstTranscriptAt, sparseSentAt.UTC())
	assert.Contains(sparseBody, "[00:00] Untimed: No timestamp")
	assert.Contains(sparseBody, "[00:00] Timed: First timestamp")
	assert.Contains(sparseBody, "[00:30] Later: Thirty seconds later")
	var sparseMeta meetingMetadata
	require.NoError(json.Unmarshal([]byte(sparseMetadata), &sparseMeta))
	assert.EqualValues(45, sparseMeta.DurationSeconds)

	var fallbackSentAt time.Time
	var fallbackBody string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.sent_at, mb.body_text
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_message_id = ?`), "missing-transcript-times").Scan(
		&fallbackSentAt, &fallbackBody,
	))
	assert.Equal(createdAt.Add(time.Hour), fallbackSentAt.UTC())
	assert.Contains(fallbackBody, "[00:00] Untimed: Created-at fallback")
}

func TestImport_RejectsFetchedNoteWithInvalidID(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fullNote []byte
	}{
		{
			name: "missing ID",
			fullNote: []byte(`{
				"title":"Missing ID","created_at":"2026-07-14T10:00:00Z",
				"updated_at":"2026-07-14T11:00:00Z"
			}`),
		},
		{
			name: "mismatched ID",
			fullNote: []byte(`{
				"id":"different-note","title":"Mismatched ID",
				"created_at":"2026-07-14T10:00:00Z","updated_at":"2026-07-14T11:00:00Z"
			}`),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			api := &fakeAPI{
				notes: map[string][]byte{
					"listed-note": []byte(`{
						"id":"listed-note","title":"Listed note",
						"created_at":"2026-07-14T10:00:00Z","updated_at":"2026-07-14T11:00:00Z"
					}`),
				},
				fullNotes:  map[string][]byte{"listed-note": tt.fullNote},
				orderedIDs: []string{"listed-note"},
			}
			imp, st := newTestImporter(t, api)

			sum, err := imp.Import(context.Background(), ImportOptions{
				Identifier: "work", AccountEmail: "user@example.com",
			})

			require.ErrorContains(err, "partial Granola sync")
			assert.EqualValues(1, sum.NotesProcessed)
			assert.EqualValues(1, sum.Errors)
			assert.Zero(sum.NotesAdded)
			assert.Zero(sum.NotesUpdated)
			var messageCount, completedRunCount int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
			require.NoError(st.DB().QueryRow(
				`SELECT COUNT(*) FROM sync_runs WHERE status = 'completed'`,
			).Scan(&completedRunCount))
			assert.Zero(messageCount, "invalid response must not be archived")
			assert.Zero(completedRunCount, "invalid response must not advance a successful cursor")
		})
	}
}

func TestImport_AccountIdentityControlsFromMe(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	noteFor := func(id, organizerEmail string) []byte {
		var payload map[string]any
		require.NoError(json.Unmarshal(loadFixture(t, "note_no_calendar.json"), &payload))
		payload["id"] = id
		payload["owner"] = map[string]any{"name": "Test Organizer", "email": organizerEmail}
		raw, err := json.Marshal(payload)
		require.NoError(err)
		return raw
	}
	api := &fakeAPI{
		notes: map[string][]byte{
			"note-primary": noteFor("note-primary", "USER-A@EXAMPLE.COM"),
			"note-alias":   noteFor("note-alias", "user-b@example.com"),
			"note-other":   noteFor("note-other", "user-c@example.com"),
		},
		orderedIDs: []string{"note-primary", "note-alias", "note-other"},
	}
	imp, st := newTestImporter(t, api)
	source, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, " User-B@Example.COM ", "manual"))

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "work",
		AccountEmail: " user-a@example.com ",
	})
	require.NoError(err)
	assert.Equal(source.ID, sum.SourceID)

	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: "note-primary", want: true},
		{id: "note-alias", want: true},
		{id: "note-other", want: false},
	} {
		var got bool
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT is_from_me FROM messages WHERE source_id = ? AND source_message_id = ?`),
			source.ID, tc.id).Scan(&got))
		assert.Equal(tc.want, got, tc.id)
	}

	var msgID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		source.ID, "note-primary").Scan(&msgID))
	metaJSON, err := st.GetMessageMetadata(msgID)
	require.NoError(err)
	require.True(metaJSON.Valid)
	var meta meetingMetadata
	require.NoError(json.Unmarshal([]byte(metaJSON.String), &meta))
	assert.Equal("work", meta.AccountID, "metadata preserves the source label")
}

func TestImport_NormalizesSentAtToUTC(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	raw := strings.Replace(
		string(loadFixture(t, "note_full.json")),
		`"scheduled_start_time": "2026-06-01T15:00:00Z"`,
		`"scheduled_start_time": "2026-06-01T15:00:00-05:00"`,
		1,
	)
	api := &fakeAPI{notes: map[string][]byte{"not_Ab12Cd34Ef56Gh": []byte(raw)}}
	imp, st := newTestImporter(t, api)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	var sentAt string
	require.NoError(st.DB().QueryRow(`SELECT CAST(sent_at AS TEXT) FROM messages`).Scan(&sentAt))
	assert.Contains(sentAt, "2026-06-01 20:00:00")
	assert.NotContains(sentAt, "-05:00")
}

func TestImport_IdempotentAndRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := &fakeAPI{notes: map[string][]byte{
		"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json"),
	}}
	imp, st := newTestImporter(t, api)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)

	// Second run: the boundary overlap sees the same note but recognizes that
	// its ID was already covered at that exact updated_at timestamp.
	sum2, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, sum2.NotesAdded, "re-import must not add rows")
	assert.EqualValues(0, sum2.NotesUpdated, "unchanged cursor-boundary notes are no-ops")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)

	// Server-side edit with a newer updated_at: row refreshes in place.
	edited := strings.ReplaceAll(string(api.notes["not_Ab12Cd34Ef56Gh"]),
		"Quarterly Planning Review", "Quarterly Planning Review v2")
	edited = strings.Replace(edited, "2026-06-01T16:45:00Z", "2026-06-03T10:00:00Z", 1)
	var editedPayload map[string]any
	require.NoError(json.Unmarshal([]byte(edited), &editedPayload))
	editedPayload["attendees"] = []map[string]any{{"name": "Alice Smith", "email": "alice@example.com"}}
	editedBytes, err := json.Marshal(editedPayload)
	require.NoError(err)
	api.mu.Lock()
	api.notes["not_Ab12Cd34Ef56Gh"] = editedBytes
	api.mu.Unlock()

	sum3, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, sum3.NotesAdded)
	assert.EqualValues(1, sum3.NotesUpdated)
	latest, err := st.GetLatestSync(sum3.SourceID)
	require.NoError(err)
	assert.Equal(sum3.NotesUpdated, latest.MessagesUpdated, "refresh count persisted to sync history")

	var subject, body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.subject, mb.body_text FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_message_id = ?`), "not_Ab12Cd34Ef56Gh").Scan(&subject, &body))
	assert.Equal("Quarterly Planning Review v2", subject)
	assert.Contains(body, "Quarterly Planning Review v2")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count, "refresh must update in place, not add")
	var conversationParticipantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN messages m ON m.conversation_id = cp.conversation_id
		WHERE m.source_message_id = ?`), "not_Ab12Cd34Ef56Gh").Scan(&conversationParticipantCount))
	assert.Equal(1, conversationParticipantCount, "refresh must remove stale conversation participants")
}

func TestIngestNote_RawFailureRollsBackCanonicalWrite(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a raw-archive write failure")
	require := require.New(t)
	assert := assert.New(t)
	api := &fakeAPI{notes: map[string][]byte{}}
	imp, st := newTestImporter(t, api)
	require.True(st.FTS5Available(), "atomic rollback test inspects the SQLite FTS table")
	source, err := st.GetOrCreateSource(SourceType, "alice@example.com")
	require.NoError(err)

	_, err = st.DB().Exec(`
		CREATE TRIGGER fail_granola_raw_archive
		BEFORE INSERT ON message_raw
		WHEN NEW.raw_format = 'granola_json'
		BEGIN
			SELECT RAISE(ABORT, 'forced granola raw failure');
		END
	`)
	require.NoError(err)

	raw := loadFixture(t, "note_full.json")
	var note Note
	require.NoError(json.Unmarshal(raw, &note))
	note.Raw = append(json.RawMessage(nil), raw...)

	added, err := imp.ingestNote(source.ID, "alice@example.com", nil, &note)
	require.Error(err)
	assert.False(added)
	assert.Contains(err.Error(), "upsert raw")

	for table, want := range map[string]int{
		"messages":           0,
		"conversations":      0,
		"message_bodies":     0,
		"message_raw":        0,
		"message_recipients": 0,
		"messages_fts":       0,
	} {
		var got int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&got), table)
		assert.Equal(want, got, table)
	}
}

func TestImport_FatalRunPersistsSuccessfulRefreshCounters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	api := &fakeAPI{
		notes: map[string][]byte{
			"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json"),
		},
		hasMore: true,
		cursor:  "repeated",
	}
	imp, st := newTestImporter(t, api)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(err)
	assert.EqualValues(2, sum.NotesProcessed)
	assert.EqualValues(1, sum.NotesAdded)
	assert.EqualValues(1, sum.NotesUpdated)

	latest, latestErr := st.GetLatestSync(sum.SourceID)
	require.NoError(latestErr)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.Equal(sum.NotesProcessed, latest.MessagesProcessed)
	assert.Equal(sum.NotesAdded, latest.MessagesAdded)
	assert.Equal(sum.NotesUpdated, latest.MessagesUpdated)
}

func TestImport_CursorAdvancesOnlyOnCleanRuns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	updatedID := "not_Ab12Cd34Ef56Gh"
	failingID := "not_Zz98Yy87Xx76Wv"
	api := &fakeAPI{
		notes: map[string][]byte{
			updatedID: loadFixture(t, "note_full.json"),
			failingID: loadFixture(t, "note_no_calendar.json"),
		},
		orderedIDs: []string{updatedID, failingID},
		fail:       map[string]bool{},
	}
	imp, st := newTestImporter(t, api)

	// Clean run: watermark = max updated_at across both notes.
	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(0, sum.Errors)

	// Reads the newest completed run by id: GetLastSuccessfulSync orders by
	// completed_at, which has second precision and ties across the
	// back-to-back runs this test performs.
	cursorOf := func() string {
		var blob string
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT cursor_after FROM sync_runs
			WHERE source_id = ? AND status = 'completed'
			ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&blob))
		var state syncState
		require.NoError(json.Unmarshal([]byte(blob), &state))
		return state.UpdatedAfter
	}
	firstCursor := cursorOf()
	assert.Equal("2026-06-02T09:50:00Z", firstCursor)

	// Failing run: one note 404s; the cursor must not advance.
	edited := strings.Replace(string(api.notes[updatedID]),
		"2026-06-01T16:45:00Z", "2026-06-05T12:00:00Z", 1)
	failing := strings.Replace(string(api.notes[failingID]),
		"2026-06-02T09:50:00Z", "2026-06-04T12:00:00Z", 1)
	api.mu.Lock()
	api.notes[updatedID] = []byte(edited)
	api.notes[failingID] = []byte(failing)
	api.fail[failingID] = true
	api.mu.Unlock()

	sum2, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(err)
	assert.Contains(err.Error(), "partial")
	assert.EqualValues(1, sum2.Errors)
	assert.EqualValues(2, sum2.NotesProcessed)
	assert.EqualValues(0, sum2.NotesAdded)
	assert.EqualValues(1, sum2.NotesUpdated)
	latest, latestErr := st.GetLatestSync(sum2.SourceID)
	require.NoError(latestErr)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.Equal(sum2.NotesProcessed, latest.MessagesProcessed)
	assert.Equal(sum2.NotesAdded, latest.MessagesAdded)
	assert.Equal(sum2.NotesUpdated, latest.MessagesUpdated)
	assert.Equal(sum2.Errors, latest.ErrorsCount)
	assert.Equal(firstCursor, cursorOf(), "cursor must hold until a clean run covers the failed note")

	// Clean retry: cursor advances past the edited note.
	api.mu.Lock()
	api.fail[failingID] = false
	api.mu.Unlock()
	sum3, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(0, sum3.Errors)
	assert.Equal("2026-06-05T12:00:00Z", cursorOf())
}

func TestImport_IncrementalDiscoversLateNoteAtCursorTimestamp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	updatedAt := time.Date(2026, 7, 14, 18, 30, 0, 123456789, time.UTC)
	firstID := "not_FirstBoundaryNote"
	lateID := "not_LateBoundaryNote"
	api := &fakeAPI{
		notes: map[string][]byte{
			firstID: noteFixtureAt(t, firstID, updatedAt),
		},
		orderedIDs:          []string{firstID},
		respectUpdatedAfter: true,
	}
	imp, st := newTestImporter(t, api)

	first, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "alice@example.com",
		AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(1, first.NotesAdded)

	api.mu.Lock()
	api.notes[lateID] = noteFixtureAt(t, lateID, updatedAt)
	api.orderedIDs = []string{firstID, lateID}
	api.mu.Unlock()

	second, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "alice@example.com",
		AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(1, second.NotesAdded,
		"a note first listed at the cursor timestamp must remain discoverable")
	assert.Zero(second.NotesUpdated,
		"the already-covered boundary note must not become a false update")

	third, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "alice@example.com",
		AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.Zero(third.NotesAdded)
	assert.Zero(third.NotesUpdated,
		"overlapping the boundary must not create repeated cache-invalidating updates")

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count)
}

func TestImport_CoveredCursorBoundaryDoesNotConsumeLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	updatedAt := time.Date(2026, 7, 14, 18, 30, 0, 123456789, time.UTC)
	firstID := "not_FirstLimitedBoundary"
	lateID := "not_LateLimitedBoundary"
	api := &fakeAPI{
		notes: map[string][]byte{
			firstID: noteFixtureAt(t, firstID, updatedAt),
		},
		orderedIDs:          []string{firstID},
		respectUpdatedAfter: true,
	}
	imp, _ := newTestImporter(t, api)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	api.mu.Lock()
	api.notes[lateID] = noteFixtureAt(t, lateID, updatedAt)
	api.orderedIDs = []string{firstID, lateID}
	api.mu.Unlock()

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com",
		Limit:      1,
	})
	require.NoError(err)
	assert.EqualValues(1, sum.NotesProcessed)
	assert.EqualValues(1, sum.NotesAdded,
		"an already-covered overlap row must not starve a new note under --limit")
}

func TestImport_LimitedRunsRememberNewCursorBoundaryIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	updatedAt := time.Date(2026, 7, 14, 18, 30, 0, 123456789, time.UTC)
	firstID := "not_InitialLimitedBoundary"
	lateFirstID := "not_FirstLateLimitedBoundary"
	lateSecondID := "not_SecondLateLimitedBoundary"
	api := &fakeAPI{
		notes: map[string][]byte{
			firstID: noteFixtureAt(t, firstID, updatedAt),
		},
		orderedIDs:          []string{firstID},
		respectUpdatedAfter: true,
	}
	imp, st := newTestImporter(t, api)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	api.mu.Lock()
	api.notes[lateFirstID] = noteFixtureAt(t, lateFirstID, updatedAt)
	api.notes[lateSecondID] = noteFixtureAt(t, lateSecondID, updatedAt)
	api.orderedIDs = []string{firstID, lateFirstID, lateSecondID}
	api.mu.Unlock()

	firstLimited, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com",
		Limit:      1,
	})
	require.NoError(err)
	assert.EqualValues(1, firstLimited.NotesAdded)
	assert.Zero(firstLimited.NotesUpdated)

	secondLimited, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com",
		Limit:      1,
	})
	require.NoError(err)
	assert.EqualValues(1, secondLimited.NotesAdded,
		"the second late boundary note must not be starved by the first")
	assert.Zero(secondLimited.NotesUpdated,
		"the first late boundary note must be remembered after a limited run")

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(3, count)
}

func TestImport_LimitDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newerID := "not_Ab12Cd34Ef56Gh"
	olderID := "not_Zz98Yy87Xx76Wv"
	newer := strings.Replace(
		string(loadFixture(t, "note_full.json")),
		"2026-06-01T16:45:00Z",
		"2026-06-10T16:45:00Z",
		1,
	)
	api := &fakeAPI{
		notes: map[string][]byte{
			newerID: []byte(newer),
			olderID: loadFixture(t, "note_no_calendar.json"),
		},
		orderedIDs:          []string{newerID, olderID},
		respectUpdatedAfter: true,
	}
	imp, st := newTestImporter(t, api)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", Limit: 1})
	require.NoError(err)
	require.EqualValues(1, sum.NotesProcessed)

	var cursorJSON string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT cursor_after FROM sync_runs
		WHERE source_id = ? AND status = 'completed'
		ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&cursorJSON))
	var cursor syncState
	require.NoError(json.Unmarshal([]byte(cursorJSON), &cursor))
	assert.Empty(cursor.UpdatedAfter, "limited run must preserve the prior cursor")

	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count, "normal incremental run must still import the older note")
}

func TestImport_BoundedFullPreservesIncrementalCursor(t *testing.T) {
	tests := []struct {
		name                string
		opts                ImportOptions
		respectCreatedAfter bool
	}{
		{
			name: "created-after bound",
			opts: ImportOptions{
				Full:         true,
				CreatedAfter: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
			},
			respectCreatedAfter: true,
		},
		{
			name: "limit",
			opts: ImportOptions{
				Full:  true,
				Limit: 1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			olderID := "not_Ab12Cd34Ef56Gh"
			newerID := "not_Zz98Yy87Xx76Wv"
			api := &fakeAPI{
				notes: map[string][]byte{
					olderID: loadFixture(t, "note_full.json"),
					newerID: loadFixture(t, "note_no_calendar.json"),
				},
				orderedIDs: []string{newerID, olderID},
			}
			imp, st := newTestImporter(t, api)

			first, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
			require.NoError(err)
			cursorOf := func() string {
				var encoded string
				require.NoError(st.DB().QueryRow(st.Rebind(`
					SELECT cursor_after FROM sync_runs
					WHERE source_id = ? AND status = 'completed'
					ORDER BY id DESC LIMIT 1`), first.SourceID).Scan(&encoded))
				var state syncState
				require.NoError(json.Unmarshal([]byte(encoded), &state))
				return state.UpdatedAfter
			}
			priorCursor := cursorOf()
			require.Equal("2026-06-02T09:50:00Z", priorCursor)

			api.mu.Lock()
			api.notes[olderID] = []byte(strings.Replace(
				string(api.notes[olderID]), "2026-06-01T16:45:00Z", "2026-06-05T12:00:00Z", 1,
			))
			api.notes[newerID] = []byte(strings.Replace(
				string(api.notes[newerID]), "2026-06-02T09:50:00Z", "2026-06-10T12:00:00Z", 1,
			))
			api.respectCreatedAfter = tt.respectCreatedAfter
			api.mu.Unlock()

			tt.opts.Identifier = "alice@example.com"
			_, err = imp.Import(context.Background(), tt.opts)
			require.NoError(err)
			assert.Equal(priorCursor, cursorOf(), "partial full traversal must retain the established incremental cursor")

			api.mu.Lock()
			api.respectCreatedAfter = false
			api.respectUpdatedAfter = true
			api.mu.Unlock()
			incremental, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
			require.NoError(err)
			assert.EqualValues(2, incremental.NotesUpdated,
				"the next incremental run must still see updates on both sides of the full-sync bound")
			assert.Equal("2026-06-10T12:00:00Z", cursorOf())
		})
	}
}

func TestImport_IncompletePageWithoutCursorFails(t *testing.T) {
	require := require.New(t)
	api := &fakeAPI{
		notes: map[string][]byte{
			"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json"),
		},
		hasMore: true,
	}
	imp, _ := newTestImporter(t, api)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.ErrorContains(err, "has more notes but no cursor")
}

func TestImport_RepeatedPageCursorFails(t *testing.T) {
	require := require.New(t)
	api := &fakeAPI{
		notes: map[string][]byte{
			"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json"),
		},
		hasMore: true,
		cursor:  "same-page",
	}
	imp, _ := newTestImporter(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := imp.Import(ctx, ImportOptions{Identifier: "alice@example.com"})
	require.ErrorContains(err, "repeated page cursor")
}

func TestFormatTranscriptLine(t *testing.T) {
	assert := assert.New(t)
	tests := []struct {
		seconds int
		want    string
	}{
		{0, "[00:00] A: x"},
		{71, "[01:11] A: x"},
		{3692, "[1:01:32] A: x"},
	}
	for _, tc := range tests {
		assert.Equal(tc.want, formatTranscriptLine(time.Duration(tc.seconds)*time.Second, "A", "x"))
	}
}

func TestSnippetPreservesUTF8(t *testing.T) {
	assert := assert.New(t)
	body := strings.Repeat("a", 199) + "é" + "tail"

	got := snippet(body)

	assert.True(utf8.ValidString(got))
	assert.Equal(strings.Repeat("a", 199)+"é", got)
}
