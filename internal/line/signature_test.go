package line_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func TestSignatureRawBody(t *testing.T) {
	checks := assert.New(t)

	body := []byte(`{"events":[]}`)
	signature := lineapi.Sign("synthetic-secret", body)
	checks.True(line.Verify("synthetic-secret", body, signature))
	checks.False(line.Verify("synthetic-secret", append(body, ' '), signature))
	checks.False(line.Verify("another-secret", body, signature))
	for _, invalid := range []string{"", "!", signature[:len(signature)-1], signature + "\n"} {
		checks.False(line.Verify("synthetic-secret", body, invalid))
	}
}

func TestSanitizeEventRemovesCapabilities(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	raw := []byte(`{"type":"future","replyToken":"secret","message":{"id":"123","type":"file","fileName":"example.txt","fileSize":9007199254740993,"markAsReadToken":"secret","native":{"quoteToken":"keep"}},"unknown":[{"replyToken":"secret","nested":[{"markAsReadToken":"secret","number":9007199254740993}]}]}`)
	clean, err := line.SanitizeEvent(raw)
	must.NoError(err)
	checks.NotContains(string(clean), "replyToken")
	checks.NotContains(string(clean), "markAsReadToken")
	checks.Contains(string(clean), `9007199254740993`)
	checks.Contains(string(clean), `"quoteToken":"keep"`)
	var event line.Event
	must.NoError(json.Unmarshal(raw, &event))
	checks.JSONEq(string(clean), string(event.Raw))
	checks.Contains(string(event.Message.Raw), `"native":{"quoteToken":"keep"}`)
	for _, invalid := range []string{`null`, `[]`, `{`, `{} {}`, `{"replyToken":}`} {
		_, err := line.SanitizeEvent([]byte(invalid))
		must.Error(err)
	}
}

func TestOfficialEventShapes(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	for _, tc := range []struct {
		source   line.Source
		id, kind string
	}{
		{line.Source{Type: "user", UserID: "U00000000000000000000000000000002"}, "user:U00000000000000000000000000000002", "direct_chat"},
		{line.Source{Type: "group", GroupID: "C00000000000000000000000000000003"}, "group:C00000000000000000000000000000003", "group_chat"},
		{line.Source{Type: "room", RoomID: "R00000000000000000000000000000004"}, "room:R00000000000000000000000000000004", "group_chat"},
	} {
		id, kind, err := tc.source.ConversationID()
		must.NoError(err)
		checks.Equal(tc.id, id)
		checks.Equal(tc.kind, kind)
	}
	for _, source := range []line.Source{{}, {Type: "user"}, {Type: "group"}, {Type: "room"}, {Type: "future", UserID: "U00000000000000000000000000000002"}} {
		_, _, err := source.ConversationID()
		must.Error(err)
	}
}
