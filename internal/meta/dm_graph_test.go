package meta

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDMConversationsRequestShape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v26.0/100/conversations", r.URL.Path)
		assert.Equal("after=opaque%2B%2F%3D&fields=id%2Cupdated_time%2Cparticipants&limit=25&platform=messenger", r.URL.RawQuery)
		_, _ = fmt.Fprint(w, `{"data":[{"id":"t_1","updated_time":"2026-11-02T00:00:00+0000","participants":{"data":[{"id":"300","name":"Example","email":"ignored@example.com"}]}}],"paging":{"cursors":{"after":"next"},"next":"https://unsafe.example/secret"}}`)
	})
	p, err := c.Conversations(context.Background(), "100", PlatformMessenger.GraphPlatform(), "opaque+/=")
	require.NoError(err)
	require.Len(p.Data, 1)
	assert.Equal("300", p.Data[0].Participants[0].ID)
	assert.Equal("next", p.After)
}

func TestDMConversationMessageIDs(t *testing.T) {
	for _, count := range []int{0, 2, 20, 25} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			calls := 0
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal("/v26.0/t_1", r.URL.Path)
				assert.Equal("fields=messages", r.URL.RawQuery)
				_, _ = fmt.Fprint(w, `{"messages":{"data":[`)
				for i := range count {
					if i > 0 {
						_, _ = fmt.Fprint(w, ",")
					}
					_, _ = fmt.Fprintf(w, `{"id":"m_%d"}`, i)
				}
				if count == 2 || count == 25 {
					_, _ = fmt.Fprint(w, `],"paging":{"next":"https://unsafe.example/older"}}}`)
				} else {
					_, _ = fmt.Fprint(w, `]}}`)
				}
			})
			ids, capped, err := c.ConversationMessageIDs(context.Background(), "t_1")
			require.NoError(err)
			assert.Len(ids, min(20, count))
			assert.Equal(count == 2 || count > 20, capped)
			if count > 0 {
				assert.Equal("m_0", ids[0])
			}
			assert.Equal(1, calls)
		})
	}
}

func TestDMMessageNodeFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v26.0/m_1", r.URL.Path)
		assert.Equal("id,created_time,from,to,message,attachments,shares,story,reply_to,is_unsupported", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"id":"m_1","from":{"id":"300"},"to":{"data":[{"id":"100"}]},"message":"hello","attachments":{"data":[{"id":"attachment_example","mime_type":"image/jpeg","image_data":{"url":"https://cdn.example/image"}}]},"shares":{"data":[{"link":"https://example.com"}]},"reply_to":{"mid":"m_0"}}`)
	})
	m, err := c.Message(context.Background(), "m_1")
	require.NoError(err)
	assert.Equal("hello", m.Message)
	assert.True(m.MessagePresent)
	assert.Equal(1, m.Shares)
	require.Len(m.Attachments, 1)
	assert.Equal("image", m.Attachments[0].Type)
	assert.Equal("m_0", m.ReplyTo.Mid)
}

func TestDMMessagePresenceDistinguishesOmittedFromEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"m_1","from":{"id":"300"},"message":""}`)
	})
	m, err := c.Message(t.Context(), "m_1")
	require.NoError(err)
	assert.Empty(m.Message)
	assert.True(m.MessagePresent)
}

func TestDMGraphAttachmentKinds(t *testing.T) {
	for _, tt := range []struct{ name, node, want string }{
		{"image", `{"image_data":{"url":"https://cdn.example/image"}}`, "image"},
		{"sticker", `{"image_data":{"render_as_sticker":true,"url":"https://cdn.example/sticker"}}`, "sticker"},
		{"video", `{"video_data":{"url":"https://cdn.example/video"}}`, "video"},
		{"audio", `{"mime_type":"audio/mpeg","file_url":"https://cdn.example/audio"}`, "audio"},
		{"file", `{"mime_type":"application/pdf","file_url":"https://cdn.example/file"}`, "file"},
		{"template", `{"generic_template":{"title":"Example"}}`, "generic_template"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"id":"m_1","from":{"id":"300"},"attachments":{"data":[%s]},"story":{"id":"story_example","link":"https://cdn.example/story"}}`, tt.node)
			})
			m, err := c.Message(t.Context(), "m_1")
			require.NoError(err)
			assert.False(m.MessagePresent)
			w := dmTestWriter(t, PlatformInstagram)
			rec, err := recordFromNode(w.src, m)
			require.NoError(err)
			id, _, err := w.persist(t.Context(), rec)
			require.NoError(err)
			meta, blob := dmSavedMeta(t, w, id)
			require.Len(meta.Attachments, 1)
			assert.Equal(tt.want, meta.Attachments[0].Type)
			require.NotNil(meta.Story)
			assert.Equal("story_example", meta.Story.ID)
			assert.Empty(meta.Story.Kind)
			assert.NotContains(blob, "https://")
		})
	}
}

func TestDMReadRejectsUnsafeIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c := fixtureClient(t, func(http.ResponseWriter, *http.Request) { assert.Fail("unsafe ID made a request") })
	for _, id := range []string{"../me", "x/y", "x?y", "..", "x\n"} {
		_, err := c.Message(context.Background(), id)
		require.Error(err)
		_, _, err = c.ConversationMessageIDs(context.Background(), id)
		require.Error(err)
	}
	_, err := c.Conversations(context.Background(), "100", "messenger", string(make([]byte, 4097)))
	require.Error(err)
}
