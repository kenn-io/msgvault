package meta

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type adReadFixture struct {
	calls                                    atomic.Int32
	owner, expectedAfter                     string
	duplicate, organicAd                     bool
	unresolved                               string
	nodeStatus, adStatus, continuationStatus int
	wrongID                                  bool
	stream                                   bool
	late                                     bool
	denied                                   atomic.Int32
}

func newAdReadFixture(t *testing.T) (*adReadFixture, *Client, *Client) {
	t.Helper()
	assert := assert.New(t)
	f := &adReadFixture{owner: "200"}
	page := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		token := "synthetic-page-secret"
		if strings.Contains(r.URL.Path, "/act_700/") {
			token = "synthetic-ad-secret"
		}
		assert.Equal("Bearer "+token, r.Header.Get("Authorization"))
		assert.Empty(r.URL.Query().Get("access_token"))
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200"}}`)
		case "/v26.0/200/media":
			extra := ""
			if f.organicAd {
				extra = `,{"id":"900","caption":"Ad-only media","owner":{"id":"200"}}`
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"300","caption":"Organic media","owner":{"id":"200"}}%s]}`, extra)
		case "/v26.0/300/comments":
			extra := ""
			if f.late {
				extra = `,{"id":"401","text":"Late organic comment"}`
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"400","text":"Organic comment","from":{"id":"300"}}%s]}`, extra)
		case "/v26.0/act_700/adcreatives":
			if f.continuationStatus != 0 && r.URL.Query().Get("after") != "" {
				http.Error(w, "synthetic-ad-secret private-body", f.continuationStatus)
				return
			}
			if f.adStatus != 0 {
				f.denied.Add(1)
				http.Error(w, "synthetic-ad-secret private-body", f.adStatus)
				return
			}
			assert.Equal("id,account_id,effective_instagram_media_id", r.URL.Query().Get("fields"))
			assert.Empty(r.URL.Query().Get("order"))
			after := r.URL.Query().Get("after")
			if f.expectedAfter != "" {
				assert.Equal(f.expectedAfter, after)
			}
			if f.unresolved != "" && after == "" {
				target := ""
				if f.unresolved == "foreign owner" {
					target = "901"
				}
				if f.unresolved == "missing owner" {
					target = "902"
				}
				account := "700"
				if f.unresolved == "foreign ad account" {
					target, account = "900", "701"
				}
				if f.unresolved == "missing ad account" {
					target, account = "900", ""
				}
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"599","account_id":%q,"effective_instagram_media_id":%q}],"paging":{"cursors":{"after":"ad-next"},"next":"https://example.test/not-followed"}}`, account, target)
				return
			}
			id := "600"
			if after == "ad-next" && f.unresolved == "" {
				id = "601"
			}
			paging := ""
			if f.duplicate && after != "ad-next" {
				paging = `,"paging":{"cursors":{"after":"ad-next"},"next":"https://example.test/not-followed"}`
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"account_id":"700","effective_instagram_media_id":"900"}]%s}`, id, paging)
		case "/v26.0/900":
			if f.stream {
				flusher, ok := w.(http.Flusher)
				if !assert.True(ok) {
					http.Error(w, "cannot flush fixture", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Length", "1024")
				_, _ = fmt.Fprint(w, `{"id":"900","caption":"synthetic-page-secret private-body`)
				flusher.Flush()
				<-r.Context().Done()
				return
			}
			if f.nodeStatus != 0 {
				http.Error(w, "synthetic-page-secret private-body", f.nodeStatus)
				return
			}
			assert.Equal("id,caption,timestamp,permalink,owner{id}", r.URL.Query().Get("fields"))
			id := "900"
			if f.wrongID {
				id = "901"
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"caption":"Ad-only media","timestamp":"2000-01-02T03:04:05+0000","owner":{"id":%q}}`, id, f.owner)
		case "/v26.0/901":
			_, _ = fmt.Fprint(w, `{"id":"901","caption":"Foreign media","owner":{"id":"201"}}`)
		case "/v26.0/902":
			_, _ = fmt.Fprint(w, `{"id":"902","caption":"Unknown owner"}`)
		case "/v26.0/900/comments":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"1000","text":"Ad orchard","from":{"id":"300"}}]}`)
		case "/v26.0/400/replies", "/v26.0/401/replies", "/v26.0/1000/replies":
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		default:
			assert.Fail("unexpected ad fixture request", r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	ad, err := newClient("synthetic-ad-secret", page.origin, page.http)
	require.NoError(t, err)
	return f, page, ad
}

func TestAdCreativeWireBoundaries(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f, page, ad := newAdReadFixture(t)
	f.expectedAfter = "opaque+/="
	creatives, err := ad.AdCreatives(t.Context(), "700", f.expectedAfter)
	require.NoError(err)
	require.Len(creatives.Data, 1)
	assert.Equal("900", creatives.Data[0].EffectiveInstagramMediaID)
	assert.Equal("700", creatives.Data[0].AccountID)
	media, err := page.InstagramMediaByID(t.Context(), creatives.Data[0].EffectiveInstagramMediaID)
	require.NoError(err)
	require.NotNil(media.Owner)
	assert.Equal("200", media.Owner.ID)
	assert.Equal(int32(2), f.calls.Load())
	invalid := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Fail("invalid identity reached HTTP")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	})
	_, err = invalid.AdCreatives(t.Context(), "700/path", "")
	require.Error(err)
	_, err = invalid.InstagramMediaByID(t.Context(), "../900")
	require.Error(err)
}

func TestInstagramAdsCanonicalBudgetAndProvenance(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, page, ad := newAdReadFixture(t)
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	completed, paused := false, false
	for range 12 {
		before := f.calls.Load()
		sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{MaxRequests: 4})
		require.NoError(err)
		assert.Equal(int(f.calls.Load()-before), sum.Requests)
		assert.LessOrEqual(sum.Requests, 4)
		if !sum.Partial {
			completed = true
			break
		}
		paused = true
	}
	require.True(completed)
	assert.True(paused)
	root, child := message(t, st, src.ID, "900"), message(t, st, src.ID, "1000")
	assert.Equal(root.ID, child.ReplyToMessageID.Int64)
	assert.Equal(root.ConversationID, child.ConversationID)
	assert.Equal(message(t, st, src.ID, "400").SenderID, child.SenderID)
	assert.Equal(message(t, st, src.ID, "300").SenderID, root.SenderID)
	ids, err := st.MessageExistsBatch(src.ID, []string{"600"})
	require.NoError(err)
	assert.Empty(ids, "creative IDs are never comment roots")
	f.duplicate, f.organicAd = true, true
	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
	require.NoError(err)
	assert.Zero(sum.MessagesAdded)
	for _, row := range []int64{root.ID, child.ID} {
		saved, err := st.GetMessageMetadata(row)
		require.NoError(err)
		var metadata map[string]any
		require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
		assert.EqualValues(2, metadata["version"], "older metadata-v1 importers must refuse enriched records rather than erase creative provenance")
		assert.Equal(true, metadata["ad"])
		assert.Equal(true, metadata["organic"])
		assert.Equal("700", metadata["ad_account_id"])
		assert.Equal("600", metadata["ad_creative_id"], "keep the first verified association on replay")
		assert.Equal("900", metadata["media_id"])
	}
}

func TestInstagramAdsRefusalsRetainCoverageAndOrganicProgress(t *testing.T) {
	for _, problem := range []string{"missing target", "missing owner", "foreign owner", "foreign ad account", "missing ad account"} {
		t.Run(problem, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, page, ad := newAdReadFixture(t)
			f.unresolved = problem
			imp := NewImporter(st, page)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			found, refused := false, false
			for range 24 {
				before := f.calls.Load()
				sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{MaxRequests: 3})
				require.NotNil(sum)
				assert.Equal(int(f.calls.Load()-before), sum.Requests)
				assert.LessOrEqual(sum.Requests, 3)
				assert.True(sum.Partial, "a refusal from an earlier paused run cannot become false complete coverage")
				if err != nil && strings.Contains(err.Error(), `"599"`) {
					refused = true
					if problem == "foreign owner" {
						assert.Contains(err.Error(), `"901"`)
					}
				}
				ids, err := st.MessageExistsBatch(src.ID, []string{"1000"})
				require.NoError(err)
				if ids["1000"] != 0 {
					found = true
					break
				}
			}
			require.True(refused)
			require.True(found, "a refused candidate on an earlier page must not starve accessible ad comments")
			rootMeta, err := st.GetMessageMetadata(message(t, st, src.ID, "900").ID)
			require.NoError(err)
			assert.Contains(rootMeta.String, `"ad_creative_id":"600"`, "an unverified earlier creative cannot claim the owned media association")
			foreign, err := st.MessageExistsBatch(src.ID, []string{"599", "901", "902"})
			require.NoError(err)
			assert.Empty(foreign)
			observations, err := st.FindObservationsByProviderUserIDContext(t.Context(), instagramActorIdentifier("9", "100", "200", "ig_user", "201"), 10)
			require.NoError(err)
			assert.Empty(observations, "ownership refusal precedes actor writes")
			f.unresolved, f.adStatus = "", 403
			late, denied := false, false
			for range 32 {
				before := f.calls.Load()
				sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{MaxRequests: 3})
				require.NotNil(sum)
				assert.Equal(int(f.calls.Load()-before), sum.Requests)
				assert.LessOrEqual(sum.Requests, 3)
				assert.True(sum.Partial)
				if err != nil {
					if apiErr, ok := errors.AsType[*APIError](err); ok {
						assert.Equal(403, apiErr.Status)
					} else {
						assert.Contains(err.Error(), "unverified")
					}
				}
				if !denied && f.denied.Load() > 0 {
					denied, f.late = true, true
					continue
				}
				if denied {
					ids, err := st.MessageExistsBatch(src.ID, []string{"401"})
					require.NoError(err)
					if ids["401"] != 0 {
						late = true
						break
					}
				}
			}
			require.True(denied)
			require.True(late, "continued ad denial must not hide late comments on old organic media")
		})
	}
}

func TestInstagramAdsNodeFailureKeepsCandidateAndError(t *testing.T) {
	for _, status := range []int{0, 400, 403, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, page, ad := newAdReadFixture(t)
			f.nodeStatus, f.wrongID = status, status == 0
			imp := NewImporter(st, page)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			_, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
			require.Error(err)
			if status != 0 {
				var apiErr *APIError
				require.ErrorAs(err, &apiErr)
				assert.Equal(status, apiErr.Status)
			} else {
				require.ErrorContains(err, "identity mismatch")
			}
			for _, want := range []string{`"600"`, `"900"`} {
				assert.Contains(err.Error(), want)
			}
			for _, unwanted := range []string{"--full", "expired", "synthetic-page-secret", "private-body"} {
				assert.NotContains(err.Error(), unwanted)
			}
			run, err := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			var state syncState
			require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
			require.Len(state.Phases[adCreativesEdge].Posts, 1)
			assert.Equal("600", state.Phases[adCreativesEdge].Posts[0].ID)
			assert.True(state.Phases[instagramMediaEdge].Done)
			assert.Equal(store.SyncStatusFailed, run.Status)
			assert.Positive(run.ErrorsCount)
			wrong, err := st.MessageExistsBatch(src.ID, []string{"901"})
			require.NoError(err)
			assert.Empty(wrong)
			f.nodeStatus, f.wrongID = 0, false
			sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(2, sum.MessagesAdded)
			message(t, st, src.ID, "1000")
		})
	}
}

func TestInstagramAdsContinuationErrorKeepsCursor(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, page, ad := newAdReadFixture(t)
	f.duplicate, f.continuationStatus = true, http.StatusBadRequest
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
	require.Error(err)
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusBadRequest, apiErr.Status)
	assert.True(sum.Partial)
	assert.Contains(err.Error(), "instagram adcreatives")
	assert.Contains(err.Error(), "continuation request failed")
	for _, unwanted := range []string{"--full", "expired", "synthetic-ad-secret", "private-body"} {
		assert.NotContains(err.Error(), unwanted)
	}
	run, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	var state syncState
	require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
	assert.Equal("ad-next", state.Phases[adCreativesEdge].After)
	assert.False(state.Phases[adCreativesEdge].Done)
	assert.Equal(store.SyncStatusFailed, run.Status)
	child := message(t, st, src.ID, "1000")
	f.continuationStatus, f.expectedAfter = 0, "ad-next"
	sum, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Zero(sum.MessagesAdded)
	assert.Equal(child.ID, message(t, st, src.ID, "1000").ID)
}

func TestInstagramAdsVersionRecoveryAndFence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, page, ad := newAdReadFixture(t)
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	root := message(t, st, src.ID, "300")
	before := f.calls.Load()
	_, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
	require.ErrorContains(err, "--full")
	assert.Equal(before, f.calls.Load())
	execution, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(execution.Release()) })
	_, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{Full: true})
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	assert.Equal(before, f.calls.Load())
	require.NoError(execution.Release())
	_, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{Full: true})
	require.NoError(err)
	assert.Equal(root.ID, message(t, st, src.ID, "300").ID)
	before = f.calls.Load()
	_, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "701", ad, ImportOptions{})
	require.ErrorContains(err, "--full")
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.ErrorContains(err, "--full")
	assert.Equal(before, f.calls.Load())
	f.organicAd = true
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{Full: true})
	require.NoError(err)
	assert.Equal(root.ID, message(t, st, src.ID, "300").ID)
	adRoot := message(t, st, src.ID, "900")
	saved, err := st.GetMessageMetadata(adRoot.ID)
	require.NoError(err)
	assert.Contains(saved.String, `"ad_creative_id":"600"`)
	assert.Contains(saved.String, `"version":2`)
}
