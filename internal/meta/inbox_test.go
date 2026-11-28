package meta

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// inboxClient serves the Page and linked account reads, and answers each
// conversations read with page(r).
func inboxClient(t *testing.T, page func(r *http.Request) string) *Client {
	t.Helper()
	return fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/100/conversations":
			_, _ = fmt.Fprint(w, page(r))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
}

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
			imp := NewInboxImporter(st, inboxClient(t, func(r *http.Request) string {
				assert.Equal(tc.platform, r.URL.Query().Get("platform"))
				after := r.URL.Query().Get("after")
				afters = append(afters, after)
				if after == "" {
					return fmt.Sprintf(`{"data":[{"id":"t_1","participants":{"data":[{"id":"700","name":"Example Customer"},{"id":%q,"name":"Example Shopfront"}]},
						"messages":{"data":[{"id":"m_2","created_time":"2026-10-01T12:05:00+0000","from":{"id":%q},"message":%q},
						{"id":"m_sticker","created_time":"2026-10-01T12:02:00+0000","from":{"id":"700"},"message":""},
						{"id":"m_nosender","created_time":"2026-10-01T12:01:00+0000","message":"no sender"},
						{"id":"m_1","created_time":"2026-10-01T12:00:00+0000","from":{"id":"700","name":"Example Customer"},"message":"orchard question"}]}}],
						"paging":{"cursors":{"after":"c2"},"next":"https://example.test/unused"}}`, tc.owner, tc.owner, reply)
				}
				return fmt.Sprintf(`{"data":[{"id":"t_2","participants":{"data":[{"id":%q},{"id":"701","username":"example_visitor"}]},
					"messages":{"data":[{"id":"m_3","created_time":"2026-10-02T09:00:00+0000","from":{"id":"701"},"message":"hello"}]}}]}`, tc.owner)
			}))
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

			ids, err := st.MessageExistsBatch(src.ID, []string{"m_1", "m_2", "m_3", "m_sticker", "m_nosender"})
			require.NoError(err)
			assert.ElementsMatch([]string{"m_1", "m_2", "m_3", "m_nosender"}, slices.Collect(maps.Keys(ids)), "a message without text is not archived")
			question, answer, other := message(t, st, src.ID, "m_1"), message(t, st, src.ID, "m_2"), message(t, st, src.ID, "m_3")
			assert.Equal(question.ConversationID, answer.ConversationID)
			assert.NotEqual(question.ConversationID, other.ConversationID)
			assert.False(question.IsFromMe)
			assert.True(answer.IsFromMe)
			assert.False(message(t, st, src.ID, "m_nosender").IsFromMe, "a message without a sender is not credited to the owner")
			assert.Equal(tc.inbox.SourceType(), question.MessageType)
			var kind, key, title string
			var members, hits int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type, source_conversation_id, title,
				(SELECT count(*) FROM conversation_participants p WHERE p.conversation_id = c.id) FROM conversations c WHERE id=?`, question.ConversationID).Scan(&kind, &key, &title, &members))
			assert.Equal("direct_chat", kind)
			assert.Equal("700", key, "the customer's scoped ID keys the chat")
			assert.Equal("Example Customer", title)
			assert.Equal(2, members)
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'Shopfront'`).Scan(&hits))
			assert.Equal(1, hits, "the owner's messages are searchable by its name")

			reply = "edited reply"
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

func TestInboxImportArchivesOnlyOneToOneConversations(t *testing.T) {
	inbox := Inbox{App: "9", Page: "100", Account: "200"}
	// conversation renders one Graph conversation whose single message m_<id>
	// is from the first participant.
	conversation := func(id string, participants ...string) string {
		ps := make([]string, len(participants))
		for i, p := range participants {
			ps[i] = fmt.Sprintf(`{"id":%q}`, p)
		}
		return fmt.Sprintf(`{"id":%q,"participants":{"data":[%s]},"messages":{"data":[{"id":"m_%s","created_time":"2026-10-01T12:00:00+0000","from":{"id":%q},"message":"synthetic text"}]}}`,
			id, strings.Join(ps, ","), id, participants[0])
	}
	for _, tc := range []struct {
		name         string
		pages        map[string]string
		wantErr      string
		wantSkipped  int
		wantArchived []string
	}{
		{
			name: "skips conversations that are not one-to-one",
			pages: map[string]string{
				"": `{"data":[` + strings.Join([]string{conversation("group", "701", "200", "702"), conversation("ok", "700", "200"),
					conversation("alone", "200"), conversation("other", "703", "704")}, ",") + `],"paging":{"cursors":{"after":"c2"},"next":"https://example.test/unused"}}`,
				"c2": `{"data":[` + conversation("next", "705", "200") + `]}`,
			},
			wantSkipped: 3, wantArchived: []string{"m_ok", "m_next"},
		},
		{
			name:    "refuses a page where no conversation includes the owner",
			pages:   map[string]string{"": `{"data":[` + conversation("a", "700", "799") + "," + conversation("b", "701", "799", "702") + `]}`},
			wantErr: "check which account the Page is linked to",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			imp := NewInboxImporter(st, inboxClient(t, func(r *http.Request) string { return tc.pages[r.URL.Query().Get("after")] }))
			src, err := imp.Register(t.Context(), inbox)
			require.NoError(err)

			sum, err := imp.Import(t.Context(), inbox, ImportOptions{})
			if tc.wantErr != "" {
				require.ErrorContains(err, tc.wantErr)
			} else {
				require.NoError(err)
				assert.False(sum.Partial, "the sync reads past the page with skipped conversations")
				assert.Equal(tc.wantSkipped, sum.ConversationsSkipped)
			}
			ids, err := st.MessageExistsBatch(src.ID, []string{"m_group", "m_ok", "m_alone", "m_other", "m_next", "m_a", "m_b"})
			require.NoError(err)
			assert.ElementsMatch(tc.wantArchived, slices.Collect(maps.Keys(ids)))
		})
	}
}
