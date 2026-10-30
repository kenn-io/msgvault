package chatwoot

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
)

// recordingWAV is a synthetic stereo, 8 kHz PCM recording.
func recordingWAV() []byte {
	data := make([]byte, 76)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 2)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 32000)
	binary.LittleEndian.PutUint16(data[32:], 4)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(data)-44))
	return data
}

func TestCallRecordingCapAndFormat(t *testing.T) {
	mp3, err := os.ReadFile("testdata/silence.mp3")
	require.NoError(t, err)
	rawMP3, err := os.ReadFile("testdata/silence-no-id3.mp3")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, path, reported, hint, wantMIME string
		payload                              []byte
		attached                             bool
	}{
		{"call_url_wav", "/recording", "application/octet-stream", "", "audio/wav", recordingWAV(), false},
		{"attachment_wav", "/recording.wav", "application/octet-stream", "audio/mpeg", "audio/wav", recordingWAV(), true},
		{"call_url_mp3", "/recording", "application/octet-stream", "", "audio/mpeg", mp3, false},
		{"call_url_mp3_without_id3", "/recording", "application/octet-stream", "", "audio/mpeg", rawMP3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(r.Header.Get("Api_access_token"))
				w.Header().Set("Content-Type", tc.reported)
				_, err := w.Write(tc.payload)
				assert.NoError(err)
			}))
			t.Cleanup(media.Close)
			router := newChatwootMediaRouter(t, media)
			remote := router.url(t, media, tc.path)
			message := mediaRefreshCall(remote, "")
			if tc.attached {
				message["attachments"] = []any{map[string]any{"id": 2001, "file_type": "audio", "content_type": tc.hint, "file_size": len(tc.payload), "data_url": remote}}
			}
			ordinary := chatwootapi.Message(902, now().Unix(), nil)
			ordinary["attachments"] = []any{map[string]any{"id": 2002, "file_type": "audio", "file_size": len(tc.payload), "data_url": remote + "?ordinary=true"}}
			api := newContractAPI(t, 2, []map[string]any{message, ordinary})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			opts := ImportOptions{InboxID: 7, Policy: attachmentpolicy.Policy{MaxBytes: 8}, AttachmentsDir: t.TempDir()}
			_, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			meetingID := contractArchivedMessageID(t, st, "call:901")
			refs, err := st.MessageProviderAttachments(meetingID, "chatwoot:")
			require.NoError(err)
			require.Len(refs, 1)
			for _, ref := range refs {
				require.Equal(attachmentpolicy.StateStored, ref.State, "call recording uses its own cap")
				assert.Equal(tc.wantMIME, ref.MimeType)
				assert.Equal("audio", ref.MediaType)
				assert.Equal(store.AttachmentRoleStandalone, ref.Role)
				assert.NotEmpty(ref.SourceAttachmentID)
				assert.NotEmpty(ref.SourcePartKey)
				payload, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, filepath.FromSlash(ref.StoragePath)))
				require.NoError(err)
				assert.Equal(tc.payload, payload)
			}
			candidates, err := st.ListBeeperMediaCandidates(t.Context(), 0, 100)
			require.NoError(err)
			assert.NotEmpty(candidates, "the real stored call reaches shared Docbank discovery")
			ordinaryRefs, err := st.MessageProviderAttachments(contractArchivedMessageID(t, st, "902"), "chatwoot:")
			require.NoError(err)
			require.Len(ordinaryRefs, 1)
			for _, ref := range ordinaryRefs {
				assert.Equal(attachmentpolicy.StateSkipped, ref.State)
				assert.Equal(attachmentpolicy.SkipSizeCap, ref.SkipReason)
				assert.Empty(ref.ContentHash)
			}
		})
	}
}

func TestRecordingDefaultCapDeclaredBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int64
		want      attachmentpolicy.DownloadState
		downloads int
	}{
		{"at_4_gib", 4 << 30, attachmentpolicy.StateStored, 1},
		{"over_4_gib", (4 << 30) + 1, attachmentpolicy.StateSkipped, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests atomic.Int64
			media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "audio/wav")
				_, err := w.Write(recordingWAV())
				assert.NoError(err)
			}))
			t.Cleanup(media.Close)
			router := newChatwootMediaRouter(t, media)
			message := mediaRefreshCall("", "")
			message["attachments"] = []any{map[string]any{"id": 2001, "file_type": "audio", "file_size": tc.size, "data_url": router.url(t, media, "/recording.wav")}}
			api := newContractAPI(t, 2, []map[string]any{message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, AttachmentsDir: t.TempDir()})
			require.NoError(err)
			for _, id := range []string{"901", "call:901"} {
				refs, err := st.MessageProviderAttachments(contractArchivedMessageID(t, st, id), "chatwoot:")
				require.NoError(err)
				require.Len(refs, 1)
				assert.Equal(tc.want, refs["chatwoot:attachment:2001"].State)
				if tc.want == attachmentpolicy.StateSkipped {
					assert.Equal(attachmentpolicy.SkipSizeCap, refs["chatwoot:attachment:2001"].SkipReason)
					assert.Empty(refs["chatwoot:attachment:2001"].ContentHash)
				}
			}
			assert.Equal(int64(tc.downloads), requests.Load(), "declared boundary is checked without allocating GiB of fixture data")
		})
	}
}
