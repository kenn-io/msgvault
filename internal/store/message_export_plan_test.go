package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type discardingMessageExportSink struct{}

func (discardingMessageExportSink) Source(MessageExportSource) error             { return nil }
func (discardingMessageExportSink) Conversation(MessageExportConversation) error { return nil }
func (discardingMessageExportSink) Message(MessageExportMessage) error           { return nil }

func TestExportMessagesReadsWindowOnce(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "message-export-window.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())
	const messageCount = 3*messageExportPageSize + 1
	seedLiveMessages(t, s, messageCount)
	_, err = s.db.Exec("ANALYZE")
	requirements.NoError(err)

	var messageQueries int
	rebind := s.db.rebind
	s.db.rebind = func(query string) string {
		if strings.Contains(query, "JOIN conversations c") &&
			strings.Contains(query, "ORDER BY s.source_type, s.identifier, c.source_conversation_id,") {
			messageQueries++
		}
		return rebind(query)
	}
	t.Cleanup(func() { s.db.rebind = rebind })
	start := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	counts, err := s.ExportMessages(t.Context(), MessageExportFilter{
		Start: start, End: start.Add(24 * time.Hour),
	}, discardingMessageExportSink{})
	requirements.NoError(err)
	assertions.Equal(messageCount, counts.Messages)
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
