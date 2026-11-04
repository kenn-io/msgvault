//go:build cgo

package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

type messageExportSinkFunc func(MessageExportMessage) error

func (messageExportSinkFunc) Source(MessageExportSource) error             { return nil }
func (messageExportSinkFunc) Conversation(MessageExportConversation) error { return nil }
func (f messageExportSinkFunc) Message(message MessageExportMessage) error { return f(message) }

func TestExportMessagesReadsWindowOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	db, observer := sqlobserve.OpenFile(t, filepath.Join(t.TempDir(), "message-export-window.db")+testSQLiteParams, nil)
	dialect := &SQLiteDialect{}
	s := &Store{db: newLoggedDB(db, dialect.Rebind), dialect: dialect}
	requirements.NoError(s.InitSchema())
	const messageCount = 3*messageExportPageSize + 1
	seedLiveMessages(t, s, messageCount)
	_, err := s.db.Exec("ANALYZE")
	requirements.NoError(err)

	observer.Reset()
	start := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	var messageIDs []string
	counts, err := s.ExportMessages(t.Context(), MessageExportFilter{
		Start: start, End: start.Add(24 * time.Hour),
	}, messageExportSinkFunc(func(message MessageExportMessage) error {
		messageIDs = append(messageIDs, message.ID)
		if len(messageIDs) == messageExportPageSize {
			// A row hidden during the export must remain in its original snapshot.
			_, err := s.db.Exec(`UPDATE messages SET deleted_at = ? WHERE source_id = ? AND source_message_id = ?`,
				start, 1, fmt.Sprintf("m%d", messageExportPageSize+1))
			return err
		}
		return nil
	}))
	requirements.NoError(err)
	assertions.Equal(messageCount, counts.Messages)
	requirements.Len(messageIDs, messageCount)
	for i, id := range messageIDs {
		assertions.Equal(fmt.Sprintf("m%d", i+1), id)
	}
	var messageQueries int
	var bodyRootPage int
	requirements.NoError(s.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 'message_bodies'`).Scan(&bodyRootPage))
	for _, query := range observer.Statements() {
		if strings.Contains(query, "JOIN conversations c") &&
			strings.Contains(query, "SELECT metadata.*,") {
			messageQueries++
			assertions.Contains(explainPlan(t, s, query, start, start.Add(24*time.Hour)),
				"SEARCH message_bodies USING INTEGER PRIMARY KEY (rowid=?)")
			rows, err := s.db.Query("EXPLAIN "+query, start, start.Add(24*time.Hour))
			requirements.NoError(err)
			var sorters int
			sortAddress, lastInsertAddress, bodyReadAddress := -1, -1, -1
			for rows.Next() {
				var addr, p1, p2, p3, p5 int
				var opcode string
				var p4, comment any
				requirements.NoError(rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment))
				switch opcode {
				case "SorterOpen":
					sorters++
				case "SorterInsert":
					lastInsertAddress = addr
				case "SorterSort":
					sortAddress = addr
				case "OpenRead":
					if p2 == bodyRootPage && p3 == 0 {
						bodyReadAddress = addr
					}
				}
			}
			requirements.NoError(rows.Err())
			requirements.NoError(rows.Close())
			assertions.Equal(1, sorters, "sort the window once")
			requirements.GreaterOrEqual(sortAddress, 0)
			assertions.Greater(bodyReadAddress, sortAddress, "fetch bodies after sorting metadata")
			assertions.Less(lastInsertAddress, bodyReadAddress, "keep bodies out of sorter records")
		}
	}
	assertions.Equal(1, messageQueries)
}

// A source-filtered export must seek conversations by source instead of
// scanning every conversation in the archive.
func TestMessageExportConversationsQuerySeeksBySourceSQLite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "message-export-plan.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	query, args := messageExportConversationsQuery(MessageExportFilter{
		Start: start, End: start.Add(time.Hour),
		SourceIDs: []int64{1}, MessageTypes: []string{"wanted"},
	})
	plan := explainPlan(t, st, query, args...)
	assert.Contains(plan, "SEARCH c USING")
	assert.NotContains(plan, "SCAN c")
}
