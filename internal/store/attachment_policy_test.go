package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInitSchemaAddsAttachmentOutcomeColumns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy-attachments.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.DB().Exec(`
		CREATE TABLE attachments (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL,
			content_hash TEXT,
			storage_path TEXT NOT NULL,
			thumbnail_hash TEXT,
			thumbnail_path TEXT
		)
	`)
	require.NoError(err)
	require.NoError(st.InitSchema())

	var state, reason sql.NullString
	_, err = st.DB().Exec(`INSERT INTO attachments (message_id, storage_path) VALUES (1, 'legacy:pending')`)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`
		SELECT attachment_state, attachment_skip_reason FROM attachments WHERE message_id = 1
	`).Scan(&state, &reason))
	assert.False(state.Valid)
	assert.False(reason.Valid)
}

// newPolicyMessage archives one message in a conversation whose roster the
// provider has read: participants is both the observed participant count and
// the archived membership record.
func newPolicyMessage(t *testing.T, st *store.Store, sourceType, identifier, conversationType, sourceMessageID string, participants int) (int64, int64) {
	t.Helper()
	source, err := st.GetOrCreateSource(sourceType, identifier)
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "conversation-"+sourceMessageID, conversationType, sourceMessageID)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`), participants, conversationID)
	require.NoError(t, err)
	require.NoError(t, st.SetConversationMemberCount(conversationID, participants))
	return source.ID, insertStoreTestMessage(t, st, source.ID, conversationID, sourceMessageID)
}

func TestAttachmentOutcomeRoundTrip(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, messageID := newPolicyMessage(t, st, "beeper", "signal", "direct_chat", "round-trip", 2)
	want := store.AttachmentRef{
		Filename: "photo.jpg", MimeType: "image/jpeg", Size: 2048,
		SourceAttachmentID: "beeper:asset-1", StoragePath: "https://example.invalid/asset-1",
		State: attachmentpolicy.StateSkipped, SkipReason: attachmentpolicy.SkipPolicyScope,
	}
	require.NoError(t, st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{want}))

	refs, err := st.MessageBeeperAttachments(messageID)
	require.NoError(t, err)
	// The replacement path normalizes role provenance and keys the occurrence
	// by its provider attachment ID; the typed outcome rides along unchanged.
	want.Role, want.RoleSource = store.AttachmentRoleUnknown, store.AttachmentRoleSourceUnknown
	want.SourcePartKey = want.SourceAttachmentID
	assert.Equal(t, want, refs[want.SourceAttachmentID])
}

func TestRetryableAttachmentMessagesReevaluateSkippedUnderCurrentPolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	sourceID, pendingID := newPolicyMessage(t, st, "beeper", "signal", "group_chat", "pending", 4)
	_, failedID := newPolicyMessage(t, st, "beeper", "signal", "group_chat", "failed", 4)
	_, skippedID := newPolicyMessage(t, st, "beeper", "signal", "group_chat", "skipped", 4)
	_, storedID := newPolicyMessage(t, st, "beeper", "signal", "group_chat", "stored", 4)

	for _, item := range []struct {
		messageID int64
		name      string
		state     attachmentpolicy.DownloadState
		hash      string
		path      string
	}{
		{messageID: pendingID, name: "pending", state: attachmentpolicy.StatePending, path: "https://example.invalid/pending"},
		{messageID: failedID, name: "failed", state: attachmentpolicy.StateFailed, path: "https://example.invalid/failed"},
		{messageID: skippedID, name: "skipped", state: attachmentpolicy.StateSkipped, path: "https://example.invalid/skipped"},
		{messageID: storedID, name: "stored", state: attachmentpolicy.StateStored, hash: strings.Repeat("ab", 32), path: "ab/" + strings.Repeat("ab", 32)},
	} {
		require.NoError(st.ReplaceMessageBeeperAttachments(item.messageID, []store.AttachmentRef{
			{SourceAttachmentID: "beeper:" + item.name, StoragePath: item.path, ContentHash: item.hash, State: item.state},
		}))
	}

	items, err := st.ListBeeperRetryableAttachmentMessages(sourceID, attachmentpolicy.Policy{
		MaxParticipants: 3,
	})
	require.NoError(err)
	assert.Equal([]store.BeeperPendingAttachmentMessage{
		{MessageID: pendingID, SourceMessageID: "pending", ChatID: "conversation-pending", ConversationType: "group_chat", ParticipantCount: 4},
		{MessageID: failedID, SourceMessageID: "failed", ChatID: "conversation-failed", ConversationType: "group_chat", ParticipantCount: 4},
	}, items)

	items, err = st.ListBeeperRetryableAttachmentMessages(sourceID, attachmentpolicy.Policy{})
	require.NoError(err)
	assert.Equal([]store.BeeperPendingAttachmentMessage{
		{MessageID: pendingID, SourceMessageID: "pending", ChatID: "conversation-pending", ConversationType: "group_chat", ParticipantCount: 4},
		{MessageID: failedID, SourceMessageID: "failed", ChatID: "conversation-failed", ConversationType: "group_chat", ParticipantCount: 4},
		{MessageID: skippedID, SourceMessageID: "skipped", ChatID: "conversation-skipped", ConversationType: "group_chat", ParticipantCount: 4},
	}, items)
}

func TestBeeperRetryPolicyReportsNewSizeSkips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	sourceID, overCapID := newPolicyMessage(t, st, "beeper", "signal", "direct_chat", "over-cap", 2)
	_, hugeID := newPolicyMessage(t, st, "beeper", "signal", "direct_chat", "huge", 2)
	_, scopeID := newPolicyMessage(t, st, "beeper", "signal", "channel", "scope", 8)
	for _, item := range []struct {
		messageID int64
		name      string
		size      int
	}{
		{messageID: overCapID, name: "over-cap", size: 9 << 20},
		{messageID: hugeID, name: "huge", size: math.MaxInt64 - 1},
		{messageID: scopeID, name: "scope", size: 1 << 20},
	} {
		require.NoError(st.ReplaceMessageBeeperAttachments(item.messageID, []store.AttachmentRef{{
			SourceAttachmentID: "beeper:" + item.name,
			StoragePath:        "https://example.invalid/" + item.name,
			Size:               item.size,
			State:              attachmentpolicy.StatePending,
		}}))
	}

	result, err := st.ApplyBeeperRetryableAttachmentPolicy(t.Context(), sourceID, attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeDirect, MaxBytes: 5 << 20,
	})
	require.NoError(err)
	assert.EqualValues(3, result.NewlySkipped)
	assert.EqualValues(2, result.AttachmentsOverCap)
	assert.Equal(int64(math.MaxInt64), result.AttachmentsOverCapBytes)
	assert.EqualValues(2, result.AttachmentsOverCapUnknownSize,
		"archived markers and saturated totals must be presented as lower bounds")
}

func TestExcludeAttachmentOccurrencesRemovesOnlySelectedBlobReference(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, excludedMessageID := newPolicyMessage(t, st, "beeper", "signal", "channel", "excluded", 20)
	_, retainedMessageID := newPolicyMessage(t, st, "beeper", "signal", "direct_chat", "retained", 2)
	hash := strings.Repeat("cd", 32)
	path := hash[:2] + "/" + hash
	for messageID, sourceID := range map[int64]string{excludedMessageID: "beeper:excluded", retainedMessageID: "beeper:retained"} {
		require.NoError(st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{{
			SourceAttachmentID: sourceID, StoragePath: path, ContentHash: hash,
			Size: 99, State: attachmentpolicy.StateStored,
		}}))
	}

	candidates, err := st.ListAttachmentPolicyCandidates(context.Background())
	require.NoError(err)
	require.Len(candidates, 2)
	var excluded store.AttachmentPolicyCandidate
	for _, candidate := range candidates {
		if candidate.MessageID == excludedMessageID {
			excluded = candidate
		}
	}
	require.NotZero(excluded.AttachmentID)
	require.NoError(st.ExcludeAttachmentOccurrences(context.Background(), []store.AttachmentExclusion{{
		AttachmentID: excluded.AttachmentID, Reason: attachmentpolicy.SkipPolicyScope,
	}}))

	var state, reason, contentHash, storagePath string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT attachment_state, attachment_skip_reason, COALESCE(content_hash, ''), storage_path
		FROM attachments WHERE id = ?`), excluded.AttachmentID).Scan(&state, &reason, &contentHash, &storagePath))
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipPolicyScope), reason)
	assert.Empty(contentHash)
	assert.Equal("excluded:beeper:excluded", storagePath)

	referenced, err := st.AttachmentBlobReferenced(context.Background(), hash, path)
	require.NoError(err)
	assert.True(referenced)
}

func TestAttachmentPolicyCandidatesIncludeLegacyAliasesSlackdumpAndTeamsInlineRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, slackMessageID := newPolicyMessage(t, st, "slack", "T01:U01", "channel", "legacy-alias", 5)
	_, slackdumpMessageID := newPolicyMessage(t, st, "slackdump", "T02:U02", "channel", "exported-file", 3)
	_, teamsMessageID := newPolicyMessage(t, st, "teams", "user@example.com", "direct_chat", "legacy-inline", 2)
	aliasHash := strings.Repeat("ab", 32)
	slackdumpHash := strings.Repeat("bc", 32)
	inlineHash := strings.Repeat("cd", 32)

	var aliasID int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO attachments (message_id, storage_path, content_hash, source_attachment_id)
		VALUES (?, ?, NULL, ?)
		RETURNING id
	`), slackMessageID, aliasHash[:2]+"/"+aliasHash, "slack:legacy-alias").Scan(&aliasID)
	require.NoError(err)
	var slackdumpID int64
	err = st.DB().QueryRow(st.Rebind(`
		INSERT INTO attachments (message_id, storage_path, content_hash, source_attachment_id, attachment_state)
		VALUES (?, ?, ?, ?, 'stored')
		RETURNING id
	`), slackdumpMessageID, slackdumpHash[:2]+"/"+slackdumpHash, slackdumpHash, "slack:F_EXPORT").Scan(&slackdumpID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE conversations SET participant_count = 99
		WHERE id = (SELECT conversation_id FROM messages WHERE id = ?)
	`), slackdumpMessageID)
	require.NoError(err)
	var inlineID int64
	err = st.DB().QueryRow(st.Rebind(`
		INSERT INTO attachments (message_id, storage_path, content_hash, filename, mime_type, source_attachment_id)
		VALUES (?, ?, ?, NULL, NULL, NULL)
		RETURNING id
	`), teamsMessageID, inlineHash[:2]+"/"+inlineHash, inlineHash).Scan(&inlineID)
	require.NoError(err)

	candidates, err := st.ListAttachmentPolicyCandidates(context.Background())
	require.NoError(err)
	require.Len(candidates, 3)
	byID := make(map[int64]store.AttachmentPolicyCandidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.AttachmentID] = candidate
	}
	assert.Equal(aliasHash, byID[aliasID].ContentHash)
	assert.Equal("slack:legacy-alias", byID[aliasID].SourceAttachmentID)
	assert.Equal(slackdumpHash, byID[slackdumpID].ContentHash)
	assert.Equal("slack:F_EXPORT", byID[slackdumpID].SourceAttachmentID)
	assert.Equal(3, byID[slackdumpID].ParticipantCount, "Slackdump roster snapshot is authoritative")
	assert.Equal(inlineHash, byID[inlineID].ContentHash)
	assert.Equal(fmt.Sprintf("teams:inline:legacy:%d", inlineID), byID[inlineID].SourceAttachmentID)

	require.NoError(st.ExcludeAttachmentOccurrences(context.Background(), []store.AttachmentExclusion{
		{AttachmentID: aliasID, Reason: attachmentpolicy.SkipPolicyScope, SourceAttachmentID: byID[aliasID].SourceAttachmentID},
		{AttachmentID: inlineID, Reason: attachmentpolicy.SkipPolicyScope, SourceAttachmentID: byID[inlineID].SourceAttachmentID},
	}))
	var sourceAttachmentID, state string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT source_attachment_id, attachment_state FROM attachments WHERE id = ?
	`), inlineID).Scan(&sourceAttachmentID, &state))
	assert.Equal(fmt.Sprintf("teams:inline:legacy:%d", inlineID), sourceAttachmentID)
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
}
