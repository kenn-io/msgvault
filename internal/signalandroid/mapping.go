package signalandroid

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/textimport"
)

// Signal Android message type bits (MessageTypes in the Signal Android
// source). The low five bits hold the base type; flags sit above them.
const (
	baseTypeMask           int64 = 0x1F
	baseInboxType          int64 = 20
	baseOutboxType         int64 = 21
	baseSendingType        int64 = 22
	baseSentType           int64 = 23
	baseSentFailedType     int64 = 24
	baseSendingSkippedType int64 = 28

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
		base != baseSentType && base != baseSentFailedType && base != baseSendingSkippedType:
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
	case baseOutboxType, baseSendingType, baseSentType, baseSentFailedType, baseSendingSkippedType:
		return true
	}
	return false
}

// isSent reports whether a message left its sender: every received message,
// and an own message only once Signal marked it sent.
func isSent(messageType int64) bool {
	switch messageType & baseTypeMask {
	case baseOutboxType, baseSendingType, baseSentFailedType, baseSendingSkippedType:
		return false
	}
	return true
}

// normalizeACI returns the lower-case UUID of a Signal ACI, or "".
func normalizeACI(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(strings.TrimPrefix(value, "ACI:"), "aci:")
	return normalizeUUID(value)
}

// normalizePNI accepts both the bare UUID and the PNI-prefixed format.
func normalizePNI(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.TrimPrefix(value, "pni:")
	return normalizeUUID(value)
}

func normalizeUUID(value string) string {
	value = strings.ToLower(value)
	if len(value) != 36 || uuid.Validate(value) != nil || value == "00000000-0000-0000-0000-000000000000" {
		return ""
	}
	return value
}

// NormalizePhone returns the E.164 form of a phone number given with its
// country code, or "" for anything else. Import uses it for --phone.
func NormalizePhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "+") {
		return ""
	}
	phone, err := textimport.NormalizePhone(raw)
	if err != nil {
		return ""
	}
	return phone
}

// messageID includes the stable chat identity, author, and original sent time.
func messageID(chatID, authorToken string, dateSent int64) string {
	return chatID + ":" + authorToken + ":" + strconv.FormatInt(dateSent, 10)
}

// conversationIdentity maps a chat recipient to its source conversation ID
// and type. ok is false for chats the importer does not read: MMS groups,
// story distribution lists and call links.
func conversationIdentity(r sigRecipient, archived map[string]bool) (sourceConvID, convType string, ok bool) {
	if r.GroupID != "" {
		return groupIdentity(r.GroupID)
	}
	if r.ACI != "" && archived[directACIPrefix+r.ACI] {
		return directACIPrefix + r.ACI, "direct_chat", true
	}
	if r.PNI != "" && archived[directPNIPrefix+r.PNI] {
		return directPNIPrefix + r.PNI, "direct_chat", true
	}
	if r.ACI != "" {
		return directACIPrefix + r.ACI, "direct_chat", true
	}
	if r.PNI != "" {
		return directPNIPrefix + r.PNI, "direct_chat", true
	}
	return "", "", false
}

func groupIdentity(stored string) (string, string, bool) {
	if id, ok := groupIdentifier(stored, groupV2IDPrefix); ok {
		return groupPrefix + id, "group_chat", true
	}
	id, ok := groupIdentifier(stored, "__textsecure_group__!")
	if !ok {
		return "", "", false
	}
	return "signal:group-v1:" + id, "group_chat", true
}

// groupIdentifier converts Signal's stored group ID with the given prefix
// to unpadded base64url of the raw bytes.
func groupIdentifier(stored, prefix string) (string, bool) {
	hexID, found := strings.CutPrefix(stored, prefix)
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
// "@name" at their recorded UTF-16 offsets.
func applyMentions(body string, mentions []sigMention, nameOf func(int64) string) string {
	if len(mentions) == 0 || !strings.ContainsRune(body, objectReplacementRune) {
		return body
	}
	var b strings.Builder
	next := 0
	var offset int64
	for _, r := range body {
		for next < len(mentions) && mentions[next].RangeStart < offset {
			next++
		}
		if r == objectReplacementRune && next < len(mentions) && mentions[next].RangeStart == offset {
			b.WriteString("@" + nameOf(mentions[next].RecipientID))
			next++
		} else {
			b.WriteRune(r)
		}
		offset += int64(utf16.RuneLen(r))
	}
	return b.String()
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
	return attachmentpolicy.MediaType(strings.ToLower(a.ContentType))
}
