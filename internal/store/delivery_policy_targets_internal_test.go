package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeliverySourceEmailEvidenceUsesParticipantIndexes(t *testing.T) {
	requirements := require.New(t)
	st := openTestStore(t)
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+deliverySourceEmailEvidenceQuery, 1, 1, 1, 1)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &notUsed, &detail))
		plan = append(plan, detail)
	}
	requirements.NoError(rows.Err())

	joined := strings.Join(plan, "\n")
	requirements.Contains(joined, "idx_messages_sender", "sender evidence must use the sender index:\n%s", joined)
	requirements.Contains(joined, "idx_message_recipients_participant", "recipient evidence must use the participant index:\n%s", joined)
}
