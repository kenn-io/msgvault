package imap

import (
	"fmt"
	"io"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Collect the SDK's parsed items without losing FLAGS presence. Its buffered
// collector uses nil for both an absent FLAGS item and an explicit FLAGS ().
// Sync needs that distinction to keep unknown provider read state unknown.
func collectObservedFetch(command *imapclient.FetchCommand) ([]*imapclient.FetchMessageBuffer, error) {
	defer func() { _ = command.Close() }()
	var messages []*imapclient.FetchMessageBuffer
	for message := command.Next(); message != nil; message = command.Next() {
		buffer := &imapclient.FetchMessageBuffer{SeqNum: message.SeqNum}
		for item := message.Next(); item != nil; item = message.Next() {
			switch value := item.(type) {
			case imapclient.FetchItemDataFlags:
				buffer.Flags = append([]imapapi.Flag{}, value.Flags...)
			case imapclient.FetchItemDataUID:
				buffer.UID = value.UID
			case imapclient.FetchItemDataModSeq:
				buffer.ModSeq = value.ModSeq
			case imapclient.FetchItemDataRFC822Size:
				buffer.RFC822Size = value.Size
			case imapclient.FetchItemDataInternalDate:
				buffer.InternalDate = value.Time
			case imapclient.FetchItemDataEnvelope:
				buffer.Envelope = value.Envelope
			case imapclient.FetchItemDataBodyStructure:
				buffer.BodyStructure = value.BodyStructure
			case imapclient.FetchItemDataBodySection:
				body, err := readObservedLiteral(value.Literal)
				if err != nil {
					return messages, err
				}
				buffer.BodySection = append(buffer.BodySection, imapclient.FetchBodySectionBuffer{Section: value.Section, Bytes: body})
			case imapclient.FetchItemDataBinarySection:
				body, err := readObservedLiteral(value.Literal)
				if err != nil {
					return messages, err
				}
				buffer.BinarySection = append(buffer.BinarySection, imapclient.FetchBinarySectionBuffer{Section: value.Section, Bytes: body})
			case imapclient.FetchItemDataBinarySectionSize:
				buffer.BinarySectionSize = append(buffer.BinarySectionSize, value)
			default:
				return messages, fmt.Errorf("unsupported observed FETCH item %T", item)
			}
		}
		messages = append(messages, buffer)
	}
	if err := command.Close(); err != nil {
		return messages, fmt.Errorf("finish observed IMAP FETCH: %w", err)
	}
	return messages, nil
}

func readObservedLiteral(literal imapapi.LiteralReader) ([]byte, error) {
	if literal == nil {
		return nil, nil
	}
	return io.ReadAll(literal)
}
