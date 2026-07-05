package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

var errIMAPRawBodyMissing = errors.New("IMAP fetch result did not include raw body")
var errIMAPFetchResultMissing = errors.New("IMAP fetch result missing from response")
var errIMAPSkippedAfterChunkFailed = errors.New("IMAP fetch skipped after earlier chunk failure")

type batchFetchItem struct {
	idx int
	uid imap.UID
}

func newRawBatchResults(messageIDs []string) []gmailapi.RawMessageBatchResult {
	results := make([]gmailapi.RawMessageBatchResult, len(messageIDs))
	for i, id := range messageIDs {
		results[i].ID = id
	}
	return results
}

func rawBatchMessages(results []gmailapi.RawMessageBatchResult) []*gmailapi.RawMessage {
	messages := make([]*gmailapi.RawMessage, len(results))
	for i, result := range results {
		messages[i] = result.Message
	}
	return messages
}

func markRawBatchError(results []gmailapi.RawMessageBatchResult, items []batchFetchItem, err error) {
	for _, item := range items {
		results[item.idx].Err = err
	}
}

func rawBatchFetchOptions() *imap.FetchOptions {
	return &imap.FetchOptions{
		UID:          true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection:  []*imap.FetchItemBodySection{{Peek: true}}, // BODY.PEEK[] to avoid marking \Seen
	}
}

func rawMIMEMessageID(rawMIME []byte) string {
	entity, err := gomessage.Read(bytes.NewReader(rawMIME))
	if err != nil {
		return ""
	}
	header := gomail.Header{Header: entity.Header}
	msgID, err := header.MessageID()
	if err != nil {
		return ""
	}
	return msgID
}

func (c *Client) applyFetchResults(
	results []gmailapi.RawMessageBatchResult,
	uidToIdx map[imap.UID]int,
	mailbox string,
	chunk []batchFetchItem,
	msgs []*imapclient.FetchMessageBuffer,
) {
	seenReturnedUIDs := make(map[imap.UID]bool, len(msgs))
	for _, msgBuf := range msgs {
		idx, ok := uidToIdx[msgBuf.UID]
		if !ok {
			continue
		}
		seenReturnedUIDs[msgBuf.UID] = true

		var rawMIME []byte
		if len(msgBuf.BodySection) > 0 {
			rawMIME = msgBuf.BodySection[0].Bytes
		}
		if len(rawMIME) == 0 {
			results[idx].Message = nil
			results[idx].Err = errIMAPRawBodyMissing
			continue
		}

		// Dedup by RFC822 Message-ID when listing All Mail alongside
		// Trash/Spam. On Gmail these are disjoint, but non-Gmail servers may
		// overlap. Return a non-nil stub with empty Raw so the caller treats
		// this as a skip, not a fetch error.
		msgID := compositeID(mailbox, msgBuf.UID)
		rfc822MessageID := rawMIMEMessageID(rawMIME)
		if c.seenRFC822IDs != nil &&
			rfc822MessageID != "" {
			if c.seenRFC822IDs[rfc822MessageID] {
				results[idx].Message = &gmailapi.RawMessage{ID: msgID}
				results[idx].Err = nil
				continue
			}
			c.seenRFC822IDs[rfc822MessageID] = true
		}

		labels := []string{mailbox}

		// Merge labels from other mailboxes via the label map built during
		// listing. The map keys on RFC822 Message-ID and maps to the other
		// mailbox names the message appears in. Skip the current mailbox to
		// avoid duplicates that would violate the message_labels primary key.
		if c.msgIDToLabels != nil &&
			rfc822MessageID != "" {
			if extra, ok := c.msgIDToLabels[rfc822MessageID]; ok {
				for _, lbl := range extra {
					if lbl != mailbox {
						labels = append(labels, lbl)
					}
				}
			}
		}

		results[idx].Message = &gmailapi.RawMessage{
			ID:           msgID,
			ThreadID:     msgID,
			LabelIDs:     labels,
			InternalDate: msgBuf.InternalDate.UnixMilli(),
			SizeEstimate: msgBuf.RFC822Size,
			Raw:          rawMIME,
		}
		results[idx].Err = nil
	}

	for _, item := range chunk {
		if seenReturnedUIDs[item.uid] {
			continue
		}
		if results[item.idx].Message == nil && results[item.idx].Err == nil {
			results[item.idx].Err = errIMAPFetchResultMissing
		}
	}
}

// GetMessagesRawBatchWithErrors fetches multiple messages, grouping by mailbox for efficiency.
// Results are returned in the same order as messageIDs with per-message fetch errors preserved.
//
// UIDs per mailbox are fetched in chunks of fetchChunkSize to avoid huge FETCH
// commands that time out on large mailboxes. On network errors the connection is
// re-established and the failed chunk is retried once; if reconnect itself fails
// the function returns immediately with whatever results were collected.
func (c *Client) GetMessagesRawBatchWithErrors(ctx context.Context, messageIDs []string) ([]gmailapi.RawMessageBatchResult, error) {
	results := newRawBatchResults(messageIDs)

	byMailbox := make(map[string][]batchFetchItem, 4)
	for i, id := range messageIDs {
		mailbox, uid, err := parseCompositeID(id)
		if err != nil {
			c.logger.Warn("invalid message ID in batch", "id", id, "error", err)
			results[i].Err = err
			continue
		}
		byMailbox[mailbox] = append(byMailbox[mailbox], batchFetchItem{i, uid})
	}

	fetchOpts := rawBatchFetchOptions()

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	// Process allMailFolder first so seenRFC822IDs is populated from
	// the canonical source before checking Trash/Junk for duplicates.
	mailboxOrder := make([]string, 0, len(byMailbox))
	for mb := range byMailbox {
		mailboxOrder = append(mailboxOrder, mb)
	}
	sort.Strings(mailboxOrder)
	if c.allMailFolder != "" {
		for i, mb := range mailboxOrder {
			if mb == c.allMailFolder {
				mailboxOrder = append(
					append([]string{mb}, mailboxOrder[:i]...),
					mailboxOrder[i+1:]...,
				)
				break
			}
		}
	}

	for _, mailbox := range mailboxOrder {
		items := byMailbox[mailbox]
		if ctx.Err() != nil {
			return results, ctx.Err()
		}

		if err := c.selectMailbox(mailbox); err != nil {
			if isNetworkError(err) {
				c.logger.Warn("network error selecting mailbox, reconnecting", "mailbox", mailbox, "error", err)
				if reconErr := c.reconnect(ctx); reconErr != nil {
					return results, fmt.Errorf("reconnect failed fetching mailbox %q: %w", mailbox, reconErr)
				}
				if err := c.selectMailbox(mailbox); err != nil {
					c.logger.Warn("skipping mailbox batch after reconnect", "mailbox", mailbox, "error", err)
					markRawBatchError(results, items, err)
					continue
				}
			} else {
				c.logger.Warn("skipping mailbox batch", "mailbox", mailbox, "error", err)
				markRawBatchError(results, items, err)
				continue
			}
		}

		// Build UID→result-index map for all items in this mailbox.
		uidToIdx := make(map[imap.UID]int, len(items))
		for _, item := range items {
			uidToIdx[item.uid] = item.idx
		}

		// Fetch in chunks to avoid huge UID FETCH commands that time out on
		// large mailboxes.
	chunkLoop:
		for chunkStart := 0; chunkStart < len(items); chunkStart += fetchChunkSize {
			if ctx.Err() != nil {
				return results, ctx.Err()
			}

			chunk := items[chunkStart:]
			if len(chunk) > fetchChunkSize {
				chunk = chunk[:fetchChunkSize]
			}

			var uidSet imap.UIDSet
			for _, item := range chunk {
				uidSet.AddNum(item.uid)
			}

			msgs, err := c.conn.Fetch(uidSet, fetchOpts).Collect()
			if err != nil {
				if isNetworkError(err) {
					c.logger.Warn("network error during UID FETCH, reconnecting", "mailbox", mailbox, "error", err)
					if reconErr := c.reconnect(ctx); reconErr != nil {
						return results, fmt.Errorf("reconnect failed fetching chunk in mailbox %q: %w", mailbox, reconErr)
					}
					if selErr := c.selectMailbox(mailbox); selErr != nil {
						c.logger.Warn("skipping remaining chunks after reconnect", "mailbox", mailbox, "error", selErr)
						markRawBatchError(results, chunk, selErr)
						if chunkStart+len(chunk) < len(items) {
							markRawBatchError(results, items[chunkStart+len(chunk):], errIMAPSkippedAfterChunkFailed)
						}
						break chunkLoop
					}
					msgs, err = c.conn.Fetch(uidSet, fetchOpts).Collect()
					if err != nil {
						c.logger.Warn("UID FETCH failed after reconnect", "mailbox", mailbox, "error", err)
						markRawBatchError(results, chunk, err)
						if chunkStart+len(chunk) < len(items) {
							markRawBatchError(results, items[chunkStart+len(chunk):], errIMAPSkippedAfterChunkFailed)
						}
						break chunkLoop
					}
				} else {
					c.logger.Warn("UID FETCH failed", "mailbox", mailbox, "error", err)
					markRawBatchError(results, chunk, err)
					if chunkStart+len(chunk) < len(items) {
						markRawBatchError(results, items[chunkStart+len(chunk):], errIMAPSkippedAfterChunkFailed)
					}
					break chunkLoop
				}
			}

			c.applyFetchResults(results, uidToIdx, mailbox, chunk, msgs)
		}
	}
	return results, nil
}
