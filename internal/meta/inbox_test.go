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

var inboxTestCases = []struct {
	name, platform, owner string
	inbox                 Inbox
}{
	{"messenger", "messenger", "100", Inbox{App: "9", Page: "100"}},
	{"instagram", "instagram", "200", Inbox{App: "9", Page: "100", Account: "200"}},
}

// inboxPages serves the Page and linked account reads, and the conversation
// pages keyed by their after cursor, and answers any other cursor with a 400.
// It records each cursor it was asked for.
func inboxPages(t *testing.T, pages map[string]string, afters *[]string) *Client {
	t.Helper()
	return fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/100/conversations":
			after := r.URL.Query().Get("after")
			*afters = append(*afters, after)
			body, ok := pages[after]
			if !ok {
				http.Error(w, "invalid fixture cursor", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, body)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
}

// inboxConversation renders one Graph conversation. messages maps each
// message ID to its sender's ID.
func inboxConversation(id string, participants []string, messages map[string]string) string {
	ps := make([]string, 0, len(participants))
	for _, p := range participants {
		ps = append(ps, fmt.Sprintf(`{"id":%q,"name":"Example %s"}`, p, p))
	}
	ms := make([]string, 0, len(messages))
	for _, mid := range slices.Sorted(maps.Keys(messages)) {
		ms = append(ms, fmt.Sprintf(`{"id":%q,"created_time":"2026-10-01T12:00:00+0000","from":{"id":%q},"message":"synthetic text"}`, mid, messages[mid]))
	}
	return fmt.Sprintf(`{"id":%q,"participants":{"data":[%s]},"messages":{"data":[%s]}}`, id, strings.Join(ps, ","), strings.Join(ms, ","))
}

func TestInboxImportSkipsConversationsThatAreNotOneToOne(t *testing.T) {
	for _, tc := range inboxTestCases {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			var afters []string
			first := strings.Join([]string{
				inboxConversation("t_group", []string{tc.owner, "701", "702"}, map[string]string{"m_g1": "701", "m_g2": "702"}),
				inboxConversation("t_ok", []string{"700", tc.owner}, map[string]string{"m_ok": "700"}),
				inboxConversation("t_alone", []string{tc.owner}, map[string]string{"m_alone": tc.owner}),
				inboxConversation("t_other", []string{"703", "704"}, map[string]string{"m_other": "703"}),
			}, ",")
			c := inboxPages(t, map[string]string{
				"":   `{"data":[` + first + `],"paging":{"cursors":{"after":"c2"},"next":"https://example.test/unused"}}`,
				"c2": `{"data":[` + inboxConversation("t_2", []string{tc.owner, "705"}, map[string]string{"m_next": "705"}) + `]}`,
			}, &afters)
			imp := NewInboxImporter(st, c)
			src, err := imp.Register(t.Context(), tc.inbox)
			require.NoError(err)

			sum, err := imp.Import(t.Context(), tc.inbox, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(3, sum.ConversationsSkipped)
			assert.Equal(2, sum.MessagesProcessed)
			assert.Equal([]string{"", "c2"}, afters, "the sync reads past the page with skipped conversations")
			ids, err := st.MessageExistsBatch(src.ID, []string{"m_ok", "m_next", "m_g1", "m_g2", "m_alone", "m_other"})
			require.NoError(err)
			assert.ElementsMatch([]string{"m_ok", "m_next"}, slices.Collect(maps.Keys(ids)))
		})
	}
}

func TestInboxImportRefusesPageWithoutOwner(t *testing.T) {
	for _, tc := range inboxTestCases {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			var afters []string
			c := inboxPages(t, map[string]string{
				"": `{"data":[` + inboxConversation("t_a", []string{"700", "799"}, map[string]string{"m_a": "700"}) + `,` +
					inboxConversation("t_b", []string{"701", "799", "702"}, map[string]string{"m_b": "701"}) + `]}`,
			}, &afters)
			imp := NewInboxImporter(st, c)
			src, err := imp.Register(t.Context(), tc.inbox)
			require.NoError(err)

			_, err = imp.Import(t.Context(), tc.inbox, ImportOptions{})
			require.Error(err)
			assert.Contains(err.Error(), "check which account the Page is linked to")
			assert.Contains(err.Error(), `"t_a"`)
			ids, err := st.MessageExistsBatch(src.ID, []string{"m_a", "m_b"})
			require.NoError(err)
			assert.Empty(ids)
		})
	}
}

func TestInboxImportRejectedSavedPositionNamesFull(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	inbox := Inbox{App: "9", Page: "100"}
	var afters []string
	pages := map[string]string{
		"": `{"data":[` + inboxConversation("t_1", []string{"100", "700"}, map[string]string{"m_1": "700"}) + `],"paging":{"cursors":{"after":"c2"},"next":"https://example.test/unused"}}`,
	}
	imp := NewInboxImporter(testutil.NewSQLiteTestStore(t), inboxPages(t, pages, &afters))
	_, err := imp.Register(t.Context(), inbox)
	require.NoError(err)
	sum, err := imp.Import(t.Context(), inbox, ImportOptions{MaxRequests: 1})
	require.NoError(err)
	require.True(sum.Partial)

	_, err = imp.Import(t.Context(), inbox, ImportOptions{})
	require.Error(err)
	assert.Equal([]string{"", "c2"}, afters)
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusBadRequest, apiErr.Status)
	assert.Contains(err.Error(), "run this sync again with --full")

	delete(pages, "")
	_, err = imp.Import(t.Context(), inbox, ImportOptions{Full: true})
	require.ErrorAs(err, &apiErr, "a 400 on the first page is not about a saved position")
	assert.NotContains(err.Error(), "--full")
}

func TestInboxImportSkipsMessagesWithoutText(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	inbox := Inbox{App: "9", Page: "100"}
	page := func(text string) string {
		return `{"data":[{"id":"t_1","participants":{"data":[{"id":"100"},{"id":"700","name":"Example Customer"}]},
			"messages":{"data":[{"id":"m_sticker","created_time":"2026-10-01T12:05:00+0000","from":{"id":"700"},"message":""},
			{"id":"m_text","created_time":"2026-10-01T12:00:00+0000","from":{"id":"700"},"message":"` + text + `"}]}}]}`
	}
	var afters []string
	pages := map[string]string{"": page("orchard question")}
	imp := NewInboxImporter(st, inboxPages(t, pages, &afters))
	src, err := imp.Register(t.Context(), inbox)
	require.NoError(err)

	sum, err := imp.Import(t.Context(), inbox, ImportOptions{})
	require.NoError(err)
	assert.Equal(1, sum.MessagesProcessed)
	assert.Equal(1, sum.MessagesAdded)
	ids, err := st.MessageExistsBatch(src.ID, []string{"m_text", "m_sticker"})
	require.NoError(err)
	assert.ElementsMatch([]string{"m_text"}, slices.Collect(maps.Keys(ids)))

	pages[""] = page("")
	sum, err = imp.Import(t.Context(), inbox, ImportOptions{})
	require.NoError(err)
	assert.Zero(sum.MessagesProcessed)
	body, err := st.GetMessageBodyText(t.Context(), ids["m_text"])
	require.NoError(err)
	assert.Equal("orchard question", body, "a message that comes back without text keeps its archived text")
}

func TestInboxImportRefusesOtherAccountsAndUnknownSenders(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	owner, firstFrom, visitor := "200", "", `,"username":"example_visitor"`
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/100/conversations":
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"t_1","participants":{"data":[{"id":%q,"username":"example_business"},{"id":"701"%s}]},
				"messages":{"data":[{"id":"m_1","created_time":"2026-10-02T09:00:00+0000"%s,"message":"no sender"},
				{"id":"m_2","created_time":"2026-10-02T09:01:00+0000","from":{"id":%q,"username":"example_business"},"message":"our reply"}]}}]}`, owner, visitor, firstFrom, owner)
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

	// The sender appears later without a name: search uses the name already
	// known for that participant.
	firstFrom, visitor = `,"from":{"id":"701"}`, ""
	_, err = imp.Import(t.Context(), inbox, ImportOptions{})
	require.NoError(err)
	var fromName string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT from_addr FROM messages_fts WHERE rowid = ?`, message(t, st, src.ID, "m_1").ID).Scan(&fromName))
	assert.Equal("example_visitor", fromName)
}
