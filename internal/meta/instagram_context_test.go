package meta

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInstagramIdentityScopeAndPersonLinks(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	_, c := newInstagramImportFixture(t)
	imp := NewImporter(st, c)
	var senders []int64
	for _, app := range []string{"9", "99"} {
		src, err := imp.RegisterInstagram(t.Context(), app, "100", "200")
		require.NoError(err)
		_, err = imp.ImportInstagram(t.Context(), app, "100", "200", ImportOptions{})
		require.NoError(err)
		senders = append(senders, message(t, st, src.ID, "400").SenderID.Int64)
	}
	assert.NotEqual(senders[0], senders[1])
	key := instagramActorIdentifier("9", "100", "200", "ig_comment_from_user", "200")
	for _, other := range []string{
		instagramActorIdentifier("9", "101", "200", "ig_comment_from_user", "200"),
		instagramActorIdentifier("9", "100", "201", "ig_comment_from_user", "200"),
		instagramActorIdentifier("9", "100", "200", "ig_user", "200"), ActorIdentifier("9", "100", "200"),
	} {
		assert.NotEqual(key, other)
	}
	email, err := st.EnsureParticipant("example@example.com", "Example Customer", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(email)
	require.NoError(err)
	_, err = st.LinkParticipants(email, senders[0])
	require.NoError(err)
	linked, err := st.PersonForParticipants([]int64{senders[0]})
	require.NoError(err)
	assert.Equal(person.ID, linked.ID)
	observations, err := st.FindObservationsByProviderUserIDContext(t.Context(), key, 10)
	require.NoError(err)
	require.NotEmpty(observations)
	assert.Equal(senders[0], observations[0].ParticipantID)
}

func TestInstagramReplayRefusesChangedContext(t *testing.T) {
	for _, field := range []string{"version", "app_id", "page_id", "instagram_account_id", "media_id", "parent_id"} {
		t.Run(field, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, c := newInstagramImportFixture(t)
			imp := NewImporter(st, c)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			require.NoError(err)
			prior := message(t, st, src.ID, "400")
			saved, err := st.GetMessageMetadata(prior.ID)
			require.NoError(err)
			var metadata map[string]any
			require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
			if field == "version" {
				metadata[field] = 99
			} else {
				metadata[field] = "999"
			}
			encoded, err := json.Marshal(metadata)
			require.NoError(err)
			require.NoError(st.SetMessageMetadata(prior.ID, sql.NullString{String: string(encoded), Valid: true}))
			f.comments[0].Text = "must not overwrite refused context"
			_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			require.ErrorContains(err, `"400"`)
			body, err := st.GetMessageBodyText(t.Context(), prior.ID)
			require.NoError(err)
			assert.Equal("searchableorchard", body)
			assert.Equal(prior.ReplyToMessageID, message(t, st, src.ID, "400").ReplyToMessageID)
		})
	}
}

// Synthetic declared-parent capability; no real Graph depth is asserted.
func TestInstagramDeclaredParentsAndNestedPagination(t *testing.T) {
	for _, mode := range []string{"declared parent differs from frame", "three nested siblings", "missing parent", "self parent"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, c := newInstagramImportFixture(t)
			ids := []string{"500"}
			switch mode {
			case "declared parent differs from frame":
				f.comments = append(f.comments, f.replies[0])
				f.replies = nil
			case "three nested siblings":
				f.replies = append(f.replies, IGComment{ID: "501", ParentID: "400"}, IGComment{ID: "502", ParentID: "400"})
				ids = append(ids, "501", "502")
			case "missing parent":
				f.replies[0].ParentID = "999"
			case "self parent":
				f.replies[0].ParentID = "500"
			}
			imp := NewImporter(st, c)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			if mode == "missing parent" || mode == "self parent" {
				require.ErrorContains(err, `"500"`)
				rows, err := st.MessageExistsBatch(src.ID, ids)
				require.NoError(err)
				assert.Empty(rows)
				return
			}
			require.NoError(err)
			parent := message(t, st, src.ID, "400")
			for _, id := range ids {
				child := message(t, st, src.ID, id)
				assert.Equal(parent.ID, child.ReplyToMessageID.Int64)
				assert.Equal(parent.ConversationID, child.ConversationID)
			}
		})
	}
}
