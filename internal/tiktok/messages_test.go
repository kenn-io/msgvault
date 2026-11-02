package tiktok

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMessageEventsPreserveLargeIDsAndTypedContent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	raw := []byte(`{"client_key":"app-example","event":"im_receive_msg","user_openid":"account-example","content":"{\"message_id\":9007199254740999,\"conversation_id\":\"conversation+example/\",\"timestamp\":1700000000001,\"type\":\"video\",\"video\":{\"media_id\":9007199254740997}}"}`)
	event, message, err := DecodeMessageEvent("app-example", raw)
	requirements.NoError(err)
	assertions.Equal("im_receive_msg", event.Event)
	requirements.NotNil(message)
	assertions.Equal(ID("9007199254740999"), message.ID)
	assertions.Equal(ID("9007199254740997"), message.Video.ID)
	assertions.Equal("video", message.Kind())
	_, _, err = DecodeMessageEvent("another-app", raw)
	requirements.ErrorIs(err, ErrInvalidEvent)
	_, message, err = DecodeMessageEvent("app-example", []byte(`{"client_key":"app-example","event":"im_receive_msg_eu","user_openid":"account-example"}`))
	requirements.NoError(err)
	assertions.Nil(message)
	_, _, err = DecodeMessageEvent("app-example", []byte(`{"client_key":"app-example","event":"im_receive_msg","user_openid":"account-example","content":{"message_id":1}}`))
	requirements.ErrorIs(err, ErrInvalidEvent)
}

func TestMessagingArchivesMediaEchoesAndSharedCommenterIdentity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	attachments := t.TempDir()
	var downloads atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/business/get/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example-account"}}`)
		case "/business/video/list/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"videos":[{"item_id":"video-example","caption":"Example video","create_time":1700000000}],"has_more":false}}`)
		case "/business/comment/list/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"comments":[{"comment_id":"comment-example","unique_identifier":"actor-example","text":"Example comment","create_time":1700000000}],"has_more":false}}`)
		case "/business/message/media/download/":
			var body map[string]string
			if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
				http.Error(w, "invalid fixture", http.StatusBadRequest)
				return
			}
			assertions.Equal("VIDEO", body["media_type"])
			assertions.Equal("conversation+example/", body["conversation_id"])
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"download_url":"https://media.tiktokcdn.com/synthetic-media"}}`)
		case "/synthetic-media":
			downloads.Add(1)
			assertions.Equal("fixture-access-token", r.Header.Get("X-User"))
			assertions.Empty(r.Header.Get("Access-Token"))
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = fmt.Fprint(w, "synthetic media bytes")
		default:
			http.NotFound(w, r)
		}
	})
	// Keep URL validation and the real HTTP request path. Only route the
	// provider CDN hostname to the local synthetic HTTP fixture.
	target, err := url.Parse(c.baseURL)
	requirements.NoError(err)
	transport := c.http.Transport
	c.http.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "media.tiktokcdn.com" {
			copyRequest := r.Clone(r.Context())
			copyURL := *r.URL
			copyURL.Host, copyURL.Scheme = target.Host, target.Scheme
			copyRequest.URL = &copyURL
			return transport.RoundTrip(copyRequest)
		}
		return transport.RoundTrip(r)
	})
	comments := NewImporter(st, c)
	commentSource, err := comments.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	_, err = comments.Import(t.Context(), "app-example", "account-example", ImportOptions{})
	requirements.NoError(err)
	imp := NewMessageImporter(st, c, MessageImportOptions{AttachmentsDir: attachments, MaxMediaBytes: 1024})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	inbound := MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}
	m := BusinessMessage{ID: "9007199254740999", ConversationID: "conversation+example/", Timestamp: 1700000000001, Type: "video", FromUser: &MessageActor{ID: "actor-example", DisplayName: "Example Viewer"}, ToUser: &MessageActor{ID: "account-example"}, Video: &MessageMedia{ID: "9007199254740997"}}
	for i := range 2 {
		sum, err := imp.Apply(t.Context(), "app-example", inbound, m)
		requirements.NoError(err)
		assertions.Equal(1-i, sum.MessagesAdded)
	}
	assertions.EqualValues(1, downloads.Load())
	echo := m
	echo.ID = "9007199254741001"
	echo.Type = "text"
	echo.Video = nil
	echo.Text = &MessageText{Body: "Example outgoing echo"}
	echo.FromUser, echo.ToUser = m.ToUser, m.FromUser
	_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_send_msg", Account: "account-example"}, echo)
	requirements.NoError(err)
	shared := m
	shared.ID = "9007199254741003"
	shared.Type = "share_post"
	shared.Video = nil
	shared.SharePost = &SharedPost{ID: "post-example", EmbedURL: "https://www.tiktok.com/player/v1/100001"}
	_, err = imp.Apply(t.Context(), "app-example", inbound, shared)
	requirements.NoError(err)
	sticker := m
	sticker.ID, sticker.Type, sticker.Video = "9007199254741005", "sticker", nil
	sticker.Sticker = &MessageURL{URL: "https://media.tiktokcdn.com/synthetic-media"}
	_, err = imp.Apply(t.Context(), "app-example", inbound, sticker)
	requirements.NoError(err)
	assertions.EqualValues(2, downloads.Load())
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(m.ConversationID, m.ID), messageKey(echo.ConversationID, echo.ID), messageKey(shared.ConversationID, shared.ID), messageKey(sticker.ConversationID, sticker.ID)})
	requirements.NoError(err)
	requirements.Len(rows, 4)
	assertions.True(rows[messageKey(echo.ConversationID, echo.ID)].SourceIsFromMe.Bool)
	stored, err := st.GetMessageContext(t.Context(), rows[messageKey(m.ConversationID, m.ID)].ID)
	requirements.NoError(err)
	requirements.Len(stored.Attachments, 1)
	hash := stored.Attachments[0].ContentHash
	requirements.Len(hash, 64)
	blob, err := os.ReadFile(filepath.Join(attachments, hash[:2], hash))
	requirements.NoError(err)
	assertions.Equal("synthetic media bytes", string(blob))
	storedSticker, err := st.GetMessageContext(t.Context(), rows[messageKey(sticker.ConversationID, sticker.ID)].ID)
	requirements.NoError(err)
	requirements.Len(storedSticker.Attachments, 1)
	assertions.Contains(storedSticker.Body, "[sticker]")
	commentRows, err := st.StoredMessagesContext(t.Context(), commentSource.ID, commentFormat, []string{"comment-example"})
	requirements.NoError(err)
	assertions.Equal(commentRows["comment-example"].SenderID, rows[messageKey(m.ConversationID, m.ID)].SenderID)
	assertions.Contains(rows[messageKey(shared.ConversationID, shared.ID)].Snippet.String, "https://www.tiktok.com/player/v1/100001")
}

func TestMessagingPullCapsConversationsAndEncodesPlus(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var contentReads atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/business/get/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example-account"}}`)
		case "/business/message/conversation/list/":
			var conversations []BusinessConversation
			for i := range 100 {
				conversations = append(conversations, BusinessConversation{ID: ID(fmt.Sprintf("conversation+%s%d", r.URL.Query().Get("conversation_type"), i)), UpdateTime: Integer(i)})
			}
			assertions.NoError(json.MarshalWrite(w, map[string]any{"code": 0, "data": ConversationPage{Conversations: conversations}}))
		case "/business/message/content/list/":
			contentReads.Add(1)
			assertions.True(strings.HasPrefix(r.URL.Query().Get("conversation_id"), "conversation+"))
			assertions.Contains(r.URL.RawQuery, "%2B")
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"messages":[]}}`)
		default:
			http.NotFound(w, r)
		}
	})
	imp := NewMessageImporter(st, c, MessageImportOptions{AttachmentsDir: t.TempDir()})
	_, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	assertions.EqualValues(100, contentReads.Load())
	assertions.Equal(103, sum.Requests)
}

func TestMediaURLsDoNotForwardCredentialsOutsideProvider(t *testing.T) {
	requirements := require.New(t)
	c, err := NewClient("fixture-access-token")
	requirements.NoError(err)
	calls := 0
	c.http.Transport = fixtureTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected fixture dispatch")
	})
	credentialed := &url.URL{Scheme: "https", Host: "media.tiktokcdn.com", Path: "/image", User: url.UserPassword("example-user", "example-password")}
	for _, target := range []string{"https://example.com/image", "http://media.tiktokcdn.com/image", "https://media.tiktokcdn.com.example.com/image", credentialed.String(), "https://127.0.0.1/image"} {
		_, err := c.OpenMessageMedia(t.Context(), "account-example", BusinessMessage{Type: "sticker", Sticker: &MessageURL{URL: target}}, 1024)
		requirements.Error(err)
	}
	assert.Zero(t, calls, "rejected URLs must never reach the transport")
}

func TestMessagesRejectOversizedHistoryPage(t *testing.T) {
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		page := MessagePage{Messages: make([]BusinessMessage, 21)}
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"code": 0, "data": page}))
	})
	_, err := c.Messages(t.Context(), "account-example", "conversation+example/")
	requirements.ErrorContains(err, "20-message")
}

func TestMessagingBackfillContinuesAfterMediaFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/business/get/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example-account"}}`)
		case "/business/message/conversation/list/":
			if r.URL.Query().Get("conversation_type") == "SINGLE" {
				_, _ = fmt.Fprint(w, `{"code":0,"data":{"conversations":[{"conversation_id":"conversation-example"}]}}`)
			} else {
				_, _ = fmt.Fprint(w, `{"code":0,"data":{"conversations":[]}}`)
			}
		case "/business/message/content/list/":
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"messages":[{"message_id":"media-message","timestamp":1700000000001,"message_type":"IMAGE","image":{"media_id":"media-example"}},{"message_id":"text-message","timestamp":1700000000002,"message_type":"TEXT","text":{"body":"Example follow-up"}}]}}`)
		case "/business/message/media/download/":
			_, _ = fmt.Fprint(w, `{"code":40105,"message":"unavailable fixture media"}`)
		default:
			http.NotFound(w, r)
		}
	})
	imp := NewMessageImporter(st, c, MessageImportOptions{AttachmentsDir: t.TempDir()})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), "app-example", "account-example")
	requirements.Error(err)
	assertions.True(sum.Partial)
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey("conversation-example", "media-message"), messageKey("conversation-example", "text-message")})
	requirements.NoError(err)
	requirements.Len(rows, 2)
	assertions.Equal("Example follow-up", rows[messageKey("conversation-example", "text-message")].Snippet.String)
}

func TestMessageRawPreservesNestedProviderFields(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example-account"}}`)
	})
	imp := NewMessageImporter(st, c, MessageImportOptions{AttachmentsDir: t.TempDir()})
	source, err := imp.Register(t.Context(), "app-example", "account-example")
	requirements.NoError(err)
	body := []byte(`{"message_id":9007199254740999,"conversation_id":"conversation-example","timestamp":1700000000001,"type":"text","text":{"body":"Example","provider_extension":9007199254740997},"reaction":[{"emoji":"example"}]}`)
	var message BusinessMessage
	requirements.NoError(json.Unmarshal(body, &message))
	_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, message)
	requirements.NoError(err)
	rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(message.ConversationID, message.ID)})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	data, err := st.GetMessageRawContext(t.Context(), rows[messageKey(message.ConversationID, message.ID)].ID)
	requirements.NoError(err)
	assert.Equal(t, body, data)
}

func TestMessageRepliesResolveAfterParentArrives(t *testing.T) {
	for _, mode := range []string{"webhook", "pull"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			child := BusinessMessage{ID: "reply-example", ConversationID: "conversation-example", Timestamp: 1700000000001, Type: "text", Text: &MessageText{Body: "Example reply"}, Reference: &MessageReference{ID: "parent-example"}}
			parent := BusinessMessage{ID: "parent-example", ConversationID: child.ConversationID, Timestamp: child.Timestamp, Type: "text", Text: &MessageText{Body: "Example parent"}}
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/business/get/":
					_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
				case "/business/message/conversation/list/":
					_, _ = fmt.Fprint(w, `{"code":0,"data":{"conversations":[{"conversation_id":"conversation-example"}]}}`)
				case "/business/message/content/list/":
					assertions.NoError(json.MarshalWrite(w, map[string]any{"code": 0, "data": MessagePage{Messages: []BusinessMessage{child, parent}}}))
				default:
					http.NotFound(w, r)
				}
			})
			imp := NewMessageImporter(st, c, MessageImportOptions{AttachmentsDir: t.TempDir()})
			source, err := imp.Register(t.Context(), "app-example", "account-example")
			requirements.NoError(err)
			if mode == "pull" {
				_, err = imp.Import(t.Context(), "app-example", "account-example")
				requirements.NoError(err)
			} else {
				for _, message := range []BusinessMessage{child, parent} {
					_, err = imp.Apply(t.Context(), "app-example", MessageEvent{ClientKey: "app-example", Event: "im_receive_msg", Account: "account-example"}, message)
					requirements.NoError(err)
				}
			}
			rows, err := st.StoredMessagesContext(t.Context(), source.ID, messageFormat, []string{messageKey(child.ConversationID, child.ID), messageKey(parent.ConversationID, parent.ID)})
			requirements.NoError(err)
			requirements.Len(rows, 2)
			var replyTo sql.NullInt64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT reply_to_message_id FROM messages WHERE id = ?", rows[messageKey(child.ConversationID, child.ID)].ID).Scan(&replyTo))
			requirements.True(replyTo.Valid)
			assertions.Equal(rows[messageKey(parent.ConversationID, parent.ID)].ID, replyTo.Int64)
		})
	}
}
