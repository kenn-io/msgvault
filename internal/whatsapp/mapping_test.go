package whatsapp

import (
	"database/sql"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		user, server string
		want         string
	}{
		{"447700900000", "s.whatsapp.net", "+447700900000"},
		{"12025551234", "s.whatsapp.net", "+12025551234"},
		{"+447700900000", "s.whatsapp.net", "+447700900000"},
		{"", "s.whatsapp.net", ""},
		{"447700900000", "g.us", "+447700900000"},
	}

	for _, tt := range tests {
		got := normalizePhone(tt.user, tt.server)
		assertpkg.Equal(t, tt.want, got, "normalizePhone(%q, %q)", tt.user, tt.server)
	}
}

func TestMapMediaType(t *testing.T) {
	tests := []struct {
		waType int
		want   string
	}{
		{0, ""}, // text
		{1, "image"},
		{2, "video"},
		{3, "audio"},
		{4, "gif"},
		{5, "voice_note"},
		{13, "document"},
		{90, "sticker"},
		{7, ""},  // system (no media type)
		{15, ""}, // call
		{99, ""}, // poll
	}

	for _, tt := range tests {
		got := mapMediaType(tt.waType)
		assertpkg.Equal(t, tt.want, got, "mapMediaType(%d)", tt.waType)
	}
}

func TestIsMediaType(t *testing.T) {
	assertpkg.True(t, isMediaType(1), "isMediaType(1) should be true (image)")
	assertpkg.False(t, isMediaType(0), "isMediaType(0) should be false (text)")
	assertpkg.False(t, isMediaType(7), "isMediaType(7) should be false (system)")
}

func TestIsSkippedType(t *testing.T) {
	skipped := []int{7, 9, 10, 15, 64, 66, 99, 11}
	for _, typ := range skipped {
		assertpkg.True(t, isSkippedType(typ), "isSkippedType(%d) should be true", typ)
	}

	notSkipped := []int{0, 1, 2, 3, 4, 5, 13, 90}
	for _, typ := range notSkipped {
		assertpkg.False(t, isSkippedType(typ), "isSkippedType(%d) should be false", typ)
	}
}

func TestIsGroupChat(t *testing.T) {
	tests := []struct {
		name string
		chat waChat
		want bool
	}{
		{
			name: "direct chat",
			chat: waChat{Server: "s.whatsapp.net", GroupType: 0},
			want: false,
		},
		{
			name: "standard group",
			chat: waChat{Server: "g.us", GroupType: 1},
			want: true,
		},
		{
			name: "community sub-group (g.us + type=0)",
			chat: waChat{Server: "g.us", GroupType: 0},
			want: true,
		},
		{
			name: "broadcast",
			chat: waChat{Server: "broadcast", GroupType: 0},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isGroupChat(tt.chat)
			assertpkg.Equal(t, tt.want, got, "isGroupChat()")
		})
	}
}

func TestMapConversation(t *testing.T) {
	assert := assertpkg.New(t)
	// Direct chat.
	direct := waChat{
		RawString: "447700900000@s.whatsapp.net",
		GroupType: 0,
	}
	id, typ, title := mapConversation(direct)
	assert.Equal("447700900000@s.whatsapp.net", id, "direct chat sourceConvID")
	assert.Equal("direct_chat", typ, "direct chat convType")
	assert.Empty(title, "direct chat title")

	// Group chat.
	group := waChat{
		RawString: "120363001234567890@g.us",
		Server:    "g.us",
		GroupType: 1,
		Subject:   sql.NullString{String: "Family Group", Valid: true},
	}
	id, typ, title = mapConversation(group)
	assert.Equal("120363001234567890@g.us", id, "group chat sourceConvID")
	assert.Equal("group_chat", typ, "group chat convType")
	assert.Equal("Family Group", title, "group chat title")

	// Group with group_type=0 but g.us server (e.g. WhatsApp Community sub-groups).
	community := waChat{
		RawString: "120363377259312783@g.us",
		Server:    "g.us",
		GroupType: 0,
		Subject:   sql.NullString{String: "AI Impact", Valid: true},
	}
	_, typ, title = mapConversation(community)
	assert.Equal("group_chat", typ, "g.us with group_type=0: convType")
	assert.Equal("AI Impact", title, "g.us with group_type=0: title")
}

func TestMapMessage(t *testing.T) {
	assert := assertpkg.New(t)
	msg := waMessage{
		RowID:       42,
		ChatRowID:   1,
		FromMe:      1,
		KeyID:       "ABC123",
		Timestamp:   1700000000000, // ms
		MessageType: 0,
		TextData:    sql.NullString{String: "Hello world", Valid: true},
	}

	senderID := sql.NullInt64{Int64: 99, Valid: true}
	result := mapMessage(msg, 10, 20, senderID)

	assert.Equal(int64(10), result.ConversationID)
	assert.Equal(int64(20), result.SourceID)
	assert.Equal("ABC123", result.SourceMessageID)
	assert.Equal("whatsapp", result.MessageType)
	assert.True(result.IsFromMe, "IsFromMe should be true")
	assert.True(result.SentAt.Valid, "SentAt should be valid")
	assert.Equal(int64(1700000000), result.SentAt.Time.Unix(), "SentAt Unix")
	assert.True(result.Snippet.Valid, "Snippet valid")
	assert.Equal("Hello world", result.Snippet.String, "Snippet")
	assert.False(result.HasAttachments, "HasAttachments should be false for text message")
}

func TestMapMessageSnippetTruncation(t *testing.T) {
	// Create a message with text longer than 100 characters.
	var longText strings.Builder
	for range 150 {
		longText.WriteString("x")
	}

	msg := waMessage{
		KeyID:       "LONG1",
		Timestamp:   1700000000000,
		MessageType: 0,
		TextData:    sql.NullString{String: longText.String(), Valid: true},
	}

	result := mapMessage(msg, 1, 1, sql.NullInt64{})
	requirepkg.True(t, result.Snippet.Valid, "Snippet should be valid")
	assertpkg.Len(t, []rune(result.Snippet.String), 100, "Snippet rune count")
}

func TestMapGroupRole(t *testing.T) {
	tests := []struct {
		admin int
		want  string
	}{
		{0, "member"},
		{1, "admin"},
		{2, "admin"}, // superadmin
		{3, "member"},
	}

	for _, tt := range tests {
		got := mapGroupRole(tt.admin)
		assertpkg.Equal(t, tt.want, got, "mapGroupRole(%d)", tt.admin)
	}
}

func TestMapReaction(t *testing.T) {
	r := waReaction{
		ReactionValue: sql.NullString{String: "❤️", Valid: true},
	}
	val := mapReaction(r)
	assertpkg.Equal(t, "emoji", reactionTypeEmoji, "reaction type")
	assertpkg.Equal(t, "❤️", val, "reaction value")

	// Empty reaction.
	empty := waReaction{
		ReactionValue: sql.NullString{},
	}
	val = mapReaction(empty)
	assertpkg.Empty(t, val, "empty reaction value")
}

func TestResolveLidSender(t *testing.T) {
	lidMap := map[int64]waLidMapping{
		100: {LidRowID: 100, PhoneUser: "447957366403", PhoneServer: "s.whatsapp.net"},
		200: {LidRowID: 200, PhoneUser: "12025551234", PhoneServer: "s.whatsapp.net"},
	}

	tests := []struct {
		name     string
		jidRowID sql.NullInt64
		server   string
		want     string
	}{
		{
			name:     "lid sender found in map",
			jidRowID: sql.NullInt64{Int64: 100, Valid: true},
			server:   "lid",
			want:     "+447957366403",
		},
		{
			name:     "lid sender not in map",
			jidRowID: sql.NullInt64{Int64: 999, Valid: true},
			server:   "lid",
			want:     "",
		},
		{
			name:     "non-lid server ignored",
			jidRowID: sql.NullInt64{Int64: 100, Valid: true},
			server:   "s.whatsapp.net",
			want:     "",
		},
		{
			name:     "null jid row id",
			jidRowID: sql.NullInt64{Valid: false},
			server:   "lid",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLidSender(tt.jidRowID, tt.server, lidMap)
			assertpkg.Equal(t, tt.want, got, "resolveLidSender()")
		})
	}
}

func TestChatTitle(t *testing.T) {
	// Group with subject.
	group := waChat{
		Subject:   sql.NullString{String: "Work Chat", Valid: true},
		User:      "120363001234567890",
		Server:    "g.us",
		RawString: "120363001234567890@g.us",
	}
	assertpkg.Equal(t, "Work Chat", chatTitle(group), "chatTitle(group)")

	// Direct chat.
	direct := waChat{
		User:      "447700900000",
		Server:    "s.whatsapp.net",
		RawString: "447700900000@s.whatsapp.net",
	}
	assertpkg.Equal(t, "+447700900000", chatTitle(direct), "chatTitle(direct)")
}
