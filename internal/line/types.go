// Package line imports LINE message history from the iOS app's Core Data
// store, the Line.sqlite file an iPhone backup holds for the LINE app.
package line

import (
	"errors"
	"strings"
	"time"
)

const SourceType = "line"
const MessageType = "line"

// ErrNoOwner is returned when the store has no Keep Memo chat to name the
// account owner and --me was not given.
var ErrNoOwner = errors.New("line: the store does not identify your LINE account; pass --me <your LINE user ID>")

// contentKind classifies ZMESSAGE.ZCONTENTTYPE. LINE's protocol numbers the
// shared kinds (0 text, 1 image, 2 video, 3 audio, 5 flex, 6 call, 7 sticker,
// 13 contact, 14 file, 16 post, 17 rich, 18 chat event); the iOS client adds
// its own above 100 (101 unsent, 107 text with a link preview, 111 and 112
// grouped images).
type contentKind int

const (
	kindOther contentKind = iota
	kindText
	kindMedia
	kindSticker
	kindCall
	kindSystem
	kindUnsent
)

var contentKinds = map[int]contentKind{
	0: kindText, 5: kindText, 12: kindText, 17: kindText, 107: kindText,
	1: kindMedia, 2: kindMedia, 3: kindMedia, 14: kindMedia, 111: kindMedia, 112: kindMedia,
	7: kindSticker, 9: kindSticker,
	6:   kindCall,
	18:  kindSystem,
	101: kindUnsent,
}

// Rich messages (17) store a one-character placeholder in ZTEXT; their
// readable text is the ALT_TEXT entry of the content metadata.
const contentTypeRich = 17

// chatKind classifies ZCHAT.ZTYPE: 0 a direct chat with one user, 1 a room (an
// ad hoc multi-user chat), 2 a group, 100 the owner's own Keep Memo chat.
type chatKind int

const (
	chatUnknown chatKind = iota
	chatDirect
	chatRoom
	chatGroup
	chatSelf
)

var chatKinds = map[int]chatKind{0: chatDirect, 1: chatRoom, 2: chatGroup, 100: chatSelf}

func (k chatKind) conversationType() string {
	switch k {
	case chatDirect, chatSelf:
		return "direct_chat"
	case chatRoom, chatGroup:
		return "group_chat"
	case chatUnknown:
		return ""
	}
	return ""
}

// Chat is one row of ZCHAT with its members resolved.
type Chat struct {
	RowID      int64
	MID        string // LINE's chat identifier: u… for a user, r… for a room, c… for a group
	Kind       chatKind
	Title      string // the group's name, when the store knows it
	TitleKnown bool
	Members    []string // member MIDs, when the store lists them
}

// User is one row of ZUSER.
type User struct {
	RowID int64
	MID   string
	Name  string
}

// Message is one row of ZMESSAGE.
type Message struct {
	RowID       int64
	ID          string // LINE's server message id; empty for a send that never completed
	ContentType int
	SentAt      time.Time
	SenderRowID int64 // 0 when the owner sent it
	Text        string
	Metadata    metadata
}

func (m *Message) kind() contentKind {
	if m.Metadata.SystemType != "" {
		return kindSystem
	}
	return contentKinds[m.ContentType]
}

// body is the message text an archive reader would want, or "" for a row
// that carries none.
func (m *Message) body() string {
	if m.ContentType == contentTypeRich {
		return m.Metadata.AltText
	}
	return m.Text
}

// displayName picks the name the owner sees for a contact: their own
// nickname for the user first, then the address book match, then the
// user's profile name.
func displayName(custom, addressBook, profile string) string {
	for _, name := range []string{custom, addressBook, profile} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return ""
}
