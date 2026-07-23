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
	assert := assert.New(t)
	tm := tsTime("1734567890.123456")
	assert.Equal(time.Date(2024, 12, 19, 0, 24, 50, 123456000, time.UTC), tm)
	assert.True(tsTime("").IsZero())
	assert.True(tsTime("garbage").IsZero())

	// Numeric, not lexical: a 9-digit second count sorts before a 10-digit one.
	assert.True(tsLess("999999999.000001", "1734567890.000001"))
	assert.False(tsLess("1734567890.000002", "1734567890.000001"))
}

func TestMessageRawCapturedOnDecode(t *testing.T) {
	blob := `{"type":"message","ts":"1.000001","text":"hi","unmodeled_field":{"kept":true}}`
	var m Message
	require.NoError(t, json.Unmarshal([]byte(blob), &m))
	assert.JSONEq(t, blob, string(m.Raw), "raw archive must preserve fields the struct does not model")
}

func TestThreadPredicates(t *testing.T) {
	assert := assert.New(t)
	root := Message{TS: "5.0", ThreadTS: "5.0", ReplyCount: 2}
	reply := Message{TS: "6.0", ThreadTS: "5.0"}
	plain := Message{TS: "7.0"}
	assert.True(root.IsThreadRoot())
	assert.False(root.IsThreadReply())
	assert.True(reply.IsThreadReply())
	assert.False(reply.IsThreadRoot())
	assert.False(plain.IsThreadRoot())
	assert.False(plain.IsThreadReply())
}

func TestPayloadTextExtraction(t *testing.T) {
	lookup := func(string) string { return "" }

	t.Run("fallback wins per attachment", func(t *testing.T) {
		m := Message{Attachments: []LegacyAttachment{
			{Fallback: "Build #42 failed", Title: "ignored", Text: "ignored too"},
		}}
		assert.Equal(t, "Build #42 failed", payloadText(&m, lookup))
	})

	t.Run("composed parts when no fallback", func(t *testing.T) {
		m := Message{Attachments: []LegacyAttachment{{
			Pretext: "Deploy finished",
			Title:   "api-server v1.2.3",
			Text:    "all checks green",
			Fields: []struct {
				Title string `json:"title"`
				Value string `json:"value"`
			}{{Title: "Env", Value: "prod"}},
			Footer: "deploybot",
		}}}
		assert.Equal(t, "Deploy finished\napi-server v1.2.3\nall checks green\nEnv: prod\ndeploybot",
			payloadText(&m, lookup))
	})

	t.Run("shape-shifting block elements decode (live regression)", func(t *testing.T) {
		// Observed live: actions-block buttons carry text as an OBJECT while
		// context elements carry it as a string; both must decode, and image
		// elements (no text) must not error.
		blob := `{"type":"message","ts":"1.000001","bot_id":"B1","blocks":[
			{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Approve"}}]},
			{"type":"context","elements":[{"type":"mrkdwn","text":"requested by alice"},{"type":"image","image_url":"https://x.example/i.png","alt_text":"pic"}]}
		]}`
		var m Message
		require.NoError(t, json.Unmarshal([]byte(blob), &m))
		assert.Equal(t, "requested by alice", payloadText(&m, lookup),
			"context text extracted; button labels and images are not content")
	})

	t.Run("block kit section header context", func(t *testing.T) {
		m := Message{Blocks: []Block{
			{Type: "header", Text: &BlockText{Type: "plain_text", Text: "Alert!"}},
			{Type: "section", Text: &BlockText{Type: "mrkdwn", Text: "disk *full* on <https://host.example|host>"},
				Fields: []BlockText{{Type: "mrkdwn", Text: "Sev: 1"}}},
			{Type: "context", Elements: []BlockElement{{Type: "mrkdwn", Text: "triggered 5m ago"}}},
			{Type: "rich_text"}, // duplicates message text: never extracted
			{Type: "divider"},
		}}
		assert.Equal(t, "Alert!\ndisk *full* on host (https://host.example)\nSev: 1\ntriggered 5m ago",
			payloadText(&m, lookup))
	})
}

func TestMapMessagePayloadRules(t *testing.T) {
	lookup := func(string) string { return "" }
	unfurl := []LegacyAttachment{{Fallback: "Some Page Title — example.com"}}

	t.Run("bot message with empty text gets payload body", func(t *testing.T) {
		m := Message{TS: "1.000001", BotID: "B1", Attachments: unfurl}
		_, text := mapMessage(&m, "C1", 1, 1, false, lookup)
		assert.Equal(t, "Some Page Title — example.com", text)
	})

	t.Run("bot message with text appends payload", func(t *testing.T) {
		m := Message{TS: "1.000001", BotID: "B1", Text: "heads up", Attachments: unfurl}
		_, text := mapMessage(&m, "C1", 1, 1, false, lookup)
		assert.Equal(t, "heads up\nSome Page Title — example.com", text)
	})

	t.Run("user message with text ignores unfurl attachments", func(t *testing.T) {
		m := Message{TS: "1.000001", User: "U1", Text: "check https://example.com", Attachments: unfurl}
		_, text := mapMessage(&m, "C1", 1, 1, false, lookup)
		assert.Equal(t, "check https://example.com", text)
	})

	t.Run("files placeholder still applies when no payload", func(t *testing.T) {
		m := Message{TS: "1.000001", User: "U1", Files: []File{{Name: "a.png"}}}
		_, text := mapMessage(&m, "C1", 1, 1, false, lookup)
		assert.Equal(t, "[file: a.png]", text)
	})
}

func TestConversationTypeAndTitle(t *testing.T) {
	assert := assert.New(t)
	lookup := func(id string) string {
		if id == "UALICE" {
			return "Alice"
		}
		return ""
	}
	channel := &Conversation{IsChannel: true, Name: "general"}
	assert.Equal("channel", conversationType(channel))
	assert.Equal("#general", conversationTitle(channel, lookup))

	im := &Conversation{IsIM: true, User: "UALICE"}
	assert.Equal("direct_chat", conversationType(im))
	assert.Equal("Alice", conversationTitle(im, lookup))

	mpim := &Conversation{IsMpim: true, Name: "mpdm-a--b-1"}
	assert.Equal("group_chat", conversationType(mpim))
	assert.Equal("mpdm-a--b-1", conversationTitle(mpim, lookup))
}
