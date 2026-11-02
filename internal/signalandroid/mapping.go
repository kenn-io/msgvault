package signalandroid

import (
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/textimport"
)

// Signal Android message type bits (MessageTypes in the Signal Android
// source). The low five bits hold the base type; flags sit above them.
const (
	baseTypeMask       int64 = 0x1F
	baseInboxType      int64 = 20
	baseOutboxType     int64 = 21
	baseSendingType    int64 = 22
	baseSentType       int64 = 23
	baseSentFailedType int64 = 24

	keyExchangeMask       int64 = 0xFF00
	groupUpdateBit        int64 = 0x10000
	groupQuitBit          int64 = 0x20000
	expirationTimerBit    int64 = 0x40000
	endSessionBit         int64 = 0x400000
	pushMessageBit        int64 = 0x200000
	secureMessageBit      int64 = 0x800000
	specialTypesMask      int64 = 0xF00000000
	groupV2IDPrefix             = "__signal_group__v2__!"
	objectReplacementRune       = '\uFFFC'
	snippetRunes                = 100
)

// skipReason explains why a message row is not imported. Empty means the
// row is an ordinary Signal message.
func skipReason(m sigMessage) string {
	base := m.Type & baseTypeMask
	switch {
	case base != baseInboxType && base != baseOutboxType && base != baseSendingType &&
		base != baseSentType && base != baseSentFailedType:
		// Calls, profile and number changes, safety-number and other
		// conversation events, and drafts.
		return "event"
	case m.Type&(pushMessageBit|secureMessageBit) == 0:
		// Plain SMS and MMS kept by Signal as the default SMS app.
		return "sms"
	case m.Type&(groupUpdateBit|groupQuitBit|expirationTimerBit|endSessionBit|keyExchangeMask) != 0:
		return "event"
	case m.Type&specialTypesMask != 0:
		// Payments, gift badges, story reactions, pins and similar.
		return "event"
	case m.StoryType != 0 || m.ParentStoryID != 0:
		return "story"
	case m.RemoteDeleted != 0:
		return "deleted"
	case m.ScheduledDate > 0:
		return "scheduled"
	}
	return ""
}

// isOutgoing reports whether the message was sent by the account owner.
func isOutgoing(messageType int64) bool {
	switch messageType & baseTypeMask {
	case baseOutboxType, baseSendingType, baseSentType, baseSentFailedType:
		return true
	}
	return false
}

// isSent reports whether a message left its sender: every received message,
// and an own message only once Signal marked it sent.
func isSent(messageType int64) bool {
	switch messageType & baseTypeMask {
	case baseOutboxType, baseSendingType, baseSentFailedType:
		return false
	}
	return true
}

// normalizeACI returns the lower-case UUID of a Signal ACI, or "".
func normalizeACI(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "ACI:"), "aci:")
	value = strings.ToLower(value)
	if !isUUID(value) || value == "00000000-0000-0000-0000-000000000000" {
		return ""
	}
	return value
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
	}
	return true
}

// NormalizePhone returns the E.164 form of a phone number given with its
// country code, or "" for anything else. Import uses it for --phone.
func NormalizePhone(raw string) string {
	return normalizeE164(raw)
}

// normalizeE164 returns an E.164 phone number, or "" for anything else.
func normalizeE164(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "+") {
		return ""
	}
	phone, err := textimport.NormalizePhone(raw)
	if err != nil || !strings.HasPrefix(phone, "+") {
		return ""
	}
	return phone
}

// identityToken returns the recipient's stable identity: its ACI, or its
// E.164 number for a contact Signal knows only by number. Empty means the
// recipient has neither.
func identityToken(r sigRecipient) string {
	if r.ACI != "" {
		return r.ACI
	}
	return r.E164
}

// messageID returns the stable source message ID "<author>:<date_sent>".
func messageID(authorToken string, dateSent int64) string {
	return authorToken + ":" + strconv.FormatInt(dateSent, 10)
}

// conversationIdentity maps a chat recipient to its source conversation ID
// and type. ok is false for chats the importer does not read: legacy (v1)
// and MMS groups, story distribution lists and call links.
func conversationIdentity(r sigRecipient) (sourceConvID, convType string, ok bool) {
	if r.GroupID != "" {
		id, isV2 := groupV2Identifier(r.GroupID)
		if !isV2 {
			return "", "", false
		}
		return groupPrefix + id, "group_chat", true
	}
	if r.ACI != "" {
		return directACIPrefix + r.ACI, "direct_chat", true
	}
	if r.E164 != "" {
		return directE164Prefix + r.E164, "direct_chat", true
	}
	return "", "", false
}

// groupV2Identifier converts Signal's stored group ID
// ("__signal_group__v2__!<hex>") to unpadded base64url of the raw bytes.
func groupV2Identifier(stored string) (string, bool) {
	hexID, found := strings.CutPrefix(stored, groupV2IDPrefix)
	if !found || hexID == "" {
		return "", false
	}
	raw, err := hex.DecodeString(hexID)
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(raw), true
}

// applyMentions replaces Signal's mention placeholders (U+FFFC) with
// "@name", in the order the mentions appear in the body.
func applyMentions(body string, mentions []sigMention, nameOf func(int64) string) string {
	if len(mentions) == 0 || !strings.ContainsRune(body, objectReplacementRune) {
		return body
	}
	ordered := append([]sigMention(nil), mentions...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RangeStart < ordered[j].RangeStart })
	var b strings.Builder
	next := 0
	for _, r := range body {
		if r == objectReplacementRune && next < len(ordered) {
			b.WriteString("@" + nameOf(ordered[next].RecipientID))
			next++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// snippet returns the first 100 runes of text.
func snippet(text string) string {
	if utf8.RuneCountInString(text) <= snippetRunes {
		return text
	}
	return string([]rune(text)[:snippetRunes])
}

// attachmentMediaType classifies an attachment for msgvault's media views.
func attachmentMediaType(a sigAttachment) string {
	switch {
	case a.StickerPackID != "":
		return "sticker"
	case a.VoiceNote:
		return "voice_note"
	case a.VideoGIF:
		return "gif"
	}
	contentType := strings.ToLower(a.ContentType)
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image"
	case strings.HasPrefix(contentType, "video/"):
		return "video"
	case strings.HasPrefix(contentType, "audio/"):
		return "audio"
	}
	return "document"
}
