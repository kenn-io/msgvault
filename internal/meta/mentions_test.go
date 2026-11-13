package meta

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// tagsFixture serves two tags pages and calls onTags with each requested
// cursor. Neither page carries paging.next, as the tags edge may omit it, so
// the importer must follow the cursor to an empty page.
func tagsFixture(t *testing.T, onTags func(after string)) *Client {
	t.Helper()
	return fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/200/tags":
			after := r.URL.Query().Get("after")
			onTags(after)
			switch after {
			case "":
				_, _ = fmt.Fprint(w, `{"data":[{"id":"700","caption":"tagged orchardmarket","timestamp":"2000-01-02T03:04:05+0000","username":"example_visitor"},
					{"id":"701","caption":"own post","owner":{"id":"200"}}],"paging":{"cursors":{"after":"p2"}}}`)
			case "p2":
				_, _ = fmt.Fprint(w, `{"data":[{"id":"702","username":"example_friend"}],"paging":{"cursors":{"after":"p3"}}}`)
			case "p3":
				_, _ = fmt.Fprint(w, `{"data":[]}`)
			default:
				assert.Fail(t, "unexpected tags cursor", after)
			}
		default:
			assert.Fail(t, "unexpected fixture request", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
}

func TestImportTagsResumesAndArchivesOtherAccountsMedia(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var afters []string
	imp := NewMentionImporter(st, tagsFixture(t, func(after string) { afters = append(afters, after) }))
	src, err := imp.Register(t.Context(), "9", "100", "200")
	require.NoError(err)

	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 1})
	require.NoError(err)
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.Equal([]string{"", "p2", "p3"}, afters, "the second run resumes at the saved cursor")

	ids, err := st.MessageExistsBatch(src.ID, []string{"700", "701"})
	require.NoError(err)
	assert.NotContains(ids, "701", "the account's own media is skipped")
	var subject, metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT subject, metadata FROM messages WHERE id=?`, ids["700"]).Scan(&subject, &metadata))
	assert.Equal("@example_visitor", subject)
	assert.JSONEq(`{"app_id":"9","page_id":"100","instagram_account_id":"200","media_id":"700","kinds":["tag"]}`, metadata)
	var hits int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'orchardmarket'`).Scan(&hits))
	assert.Equal(1, hits)

	afters = nil
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.Equal([]string{"", "p2", "p3"}, afters, "a completed scan starts again from the newest tags")
}

// A --full run publishes its reset before its first request, so a crash
// before the first page cannot leave the next sync on an older cursor.
func TestImportTagsFullResetIsSavedBeforeFirstRequest(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var sourceID int64
	var resumeFrom []string
	imp := NewMentionImporter(st, tagsFixture(t, func(string) {
		// A lookup error leaves "", which fails the JSON check below.
		blob, _ := savedCheckpoint(t.Context(), st, sourceID)
		resumeFrom = append(resumeFrom, blob)
	}))
	src, err := imp.Register(t.Context(), "9", "100", "200")
	require.NoError(err)
	sourceID = src.ID
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 1})
	require.NoError(err)

	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{Full: true, MaxRequests: 1})
	require.NoError(err)
	require.Len(resumeFrom, 2)
	assert.JSONEq(t, `{"after":""}`, resumeFrom[1], "the next sync would start from the top")
}

// cancelOnLastPage cancels the run as it returns the final tags page.
type cancelOnLastPage struct {
	MentionAPI

	cancel context.CancelFunc
}

func (c cancelOnLastPage) Tags(ctx context.Context, accountID, after string) (IGMediaPage, error) {
	page, err := c.MentionAPI.Tags(ctx, accountID, after)
	if err == nil && page.After == "" {
		c.cancel()
	}
	return page, err
}

// A run that reached the last page but could not finish still sends the
// next sync to the newest tags instead of an older saved cursor.
func TestImportTagsUnfinishedFullScanStartsNextSyncFromTop(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var afters []string
	c := tagsFixture(t, func(after string) { afters = append(afters, after) })
	imp := NewMentionImporter(st, c)
	_, err := imp.Register(t.Context(), "9", "100", "200")
	require.NoError(err)
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 1})
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = NewMentionImporter(st, cancelOnLastPage{MentionAPI: c, cancel: cancel}).
		ImportTags(ctx, "9", "100", "200", ImportOptions{Full: true})
	require.Error(err)

	afters = nil
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.Equal(t, []string{"", "p2", "p3"}, afters)
}
