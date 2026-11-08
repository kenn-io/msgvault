package feeds

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const atomFeed = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example Project releases</title>
  <link href="https://example.com/"/>
  <id>urn:example:releases</id>
  <updated>2026-11-07T15:00:00Z</updated>
  <entry>
    <title>Example Project 1.2.0</title>
    <link href="/releases/1.2.0"/>
    <id>urn:example:release:1.2.0</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <author><name>Release Bot</name></author>
    <content type="html">&lt;p&gt;Faster &lt;b&gt;widgetsync&lt;/b&gt;.&lt;/p&gt;</content>
  </entry>
  <entry>
    <title>Example Project 1.1.0</title>
    <link href="https://example.com/releases/1.1.0"/>
    <id>urn:example:release:1.1.0</id>
    <updated>2026-10-01T12:00:00Z</updated>
    <author><name>Jordan Example</name><uri>https://jordan.example.org/</uri></author>
    <summary>First gizmo release.</summary>
  </entry>
</feed>`

const rssFeed = `<?xml version="1.0"?>
<rss version="2.0">
  <channel>
    <title>Example Notes</title>
    <link>https://notes.example.net/</link>
    <description>Notes</description>
    <item>
      <title>Undated note</title>
      <link>https://notes.example.net/undated</link>
      <description>A note with no date and no guid.</description>
    </item>
    <item>
      <title>Dated note</title>
      <guid isPermaLink="false">note-2</guid>
      <pubDate>Fri, 06 Nov 2026 09:30:00 -0500</pubDate>
      <description>&lt;p&gt;Second note about quokkas.&lt;/p&gt;</description>
      <category>animals</category>
      <enclosure url="https://notes.example.net/note-2.mp3" length="1234" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`

const jsonFeed = `{
  "version": "https://jsonfeed.org/version/1.1",
  "title": "Example JSON",
  "home_page_url": "https://json.example.com/",
  "items": [
    {"id": "j-1", "url": "https://json.example.com/j-1", "title": "First JSON item",
     "content_text": "Plain text about narwhals.", "date_published": "2026-11-05T08:00:00Z"}
  ]
}`

// feedServer serves feeds by path and answers conditional requests.
type feedServer struct {
	mu        sync.Mutex
	bodies    map[string]string
	requests  map[string][]http.Header
	onRequest func(path string)
	server    *httptest.Server
}

func newFeedServer(t *testing.T, bodies map[string]string) *feedServer {
	t.Helper()
	fs := &feedServer{bodies: bodies, requests: map[string][]http.Header{}}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		body, ok := fs.bodies[r.URL.Path]
		fs.requests[r.URL.Path] = append(fs.requests[r.URL.Path], r.Header.Clone())
		onRequest := fs.onRequest
		fs.mu.Unlock()
		if onRequest != nil {
			onRequest(r.URL.Path)
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `"` + hashParts(body)[:16] + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Sat, 07 Nov 2026 15:00:00 GMT")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *feedServer) url(path string) string { return fs.server.URL + path }

func (fs *feedServer) set(path, body string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.bodies[path] = body
}

func (fs *feedServer) requestHeaders(path string) []http.Header {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]http.Header(nil), fs.requests[path]...)
}

func newTestSyncer(st *store.Store, now time.Time) *Syncer {
	syncer := NewSyncer(st, NewFetcher("msgvault-test"), nil)
	syncer.now = func() time.Time { return now }
	return syncer
}

type archivedItem struct {
	ID          int64
	Key         string
	MessageType string
	Subject     string
	SentAt      time.Time
	Body        string
	BodyHTML    sql.NullString
	Sender      string
	SenderType  string
	SenderValue string
	Feed        string
	FeedTitle   string
	Metadata    string
}

// archivedItems returns the archived feed items keyed by their ID within
// their feed: the item key without its feed prefix.
func archivedItems(t *testing.T, st *store.Store) map[string]archivedItem {
	t.Helper()
	items := map[string]archivedItem{}
	for _, item := range archivedItemList(t, st) {
		_, id, _ := strings.Cut(item.Key, ":")
		items[id] = item
	}
	return items
}

func archivedItemList(t *testing.T, st *store.Store) []archivedItem {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.id, m.source_message_id, m.message_type, COALESCE(m.subject, ''), m.sent_at,
		       COALESCE(mb.body_text, ''), mb.body_html, COALESCE(p.display_name, ''),
		       COALESCE(pi.identifier_type, ''), COALESCE(pi.identifier_value, ''),
		       c.source_conversation_id, COALESCE(c.title, ''), COALESCE(CAST(m.metadata AS TEXT), '')
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		LEFT JOIN participants p ON p.id = m.sender_id
		LEFT JOIN participant_identifiers pi ON pi.participant_id = p.id
		WHERE s.source_type = ?
	`), SourceType)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var items []archivedItem
	for rows.Next() {
		var item archivedItem
		require.NoError(t, rows.Scan(&item.ID, &item.Key, &item.MessageType, &item.Subject, &item.SentAt,
			&item.Body, &item.BodyHTML, &item.Sender, &item.SenderType, &item.SenderValue,
			&item.Feed, &item.FeedTitle, &item.Metadata))
		items = append(items, item)
	}
	require.NoError(t, rows.Err())
	return items
}

func TestFollowArchivesAtomItemsAsFeedMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/releases.atom": atomFeed})
	summary, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{server.url("/releases.atom")})
	require.NoError(err)
	require.Len(summary.Feeds, 1)
	require.NoError(summary.Feeds[0].Err)
	assert.Equal(2, summary.Added())

	source, err := st.GetSourceByID(summary.SourceID)
	require.NoError(err)
	assert.True(source.LastSyncAt.Valid, "a completed sync records when the source last synced")

	items := archivedItems(t, st)
	require.Len(items, 2)
	release := items["urn:example:release:1.2.0"]
	assert.Equal(MessageType, release.MessageType)
	assert.Equal("Example Project 1.2.0", release.Subject)
	assert.True(release.SentAt.Equal(time.Date(2026, time.November, 7, 15, 0, 0, 0, time.UTC)))
	assert.Equal("Faster widgetsync.\n\n"+server.url("/releases/1.2.0"), release.Body,
		"a relative link resolves against the feed document, not the site")
	assert.Contains(release.BodyHTML.String, "<b>widgetsync</b>")
	assert.Equal("Release Bot", release.Sender)
	assert.True(strings.HasPrefix(release.SenderValue, "id:feeds:author:"), "a name-only author is scoped to its feed")
	assert.Equal(server.url("/releases.atom"), release.Feed)
	assert.Equal("Example Project releases", release.FeedTitle)

	older := items["urn:example:release:1.1.0"]
	assert.Equal("Jordan Example", older.Sender)
	assert.Equal("url:https://jordan.example.org/", older.SenderValue)
	assert.Contains(older.Body, "First gizmo release.")

	list, err := NewRegistry(st).List(t.Context())
	require.NoError(err)
	require.Len(list, 1)
	assert.True(list[0].Followed())
	assert.Equal("Example Project releases", list[0].State.Title)
	assert.NotEmpty(list[0].State.ETag)
	assert.NotNil(list[0].State.CheckedAt)

	ids, _, err := st.SearchMessageIDsQueryContext(t.Context(),
		&search.Query{TextTerms: []string{"widgetsync"}, MessageTypes: []string{MessageType}}, 10)
	require.NoError(err)
	assert.Equal([]int64{release.ID}, ids, "items are searchable by type and text")
}

func TestSyncUsesValidatorsAndArchivesEachItemOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/releases.atom": atomFeed})
	feedURL := server.url("/releases.atom")
	syncer := newTestSyncer(st, time.Now())
	_, err := syncer.Follow(t.Context(), []string{feedURL})
	require.NoError(err)

	summary, err := syncer.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	require.Len(summary.Feeds, 1)
	assert.True(summary.Feeds[0].NotModified, "an unchanged feed answers 304")
	headers := server.requestHeaders("/releases.atom")
	require.Len(headers, 2)
	assert.NotEmpty(headers[1].Get("If-None-Match"))
	assert.Equal("Sat, 07 Nov 2026 15:00:00 GMT", headers[1].Get("If-Modified-Since"))

	edited := strings.Replace(atomFeed, "Faster &lt;b&gt;widgetsync", "Edited text", 1)
	edited = strings.Replace(edited, "<entry>", `<entry>
    <title>Example Project 1.3.0</title>
    <link href="https://example.com/releases/1.3.0"/>
    <id>urn:example:release:1.3.0</id>
    <updated>2026-11-08T15:00:00Z</updated>
    <content type="html">Gizmo cache.</content>
  </entry>
  <entry>`, 1)
	server.set("/releases.atom", edited)
	summary, err = syncer.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(1, summary.Added(), "only the new item is added")

	items := archivedItems(t, st)
	require.Len(items, 3)
	assert.Contains(items["urn:example:release:1.2.0"].Body, "widgetsync", "an archived item stays as first archived")
	assert.Equal("Example Project 1.3.0", items["urn:example:release:1.3.0"].Subject)
	assert.Equal("Example Project releases", items["urn:example:release:1.3.0"].Sender, "an item without an author is from the feed's site")
	assert.Equal("url:https://example.com/", items["urn:example:release:1.3.0"].SenderValue)

	summary, err = syncer.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.False(summary.Feeds[0].NotModified, "--full downloads the feed again")
	assert.Equal(0, summary.Added())
}

func TestSyncArchivesRSSAndJSONFeeds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/notes.rss": rssFeed, "/feed.json": jsonFeed})
	firstSeen := time.Date(2026, time.November, 7, 20, 0, 0, 0, time.UTC)
	syncer := newTestSyncer(st, firstSeen)
	summary, err := syncer.Follow(t.Context(), []string{server.url("/notes.rss"), server.url("/feed.json")})
	require.NoError(err)
	assert.Equal(3, summary.Added())

	items := archivedItems(t, st)
	undated := items["https://notes.example.net/undated"]
	assert.Equal("Undated note", undated.Subject, "an item without a guid is keyed by its link")
	assert.True(undated.SentAt.Equal(firstSeen), "an undated item is dated when first seen")
	assert.Equal("Example Notes", undated.Sender, "without an author the feed's site sent the item")
	assert.Equal("url:https://notes.example.net/", undated.SenderValue)

	dated := items["note-2"]
	assert.True(dated.SentAt.Equal(time.Date(2026, time.November, 6, 14, 30, 0, 0, time.UTC)))
	assert.Contains(dated.Body, "Second note about quokkas.")
	var metadata struct {
		ProviderMetadata struct {
			Categories []string            `json:"categories"`
			Enclosures []map[string]string `json:"enclosures"`
		} `json:"provider_metadata"`
	}
	require.NoError(json.Unmarshal([]byte(dated.Metadata), &metadata))
	assert.Equal([]string{"animals"}, metadata.ProviderMetadata.Categories)
	assert.Equal([]map[string]string{{
		"url": "https://notes.example.net/note-2.mp3", "type": "audio/mpeg", "length": "1234",
	}}, metadata.ProviderMetadata.Enclosures)

	jsonItem := items["j-1"]
	assert.Equal("First JSON item", jsonItem.Subject)
	assert.Contains(jsonItem.Body, "Plain text about narwhals.")

	syncer.now = func() time.Time { return firstSeen.Add(24 * time.Hour) }
	_, err = syncer.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.True(archivedItems(t, st)["https://notes.example.net/undated"].SentAt.Equal(firstSeen),
		"the first-seen date does not move")
}

func TestFollowRejectsURLsThatAreNotFeeds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{
		"/page.html":     "<!doctype html><html><body>Not a feed</body></html>",
		"/releases.atom": atomFeed,
	})
	summary, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{
		server.url("/page.html"), server.url("/missing.xml"), server.url("/releases.atom"),
	})
	require.NoError(err)
	require.Len(summary.Feeds, 3)
	require.ErrorIs(summary.Feeds[0].Err, ErrNotAFeed)
	require.Error(summary.Feeds[1].Err)
	assert.Contains(summary.Feeds[1].Err.Error(), "404")
	require.NoError(summary.Feeds[2].Err)
	assert.Equal(2, summary.Failed())
	require.NoError(summary.SourceErr(), "one working feed means the source works")
	latest, err := st.GetLatestSync(summary.SourceID)
	require.NoError(err)
	assert.Equal(int64(2), latest.ErrorsCount, "feeds that fail to follow are errors of the run")

	list, err := NewRegistry(st).List(t.Context())
	require.NoError(err)
	require.Len(list, 1, "only the reachable feed is followed")
	assert.Equal(server.url("/releases.atom"), list[0].URL)

	_, err = newTestSyncer(st, time.Now()).Follow(t.Context(), []string{"ftp://example.com/feed"})
	require.ErrorIs(err, ErrInvalidURL)
}

func TestSyncRecordsAFailingFeedAndSyncsTheRest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/a.atom": atomFeed, "/b.rss": rssFeed})
	syncer := newTestSyncer(st, time.Now())
	_, err := syncer.Follow(t.Context(), []string{server.url("/a.atom"), server.url("/b.rss")})
	require.NoError(err)

	server.set("/a.atom", "not xml at all")
	server.set("/b.rss", strings.Replace(rssFeed, "<item>", `<item>
      <title>Note after the failure</title>
      <guid>note-3</guid>
      <description>Archived although another feed failed.</description>
    </item>
    <item>`, 1))
	summary, err := syncer.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err, "one broken feed does not fail the sync")
	assert.Equal(1, summary.Failed())
	assert.Equal(1, summary.Added(), "the feed after the failing one still syncs")
	assert.Equal("Note after the failure", archivedItems(t, st)["note-3"].Subject)

	latest, err := st.GetLatestSync(summary.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.ErrorsCount, "the failed feed is an error of the run")
	failures, err := st.CountSyncRunItemsContext(t.Context(), latest.ID, store.SyncRunItemStatusError)
	require.NoError(err)
	assert.Equal(int64(1), failures)

	list, err := NewRegistry(st).List(t.Context())
	require.NoError(err)
	byURL := map[string]Feed{}
	for _, feed := range list {
		byURL[feed.URL] = feed
	}
	assert.Contains(byURL[server.url("/a.atom")].State.LastError, "not an RSS, Atom or JSON feed")
	assert.NotEmpty(byURL[server.url("/a.atom")].State.ETag, "a failed fetch keeps the last good validators")
	assert.Empty(byURL[server.url("/b.rss")].State.LastError)
}

func TestRelativeLinksResolveAgainstTheFeedDocument(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old.atom":
			http.Redirect(w, r, server.URL+"/blog/feed.atom", http.StatusMovedPermanently)
		case "/blog/feed.atom":
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example blog</title>
  <link href="https://example.com/"/>
  <id>urn:example:blog</id>
  <updated>2026-11-07T15:00:00Z</updated>
  <entry>
    <title>First post</title>
    <link href="posts/1"/>
    <id>urn:example:post:1</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <content type="html">Hello.</content>
  </entry>
  <entry>
    <title>Chart post</title>
    <link href="posts/2"/>
    <id>urn:example:post:2</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <content type="html">&lt;img src="images/chart.png"&gt; &lt;a href="#notes"&gt;notes&lt;/a&gt;</content>
  </entry>
</feed>`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	st := testutil.NewTestStore(t)
	summary, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{server.URL + "/old.atom"})
	require.NoError(err)
	require.NoError(summary.Feeds[0].Err)
	post := archivedItems(t, st)["urn:example:post:1"]
	assert.Equal("Hello.\n\n"+server.URL+"/blog/posts/1", post.Body,
		"resolved against where the document came from after the redirect")
	assert.Equal("url:https://example.com/", post.SenderValue, "the feed's site still sends an authorless item")
	chart := archivedItems(t, st)["urn:example:post:2"].BodyHTML.String
	assert.Contains(chart, `src="`+server.URL+`/blog/images/chart.png"`, "the HTML's relative image is made absolute")
	assert.Contains(chart, `href="#notes"`)
}

func TestSyncFetchesTheLeastRecentlyCheckedFeedsFirst(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/a.atom": atomFeed, "/b.rss": rssFeed, "/c.json": jsonFeed})
	clock := time.Date(2026, time.November, 7, 12, 0, 0, 0, time.UTC)
	syncer := newTestSyncer(st, clock)
	syncer.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	a, b, c := server.url("/a.atom"), server.url("/b.rss"), server.url("/c.json")
	_, err := syncer.Follow(t.Context(), []string{a, b, c})
	require.NoError(err)

	// The scheduler asks the sync to yield while it fetches the first feed.
	ctx, preempt := jobctx.WithPreemption(t.Context())
	server.mu.Lock()
	server.onRequest = func(string) { preempt() }
	server.mu.Unlock()
	summary, err := syncer.Sync(ctx, SyncOptions{})
	require.NoError(err)
	require.Equal([]string{a}, feedURLs(summary), "the sync yields after the first feed")

	server.mu.Lock()
	server.onRequest = nil
	server.mu.Unlock()
	summary, err = syncer.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	require.Equal([]string{b, c, a}, feedURLs(summary), "the next sync starts with the feeds the last one didn't reach")
}

func feedURLs(summary Summary) []string {
	urls := make([]string, 0, len(summary.Feeds))
	for _, feed := range summary.Feeds {
		urls = append(urls, feed.URL)
	}
	return urls
}

func TestPlainTextBodiesKeepTheirAngleBrackets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{
		"/text.json": `{"version": "https://jsonfeed.org/version/1.1", "title": "Text JSON",
			"items": [{"id": "j-text", "title": "JSON text", "content_text": "Use <widget> here."}]}`,
		"/latin1.atom": "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n" +
			`<feed xmlns="http://www.w3.org/2005/Atom"><title>Latin-1 Atom</title><id>urn:example:latin1</id>` +
			`<updated>2026-11-07T15:00:00Z</updated><entry><title>Latin-1 summary</title><id>urn:example:latin1:1</id>` +
			"<updated>2026-11-07T15:00:00Z</updated><summary>Caf\xe9 &lt;gizmo&gt; here.</summary></entry></feed>",
		"/text.atom": `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Text Atom</title>
  <id>urn:example:text</id>
  <updated>2026-11-07T15:00:00Z</updated>
  <entry>
    <title>Atom text</title>
    <id>urn:example:text:1</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <content>Use &lt;gadget&gt; here.</content>
  </entry>
  <entry>
    <title>Atom summary</title>
    <id>urn:example:text:2</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <summary>Use &lt;gizmo&gt; here.</summary>
  </entry>
  <entry>
    <title>Atom HTML summary</title>
    <id>urn:example:text:3</id>
    <updated>2026-11-07T15:00:00Z</updated>
    <summary type="html">&lt;b&gt;Bold&lt;/b&gt; news</summary>
  </entry>
</feed>`,
	})
	_, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{server.url("/text.json"), server.url("/text.atom"), server.url("/latin1.atom")})
	require.NoError(err)

	items := archivedItems(t, st)
	assert.Equal("Caf\u00e9 <gizmo> here.", items["urn:example:latin1:1"].Body, "a text summary in a non-UTF-8 feed is text")
	assert.Equal("Use <widget> here.", items["j-text"].Body, "JSON content_text is plain text")
	assert.False(items["j-text"].BodyHTML.Valid)
	assert.Equal("Use <gadget> here.", items["urn:example:text:1"].Body, "Atom content without a type is text")
	assert.False(items["urn:example:text:1"].BodyHTML.Valid)
	assert.Equal("Use <gizmo> here.", items["urn:example:text:2"].Body, "an Atom summary without a type is text")
	assert.False(items["urn:example:text:2"].BodyHTML.Valid)
	assert.Contains(items["urn:example:text:3"].BodyHTML.String, "<b>Bold</b>", "an HTML summary stays HTML")
}

func TestFeedsCarryingNULStillArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/nul.json": `{"version": "https://jsonfeed.org/version/1.1",
		"title": "NUL\u0000 feed",
		"items": [
			{"id": "bad\u0000id", "title": "Bad\u0000 title", "content_text": "a\u0000b"},
			{"id": "good", "title": "Good", "content_text": "fine"}
		]}`})
	summary, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{server.url("/nul.json")})
	require.NoError(err)
	require.NoError(summary.Feeds[0].Err)
	assert.Equal(2, summary.Added())
	items := archivedItems(t, st)
	assert.Equal("Bad title", items["badid"].Subject)
	assert.Equal("NUL feed", items["good"].FeedTitle)
}

func TestSyncInWhichEveryFeedFailsIsAFailedRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/a.atom": atomFeed, "/b.rss": rssFeed})
	syncer := newTestSyncer(st, time.Now())
	summary, err := syncer.Follow(t.Context(), []string{server.url("/a.atom"), server.url("/b.rss")})
	require.NoError(err)
	lastGood, err := st.GetLastSuccessfulSyncContext(t.Context(), summary.SourceID)
	require.NoError(err)

	server.set("/a.atom", "not xml")
	server.set("/b.rss", "not xml")
	_, err = syncer.Sync(t.Context(), SyncOptions{Full: true})
	require.ErrorIs(err, ErrEveryFeedFailed)
	latest, err := st.GetLatestSync(summary.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	stillGood, err := st.GetLastSuccessfulSyncContext(t.Context(), summary.SourceID)
	require.NoError(err)
	assert.Equal(lastGood.ID, stillGood.ID, "the last good sync doesn't advance during an outage")
}

func TestSyncThatYieldsMidFetchBlamesNoFeed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/a.atom": atomFeed})
	syncer := newTestSyncer(st, time.Now())
	_, err := syncer.Follow(t.Context(), []string{server.url("/a.atom")})
	require.NoError(err)

	// The scheduler hands the job's slot to waiting work while it fetches.
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	server.mu.Lock()
	server.onRequest = func(string) { cancel(jobctx.ErrYieldedToWaiter) }
	server.mu.Unlock()
	summary, err := syncer.Sync(ctx, SyncOptions{Full: true})
	require.ErrorIs(err, context.Canceled)
	require.NotErrorIs(err, ErrEveryFeedFailed, "an interruption is no outage")
	assert.Empty(summary.Feeds, "the interrupted feed wasn't checked")

	latest, err := st.GetLatestSync(summary.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCancelled, latest.Status)
	list, err := NewRegistry(st).List(t.Context())
	require.NoError(err)
	assert.Empty(list[0].State.LastError, "the feed keeps no error for an interruption")
}

func TestItemNameNamesAnItemByWhatAPersonRecognizes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Release 1.2", itemName(messageimport.ImportedMessage{Title: "Release 1.2", URL: "https://x.example/1", ExternalID: "k"}))
	assert.Equal(t, "https://x.example/1", itemName(messageimport.ImportedMessage{URL: "https://x.example/1", ExternalID: "k"}))
	assert.Equal(t, "k", itemName(messageimport.ImportedMessage{ExternalID: "k"}))
}

func TestFeedsWithTheSameItemIDsKeepTheirOwnItems(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/a.atom": atomFeed, "/b.atom": atomFeed})
	summary, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{server.url("/a.atom"), server.url("/b.atom")})
	require.NoError(err)
	assert.Equal(4, summary.Added(), "each feed archives its own copy of item IDs the other also uses")

	perFeed := map[string]int{}
	for _, item := range archivedItemList(t, st) {
		perFeed[item.Feed]++
	}
	assert.Equal(map[string]int{server.url("/a.atom"): 2, server.url("/b.atom"): 2}, perFeed)
}

func TestFollowArchivesAFeedWhoseURLIsLongerThanAConversationID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/releases.atom": atomFeed})
	feedURL := server.url("/releases.atom?q=" + strings.Repeat("a", 300))
	syncer := newTestSyncer(st, time.Now())
	summary, err := syncer.Follow(t.Context(), []string{feedURL})
	require.NoError(err)
	require.NoError(summary.Feeds[0].Err)
	assert.Equal(2, summary.Added())
	for _, item := range archivedItemList(t, st) {
		assert.Equal(conversationKey(feedURL), item.Feed)
	}

	list, err := NewRegistry(st).List(t.Context())
	require.NoError(err)
	require.Len(list, 1)
	assert.Equal(feedURL, list[0].URL, "the feed keeps its full URL")
	summary, err = syncer.Sync(t.Context(), SyncOptions{URLs: []string{feedURL}})
	require.NoError(err)
	assert.True(summary.Feeds[0].NotModified, "syncs find the feed by its full URL")
}

func TestRegistryRemovesAndResumesFeeds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	server := newFeedServer(t, map[string]string{"/releases.atom": atomFeed})
	feedURL := server.url("/releases.atom")
	syncer := newTestSyncer(st, time.Now())
	_, err := syncer.Follow(t.Context(), []string{feedURL})
	require.NoError(err)
	registry := NewRegistry(st)

	_, err = registry.Remove(t.Context(), feedURL)
	require.NoError(err)
	summary, err := syncer.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Empty(summary.Feeds, "a removed feed is not fetched")
	assert.Len(archivedItems(t, st), 2, "a removed feed keeps its items")
	_, err = syncer.Sync(t.Context(), SyncOptions{URLs: []string{feedURL}})
	require.ErrorIs(err, ErrNotFollowed)
	_, err = registry.Remove(t.Context(), feedURL)
	require.ErrorIs(err, ErrNotFollowed)

	_, added, err := registry.Add(t.Context(), feedURL, "")
	require.NoError(err)
	assert.True(added, "adding a removed feed follows it again")
	list, err := registry.List(t.Context())
	require.NoError(err)
	require.Len(list, 1)
	assert.True(list[0].Followed())
	assert.Equal("Example Project releases", list[0].State.Title, "resuming keeps what the feed recorded")
}

func TestNormalizeURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: " HTTPS://Example.COM/Feed.xml ", want: "https://example.com/Feed.xml"},
		{raw: "http://example.com/feed?format=rss", want: "http://example.com/feed?format=rss"},
		{raw: "example.com/feed", wantErr: true},
		{raw: "ftp://example.com/feed", wantErr: true},
		{raw: "", wantErr: true},
	} {
		got, err := NormalizeURL(tc.raw)
		if tc.wantErr {
			require.ErrorIs(t, err, ErrInvalidURL, tc.raw)
			continue
		}
		require.NoError(t, err, tc.raw)
		assert.Equal(t, tc.want, got)
	}
}

func TestParseOPML(t *testing.T) {
	t.Parallel()
	urls, err := ParseOPML(strings.NewReader(`<?xml version="1.0"?>
<opml version="2.0">
  <head><title>Subscriptions</title></head>
  <body>
    <outline text="Tech">
      <outline type="rss" text="A" xmlUrl="https://a.example.com/feed"/>
      <outline type="rss" text="B" xmlUrl="https://b.example.com/atom.xml"/>
    </outline>
    <outline type="rss" text="A again" xmlUrl="https://a.example.com/feed"/>
    <outline type="rss" text="C" xmlUrl="https://c.example.com/feed.json"/>
  </body>
</opml>`))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"https://a.example.com/feed", "https://b.example.com/atom.xml", "https://c.example.com/feed.json",
	}, urls)

	_, err = ParseOPML(strings.NewReader(`<opml><body><outline text="empty"/></body></opml>`))
	require.ErrorIs(t, err, ErrNoFeedsInOPML)
}
