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
	redirect string // when set, answer 302 to this location
	status   int
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
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(strings.NewReader(rt.body)),
		ContentLength: int64(len(rt.body)),
		Request:       req,
	}, nil
}

func TestDownloadFileRefusesOffHostAndRedirects(t *testing.T) {
	rt := &recordingTransport{body: "bytes"}
	c := NewClient("", "xoxp-test")
	c.mediaTransport = rt

	// Off-host: rejected before any request is made.
	_, err := c.DownloadFile(context.Background(), "https://attacker.example/x", 100)
	require.ErrorIs(t, err, errOffHost)
	assert.Empty(t, rt.requests, "the token must never travel to an off-host URL")

	// On-host succeeds.
	data, err := c.DownloadFile(context.Background(), "https://files.slack.com/files-pri/T01-F01/a.png", 100)
	require.NoError(t, err)
	assert.Equal(t, "bytes", string(data))
	require.Len(t, rt.requests, 1)

	// A redirect — even one a compromised response injects — is refused.
	rt.redirect = "https://attacker.example/steal"
	_, err = c.DownloadFile(context.Background(), "https://files.slack.com/files-pri/T01-F02/b.png", 100)
	require.ErrorIs(t, err, errOffHost)
	assert.Len(t, rt.requests, 2, "the redirect target must not be followed")
}

func TestDownloadFileSizeCap(t *testing.T) {
	rt := &recordingTransport{body: strings.Repeat("x", 64)}
	c := NewClient("", "xoxp-test")
	c.mediaTransport = rt
	_, err := c.DownloadFile(context.Background(), "https://files.slack.com/files-pri/T01-F01/a.png", 10)
	assert.ErrorIs(t, err, ErrAssetTooLarge)
}

func TestPersistFilesLinkRowsAndPendingMarkers(t *testing.T) {
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
	require.NoError(t, err)

	rows, err := st.DB().Query(st.Rebind(`
		SELECT a.source_attachment_id, COALESCE(a.content_hash, ''), COALESCE(a.media_type, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "C01:"+ts(6))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string][2]string{}
	for rows.Next() {
		var id, hash, mediaType string
		require.NoError(t, rows.Scan(&id, &hash, &mediaType))
		got[id] = [2]string{hash, mediaType}
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 3)
	assert.NotEmpty(t, got["slack:F_OK"][0], "on-host file downloads into content-addressed storage")
	assert.Equal(t, "image", got["slack:F_OK"][1])
	assert.Empty(t, got["slack:F_EXT"][0])
	assert.Equal(t, "link", got["slack:F_EXT"][1], "external file records metadata only")
	assert.Empty(t, got["slack:F_BIG"][0])
	assert.Empty(t, got["slack:F_BIG"][1], "over-cap file leaves a retryable pending marker")

	// The pending list sees the over-cap marker but not the link row.
	src, err := st.GetOrCreateSource("slack", "T01:UME")
	require.NoError(t, err)
	pending, err := st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// Raising the cap and backfilling repairs the pending download (the
	// declared size in the archived raw JSON no longer exceeds it).
	opts.MaxMediaBytes = 1 << 50
	sum, err := imp.BackfillMedia(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.AttachmentsDownloaded)
	pending, err = st.ListSlackPendingAttachmentMessages(src.ID)
	require.NoError(t, err)
	assert.Empty(t, pending)
}
