package archive_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestLibraryHandlerExportsMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.com")
	require.NoError(err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Plans")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "message-1", MessageType: "email",
		Subject: sql.NullString{String: "Plans", Valid: true},
		SentAt:  sql.NullTime{Time: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	srv := archive.NewServer(archive.ServerOptions{Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL())})
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/messages/export?start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z", nil)
	w := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(w, r)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	require.Len(lines, 5)
	assert.Contains(lines[3], `"subject":"Plans"`)
	assert.Contains(lines[4], `"record_type":"complete"`, "an embedded archive streams the whole export")
}
