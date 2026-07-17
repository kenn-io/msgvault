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
	entity, _ := gomessage.Read(bytes.NewReader(rawMIME))
	if entity == nil {
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
		var rfc822MessageID string
		if c.seenRFC822IDs != nil || c.msgIDToLabels != nil {
			rfc822MessageID = rawMIMEMessageID(rawMIME)
		}
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

// batchMailboxOrder returns the mailboxes sorted by name, except that
// allMailFolder (when present) sorts first so seenRFC822IDs is populated
// from the canonical source before checking Trash/Junk for duplicates.
func batchMailboxOrder(byMailbox map[string][]batchFetchItem, allMailFolder string) []string {
	order := make([]string, 0, len(byMailbox))
	for mb := range byMailbox {
		order = append(order, mb)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i] == allMailFolder || order[j] == allMailFolder {
			return order[i] == allMailFolder
		}
		return order[i] < order[j]
	})
	return order
}

// selectBatchMailbox selects the mailbox, reconnecting once on network
// errors. On non-fatal failure it marks all items with the error and
// returns ok=false so the caller can skip the mailbox; a non-nil error
// means reconnect failed and the whole batch should be abandoned.
func (c *Client) selectBatchMailbox(
	ctx context.Context,
	mailbox string,
	items []batchFetchItem,
	results []gmailapi.RawMessageBatchResult,
) (bool, error) {
	err := c.selectMailbox(mailbox)
	if err == nil {
		return true, nil
	}
	if isNetworkError(err) {
		c.logger.Warn("network error selecting mailbox, reconnecting", "mailbox", mailbox, "error", err)
		if reconErr := c.reconnect(ctx); reconErr != nil {
			return false, fmt.Errorf("reconnect failed fetching mailbox %q: %w", mailbox, reconErr)
		}
		err = c.selectMailbox(mailbox)
		if err == nil {
			return true, nil
		}
		c.logger.Warn("skipping mailbox batch after reconnect", "mailbox", mailbox, "error", err)
	} else {
		c.logger.Warn("skipping mailbox batch", "mailbox", mailbox, "error", err)
	}
	markRawBatchError(results, items, err)
	return false, nil
}

// fetchChunk runs one UID FETCH, reconnecting and retrying once on network
// errors. fatal reports that the connection could not be re-established and
// the whole batch should be abandoned; otherwise a non-nil error is local
// to this chunk.
func (c *Client) fetchChunk(
	ctx context.Context,
	mailbox string,
	uidSet imap.UIDSet,
	fetchOpts *imap.FetchOptions,
) (msgs []*imapclient.FetchMessageBuffer, fatal bool, err error) {
	msgs, err = c.conn.Fetch(uidSet, fetchOpts).Collect()
	if err == nil {
		return msgs, false, nil
	}
	if !isNetworkError(err) {
		c.logger.Warn("UID FETCH failed", "mailbox", mailbox, "error", err)
		return nil, false, fmt.Errorf("UID FETCH in mailbox %q: %w", mailbox, err)
	}
	c.logger.Warn("network error during UID FETCH, reconnecting", "mailbox", mailbox, "error", err)
	if reconErr := c.reconnect(ctx); reconErr != nil {
		return nil, true, fmt.Errorf("reconnect failed fetching chunk in mailbox %q: %w", mailbox, reconErr)
	}
	if selErr := c.selectMailbox(mailbox); selErr != nil {
		c.logger.Warn("mailbox reselect failed after reconnect", "mailbox", mailbox, "error", selErr)
		return nil, false, selErr
	}
	msgs, err = c.conn.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		c.logger.Warn("UID FETCH failed after reconnect", "mailbox", mailbox, "error", err)
		return nil, false, fmt.Errorf("UID FETCH after reconnect in mailbox %q: %w", mailbox, err)
	}
	return msgs, false, nil
}

// fetchMailboxBatch fetches all items of one mailbox in chunks of
// fetchChunkSize (huge UID FETCH commands time out on large mailboxes).
// When a chunk fails non-fatally, the chunk's items are marked with the
// error, the mailbox's remaining items are marked as skipped, and the
// mailbox is abandoned. A non-nil return error aborts the whole batch.
func (c *Client) fetchMailboxBatch(
	ctx context.Context,
	mailbox string,
	items []batchFetchItem,
	fetchOpts *imap.FetchOptions,
	results []gmailapi.RawMessageBatchResult,
) error {
	uidToIdx := make(map[imap.UID]int, len(items))
	for _, item := range items {
		uidToIdx[item.uid] = item.idx
	}

	for chunkStart := 0; chunkStart < len(items); chunkStart += fetchChunkSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		end := min(chunkStart+fetchChunkSize, len(items))
		chunk := items[chunkStart:end]

		var uidSet imap.UIDSet
		for _, item := range chunk {
			uidSet.AddNum(item.uid)
		}

		msgs, fatal, err := c.fetchChunk(ctx, mailbox, uidSet, fetchOpts)
		if fatal {
			return err
		}
		if err != nil {
			markRawBatchError(results, chunk, err)
			markRawBatchError(results, items[end:], errIMAPSkippedAfterChunkFailed)
			return nil
		}

		c.applyFetchResults(results, uidToIdx, mailbox, chunk, msgs)
	}
	return nil
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

	for _, mailbox := range batchMailboxOrder(byMailbox, c.allMailFolder) {
		items := byMailbox[mailbox]
		if ctx.Err() != nil {
			return results, ctx.Err()
		}

		ok, err := c.selectBatchMailbox(ctx, mailbox, items, results)
		if err != nil {
			return results, err
		}
		if !ok {
			continue
		}

		if err := c.fetchMailboxBatch(ctx, mailbox, items, fetchOpts, results); err != nil {
			return results, err
		}
	}
	return results, nil
}
