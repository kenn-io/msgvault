package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInboxImportArchivesConversationsAsChats(t *testing.T) {
	for _, tc := range []struct {
		name, platform, owner string
		inbox                 Inbox
	}{
		{"messenger", "messenger", "100", Inbox{App: "9", Page: "100"}},
		{"instagram", "instagram", "200", Inbox{App: "9", Page: "100", Account: "200"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			reply := "first reply"
			var afters []string
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v26.0/me":
					_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
				case "/v26.0/100":
					_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
				case "/v26.0/100/conversations":
					assert.Equal(tc.platform, r.URL.Query().Get("platform"))
					after := r.URL.Query().Get("after")
					afters = append(afters, after)
					if after == "" {
						_, _ = fmt.Fprintf(w, `{"data":[{"id":"t_1","participants":{"data":[{"id":"700","name":"Example Customer"},{"id":%q,"name":"Example Page"}]},
							"messages":{"data":[{"id":"m_2","created_time":"2026-10-01T12:05:00+0000","from":{"id":%q},"message":%q},
							{"id":"m_1","created_time":"2026-10-01T12:00:00+0000","from":{"id":"700","name":"Example Customer"},"message":"orchard question"}]}}],
							"paging":{"cursors":{"after":"c2"},"next":"https://example.test/unused"}}`, tc.owner, tc.owner, reply)
						return
					}
					_, _ = fmt.Fprintf(w, `{"data":[{"id":"t_2","participants":{"data":[{"id":%q},{"id":"701","username":"example_visitor"}]},
						"messages":{"data":[{"id":"m_3","created_time":"2026-10-02T09:00:00+0000","from":{"id":"701"},"message":"hello"}]}}]}`, tc.owner)
				default:
					assert.Fail("unexpected Graph read", r.URL.Path)
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			})
			imp := NewInboxImporter(st, c)
			src, err := imp.Register(t.Context(), tc.inbox)
			require.NoError(err)
			assert.Equal(tc.inbox.SourceType(), src.SourceType)

			sum, err := imp.Import(t.Context(), tc.inbox, ImportOptions{MaxRequests: 1})
			require.NoError(err)
			assert.True(sum.Partial)
			sum, err = imp.Import(t.Context(), tc.inbox, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal([]string{"", "c2"}, afters, "the second run resumes at the saved cursor")

			question, answer, other := message(t, st, src.ID, "m_1"), message(t, st, src.ID, "m_2"), message(t, st, src.ID, "m_3")
			assert.Equal(question.ConversationID, answer.ConversationID)
			assert.NotEqual(question.ConversationID, other.ConversationID)
			assert.False(question.IsFromMe)
			assert.True(answer.IsFromMe)
			assert.Equal(tc.inbox.SourceType(), question.MessageType)
			var kind, key, title string
			var members int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type, source_conversation_id, title,
				(SELECT count(*) FROM conversation_participants p WHERE p.conversation_id = c.id) FROM conversations c WHERE id=?`, question.ConversationID).Scan(&kind, &key, &title, &members))
			assert.Equal("direct_chat", kind)
			assert.Equal("700", key, "the customer's scoped ID keys the chat")
			assert.Equal("Example Customer", title)
			assert.Equal(2, members)

			reply = "edited reply"
			afters = nil
			sum, err = imp.Import(t.Context(), tc.inbox, ImportOptions{})
			require.NoError(err)
			assert.Equal(0, sum.MessagesAdded)
			assert.Equal(1, sum.MessagesUpdated)
			body, err := st.GetMessageBodyText(t.Context(), answer.ID)
			require.NoError(err)
			assert.Equal("edited reply", body)
		})
	}
}

func TestInboxImportRefusesOtherAccountsAndUnknownSenders(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	owner := "200"
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/100/conversations":
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"t_1","participants":{"data":[{"id":%q,"username":"example_business"},{"id":"701","username":"example_visitor"}]},
				"messages":{"data":[{"id":"m_1","created_time":"2026-10-02T09:00:00+0000","message":"no sender"},
				{"id":"m_2","created_time":"2026-10-02T09:01:00+0000","from":{"id":%q,"username":"example_business"},"message":"our reply"}]}}]}`, owner, owner)
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	inbox := Inbox{App: "9", Page: "100", Account: "200"}
	imp := NewInboxImporter(st, c)
	src, err := imp.Register(t.Context(), inbox)
	require.NoError(err)

	owner = "299"
	_, err = imp.Import(t.Context(), inbox, ImportOptions{})
	require.Error(err, "a Page linked to another account must not fill this source")
	ids, err := st.MessageExistsBatch(src.ID, []string{"m_1", "m_2"})
	require.NoError(err)
	assert.Empty(ids)

	owner = "200"
	_, err = imp.Import(t.Context(), inbox, ImportOptions{})
	require.NoError(err)
	assert.False(message(t, st, src.ID, "m_1").IsFromMe, "a message without a sender is not credited to the account")
	reply := message(t, st, src.ID, "m_2")
	assert.True(reply.IsFromMe)
	var hits int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'example_business'`).Scan(&hits))
	assert.Equal(1, hits, "the account's own messages are searchable by its name")
}
