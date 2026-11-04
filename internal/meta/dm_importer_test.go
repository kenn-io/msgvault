package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func dmRegistrationFixture(t *testing.T, page, ig string) (*store.Store, *DMImporter, *[]string) {
	t.Helper()
	assert := assert.New(t)

	st := testutil.NewSQLiteTestStore(t)
	paths := []string{}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":"Example Page","category":"Community"}`, page)
		case "/v26.0/100":
			_, _ = fmt.Fprintf(w, `{"id":"100","instagram_business_account":{"id":%q,"username":"example_business"}}`, ig)
		default:
			assert.Fail("unexpected Graph request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return st, NewDMImporter(st, c), &paths
}
func TestRegisterFacebookMessagesVerifiesPage(t *testing.T) {
	for _, page := range []string{"100", "101"} {
		t.Run(page, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, imp, _ := dmRegistrationFixture(t, page, "1784")
			s := DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}
			src, err := imp.Register(t.Context(), s)
			if page != "100" {
				require.Error(err)
				_, err = st.GetSourceByTypeAndIdentifier(SourceTypeFacebookMessages, s.Identifier())
				require.ErrorIs(err, store.ErrSourceNotFound)
				return
			}
			require.NoError(err)
			require.NotNil(src)
			assert.Equal(SourceTypeFacebookMessages, src.SourceType)
			assert.Equal("app:9:page:100", src.Identifier)
			var key, signal string
			require.NoError(st.DB().QueryRow("SELECT address, source_signal FROM account_identities WHERE source_id = ?", src.ID).Scan(&key, &signal))
			assert.Equal("facebook_messages:1:9:3:100:3:100", key)
			assert.Equal("facebook_page_token", signal)
			assert.Equal("Example Page", src.DisplayName.String)
		})
	}
}
func TestRegisterInstagramMessagesVerifiesLink(t *testing.T) {
	for _, ig := range []string{"1784", "1785"} {
		t.Run(ig, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, imp, _ := dmRegistrationFixture(t, "100", ig)
			s := DMSource{AppID: "9", PageID: "100", IGID: "1784", Platform: PlatformInstagram}
			src, err := imp.Register(t.Context(), s)
			if ig != "1784" {
				require.Error(err)
				_, err = st.GetSourceByTypeAndIdentifier(SourceTypeInstagramMessages, s.Identifier())
				require.ErrorIs(err, store.ErrSourceNotFound)
				return
			}
			require.NoError(err)
			require.NotNil(src)
			assert.Equal("example_business", src.DisplayName.String)
		})
	}
}
func TestRegisterRetiredSourceRefused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, imp, paths := dmRegistrationFixture(t, "100", "1784")
	s := DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}
	a, err := st.GetOrCreateSource(s.Platform.SourceType(), s.Identifier())
	require.NoError(err)
	b, err := st.GetOrCreateSource(s.Platform.SourceType(), "app:99:page:100")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: a.ID, IntoSourceID: b.ID})
	require.NoError(err)
	_, err = imp.Register(t.Context(), s)
	require.ErrorIs(err, store.ErrSourceRetired)
	assert.Empty(*paths)
}

// Synthetic Graph endpoint, real Client/Importer/Store; no provider configuration.
type dmBackfillFixture struct {
	count         int
	nodes         map[string]int
	calls         int
	conversations int
	denied        bool
	expired       bool
	missing       bool
	omitAuthor    bool
	fromOwner     bool
	newer         bool
	nodeError     *APIError
	text          string
	providerEmail string
	firstUpdated  string
	secondUpdated string
}

func setupDMBackfill(t *testing.T, p DMPlatform, count int) (*store.Store, *dmBackfillFixture, *DMImporter, DMSource) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	f := &dmBackfillFixture{count: count, nodes: map[string]int{}, text: "synthetic inbox text"}
	src := DMSource{AppID: "9", PageID: "100", Platform: p}
	if p == PlatformInstagram {
		src.IGID = "1784"
	}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		var body any
		switch r.URL.Path {
		case "/v26.0/me":
			body = map[string]any{"id": "100", "name": "Example Page", "category": "Community"}
		case "/v26.0/100":
			body = map[string]any{"id": "100", "instagram_business_account": map[string]string{"id": "1784", "username": "example_business"}}
		case "/v26.0/100/conversations":
			f.conversations++
			if f.denied {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"error":{"code":200}}`)
				return
			}
			if f.expired && r.URL.Query().Get("after") != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			first := "2026-11-01T00:00:00+0000"
			if f.firstUpdated != "" {
				first = f.firstUpdated
			}
			stamp := "2026-11-01T00:00:00+0000"
			if f.newer {
				stamp = "2026-11-02T00:00:00+0000"
			}
			if f.secondUpdated != "" {
				stamp = f.secondUpdated
			}
			if r.URL.Query().Get("after") == "next" {
				body = map[string]any{"data": []any{map[string]any{"id": "t_2", "updated_time": stamp}}}
			} else {
				customer := map[string]string{"id": "300", "name": "Example Customer"}
				if f.providerEmail != "" {
					customer["email"] = f.providerEmail
				}
				body = map[string]any{"data": []any{map[string]any{"id": "t_1", "updated_time": first, "participants": map[string]any{"data": []any{customer}}}}, "paging": map[string]any{"cursors": map[string]string{"after": "next"}, "next": "https://unsafe.example/not-followed"}}
			}
		case "/v26.0/t_1", "/v26.0/t_2":
			ids := []any{}
			if r.URL.Path == "/v26.0/t_1" {
				for i := range f.count {
					ids = append(ids, map[string]string{"id": "m_" + strconv.Itoa(i)})
				}
			} else if f.newer {
				ids = append(ids, map[string]string{"id": "new_1"})
			}
			body = map[string]any{"messages": map[string]any{"data": ids}}
		default:
			id := strings.TrimPrefix(r.URL.Path, "/v26.0/")
			f.nodes[id]++
			if f.missing && id == "m_0" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":{"code":9000001}}`)
				return
			}
			if f.nodeError != nil && id == "m_0" {
				w.WriteHeader(f.nodeError.Status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"error_subcode":%d}}`, f.nodeError.Code, f.nodeError.Subcode)
				return
			}
			authorID, recipientID := "300", dmOwner(src)
			authorName, recipientName := "Example Customer", ""
			if f.fromOwner {
				authorID, recipientID = dmOwner(src), "300"
				authorName, recipientName = "Example Page", "Example Customer"
			}
			messageBody := map[string]any{
				"id":           id,
				"to":           map[string]any{"data": []any{map[string]string{"id": recipientID, "name": recipientName}}},
				"message":      f.text,
				"created_time": "2026-11-01T00:00:00+0000",
			}
			if !f.omitAuthor {
				author := map[string]string{"id": authorID, "name": authorName}
				if f.providerEmail != "" {
					author["email"] = f.providerEmail
				}
				messageBody["from"] = author
			}
			body = messageBody
		}
		data, err := json.Marshal(body)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	})
	imp := NewDMImporter(st, c)
	_, err := imp.Register(t.Context(), src)
	require.NoError(err)
	f.calls = 0
	return st, f, imp, src
}

func TestDMImportRejectsMissingAuthorAndRetriesWhenGraphProvidesIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 1)
	f.omitAuthor = true
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.ErrorContains(err, "direct message author ID is missing or invalid")
	require.NotNil(sum)
	assert.True(sum.Partial)
	assert.Equal(1, f.nodes["m_0"])

	source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, dmRawFormat, []string{"m_0"})
	require.NoError(err)
	assert.NotContains(rows, "m_0", "a participant must not be substituted for an absent author")

	f.omitAuthor = false
	f.fromOwner = true
	sum, err = imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	require.NotNil(sum)
	assert.False(sum.Partial)
	assert.Equal(1, sum.MessagesAdded)

	rows, err = st.StoredMessagesContext(t.Context(), source.ID, dmRawFormat, []string{"m_0"})
	require.NoError(err)
	row, ok := rows["m_0"]
	require.True(ok)
	require.True(row.SourceIsFromMe.Valid)
	assert.True(row.SourceIsFromMe.Bool)
	wantOwner, err := ResolveDMActor(t.Context(), st, source.ID, src, dmOwner(src), "")
	require.NoError(err)
	assert.True(row.SenderID.Valid)
	assert.Equal(wantOwner, row.SenderID.Int64)
	conversation, err := st.MessageConversation(row.ID)
	require.NoError(err)
	assert.Equal("psid:300", conversation.SourceConversationID)
}

func TestDMImportAuthoritativeGraphAuthorCorrectsStoredAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 1)
	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)

	source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, dmRawFormat, []string{"m_0"})
	require.NoError(err)
	prior := rows["m_0"]
	require.True(prior.SourceIsFromMe.Valid)
	assert.False(prior.SourceIsFromMe.Bool)

	f.newer = true
	f.fromOwner = true
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	require.NotNil(sum)
	assert.Equal(1, sum.MessagesAdded)
	assert.Equal(2, sum.MessagesProcessed)

	rows, err = st.StoredMessagesContext(t.Context(), source.ID, dmRawFormat, []string{"m_0"})
	require.NoError(err)
	current := rows["m_0"]
	require.True(current.SourceIsFromMe.Valid)
	assert.True(current.SourceIsFromMe.Bool)
	wantOwner, err := ResolveDMActor(t.Context(), st, source.ID, src, dmOwner(src), "")
	require.NoError(err)
	assert.True(current.SenderID.Valid)
	assert.Equal(wantOwner, current.SenderID.Int64)
}

type dmClockAdvancingAPI struct {
	DMAPI

	advance func()
}

func (a dmClockAdvancingAPI) Message(ctx context.Context, mid string) (DMMessage, error) {
	message, err := a.DMAPI.Message(ctx, mid)
	a.advance()
	return message, err
}

func TestDMImportCapturesGraphObservationBeforeMessageRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	assertionsTime := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	now := assertionsTime
	st, _, imp, src := setupDMBackfill(t, PlatformMessenger, 1)
	imp.now = func() time.Time { return now }
	imp.api = dmClockAdvancingAPI{DMAPI: imp.api, advance: func() { now = now.Add(time.Minute) }}

	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	value, found, err := st.GetArchiveMarker(t.Context(), dmGraphObservedKey(source.ID, "m_0"))
	require.NoError(err)
	require.True(found)
	observedAt, err := strconv.ParseInt(value, 10, 64)
	require.NoError(err)
	assert.Equal(assertionsTime.UnixMilli(), observedAt)
	assert.Equal(assertionsTime.Add(time.Minute).UnixMilli(), now.UnixMilli())
}

func TestDMImportBackfillCapsAt20(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, imp, src := setupDMBackfill(t, PlatformMessenger, 25)
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	require.NotNil(sum)
	assert.Equal(20, sum.MessagesAdded)
	assert.False(sum.Partial)
	assert.Contains(sum.Note, "20 most recent messages per thread")
	s, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	m := message(t, st, s.ID, "m_0")
	blob, err := st.GetConversationMetadata(m.ConversationID)
	require.NoError(err)
	assert.Contains(blob.String, `"backfill_capped":true`)
}
func TestDMImportBudgetPauseResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 7)
	sum, err := imp.Import(t.Context(), src, ImportOptions{MaxRequests: 5})
	require.NoError(err)
	require.True(sum.Partial)
	assert.Equal(5, sum.Requests)
	assert.Equal(5, f.calls)
	sum, err = imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(1, f.nodes["m_0"])
	s, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	n, err := st.CountMessagesWithRaw(s.ID)
	require.NoError(err)
	assert.EqualValues(7, n)
}
func TestDMImportHighWaterSkipsOlderConversations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, f, imp, src := setupDMBackfill(t, PlatformMessenger, 2)
	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	lists := f.conversations
	f.newer = true
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(1, sum.MessagesAdded)
	assert.Equal(2, f.nodes["m_0"])
	assert.Equal(lists+2, f.conversations)
	assert.Equal(1, f.nodes["new_1"])
	_, err = imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.Equal(2, f.nodes["m_0"])
}
func TestDMImportFullRescan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 2)
	f.newer = true
	f.firstUpdated = "2026-11-01T00:00:00+0000"
	f.secondUpdated = "2026-11-02T00:00:00+0000"
	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	f.text = "full scan refreshed the old conversation"
	sum, err := imp.Import(t.Context(), src, ImportOptions{Full: true})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Zero(sum.MessagesAdded)
	assert.Equal(2, f.nodes["m_0"])
	source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	m := message(t, st, source.ID, "m_0")
	body, err := st.GetMessageBodyText(m.ID)
	require.NoError(err)
	assert.Equal(f.text, body)
}

func TestDMImportNodeFailureRetainsCheckpoint(t *testing.T) {
	for _, failure := range []APIError{{Status: 400, Code: 190}, {Status: 400, Code: 4}, {Status: 400, Code: 100}, {Status: 400, Code: 100, Subcode: 33}, {Status: 400}, {Status: 500}} {
		t.Run(failure.Error(), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			_, f, imp, src := setupDMBackfill(t, PlatformMessenger, 2)
			f.nodeError = &failure
			sum, err := imp.Import(t.Context(), src, ImportOptions{})
			require.Error(err)
			assert.True(sum.Partial)
			assert.Zero(sum.MessagesSkipped)
			assert.Zero(f.nodes["m_1"])
			f.nodeError = nil
			sum, err = imp.Import(t.Context(), src, ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(2, sum.MessagesAdded)
			assert.Equal(2, f.nodes["m_0"])
		})
	}
}

func TestDMImportEqualTimestampFindsNewArrival(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, f, imp, src := setupDMBackfill(t, PlatformMessenger, 1)
	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	f.count = 2
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(1, sum.MessagesAdded)
	assert.Equal(1, f.nodes["m_1"])
}

func TestDMImportPausedFullScanRefreshesRemainingNodes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 2)
	f.newer = true
	f.firstUpdated = "2026-11-01T00:00:00+0000"
	f.secondUpdated = "2026-11-02T00:00:00+0000"
	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	f.text = "updated fixture text"
	sum, err := imp.Import(t.Context(), src, ImportOptions{Full: true, MaxRequests: 4})
	require.NoError(err)
	require.True(sum.Partial)
	assert.Equal(2, f.nodes["m_0"])
	assert.Equal(1, f.nodes["m_1"])
	sum, err = imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(2, f.nodes["m_0"])
	assert.Equal(2, f.nodes["m_1"])
	source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	m := message(t, st, source.ID, "m_1")
	body, err := st.GetMessageBodyText(m.ID)
	require.NoError(err)
	assert.Equal(f.text, body)
}
func TestDMImportPermissionDeniedPartial(t *testing.T) {
	for _, p := range []DMPlatform{PlatformMessenger, PlatformInstagram} {
		t.Run(string(p), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, f, imp, src := setupDMBackfill(t, p, 2)
			f.denied = true
			sum, err := imp.Import(t.Context(), src, ImportOptions{})
			require.ErrorContains(err, "permission_missing: needs ")
			require.True(sum.Partial)
			if p == PlatformMessenger {
				assert.Contains(err.Error(), "pages_messaging, pages_manage_metadata, pages_read_engagement")
			} else {
				assert.Contains(err.Error(), "instagram_basic, instagram_manage_messages, pages_manage_metadata")
			}
			s, err := st.GetSourceByTypeAndIdentifier(p.SourceType(), src.Identifier())
			require.NoError(err)
			run, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), s.ID, p.SourceType())
			require.NoError(err)
			assert.NotEmpty(run.CursorBefore.String)
		})
	}
}
func TestDMImportOldMessageDeletedErrorSkipped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 2)
	f.missing = true
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.Equal(1, sum.MessagesSkipped)
	assert.Equal(1, sum.MessagesAdded)
	s, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	ids, err := st.MessageExistsBatch(s.ID, []string{"m_0"})
	require.NoError(err)
	assert.Empty(ids)
}
func TestDMImportExpiredCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 2)
	f.newer = true
	f.firstUpdated = "2026-11-01T00:00:00+0000"
	f.secondUpdated = "2026-11-02T00:00:00+0000"
	_, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	assert.Equal(1, f.nodes["m_0"])

	f.expired = true
	sum, err := imp.Import(t.Context(), src, ImportOptions{Full: true})
	require.ErrorContains(err, "facebook continuation request failed")
	assert.True(sum.Partial)
	assert.Equal(2, f.nodes["m_0"])

	f.expired = false
	f.text = "full retry refreshed the old conversation"
	sum, err = imp.Import(t.Context(), src, ImportOptions{Full: true})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(3, f.nodes["m_0"])
	source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	m := message(t, st, source.ID, "m_0")
	body, err := st.GetMessageBodyText(m.ID)
	require.NoError(err)
	assert.Equal(f.text, body)
}

func TestDMImportPausedScanKeepsArrivalBoundary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nanosecond  int
		firstUpdate string
	}{
		{"later second", 0, "2026-11-01T01:30:00+0000"},
		{"same second as fractional start", 500_000_000, "2026-11-01T01:00:00+0000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			_, f, imp, src := setupDMBackfill(t, PlatformMessenger, 1)
			clock := time.Date(2026, 11, 1, 1, 0, 0, tc.nanosecond, time.UTC)
			imp.now = func() time.Time { return clock }
			sum, err := imp.Import(t.Context(), src, ImportOptions{MaxRequests: 4})
			require.NoError(err)
			require.True(sum.Partial)
			// The first thread changes after it was read. Provider timestamps
			// can round that arrival down to the scan's starting second.
			f.count = 2
			f.firstUpdated = tc.firstUpdate
			f.secondUpdated = "2026-11-02T00:00:00+0000"
			f.newer = true
			clock = clock.Add(48 * time.Hour)
			sum, err = imp.Import(t.Context(), src, ImportOptions{})
			require.NoError(err)
			require.False(sum.Partial)
			assert.Zero(f.nodes["m_1"])
			sum, err = imp.Import(t.Context(), src, ImportOptions{})
			require.NoError(err)
			assert.Equal(1, sum.MessagesAdded)
			assert.Equal(1, f.nodes["m_1"])
		})
	}
}
