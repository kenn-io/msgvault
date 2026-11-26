package whatsapp

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"go.kenn.io/msgvault/internal/store"
)

const appleMediaJoin = "LEFT JOIN ZWAMEDIAITEM mi ON mi.Z_PK = m.ZMEDIAITEM"

// appleQuotedStanza extracts field 5 from the ZMETADATA protobuf. Read the
// whole blob so a truncated field after the quote cannot create a link.
func appleQuotedStanza(metadata []byte) string {
	var stanza []byte
	for len(metadata) > 0 {
		key, n := binary.Uvarint(metadata)
		field := key >> 3
		if n <= 0 || field == 0 || field > (1<<29)-1 {
			return ""
		}
		metadata = metadata[n:]
		var size int
		switch key & 7 {
		case 0:
			_, size = binary.Uvarint(metadata)
		case 1:
			size = 8
		case 2:
			length, width := binary.Uvarint(metadata)
			if width <= 0 || length > math.MaxInt {
				return ""
			}
			payloadSize := int(length)
			if payloadSize > len(metadata)-width {
				return ""
			}
			size = width + payloadSize
			if field == 5 {
				stanza = metadata[width:size]
			}
		case 5:
			size = 4
		default:
			return ""
		}
		if size <= 0 || size > len(metadata) {
			return ""
		}
		metadata = metadata[size:]
	}
	for _, char := range stanza {
		if char < '!' || char > '~' {
			return ""
		}
	}
	return string(stanza)
}

type appleReply struct {
	messageID int64
	targetID  int64
	stanzaID  string
}

// appendAppleReplies uses archived targets from the current batch first.
// Keep the rest until later pages and chats have been stored.
func appendAppleReplies(
	replies []appleReply, messages []appleMessage, stored map[string]store.StoredMessage,
	duplicateStanzas map[string]struct{},
) []appleReply {
	for _, message := range messages {
		quoted := message.QuotedStanzaID
		if _, duplicate := duplicateStanzas[quoted]; duplicate || quoted == message.StanzaID {
			continue
		}
		archived := stored[message.StanzaID]
		if archived.ReplyToMessageID.Valid {
			continue
		}
		reply := appleReply{messageID: archived.ID, stanzaID: quoted}
		if target := stored[quoted]; target.Raw != nil {
			reply.targetID = target.ID
		}
		replies = append(replies, reply)
	}
	return replies
}

func (imp *Importer) linkAppleReplies(ctx context.Context, sourceID int64, replies []appleReply) error {
	var pending []string
	for _, reply := range replies {
		if reply.targetID == 0 {
			pending = append(pending, reply.stanzaID)
		}
	}
	targets, err := imp.store.StoredMessagesContext(ctx, sourceID, appleRawFormat, pending)
	if err != nil {
		return fmt.Errorf("load Apple reply targets: %w", err)
	}
	for _, reply := range replies {
		targetID := reply.targetID
		if targetID == 0 {
			target := targets[reply.stanzaID]
			// A same-source message imported from Android does not establish
			// that the quoted stanza was imported from ChatStorage.sqlite.
			if target.Raw == nil {
				continue
			}
			targetID = target.ID
		}
		if err := imp.store.SetMessageReplyContext(ctx, reply.messageID, targetID); err != nil {
			return fmt.Errorf("link Apple reply: %w", err)
		}
	}
	return nil
}
