package imap

import (
	"context"
	"fmt"
	"maps"
	"slices"

	imap "github.com/emersion/go-imap/v2"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

// MailboxDelta describes the membership changes observed for one mailbox.
type MailboxDelta struct {
	Mailbox      string
	State        FolderState
	ChangedUIDs  []imap.UID
	VanishedUIDs []imap.UID
	Reset        bool
	Incremental  bool
}

type protocolDeltaCapture struct {
	vanished []imap.UID
}

// ObservedMailboxDeltas returns a defensive snapshot of mailbox changes from
// the most recent listing.
func (c *Client) ObservedMailboxDeltas() []MailboxDelta {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observedMailboxDeltas == nil {
		return nil
	}
	deltas := make([]MailboxDelta, len(c.observedMailboxDeltas))
	for i, delta := range c.observedMailboxDeltas {
		delta.State.KnownUIDs = cloneKnownUIDs(delta.State.KnownUIDs)
		delta.ChangedUIDs = append([]imap.UID(nil), delta.ChangedUIDs...)
		delta.VanishedUIDs = append([]imap.UID(nil), delta.VanishedUIDs...)
		deltas[i] = delta
	}
	return deltas
}

func folderStateUnchanged(prior, current FolderState) bool {
	return prior.UIDValidity == current.UIDValidity &&
		prior.UIDNext == current.UIDNext &&
		prior.HighestModSeq == current.HighestModSeq
}

func (c *Client) qresyncBaselinePresent(mailboxes []string) bool {
	if len(c.priorFolderStates) == 0 || len(c.priorFolderStates) != len(mailboxes) {
		return false
	}
	for _, mailbox := range mailboxes {
		state, ok := c.priorFolderStates[mailbox]
		if !ok || state.KnownUIDs == nil {
			return false
		}
	}
	return true
}

func (c *Client) qresyncEligible(prior, current FolderState) bool {
	return prior.UIDValidity != 0 &&
		prior.UIDNext != 0 &&
		current.UIDValidity != 0 &&
		current.UIDNext != 0 &&
		prior.UIDValidity == current.UIDValidity &&
		prior.UIDNext <= current.UIDNext &&
		prior.HighestModSeq != 0 &&
		current.HighestModSeq >= prior.HighestModSeq &&
		prior.KnownUIDs != nil
}

func (c *Client) enableQresync() bool {
	caps := c.conn.Caps()
	if !caps.Has(imap.CapEnable) || !caps.Has(imap.CapQResync) {
		return false
	}
	if c.qresyncEnabled {
		return true
	}
	data, err := c.conn.Enable(imap.CapQResync).Wait()
	if err != nil || data == nil || !data.Caps.Has(imap.CapQResync) {
		return false
	}
	c.qresyncEnabled = true
	return true
}

func (c *Client) tryBuildQresyncMessageList(
	ctx context.Context,
	mailboxes []string,
	statuses map[string]FolderState,
) (bool, error) {
	if c.forceFullEnumeration || c.labelsSnapshotFilteredLocked() || len(mailboxes) == 0 ||
		len(statuses) != len(mailboxes) || !c.qresyncBaselinePresent(mailboxes) {
		return false, nil
	}

	for _, mailbox := range mailboxes {
		prior, priorOK := c.priorFolderStates[mailbox]
		current, currentOK := statuses[mailbox]
		if !priorOK || !currentOK || !c.qresyncEligible(prior, current) {
			return false, nil
		}
	}
	if !c.enableQresync() {
		return false, nil
	}

	c.observedFolderStates = make(map[string]FolderState, len(mailboxes))
	c.observedMailboxDeltas = make([]MailboxDelta, 0, len(mailboxes))
	var messages []gmailapi.MessageID
	activeSourceAliases := make(map[string]string)
	unchanged := 0
	if c.listProgress != nil {
		c.listProgress(0, len(mailboxes), "", 0, 0)
	}
	for i, mailbox := range mailboxes {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		prior := c.priorFolderStates[mailbox]
		var delta MailboxDelta
		delta, err := c.collectQresyncMailbox(mailbox, prior)
		if err != nil {
			return false, err
		}
		if len(delta.ChangedUIDs) == 0 && len(delta.VanishedUIDs) == 0 {
			unchanged++
		}
		for _, uid := range delta.ChangedUIDs {
			sourceMessageID := compositeID(mailbox, uid)
			messages = append(messages, gmailapi.MessageID{ID: sourceMessageID})
			if canonicalSourceMessageID := c.sourceMessageAliases[sourceMessageID]; canonicalSourceMessageID != "" {
				activeSourceAliases[sourceMessageID] = canonicalSourceMessageID
			}
		}
		c.trackFolderMessages(mailbox, delta.State, delta.ChangedUIDs)
		c.observedFolderStates[mailbox] = delta.State
		c.observedMailboxDeltas = append(c.observedMailboxDeltas, delta)
		if c.listProgress != nil {
			c.listProgress(i+1, len(mailboxes), mailbox, len(messages), unchanged)
		}
	}
	c.messageListCache = messages
	c.activeSourceAliases = activeSourceAliases
	c.labelMapComplete = true
	return true, nil
}

func (c *Client) collectQresyncMailbox(mailbox string, prior FolderState) (MailboxDelta, error) {
	c.clearQresyncCapture()
	selected, err := c.conn.Select(mailbox, &imap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		return MailboxDelta{}, fmt.Errorf("CONDSTORE SELECT %q: %w", mailbox, err)
	}
	if selected.UIDValidity == 0 {
		return MailboxDelta{}, fmt.Errorf(
			"CONDSTORE SELECT %q returned no UIDVALIDITY", mailbox)
	}
	if selected.UIDNext == 0 {
		return MailboxDelta{}, fmt.Errorf(
			"CONDSTORE SELECT %q returned no UIDNEXT", mailbox)
	}
	if selected.UIDValidity != prior.UIDValidity {
		return MailboxDelta{}, fmt.Errorf("CONDSTORE SELECT %q changed UIDVALIDITY from %d to %d",
			mailbox, prior.UIDValidity, selected.UIDValidity)
	}
	if uint32(selected.UIDNext) < prior.UIDNext {
		return MailboxDelta{}, fmt.Errorf(
			"CONDSTORE SELECT %q regressed UIDNEXT from %d to %d",
			mailbox, prior.UIDNext, selected.UIDNext)
	}
	if prior.HighestModSeq == 0 || selected.HighestModSeq == 0 {
		return MailboxDelta{}, fmt.Errorf("CONDSTORE SELECT %q returned unusable modseq", mailbox)
	}
	if selected.HighestModSeq < prior.HighestModSeq {
		return MailboxDelta{}, fmt.Errorf(
			"CONDSTORE SELECT %q regressed modseq from %d to %d",
			mailbox, prior.HighestModSeq, selected.HighestModSeq)
	}
	c.selectedMailbox = mailbox
	c.selectedUIDValidity = selected.UIDValidity
	c.selectedNumMessages = selected.NumMessages

	changedSet := make(map[imap.UID]struct{})
	vanishedSet := make(map[imap.UID]struct{})
	if selected.UIDNext > 1 {
		var requestedUIDs imap.UIDSet
		requestedUIDs.AddRange(1, selected.UIDNext-1)
		if requestedUIDs.Dynamic() {
			return MailboxDelta{}, fmt.Errorf(
				"QRESYNC UID FETCH in %q requires static typed UID set", mailbox)
		}
		c.beginQresyncCapture()
		defer c.clearQresyncCapture()
		msgs, fetchErr := c.conn.Fetch(requestedUIDs, &imap.FetchOptions{
			UID:          true,
			Flags:        true,
			ModSeq:       true,
			ChangedSince: prior.HighestModSeq,
			Vanished:     true,
		}).Collect()
		if fetchErr != nil {
			return MailboxDelta{}, fmt.Errorf("QRESYNC UID FETCH in %q: %w", mailbox, fetchErr)
		}

		for _, msg := range msgs {
			if msg.UID != 0 {
				changedSet[msg.UID] = struct{}{}
			}
		}
		for _, uid := range c.snapshotQresyncCapture().vanished {
			vanishedSet[uid] = struct{}{}
		}
	}
	for uid := range vanishedSet {
		delete(changedSet, uid)
	}
	changed := maps.Keys(changedSet)
	vanished := maps.Keys(vanishedSet)
	changedUIDs := slices.Collect(changed)
	vanishedUIDs := slices.Collect(vanished)
	slices.Sort(changedUIDs)
	slices.Sort(vanishedUIDs)

	known := make(map[imap.UID]struct{}, len(prior.KnownUIDs)+len(changedUIDs))
	for _, uid := range prior.KnownUIDs {
		known[imap.UID(uid)] = struct{}{}
	}
	for _, uid := range vanishedUIDs {
		delete(known, uid)
	}
	for _, uid := range changedUIDs {
		known[uid] = struct{}{}
	}
	knownUIDs := slices.Collect(maps.Keys(known))
	slices.Sort(knownUIDs)

	return MailboxDelta{
		Mailbox: mailbox,
		State: FolderState{
			UIDValidity:   selected.UIDValidity,
			UIDNext:       uint32(selected.UIDNext),
			HighestModSeq: selected.HighestModSeq,
			KnownUIDs:     uidsToUint32(knownUIDs),
		},
		ChangedUIDs:  changedUIDs,
		VanishedUIDs: vanishedUIDs,
		Incremental:  true,
	}, nil
}

func (c *Client) beginQresyncCapture() {
	c.qresyncCaptureMu.Lock()
	c.qresyncCapture = &protocolDeltaCapture{}
	c.qresyncCaptureMu.Unlock()
}

func (c *Client) clearQresyncCapture() {
	c.qresyncCaptureMu.Lock()
	c.qresyncCapture = nil
	c.qresyncCaptureMu.Unlock()
}

func (c *Client) snapshotQresyncCapture() protocolDeltaCapture {
	c.qresyncCaptureMu.Lock()
	defer c.qresyncCaptureMu.Unlock()
	if c.qresyncCapture == nil {
		return protocolDeltaCapture{}
	}
	return protocolDeltaCapture{
		vanished: append([]imap.UID(nil), c.qresyncCapture.vanished...),
	}
}

func (c *Client) captureQresyncVanished(set imap.UIDSet, _ bool) {
	uids, ok := set.Nums()
	if !ok {
		return
	}
	c.qresyncCaptureMu.Lock()
	if c.qresyncCapture != nil {
		c.qresyncCapture.vanished = append(c.qresyncCapture.vanished, uids...)
	}
	c.qresyncCaptureMu.Unlock()
}

func uidsToUint32(uids []imap.UID) []uint32 {
	values := make([]uint32, len(uids))
	for i, uid := range uids {
		values[i] = uint32(uid)
	}
	return values
}

func mergeKnownUIDs(prior []uint32, additions []imap.UID) []uint32 {
	if prior == nil {
		return nil
	}
	known := make(map[uint32]struct{}, len(prior)+len(additions))
	for _, uid := range prior {
		known[uid] = struct{}{}
	}
	for _, uid := range additions {
		known[uint32(uid)] = struct{}{}
	}
	values := slices.Collect(maps.Keys(known))
	slices.Sort(values)
	return values
}
