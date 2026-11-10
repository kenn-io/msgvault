package tumblr

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
)

func TestTumblrParserEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data, err := os.ReadFile("testdata/observed-shape/conversations/convo_700.html")
	require.NoError(err)
	c, err := parseConversation(t.Context(), data)
	require.NoError(err)
	assert.Equal("700", c.ID)
	assert.Equal("ACTIVE", c.Status)
	require.Len(c.Participants, 2)
	assert.Equal(Participant{"orchard-example", "opaque-owner", "Active"}, c.Participants[0])
	require.Len(c.Messages, 4)
	assert.Equal([]string{"https://posts.example/42"}, c.Messages[0].Targets)
	assert.Equal("Hello 🌳\nSecond line & tea", c.Messages[1].Text)
	assert.Equal(int64(1704067201), c.Messages[1].SentAt.Time.Unix())
	assert.Contains(c.Messages[1].HTML, "message-info")
	assert.Empty(c.Messages[1].Media)
	c, err = parseConversation(t.Context(), []byte(strings.ReplaceAll(string(data), "<p>Hello", "<script>ignore me</script><style>ignore css</style><p>Hello")))
	require.NoError(err)
	assert.NotContains(c.Messages[1].Text, "ignore")
}
func TestTumblrParserSyntheticCases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data, err := os.ReadFile("testdata/synthetic-media/conversations/convo_700.html")
	require.NoError(err)
	c, err := parseConversation(t.Context(), data)
	require.NoError(err)
	assert.Equal(1, c.Malformed)
	require.Len(c.Messages, 4)
	assert.Equal("IMAGE", c.Messages[0].Type)
	assert.Empty(c.Messages[0].Text)
	assert.Equal([]string{"../media/picture.txt"}, c.Messages[0].Media)
	assert.False(c.Messages[1].SentAt.Valid)
	assert.True(c.Messages[3].DateConflict)
	assert.False(c.Messages[3].SentAt.Valid)
	for _, date := range []string{"", "unknown", "2024-01-01 00:00:00 GMT", "2024-01-01 00:00:00 GMT (broken)"} {
		html := `<h2>Conversation ID: 1</h2><div class="container"><span class="message-info">visitor TEXT @ ` + date + `</span><p>Example</p></div>`
		c, err := parseConversation(t.Context(), []byte(html))
		require.NoError(err)
		require.Len(c.Messages, 1)
		assert.Equal(date == "2024-01-01 00:00:00 GMT", c.Messages[0].SentAt.Valid)
	}
}
