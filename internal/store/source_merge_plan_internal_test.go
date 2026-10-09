package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergePayloadFingerprintCandidatesUseSenderAndCanonicalTimestamp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	instant := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	source := []mergeMessage{
		{kind: sql.NullString{String: "email", Valid: true}, sender: sql.NullInt64{Int64: 7, Valid: true}, sent: sql.NullTime{Time: instant, Valid: true}},
		{kind: sql.NullString{String: "email", Valid: true}, sender: sql.NullInt64{Int64: 7, Valid: true}, sent: sql.NullTime{Time: instant.Add(time.Minute), Valid: true}},
	}
	candidates := mergePayloadFingerprintCandidateKeys(source)
	require.Len(candidates, 2)

	equivalentOffset := mergeMessage{
		kind: sql.NullString{String: "email", Valid: true}, sender: sql.NullInt64{Int64: 7, Valid: true},
		sent: sql.NullTime{Time: instant.In(time.FixedZone("example offset", 2*60*60)), Valid: true},
	}
	assert.True(mergePayloadFingerprintCandidate(equivalentOffset, candidates),
		"the same instant must remain a candidate when its stored UTC offset differs")

	differentSender := equivalentOffset
	differentSender.sender = sql.NullInt64{Int64: 8, Valid: true}
	assert.False(mergePayloadFingerprintCandidate(differentSender, candidates))

	differentInstant := equivalentOffset
	differentInstant.sent = sql.NullTime{Time: instant.Add(time.Second), Valid: true}
	assert.False(mergePayloadFingerprintCandidate(differentInstant, candidates))

	missingSender := equivalentOffset
	missingSender.sender = sql.NullInt64{}
	assert.False(mergePayloadFingerprintCandidate(missingSender, candidates))

	unstableChat := equivalentOffset
	unstableChat.kind = sql.NullString{String: "beeper", Valid: true}
	unstableChat.chatID = sql.NullString{String: "account-local-id", Valid: true}
	assert.False(mergePayloadFingerprintCandidate(unstableChat, candidates))
}

func TestSourceMergePlanFingerprintsOnlyRowsWithDestinationCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := OpenForTest(filepath.Join(t.TempDir(), "merge-fingerprint-candidates.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	senderID, err := st.EnsureParticipant("owner@example.test", "Example Owner", "example.test")
	require.NoError(err)

	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	addMessage := func(sourceID int64, conversationID, providerID, body string, sentAt time.Time) {
		conversation, err := st.EnsureConversation(sourceID, conversationID, "Example chat")
		require.NoError(err)
		messageID, err := st.UpsertMessage(&Message{
			SourceID: sourceID, ConversationID: conversation, SourceMessageID: providerID,
			MessageType: "beeper", SenderID: sql.NullInt64{Int64: senderID, Valid: true},
			SentAt: sql.NullTime{Time: sentAt, Valid: true},
		})
		require.NoError(err)
		require.NoError(st.UpsertMessageBody(messageID, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	}
	addMessage(into.ID, "!shared:example.test", "destination", "Destination body", baseTime)
	addMessage(from.ID, "!shared:example.test", "matching-source", "Different source body", baseTime)
	addMessage(from.ID, "!source-only:example.test", "nonmatching-source", "Nonmatching source body", baseTime.Add(time.Minute))

	var plan sourceMergePlan
	err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
		var planErr error
		plan, planErr = buildSourceMergePlan(t.Context(), tx, from.ID, into.ID, &SourceMergeResult{})
		return planErr
	})
	require.NoError(err)
	require.Len(plan.messages, 2)
	assert.NotEmpty(plan.messages[0].fingerprint, "the source message with a destination identity candidate is fingerprinted")
	assert.Empty(plan.messages[1].fingerprint, "a complete source payload without any destination candidate is not fingerprinted")
}
