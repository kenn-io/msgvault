package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

var errIMAPRawBodyMissing = errors.New("IMAP fetch result did not include raw body")
var errIMAPFetchResultMissing = fmt.Errorf(
	"IMAP fetch result missing from response: %w", gmailapi.ErrMessageGone)

// errIMAPLabelBodyMissing is the label-fetch counterpart of
// errIMAPRawBodyMissing. The server returned the UID, so the message is still
// in the mailbox and only its headers are missing. That is a fetch failure,
// not the expunge race, and it must not reach gmailapi.ErrMessageGone: a run
// that acknowledged it would drop a live message from an authoritative
// snapshot.
var errIMAPLabelBodyMissing = errors.New("IMAP fetch result did not include message headers")
var errIMAPSkippedAfterChunkFailed = errors.New("IMAP fetch skipped after earlier chunk failure")

// errIMAPOmittedButPresent marks a UID that no FETCH response returned and
// that a UID SEARCH then reported as still in the mailbox. The message is
// there, so this is a fetch failure, and it must not reach
// gmailapi.ErrMessageGone: acknowledging it would drop a live message.
var errIMAPOmittedButPresent = errors.New(
	"IMAP fetch returned no result for a UID the mailbox still reports")

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
		Flags:        true,
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

func normalizeRFC822MessageID(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
		return value[1 : len(value)-1]
	}
	return value
}

func (c *Client) labelsForMessage(mailbox, rfc822MessageID string) []string {
	labels := []string{mailbox}
	if c.msgIDToLabels == nil || rfc822MessageID == "" {
		return labels
	}

	for _, label := range c.msgIDToLabels[rfc822MessageID] {
		if label != mailbox {
			labels = append(labels, label)
		}
	}
	return labels
}

func (c *Client) applyFetchResults(
	results []gmailapi.RawMessageBatchResult,
	uidToIdx map[imap.UID]int,
	mailbox string,
	chunk []batchFetchItem,
	msgs []*imapclient.FetchMessageBuffer,
) []batchFetchItem {
	seenReturnedUIDs := make(map[imap.UID]bool, len(msgs))
	// Only a message with no Message-ID header needs a durable alias, so the
	// chunk's aliases are read once, and only if one turns up.
	loadAliases := sync.OnceFunc(func() {
		uids := make([]imap.UID, len(chunk))
		for i, item := range chunk {
			uids[i] = item.uid
		}
		c.loadSourceMessageAliases(mailbox, uids)
	})
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

		// Dedup by RFC822 Message-ID when fully enumerated mailboxes overlap.
		// Return a non-nil stub with empty Raw so the caller treats this as a
		// skip, not a fetch error.
		msgID := compositeID(mailbox, msgBuf.UID)
		rfc822MessageID := rawMIMEMessageID(rawMIME)
		canonicalSourceMessageID := ""
		var rawSHA256 [32]byte
		if rfc822MessageID == "" && c.preferredRawSourceIDs != nil {
			rawSHA256 = sha256.Sum256(rawMIME)
			if mailbox == c.allMailFolder {
				// A canonical-mailbox UID is its own durable identity. Raw
				// matching is only a fallback for copies in secondary mailboxes;
				// using it here would collapse distinct byte-identical messages.
				canonicalSourceMessageID = msgID
				if canonical, exists := c.preferredRawSourceIDs[rawSHA256]; !exists {
					c.preferredRawSourceIDs[rawSHA256] = msgID
				} else if canonical != msgID {
					// An empty value marks a digest shared by distinct canonical
					// messages. Secondary copies cannot safely choose either one.
					c.preferredRawSourceIDs[rawSHA256] = ""
				}
			} else {
				priorState, sameMailboxEpoch := c.priorFolderStates[mailbox]
				if sameMailboxEpoch && priorState.UIDValidity == c.selectedUIDValidity {
					loadAliases()
					canonicalSourceMessageID = c.sourceMessageAliases[msgID]
				}
				if canonicalSourceMessageID == "" {
					canonicalSourceMessageID = c.preferredRawSourceIDs[rawSHA256]
				}
			}
		}
		c.recordMembershipLocked(
			mailbox, msgBuf.UID, canonicalSourceMessageID, rfc822MessageID,
			rawSHA256, msgBuf.RFC822Size, msgBuf.Flags)
		if canonicalSourceMessageID != "" && canonicalSourceMessageID != msgID {
			results[idx].Message = &gmailapi.RawMessage{ID: msgID}
			results[idx].Err = nil
			continue
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

		// Merge labels from other mailboxes via the label map built during
		// listing. The map keys on RFC822 Message-ID and maps to the other
		// mailbox names the message appears in. Skip the current mailbox to
		// avoid duplicates that would violate the message_labels primary key.
		labels := c.labelsForMessage(mailbox, rfc822MessageID)

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

	var omitted []batchFetchItem
	for _, item := range chunk {
		if seenReturnedUIDs[item.uid] {
			continue
		}
		if results[item.idx].Message == nil && results[item.idx].Err == nil {
			omitted = append(omitted, item)
		}
	}
	return omitted
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

		omitted := c.applyFetchResults(results, uidToIdx, mailbox, chunk, msgs)
		if len(omitted) > 0 {
			var fatalErr error
			omitted, fatalErr = c.recheckOmittedRaw(
				ctx, results, uidToIdx, mailbox, omitted, fetchOpts)
			if fatalErr != nil {
				return fatalErr
			}
		}
		c.markOmittedOutcome(mailbox, omitted,
			func(item batchFetchItem, err error) { results[item.idx].Err = err })
	}
	return nil
}

// recheckOmittedRaw re-asks the server for the UIDs a chunk's FETCH response
// left out, and returns the ones it leaves out a second time.
//
// A UID missing from a FETCH response is not an expunge notice. The server can
// drop a UID for its own reasons, and believing the first omission loses the
// message outright on a QRESYNC account: the run acknowledges the UID, stores
// no membership for it, and still advances HIGHESTMODSEQ, so no later
// CHANGEDSINCE fetch has any reason to report it. Only the paths that
// reconcile a mailbox by message count recover on their own.
//
// The recheck costs one extra FETCH for a chunk that had an omission, and
// nothing at all for a chunk that did not. When the recheck itself fails
// nothing was learned, so the UIDs are recorded as fetch errors rather than
// absences, which holds the commit back instead of acting on a guess.
func (c *Client) recheckOmittedRaw(
	ctx context.Context,
	results []gmailapi.RawMessageBatchResult,
	uidToIdx map[imap.UID]int,
	mailbox string,
	omitted []batchFetchItem,
	fetchOpts *imap.FetchOptions,
) ([]batchFetchItem, error) {
	var uidSet imap.UIDSet
	for _, item := range omitted {
		uidSet.AddNum(item.uid)
	}
	msgs, fatal, err := c.fetchChunk(ctx, mailbox, uidSet, fetchOpts)
	if fatal {
		// The reconnect failed, so there is no connection left to run the next
		// chunk on. The batch has to end here, exactly as it does when the
		// first fetch of a chunk hits this.
		return nil, err
	}
	if err != nil {
		c.logger.Warn("could not recheck UIDs missing from a FETCH response",
			"mailbox", mailbox, "uids", len(omitted), "error", err)
		markRawBatchError(results, omitted, err)
		return nil, nil
	}
	return c.applyFetchResults(results, uidToIdx, mailbox, omitted, msgs), nil
}

// confirmOmittedPresent asks the server which of the UIDs it left out of both
// FETCH attempts it still holds.
//
// Two omissions are still two answers to the same question: both attempts are
// the same command against the same mailbox, so a server that drops a UID for
// a structural reason drops it from the retry as well. UID SEARCH is different
// evidence, and it costs one command for the whole set. A UID it reports is a
// live message, and calling that gone lets the run acknowledge it, forget its
// membership and advance the mailbox cursor past it.
//
// The search names the omitted UIDs exactly and carries none of the
// since/before filters enumerateMailbox applies: a date filter here would hide
// a live message behind the same silence this exists to distrust.
//
// An error means nothing was learned. Callers must then record a fetch error
// rather than an absence, which holds the commit back instead of acting on a
// guess.
func (c *Client) confirmOmittedPresent(
	mailbox string, omitted []imap.UID,
) (map[imap.UID]bool, error) {
	present := make(map[imap.UID]bool, len(omitted))
	if len(omitted) == 0 {
		return present, nil
	}

	var uidSet imap.UIDSet
	for _, uid := range omitted {
		uidSet.AddNum(uid)
	}
	searchData, err := c.conn.UIDSearch(
		&imap.SearchCriteria{UID: []imap.UIDSet{uidSet}}, nil).Wait()
	if err != nil {
		c.logger.Warn("could not confirm UIDs missing from a FETCH response",
			"mailbox", mailbox, "uids", len(omitted), "error", err)
		return nil, fmt.Errorf(
			"UID SEARCH confirming UIDs missing from a FETCH in mailbox %q: %w",
			mailbox, err)
	}

	for _, uid := range searchData.AllUIDs() {
		present[uid] = true
	}
	return present, nil
}

// markOmittedOutcome records what confirmOmittedPresent established about the
// UIDs no FETCH response returned, and forgets the membership observed during
// enumeration for the ones the mailbox no longer reports.
func (c *Client) markOmittedOutcome(
	mailbox string,
	omitted []batchFetchItem,
	setErr func(item batchFetchItem, err error),
) {
	uids := make([]imap.UID, len(omitted))
	for i, item := range omitted {
		uids[i] = item.uid
	}
	present, searchErr := c.confirmOmittedPresent(mailbox, uids)
	for _, item := range omitted {
		switch {
		case searchErr != nil:
			setErr(item, searchErr)
		case present[item.uid]:
			setErr(item, errIMAPOmittedButPresent)
		default:
			setErr(item, errIMAPFetchResultMissing)
			c.forgetMembershipLocked(mailbox, item.uid)
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

func newLabelBatchResults(messageIDs []string) []gmailapi.MessageLabelsBatchResult {
	results := make([]gmailapi.MessageLabelsBatchResult, len(messageIDs))
	for i, id := range messageIDs {
		results[i].ID = id
	}
	return results
}

func markLabelBatchError(results []gmailapi.MessageLabelsBatchResult, items []batchFetchItem, err error) {
	for _, item := range items {
		results[item.idx].Err = err
	}
}

func (c *Client) applyLabelFetchResults(
	results []gmailapi.MessageLabelsBatchResult,
	uidToIdx map[imap.UID]int,
	mailbox string,
	chunk []batchFetchItem,
	msgs []*imapclient.FetchMessageBuffer,
) []batchFetchItem {
	seenUIDs := make(map[imap.UID]bool, len(msgs))
	for _, msgBuf := range msgs {
		idx, ok := uidToIdx[msgBuf.UID]
		if !ok {
			continue
		}
		seenUIDs[msgBuf.UID] = true
		// An empty section is as unreadable as an absent one, and the UID was
		// returned either way, so this is a fetch failure and not an absence.
		if len(msgBuf.BodySection) == 0 || len(msgBuf.BodySection[0].Bytes) == 0 {
			results[idx].Err = errIMAPLabelBodyMissing
			continue
		}
		rfc822MessageID := rawMIMEMessageID(msgBuf.BodySection[0].Bytes)
		c.recordMembershipLocked(
			mailbox, msgBuf.UID, "", rfc822MessageID, [32]byte{}, 0, msgBuf.Flags)
		results[idx].LabelIDs = c.labelsForMessage(mailbox, rfc822MessageID)
		results[idx].RFC822MessageID = rfc822MessageID
		results[idx].Err = nil
	}

	var omitted []batchFetchItem
	for _, item := range chunk {
		if !seenUIDs[item.uid] && results[item.idx].Err == nil {
			omitted = append(omitted, item)
		}
	}
	return omitted
}

func (c *Client) selectLabelBatchMailbox(
	ctx context.Context,
	mailbox string,
	items []batchFetchItem,
	results []gmailapi.MessageLabelsBatchResult,
) (bool, error) {
	err := c.selectMailbox(mailbox)
	if err == nil {
		return true, nil
	}
	if isNetworkError(err) {
		c.logger.Warn("network error selecting mailbox for label fetch, reconnecting",
			"mailbox", mailbox, "error", err)
		if reconErr := c.reconnect(ctx); reconErr != nil {
			return false, fmt.Errorf("reconnect failed fetching labels from mailbox %q: %w", mailbox, reconErr)
		}
		err = c.selectMailbox(mailbox)
		if err == nil {
			return true, nil
		}
		c.logger.Warn("skipping mailbox label batch after reconnect",
			"mailbox", mailbox, "error", err)
	} else {
		c.logger.Warn("skipping mailbox label batch", "mailbox", mailbox, "error", err)
	}
	markLabelBatchError(results, items, err)
	return false, nil
}

func (c *Client) fetchMailboxLabelBatch(
	ctx context.Context,
	mailbox string,
	items []batchFetchItem,
	fetchOpts *imap.FetchOptions,
	results []gmailapi.MessageLabelsBatchResult,
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
			markLabelBatchError(results, chunk, err)
			markLabelBatchError(results, items[end:], errIMAPSkippedAfterChunkFailed)
			return nil
		}
		omitted := c.applyLabelFetchResults(results, uidToIdx, mailbox, chunk, msgs)
		if len(omitted) > 0 {
			var fatalErr error
			omitted, fatalErr = c.recheckOmittedLabels(
				ctx, results, uidToIdx, mailbox, omitted, fetchOpts)
			if fatalErr != nil {
				return fatalErr
			}
		}
		c.markOmittedOutcome(mailbox, omitted,
			func(item batchFetchItem, err error) { results[item.idx].Err = err })
	}
	return nil
}

// recheckOmittedLabels is recheckOmittedRaw for the label fetch. See that
// function for why one omission is not enough to call a message gone.
func (c *Client) recheckOmittedLabels(
	ctx context.Context,
	results []gmailapi.MessageLabelsBatchResult,
	uidToIdx map[imap.UID]int,
	mailbox string,
	omitted []batchFetchItem,
	fetchOpts *imap.FetchOptions,
) ([]batchFetchItem, error) {
	var uidSet imap.UIDSet
	for _, item := range omitted {
		uidSet.AddNum(item.uid)
	}
	msgs, fatal, err := c.fetchChunk(ctx, mailbox, uidSet, fetchOpts)
	if fatal {
		return nil, err
	}
	if err != nil {
		c.logger.Warn("could not recheck UIDs missing from a label FETCH response",
			"mailbox", mailbox, "uids", len(omitted), "error", err)
		markLabelBatchError(results, omitted, err)
		return nil, nil
	}
	return c.applyLabelFetchResults(results, uidToIdx, mailbox, omitted, msgs), nil
}

// GetMessageLabelsBatch fetches only the Message-ID header needed to recover
// mailbox memberships for existing messages. It deliberately avoids BODY[]
// so rescans can refresh labels without downloading message bodies or
// attachments again. Per-message failures do not abort the rest of the batch.
func (c *Client) GetMessageLabelsBatch(ctx context.Context, messageIDs []string) ([]gmailapi.MessageLabelsBatchResult, error) {
	results := newLabelBatchResults(messageIDs)
	byMailbox := make(map[string][]batchFetchItem, 4)
	for i, id := range messageIDs {
		mailbox, uid, err := parseCompositeID(id)
		if err != nil {
			results[i].Err = err
			continue
		}
		byMailbox[mailbox] = append(byMailbox[mailbox], batchFetchItem{idx: i, uid: uid})
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(ctx); err != nil {
		return nil, err
	}

	fetchOpts := messageIDHeaderFetchOptions()
	for _, mailbox := range batchMailboxOrder(byMailbox, c.allMailFolder) {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}

		items := byMailbox[mailbox]
		ok, err := c.selectLabelBatchMailbox(ctx, mailbox, items, results)
		if err != nil {
			return results, err
		}
		if !ok {
			continue
		}

		if err := c.fetchMailboxLabelBatch(ctx, mailbox, items, fetchOpts, results); err != nil {
			return results, err
		}
	}

	return results, nil
}

func (c *Client) sourceMessageMetadata(
	ctx context.Context, messageID string,
) (string, bool, error) {
	invalidated, err := c.sourceMessageIDInvalidated(messageID)
	if err != nil {
		return "", false, err
	}
	if invalidated {
		return "", false, nil
	}

	results, err := c.GetMessageLabelsBatch(ctx, []string{messageID})
	if err != nil {
		return "", false, err
	}
	if len(results) != 1 {
		return "", false, fmt.Errorf(
			"source message validation returned %d results", len(results))
	}
	if results[0].Err != nil {
		// Only an absent UID is definitive absence. A returned UID whose
		// headers are missing is a live message, and reporting it as absent
		// would let SourceMessageMatches conclude a mismatch and rekey it.
		if errors.Is(results[0].Err, errIMAPFetchResultMissing) {
			return "", false, nil
		}
		return "", false, results[0].Err
	}
	return results[0].RFC822MessageID, true, nil
}

func (c *Client) sourceMessageIDInvalidated(
	messageID string,
) (bool, error) {
	matches, known, err := c.sourceMessageIDEpochMatches(messageID)
	return known && !matches, err
}

func (c *Client) sourceMessageIDEpochMatches(
	messageID string,
) (matches bool, known bool, err error) {
	mailbox, _, err := parseCompositeID(messageID)
	if err != nil {
		return false, false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mailboxCache != nil &&
		!slices.Contains(c.mailboxCache, mailbox) {
		return false, true, nil
	}
	prior, hasPrior := c.priorFolderStates[mailbox]
	observed, hasObserved := c.observedFolderStates[mailbox]
	if !hasPrior || !hasObserved {
		return false, false, nil
	}
	return prior.UIDValidity == observed.UIDValidity, true, nil
}

// SeedValidatedMessageDedup records a fetched RFC822 identity only after sync
// has validated the exact source ID and reconciled its labels.
func (c *Client) SeedValidatedMessageDedup(
	messageID, rfc822MessageID string,
) error {
	mailbox, _, err := parseCompositeID(messageID)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	seedDedup := mailbox == c.allMailFolder ||
		(c.allMailFolder == "" && c.labelMapComplete)
	if seedDedup &&
		c.seenRFC822IDs != nil &&
		rfc822MessageID != "" {
		c.seenRFC822IDs[rfc822MessageID] = true
	}
	return nil
}

// SourceMessageExists reports whether an exact mailbox|uid identifier still
// exists. A missing FETCH result is definitive absence; all other failures are
// returned so callers can retry without replacing a canonical identifier.
func (c *Client) SourceMessageExists(
	ctx context.Context, messageID string,
) (bool, error) {
	_, exists, err := c.sourceMessageMetadata(ctx, messageID)
	return exists, err
}

// FetchedSourceMessageMatches validates identity metadata already fetched for
// an exact source ID, including the mailbox's UIDVALIDITY epoch.
func (c *Client) FetchedSourceMessageMatches(
	messageID, expectedRFC822MessageID, actualRFC822MessageID string,
) (matches bool, conclusive bool, err error) {
	epochMatches, epochKnown, err := c.sourceMessageIDEpochMatches(messageID)
	if err != nil {
		return false, false, err
	}
	if epochKnown && !epochMatches {
		return false, true, nil
	}

	expected := normalizeRFC822MessageID(expectedRFC822MessageID)
	actual := normalizeRFC822MessageID(actualRFC822MessageID)
	if expected != "" && actual != "" {
		return expected == actual, true, nil
	}
	if epochKnown {
		return true, true, nil
	}
	return false, false, nil
}

// SourceMessageMatches reports whether an exact mailbox|uid identifier still
// resolves to the expected RFC822 Message-ID. A UIDVALIDITY change invalidates
// the identifier even when the same numeric UID has since been reused.
func (c *Client) SourceMessageMatches(
	ctx context.Context,
	messageID, expectedRFC822MessageID string,
) (matches bool, conclusive bool, err error) {
	mailbox, _, err := parseCompositeID(messageID)
	if err != nil {
		return false, false, err
	}
	c.mu.Lock()
	included := c.mailboxIncludedLocked(mailbox)
	c.mu.Unlock()
	if !included {
		return false, false, nil
	}

	actualRFC822MessageID, exists, err := c.sourceMessageMetadata(ctx, messageID)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, true, nil
	}
	return normalizeRFC822MessageID(actualRFC822MessageID) ==
		normalizeRFC822MessageID(expectedRFC822MessageID), true, nil
}

// IsPreferredSourceMessageID reports whether messageID belongs to the
// mailbox advertised with the \All special-use attribute. A complete scan
// may use that mailbox's identifier as the stable canonical source ID.
func (c *Client) IsPreferredSourceMessageID(messageID string) bool {
	mailbox, _, err := parseCompositeID(messageID)
	if err != nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allMailFolder != "" && mailbox == c.allMailFolder
}

// DefersAuthoritativeLabelReconciliation reports that complete IMAP mailbox
// labels are committed by the post-sync mailbox-delta transaction. Syncer may
// still merge labels for filtered or otherwise incomplete snapshots.
func (c *Client) DefersAuthoritativeLabelReconciliation() bool {
	return true
}
