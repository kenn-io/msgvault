//go:build fts5

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// BenchmarkExploreFullText measures the complete first-page HTTP path, including
// SQLite FTS candidate resolution, DuckDB projection, identity hydration, and
// JSON encoding. The synthetic archive has 20,000 emails with two recipients
// each. Body-only terms match 20, 5,000, and 20,000 messages; the broad case
// exceeds the production 10,000-candidate limit. Fixture creation and a warm
// request are excluded. This is a measurement, not a wall-clock CI assertion.
func BenchmarkExploreFullText(b *testing.B) {
	const messageCount = 20_000
	st, err := store.OpenForTest(filepath.Join(b.TempDir(), "archive.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, st.Close()) })
	require.NoError(b, st.InitSchema())
	require.True(b, st.FTS5Available())

	// Bulk inserts keep setup cheap while retaining the real Store schema,
	// indexes, and production FTS backfill. No existing archive is opened.
	for _, statement := range []string{
		`INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'archive-a@example.com')`,
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES (101, 1, 'c1', 'email')`,
		`INSERT INTO participants (id, email_address, domain, display_name) VALUES
			(1, 'alice@example.com', 'example.com', 'Alice'), (2, 'bob@members.example', 'members.example', 'Bob')`,
		fmt.Sprintf(`WITH RECURSIVE ids(id) AS (SELECT 1 UNION ALL SELECT id + 1 FROM ids WHERE id < %d)
			INSERT INTO messages (id, source_id, source_message_id, conversation_id, message_type, subject, snippet, sent_at, sender_id, size_estimate)
			SELECT id, 1, 'm' || id, 101, 'email', 'Message ' || id, 'Synthetic correspondence',
				'2026-07-18 10:00:00', 1, 1000 FROM ids`, messageCount),
		`INSERT INTO message_bodies (message_id, body_text)
			SELECT id, 'Project discussion and the latest status of the delivery schedule. '
				|| CASE WHEN id % 4 = 0 THEN 'Budget planning for the next quarter. ' ELSE '' END
				|| CASE WHEN id <= 20 THEN 'Quartz review. ' ELSE '' END FROM messages`,
		`INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
			SELECT id, 1, 'from', 'Alice', 'alice@example.com' FROM messages
			UNION ALL SELECT id, 2, 'to', 'Bob', 'bob@members.example' FROM messages`,
	} {
		_, err := st.DB().Exec(statement)
		require.NoError(b, err)
	}
	indexed, err := st.BackfillFTS(nil)
	require.NoError(b, err)
	require.Equal(b, int64(messageCount), indexed)

	var messages, recipients strings.Builder
	for id := 1; id <= messageCount; id++ {
		if id > 1 {
			messages.WriteByte(',')
			recipients.WriteByte(',')
		}
		fmt.Fprintf(&messages, `(%d::BIGINT, 1::BIGINT, 'm%d', 101::BIGINT, 'Message %d', 'Synthetic correspondence', TIMESTAMP '2026-07-18 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, 1::BIGINT, NULL::BIGINT, 'email', NULL::VARCHAR, false, 2026, 7)`, id, id, id)
		fmt.Fprintf(&recipients, `(%d::BIGINT, 1::BIGINT, 'from', 'Alice'), (%d::BIGINT, 2::BIGINT, 'to', 'Bob')`, id, id)
	}
	engine, _ := newExploreDuckDBFixtureWithMessagesAndRecipients(b, messages.String(), recipients.String(), messageCount)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Engine: engine, Logger: testLogger(),
	})
	router := srv.Router()

	for _, tc := range []struct {
		name, term string
		matches    int64
		saturated  bool
	}{
		{name: "rare_20", term: "quartz", matches: 20},
		{name: "common_5000", term: "budget", matches: 5_000},
		{name: "broad_20000", term: "project", matches: 20_000, saturated: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			body := fmt.Sprintf(`{"query":%q,"search_mode":"full_text","limit":50}`, tc.term)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/explore", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(b, http.StatusOK, response.Code, response.Body.String())
			var result ExploreHTTPResponse
			require.NoError(b, json.Unmarshal(response.Body.Bytes(), &result))
			require.Len(b, result.Rows, int(min(tc.matches, 50)))
			require.Equal(b, tc.saturated, result.CandidatePoolSaturated)
			if tc.saturated {
				require.Nil(b, result.TotalCount)
			} else {
				require.NotNil(b, result.TotalCount)
				require.Equal(b, tc.matches, *result.TotalCount)
			}
			b.ReportAllocs()
			for b.Loop() {
				request := httptest.NewRequest(http.MethodPost, "/api/v1/explore", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				require.Equal(b, http.StatusOK, response.Code)
			}
		})
	}
}
