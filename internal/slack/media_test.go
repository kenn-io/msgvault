package slack

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFetchableURLHostRestriction(t *testing.T) {
	tests := []struct {
		name string
		url  string
		ok   bool
	}{
		{"real host", "https://files.slack.com/files-pri/T01-F01/a.png", true},
		{"case-insensitive host", "https://FILES.SLACK.COM/files-pri/T01-F01/a.png", true},
		{"attacker host", "https://attacker.example/files-pri/T01-F01/a.png", false},
		{"subdomain spoof", "https://files.slack.com.attacker.example/a.png", false},
		{"http downgrade", "http://files.slack.com/a.png", false},
		{"userinfo trick", "https://files.slack.com@attacker.example/a.png", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fetchableURL(tt.url)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

// recordingTransport serves canned bodies for files.slack.com and records
// every request it sees (it must never see an off-host one).
type recordingTransport struct {
	requests []string
	body     string
	// bodyByPath overrides body per URL path (distinct content hashes:
	// the store dedupes same-hash rows per message).
	bodyByPath map[string]string
	redirect   string // when set, answer 302 to this location
	status     int
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.requests = append(rt.requests, req.URL.String())
	if rt.redirect != "" {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {rt.redirect}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	}
	status := rt.status
	if status == 0 {
		status = http.StatusOK
	}
	body := rt.body
	if b, ok := rt.bodyByPath[req.URL.Path]; ok {
		body = b
	}
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func TestDownloadFileRefusesOffHostAndRedirects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rt := &recordingTransport{body: "bytes"}
	c := NewClient("", "xoxp-test")
	c.mediaTransport = rt

	// Off-host: rejected before any request is made.
	_, err := c.DownloadFile(context.Background(), "https://attacker.example/x", 100)
	require.ErrorIs(err, errOffHost)
	assert.Empty(rt.requests, "the token must never travel to an off-host URL")

	// On-host succeeds.
	data, err := c.DownloadFile(context.Background(), "https://files.slack.com/files-pri/T01-F01/a.png", 100)
	require.NoError(err)
	assert.Equal("bytes", string(data))
	require.Len(rt.requests, 1)

	// A redirect — even one a compromised response injects — is refused.
	rt.redirect = "https://attacker.example/steal"
	_, err = c.DownloadFile(context.Background(), "https://files.slack.com/files-pri/T01-F02/b.png", 100)
	require.ErrorIs(err, errOffHost)
	assert.Len(rt.requests, 2, "the redirect target must not be followed")
}

func TestDownloadFileSizeCap(t *testing.T) {
	rt := &recordingTransport{body: strings.Repeat("x", 64)}
	c := NewClient("", "xoxp-test")
	c.mediaTransport = rt
	_, err := c.DownloadFile(context.Background(), "https://files.slack.com/files-pri/T01-F01/a.png", 10)
	assert.ErrorIs(t, err, ErrAssetTooLarge)
}

func TestPersistFilesLinkRowsAndPendingMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	general := f.conv("C01")
	general.Msgs[6].Files = []map[string]any{
		{"id": "F_OK", "name": "a.png", "mimetype": "image/png", "size": 5,
			"url_private": "https://files.slack.com/files-pri/T01-F_OK/a.png",
			"permalink":   "https://testers.slack.com/files/F_OK"},
		{"id": "F_EXT", "name": "doc.pdf", "mimetype": "application/pdf", "is_external": true,
			"url_private": "https://ext.example/doc.pdf",
			"permalink":   "https://testers.slack.com/files/F_EXT"},
		{"id": "F_BIG", "name": "video.mp4", "mimetype": "video/mp4", "size": 1 << 40,
			"url_private": "https://files.slack.com/files-pri/T01-F_BIG/video.mp4",
			"permalink":   "https://testers.slack.com/files/F_BIG"},
	}

	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })
	srv := f.serve()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	client.mediaTransport = &recordingTransport{body: "png01"}
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")

	opts := ImportOptions{
		TeamID: "T01", UserID: "UME",
		AttachmentsDir: t.TempDir(), MaxMediaBytes: 1 << 20,
	}
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	rows, err := st.DB().Query(st.Rebind(`
		SELECT a.source_attachment_id, COALESCE(a.content_hash, ''), COALESCE(a.media_type, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(6))
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[string][2]string{}
	for rows.Next() {
		var id, hash, mediaType string
		require.NoError(rows.Scan(&id, &hash, &mediaType))
		got[id] = [2]string{hash, mediaType}
	}
	require.NoError(rows.Err())

	require.Len(got, 3)
	assert.NotEmpty(got["slack:F_OK"][0], "on-host file downloads into content-addressed storage")
	assert.Equal("image", got["slack:F_OK"][1])
	assert.Empty(got["slack:F_EXT"][0])
	assert.Equal("link", got["slack:F_EXT"][1], "external file records metadata only")
	assert.Empty(got["slack:F_BIG"][0])
	assert.Empty(got["slack:F_BIG"][1], "over-cap file leaves a retryable pending marker")

	// The pending list sees the over-cap marker but not the link row.
	src, err := st.GetOrCreateSource("slack", "T01:UME")
	require.NoError(err)
	pending, err := st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(err)
	require.Len(pending, 1)

	// Raising the cap and backfilling repairs the pending download (the
	// declared size in the archived raw JSON no longer exceeds it).
	opts.MaxMediaBytes = 1 << 50
	sum, err := imp.BackfillMedia(context.Background(), opts)
	require.NoError(err)
	assert.Equal(1, sum.AttachmentsDownloaded)
	pending, err = st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(err)
	assert.Empty(pending)
}

func TestPersistFilesPreservesTombstonedAndOmittedDownloads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.conv("C01").Msgs[6].Files = []map[string]any{
		{"id": "F_TOMB", "name": "t.png", "mimetype": "image/png", "size": 5,
			"url_private": "https://files.slack.com/files-pri/T01-F_TOMB/t.png",
			"permalink":   "https://testers.slack.com/files/F_TOMB"},
		{"id": "F_GONE", "name": "g.png", "mimetype": "image/png", "size": 5,
			"url_private": "https://files.slack.com/files-pri/T01-F_GONE/g.png",
			"permalink":   "https://testers.slack.com/files/F_GONE"},
		{"id": "F_PEND", "name": "big.mp4", "mimetype": "video/mp4", "size": 1 << 40,
			"url_private": "https://files.slack.com/files-pri/T01-F_PEND/big.mp4",
			"permalink":   "https://testers.slack.com/files/F_PEND"},
		{"id": "F_LINK", "name": "doc.pdf", "mimetype": "application/pdf", "is_external": true,
			"url_private": "https://ext.example/doc.pdf",
			"permalink":   "https://testers.slack.com/files/F_LINK"},
	}

	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })
	srv := f.serve()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	client.mediaTransport = &recordingTransport{body: "png03", bodyByPath: map[string]string{
		"/files-pri/T01-F_GONE/g.png": "png04",
	}}
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")

	opts := ImportOptions{
		TeamID: "T01", UserID: "UME",
		AttachmentsDir: t.TempDir(), MaxMediaBytes: 1 << 20,
	}
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// The source deletes one downloaded file (tombstone) and an edit drops
	// the others from the message entirely; the oversized pending marker
	// and the external link's metadata row also stop being listed.
	// Deletions at the source must never reach into the archive — the
	// downloaded rows AND the metadata-only link row survive; only the
	// stale pending marker has nothing to keep.
	f.mu.Lock()
	f.conv("C01").Msgs[6].Files = []map[string]any{
		{"id": "F_TOMB", "mode": "tombstone"},
	}
	f.mu.Unlock()

	full := opts
	full.Full = true
	_, err = imp.Import(context.Background(), full)
	require.NoError(err)

	rows, err := st.DB().Query(st.Rebind(`
		SELECT a.source_attachment_id, COALESCE(a.content_hash, ''), COALESCE(a.media_type, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(6))
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[string][2]string{}
	for rows.Next() {
		var id, hash, mediaType string
		require.NoError(rows.Scan(&id, &hash, &mediaType))
		got[id] = [2]string{hash, mediaType}
	}
	require.NoError(rows.Err())

	assert.NotEmpty(got["slack:F_TOMB"][0], "a tombstoned file keeps its archived attachment row")
	assert.NotEmpty(got["slack:F_GONE"][0], "a file dropped by an edit keeps its archived attachment row")
	assert.Equal("link", got["slack:F_LINK"][1], "an omitted external file keeps its metadata-only link row")
	_, pendKept := got["slack:F_PEND"]
	assert.False(pendKept, "a stale pending marker clears once the source stops listing the file")
}

func TestPersistFilesKeepsAliasRowsForDuplicateContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	// Two distinct Slack files whose bytes are identical (the transport
	// serves one body for every path): the schema's (message_id,
	// content_hash) uniqueness must not silently drop the second file ID.
	f.conv("C01").Msgs[6].Files = []map[string]any{
		{"id": "F_DUP1", "name": "a.png", "mimetype": "image/png", "size": 5,
			"url_private": "https://files.slack.com/files-pri/T01-F_DUP1/a.png",
			"permalink":   "https://testers.slack.com/files/F_DUP1"},
		{"id": "F_DUP2", "name": "copy-of-a.png", "mimetype": "image/png", "size": 5,
			"url_private": "https://files.slack.com/files-pri/T01-F_DUP2/copy-of-a.png",
			"permalink":   "https://testers.slack.com/files/F_DUP2"},
	}

	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })
	srv := f.serve()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	rt := &recordingTransport{body: "png05"}
	client.mediaTransport = rt
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")

	opts := ImportOptions{
		TeamID: "T01", UserID: "UME",
		AttachmentsDir: t.TempDir(), MaxMediaBytes: 1 << 20,
	}
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// Both file IDs keep a row: one carries the hash, the duplicate is an
	// alias (same CAS path, hash re-derived on read).
	src, err := st.GetOrCreateSource("slack", "T01:UME")
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "C01:"+ts(6)).Scan(&messageID))
	refs, err := st.MessageSlackAttachments(messageID)
	require.NoError(err)
	require.Len(refs, 2, "duplicate-content file IDs must both keep attachment rows")
	assert.NotEmpty(refs["slack:F_DUP1"].ContentHash)
	assert.NotEmpty(refs["slack:F_DUP2"].ContentHash, "the alias row must read back as downloaded")
	assert.Equal(refs["slack:F_DUP1"].StoragePath, refs["slack:F_DUP2"].StoragePath)

	// Aliases are downloaded, not pending — and repairs must not re-fetch.
	pending, err := st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(err)
	assert.Empty(pending, "alias rows are downloaded, never pending work")

	full := opts
	full.Full = true
	_, err = imp.Import(context.Background(), full)
	require.NoError(err)
	assert.Len(rt.requests, 2, "a repair pass must not re-download duplicate-content files")
	refs, err = st.MessageSlackAttachments(messageID)
	require.NoError(err)
	assert.Len(refs, 2)
}

func TestNoMediaDefersFilesAsPendingNotLinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.conv("C01").Msgs[6].Files = []map[string]any{
		{"id": "F_DEFER", "name": "b.png", "mimetype": "image/png", "size": 5,
			"url_private": "https://files.slack.com/files-pri/T01-F_DEFER/b.png",
			"permalink":   "https://testers.slack.com/files/F_DEFER"},
	}

	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })
	srv := f.serve()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	client.mediaTransport = &recordingTransport{body: "png02"}
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")

	// Sync with media disabled: the hosted file must become a PENDING
	// marker — a link row would hide it from backfill forever.
	opts := ImportOptions{TeamID: "T01", UserID: "UME", NoMedia: true, AttachmentsDir: t.TempDir()}
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.Equal(1, sum.AttachmentsPending)

	src, err := st.GetOrCreateSource("slack", "T01:UME")
	require.NoError(err)
	pending, err := st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(err)
	require.Len(pending, 1, "a --no-media deferred hosted file must stay discoverable")

	// Enabling media and backfilling downloads it.
	opts.NoMedia = false
	bsum, err := imp.BackfillMedia(context.Background(), opts)
	require.NoError(err)
	assert.Equal(1, bsum.AttachmentsDownloaded)
	var hash string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(a.content_hash,'') FROM attachments a
		WHERE a.source_attachment_id = ?`), "slack:F_DEFER").Scan(&hash))
	assert.NotEmpty(hash)
}
