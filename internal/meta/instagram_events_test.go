package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type instagramEventFixture struct {
	mu     sync.Mutex
	nodes  map[string]string
	paths  []string
	fields map[string]string
	status int
}

func newInstagramEventFixture(t *testing.T) (*instagramEventFixture, *Client) {
	t.Helper()
	assert := assert.New(t)
	f := &instagramEventFixture{nodes: map[string]string{
		"me":  `{"id":"100","name":"Example Page","category":"Community"}`,
		"100": `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`,
		"300": `{"id":"300","caption":"Synthetic media","timestamp":"2000-01-02T03:04:05+0000","owner":{"id":"200"}}`,
		"400": `{"id":"400","text":"nodeorchard","timestamp":"2000-01-02T04:00:00+0000","from":{"id":"700","username":"example_customer"},"media":{"id":"300"}}`,
		"500": `{"id":"500","text":"node reply","timestamp":"2000-01-02T05:00:00+0000","parent_id":"400","media":{"id":"300"}}`,
	}, fields: map[string]string{}}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/v26.0/")
		f.paths = append(f.paths, id)
		f.fields[id] = r.URL.Query().Get(graphFieldsParam)
		assert.Empty(r.URL.Query().Get("access_token"))
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = fmt.Fprint(w, `{"error":{"code":200}}`)
			return
		}
		wire, ok := f.nodes[id]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"code":100,"error_subcode":33}}`)
			return
		}
		_, _ = fmt.Fprint(w, wire)
	})
	return f, c
}

func (f *instagramEventFixture) node(id, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[id] = body
}

func (f *instagramEventFixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

func instagramEvent(t *testing.T, id string) InstagramCommentEvent {
	t.Helper()
	require := require.New(t)
	var event InstagramCommentEvent
	require.NoError(json.Unmarshal([]byte(`{"id":"`+id+`","text":"untrusted payload text","media":{"id":"300","ad_id":"900"}}`), &event))
	event.Time = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	return event
}

type instagramEventSetup struct {
	store    *store.Store
	fixture  *instagramEventFixture
	client   *Client
	importer *InstagramEventImporter
	source   *store.Source
}

func setupInstagramEvents(t *testing.T) instagramEventSetup {
	t.Helper()
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, c := newInstagramEventFixture(t)
	src, err := NewImporter(st, c).RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	return instagramEventSetup{store: st, fixture: f, client: c, importer: NewInstagramEventImporter(st, c), source: src}
}

func applyInstagram(t *testing.T, imp *InstagramEventImporter, events ...InstagramCommentEvent) []EventOutcome {
	t.Helper()
	require := require.New(t)
	out, err := imp.ApplyEvents(t.Context(), "9", "100", "200", events, EventOptions{})
	require.NoError(err)
	require.Len(out, len(events))
	return out
}

func instagramEventMetadata(t *testing.T, st *store.Store, src int64, id string) objectMetadata {
	t.Helper()
	require := require.New(t)
	row := message(t, st, src, id)
	saved, err := st.GetMessageMetadataContext(t.Context(), row.ID)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
	return metadata
}

func TestInstagramEventNodeReads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, c := newInstagramEventFixture(t)
	comment, err := c.InstagramComment(t.Context(), "400")
	require.NoError(err)
	assert.Equal("400", comment.ID)
	assert.Equal("300", comment.Media.ID)
	assert.Equal("nodeorchard", comment.Text)
	assert.Equal("2000-01-02T04:00:00+0000", comment.Timestamp)
	media, err := c.InstagramMediaNode(t.Context(), "300")
	require.NoError(err)
	assert.Equal("200", media.Owner.ID)
	f.mu.Lock()
	assert.Equal(instagramCommentFields+",media{id}", f.fields["400"])
	assert.Equal("id,caption,timestamp,permalink,owner{id}", f.fields["300"])
	f.mu.Unlock()
	for _, id := range []string{"../400", "https://example.test/400", ""} {
		before := f.requests()
		_, err = c.InstagramComment(t.Context(), id)
		require.Error(err)
		assert.Equal(before, f.requests())
	}
	f.node("400", `{"id":"401"}`)
	_, err = c.InstagramComment(t.Context(), "400")
	require.Error(err)
	f.node("300", `{"id":"301"}`)
	_, err = c.InstagramMediaNode(t.Context(), "300")
	require.Error(err)
}

func TestInstagramEventUsesNodeTimestampAndProvenance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := setupInstagramEvents(t)
	st, f, imp, src := env.store, env.fixture, env.importer, env.source
	event := instagramEvent(t, "400")
	assert.Equal("applied", applyInstagram(t, imp, event)[0].Class)
	row := message(t, st, src.ID, "400")
	root := message(t, st, src.ID, "300")
	assert.Equal(root.ConversationID, row.ConversationID)
	assert.Equal(root.ID, row.ReplyToMessageID.Int64)
	stored, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"400"})
	require.NoError(err)
	assert.Equal(time.Date(2000, 1, 2, 4, 0, 0, 0, time.UTC), stored["400"].SentAt.Time.UTC())
	body, err := st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("nodeorchard", body)
	metadata := instagramEventMetadata(t, st, src.ID, "400")
	assert.True(metadata.matches("9", "100", "200", "300"))
	assert.Equal(event.Time, metadata.BodyTime)
	require.NotNil(metadata.WebhookMedia)
	assert.Equal("900", metadata.WebhookMedia.AdID)
	assert.False(metadata.Ad)
	assert.False(instagramEventMetadata(t, st, src.ID, "300").Ad)
	assert.Equal([]string{"me", "100", "400", "300"}, f.requests())

	assert.Equal("applied", applyInstagram(t, imp, event)[0].Class)
	assert.Equal(row.ID, message(t, st, src.ID, "400").ID)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(2, count)
	completed, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), src.ID, InstagramSourceType+"_webhook")
	require.NoError(err)
	require.NotNil(completed)
	poll, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), src.ID, InstagramSourceType)
	require.ErrorIs(err, store.ErrSyncRunNotFound)
	assert.Nil(poll)
}

func TestInstagramEventReplyChain(t *testing.T) {
	assert := assert.New(t)
	env := setupInstagramEvents(t)
	st, imp, src := env.store, env.importer, env.source
	assert.Equal("applied", applyInstagram(t, imp, instagramEvent(t, "500"))[0].Class)
	root, parent, reply := message(t, st, src.ID, "300"), message(t, st, src.ID, "400"), message(t, st, src.ID, "500")
	assert.Equal(root.ID, parent.ReplyToMessageID.Int64)
	assert.Equal(parent.ID, reply.ReplyToMessageID.Int64)
	assert.Equal(root.ConversationID, reply.ConversationID)
}

func TestInstagramEventRejectsUnverifiedContext(t *testing.T) {
	for _, tc := range []struct{ name, node, wire string }{
		{"missing timestamp", "400", `{"id":"400","media":{"id":"300"}}`},
		{"foreign media", "400", `{"id":"400","timestamp":"2000-01-02T04:00:00Z","media":{"id":"301"}}`},
		{"missing media", "400", `{"id":"400","timestamp":"2000-01-02T04:00:00Z"}`},
		{"foreign owner", "300", `{"id":"300","owner":{"id":"201"}}`},
		{"missing owner", "300", `{"id":"300"}`},
		{"cycle", "400", `{"id":"400","timestamp":"2000-01-02T04:00:00Z","parent_id":"400","media":{"id":"300"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			env := setupInstagramEvents(t)
			st, f, imp, src := env.store, env.fixture, env.importer, env.source
			f.node(tc.node, tc.wire)
			assert.Equal("invalid", applyInstagram(t, imp, instagramEvent(t, "400"))[0].Class)
			var count int
			require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
			assert.Zero(count)
		})
	}
}

func TestInstagramEventRequestBudgetAndDepth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := setupInstagramEvents(t)
	f, imp := env.fixture, env.importer
	before := len(f.requests())
	out, err := imp.ApplyEvents(t.Context(), "9", "100", "200", []InstagramCommentEvent{instagramEvent(t, "400")}, EventOptions{MaxRequests: 1})
	require.NoError(err)
	assert.Equal("retry", out[0].Class)
	assert.Equal("request_budget", out[0].ErrorClass)
	assert.Equal(1, len(f.requests())-before)

	for i := 400; i < 410; i++ {
		f.node(strconv.Itoa(i), fmt.Sprintf(`{"id":"%d","timestamp":"2000-01-02T04:00:00Z","parent_id":"%d","media":{"id":"300"}}`, i, i+1))
	}
	out = applyInstagram(t, imp, instagramEvent(t, "400"))
	assert.Equal("invalid", out[0].Class)
	assert.Equal("invalid_ancestry", out[0].ErrorClass)
}

func TestInstagramEventBusyAndUnrouted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := setupInstagramEvents(t)
	st, f, imp, src := env.store, env.fixture, env.importer, env.source
	exec, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(exec.Release()) })
	before := f.requests()
	out := applyInstagram(t, imp, instagramEvent(t, "400"))
	assert.Equal("retry", out[0].Class)
	assert.Equal("source_busy", out[0].ErrorClass)
	assert.False(out[0].CountAttempt)
	assert.Equal(before, f.requests())
	require.NoError(exec.Release())
	out, err = imp.ApplyEvents(t.Context(), "9", "100", "201", []InstagramCommentEvent{instagramEvent(t, "400")}, EventOptions{})
	require.NoError(err)
	assert.Equal("unrouted", out[0].Class)
	assert.Equal(before, f.requests())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err = imp.ApplyEvents(ctx, "9", "100", "200", []InstagramCommentEvent{instagramEvent(t, "400")}, EventOptions{})
	require.ErrorIs(err, context.Canceled)
	assert.Equal("retry", out[0].Class)
	assert.Equal(before, f.requests())
}

func TestInstagramEventGraphErrorsRetainedForRetry(t *testing.T) {
	assert := assert.New(t)
	env := setupInstagramEvents(t)
	f, imp := env.fixture, env.importer
	f.mu.Lock()
	f.status = http.StatusForbidden
	f.mu.Unlock()
	out := applyInstagram(t, imp, instagramEvent(t, "400"))
	assert.Equal("retry", out[0].Class)
	assert.True(out[0].CountAttempt)
	assert.Equal("permission_missing", out[0].ErrorClass)
}

func TestInstagramEventUpdatesKeepPriorEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := setupInstagramEvents(t)
	st, f, imp, src := env.store, env.fixture, env.importer, env.source
	event := instagramEvent(t, "400")
	require.Equal("applied", applyInstagram(t, imp, event)[0].Class)
	original := message(t, st, src.ID, "400")
	f.node("400", `{"id":"400","text":"updated orchard","timestamp":"2000-01-02T04:00:00+0000","media":{"id":"300"}}`)
	event.Media = InstagramWebhookMedia{ID: "300"}
	event.Time++
	require.Equal("applied", applyInstagram(t, imp, event)[0].Class)
	updated := message(t, st, src.ID, "400")
	assert.Equal(original.ID, updated.ID)
	assert.Equal(original.SenderID, updated.SenderID)
	metadata := instagramEventMetadata(t, st, src.ID, "400")
	require.NotNil(metadata.WebhookMedia)
	assert.Equal("900", metadata.WebhookMedia.AdID)
	assert.Equal(event.Time, metadata.BodyTime)

	f.node("400", `{"id":"400","text":"equal time repair","timestamp":"2000-01-02T04:00:00+0000","media":{"id":"300"}}`)
	require.Equal("applied", applyInstagram(t, imp, event)[0].Class)
	body, err := st.GetMessageBodyText(original.ID)
	require.NoError(err)
	assert.Equal("equal time repair", body)
	before := f.requests()
	event.Time--
	assert.Equal("ignored", applyInstagram(t, imp, event)[0].Class)
	assert.Equal(before, f.requests())
}

func TestInstagramEventDoesNotReplacePollingCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := setupInstagramEvents(t)
	st, f, c, imp, src := env.store, env.fixture, env.client, env.importer, env.source
	f.node("200/media", `{"data":[{"id":"300","caption":"Synthetic media","timestamp":"2000-01-02T03:04:05+0000","owner":{"id":"200"}}]}`)
	f.node("300/comments", `{"data":[{"id":"400","text":"polled orchard","timestamp":"2000-01-02T04:00:00+0000"}]}`)
	f.node("400/replies", `{"data":[]}`)
	poller := NewImporter(st, c)
	summary, err := poller.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 3})
	require.NoError(err)
	require.True(summary.Partial)
	before, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, InstagramSourceType)
	require.NoError(err)
	require.NotNil(before)
	state, err := loadState(t.Context(), st, src, false)
	require.NoError(err)
	require.Equal("applied", applyInstagram(t, imp, instagramEvent(t, "400"))[0].Class)
	row := message(t, st, src.ID, "400")
	after, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, InstagramSourceType)
	require.NoError(err)
	assert.Equal(before.ID, after.ID)
	assert.Equal(before.CursorBefore, after.CursorBefore)
	loaded, err := loadState(t.Context(), st, src, false)
	require.NoError(err)
	assert.Equal(state, loaded)
	summary, err = poller.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	assert.False(summary.Partial)
	assert.Equal(row.ID, message(t, st, src.ID, "400").ID)
	body, err := st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("polled orchard", body)
	metadata := instagramEventMetadata(t, st, src.ID, "400")
	assert.True(instagramEventMetadata(t, st, src.ID, "300").Organic)
	require.NotNil(metadata.WebhookMedia)
	assert.Equal("900", metadata.WebhookMedia.AdID)
	assert.Equal(row.SenderID, message(t, st, src.ID, "400").SenderID)
}

func TestInstagramEventRetiredSourceAndArchiveFailure(t *testing.T) {
	t.Run("retired", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		env := setupInstagramEvents(t)
		st, f, imp, src := env.store, env.fixture, env.importer, env.source
		other, err := st.GetOrCreateSource(InstagramSourceType, InstagramSourceIdentifier("99", "100", "200"))
		require.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
		require.NoError(err)
		before := f.requests()
		out := applyInstagram(t, imp, instagramEvent(t, "400"))[0]
		assert.Equal("unrouted", out.Class)
		assert.Equal("source_retired", out.ErrorClass)
		assert.Equal(before, f.requests())
	})
	t.Run("archive retry", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		env := setupInstagramEvents(t)
		st, imp := env.store, env.importer
		_, err := st.DB().Exec(`CREATE TRIGGER fail_instagram_write BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'synthetic unavailable'); END`)
		require.NoError(err)
		out := applyInstagram(t, imp, instagramEvent(t, "400"))[0]
		assert.Equal("retry", out.Class)
		assert.True(out.CountAttempt)
		_, err = st.DB().Exec(`DROP TRIGGER fail_instagram_write`)
		require.NoError(err)
		assert.Equal("applied", applyInstagram(t, imp, instagramEvent(t, "400"))[0].Class)
	})
	t.Run("not found retry", func(t *testing.T) {
		assert := assert.New(t)
		env := setupInstagramEvents(t)
		imp := env.importer
		out := applyInstagram(t, imp, instagramEvent(t, "600"))[0]
		assert.Equal("retry", out.Class)
		assert.True(out.CountAttempt)
		assert.Equal(string(ClassRejected), out.ErrorClass)
	})
}

func TestInstagramPollingCachedAncestorKeepsNewerNodeBody(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	env := setupInstagramEvents(t)
	f, st := env.fixture, env.store
	f.node("200/media", `{"data":[{"id":"300","caption":"Synthetic media","timestamp":"2000-01-02T03:04:05Z","owner":{"id":"200"}}]}`)
	f.node("300/comments", `{"data":[{"id":"401","text":"first comment","timestamp":"2000-01-02T04:00:00Z"},{"id":"400","text":"cached parent","timestamp":"2000-01-02T04:00:00Z"}]}`)
	f.node("401/replies", `{"data":[]}`)
	f.node("400/replies", `{"data":[]}`)
	poller := NewImporter(st, env.client)
	summary, err := poller.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 4})
	require.NoError(err)
	require.True(summary.Partial)
	require.Contains(f.requests(), "300/comments", "the parent page must be cached before the webhook")
	out := applyInstagram(t, env.importer, instagramEvent(t, "500"))
	require.Equal("applied", out[0].Class)
	_, err = poller.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	row := message(t, st, env.source.ID, "400")
	body, err := st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("nodeorchard", body, "the pending cached parent must not replace a newer ancestor fetch")
	f.node("300/comments", `{"data":[{"id":"400","text":"fresh polling parent","timestamp":"2000-01-02T04:00:00Z"}]}`)
	_, err = poller.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	body, err = st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("fresh polling parent", body)
}
