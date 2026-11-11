package meta

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type adFixture struct {
	mediaStatus                        int
	mediaBody                          string
	mediaLookupsNotFound               bool // every media-by-ID read returns a bare 404
	creativeStatus                     int
	creativeBody, creativeFailAfter    string
	creativeReads, adReads, ownedReads atomic.Int32
}

// newAdFixture serves one organic media item (300) and an ad account whose
// creatives use: ad-only owned media 900 (twice), the organic media 300,
// foreign media 901, unreadable media 902, media with no owner (903) or an
// empty owner (904), and no Instagram media at all.
func newAdFixture(t *testing.T) (*adFixture, *Client, *Client) {
	t.Helper()
	f := &adFixture{}
	page := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		token := "synthetic-page-secret"
		if r.URL.Path == "/v26.0/act_700/adcreatives" {
			token = "synthetic-ad-secret"
		}
		assert.Equal(t, "Bearer "+token, r.Header.Get("Authorization"), r.URL.Path)
		if f.mediaLookupsNotFound && slices.Contains([]string{"/v26.0/300", "/v26.0/900", "/v26.0/901", "/v26.0/902", "/v26.0/903", "/v26.0/904"}, r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
			f.creativeReads.Add(1)
			if f.creativeStatus != 0 && r.URL.Query().Get("after") == f.creativeFailAfter {
				w.WriteHeader(f.creativeStatus)
				_, _ = fmt.Fprint(w, f.creativeBody)
				return
			}
			if r.URL.Query().Get("after") == "" {
				_, _ = fmt.Fprint(w, `{"data":[{"id":"600","effective_instagram_media_id":"900"},{"id":"601","effective_instagram_media_id":"900"},{"id":"602","effective_instagram_media_id":"300"}],"paging":{"cursors":{"after":"next"},"next":"https://example.test/unused"}}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"data":[{"id":"603","effective_instagram_media_id":"901"},{"id":"604","effective_instagram_media_id":"902"},{"id":"605"},{"id":"606","effective_instagram_media_id":"903"},{"id":"607","effective_instagram_media_id":"904"}]}`)
		case "/v26.0/900":
			f.adReads.Add(1)
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
		case "/v26.0/902":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"synthetic denial","code":10}}`)
		case "/v26.0/903":
			_, _ = fmt.Fprint(w, `{"id":"903","caption":"Media without an owner"}`)
		case "/v26.0/904":
			_, _ = fmt.Fprint(w, `{"id":"904","caption":"Media with an empty owner","owner":{"id":""}}`)
		case "/v26.0/900/comments":
			f.ownedReads.Add(1)
			_, _ = fmt.Fprint(w, `{"data":[{"id":"1000","text":"adorchardcomment"}]}`)
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
	f, page, ads := newAdFixture(t)
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	runs := 0
	for ; runs < 20; runs++ {
		sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{MaxRequests: 4})
		require.NoError(err)
		if !sum.Partial {
			break
		}
	}
	require.Less(runs, 20, "bounded runs must resume to completion")
	assert.Positive(runs, "the budget forces at least one resume from a saved ad checkpoint")
	assert.Equal(int32(1), f.adReads.Load(), "a media ID repeated on one creative page is read once")
	assert.Equal(int32(1), f.ownedReads.Load())

	stored, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"300", "400", "900", "901", "902", "903", "904", "1000"})
	require.NoError(err)
	assert.ElementsMatch([]string{"300", "400", "900", "1000"}, slices.Collect(maps.Keys(stored)), "foreign, ownerless and unreadable ad media are skipped")
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
	assert.NotEqual("true", string(flags("1000")["organic"]), "ad-only media comments are not organic")
	shared := flags("300")
	assert.JSONEq("true", string(shared["organic"]))
	assert.JSONEq("true", string(shared["ad"]), "media found both ways keeps both flags on one row")
	sharedComment := flags("400")
	assert.JSONEq("true", string(sharedComment["organic"]))
	assert.JSONEq("true", string(sharedComment["ad"]), "a comment on media found both ways keeps both flags")
}

func TestInstagramAdMediaSkippedCount(t *testing.T) {
	for _, tt := range []struct {
		name        string
		notFound    bool
		wantSkipped int
		archived    []string
	}{
		// 901 another account, 902 unreadable, 903 and 904 no owner.
		{"default fixture", false, 4, []string{"300", "400", "900", "1000"}},
		// Every media lookup, including 300 and 900, returns a bare 404.
		{"every lookup not found", true, 6, []string{"300", "400"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, page, ads := newAdFixture(t)
			f.mediaLookupsNotFound = tt.notFound
			imp := NewImporter(st, page)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(tt.wantSkipped, sum.AdMediaSkipped)
			stored, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"300", "400", "900", "901", "902", "903", "904", "1000"})
			require.NoError(err)
			assert.ElementsMatch(tt.archived, slices.Collect(maps.Keys(stored)))
		})
	}
}

func TestInstagramAdMediaMissingIsSkipped(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"bare 403", http.StatusForbidden, ""},
		{"bare 404", http.StatusNotFound, ""},
		{"code 100 subcode 33", http.StatusBadRequest, `{"error":{"code":100,"error_subcode":33}}`},
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
			require.NoError(err, "unreadable ad media is skipped, not fatal")
			assert.False(sum.Partial)
			assert.Equal(5, sum.AdMediaSkipped, "900 plus the default fixture's four skips")
			exists, err := st.MessageExistsBatch(src.ID, []string{"900", "1000"})
			require.NoError(err)
			assert.Empty(exists)
			saved, err := st.GetMessageMetadata(message(t, st, src.ID, "300").ID)
			require.NoError(err)
			var metadata map[string]jsontext.Value
			require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
			assert.JSONEq("true", string(metadata["ad"]), "the run went on to 300, after 900 on the same creative page")
		})
	}
}

func TestInstagramAdMediaRetryableFailureFailsSync(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		body     string
		wantCode string
	}{
		{"server error", http.StatusServiceUnavailable, "", "HTTP 503"},
		{"rate limit as HTTP 400", http.StatusBadRequest, `{"error":{"code":4}}`, "code 4"},
		{"expired token", http.StatusBadRequest, `{"error":{"code":190}}`, "code 190"},
		{"invalid parameter without subcode", http.StatusBadRequest, `{"error":{"code":100}}`, "code 100"},
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
			require.Error(err, "a retryable media failure is not skipped")
			assert.Contains(err.Error(), tt.wantCode)
			assert.True(sum.Partial)
			exists, err := st.MessageExistsBatch(src.ID, []string{"900"})
			require.NoError(err)
			assert.Empty(exists)

			f.mediaStatus = 0
			sum, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
			require.NoError(err)
			assert.Equal(4, sum.AdMediaSkipped, "only the default fixture's skips; 900 is archived")
			exists, err = st.MessageExistsBatch(src.ID, []string{"900", "1000"})
			require.NoError(err)
			assert.Len(exists, 2, "the next run retries the failed media")
		})
	}
}

func TestInstagramAdCreativeReadFailureFailsSyncAndResumes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		failAfter string
		body      string
		wantCode  string
		archived  []string // archived by the failed run
		missing   []string // not archived by the failed run
		reads     [2]int32 // creative reads after the failed and the next run
	}{
		{
			name:     "ad token refused",
			body:     `{"error":{"message":"token synthetic-ad-secret lacks ads_read","code":200}}`,
			wantCode: "code 200",
			archived: []string{"300", "400"},
			missing:  []string{"900", "1000"},
			reads:    [2]int32{1, 3},
		},
		{
			name:      "rate limit on the second page",
			failAfter: "next",
			body:      `{"error":{"code":17}}`,
			wantCode:  "code 17",
			archived:  []string{"300", "400", "900", "1000"},
			reads:     [2]int32{2, 3},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, page, ads := newAdFixture(t)
			f.creativeStatus, f.creativeBody, f.creativeFailAfter = http.StatusBadRequest, tt.body, tt.failAfter
			imp := NewImporter(st, page)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)

			sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
			require.Error(err, "a failed creative read fails the sync")
			assert.Contains(err.Error(), tt.wantCode)
			assert.NotContains(err.Error(), "synthetic-ad-secret")
			assert.NotContains(err.Error(), "synthetic-page-secret")
			assert.True(sum.Partial)
			assert.Equal(tt.reads[0], f.creativeReads.Load())
			exists, err := st.MessageExistsBatch(src.ID, tt.archived)
			require.NoError(err)
			assert.Len(exists, len(tt.archived), "media read before the failure is archived in the same run")
			if len(tt.missing) > 0 {
				exists, err = st.MessageExistsBatch(src.ID, tt.missing)
				require.NoError(err)
				assert.Empty(exists)
			}

			f.creativeStatus = 0
			sum, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(tt.reads[1], f.creativeReads.Load(), "the next run resumes at the failed creative page")
			exists, err = st.MessageExistsBatch(src.ID, []string{"900", "1000"})
			require.NoError(err)
			assert.Len(exists, 2)
		})
	}
}

func TestInstagramAdAccountChangeRestartsCycle(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, page, ads := newAdFixture(t)
	imp := NewImporter(st, page)
	_, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	// Two verification reads and the media page: the run pauses with the ad
	// phase next in its checkpoint.
	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(sum.Partial)

	sum, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err, "removing the ad account starts a new cycle instead of refusing the checkpoint")
	assert.False(sum.Partial)
	assert.Zero(f.creativeReads.Load())

	sum, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(int32(2), f.creativeReads.Load())
}
