package youtube

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	yt "google.golang.org/api/youtube/v3"
)

func TestArchiveCheckpointDoesNotRepeatVideoMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	c := archiveFixture(t, false, nil, nil)
	description := strings.Repeat("synthetic description ", 200)
	original := c.api.Transport
	if original == nil {
		original = http.DefaultTransport
	}
	c.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil || req.URL.Path != "/youtube/v3/videos" {
			return resp, err
		}
		var videos yt.VideoListResponse
		decodeErr := json.UnmarshalRead(resp.Body, &videos)
		closeErr := resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		for i := range videos.Items {
			videos.Items[i].Snippet.Description = description
		}
		encoded, err := json.Marshal(videos)
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(strings.NewReader(string(encoded)))
		resp.ContentLength = int64(len(encoded))
		return resp, nil
	})
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "UCfixture")
	requirements.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 3})
	requirements.NoError(err)
	checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), src.ID, SourceType)
	requirements.NoError(err)
	requirements.NotNil(checkpoint)
	assertions.Less(len(checkpoint.CursorBefore.String), 2048, "resume state must stay independent of archived description size")
	rows, err := st.MessageMetadataBatch(t.Context(), src.ID, []string{"video:oldvideo"})
	requirements.NoError(err)
	requirements.Contains(rows, "video:oldvideo")
	body, err := st.GetMessageBodyText(t.Context(), rows["video:oldvideo"].ID)
	requirements.NoError(err)
	assertions.Equal(description+"\n\nhttps://www.youtube.com/watch?v=oldvideo", body, "compact resume state must not discard archived video text")
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	requirements.NoError(err)
	comments, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"parent"})
	requirements.NoError(err)
	requirements.Contains(comments, "parent")
	assertions.Equal("Fixture video", comments["parent"].Subject.String, "resumed comments retain the root title")
}
