package beeper

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mediaChat builds a chat with one image message whose asset may or may not
// be present on the fake server.
func mediaChat() *fakeChat {
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	return &fakeChat{
		ID: "!media:beeper.local", AccountID: "signal", Network: "Signal", Title: "Media", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
		Msgs: []fakeMsg{{
			ID: "p0", SortKey: 0, Timestamp: base, Type: "IMAGE",
			SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
			Attachments: []map[string]any{{
				"id": "mxc://beeper.local/photo1", "type": "img", "mimeType": "image/png",
				"fileName": "photo.png", "fileSize": 11,
				"size": map[string]any{"width": 640, "height": 480},
			}},
		}},
		LastActivity: base,
	}
}

// addFillerMessages appends plain text messages that persist at the source,
// so verification sampling can distinguish single-message churn from a wipe.
func addFillerMessages(ch *fakeChat, n int) {
	last := ch.Msgs[len(ch.Msgs)-1]
	for i := range n {
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: "fill" + strconv.Itoa(i), SortKey: last.SortKey + 1 + i,
			Timestamp: last.Timestamp.Add(time.Duration(i+1) * time.Minute),
			Text:      "filler", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
		})
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
}

func mediaImportOptions(t *testing.T) ImportOptions {
	t.Helper()
	return ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()}
}

func TestImportDownloadsAttachment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(mediaChat())
	f.setAsset("mxc://beeper.local/photo1", []byte("hello bytes"))
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.EqualValues(1, sum.AttachmentsDownloaded)
	assert.EqualValues(0, sum.AttachmentsPending)

	var filename, mimeType, storagePath, contentHash, mediaType string
	var width, height int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.filename, a.mime_type, a.storage_path, a.content_hash, a.media_type, a.width, a.height
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "p0").
		Scan(&filename, &mimeType, &storagePath, &contentHash, &mediaType, &width, &height))
	assert.Equal("photo.png", filename)
	assert.Equal("image/png", mimeType)
	assert.NotEmpty(contentHash)
	assert.Equal("image", mediaType)
	assert.EqualValues(640, width)
	assert.EqualValues(480, height)

	// Bytes are content-addressed on disk.
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.Equal("hello bytes", string(data))

	// Re-running does not duplicate rows — and does not re-download media the
	// message already has (re-persists must reuse the stored attachment).
	f.resetRequests()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: opts.AttachmentsDir, Full: true})
	require.NoError(err)
	var attCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "p0").Scan(&attCount))
	assert.Equal(1, attCount)
	for _, req := range f.requests() {
		assert.NotContains(req, "/v1/assets/serve", "already-stored media must not be re-fetched: %s", req)
	}

	// Metadata folded into the attachment row survives the re-persist.
	var keptMediaType string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.media_type FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "p0").Scan(&keptMediaType))
	assert.Equal("image", keptMediaType)
}

func TestImportMediaFailureLeavesMarkerAndBackfillRepairs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(mediaChat())
	// Asset intentionally missing: download fails, message still archives.
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.EqualValues(0, sum.AttachmentsDownloaded)
	assert.EqualValues(1, sum.AttachmentsPending)

	var msgCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "p0").Scan(&msgCount))
	require.Equal(1, msgCount, "message archives even when its media fails")

	// Marker row: asset URL in storage_path, no hash, prefixed source id.
	var storagePath, sourceAttID string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.storage_path, a.source_attachment_id
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "p0").Scan(&storagePath, &sourceAttID))
	assert.Equal("mxc://beeper.local/photo1", storagePath)
	assert.Equal("beeper:mxc://beeper.local/photo1", sourceAttID)

	// Error recorded per item.
	var itemCount int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_run_items WHERE error_kind = 'beeper_media_error'`).Scan(&itemCount))
	assert.Equal(1, itemCount)

	// The asset becomes available; backfill repairs the marker.
	f.setAsset("mxc://beeper.local/photo1", []byte("late bytes"))
	bsum, err := imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.EqualValues(1, bsum.AttachmentsDownloaded)
	assert.EqualValues(0, bsum.AttachmentsPending)

	var attCount int
	var contentHash string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*), MAX(COALESCE(a.content_hash, ''))
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "p0").Scan(&attCount, &contentHash))
	assert.Equal(1, attCount, "marker replaced, not duplicated")
	assert.NotEmpty(contentHash)

	// Nothing pending anymore: a second backfill visits no messages.
	bsum, err = imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.EqualValues(0, bsum.MessagesProcessed)

	// The media run must carry the sync state forward: a following sync-beeper
	// still sees completed chats and the anchor, not an empty baseline.
	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	require.True(run.CursorAfter.Valid)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.NotEmpty(state.Chats, "backfill-beeper-media must not clobber the sync baseline")
	assert.NotEmpty(state.Anchors)
	f.resetRequests()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: opts.AttachmentsDir})
	require.NoError(err)
	for _, req := range f.requests() {
		assert.NotContains(req, "direction=before", "sync after media backfill must stay incremental: %s", req)
	}
}

func TestImportMediaSizeCap(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	ch := mediaChat()
	ch.Msgs[0].Attachments[0]["id"] = "mxc://beeper.local/huge1"
	ch.Msgs[0].Attachments[0]["fileSize"] = 10 << 20
	f.addChat(ch)
	f.setAsset("mxc://beeper.local/huge1", []byte("would be huge"))
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	opts.MaxMediaBytes = 1 << 20 // 1 MB cap; declared size is 10 MB
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.EqualValues(0, sum.AttachmentsDownloaded)
	assert.EqualValues(1, sum.AttachmentsPending)
	assert.EqualValues(0, sum.Errors, "over-cap is a skip, not an error")

	var itemCount int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_run_items WHERE error_kind = 'beeper_media_too_large'`).Scan(&itemCount))
	assert.Equal(1, itemCount)
}

func TestImportMediaSizeCapUndeclaredSize(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// fileSize is untrusted metadata: with none declared, the cap must still
	// hold at download time (bounded reader), not after buffering the body.
	f := newFakeBeeper(t)
	ch := mediaChat()
	delete(ch.Msgs[0].Attachments[0], "fileSize")
	f.addChat(ch)
	f.setAsset("mxc://beeper.local/photo1", make([]byte, 2<<20)) // 2 MiB body
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	opts.MaxMediaBytes = 1 << 20 // 1 MiB cap
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.EqualValues(0, sum.AttachmentsDownloaded)
	assert.EqualValues(1, sum.AttachmentsPending)
	assert.EqualValues(0, sum.Errors, "over-cap is a skip, not an error")

	var itemCount int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_run_items WHERE error_kind = 'beeper_media_too_large'`).Scan(&itemCount))
	assert.Equal(1, itemCount)
}

func TestImportNoMediaSkipsDownloadsEntirely(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(mediaChat())
	f.setAsset("mxc://beeper.local/photo1", []byte("bytes"))
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	opts.NoMedia = true
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	assert.EqualValues(0, sum.AttachmentsDownloaded)
	assert.EqualValues(0, sum.AttachmentsPending)

	var attCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attCount))
	assert.Zero(attCount, "no rows, not even markers, when media is disabled")

	// Message metadata still reflects the attachment.
	var hasAtt bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT has_attachments FROM messages WHERE source_message_id = ?`), "p0").Scan(&hasAtt))
	assert.True(hasAtt)
}

func TestImportClearsAttachmentsRemovedAtSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(mediaChat())
	f.setAsset("mxc://beeper.local/photo1", []byte("bytes"))
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	var attCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attCount))
	require.Equal(1, attCount)

	// The sender redacts the media: the re-persisted message has no
	// attachments, so the stale row must be cleared, not kept forever.
	f.chat("!media:beeper.local").Msgs[0].Attachments = nil
	_, err = imp.Import(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir, Full: true,
	})
	require.NoError(err)

	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attCount))
	assert.Zero(attCount, "attachments removed at the source must be cleared")
	var hasAtt bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT has_attachments FROM messages WHERE source_message_id = ?`), "p0").Scan(&hasAtt))
	assert.False(hasAtt)
}

func TestBackfillMediaClearsMarkerWhenAttachmentGone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(mediaChat())
	// Asset missing: the sync leaves a pending marker.
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// Before the retry, the message loses its attachment at the source; the
	// marker must be cleared or BackfillMedia would revisit it forever.
	f.chat("!media:beeper.local").Msgs[0].Attachments = nil
	bsum, err := imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	require.EqualValues(1, bsum.MessagesProcessed)

	var attCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attCount))
	assert.Zero(attCount)

	bsum, err = imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.EqualValues(0, bsum.MessagesProcessed, "cleared marker must not be revisited")
}

func TestBackfillMediaTransientFetchErrorKeepsMarker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	ch := mediaChat()
	// A newer plain message so the sync anchors on it, keeping the anchor
	// distinct from the message whose fetch this test makes fail.
	ch.Msgs = append(ch.Msgs, fakeMsg{
		ID: "p1", SortKey: 1, Timestamp: ch.Msgs[0].Timestamp.Add(time.Minute),
		Text: "anchor me", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)
	// Asset missing: the sync leaves a pending marker.
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// The retry hits a transient failure fetching the message: the run must
	// surface the error (not report clean counts) and keep the marker.
	f.setMessageGetFailure("p0", true)
	bsum, err := imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.Positive(bsum.Errors, "transient fetch failures must be reported")
	assert.EqualValues(0, bsum.AttachmentsDownloaded)

	var markers int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM attachments WHERE content_hash IS NULL OR content_hash = ''`).Scan(&markers))
	require.Equal(1, markers, "marker must survive a transient failure")

	// Healed: the asset appears and the retry succeeds.
	f.setMessageGetFailure("p0", false)
	f.setAsset("mxc://beeper.local/photo1", []byte("finally"))
	bsum, err = imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.EqualValues(1, bsum.AttachmentsDownloaded)
	assert.EqualValues(0, bsum.Errors)
}

func TestBackfillMediaVerifiesAnchorFirst(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(mediaChat())
	// Asset missing: the sync leaves a pending marker.
	imp, _, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// Simulate a reinstall re-assigning message IDs: the backfill trusts
	// stored IDs, so it must abort like the main sync instead of attaching
	// some other message's media to archived rows.
	ch := f.chat("!media:beeper.local")
	for i := range ch.Msgs {
		ch.Msgs[i].Timestamp = ch.Msgs[i].Timestamp.Add(time.Hour)
	}
	f.setAsset("mxc://beeper.local/photo1", []byte("wrong install"))
	f.resetRequests()

	_, err = imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.Error(err)
	assert.Contains(err.Error(), "re-assigned")
	for _, req := range f.requests() {
		assert.NotContains(req, "/v1/assets/serve", "no media may be fetched after an anchor mismatch: %s", req)
	}
}

func TestBackfillMediaGoneMessageKeepsDownloadedRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	ch := mediaChat()
	// Two attachments on one message: one downloadable, one missing.
	ch.Msgs[0].Attachments = append(ch.Msgs[0].Attachments, map[string]any{
		"id": "mxc://beeper.local/photo2", "type": "img", "mimeType": "image/png", "fileName": "gone.png",
	})
	addFillerMessages(ch, 5)
	f.addChat(ch)
	f.setAsset("mxc://beeper.local/photo1", []byte("archived bytes"))
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	sum, err := imp.Import(context.Background(), opts)
	require.NoError(err)
	require.EqualValues(1, sum.AttachmentsDownloaded)
	require.EqualValues(1, sum.AttachmentsPending)

	// The source message disappears before the retry (other messages
	// remain): only the pending marker may be cleared — the downloaded
	// attachment is archived media — and a gone message is expected churn,
	// not an error.
	chat := f.chat("!media:beeper.local")
	chat.Msgs = chat.Msgs[1:] // drop p0, keep fillers
	bsum, err := imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.EqualValues(0, bsum.Errors)

	var downloaded, markers int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM attachments WHERE content_hash IS NOT NULL AND content_hash != ''`).Scan(&downloaded))
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM attachments WHERE content_hash IS NULL OR content_hash = ''`).Scan(&markers))
	assert.Equal(1, downloaded, "downloaded media must survive the marker cleanup")
	assert.Zero(markers)

	// The cleared marker must not be revisited by later backfills.
	bsum, err = imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)
	assert.EqualValues(0, bsum.MessagesProcessed)
}

func TestBackfillMediaRearmsLostAnchor(t *testing.T) {
	require := require.New(t)

	f := newFakeBeeper(t)
	ch := mediaChat()
	// A newer plain message becomes the anchor, distinct from the media message.
	ch.Msgs = append(ch.Msgs, fakeMsg{
		ID: "p1", SortKey: 1, Timestamp: ch.Msgs[0].Timestamp.Add(time.Minute),
		Text: "anchor me", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
	})
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)
	f.setAsset("mxc://beeper.local/photo1", []byte("bytes"))
	imp, st, done := newTestImporter(t, f)
	defer done()

	opts := mediaImportOptions(t)
	_, err := imp.Import(context.Background(), opts)
	require.NoError(err)

	// The anchor message is deleted (chat alive). The media run soft-clears
	// it and — becoming the newest completed baseline — must re-arm before
	// persisting, or every later run would start unguarded.
	chat := f.chat("!media:beeper.local")
	chat.Msgs = chat.Msgs[:1] // drop p1, keep p0
	_, err = imp.BackfillMedia(context.Background(), ImportOptions{
		AccountID: "signal", AttachmentsDir: opts.AttachmentsDir,
	})
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotEmpty(state.Anchors, "media runs must not persist a disarmed reinstall guard")
	require.Equal("p0", state.Anchors[0].MessageID)
}

func TestMediaTypeOf(t *testing.T) {
	assert := assert.New(t)
	cases := []struct {
		name string
		att  Attachment
		want string
	}{
		{"image", Attachment{Type: "img"}, "image"},
		{"video", Attachment{Type: "video"}, "video"},
		{"audio", Attachment{Type: "audio"}, "audio"},
		{"unknown type", Attachment{Type: "something"}, "document"},
		// Flags outrank the base type: a sticker served as img is a sticker.
		{"sticker wins over img", Attachment{Type: "img", IsSticker: true}, "sticker"},
		{"gif wins over video", Attachment{Type: "video", IsGif: true}, "gif"},
		{"voice note wins over audio", Attachment{Type: "audio", IsVoiceNote: true}, "voice_note"},
	}
	for _, tc := range cases {
		assert.Equal(tc.want, mediaTypeOf(&tc.att), tc.name)
	}

	assert.Equal("mxc://x/1", assetRef(&Attachment{ID: "mxc://x/1", SrcURL: "http://y"}))
	assert.Equal("http://y", assetRef(&Attachment{SrcURL: "http://y"}), "srcURL is the fallback when the asset ID is empty")
	assert.Empty(assetRef(&Attachment{}))
}
