package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

const (
	rtOwnerID     = int64(1)
	rtPatID       = int64(2)
	rtPatWorkID   = int64(3)
	rtBystanderID = int64(4)
)

const relationshipTimelineMessagesCols = "id, source_id, source_message_id, conversation_id, subject, snippet, " +
	"sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, sender_id, message_type, is_from_me, year, month"

// writeRelationshipTimelineFixture writes (or rewrites in place) the
// Parquet fixture for the relationship timeline HTTP tests: an owner, a
// counterpart (Pat) linked with a chat-only alias (Pat Work) into one
// identity cluster, and an unlinked bystander used to simulate a later
// identity link. Three chat messages share conversation 500 — two at
// 23:00/23:30 UTC on 2026-07-13, one at 00:30 UTC the next day — so
// America/Chicago collapses them into one chat_burst alongside one email
// and one meeting (three rows total), matching the query-layer fixture.
//
// linkBystander controls whether the bystander is folded into Pat's
// cluster, simulating "rebuild the cache with a new link": it changes both
// participant_clusters and IdentityRevision, so CacheRevision changes too
// (see query.CacheSyncState.Revision), letting a test call this twice
// against the same analyticsDir to exercise cursor drift detection without
// tearing down the engine.
func writeRelationshipTimelineFixture(t *testing.T, analyticsDir string, now time.Time, linkBystander bool) {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	day1Late1 := time.Date(2026, 7, 13, 23, 0, 0, 0, time.UTC)
	day1Late2 := time.Date(2026, 7, 13, 23, 30, 0, 0, time.UTC)
	day2Early := time.Date(2026, 7, 14, 0, 30, 0, 0, time.UTC)

	type msgRow struct {
		id, convID, fromID, toID int64
		isFromMe                 bool
		messageType              string
		sentAt                   time.Time
	}
	rows := []msgRow{
		{1, 500, rtPatID, rtOwnerID, false, "imessage", day1Late1},
		{2, 500, rtPatID, rtOwnerID, false, "imessage", day1Late2},
		{3, 500, rtPatID, rtOwnerID, false, "imessage", day2Early},
		{4, 601, rtPatID, rtOwnerID, false, "email", time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)},
		{5, 602, rtOwnerID, rtPatID, true, "calendar_event", time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)},
	}
	var messageRows, recipientRows []string
	for _, m := range rows {
		messageRows = append(messageRows, fmt.Sprintf(
			"(%d::BIGINT, 1::BIGINT, 'm%d', %d::BIGINT, '', 'Preview %d', TIMESTAMP '%s', 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, %s, %v, %d, %d)",
			m.id, m.id, m.convID, m.id, m.sentAt.Format("2006-01-02 15:04:05"), sqlQuote(m.messageType), m.isFromMe, m.sentAt.Year(), int(m.sentAt.Month())))
		recipientRows = append(recipientRows,
			fmt.Sprintf("(%d::BIGINT, %d::BIGINT, 'from', '')", m.id, m.fromID),
			fmt.Sprintf("(%d::BIGINT, %d::BIGINT, 'to', '')", m.id, m.toID))
	}

	clusterRows := "(2::BIGINT, 2::BIGINT), (3::BIGINT, 2::BIGINT), (4::BIGINT, 4::BIGINT)"
	identityRevision := int64(1)
	if linkBystander {
		clusterRows = "(2::BIGINT, 2::BIGINT), (3::BIGINT, 2::BIGINT), (4::BIGINT, 2::BIGINT)"
		identityRevision = 2
	}

	tables := []struct {
		dir, file, columns, values string
		empty                      bool
	}{
		{dir: "messages/year=2026", file: "messages.parquet", columns: relationshipTimelineMessagesCols, values: strings.Join(messageRows, ",\n")},
		{dir: "sources", file: "sources.parquet", columns: "id, account_email, source_type", values: `(1::BIGINT, 'owner@example.com', 'gmail')`},
		{
			dir: "participants", file: "participants.parquet", columns: "id, email_address, domain, display_name, phone_number",
			values: `(1::BIGINT, 'owner@example.com', 'example.com', 'Owner', ''),
				(2::BIGINT, 'pat@chat.example', 'chat.example', 'Pat', ''),
				(3::BIGINT, 'pat@work.example', 'work.example', 'Pat Work', ''),
				(4::BIGINT, 'pat@personal.example', 'personal.example', 'Pat Personal', '')`,
		},
		{dir: "participant_identifiers", file: "participant_identifiers.parquet", columns: "participant_id, identifier_type, identifier_value, display_value, is_primary", values: `(0::BIGINT, '', '', '', false)`, empty: true},
		{dir: "message_recipients", file: "message_recipients.parquet", columns: "message_id, participant_id, recipient_type, display_name", values: strings.Join(recipientRows, ",\n")},
		{dir: "labels", file: "labels.parquet", columns: "id, name", values: `(0::BIGINT, '')`, empty: true},
		{dir: "message_labels", file: "message_labels.parquet", columns: "message_id, label_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "attachments", file: "attachments.parquet", columns: "attachment_id, message_id, size, filename", values: `(0::BIGINT, 0::BIGINT, 0::BIGINT, '')`, empty: true},
		{
			dir: "conversations", file: "conversations.parquet", columns: "id, source_conversation_id, title, conversation_type",
			values: `(500::BIGINT, 'c500', '', 'direct_chat'),
				(601::BIGINT, 'c601', '', 'email'),
				(602::BIGINT, 'c602', '', 'calendar')`,
		},
		{
			dir: "conversation_participants", file: "conversation_participants.parquet", columns: "conversation_id, participant_id",
			values: `(500::BIGINT, 1::BIGINT), (500::BIGINT, 2::BIGINT)`,
		},
		{dir: "owner_participants", file: "owner_participants.parquet", columns: "source_id, participant_id", values: `(1::BIGINT, 1::BIGINT)`},
		{dir: "participant_clusters", file: "participant_clusters.parquet", columns: "participant_id, canonical_id", values: clusterRows},
	}
	for _, table := range tables {
		dir := filepath.Join(analyticsDir, table.dir)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		where := ""
		if table.empty {
			where = " WHERE false"
		}
		path := filepath.ToSlash(filepath.Join(dir, table.file))
		_, err := db.Exec(fmt.Sprintf("COPY (SELECT * FROM (VALUES %s) AS t(%s)%s) TO '%s' (FORMAT PARQUET)", table.values, table.columns, where, path))
		require.NoError(t, err, "write %s", table.dir)
	}

	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err)
	state, err := json.Marshal(query.CacheSyncState{
		LastMessageID: 5, LastSyncAt: now, SchemaVersion: query.CacheSchemaVersion,
		PublishedAt: now, DatasetFingerprint: fingerprint, IdentityRevision: identityRevision,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), state, 0o600))
}

// newRelationshipTimelineDuckDBFixture builds a fresh engine over a freshly
// written fixture and returns both, so a test can later call
// writeRelationshipTimelineFixture again against the same directory to
// simulate a cache rebuild without recreating the engine.
func newRelationshipTimelineDuckDBFixture(t *testing.T, now time.Time) (*query.DuckDBEngine, string) {
	t.Helper()
	analyticsDir := t.TempDir()
	writeRelationshipTimelineFixture(t, analyticsDir, now, false)
	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	return engine, analyticsDir
}

func TestRelationshipTimelineOverHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	engine, _ := newRelationshipTimelineDuckDBFixture(t, now)
	srv := newTestServerWithEngine(t, engine)

	response := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatID), `{"timezone":"America/Chicago"}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var page RelationshipTimelineHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &page))

	assert.Equal(rtPatID, page.CanonicalID)
	require.Len(page.Rows, 3, "one chat burst + one email + one meeting")
	assert.Equal(int64(3), page.TotalCount)

	burst := page.Rows[0]
	assert.Equal("chat_burst", burst.Kind)
	assert.Equal(int64(3), burst.MessageCount)
	assert.Equal("email", page.Rows[1].Kind)
	assert.Equal("event", page.Rows[2].Kind)
}

func TestRelationshipTimelineResolvesAnyMemberIDOverHTTP(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	engine, _ := newRelationshipTimelineDuckDBFixture(t, now)
	srv := newTestServerWithEngine(t, engine)

	// rtPatWorkID (3) is a member alias, not the canonical (minimum) ID.
	response := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatWorkID), `{"timezone":"America/Chicago"}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var page RelationshipTimelineHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &page))
	assert.Equal(rtPatID, page.CanonicalID, "an alias ID must resolve to the cluster's canonical ID")
	assert.Equal(int64(3), page.TotalCount)
}

func TestRelationshipTimelinePaginatesWithStableCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	engine, _ := newRelationshipTimelineDuckDBFixture(t, now)
	srv := newTestServerWithEngine(t, engine)

	seenKeys := make(map[string]bool)
	cursor := ""
	for range 5 {
		body := `{"timezone":"America/Chicago","limit":1}`
		if cursor != "" {
			body = fmt.Sprintf(`{"timezone":"America/Chicago","limit":1,"cursor":%q}`, cursor)
		}
		response := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatID), body)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var result RelationshipTimelineHTTPResponse
		require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
		require.Len(result.Rows, 1)
		assert.Equal(int64(3), result.TotalCount)
		seenKeys[result.Rows[0].Key] = true
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	assert.Len(seenKeys, 3, "three single-row pages must cover all three distinct rows with no repeats")
}

func TestRelationshipTimelineCursorInvalidatedOnIdentityRevisionChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	engine, analyticsDir := newRelationshipTimelineDuckDBFixture(t, now)
	srv := newTestServerWithEngine(t, engine)

	first := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatID), `{"timezone":"America/Chicago","limit":1}`)
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var page RelationshipTimelineHTTPResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	require.NotEmpty(page.NextCursor)

	// Simulate a cache rebuild that links the bystander into Pat's cluster,
	// bumping IdentityRevision (and, transitively, CacheRevision).
	writeRelationshipTimelineFixture(t, analyticsDir, now, true)

	replay := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatID),
		fmt.Sprintf(`{"timezone":"America/Chicago","limit":1,"cursor":%q}`, page.NextCursor))
	assert.Equal(http.StatusConflict, replay.Code, replay.Body.String())
	assert.Contains(replay.Body.String(), "cursor_invalidated")
	assert.Contains(replay.Body.String(), "The timeline context changed; restart pagination")
}

func TestRelationshipTimelineRejectsBadTimezone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	engine, _ := newRelationshipTimelineDuckDBFixture(t, now)
	srv := newTestServerWithEngine(t, engine)

	response := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatID), `{"timezone":"Not/AZone"}`)
	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "invalid_explore_request")
}

func TestRelationshipTimelineRejectsBadParticipantID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	engine, _ := newRelationshipTimelineDuckDBFixture(t, now)
	srv := newTestServerWithEngine(t, engine)

	response := postExploreJSON(t, srv, "/api/v1/relationships/0/timeline", `{}`)
	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "invalid_participant_id")
}

func TestRelationshipTimelineUnavailableUnderNonAnalyzerEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := newTestServerWithEngine(t, &querytest.MockEngine{})
	response := postExploreJSON(t, srv, fmt.Sprintf("/api/v1/relationships/%d/timeline", rtPatID), `{}`)
	require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "analytical_cache_unavailable")
}
