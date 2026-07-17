package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/store"
)

type cacheRefreshCirclebackSource struct {
	meeting circleback.Meeting
}

func (s *cacheRefreshCirclebackSource) SearchMeetings(_ context.Context, _, _ string, pageIndex int) ([]circleback.Meeting, error) {
	if pageIndex > 0 {
		return nil, nil
	}
	return []circleback.Meeting{s.meeting}, nil
}

func (s *cacheRefreshCirclebackSource) ReadMeetings(_ context.Context, _ []string) ([]circleback.Meeting, error) {
	return []circleback.Meeting{s.meeting}, nil
}

func (s *cacheRefreshCirclebackSource) GetTranscripts(_ context.Context, _ []string) (map[string]*circleback.Transcript, error) {
	return map[string]*circleback.Transcript{
		"refresh-1": {ID: circleback.FlexString("refresh-1"), Text: "Meeting transcript"},
	}, nil
}

func TestCacheNeedsBuild_CirclebackRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	t.Cleanup(func() { _ = st.Close() })

	organizer := circleback.Attendee{Name: "Alice", Email: "alice@example.com"}
	source := &cacheRefreshCirclebackSource{meeting: circleback.Meeting{
		ID:           circleback.FlexString("refresh-1"),
		Name:         "Initial Meeting",
		CreatedAt:    "2026-07-01T09:00:00Z",
		StartTime:    "2026-07-01T09:00:00Z",
		EndTime:      "2026-07-01T10:00:00Z",
		Organizer:    &organizer,
		Attendees:    []circleback.Attendee{{Name: "Bob", Email: "bob@example.com"}},
		Notes:        "Initial meeting notes",
		RecordingURL: "https://media.example.com/initial",
	}}
	imp := circleback.NewImporter(st, source)
	first, err := imp.Import(context.Background(), circleback.ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(1, first.MeetingsAdded)

	var originalID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'meeting:refresh-1'`).Scan(&originalID))
	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)
	noOp, err := imp.Import(context.Background(), circleback.ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, noOp.MeetingsUpdated)
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	assert.False(staleness.NeedsBuild, "identical Circleback overlap must not invalidate cache: %+v", staleness)

	source.meeting.Name = "Refreshed Meeting"
	source.meeting.Notes = "Refreshed meeting notes"
	source.meeting.Attendees = []circleback.Attendee{{Name: "Carol", Email: "carol@example.com"}}
	source.meeting.RecordingURL = ""
	second, err := imp.Import(context.Background(), circleback.ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, second.MeetingsAdded)
	assert.EqualValues(1, second.MeetingsUpdated)

	var refreshedID int64
	var subject, snippet string
	var attachmentCount int
	require.NoError(st.DB().QueryRow(`
		SELECT id, subject, snippet, attachment_count
		FROM messages WHERE source_message_id = 'meeting:refresh-1'
	`).Scan(&refreshedID, &subject, &snippet, &attachmentCount))
	assert.Equal(originalID, refreshedID, "refresh updates the existing message")
	assert.Equal("Refreshed Meeting", subject)
	assert.Contains(snippet, "Refreshed meeting notes")
	assert.Zero(attachmentCount, "refresh clears the stale recording attachment")
	var attachmentRows int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, refreshedID).Scan(&attachmentRows))
	assert.Zero(attachmentRows, "refresh removes the stale recording junction")

	var recipient string
	var recipientCount int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*), MAX(p.email_address)
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`, refreshedID).Scan(&recipientCount, &recipient))
	assert.Equal(1, recipientCount, "refresh replaces stale attendees")
	assert.Equal("carol@example.com", recipient)

	staleness = cacheNeedsBuild(dbPath, analyticsDir)
	require.True(staleness.NeedsBuild, "Circleback refresh must invalidate cache: %+v", staleness)
	require.True(staleness.FullRebuild, "Circleback refresh requires full rebuild: %+v", staleness)

	rebuilt, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err)
	require.False(rebuilt.Skipped, "direct build-cache must honor existing-message mutations")

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	var cachedSubject string
	require.NoError(duckDB.QueryRow(`
		SELECT subject
		FROM read_parquet(?, hive_partitioning=true)
		WHERE id = ?
	`, filepath.Join(analyticsDir, "messages", "**", "*.parquet"), refreshedID).Scan(&cachedSubject))
	assert.Equal("Refreshed Meeting", cachedSubject)
}

func TestCacheNeedsBuild_SupersededCirclebackRunWithoutCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	t.Cleanup(func() { _ = st.Close() })

	organizer := circleback.Attendee{Name: "Alice", Email: "alice@example.com"}
	source := &cacheRefreshCirclebackSource{meeting: circleback.Meeting{
		ID:        circleback.FlexString("failed-refresh-1"),
		Name:      "Initial Meeting",
		CreatedAt: "2026-07-01T09:00:00Z",
		StartTime: "2026-07-01T09:00:00Z",
		EndTime:   "2026-07-01T10:00:00Z",
		Organizer: &organizer,
		Notes:     "Initial meeting notes",
	}}
	imp := circleback.NewImporter(st, source)
	first, err := imp.Import(context.Background(), circleback.ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(1, first.MeetingsAdded)
	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)

	abandonedRunID, err := st.StartSync(first.SourceID, circleback.SourceType)
	require.NoError(err)
	_, err = st.DB().Exec(`
		UPDATE messages SET subject = 'Persisted Before Exit'
		WHERE source_message_id = 'meeting:failed-refresh-1'
	`)
	require.NoError(err)
	_, err = st.StartSync(first.SourceID, circleback.SourceType)
	require.NoError(err, "starting the replacement run supersedes the abandoned run")

	var status string
	var additions, updates int64
	require.NoError(st.DB().QueryRow(`
		SELECT status, messages_added, messages_updated
		FROM sync_runs WHERE id = ?
	`, abandonedRunID).Scan(&status, &additions, &updates))
	assert.Equal("failed", status)
	assert.Zero(additions)
	assert.Zero(updates)

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(staleness.NeedsBuild, "zero-counter failed run must invalidate cache: %+v", staleness)
	require.True(staleness.FullRebuild, "failed-run progress requires a full rebuild: %+v", staleness)
	require.Contains(staleness.Reason, "failed sync")

	rebuilt, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err)
	require.False(rebuilt.Skipped, "direct build-cache must honor failed-run progress")

	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	var cachedSubject string
	require.NoError(duckDB.QueryRow(`
		SELECT subject FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = 'meeting:failed-refresh-1'
	`, filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&cachedSubject))
	assert.Equal("Persisted Before Exit", cachedSubject)

	fresh := cacheNeedsBuild(dbPath, analyticsDir)
	assert.False(fresh.NeedsBuild, "recorded failed-run watermark must not rebuild repeatedly: %+v", fresh)
}

type cacheRefreshGranolaAPI struct {
	mu   sync.Mutex
	note []byte
}

func (a *cacheRefreshGranolaAPI) setNote(note []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.note = note
}

func (a *cacheRefreshGranolaAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/notes" {
		_, _ = w.Write([]byte(`{"notes":[`))
		_, _ = w.Write(a.note)
		_, _ = w.Write([]byte(`],"hasMore":false,"cursor":""}`))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/notes/") {
		_, _ = w.Write(a.note)
		return
	}
	http.NotFound(w, r)
}

func cacheRefreshGranolaNote(t *testing.T, title, updatedAt, attendee, summary string) []byte {
	t.Helper()
	note := map[string]any{
		"id":         "note-refresh-1",
		"title":      title,
		"owner":      map[string]any{"name": "Alice", "email": "alice@example.com"},
		"created_at": "2026-07-01T09:00:00Z",
		"updated_at": updatedAt,
		"attendees":  []map[string]any{{"name": "Attendee", "email": attendee}},
		"calendar_event": map[string]any{
			"event_title":          title,
			"organiser":            "alice@example.com",
			"scheduled_start_time": "2026-07-01T09:00:00Z",
			"scheduled_end_time":   "2026-07-01T10:00:00Z",
		},
		"summary_markdown": summary,
	}
	encoded, err := json.Marshal(note)
	require.NoError(t, err)
	return encoded
}

func TestCacheNeedsBuild_GranolaRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	api := &cacheRefreshGranolaAPI{}
	api.setNote(cacheRefreshGranolaNote(t, "Initial Note", "2026-07-01T10:00:00Z", "bob@example.com", "Initial note summary"))
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	t.Cleanup(func() { _ = st.Close() })
	imp := granola.NewImporter(st, granola.NewClient(server.URL, "grn_testkey"))
	first, err := imp.Import(context.Background(), granola.ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(1, first.NotesAdded)

	var originalID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'note-refresh-1'`).Scan(&originalID))
	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)

	api.setNote(cacheRefreshGranolaNote(t, "Refreshed Note", "2026-07-02T10:00:00Z", "carol@example.com", "Refreshed note summary"))
	second, err := imp.Import(context.Background(), granola.ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, second.NotesAdded)
	assert.EqualValues(1, second.NotesUpdated)

	var refreshedID int64
	var subject, snippet string
	require.NoError(st.DB().QueryRow(`
		SELECT id, subject, snippet FROM messages WHERE source_message_id = 'note-refresh-1'
	`).Scan(&refreshedID, &subject, &snippet))
	assert.Equal(originalID, refreshedID, "refresh updates the existing note")
	assert.Equal("Refreshed Note", subject)
	assert.Contains(snippet, "Refreshed note summary")

	var recipient string
	var recipientCount int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*), MAX(p.email_address)
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`, refreshedID).Scan(&recipientCount, &recipient))
	assert.Equal(1, recipientCount, "refresh replaces stale attendees")
	assert.Equal("carol@example.com", recipient)

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(staleness.NeedsBuild, "Granola refresh must invalidate cache: %+v", staleness)
	require.True(staleness.FullRebuild, "Granola refresh requires full rebuild: %+v", staleness)
}
