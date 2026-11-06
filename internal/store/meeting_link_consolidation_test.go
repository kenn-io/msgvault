package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

func TestMeetingLinksDedupUndoAndPurge(t *testing.T) {
	for _, targetSide := range []bool{false, true} {
		t.Run(fmt.Sprintf("target=%t", targetSide), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			uid := "dedup@example.org"
			if targetSide {
				uid = ""
			}
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			survivor := note
			var duplicate int64
			if targetSide {
				_, duplicate = calendarFixture(t, st, "duplicate", singleLinkRaw("duplicate", uid))
				survivor = event
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: duplicate}))
				var target int64
				requirements.NoError(st.db.QueryRow(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&target))
				requirements.Equal(duplicate, target)
				requirements.Equal(fmt.Sprintf("evt:%d", duplicate), linkedMeetingRefs(t, st, note)[note])
			} else {
				_, duplicate = projectionFixture(t, st, "duplicate", "plaud_json", `{}`)
				requirements.NoError(st.LinkMeetingContext(t.Context(), duplicate, OccurrenceRef{MessageID: event}))
			}
			_, err := st.MergeDuplicates(survivor, []int64{duplicate}, "meetings")
			requirements.NoError(err)
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
			count, err := st.UndoDedup("meetings")
			requirements.NoError(err)
			assertions.Equal(int64(1), count)
			if targetSide {
				assertions.Equal(fmt.Sprintf("evt:%d,evt:%d", event, duplicate), linkedMeetingRefs(t, st, note)[note])
			} else {
				keys := linkedMeetingRefs(t, st, note, duplicate)
				assertions.Equal(fmt.Sprintf("evt:%d", event), keys[note])
				assertions.Equal(fmt.Sprintf("evt:%d", event), keys[duplicate])
			}
			_, err = st.MergeDuplicates(survivor, []int64{duplicate}, "purge")
			requirements.NoError(err)
			count, err = st.DeleteDedupedBatchesContext(t.Context(), []string{"purge"})
			requirements.NoError(err)
			assertions.Equal(int64(1), count)
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksSourceMessageIDMigration(t *testing.T) {
	for _, targetSide := range []bool{false, true} {
		t.Run(fmt.Sprintf("target=%t", targetSide), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			uid := "migration@example.org"
			if targetSide {
				uid = ""
			}
			eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", uid))
			noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
			var source, conversation int64
			oldKey, newKey := "legacy-note", "note"
			if targetSide {
				oldData, old := calendarFixture(t, st, "legacy-event", singleLinkRaw("legacy-event", uid))
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: old}))
				var target int64
				requirements.NoError(st.db.QueryRow(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&target))
				requirements.Equal(old, target)
				requirements.Equal(fmt.Sprintf("evt:%d", old), linkedMeetingRefs(t, st, note)[note])
				source, conversation = eventData.Message.SourceID, oldData.Message.ConversationID
				oldKey, newKey = "legacy-event", "event"
			} else {
				oldData, old := projectionFixture(t, st, "legacy-note", "plaud_json", `{}`)
				requirements.NoError(st.LinkMeetingContext(t.Context(), old, OccurrenceRef{MessageID: event}))
				source, conversation = noteData.Message.SourceID, oldData.Message.ConversationID
			}
			requirements.NoError(st.MigrateSourceMessageID(source, conversation, oldKey, newKey))
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
		})
	}
}

func TestMeetingLinksSourceMerge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "merge@example.org"))
	data, note := projectionFixture(t, st, "same-note", "plaud_json", `{}`)
	source, err := st.GetOrCreateSource("meeting_import", "copy@example.com")
	requirements.NoError(err)
	sender, err := st.EnsureParticipant("speaker@example.com", "Speaker Example", "")
	requirements.NoError(err)
	data.Message.SenderID.Int64 = sender
	data.Message.SenderID.Valid = true
	data.Message.SentAt.Time = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	data.Message.SentAt.Valid = true
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	data.Conversation.SourceConversationID = "!meeting:example.org"
	_, err = st.db.Exec(`UPDATE conversations SET source_conversation_id = ? WHERE id = ?`, data.Conversation.SourceConversationID, data.Message.ConversationID)
	requirements.NoError(err)
	data.Message.ID = 0
	data.Message.SourceID = source.ID
	data.Message.ConversationID = 0
	duplicate, err := st.PersistMessage(data)
	requirements.NoError(err)
	requirements.NoError(st.LinkMeetingContext(t.Context(), duplicate, OccurrenceRef{MessageID: event}))
	var originalSource int64
	requirements.NoError(st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&originalSource))
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: originalSource})
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, st, note)[note])
}

func TestMeetingSubsetDecisions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	data, event := calendarFixture(t, st, "event", singleLinkRaw("event", "subset@example.org"))
	data.Message.SentAt.Time = time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	data.Message.SentAt.Valid = true
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	noteData, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	noteData.Message.SentAt.Time = time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)
	noteData.Message.SentAt.Valid = true
	_, err = st.UpsertMessage(noteData.Message)
	requirements.NoError(err)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: event}))
	for _, limit := range []int{1, 2} {
		dir := t.TempDir()
		_, err := CopySubset(path, dir, limit, false)
		requirements.NoError(err)
		dst, err := OpenForTest(filepath.Join(dir, "msgvault.db"))
		requirements.NoError(err)
		var count int
		requirements.NoError(dst.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
		assertions.Equal(limit-1, count)
		if limit == 2 {
			assertions.Equal(fmt.Sprintf("evt:%d", event), linkedMeetingRefs(t, dst, note)[note])
		}
		requirements.NoError(dst.Close())
	}
}

func TestMeetingLinksUniqueNoteSourceMerge(t *testing.T) {
	for _, mode := range []string{"automatic", "saved", "new decision"} {
		t.Run(mode, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "event", singleLinkRaw("event", "merge@example.org"))
			var data *MessagePersistData
			var note int64
			if mode == "automatic" {
				data, note = identityNote(t, st, "unique-note", "merge@example.org")
			} else {
				data, note = projectionFixture(t, st, "unique-note", "meeting_json", meetingProjectionRaw)
			}
			target := OccurrenceRef{MessageID: event}
			if mode == "saved" {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, target))
			}
			destination, err := st.GetOrCreateSource("meeting_import", "empty@example.com")
			requirements.NoError(err)
			merged, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: data.Message.SourceID, IntoSourceID: destination.ID})
			requirements.NoError(err)
			requirements.Equal(int64(1), merged.MessagesMoved)
			requirements.Zero(merged.DuplicatesHidden)
			var archived bool
			requirements.NoError(st.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM source_merge_archive_only_messages WHERE message_id=?)`, note).Scan(&archived))
			requirements.True(archived)
			ids := []int64{note}
			result, err := st.GetMeetingContextContext(t.Context(), MeetingQueryScope{MessageIDs: &ids}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 1 << 20})
			requirements.NoError(err, "a unique archived note remains readable")
			var packet meetingcontent.Packet
			requirements.NoError(json.Unmarshal([]byte(result.Content), &packet))
			requirements.Len(packet.Meetings, 1)
			assertions.Equal(note, packet.Meetings[0].Meeting.MessageID)
			if mode == "new decision" {
				requirements.NoError(st.LinkMeetingContext(t.Context(), note, target))
			}
			ref := formatMeetingOccurrence(OccurrenceRef{MessageID: event})
			links, err := meetingLinksForTest(t, st, note)
			requirements.NoError(err, "a unique archived note remains readable through meeting links")
			requirements.Len(links, 1)
			assertions.Equal(ref, links[0].Ref)
			if mode == "automatic" {
				_, err = st.db.Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, note)
				requirements.NoError(err)
				_, err = meetingLinksForTest(t, st, note)
				requirements.ErrorIs(err, sql.ErrNoRows)
			}
		})
	}
}
