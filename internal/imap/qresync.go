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
		delta, err := c.collectQresyncMailbox(ctx, mailbox, prior)
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

func (c *Client) collectQresyncMailbox(
	ctx context.Context, mailbox string, prior FolderState,
) (MailboxDelta, error) {
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
	if err := c.verifyQresyncCoverage(
		ctx, mailbox, prior, uint32(selected.UIDNext), changedSet, vanishedSet,
	); err != nil {
		return MailboxDelta{}, err
	}
	if err := c.refreshExistingQresyncChanges(
		ctx, mailbox, prior, selected.HighestModSeq, changedSet, vanishedSet,
	); err != nil {
		return MailboxDelta{}, err
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

// refreshExistingQresyncChanges independently checks the mod-sequence of each
// message in the saved baseline. UIDNEXT proves how many new UIDs a
// CHANGEDSINCE response must cover, but it says nothing about how many existing
// messages changed. A plain FETCH provides that missing coverage: every live
// baseline UID must be returned, and its mod-sequence says whether it belongs
// in the delta. The normal delta fetch then reads and persists that UID's
// current flags.
func (c *Client) refreshExistingQresyncChanges(
	ctx context.Context,
	mailbox string,
	prior FolderState,
	currentHighestModSeq uint64,
	changedSet, vanishedSet map[imap.UID]struct{},
) error {
	if currentHighestModSeq == prior.HighestModSeq || len(prior.KnownUIDs) == 0 {
		return nil
	}

	for chunkStart := 0; chunkStart < len(prior.KnownUIDs); chunkStart += fetchChunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(chunkStart+fetchChunkSize, len(prior.KnownUIDs))
		expected := make(map[imap.UID]struct{}, end-chunkStart)
		var requested imap.UIDSet
		for _, value := range prior.KnownUIDs[chunkStart:end] {
			uid := imap.UID(value)
			if uid == 0 || value >= prior.UIDNext {
				return fmt.Errorf("QRESYNC %q has invalid baseline UID %d", mailbox, value)
			}
			expected[uid] = struct{}{}
			requested.AddNum(uid)
		}

		msgs, err := c.conn.Fetch(requested, &imap.FetchOptions{
			UID: true, Flags: true, ModSeq: true,
		}).Collect()
		if err != nil {
			return fmt.Errorf("QRESYNC existing-message refresh in %q: %w", mailbox, err)
		}
		for _, msg := range msgs {
			if _, wanted := expected[msg.UID]; !wanted {
				continue
			}
			if msg.ModSeq == 0 {
				return fmt.Errorf(
					"QRESYNC existing-message refresh in %q returned no modseq for UID %d",
					mailbox, msg.UID)
			}
			delete(expected, msg.UID)
			if msg.ModSeq > prior.HighestModSeq {
				changedSet[msg.UID] = struct{}{}
			}
		}
		for uid := range vanishedSet {
			delete(expected, uid)
		}
		if len(expected) != 0 {
			missing := slices.Collect(maps.Keys(expected))
			slices.Sort(missing)
			return fmt.Errorf(
				"QRESYNC existing-message refresh in %q omitted UIDs %v", mailbox, missing)
		}
	}
	return nil
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

// diffKnownUIDs compares a stored baseline against a freshly enumerated UID
// set and reports what changed. added are UIDs the baseline does not have;
// vanished are baseline UIDs the mailbox no longer holds. Both are sorted.
func diffKnownUIDs(prior []uint32, current []imap.UID) (added, vanished []imap.UID) {
	priorSet := make(map[uint32]struct{}, len(prior))
	for _, uid := range prior {
		priorSet[uid] = struct{}{}
	}
	currentSet := make(map[uint32]struct{}, len(current))
	for _, uid := range current {
		currentSet[uint32(uid)] = struct{}{}
		if _, ok := priorSet[uint32(uid)]; !ok {
			added = append(added, uid)
		}
	}
	for _, uid := range prior {
		if _, ok := currentSet[uid]; !ok {
			vanished = append(vanished, imap.UID(uid))
		}
	}
	slices.Sort(added)
	slices.Sort(vanished)
	return added, vanished
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

// verifyQresyncCoverage checks that the CHANGEDSINCE response accounted for
// every message that arrived since the last run.
//
// A server that leaves a live UID out of that response looks exactly like one
// that had nothing to report for it, and nothing downstream can tell the two
// apart: the UID never reaches KnownUIDs, it is never listed or fetched, no
// error is recorded, and HIGHESTMODSEQ still advances past it. No later run
// has a reason to ask about it again, so the message is lost for good.
//
// Only UIDs at or above the previous UIDNEXT can be checked by this arithmetic.
// A message with a UID that high was appended after the last run read UIDNEXT,
// so its mod-sequence is above the one this fetch asked about and the server was
// obliged to report it. refreshExistingQresyncChanges separately checks UIDs
// below that mark because UIDNEXT cannot reveal an omitted flag change there.
//
// UIDs are handed out in sequence, so the growth in UIDNEXT is exactly how
// many were assigned since the last run, and each of them has to come back
// either as changed or as vanished. When that many are accounted for the
// response is complete by arithmetic alone and nothing is asked of the server:
// the ordinary case of new mail arriving costs nothing, which is what QRESYNC
// is for.
//
// Only a shortfall is worth a question, and then the question goes to UID
// SEARCH, which is independent of the FETCH that misbehaved.
//
// Neither the message count nor the size of KnownUIDs appears here. KnownUIDs
// is read back from stored memberships, so it drifts from the server in both
// directions -- short when an earlier run failed to ingest something, long
// when messages left without the server reporting VANISHED -- and drift in the
// second direction hides exactly the omission this is looking for.
func (c *Client) verifyQresyncCoverage(
	ctx context.Context,
	mailbox string,
	prior FolderState,
	currentUIDNext uint32,
	changedSet, vanishedSet map[imap.UID]struct{},
) error {
	if currentUIDNext <= prior.UIDNext {
		return nil
	}
	// Unsigned throughout: prior.UIDNext is below currentUIDNext here, so the
	// difference is a real count, and int is 32 bits wide on a 32-bit build.
	assigned := currentUIDNext - prior.UIDNext

	// Only UIDs inside the assigned span count. A report about a UID at or
	// above the current UIDNEXT names a message the server has not assigned
	// yet, and letting it count would let one such report stand in for a real
	// message the response left out.
	inAssignedSpan := func(uid imap.UID) bool {
		return uint32(uid) >= prior.UIDNext && uint32(uid) < currentUIDNext
	}
	accounted := uint32(0)
	for uid := range changedSet {
		if inAssignedSpan(uid) {
			accounted++
		}
	}
	for uid := range vanishedSet {
		if inAssignedSpan(uid) {
			accounted++
		}
	}
	if accounted >= assigned {
		return nil
	}

	present, err := c.enumerateMailbox(ctx, mailbox, imap.UID(prior.UIDNext))
	if err != nil {
		return fmt.Errorf("QRESYNC coverage search in %q: %w", mailbox, err)
	}
	// enumerateMailbox reconnects on a network error, and a reconnect clears
	// the ENABLE. Every mailbox after this one would then ask for VANISHED on a
	// connection that never enabled QRESYNC. This costs nothing when the
	// connection survived, and fails the mailbox when the new one cannot enable
	// it, which the caller answers with a full enumeration.
	if !c.enableQresync() {
		return fmt.Errorf(
			"QRESYNC unavailable after the coverage search in %q", mailbox)
	}
	for _, uid := range present {
		// "UID SEARCH UID n:*" returns the last message even when n is past
		// it, so a UID below the high water mark here is an artefact of the
		// search and not a new message. RFC 3501 6.4.8.
		if uint32(uid) < prior.UIDNext {
			continue
		}
		if _, changed := changedSet[uid]; changed {
			continue
		}
		if _, vanished := vanishedSet[uid]; vanished {
			continue
		}
		return fmt.Errorf(
			"QRESYNC %q left UID %d out of the CHANGEDSINCE response", mailbox, uid)
	}
	return nil
}
