package store_test

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestAppreciationPeerKeepsNativeTextAndRejectsChangedEvidence(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	id := f.insertMessage(t, "peer-message", "chat", f.aliceID, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	const body = "I helped\n  with the move."
	addSweepBody(t, f, id, body)
	items, err := f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID, []int64{id}, 0)
	require.NoError(t, err)
	peers, err := f.store.PersonSweepAppreciationPeers(t.Context(), f.alicePersonID, items, 0)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	assert.Equal(t, body, peers[0].ExactExcerpt)
	assert.Equal(t, items[0].Ref, peers[0].Ref)
	key, err := peoplesweep.AppreciationContributionKey("slack", "person-sweep-journal", "peer-message")
	require.NoError(t, err)
	assert.Equal(t, key, peers[0].ContributionKey)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO source_merge_archive_only_messages (message_id, original_source_message_id) VALUES (?, ?)`), id, "peer-message")
	require.NoError(t, err)
	peers, err = f.store.PersonSweepAppreciationPeers(t.Context(), f.alicePersonID, items, 0)
	require.NoError(t, err)
	assert.Empty(t, peers, "an archive-only marker without origin cannot establish a native identity")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM source_merge_archive_only_messages WHERE message_id = ?`), id)
	require.NoError(t, err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "Edited body", id)
	require.NoError(t, err)
	peers, err = f.store.PersonSweepAppreciationPeers(t.Context(), f.alicePersonID, items, 0)
	require.NoError(t, err)
	assert.Empty(t, peers)
}

func TestAppreciationOwnerRequiresNativeDirectionAndCompleteDirectRoster(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	require.NoError(t, f.store.EnsureConversationParticipantContext(t.Context(), f.conversationID, f.aliceID, "member"))
	require.NoError(t, f.store.EnsureConversationParticipantContext(t.Context(), f.conversationID, f.bobID, "member"))
	require.NoError(t, f.store.SetConversationMemberCountContext(t.Context(), f.conversationID, 2))
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	peerID := f.insertMessage(t, "peer", "chat", f.aliceID, at)
	ownerID := f.insertMessage(t, "owner", "chat", f.bobID, at.Add(time.Hour))
	addSweepBody(t, f, peerID, "I helped you move.")
	addSweepBody(t, f, ownerID, "Thank you for helping me move.")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET source_is_from_me = TRUE, is_sent = TRUE WHERE id = ?`), ownerID)
	require.NoError(t, err)
	items, err := f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID, []int64{peerID}, 0)
	require.NoError(t, err)
	peers, err := f.store.PersonSweepAppreciationPeers(t.Context(), f.alicePersonID, items, 0)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	owners, err := f.store.PersonSweepAppreciationOwners(t.Context(), f.alicePersonID, peers, profile, 0, 5)
	require.NoError(t, err)
	require.Len(t, owners, 1)
	assert.Equal(t, ownerID, owners[0].Ref.MessageID)
	assert.Equal(t, "Thank you for helping me move.", owners[0].ExactExcerpt)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET source_is_from_me = NULL, is_from_me = TRUE WHERE id = ?`), ownerID)
	require.NoError(t, err)
	owners, err = f.store.PersonSweepAppreciationOwners(t.Context(), f.alicePersonID, peers, profile, 0, 5)
	require.NoError(t, err)
	assert.Empty(t, owners, "inferred ownership is not native direction")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET source_is_from_me = TRUE WHERE id = ?`), ownerID)
	require.NoError(t, err)
	metadata, err := json.Marshal(map[string]any{"member_count": 2, "roster_repair_pending": true})
	require.NoError(t, err)
	require.NoError(t, f.store.SetConversationMetadataContext(t.Context(), f.conversationID, sql.NullString{Valid: true, String: string(metadata)}))
	owners, err = f.store.PersonSweepAppreciationOwners(t.Context(), f.alicePersonID, peers, profile, 0, 5)
	require.NoError(t, err)
	assert.Empty(t, owners, "a pending roster cannot prove direct membership")
	require.NoError(t, f.store.SetConversationMetadataContext(t.Context(), f.conversationID, sql.NullString{Valid: true, String: `{}`}))
	owners, err = f.store.PersonSweepAppreciationOwners(t.Context(), f.alicePersonID, peers, profile, 0, 5)
	require.NoError(t, err)
	assert.Empty(t, owners, "two accumulated rows do not establish a complete provider roster")
	require.NoError(t, f.store.SetConversationMemberCountContext(t.Context(), f.conversationID, 3))
	owners, err = f.store.PersonSweepAppreciationOwners(t.Context(), f.alicePersonID, peers, profile, 0, 5)
	require.NoError(t, err)
	assert.Empty(t, owners, "a larger provider roster cannot be inferred to be direct")
	require.NoError(t, f.store.SetConversationMemberCountContext(t.Context(), f.conversationID, 2))
	third, err := f.store.EnsureParticipantContext(t.Context(), "third@example.test", "Third", "example.test")
	require.NoError(t, err)
	require.NoError(t, f.store.EnsureConversationParticipantContext(t.Context(), f.conversationID, third, "member"))
	owners, err = f.store.PersonSweepAppreciationOwners(t.Context(), f.alicePersonID, peers, profile, 0, 5)
	require.NoError(t, err)
	assert.Empty(t, owners, "extra accumulated identities refuse owner context")
}
