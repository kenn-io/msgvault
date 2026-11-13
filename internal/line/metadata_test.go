package line

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line/linetest"
	"howett.net/plist"
)

func TestDecodeMetadata(t *testing.T) {
	require := require.New(t)
	md, err := decodeMetadata(linetest.KeyedArchive(t, map[string]string{
		"replyOriginalMessageId": "123", "systemMessageTypeName": "ChatEvent", "ALT_TEXT": " card ", "MENTION": "{}",
	}))
	require.NoError(err)
	require.Equal(metadata{ReplyTo: "123", SystemType: "ChatEvent", AltText: " card "}, md)
	for _, blob := range [][]byte{nil, linetest.KeyedArchive(t, map[string]string{})} {
		md, err := decodeMetadata(blob)
		require.NoError(err)
		require.Equal(metadata{}, md)
	}
	archive := func(top map[string]any, objects []any) []byte {
		blob, err := plist.Marshal(map[string]any{"$top": top, "$objects": objects}, plist.BinaryFormat)
		require.NoError(err)
		return blob
	}
	for name, blob := range map[string][]byte{
		"unreadable":              []byte("bplist00 garbage"),
		"missing root":            archive(map[string]any{}, []any{"$null"}),
		"invalid root":            archive(map[string]any{"root": plist.UID(4)}, []any{"$null"}),
		"missing arrays":          archive(map[string]any{"root": plist.UID(1)}, []any{"$null", map[string]any{}}),
		"unequal arrays":          archive(map[string]any{"root": plist.UID(1)}, []any{"$null", map[string]any{"NS.keys": []any{"ALT_TEXT"}, "NS.objects": []any{}}}),
		"invalid key":             archive(map[string]any{"root": plist.UID(1)}, []any{"$null", map[string]any{"NS.keys": []any{plist.UID(9)}, "NS.objects": []any{"value"}}}),
		"invalid value reference": archive(map[string]any{"root": plist.UID(1)}, []any{"$null", map[string]any{"NS.keys": []any{"ALT_TEXT"}, "NS.objects": []any{plist.UID(9)}}}),
		"invalid value type":      archive(map[string]any{"root": plist.UID(1)}, []any{"$null", map[string]any{"NS.keys": []any{"replyOriginalMessageId"}, "NS.objects": []any{42}}}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeMetadata(blob)
			assert.Error(t, err)
		})
	}
	blob := archive(map[string]any{"root": plist.UID(1)}, []any{"$null", map[string]any{"NS.keys": []any{"unknown"}, "NS.objects": []any{plist.UID(9)}}})
	md, err = decodeMetadata(blob)
	require.NoError(err)
	require.Equal(metadata{}, md)
}

func TestMessageKindAndBody(t *testing.T) {
	assert := assert.New(t)
	text := &Message{ID: "1", ContentType: 0, Text: " hi "}
	assert.Equal(kindText, text.kind())
	assert.Equal(" hi ", text.body())
	notice := &Message{ID: "2", ContentType: 0, Text: "Message unsent", Metadata: metadata{SystemType: "ChatMessageUnsent"}}
	assert.Equal(kindSystem, notice.kind())
	rich := &Message{ID: "3", ContentType: contentTypeRich, Text: "￼", Metadata: metadata{AltText: " alt "}}
	assert.Equal(kindText, rich.kind())
	assert.Equal(" alt ", rich.body())
	assert.Equal(kindOther, (&Message{ID: "4", ContentType: 9999, Text: "x"}).kind())
	assert.Equal("Nick", displayName("Nick", "Book", "Profile"))
	assert.Equal("Book", displayName(" ", "Book", "Profile"))
	assert.Equal("Profile", displayName("", "", "Profile"))
}
