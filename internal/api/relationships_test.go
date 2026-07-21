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
	relOwnerID      = int64(1)
	relAliceID      = int64(2)
	relAlice2ID     = int64(3)
	relNewsletterID = int64(4)
)

// newRelationshipsDuckDBFixture builds a real DuckDB/Parquet engine seeded
// with: an owner (relOwnerID), a reciprocal counterpart (relAliceID) linked
// into a cluster with a chat-only alias (relAlice2ID), and an inbound-only
// newsletter sender (relNewsletterID). Mirrors the scenario used by the
// query-package fixture test, built directly as Parquet since the typed
// TestDataBuilder lives in an unexported _test.go file in another package.
func newRelationshipsDuckDBFixture(t *testing.T, now time.Time) *query.DuckDBEngine {
	t.Helper()
	engine, _ := newRelationshipsDuckDBFixtureWithDir(t, now)
	return engine
}

// newRelationshipsDuckDBFixtureWithDir is newRelationshipsDuckDBFixture plus
// the analytics directory, for tests that need to mutate the committed cache
// state file directly (e.g. simulating a real identity-revision bump).
func newRelationshipsDuckDBFixtureWithDir(t *testing.T, now time.Time) (*query.DuckDBEngine, string) {
	t.Helper()
	analyticsDir := t.TempDir()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var messageRows, recipientRows []string
	nextID := int64(1)
	addMessage := func(fromID, toID int64, isFromMe bool, messageType string, sentAt time.Time) {
		id := nextID
		nextID++
		messageRows = append(messageRows, fmt.Sprintf(
			"(%d::BIGINT, 1::BIGINT, 'm%d', %d::BIGINT, '', 'Preview %d', TIMESTAMP '%s', 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, %s, %v, %d, %d)",
			id, id, id, id, sentAt.Format("2006-01-02 15:04:05"), sqlQuote(messageType), isFromMe, sentAt.Year(), int(sentAt.Month())))
		recipientRows = append(recipientRows,
			fmt.Sprintf("(%d::BIGINT, %d::BIGINT, 'from', '')", id, fromID),
			fmt.Sprintf("(%d::BIGINT, %d::BIGINT, 'to', '')", id, toID))
	}
	for i := range 3 {
		addMessage(relOwnerID, relAliceID, true, "email", now.AddDate(0, 0, -(3-i)))
	}
	addMessage(relOwnerID, relAliceID, false, "calendar_event", now.AddDate(0, 0, -2))
	addMessage(relAlice2ID, relOwnerID, false, "imessage", now.AddDate(0, 0, -1))
	for i := range 50 {
		addMessage(relNewsletterID, relOwnerID, false, "email", now.AddDate(0, 0, -(10+i)))
	}

	tables := []struct {
		dir, file, columns, values string
		empty                      bool
	}{
		{
			dir: "messages/year=2026", file: "messages.parquet",
			columns: "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, sender_id, message_type, is_from_me, year, month",
			values:  strings.Join(messageRows, ",\n"),
		},
		{dir: "sources", file: "sources.parquet", columns: "id, account_email, source_type", values: `(1::BIGINT, 'owner@example.com', 'gmail')`},
		{
			dir: "participants", file: "participants.parquet", columns: "id, email_address, domain, display_name, phone_number",
			values: `(1::BIGINT, 'owner@example.com', 'example.com', 'Owner', ''),
				(2::BIGINT, 'alice@example.com', 'example.com', 'Alice', ''),
				(3::BIGINT, 'alice@chat.example', 'chat.example', 'Alice Chat', ''),
				(4::BIGINT, 'newsletter@example.com', 'example.com', 'Newsletter', '')`,
		},
		{dir: "participant_identifiers", file: "participant_identifiers.parquet", columns: "participant_id, identifier_type, identifier_value, display_value, is_primary", values: `(0::BIGINT, '', '', '', false)`, empty: true},
		{dir: "message_recipients", file: "message_recipients.parquet", columns: "message_id, participant_id, recipient_type, display_name", values: strings.Join(recipientRows, ",\n")},
		{dir: "labels", file: "labels.parquet", columns: "id, name", values: `(0::BIGINT, '')`, empty: true},
		{dir: "message_labels", file: "message_labels.parquet", columns: "message_id, label_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "attachments", file: "attachments.parquet", columns: "attachment_id, message_id, size, filename", values: `(0::BIGINT, 0::BIGINT, 0::BIGINT, '')`, empty: true},
		{dir: "conversations", file: "conversations.parquet", columns: "id, source_conversation_id, title, conversation_type", values: `(0::BIGINT, '', '', 'email')`, empty: true},
		{dir: "conversation_participants", file: "conversation_participants.parquet", columns: "conversation_id, participant_id", values: `(0::BIGINT, 0::BIGINT)`, empty: true},
		{dir: "owner_participants", file: "owner_participants.parquet", columns: "source_id, participant_id", values: `(1::BIGINT, 1::BIGINT)`},
		{
			dir: "participant_clusters", file: "participant_clusters.parquet", columns: "participant_id, canonical_id",
			values: `(2::BIGINT, 2::BIGINT), (3::BIGINT, 2::BIGINT)`,
		},
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
		LastMessageID: nextID - 1, LastSyncAt: now, SchemaVersion: query.CacheSchemaVersion,
		PublishedAt: now, DatasetFingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), state, 0o600))

	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	return engine, analyticsDir
}

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func TestRelationshipsRanksAndGatesOverHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	srv := newTestServerWithEngine(t, newRelationshipsDuckDBFixture(t, now))

	response := postExploreJSON(t, srv, "/api/v1/relationships", `{}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var page RelationshipsHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &page))

	require.Len(page.Rows, 1, "the newsletter must be gated out by default")
	assert.Equal(relAliceID, page.Rows[0].CanonicalID)
	assert.Equal([]int64{relAliceID, relAlice2ID}, page.Rows[0].MemberIDs)
	assert.Equal(int64(3), page.Rows[0].Signals.SentCount)
	assert.Equal(int64(1), page.Rows[0].Signals.MeetingCount)
	assert.NotEmpty(page.CacheRevision)

	showAll := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true}`)
	require.Equal(http.StatusOK, showAll.Code, showAll.Body.String())
	var allPage RelationshipsHTTPResponse
	require.NoError(json.Unmarshal(showAll.Body.Bytes(), &allPage))
	assert.Len(allPage.Rows, 2, "show_all must include the gated newsletter")
	ids := []int64{allPage.Rows[0].CanonicalID, allPage.Rows[1].CanonicalID}
	assert.ElementsMatch([]int64{relAliceID, relNewsletterID}, ids)
}

func TestRelationshipsCursorConflictsOnRevisionDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	srv := newTestServerWithEngine(t, newRelationshipsDuckDBFixture(t, now))

	first := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true,"limit":1}`)
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var page RelationshipsHTTPResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	require.NotEmpty(page.NextCursor, "the first page must offer a cursor into the second row")

	// Legitimate cursor still works for pagination.
	second := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true,"limit":1,"cursor":"`+page.NextCursor+`"}`)
	assert.Equal(http.StatusOK, second.Code, second.Body.String())

	// Doctor a decoded copy of the cursor with a stale revision, re-signed by
	// the real server key, and confirm it is rejected as drifted rather than
	// silently accepted.
	decoded, err := srv.decodeExploreCursor(page.NextCursor)
	require.NoError(err)
	decoded.Revision = "cache-doctored"
	doctored := srv.encodeExploreCursor(decoded)
	conflict := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true,"limit":1,"cursor":"`+doctored+`"}`)
	assert.Equal(http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Contains(conflict.Body.String(), "archive_revision_changed")

	decoded.Revision = page.CacheRevision
	decoded.IdentityRevision = page.IdentityRevision + 1
	doctoredIdentity := srv.encodeExploreCursor(decoded)
	identityConflict := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true,"limit":1,"cursor":"`+doctoredIdentity+`"}`)
	assert.Equal(http.StatusConflict, identityConflict.Code, identityConflict.Body.String())
	assert.Contains(identityConflict.Body.String(), "identity_revision_changed")
}

// TestRelationshipsCursorReportsIdentityDriftDistinctlyFromArchiveDrift
// covers the 409 code precedence: CacheSyncState.Revision() folds
// IdentityRevision into its hash, so a real identity-only refresh (a
// link/unlink/merge, never touching PublishedAt or the message shards)
// changes CacheRevision too, alongside IdentityRevision. The two prior
// doctored-cursor cases above only vary Revision or only vary
// IdentityRevision in isolation, so neither exercises this: they never
// prove which code wins when both actually drift together, as they do on a
// real refresh. Simulating that refresh by bumping IdentityRevision alone in
// the committed cache state must still report identity_revision_changed,
// not archive_revision_changed.
func TestRelationshipsCursorReportsIdentityDriftDistinctlyFromArchiveDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	engine, analyticsDir := newRelationshipsDuckDBFixtureWithDir(t, now)
	srv := newTestServerWithEngine(t, engine)

	first := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true,"limit":1}`)
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var page RelationshipsHTTPResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	require.NotEmpty(page.NextCursor, "the first page must offer a cursor into the second row")

	state, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err, "ReadCacheSyncState")
	state.IdentityRevision++
	data, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), data, 0o600))

	resp := postExploreJSON(t, srv, "/api/v1/relationships", `{"show_all":true,"limit":1,"cursor":"`+page.NextCursor+`"}`)
	assert.Equal(http.StatusConflict, resp.Code, resp.Body.String())
	assert.Contains(resp.Body.String(), "identity_revision_changed")
	assert.NotContains(resp.Body.String(), "archive_revision_changed")
}

func TestRelationshipsRejectsOutOfRangeLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	srv := newTestServerWithEngine(t, newRelationshipsDuckDBFixture(t, now))

	response := postExploreJSON(t, srv, "/api/v1/relationships", fmt.Sprintf(`{"limit":%d}`, exploreMaxLimit+1))
	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "invalid_limit")
}

func TestRelationshipsUnavailableUnderNonAnalyzerEngine(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := newTestServerWithEngine(t, &querytest.MockEngine{})
	response := postExploreJSON(t, srv, "/api/v1/relationships", `{}`)
	require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "analytical_cache_unavailable")
}
