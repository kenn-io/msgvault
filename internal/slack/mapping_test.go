package slack

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderText(t *testing.T) {
	names := map[string]string{"UALICE": "Alice"}
	lookup := func(id string) string { return names[id] }
	tests := []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},
		{"mention known", "hi <@UALICE>", "hi @Alice"},
		{"mention unknown", "hi <@UGONE>", "hi @UGONE"},
		{"mention labeled", "hi <@UGONE|ghost>", "hi @ghost"},
		{"channel", "see <#C042|general>", "see #general"},
		{"special", "<!here> heads up", "@here heads up"},
		{"link labeled", "read <https://example.com|the docs>", "read the docs (https://example.com)"},
		{"link bare", "read <https://example.com>", "read https://example.com"},
		{"entities", "a &lt;b&gt; &amp; c", "a <b> & c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, renderText(tt.in, lookup))
		})
	}
}

func TestMentionedUserIDs(t *testing.T) {
	m := Message{Text: "<@UALICE> meet <@WBOB|bob>, again <@UALICE>; not <#C1|chan> or <!here>"}
	assert.Equal(t, []string{"UALICE", "WBOB"}, m.MentionedUserIDs())
}

func TestTsTimeAndOrdering(t *testing.T) {
	tm := tsTime("1734567890.123456")
	assert.Equal(t, time.Date(2024, 12, 19, 0, 24, 50, 123456000, time.UTC), tm)
	assert.True(t, tsTime("").IsZero())
	assert.True(t, tsTime("garbage").IsZero())

	// Numeric, not lexical: a 9-digit second count sorts before a 10-digit one.
	assert.True(t, tsLess("999999999.000001", "1734567890.000001"))
	assert.False(t, tsLess("1734567890.000002", "1734567890.000001"))
}

func TestMessageRawCapturedOnDecode(t *testing.T) {
	blob := `{"type":"message","ts":"1.000001","text":"hi","unmodeled_field":{"kept":true}}`
	var m Message
	require.NoError(t, json.Unmarshal([]byte(blob), &m))
	assert.JSONEq(t, blob, string(m.Raw), "raw archive must preserve fields the struct does not model")
}

func TestThreadPredicates(t *testing.T) {
	root := Message{TS: "5.0", ThreadTS: "5.0", ReplyCount: 2}
	reply := Message{TS: "6.0", ThreadTS: "5.0"}
	plain := Message{TS: "7.0"}
	assert.True(t, root.IsThreadRoot())
	assert.False(t, root.IsThreadReply())
	assert.True(t, reply.IsThreadReply())
	assert.False(t, reply.IsThreadRoot())
	assert.False(t, plain.IsThreadRoot())
	assert.False(t, plain.IsThreadReply())
}

func TestConversationTypeAndTitle(t *testing.T) {
	lookup := func(id string) string {
		if id == "UALICE" {
			return "Alice"
		}
		return ""
	}
	channel := &Conversation{IsChannel: true, Name: "general"}
	assert.Equal(t, "channel", conversationType(channel))
	assert.Equal(t, "#general", conversationTitle(channel, lookup))

	im := &Conversation{IsIM: true, User: "UALICE"}
	assert.Equal(t, "direct_chat", conversationType(im))
	assert.Equal(t, "Alice", conversationTitle(im, lookup))

	mpim := &Conversation{IsMpim: true, Name: "mpdm-a--b-1"}
	assert.Equal(t, "group_chat", conversationType(mpim))
	assert.Equal(t, "mpdm-a--b-1", conversationTitle(mpim, lookup))
}
