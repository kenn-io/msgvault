// Package postgresprofile_test exercises the PostgreSQL archive without importing
// the SQLite- and DuckDB-specific tests in the store and query packages.
package postgresprofile_test

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchiveWriteSearchRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires MSGVAULT_TEST_DB pointing at a disposable PostgreSQL database")
	}
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "CEXAMPLE", "General")
	require.NoError(err)
	message := &store.Message{
		SourceID: source.ID, ConversationID: conversation,
		SourceMessageID: "CEXAMPLE:1704110400.000001", MessageType: "slack",
		SentAt:  sql.NullTime{Time: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject: sql.NullString{String: "Project update", Valid: true},
		Snippet: sql.NullString{String: "See the full message", Valid: true},
	}
	id, err := st.UpsertMessage(message)
	require.NoError(err)
	const body = "The observatory is ready for the launch."
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	require.NoError(st.UpsertFTS(id, message.Subject.String, body, "", "", ""))

	// A retried import must keep the same message and its indexed body.
	repeatedID, err := st.UpsertMessage(message)
	require.NoError(err)
	assert.Equal(id, repeatedID)
	engine := query.NewPostgreSQLEngine(st.DB())
	results, err := engine.Search(t.Context(), search.Parse("observatory"), 50, 0)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	detail, err := engine.GetMessage(t.Context(), id)
	require.NoError(err)
	assert.Equal(body, detail.BodyText)
}
