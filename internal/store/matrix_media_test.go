package store_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestListMatrixRetryableAttachmentMessagesIncludesDeletedAndFailedAndClampsLimit(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	room, err := s.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Media")
	req.NoError(err)
	var ids []int64
	for _, state := range []attachmentpolicy.DownloadState{attachmentpolicy.StatePending, attachmentpolicy.StateFailed, attachmentpolicy.StatePending, attachmentpolicy.StateStored} {
		messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: string(rune('a' + len(ids))), MessageType: "matrix"})
		req.NoError(err)
		ids = append(ids, messageID)
		req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/media", StoragePath: "mxc://example.org/media", State: state}}))
	}
	req.NoError(s.MarkMessageDeleted(source.ID, "c"))
	items, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, 0, 100, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(items, 3)
	check.Equal(ids[0], items[0].MessageID)
	check.Equal(ids[1], items[1].MessageID)
	check.Equal(ids[2], items[2].MessageID)
	items, err = s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, ids[0], 1, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(items, 1)
	check.Equal(ids[1], items[0].MessageID)
	items, err = s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, ids[1], 100, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(items, 1)
	check.Equal(ids[2], items[0].MessageID)
	for i := range 102 {
		messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: fmt.Sprintf("$media%d", i), MessageType: "matrix"})
		req.NoError(err)
		req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/media", StoragePath: "mxc://example.org/media", State: attachmentpolicy.StatePending}}))
	}
	for _, limit := range []int{-1, 0, 101} {
		items, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, ids[3], limit, attachmentpolicy.Policy{})
		req.NoError(err)
		req.Len(items, 100)
		check.Equal("!room:example.org", items[0].ChatID)
		check.Equal("$media0", items[0].SourceMessageID)
		tail, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, items[99].MessageID, limit, attachmentpolicy.Policy{})
		req.NoError(err)
		check.Len(tail, 2)
	}
}

func TestMatrixMediaReplacementAndExclusionRecomputeSizeEstimate(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	_, messageID := newPolicyMessage(t, s, "matrix", "@archive:example.org", "group_chat", "media", 4)
	body := "sixteen bytes!!!"
	req.Len(body, 16)
	req.NoError(s.UpdateMessageDerivedText(messageID, sql.NullString{String: body, Valid: true}, sql.NullString{}, sql.NullString{String: body, Valid: true}, store.FTSDoc{Body: body}))
	ref := store.AttachmentRef{SourceAttachmentID: "matrix:mxc://example.org/media", StoragePath: "aa/media", ContentHash: strings.Repeat("a", 64), Size: 100, State: attachmentpolicy.StateStored}
	req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{ref}))
	var size int64
	req.NoError(s.DB().QueryRow(s.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messageID).Scan(&size))
	check.Equal(int64(116), size)
	candidates, err := s.ListAttachmentPolicyCandidates(t.Context())
	req.NoError(err)
	req.Len(candidates, 1)
	check.Equal("matrix", candidates[0].SourceType)
	check.Equal(ref.SourceAttachmentID, candidates[0].SourceAttachmentID)
	req.NoError(s.ExcludeAttachmentOccurrences(t.Context(), []store.AttachmentExclusion{{AttachmentID: candidates[0].AttachmentID, Reason: attachmentpolicy.SkipPolicyScope}}))
	req.NoError(s.DB().QueryRow(s.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messageID).Scan(&size))
	check.Equal(int64(16), size)
}

func TestMatrixRetryDirectScope(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	var expected []int64
	for i, kind := range []string{"group_chat", "direct_chat", "channel"} {
		room, err := s.EnsureConversationWithType(source.ID, fmt.Sprintf("!room%d:example.org", i), kind, "Media")
		req.NoError(err)
		req.NoError(s.SetConversationMemberCount(room, 2))
		for _, state := range []attachmentpolicy.DownloadState{attachmentpolicy.StateSkipped, attachmentpolicy.StatePending, attachmentpolicy.StateFailed} {
			messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: fmt.Sprintf("$%d-%s", i, state), MessageType: "matrix"})
			req.NoError(err)
			req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/media", StoragePath: "matrix:mxc://example.org/media", State: state}}))
			if kind == "direct_chat" || state != attachmentpolicy.StateSkipped {
				expected = append(expected, messageID)
			}
		}
	}
	var got []int64
	var after int64
	for {
		items, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, after, 1, attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect, MaxParticipants: 20})
		req.NoError(err)
		if len(items) == 0 {
			break
		}
		req.Len(items, 1)
		after = items[0].MessageID
		got = append(got, after)
	}
	check.Equal(expected, got)
}

func TestMatrixRetrySkipsRequireResolvedRoster(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	room, err := s.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Media")
	req.NoError(err)
	messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "$media", MessageType: "matrix"})
	req.NoError(err)
	req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/media", StoragePath: "mxc://example.org/media", State: attachmentpolicy.StateSkipped, SkipReason: attachmentpolicy.SkipParticipantThreshold}}))
	policy := attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxParticipants: 20}
	for _, scenario := range []struct {
		metadata string
		limit    int
		want     int
	}{
		{`{}`, 20, 0},
		{`{"member_count_unknown":true}`, 20, 0},
		{`{"member_count":30}`, 20, 0},
		{`{"member_count":2,"member_count_unknown":true}`, 20, 0},
		{`{"member_count":2}`, 20, 1},
		{`{"member_count":30}`, 40, 1},
		{`{"member_count":30,"member_count_unknown":true}`, 0, 1},
	} {
		_, err = s.DB().Exec(s.Rebind(`UPDATE conversations SET metadata = ?, participant_count = 30 WHERE id = ?`), scenario.metadata, room)
		req.NoError(err)
		policy.MaxParticipants = scenario.limit
		items, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, 0, 100, policy)
		req.NoError(err)
		check.Len(items, scenario.want, scenario.metadata)
	}
}

func TestMatrixRetryPagesAdvancePastExcludedRows(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	room, err := s.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Media")
	req.NoError(err)
	req.NoError(s.SetConversationMemberCount(room, 30))
	var eligible []int64
	for i, kind := range []string{"roster", "size", "alias", "pending", "roster", "pending", "failed"} {
		messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: fmt.Sprintf("$media%d", i), MessageType: "matrix"})
		req.NoError(err)
		ref := store.AttachmentRef{SourceAttachmentID: "matrix:mxc://example.org/file", StoragePath: "mxc://example.org/file", State: attachmentpolicy.StateSkipped}
		switch kind {
		case "size":
			ref.Size = 101
		case "alias":
			ref.StoragePath = "aa/" + strings.Repeat("a", 64)
		case "pending":
			ref.State = attachmentpolicy.StatePending
			eligible = append(eligible, messageID)
		case "failed":
			ref.State = attachmentpolicy.StateFailed
			eligible = append(eligible, messageID)
		}
		req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{ref}))
	}
	policy := attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxParticipants: 20, MaxBytes: 100}
	logs := captureAttachmentQueryLogs(t)
	items, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, 0, 1, policy)
	req.NoError(err)
	req.Len(items, 1)
	check.Equal(eligible[0], items[0].MessageID)
	check.Equal(1, strings.Count(logs.String(), `"kind":"query"`), "SQL excludes ineligible skipped rows before paging")
	items, err = s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, items[0].MessageID, 1, policy)
	req.NoError(err)
	req.Len(items, 1)
	check.Equal(eligible[1], items[0].MessageID)
	items, err = s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), source.ID, items[0].MessageID, 1, policy)
	req.NoError(err)
	req.Len(items, 1)
	check.Equal(eligible[2], items[0].MessageID)
}

func TestMatrixOutstandingMedia(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	other, err := s.GetOrCreateSource("matrix", "@other:example.org")
	req.NoError(err)
	for i, item := range []struct {
		account, room string
		state         attachmentpolicy.DownloadState
		deleted       bool
	}{
		{source.Identifier, "!room:example.org", attachmentpolicy.StatePending, false},
		{source.Identifier, "!room:example.org", attachmentpolicy.StateFailed, false},
		{source.Identifier, "!room:example.org", attachmentpolicy.StateSkipped, false},
		{source.Identifier, "!excluded:example.org", attachmentpolicy.StatePending, false},
		{source.Identifier, "!room:example.org", attachmentpolicy.StatePending, true},
		{other.Identifier, "!room:example.org", attachmentpolicy.StateFailed, false},
	} {
		source, err := s.GetSourceByTypeAndIdentifier("matrix", item.account)
		req.NoError(err)
		room, err := s.EnsureConversation(source.ID, item.room, "Media")
		req.NoError(err)
		id := fmt.Sprintf("$media%d", i)
		messageID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: id, MessageType: "matrix"})
		req.NoError(err)
		req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), messageID, []store.AttachmentRef{{SourceAttachmentID: "matrix:mxc://example.org/file", StoragePath: "mxc://example.org/file", State: item.state}}))
		if item.deleted {
			req.NoError(s.MarkMessageDeleted(source.ID, id))
		}
	}
	pending, failed, err := s.MatrixOutstandingMedia(t.Context(), source.ID, []string{"!room:example.org", "!excluded:example.org"}, []string{"!excluded:example.org"})
	req.NoError(err)
	check.Equal(int64(2), pending)
	check.Equal(int64(1), failed)
	pending, failed, err = s.MatrixOutstandingMedia(t.Context(), source.ID, []string{"!other:example.org"}, nil)
	req.NoError(err)
	check.Zero(pending)
	check.Zero(failed)
}

func TestMatrixProviderQueriesSkipMergedArchiveOnlyMessages(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	history, err := s.GetOrCreateSource("matrix", "@history:example.org")
	req.NoError(err)
	archive, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	room, err := s.EnsureConversationWithType(history.ID, "!room:example.org", "direct_chat", "History")
	req.NoError(err)
	persist := func(eventID, snippet, raw string) int64 {
		t.Helper()
		messageID, err := s.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{
				SourceID: history.ID, ConversationID: room, SourceMessageID: eventID,
				MessageType: "matrix", Snippet: sql.NullString{String: snippet, Valid: true},
			},
			BodyText: sql.NullString{String: snippet, Valid: true}, RawMIME: []byte(raw), RawFormat: "matrix_json",
		})
		req.NoError(err)
		return messageID
	}
	placeholderID := persist("$encrypted", store.MatrixEncryptedPlaceholder, `{"type":"m.room.encrypted","content":{"ciphertext":"opaque"}}`)
	legacyID := persist("$legacy", "file", `{"type":"m.room.message","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/legacy"}}`)
	retryID := persist("$retry", "file", `{"type":"m.room.message","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/retry"}}`)
	storedID := persist("$stored", "file", `{"type":"m.room.message","content":{"msgtype":"m.file","body":"file","url":"mxc://example.org/stored"}}`)
	req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), retryID, []store.AttachmentRef{{
		SourceAttachmentID: "matrix:mxc://example.org/retry", StoragePath: "mxc://example.org/retry", State: attachmentpolicy.StatePending,
	}}))
	hash := strings.Repeat("a", 64)
	req.NoError(s.ReplaceMessageMatrixAttachments(t.Context(), storedID, []store.AttachmentRef{{
		SourceAttachmentID: "matrix:mxc://example.org/stored", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 100, State: attachmentpolicy.StateStored,
	}}))
	_, err = s.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: archive.ID})
	req.NoError(err)

	rooms, err := s.ListMatrixRoomIDsContext(t.Context(), archive.ID)
	req.NoError(err)
	check.Empty(rooms)
	placeholders, err := s.MatrixPlaceholderRooms(t.Context(), archive.ID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	check.Empty(placeholders)
	page, scanned, err := s.MatrixPlaceholders(t.Context(), room, 0, 10)
	req.NoError(err)
	check.Empty(page)
	check.Equal(int64(0), scanned)
	legacy, err := s.ListMatrixLegacyMediaMessagesContext(t.Context(), archive.ID, 0, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	check.Empty(legacy)
	retry, err := s.ListMatrixRetryableAttachmentMessagesContext(t.Context(), archive.ID, 0, 100, attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll})
	req.NoError(err)
	check.Empty(retry)
	pending, failed, err := s.MatrixOutstandingMedia(t.Context(), archive.ID, nil, nil)
	req.NoError(err)
	check.Zero(pending)
	check.Zero(failed)
	candidates, err := s.ListAttachmentPolicyCandidates(t.Context())
	req.NoError(err)
	check.Empty(candidates)
	for _, messageID := range []int64{placeholderID, legacyID, retryID, storedID} {
		var archiveOnly bool
		req.NoError(s.DB().QueryRow(s.Rebind(`SELECT EXISTS (SELECT 1 FROM source_merge_archive_only_messages WHERE message_id = ?)`), messageID).Scan(&archiveOnly))
		check.True(archiveOnly)
	}
}
