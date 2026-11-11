package meta

import (
	"context"
	"errors"
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

// tagsFixture serves the two-page tags fixture and calls onTags with each
// requested cursor before it answers.
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
				_, _ = fmt.Fprint(w, `{"data":[{"id":"700","username":"example_visitor"}],"paging":{"cursors":{"after":"p2"}}}`)
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

// A --full run publishes its reset before its first request, so a crash
// before the first page cannot leave the next sync on an older cursor.
func TestImportTagsFullResetIsSavedBeforeFirstRequest(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	full := false
	var sourceID int64
	var resumeFrom []string
	var lookupErr error
	c := tagsFixture(t, func(after string) {
		if !full {
			return
		}
		// The handler runs off the test goroutine, so the error is checked
		// after the run.
		blob, err := savedCheckpoint(t.Context(), st, sourceID)
		lookupErr = errors.Join(lookupErr, err)
		resumeFrom = append(resumeFrom, blob)
	})
	imp := NewMentionImporter(st, c)
	src, err := imp.Register(t.Context(), "9", "100", "200")
	require.NoError(err)
	sourceID = src.ID
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 1})
	require.NoError(err)

	full = true
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{Full: true, MaxRequests: 1})
	require.NoError(err)
	require.NoError(lookupErr)
	require.Len(resumeFrom, 1)
	assert.JSONEq(`{"after":""}`, resumeFrom[0], "the next sync would start from the top")
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
	assert, require := assert.New(t), require.New(t)
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
	afters = nil
	_, err = NewMentionImporter(st, cancelOnLastPage{MentionAPI: c, cancel: cancel}).
		ImportTags(ctx, "9", "100", "200", ImportOptions{Full: true})
	require.Error(err)
	require.Equal([]string{"", "p2", "p3"}, afters, "the run reached the last page")

	afters = nil
	_, err = imp.ImportTags(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.Equal([]string{"", "p2", "p3"}, afters)
}
