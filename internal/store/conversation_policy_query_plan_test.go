package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParticipantChatProvenanceUsesSenderAndRecipientIndexes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := testutil.NewSQLiteTestStore(t)
	query := `EXPLAIN QUERY PLAN SELECT p.id FROM participants p WHERE ` +
		store.FullParticipantNameWhere("p.id", "'Synthetic Name'")
	rows, err := s.DB().QueryContext(t.Context(), query)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()

	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &notUsed, &detail))
		details = append(details, detail)
	}
	require.NoError(rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(plan, "SEARCH policy_message USING INDEX idx_messages_sender")
	assert.Contains(plan, "SEARCH policy_recipient USING INDEX idx_message_recipients_participant")
}
