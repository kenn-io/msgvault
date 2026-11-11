package x

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXReplyRecoveryAfterConversationConsolidation(t *testing.T) {
	for _, tc := range []struct {
		name, conversation string
		repair, matches    bool
	}{
		{"adoption alias", "202", false, true},
		{"adoption foreign alias", "999", false, false},
		{"accepted repair alias", "202", true, true},
		{"accepted repair foreign alias", "999", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			at := time.Now().UTC().Add(-time.Minute)
			root := store.XPersistPostInput{
				SourceID: f.source.ID, OwnerID: "1001", PostID: "202", AuthorID: "1002", ConversationID: "202",
				EditHistory: []string{"202"}, Text: "provisional root", CreatedAt: at, FetchedAt: at,
				Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: at},
			}
			saved, err := f.st.PersistXPostContext(t.Context(), root)
			require.NoError(err)
			provisional, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "202")
			require.NoError(err)
			root.EditHistory = []string{"200", "202"}
			root.FetchedAt = at.Add(time.Second)
			_, err = f.st.PersistXPostContext(t.Context(), root)
			require.NoError(err)
			target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "202")
			require.NoError(err)
			assert.NotEqual(provisional.ConversationID, target.ConversationID)
			f.draft, err = f.st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, f.draft.Body, func(string, string) error { return nil })
			require.NoError(err)
			assert.Equal("200", f.draft.SourceConversationID)
			if !tc.matches {
				// A different source's edit proof cannot authorize this attempt.
				foreign, err := f.st.GetOrCreateSource("x", "2001")
				require.NoError(err)
				foreignRoot := root
				foreignRoot.SourceID, foreignRoot.OwnerID = foreign.ID, "2001"
				foreignRoot.PostID, foreignRoot.ConversationID = "999", "999"
				foreignRoot.EditHistory = []string{"200", "999"}
				_, err = f.st.PersistXPostContext(t.Context(), foreignRoot)
				require.NoError(err)
			}
			if tc.repair {
				undo := sendFailure(t, f.st, "message_bodies", "TRUE")
				result, err := f.sender().Send(t.Context(), f.request())
				require.Error(err)
				assert.Equal("sent_archive_pending", result.Status)
				undo()
				post := Post{ID: "9001", AuthorID: "1001", ConversationID: tc.conversation, Text: "provider reply", CreatedAt: at, EditHistory: []string{"9001"}, References: []Reference{{Type: "replied_to", ID: "202"}}}
				page := Page{Posts: []Post{post}, Users: []User{{ID: "1001", Username: "example_owner", ProtectedKnown: true}}, FetchedAt: at.Add(time.Minute)}
				require.NoError(WritePage(t.Context(), f.st, f.source.ID, "1001", page, page.FetchedAt))
			} else {
				f.post = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_, err := f.sender().Send(t.Context(), f.request())
				require.Error(err)
				f.get = func(w http.ResponseWriter, _ *http.Request) {
					_, _ = fmt.Fprintf(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":%q,"text":"provider reply","created_at":%q,"referenced_tweets":[{"type":"replied_to","id":"202"}],"edit_history_tweet_ids":["9001"]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]}}`, tc.conversation, at.Format(time.RFC3339Nano))
				}
			}
			sendReadOnly(t, f)
			req := f.request()
			if !tc.repair {
				req.AdoptPost = "9001"
			}
			prepared, prepareErr := f.sender().PrepareReply(t.Context(), req)
			result, sendErr := f.sender().Send(t.Context(), req)
			if tc.matches {
				require.NoError(prepareErr)
				require.NoError(sendErr)
				mode := "adopt"
				if tc.repair {
					mode = "repair"
				}
				assert.Equal(mode, prepared.Mode)
				assert.Equal("sent", result.Status)
				assert.Equal("9001", result.RemoteID)
				reply, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
				require.NoError(err)
				assert.Equal(target.ConversationID, reply.ConversationID)
				body, err := f.st.GetMessageBodyText(t.Context(), reply.MessageID)
				require.NoError(err)
				assert.Equal("provider reply", body)
			} else {
				require.Error(sendErr)
				if !tc.repair {
					require.Error(prepareErr)
					assert.Equal("remote_unknown", result.Status)
				} else {
					require.NoError(prepareErr)
					assert.Equal("sent_archive_pending", result.Status)
				}
			}
			assert.Equal(int32(1), f.posts.Load(), "recovery must never issue another POST")
		})
	}
}
