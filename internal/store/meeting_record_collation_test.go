//go:build cgo

package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingRecordPreviousCandidatesUseBytewiseReferenceOrdering(t *testing.T) {
	for _, logicalCopy := range []bool{false, true} {
		name := "physical reference"
		if logicalCopy {
			name = "shared UID on another copy"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if st.dialect.DriverName() == postgresDriverName {
				t.Skip("SQLite custom collation exercises non-bytewise column ordering")
			}
			raw := linkSeriesRaw
			if !logicalCopy {
				raw = strings.Replace(raw, `"iCalUID":"weekly@example.org",`, "", 1)
			}
			anchor, _ := calendarLinkFixture(t, st, "calendar-a@example.com", raw)
			linked := anchor
			if logicalCopy {
				linked, _ = calendarLinkFixture(t, st, "calendar-b@example.org", raw)
			}
			_, note := projectionFixture(t, st, "manual-earlier", "meeting_json", `{"summary_text":"Earlier evidence"}`)
			requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d@20270307T100000Z", linked), note))

			st.db.SetMaxOpenConns(1)
			conn, err := st.db.Conn(t.Context())
			requirements.NoError(err)
			err = conn.Raw(func(driverConn any) error {
				sqlite, ok := driverConn.(*sqlite3.SQLiteConn)
				requirements.True(ok)
				return sqlite.RegisterCollation("meeting_reverse", func(a, b string) int { return -strings.Compare(a, b) })
			})
			requirements.NoError(err)
			requirements.NoError(conn.Close())
			// Reference ranges require byte ordering regardless of the database's
			// default text collation. Reverse ordering makes that dependency observable.
			_, err = st.db.Exec(`ALTER TABLE meeting_link_decisions RENAME TO meeting_link_decisions_default_order;
CREATE TABLE meeting_link_decisions (
 member_message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 target_ref TEXT COLLATE meeting_reverse NOT NULL,
 target_logical TEXT COLLATE meeting_reverse NOT NULL DEFAULT '',
 decision TEXT NOT NULL CHECK(decision IN ('link', 'split')),
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY(member_message_id, target_ref)
);
INSERT INTO meeting_link_decisions SELECT * FROM meeting_link_decisions_default_order;
DROP TABLE meeting_link_decisions_default_order;`)
			requirements.NoError(err)
			budget := 500000
			target, err := resolveLinkOccurrence(t.Context(), st.snapshotReader(t.Context()), OwnerScope(), OccurrenceRef{MessageID: anchor, Key: "20270314T100000Z"}, &budget)
			requirements.NoError(err)
			candidates, err := st.previousMeetingCandidates(t.Context(), st.snapshotReader(t.Context()), OwnerScope(), target)
			requirements.NoError(err)
			assertions.Equal([]int64{note}, candidates)
		})
	}
}
