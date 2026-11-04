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
	for _, query := range observer.Statements() {
		if strings.Contains(query, "JOIN conversations c") &&
			strings.Contains(query, "SELECT metadata.*,") {
			messageQueries++
			planRows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, start, start.Add(24*time.Hour))
			requirements.NoError(err)
			parents := make(map[int]int)
			sortParent, bodySearch := -1, -1
			for planRows.Next() {
				var id, parent, unused int
				var detail string
				requirements.NoError(planRows.Scan(&id, &parent, &unused, &detail))
				parents[id] = parent
				if strings.Contains(detail, "USE TEMP B-TREE FOR ") && strings.HasSuffix(detail, "ORDER BY") {
					sortParent = parent
				}
				if strings.Contains(detail, "SEARCH message_bodies USING INTEGER PRIMARY KEY (rowid=?)") {
					bodySearch = id
				}
			}
			requirements.NoError(planRows.Err())
			requirements.NoError(planRows.Close())
			requirements.Positive(sortParent, "sort metadata in a subquery before reading bodies")
			requirements.Positive(bodySearch, "read bodies by primary key")
			// The body lookup must run outside the sorted subquery. Opcode addresses
			// alone cannot establish execution order across coroutine jumps.
			for ancestor := bodySearch; ancestor != 0; ancestor = parents[ancestor] {
				assertions.NotEqual(sortParent, ancestor, "keep body reads out of the sorted input")
			}
			rows, err := s.db.Query("EXPLAIN "+query, start, start.Add(24*time.Hour))
			requirements.NoError(err)
			var sorters int
			for rows.Next() {
				var addr, p1, p2, p3, p5 int
				var opcode string
				var p4, comment any
				requirements.NoError(rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment))
				if opcode == "SorterOpen" {
					sorters++
				}
			}
			requirements.NoError(rows.Err())
			requirements.NoError(rows.Close())
			assertions.Equal(1, sorters, "sort the window once")
		}
	}
	assertions.Equal(1, messageQueries)
}
