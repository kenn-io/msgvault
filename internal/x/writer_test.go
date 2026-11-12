package x

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWritePageAvailabilityAndAuthorRaw(t *testing.T) {
	for _, tc := range []struct {
		name, userField, availability string
		skip                          bool
	}{
		{name: "missing expansion", availability: "unverified"},
		{name: "missing protected", userField: `{"id":"1002","name":"Example Author","username":"example_author"}`, availability: "unverified"},
		{name: "null protected", userField: `{"id":"1002","name":"Example Author","username":"example_author","protected":null}`, availability: "unverified"},
		{name: "public", userField: `{"id":"1002","name":"Example Author","username":"example_author","protected":false}`, availability: "public"},
		{name: "protected", userField: `{"id":"1002","name":"Private Author","username":"private_author","protected":true}`, availability: "removed", skip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			_testRequire.NoError(err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				includes := ""
				if tc.userField != "" {
					includes = `,"includes":{"users":[` + tc.userField + `]}`
				}
				_, err := fmt.Fprint(w, `{"data":[{"id":"100","author_id":"1002","conversation_id":"100","text":"synthetic constellation","created_at":"2026-10-29T12:00:00Z"}]`+includes+`}`)
				assert.NoError(t, err)
			}))
			defer server.Close()
			page, err := NewClient(server.URL, server.Client()).Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			_testRequire.NoError(err)
			_testRequire.NoError(WritePage(t.Context(), st, source.ID, "1001", page, page.FetchedAt))
			target, err := st.ResolveXPostContext(t.Context(), source.ID, "100")
			_testRequire.NoError(err)
			_testAssert.Equal(tc.availability, target.Availability)
			if tc.skip {
				_testAssert.Zero(target.MessageID)
				pending, err := st.XCleanupPendingContext(t.Context())
				_testRequire.NoError(err)
				_testAssert.True(pending, "first-seen protected IDs retain their cleanup obligation")
				return
			}
			body, err := st.GetMessageBodyText(t.Context(), target.MessageID)
			_testRequire.NoError(err)
			_testAssert.Equal("synthetic constellation", body)
			raw, err := st.GetMessageRaw(target.MessageID)
			_testRequire.NoError(err)
			var envelope map[string]jsontext.Value
			_testRequire.NoError(json.Unmarshal(raw, &envelope))
			_testRequire.Contains(envelope, "post")
			if tc.userField == "" {
				_testAssert.NotContains(envelope, "author")
			} else {
				var author map[string]any
				_testRequire.NoError(json.Unmarshal(envelope["author"], &author))
				if tc.availability == "public" {
					_testAssert.Equal(false, author["protected"])
				} else {
					_testAssert.NotContains(author, "protected")
				}
			}
		})
	}
}

func TestWritePageSummaryDoesNotChangeBillableCounts(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	fetched := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	root := Post{ID: "100", AuthorID: "1001", ConversationID: "100", Text: "owner root", CreatedAt: fetched, EditHistory: []string{"100"}, Raw: jsontext.Value(`{"id":"100"}`)}
	retweet := Post{ID: "200", AuthorID: "1001", ConversationID: "200", Text: "filtered", CreatedAt: fetched, EditHistory: []string{"200"}, References: []Reference{{Type: "retweeted", ID: "300"}}, Raw: jsontext.Value(`{"id":"200"}`)}
	page := Page{Posts: []Post{root, retweet}, Users: []User{{ID: "1001", Name: "Example Owner", Username: "example_owner", ProtectedKnown: true}}, FetchedAt: fetched, PostCount: 2, UserCount: 1, ReservationID: "admitted-1"}
	summary, err := WritePageWithSummary(t.Context(), st, source.ID, "1001", page, fetched.Add(time.Hour))
	_testRequire.NoError(err)
	_testAssert.Equal(int64(1), summary.Added)
	_testAssert.Zero(summary.Updated)
	_testAssert.Equal(2, page.PostCount)
	_testAssert.Equal(1, page.UserCount)
	_testAssert.Equal("admitted-1", page.ReservationID)
	summary, err = WritePageWithSummary(t.Context(), st, source.ID, "1001", page, fetched.Add(2*time.Hour))
	_testRequire.NoError(err)
	_testAssert.Zero(summary.Added)
	_testAssert.Zero(summary.Updated)
	target, err := st.ResolveXPostContext(t.Context(), source.ID, "100")
	_testRequire.NoError(err)
	var owned bool
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT source_is_from_me FROM messages WHERE id=?"), target.MessageID).Scan(&owned))
	_testAssert.True(owned)
	raw, err := st.GetMessageMetadata(target.MessageID)
	_testRequire.NoError(err)
	var metadata map[string]any
	_testRequire.NoError(json.Unmarshal([]byte(raw.String), &metadata))
	_testAssert.Equal(fetched.Format(time.RFC3339), metadata["fetched_at"])
}

func TestWritePageKeepsQuotesAndIDBackedMentions(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	fetched := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	post := Post{ID: "100", AuthorID: "1002", ConversationID: "100", Text: "quoted example", CreatedAt: fetched, EditHistory: []string{"100"}, References: []Reference{{Type: "quoted", ID: "300"}}, Mentions: []Mention{{ID: "1003", Username: "example_mentioned"}, {Username: "bare_handle"}}, Raw: jsontext.Value(`{"id":"100"}`)}
	page := Page{Posts: []Post{post}, Users: []User{{ID: "1002", Name: "Example Author", Username: "example_author", ProtectedKnown: true}}, FetchedAt: fetched}
	_testRequire.NoError(WritePage(t.Context(), st, source.ID, "1001", page, fetched))
	target, err := st.ResolveXPostContext(t.Context(), source.ID, "100")
	_testRequire.NoError(err)
	var parents, mentions, bare int
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE reply_to_message_id IS NOT NULL").Scan(&parents))
	_testAssert.Zero(parents)
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM message_recipients WHERE recipient_type='to'").Scan(&mentions))
	_testAssert.Equal(1, mentions)
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_value='bare_handle'").Scan(&bare))
	_testAssert.Zero(bare)
	raw, err := st.GetMessageMetadata(target.MessageID)
	_testRequire.NoError(err)
	var metadata map[string]any
	_testRequire.NoError(json.Unmarshal([]byte(raw.String), &metadata))
	_testAssert.Equal("300", metadata["quoted_post_id"])
	_testAssert.NotContains(metadata, "referenced_message_id")
}

func TestWritePageUnknownProtectionKeepsKnownPrivateAuthor(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		postID, field := "100", `,"protected":true`
		if calls > 1 {
			postID = "200"
			field = ""
		}
		_, err := fmt.Fprintf(w, `{"data":[{"id":"%s","author_id":"1002","conversation_id":"%s","text":"not public proof","created_at":"2026-10-29T12:00:00Z"}],"includes":{"users":[{"id":"1002","name":"Unverified Name","username":"unverified_handle"%s}]}}`, postID, postID, field)
		assert.NoError(t, err)
	}))
	defer server.Close()
	client := NewClient(server.URL, server.Client())
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		page, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
		_testRequire.NoError(err)
		page.FetchedAt = at.Add(time.Duration(i) * time.Hour)
		_testRequire.NoError(WritePage(t.Context(), st, source.ID, "1001", page, page.FetchedAt))
	}
	target, err := st.ResolveXPostContext(t.Context(), source.ID, "200")
	_testRequire.NoError(err)
	_testAssert.NotEqual("public", target.Availability)
	_testAssert.Zero(target.MessageID)
	var handles int
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_contact_observations WHERE address_kind='username' AND superseded_at IS NULL AND active_until IS NULL").Scan(&handles))
	_testAssert.Zero(handles)
}

// Shrunk missing-implementation counterexample from TestEditOrderConverges:
// one canonical ID, one ordinary version, empty generated suffix text.
func TestXPropertyMinimalEditConvergence(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	post := Post{ID: "1", AuthorID: "1001", ConversationID: "1", Text: "version 0 ", LongText: "version 0 ", CreatedAt: at, EditHistory: []string{"1"}}
	page := Page{Posts: []Post{post}, Users: []User{{ID: "1001", Username: "example_author", Name: "Example Author", ProtectedKnown: true}}, FetchedAt: at}
	_testRequire.NoError(WritePage(t.Context(), st, source.ID, "1", page, at))
	target, err := st.ResolveXPostContext(t.Context(), source.ID, "1")
	_testRequire.NoError(err)
	_testRequire.Positive(target.MessageID)
	_testAssert.Equal("1", target.CurrentID)
	body, err := st.GetMessageBodyText(t.Context(), target.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("version 0 ", body)
}

// Shrunk counterexample from TestOldVersionParentResolution: one missing root,
// one child, a one-record pass before and after the root arrives.
func TestXPropertyMinimalDelayedParent(t *testing.T) {
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	write := func(post Post) {
		t.Helper()
		require.NoError(t, WritePage(t.Context(), st, source.ID, "1", Page{Posts: []Post{post}, Users: []User{{ID: "1", Username: "example_owner", ProtectedKnown: true}}, FetchedAt: at}, at))
	}
	write(Post{ID: "10000", AuthorID: "1", ConversationID: "1", Text: "child", CreatedAt: at, EditHistory: []string{"10000"}, References: []Reference{{Type: "replied_to", ID: "1"}}})
	write(Post{ID: "1", AuthorID: "1", ConversationID: "1", Text: "root", CreatedAt: at, EditHistory: []string{"1"}})
	root, err := st.ResolveXPostContext(t.Context(), source.ID, "1")
	_testRequire.NoError(err)
	_testRequire.Positive(root.MessageID)
	child, err := st.ResolveXPostContext(t.Context(), source.ID, "10000")
	_testRequire.NoError(err)
	var parent int64
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT reply_to_message_id FROM messages WHERE id=?"), child.MessageID).Scan(&parent))
	assert.Equal(t, root.MessageID, parent)
}

func TestWritePageSummaryCountsOnlyDurableEffects(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	post := func(id, author, text string, history ...string) Post {
		if len(history) == 0 {
			history = []string{id}
		}
		return Post{ID: id, AuthorID: author, ConversationID: history[0], Text: text, CreatedAt: at, EditHistory: history}
	}
	users := []User{{ID: "1001", Username: "example_owner", Name: "Example Owner", ProtectedKnown: true}, {ID: "1002", Username: "private_author", Protected: true, ProtectedKnown: true}}
	old := post("100", "1001", "original")
	_testRequire.NoError(WritePage(t.Context(), st, source.ID, "1001", Page{Posts: []Post{old}, Users: users, FetchedAt: at}, at))
	_, err = st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "400", AuthorID: "1001", ConversationID: "400", EditHistory: []string{"400"}, CreatedAt: at, FetchedAt: at, Availability: "removed"})
	_testRequire.NoError(err)
	current := post("102", "1001", "edited", "100", "102")
	retweet := post("200", "1001", "filtered retweet")
	retweet.References = []Reference{{Type: "retweeted", ID: "900"}}
	private := post("300", "1002", "filtered private")
	suppressed := post("401", "1001", "filtered removed", "400", "401")
	added := post("500", "1001", "added")
	page := Page{Posts: []Post{current, old, retweet, private, suppressed, added, added}, Users: users, FetchedAt: at.Add(time.Hour), PostCount: 7, UserCount: 2, ReservationID: "admitted-resources"}
	summary, err := WritePageWithSummary(t.Context(), st, source.ID, "1001", page, page.FetchedAt)
	_testRequire.NoError(err)
	_testAssert.Equal(WriteSummary{Added: 1, Updated: 1}, summary)
	_testAssert.Equal(7, page.PostCount)
	_testAssert.Equal(2, page.UserCount)
	_testAssert.Equal("admitted-resources", page.ReservationID)
}
