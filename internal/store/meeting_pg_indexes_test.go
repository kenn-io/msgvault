package store

import (
	"context"
	"database/sql/driver"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitSchema_PGMeetingIndexesBuildConcurrently(t *testing.T) {
	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL")
	}
	for _, index := range []string{"idx_messages_calendar_ical_uid", "idx_messages_meeting_start"} {
		t.Run(index, func(t *testing.T) {
			require := require.New(t)
			base := newRFC822IDBackfillBackendStore(t)
			_, err := base.db.ExecContext(t.Context(), `DROP INDEX `+index)
			require.NoError(err)

			// Observe statements sent to the real driver, including the schema
			// batch. A valid rebuilt index alone cannot reveal a blocking build.
			var builds []string
			st := gatedMeetingProjectionStore(t, base, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
				for statement := range strings.SplitSeq(query, ";") {
					if strings.Contains(statement, "CREATE INDEX") && strings.Contains(statement, index) {
						builds = append(builds, statement)
					}
				}
				return nil
			}})
			require.NoError(st.InitSchemaContext(t.Context()))
			var valid bool
			require.NoError(st.db.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass(?)`, index).Scan(&valid))
			require.True(valid, "upgrade must restore a usable index")
			require.NotEmpty(builds, "observe the statement that rebuilt the missing index")
			for _, statement := range builds {
				assert.Contains(t, statement, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+index,
					"archive index builds must allow concurrent writers")
			}
		})
	}
}
