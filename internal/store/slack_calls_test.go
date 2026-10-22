package store_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const archivedSlackHuddle = `{"type":"message","subtype":"huddle_thread","ts":"1790841600.1","team":"T_EXAMPLE","room":{"id":"R_EXAMPLE","date_start":1790841600,"date_end":0,"participant_history":["U_OWNER","U_PEER","U_UNRESOLVED"]}}`

func TestArchivedSlackHuddle(t *testing.T) {
	for _, sourceType := range []string{"slack", "slackdump"} {
		t.Run(sourceType, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(sourceType, "T_EXAMPLE:U_OWNER")
			require.NoError(err)
			peer, err := st.EnsureParticipantByIdentifier("slack", "T_EXAMPLE:U_PEER", "Example Peer")
			require.NoError(err)
			_, err = st.EnsureParticipantByIdentifier("slack", "T_EXAMPLE:U_OWNER", "Example Owner")
			require.NoError(err)
			_, err = st.EnsureParticipantByIdentifier("slack", "T_OTHER:U_UNRESOLVED", "Other Workspace")
			require.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(peer)
			require.NoError(err)
			convID, err := st.EnsureConversation(src.ID, "C_EXAMPLE", "Example channel")
			require.NoError(err)
			original, err := st.UpsertMessage(&store.Message{ConversationID: convID, SourceID: src.ID, SourceMessageID: "C_EXAMPLE:1790841600.1", MessageType: "slack"})
			require.NoError(err)
			require.NoError(st.UpsertMessageRawWithFormat(original, []byte(archivedSlackHuddle), "slack_json"))
			o, err := st.SlackHuddleObservationContext(t.Context(), original)
			require.NoError(err)
			assert.Equal("T_EXAMPLE:U_OWNER", o.Account)
			restricted := sourcescope.WithSourceIDs(t.Context(), []int64{})
			_, err = st.SlackHuddleObservationContext(restricted, original)
			require.ErrorIs(err, callhistory.ErrInvalid)
			_, err = st.DB().Exec(st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?`), []byte(archivedSlackHuddle), original)
			require.NoError(err)
			plain, err := st.SlackHuddleObservationContext(t.Context(), original)
			require.NoError(err)
			assert.Equal(o, plain)
			assert.Equal([]callhistory.Participant{{ID: peer, Participation: "unknown"}}, o.Participants)
			target, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
			require.NoError(err)
			first, err := st.PutCallObservationContext(t.Context(), target.ID, o)
			require.NoError(err)
			second, err := st.PutCallObservationContext(t.Context(), target.ID, o)
			require.NoError(err)
			assert.Equal(first, second)
			counts, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
			require.NoError(err)
			require.Len(counts, 1)
			assert.Zero(counts[0].Answered)
			assert.Equal(1, counts[0].Unknown)
			raw, err := st.GetMessageRaw(original)
			require.NoError(err)
			assert.True(bytes.Equal([]byte(archivedSlackHuddle), raw), "archived Slack JSON must be preserved byte-for-byte")
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), original)
			require.NoError(err)
			_, err = st.SlackHuddleObservationContext(t.Context(), original)
			require.ErrorIs(err, callhistory.ErrInvalid)
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?`), original)
			require.NoError(err)
			into, err := st.GetOrCreateSource("slack", "T_EXAMPLE:U_OTHER_OWNER")
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO source_settings (source_id, merged_into_source_id) VALUES (?, ?)`), src.ID, into.ID)
			require.NoError(err)
			_, err = st.SlackHuddleObservationContext(t.Context(), original)
			require.ErrorIs(err, store.ErrSourceRetired)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = st.SlackHuddleObservationContext(ctx, original)
			require.ErrorIs(err, context.Canceled)
		})
	}
}

func TestArchivedSlackHuddleRejectsMergedHistoryAfterInitialImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("slack", "T_EXAMPLE:U_OWNER")
	require.NoError(err)
	into, err := st.GetOrCreateSource("slack", "T_EXAMPLE:U_NEW_OWNER")
	require.NoError(err)
	peer, err := st.EnsureParticipantByIdentifier("slack", "T_EXAMPLE:U_PEER", "Example Peer")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	conversation, err := st.EnsureConversation(from.ID, "C_EXAMPLE", "Example channel")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: from.ID, ConversationID: conversation,
		SourceMessageID: "C_EXAMPLE:1790841600.1", MessageType: "slack",
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageRawWithFormat(messageID, []byte(archivedSlackHuddle), "slack_json"))

	observation, err := st.SlackHuddleObservationContext(t.Context(), messageID)
	require.NoError(err)
	callSource, err := st.GetOrCreateSource(callhistory.SourceType, observation.SourceIdentifier())
	require.NoError(err)
	_, err = st.PutCallObservationContext(t.Context(), callSource.ID, observation)
	require.NoError(err)

	merged, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID, IntoSourceID: into.ID,
	})
	require.NoError(err)
	assert.Equal(int64(1), merged.MessagesMoved)
	_, err = st.SlackHuddleObservationContext(t.Context(), messageID)
	require.ErrorIs(err, callhistory.ErrInvalid)

	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(err)
	require.Len(summary, 1)
	assert.Equal(1, summary[0].Unknown)
}

func TestArchivedSlackHuddleRejectsUnusableMessage(t *testing.T) {
	for _, tc := range []struct {
		name, source, format, raw string
		deleted                   bool
	}{
		{"wrong source", "discord", "slack_json", archivedSlackHuddle, false},
		{"wrong format", "slack", "mime", archivedSlackHuddle, false},
		{"malformed", "slack", "slack_json", `{`, false},
		{"oversized", "slack", "slack_json", strings.Repeat(" ", callhistory.MaxRawBytes+1), false},
		{"deleted", "slack", "slack_json", archivedSlackHuddle, true},
		{"missing raw", "slack", "", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(tc.source, "T_EXAMPLE:U_OWNER")
			require.NoError(err)
			convID, err := st.EnsureConversation(src.ID, "C_EXAMPLE", "Example channel")
			require.NoError(err)
			id, err := st.UpsertMessage(&store.Message{ConversationID: convID, SourceID: src.ID, SourceMessageID: "message", MessageType: "slack"})
			require.NoError(err)
			if tc.format != "" {
				require.NoError(st.UpsertMessageRawWithFormat(id, []byte(tc.raw), tc.format))
			}
			if tc.deleted {
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), id)
				require.NoError(err)
			}
			_, err = st.SlackHuddleObservationContext(t.Context(), id)
			require.ErrorIs(err, callhistory.ErrInvalid)
		})
	}
}
