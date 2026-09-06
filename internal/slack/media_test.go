package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
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

func TestPersistFilesRecordsStreamedOversizeForRetryPolicy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.conv("C01").Msgs[6].Files = []map[string]any{{
		"id": "F_STREAM_BIG", "name": "large.bin", "mimetype": "application/octet-stream",
		"url_private": "https://files.slack.com/files-pri/T01-F_STREAM_BIG/large.bin",
		"permalink":   "https://testers.slack.com/files/F_STREAM_BIG",
	}}
	srv := f.serve()
	defer srv.Close()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	transport := &recordingTransport{body: "12345678901"}
	client.mediaTransport = transport
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")
	opts := ImportOptions{
		TeamID: "T01", UserID: "UME", AttachmentsDir: t.TempDir(), MaxMediaBytes: 10,
		MediaPolicy: attachmentpolicy.Policy{MaxBytes: 10},
	}

	sum, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, sum.AttachmentsSkipped)
	var size int64
	require.NoError(st.DB().QueryRow(`
		SELECT size FROM attachments WHERE source_attachment_id = 'slack:F_STREAM_BIG'
	`).Scan(&size))
	assert.Greater(size, int64(10))
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	require.NoError(err)
	assert.Zero(backfill.MessagesProcessed)
	assert.Len(transport.requests, 1, "unchanged cap must not retry streamed oversize media")
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
		SELECT a.source_attachment_id, COALESCE(a.content_hash, ''), COALESCE(a.media_type, ''),
		       a.attachment_role, a.role_source
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(6))
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	got := map[string][4]string{}
	for rows.Next() {
		var id, hash, mediaType, role, roleSource string
		require.NoError(rows.Scan(&id, &hash, &mediaType, &role, &roleSource))
		got[id] = [4]string{hash, mediaType, role, roleSource}
	}
	require.NoError(rows.Err())

	require.Len(got, 3)
	assert.NotEmpty(got["slack:F_OK"][0], "on-host file downloads into content-addressed storage")
	assert.Equal("image", got["slack:F_OK"][1])
	assert.Equal("standalone", got["slack:F_OK"][2])
	assert.Equal("provider_explicit", got["slack:F_OK"][3])
	assert.Empty(got["slack:F_EXT"][0])
	assert.Equal("link", got["slack:F_EXT"][1], "external file records metadata only")
	assert.Equal("unknown", got["slack:F_EXT"][2])
	assert.Empty(got["slack:F_BIG"][0])
	assert.Empty(got["slack:F_BIG"][1], "over-cap file leaves a policy marker")
	assert.Equal("unknown", got["slack:F_BIG"][2])

	// A policy skip is not ordinary retry debt while the cap is unchanged.
	src, err := st.GetOrCreateSource("slack", "T01:UME")
	require.NoError(err)
	pending, err := st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(err)
	require.Empty(pending)

	// Raising the cap and backfilling re-evaluates and repairs the download (the
	// declared size in the archived raw JSON no longer exceeds it).
	opts.MaxMediaBytes = 1 << 50
	sum, err := imp.BackfillMedia(context.Background(), opts)
	require.NoError(err)
	assert.Equal(1, sum.AttachmentsDownloaded)
	pending, err = st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(err)
	assert.Empty(pending)
}

func TestPersistFilesTreatsGoneDownloadsAsTerminalLinks(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := testWorkspace(t)
			f.conv("C01").Msgs[6].Files = []map[string]any{
				{"id": "F_GONE", "name": "deleted.png", "mimetype": "image/png", "size": 123,
					"url_private": "https://files.slack.com/files-pri/T01-F_GONE/deleted.png",
					"permalink":   "https://testers.slack.com/files/F_GONE"},
			}

			prevInterval := checkpointMinInterval
			checkpointMinInterval = 0
			t.Cleanup(func() { checkpointMinInterval = prevInterval })
			srv := f.serve()
			client := NewClient(srv.URL, "xoxp-test")
			client.disableRateLimits()
			client.mediaTransport = &recordingTransport{status: status}
			st := testutil.NewTestStore(t)
			imp := NewImporter(st, client, "T01")

			opts := ImportOptions{
				TeamID: "T01", UserID: "UME",
				NoMedia: true, AttachmentsDir: t.TempDir(), MaxMediaBytes: 1 << 20,
			}
			sum, err := imp.Import(context.Background(), opts)
			require.NoError(err)
			require.Equal(1, sum.AttachmentsPending)

			src, err := st.GetOrCreateSource("slack", "T01:UME")
			require.NoError(err)
			pending, err := st.ListSlackPendingAttachmentMessages(src.ID)
			require.NoError(err)
			require.Len(pending, 1, "media-disabled import must create genuine pending debt")

			opts.NoMedia = false
			sum, err = imp.BackfillMedia(context.Background(), opts)
			require.NoError(err)
			assert.Zero(sum.AttachmentsPending)
			assert.Zero(sum.Errors)

			pending, err = st.ListSlackPendingAttachmentMessages(src.ID)
			require.NoError(err)
			assert.Empty(pending, "a deleted Slack file must not remain pending")

			var messageID int64
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT id FROM messages WHERE source_message_id = ?`), "C01:"+ts(6)).Scan(&messageID))
			refs, err := st.MessageSlackAttachments(messageID)
			require.NoError(err)
			require.Contains(refs, "slack:F_GONE")
			assert.Equal(store.AttachmentRef{
				Filename:           "deleted.png",
				MimeType:           "image/png",
				StoragePath:        "https://testers.slack.com/files/F_GONE",
				Size:               123,
				SourceAttachmentID: "slack:F_GONE",
				MediaType:          "link",
				Role:               store.AttachmentRoleUnknown,
				RoleSource:         store.AttachmentRoleSourceUnknown,
				SourcePartKey:      "slack:F_GONE",
			}, refs["slack:F_GONE"])
		})
	}
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
	// the others from the message entirely. Deletions at the source must
	// never reach into the archive: downloaded rows survive, and a pending
	// row becomes terminal metadata so the file remains discoverable without
	// wedging the retry queue forever.
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
	assert.Equal("link", got["slack:F_PEND"][1],
		"a pending file omitted at the source must become terminal metadata rather than disappear")
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

func TestMediaPolicySkipsChannelWithoutDownload(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.conv("C01").Msgs[6].Files = []map[string]any{{
		"id": "F_POLICY", "name": "policy.png", "mimetype": "image/png", "size": 5,
		"url_private": "https://files.slack.com/files-pri/T01-F_POLICY/policy.png",
		"permalink":   "https://testers.slack.com/files/F_POLICY",
	}}
	srv := f.serve()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	transport := &recordingTransport{body: "must not download"}
	client.mediaTransport = transport
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")

	sum, err := imp.Import(context.Background(), ImportOptions{
		TeamID: "T01", UserID: "UME", AttachmentsDir: t.TempDir(),
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect, MaxBytes: 100 << 20},
	})
	require.NoError(err)
	assert.Equal(1, sum.AttachmentsSkipped)
	assert.Empty(transport.requests)

	var state, reason string
	require.NoError(st.DB().QueryRow(`
		SELECT attachment_state, attachment_skip_reason FROM attachments
		WHERE source_attachment_id = 'slack:F_POLICY'
	`).Scan(&state, &reason))
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipPolicyScope), reason)
}

// TestBackfillMediaOverridesProviderMediaToggle verifies that an explicit
// backfill downloads media the provider-level [slack].media = false toggle
// deferred, while an account-level opt-out still holds. The toggle's
// documented workflow is "sync without media, backfill later", so honoring it
// inside the backfill would make backfill-slack-media a no-op for exactly the
// configuration that depends on it.
func TestBackfillMediaOverridesProviderMediaToggle(t *testing.T) {
	for _, tc := range []struct {
		name          string
		disabled      attachmentpolicy.SkipReason
		wantDownloads int
		wantReason    string
	}{
		{
			name:     "provider toggle defers to the explicit backfill",
			disabled: attachmentpolicy.SkipPolicyScope, wantDownloads: 1,
		},
		{
			name:       "account opt-out still declines",
			disabled:   attachmentpolicy.SkipAccountPolicy,
			wantReason: string(attachmentpolicy.SkipAccountPolicy),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := testWorkspace(t)
			f.conv("C01").Msgs[6].Files = []map[string]any{{
				"id": "F_TOGGLE", "name": "toggle.png", "mimetype": "image/png", "size": 5,
				"url_private": "https://files.slack.com/files-pri/T01-F_TOGGLE/toggle.png",
				"permalink":   "https://testers.slack.com/files/F_TOGGLE",
			}}

			prevInterval := checkpointMinInterval
			checkpointMinInterval = 0
			t.Cleanup(func() { checkpointMinInterval = prevInterval })
			srv := f.serve()
			defer srv.Close()
			client := NewClient(srv.URL, "xoxp-test")
			client.disableRateLimits()
			transport := &recordingTransport{body: "png06"}
			client.mediaTransport = transport
			st := testutil.NewTestStore(t)
			imp := NewImporter(st, client, "T01")

			opts := ImportOptions{
				TeamID: "T01", UserID: "UME", AttachmentsDir: t.TempDir(),
				MediaPolicy: attachmentpolicy.Policy{
					Scope: attachmentpolicy.ScopeAll, MaxBytes: 1 << 20, DisabledReason: tc.disabled,
				},
			}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			require.Equal(1, sum.AttachmentsSkipped)
			require.Empty(transport.requests, "a disabled media policy must not download during sync")

			backfill, err := imp.BackfillMedia(t.Context(), opts)
			require.NoError(err)
			assert.Equal(tc.wantDownloads, backfill.AttachmentsDownloaded)
			assert.Len(transport.requests, tc.wantDownloads)

			var contentHash, reason string
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COALESCE(content_hash, ''), COALESCE(attachment_skip_reason, '')
				FROM attachments WHERE source_attachment_id = ?
			`), "slack:F_TOGGLE").Scan(&contentHash, &reason))
			assert.Equal(tc.wantReason, reason)
			if tc.wantDownloads > 0 {
				assert.NotEmpty(contentHash, "the backfill must archive the deferred file")
			} else {
				assert.Empty(contentHash)
			}
		})
	}
}

func TestMediaTimeoutScalesWithCap(t *testing.T) {
	assert := assert.New(t)
	// The API client's 60s whole-request deadline starves large downloads
	// on slow links; the media bound must scale with the size cap (~128
	// KiB/s floor rate) above a generous minimum, and never be infinite.
	assert.Equal(10*time.Minute, mediaTimeout(1<<20), "small caps get the floor")
	assert.Equal(2000*time.Second, mediaTimeout(defaultMaxMediaBytes), "default 250 MiB cap ≈ 33m20s")
	assert.Equal(8192*time.Second, mediaTimeout(1<<30), "bigger caps scale up")
	assert.Greater(mediaTimeout(1), time.Minute, "never anywhere near the 60s API deadline")
}

// membershipRecord returns the roster the fixture channel's provider metadata
// records: the exact member count, and whether the roster is marked unreadable.
func membershipRecord(t *testing.T, st *store.Store) (count int, unknown bool) {
	t.Helper()

	var metadata string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COALESCE(CAST(metadata AS TEXT), '') FROM conversations
		WHERE source_conversation_id = ?
	`), "C01").Scan(&metadata))
	if metadata == "" {
		return 0, false
	}
	var record struct {
		MemberCount        int  `json:"member_count"`
		MemberCountUnknown bool `json:"member_count_unknown"`
	}
	require.NoError(t, json.Unmarshal([]byte(metadata), &record))
	return record.MemberCount, record.MemberCountUnknown
}

// TestMediaPolicyFailsClosedOnLaterMembershipOutage covers a members listing
// that succeeded once and then fails. The count read earlier is kept for
// reference, but the roster is unresolved: a backfill must not trust the stale
// count to release a deferred download until a sync reads the roster again.
func TestMediaPolicyFailsClosedOnLaterMembershipOutage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.conv("C01").Msgs[6].Files = []map[string]any{{
		"id": "F_LATER", "name": "later.png", "mimetype": "image/png", "size": 5,
		"url_private": "https://files.slack.com/files-pri/T01-F_LATER/later.png",
		"permalink":   "https://testers.slack.com/files/F_LATER",
	}}

	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })
	srv := f.serve()
	defer srv.Close()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	transport := &recordingTransport{body: "png07", status: http.StatusInternalServerError}
	client.mediaTransport = transport
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")
	opts := ImportOptions{
		TeamID: "T01", UserID: "UME", AttachmentsDir: t.TempDir(),
		MediaPolicy: attachmentpolicy.Policy{
			Scope: attachmentpolicy.ScopeAll, MaxParticipants: 5, MaxBytes: 1 << 20,
		},
	}
	fileOutcome := func() (state, contentHash string) {
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COALESCE(attachment_state, ''), COALESCE(content_hash, '')
			FROM attachments WHERE source_attachment_id = ?
		`), "slack:F_LATER").Scan(&state, &contentHash))
		return state, contentHash
	}

	// The roster reads fine; only the download itself fails, leaving a
	// retryable marker the backfill will decide on later.
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	count, unknown := membershipRecord(t, st)
	require.Equal(3, count)
	require.False(unknown)
	state, contentHash := fileOutcome()
	require.Equal(string(attachmentpolicy.StateFailed), state)
	require.Empty(contentHash)
	require.NotEmpty(transport.requests)
	transport.requests = nil
	transport.status = 0

	f.failMembers["C01"] = true
	_, err = imp.Import(t.Context(), opts)
	require.Error(err, "a members-listing outage leaves the run partial")
	count, unknown = membershipRecord(t, st)
	assert.Equal(3, count, "the count read earlier stays for reference")
	assert.True(unknown, "a later failed listing must be archived, not hidden behind the earlier count")

	backfill, err := imp.BackfillMedia(t.Context(), opts)
	require.NoError(err)
	assert.Zero(backfill.AttachmentsDownloaded)
	assert.Empty(transport.requests, "a stale count must not release a download while the roster is unresolved")
	state, _ = fileOutcome()
	assert.Equal(string(attachmentpolicy.StateSkipped), state)

	f.failMembers["C01"] = false
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	count, unknown = membershipRecord(t, st)
	assert.Equal(3, count)
	assert.False(unknown)
	_, err = imp.BackfillMedia(t.Context(), opts)
	require.NoError(err)
	state, contentHash = fileOutcome()
	assert.Equal(string(attachmentpolicy.StateStored), state)
	assert.NotEmpty(contentHash, "a readable roster releases the deferred download")
	assert.Len(transport.requests, 1)
}

// TestMediaPolicyFailsClosedOnUnreadableMembership covers a members-listing
// outage under a participant threshold. The count is unknown, not zero, so the
// channel's media must not download as if it were a small conversation — and
// the outage must be archived, because the backfill evaluates the archive
// rather than the run that wrote it.
func TestMediaPolicyFailsClosedOnUnreadableMembership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	f.failMembers["C01"] = true
	f.conv("C01").Msgs[6].Files = []map[string]any{{
		"id": "F_ROSTER", "name": "roster.png", "mimetype": "image/png", "size": 5,
		"url_private": "https://files.slack.com/files-pri/T01-F_ROSTER/roster.png",
		"permalink":   "https://testers.slack.com/files/F_ROSTER",
	}}

	prevInterval := checkpointMinInterval
	checkpointMinInterval = 0
	t.Cleanup(func() { checkpointMinInterval = prevInterval })
	srv := f.serve()
	defer srv.Close()
	client := NewClient(srv.URL, "xoxp-test")
	client.disableRateLimits()
	transport := &recordingTransport{body: "png07"}
	client.mediaTransport = transport
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, client, "T01")
	opts := ImportOptions{
		TeamID: "T01", UserID: "UME", AttachmentsDir: t.TempDir(),
		MediaPolicy: attachmentpolicy.Policy{
			Scope: attachmentpolicy.ScopeAll, MaxParticipants: 5, MaxBytes: 1 << 20,
		},
	}

	_, err := imp.Import(t.Context(), opts)
	require.Error(err, "a members-listing outage leaves the run partial")
	assert.Empty(transport.requests, "unknown membership must not read as a channel under the limit")

	fileOutcome := func() (reason, contentHash string) {
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COALESCE(attachment_skip_reason, ''), COALESCE(content_hash, '')
			FROM attachments WHERE source_attachment_id = ?
		`), "slack:F_ROSTER").Scan(&reason, &contentHash))
		return reason, contentHash
	}
	reason, contentHash := fileOutcome()
	assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
	assert.Empty(contentHash)
	count, unknown := membershipRecord(t, st)
	assert.True(unknown, "the failed members listing must be archived, not left absent")
	assert.Zero(count)

	// The backfill reads the same archive, so it stays closed until a sync can
	// read the roster again.
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	require.NoError(err)
	assert.Zero(backfill.AttachmentsDownloaded)
	assert.Empty(transport.requests)

	f.failMembers["C01"] = false
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	count, unknown = membershipRecord(t, st)
	assert.Equal(3, count)
	assert.False(unknown)

	_, err = imp.BackfillMedia(t.Context(), opts)
	require.NoError(err)
	reason, contentHash = fileOutcome()
	assert.Empty(reason)
	assert.NotEmpty(contentHash, "a readable roster releases the deferred download")
	assert.Len(transport.requests, 1)
}
