package imap

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	imap "github.com/emersion/go-imap/v2"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
)

// RelocationCandidate binds a saved membership to its archived canonical row.
// The saved epoch must still be present before the client can adopt it.
type RelocationCandidate struct {
	Target      gmail.MessageRelocationTarget
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// WithRelocationCandidateLoader resolves alternatives only for canonical
// source IDs lost from a complete, authoritative mailbox snapshot.
func WithRelocationCandidateLoader(load func(context.Context, []string) ([]RelocationCandidate, error)) Option {
	return func(c *Client) { c.relocationCandidateLoader = load }
}

// MessageRelocationTarget reports a target selected by this listing. Consumers
// must retain this exact guard through alias lookup and ingestion.
func (c *Client) MessageRelocationTarget(sourceMessageID string) (gmail.MessageRelocationTarget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	target, ok := c.relocationTargets[sourceMessageID]
	return target, ok
}

// finalizeMessageListLocked is shared by QRESYNC and full enumeration. No
// listing is exposed if selecting a required replacement fails.
func (c *Client) finalizeMessageListLocked(ctx context.Context, mailboxes []string, messages []gmail.MessageID) error {
	if err := c.addRelocationMessagesLocked(ctx, mailboxes, &messages); err != nil {
		c.messageListCache = nil
		c.relocationTargets = nil
		c.labelMapComplete = false
		c.observedFolderStates = nil
		c.observedMailboxDeltas = nil
		c.clearFolderAcknowledgements()
		return err
	}
	c.messageListCache = append([]gmail.MessageID{}, messages...)
	return nil
}

func (c *Client) addRelocationMessagesLocked(ctx context.Context, mailboxes []string, messages *[]gmail.MessageID) error {
	if c.relocationCandidateLoader == nil || !c.labelMapComplete ||
		!c.since.IsZero() || !c.before.IsZero() || c.labelsSnapshotFilteredLocked() ||
		c.observedMailboxDeltas == nil || !deltasCoverMailboxes(mailboxes, c.observedMailboxDeltas) {
		return nil
	}
	current := make(map[string]FolderState, len(c.observedMailboxDeltas))
	present := make(map[string]map[uint32]bool, len(c.observedMailboxDeltas))
	for _, delta := range c.observedMailboxDeltas {
		current[delta.Mailbox] = delta.State
		uids := make(map[uint32]bool, len(delta.State.KnownUIDs))
		for _, uid := range delta.State.KnownUIDs {
			uids[uid] = true
		}
		present[delta.Mailbox] = uids
	}
	var lost []string
	for mailbox, prior := range c.priorFolderStates {
		state, exists := current[mailbox]
		for _, uid := range prior.KnownUIDs {
			if !exists || state.UIDValidity != prior.UIDValidity || !present[mailbox][uid] {
				lost = append(lost, compositeID(mailbox, imap.UID(uid)))
			}
		}
	}
	if len(lost) == 0 {
		return nil
	}
	slices.Sort(lost)
	lost = slices.Compact(lost)
	candidates, err := c.relocationCandidateLoader(ctx, lost)
	if err != nil {
		return fmt.Errorf("load IMAP relocation candidates: %w", err)
	}
	// Prefer advertised All-Mail, then a stable mailbox/UID ordering. Internal
	// IDs break ties without reselecting a row by its RFC822 identity.
	slices.SortFunc(candidates, func(a, b RelocationCandidate) int {
		if a.Mailbox != b.Mailbox {
			if a.Mailbox == c.allMailFolder {
				return -1
			}
			if b.Mailbox == c.allMailFolder {
				return 1
			}
			return cmp.Compare(a.Mailbox, b.Mailbox)
		}
		if n := cmp.Compare(a.UID, b.UID); n != 0 {
			return n
		}
		return cmp.Compare(a.Target.InternalID, b.Target.InternalID)
	})
	listed := make(map[string]bool, len(*messages))
	for _, message := range *messages {
		listed[message.ID] = true
	}
	selected := make(map[int64]bool)
	c.relocationTargets = make(map[string]gmail.MessageRelocationTarget)
	var forced []gmail.MessageID
	for _, candidate := range candidates {
		target := candidate.Target
		state, exists := current[candidate.Mailbox]
		if selected[target.InternalID] || !exists || candidate.UIDValidity != state.UIDValidity ||
			!present[candidate.Mailbox][candidate.UID] {
			continue
		}
		if _, lostTarget := slices.BinarySearch(lost, target.SourceMessageID); !lostTarget {
			continue
		}
		target.RFC822MessageID = mime.NormalizeMessageID(target.RFC822MessageID)
		if target.RFC822MessageID == "" {
			continue
		}
		target.NewSourceMessageID = compositeID(candidate.Mailbox, imap.UID(candidate.UID))
		if target.SourceMessageID == target.NewSourceMessageID {
			// Relocation must change the composite key: persistence rejects an
			// unchanged source ID, and a survivor at the unchanged key is
			// refreshed by ordinary identity routing (mailbox-epoch and
			// UID-reuse checks). Only a membership epoch inconsistent with the
			// saved folder state can offer the same key here; skipping it keeps
			// that state on the self-healing path instead of failing every retry.
			continue
		}
		if other, exists := c.relocationTargets[target.NewSourceMessageID]; exists && other.InternalID != target.InternalID {
			return fmt.Errorf("IMAP relocation candidate %q has multiple archived targets", target.NewSourceMessageID)
		}
		c.relocationTargets[target.NewSourceMessageID] = target
		selected[target.InternalID] = true
		forced = append(forced, gmail.MessageID{ID: target.NewSourceMessageID})
		if !listed[target.NewSourceMessageID] {
			c.trackFolderMessages(candidate.Mailbox, state, []imap.UID{imap.UID(candidate.UID)})
		}
	}
	for _, message := range *messages {
		if _, selected := c.relocationTargets[message.ID]; !selected {
			forced = append(forced, message)
		}
	}
	*messages = forced
	return nil
}
