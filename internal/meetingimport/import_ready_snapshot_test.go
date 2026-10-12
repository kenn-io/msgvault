package meetingimport

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func abortNativeMeetingInsert(t *testing.T, st *store.Store, table string) {
	t.Helper()
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_meeting_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic meeting snapshot failure'; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_meeting_abort BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION synthetic_meeting_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_meeting_abort BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'synthetic meeting snapshot failure'); END`)
	}
	Require.NoError(t, err)
}

func removeNativeMeetingAbort(t *testing.T, st *store.Store, table string) {
	t.Helper()
	sql := `DROP TRIGGER synthetic_meeting_abort`
	if st.IsPostgreSQL() {
		sql += ` ON ` + table
	}
	_, err := st.DB().Exec(sql)
	Require.NoError(t, err)
}

func TestNativeMeetingRequiredSnapshotFailureRollsBackAndRetries(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "message_recipients", "conversation_participants", "mcp_event_log"} {
		t.Run(table, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureNativeMeetingImports(t, st, true, true)
			req := validImportRequest(t)
			req.NotifyImports = true
			abortNativeMeetingInsert(t, st, table)
			native := NewImporter(st, Hooks{})
			_, err := native.Import(t.Context(), req)
			require.Error(err)
			for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + name).Scan(&n))
				assert.Zero(n, "required meeting snapshot leaked "+name)
			}
			removeNativeMeetingAbort(t, st, table)
			result, err := native.Import(t.Context(), req)
			require.NoError(err)
			assert.NotZero(result.MessageID)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Equal(1, events)
		})
	}
}

func TestNativeMeetingPostCommitPersonLinkFailureKeepsReadyMessageAndRepairsWithoutReplay(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureNativeMeetingImports(t, st, true, true)
	req := validImportRequest(t)
	req.NotifyImports = true
	req.Meeting.Attendees = []MeetingPerson{{Name: "Synthetic Peer", Email: "peer@example.net", Phone: "+15550000002", ID: "synthetic-peer"}}
	abortNativeMeetingInsert(t, st, "participant_contact_observations")
	native := NewImporter(st, Hooks{})
	first, err := native.Import(t.Context(), req)
	require.Error(err, "native automatic-person-linking failure remains visible")
	require.NotZero(first.MessageID, "the canonical meeting committed before the native repair")
	require.NotEmpty(first.ImportRunID)
	engine := query.NewSQLiteEngine(st.DB())
	if st.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(st.DB(), query.PostgreSQLQueryDialect{})
	}
	detail, err := engine.GetMessage(t.Context(), first.MessageID)
	require.NoError(err)
	require.NotNil(detail)
	normalized, err := req.Normalize()
	require.NoError(err)
	snapshot, err := BuildSnapshot(normalized)
	require.NoError(err)
	assert.Equal(snapshot.Body, detail.BodyText)
	require.Len(detail.To, 1)
	assert.Equal("peer@example.net", detail.To[0].Email)
	assert.Equal("Synthetic Peer", detail.To[0].Name)
	var observations, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_contact_observations`).Scan(&observations))
	assert.Zero(observations, "ready message reads do not depend on optional person grouping")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
	failed, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, failed.Status)
	removeNativeMeetingAbort(t, st, "participant_contact_observations")
	second, err := native.Import(t.Context(), req)
	require.NoError(err)
	assert.Equal(first.MessageID, second.MessageID)
	assert.NotEqual(first.ImportRunID, second.ImportRunID)
	var links int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_links`).Scan(&links))
	assert.Equal(1, links, "native stable-anchor repair remains intact on unchanged retry")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events, "post-commit recovery reuses the original import decision")
	completed, err := st.GetLatestSync(second.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, completed.Status)
	assert.Zero(completed.MessagesUpdated, "repair retains the native unchanged-message counters")
}
