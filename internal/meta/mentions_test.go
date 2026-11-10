package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// The tags fixture serves two pages. Neither carries paging.next, as the tags
// edge may omit it, so the importer must follow the cursor to an empty page.
func TestImportTagsResumesAndArchivesOtherAccountsMedia(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var afters []string
	unavailable := false
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/200/tags":
			if unavailable {
				http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
				return
			}
			after := r.URL.Query().Get("after")
			afters = append(afters, after)
			switch after {
			case "":
				_, _ = fmt.Fprint(w, `{"data":[{"id":"700","caption":"tagged orchardmarket","timestamp":"2000-01-02T03:04:05+0000","username":"example_visitor"},
					{"id":"701","caption":"own post","owner":{"id":"200"}}],"paging":{"cursors":{"after":"p2"}}}`)
			case "p2":
				_, _ = fmt.Fprint(w, `{"data":[{"id":"702","timestamp":"2000-01-03T03:04:05+0000","username":"example_friend"}],"paging":{"cursors":{"after":"p3"}}}`)
			case "p3":
				_, _ = fmt.Fprint(w, `{"data":[]}`)
			default:
				assert.Fail("unexpected tags cursor", after)
			}
		default:
			assert.Fail("unexpected fixture request", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	imp := NewMentionImporter(st, c)
	src, err := imp.Register(t.Context(), "9", "100", "200")
	require.NoError(err)
	assert.Equal(MentionsSourceType, src.SourceType)

	sum, err := imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 1})
	require.NoError(err)
	assert.True(sum.Partial)
	assert.Equal(1, sum.MessagesAdded, "the account's own media is skipped")

	sum, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(2, sum.Requests)
	assert.Equal(1, sum.MessagesAdded)
	assert.Equal([]string{"", "p2", "p3"}, afters, "the second run resumes at the saved cursor")

	ids, err := st.MessageExistsBatch(src.ID, []string{"700", "701", "702"})
	require.NoError(err)
	assert.NotContains(ids, "701")
	var messageType, subject, metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_type, subject, metadata FROM messages WHERE id=?`, ids["700"]).Scan(&messageType, &subject, &metadata))
	assert.Equal(MentionsSourceType, messageType)
	assert.Equal("@example_visitor", subject)
	assert.JSONEq(`{"app_id":"9","page_id":"100","instagram_account_id":"200","media_id":"700","kind":"tag"}`, metadata)
	var hits int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'orchardmarket'`).Scan(&hits))
	assert.Equal(1, hits)

	afters = nil
	sum, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.Equal([]string{"", "p2", "p3"}, afters, "a completed scan starts again from the newest tags")
	assert.Equal(2, sum.MessagesProcessed)
	assert.Zero(sum.MessagesAdded + sum.MessagesUpdated)

	// An interrupted --full run replaces the saved position, so the next
	// ordinary sync starts from the top instead of an older cursor.
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 1})
	require.NoError(err)
	unavailable = true
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{Full: true})
	require.Error(err)
	unavailable = false
	afters = nil
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.Equal([]string{"", "p2", "p3"}, afters)
}
