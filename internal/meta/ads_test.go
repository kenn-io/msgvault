package meta

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type adFixture struct {
	mediaStatus int
	mediaBody   string
}

// newAdFixture serves one organic media item (300) and an ad account whose
// creatives use ad-only owned media 900, the organic media 300, another
// account's media 901, and no Instagram media at all.
func newAdFixture(t *testing.T) (*adFixture, *Client, *Client) {
	t.Helper()
	f := &adFixture{}
	page := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		token := "synthetic-page-secret"
		if r.URL.Path == "/v26.0/act_700/adcreatives" {
			token = "synthetic-ad-secret"
		}
		assert.Equal(t, "Bearer "+token, r.Header.Get("Authorization"), r.URL.Path)
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200"}}`)
		case "/v26.0/200/media":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"300","caption":"Organic media","owner":{"id":"200"}}]}`)
		case "/v26.0/300/comments":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"400","text":"Organic comment"}]}`)
		case "/v26.0/act_700/adcreatives":
			if r.URL.Query().Get("after") == "next" {
				_, _ = fmt.Fprint(w, `{"data":[{"id":"603","effective_instagram_media_id":"901"},{"id":"604"}]}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"data":[{"id":"600","effective_instagram_media_id":"900"},{"id":"602","effective_instagram_media_id":"300"}],"paging":{"cursors":{"after":"next"},"next":"https://example.test/unused"}}`)
		case "/v26.0/900":
			if f.mediaStatus != 0 {
				w.WriteHeader(f.mediaStatus)
				_, _ = fmt.Fprint(w, f.mediaBody)
				return
			}
			_, _ = fmt.Fprint(w, `{"id":"900","caption":"Ad-only media","timestamp":"2000-01-02T03:04:05+0000","owner":{"id":"200"}}`)
		case "/v26.0/300":
			_, _ = fmt.Fprint(w, `{"id":"300","caption":"Organic media","owner":{"id":"200"}}`)
		case "/v26.0/901":
			_, _ = fmt.Fprint(w, `{"id":"901","caption":"Another account's media","owner":{"id":"201"}}`)
		case "/v26.0/900/comments":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"1000","text":"Ad comment"}]}`)
		case "/v26.0/400/replies", "/v26.0/1000/replies":
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		default:
			assert.Fail(t, "unexpected ad fixture request", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	ads, err := newClient("synthetic-ad-secret", page.origin, page.http)
	require.NoError(t, err)
	return f, page, ads
}

func TestInstagramAdMediaArchivesOnlyOwnedMedia(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	_, page, ads := newAdFixture(t)
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	sum, err := imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)
	runs := 0
	for ; runs < 20; runs++ {
		sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{MaxRequests: 4})
		require.NoError(err)
		if !sum.Partial {
			break
		}
	}
	require.Less(runs, 20)
	assert.Positive(runs)

	stored, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"300", "400", "900", "901", "1000"})
	require.NoError(err)
	assert.ElementsMatch([]string{"300", "400", "900", "1000"}, slices.Collect(maps.Keys(stored)))
	adMedia, adComment := message(t, st, src.ID, "900"), message(t, st, src.ID, "1000")
	assert.Equal(adMedia.ID, adComment.ReplyToMessageID.Int64)
	assert.Equal(adMedia.ConversationID, adComment.ConversationID)

	flags := func(id string) map[string]jsontext.Value {
		saved, err := st.GetMessageMetadata(message(t, st, src.ID, id).ID)
		require.NoError(err)
		var metadata map[string]jsontext.Value
		require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
		return metadata
	}
	assert.JSONEq("true", string(flags("1000")["ad"]))
	assert.NotEqual("true", string(flags("1000")["organic"]))
	assert.JSONEq("true", string(flags("300")["organic"]))
	assert.JSONEq("true", string(flags("300")["ad"]))
}

func TestInstagramAdMediaSkipOrFail(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		skip   bool
	}{
		{"no owner", http.StatusOK, `{"id":"900","caption":"Ad-only media"}`, true},
		{"missing object", http.StatusBadRequest, `{"error":{"code":100,"error_subcode":33}}`, true},
		{"rate limit", http.StatusBadRequest, `{"error":{"code":4}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, page, ads := newAdFixture(t)
			f.mediaStatus, f.mediaBody = tt.status, tt.body
			imp := NewImporter(st, page)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
			if tt.skip {
				require.NoError(err)
				assert.False(sum.Partial)
				assert.Equal(2, sum.AdMediaSkipped)
			} else {
				require.Error(err)
				assert.True(sum.Partial)
				assert.Zero(sum.AdMediaSkipped)
			}
			exists, err := st.MessageExistsBatch(src.ID, []string{"900"})
			require.NoError(err)
			assert.Empty(exists)

			f.mediaStatus = 0
			_, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
			require.NoError(err)
			exists, err = st.MessageExistsBatch(src.ID, []string{"900", "1000"})
			require.NoError(err)
			assert.Len(exists, 2)
		})
	}
}
