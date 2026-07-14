package imap

import (
	"context"
	"fmt"
	"maps"

	imap "github.com/emersion/go-imap/v2"
)

// FolderState is the change-detection state of one mailbox: the
// UIDVALIDITY/UIDNEXT pair reported by STATUS. Under an unchanged
// UIDVALIDITY, UIDNEXT only advances when messages are added, so a
// mailbox whose pair matches the state saved after the last completed
// sync cannot contain messages the archive has not seen.
type FolderState struct {
	UIDValidity uint32
	UIDNext     uint32
}

// WithFolderStates provides per-mailbox states saved after the last
// completed sync. During message listing, mailboxes whose current
// STATUS matches the saved state are skipped without enumeration, and
// changed mailboxes are searched only for UIDs at or above the saved
// UIDNEXT high water mark. Ignored when a date filter is active. When the server
// exposes an \All mailbox, saved states short-circuit fully unchanged
// resyncs; changed runs still enumerate fully for label mapping.
func WithFolderStates(states map[string]FolderState) Option {
	return func(c *Client) { c.priorFolderStates = states }
}

// WithFolderStateSave sets a callback that is invoked after all listed
// messages for a mailbox have been safely handled by the syncer.
func WithFolderStateSave(fn func(string, FolderState)) Option {
	return func(c *Client) { c.folderStateSave = fn }
}

// ObservedFolderStates returns the per-mailbox states captured during
// the last message-list enumeration, for persistence after a completed
// sync. Mailboxes whose STATUS or enumeration failed are absent, so
// saved state for them is left untouched. Returns nil when folder
// tracking was disabled (date filter or no listing yet).
func (c *Client) ObservedFolderStates() map[string]FolderState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observedFolderStates == nil {
		return nil
	}
	states := make(map[string]FolderState, len(c.observedFolderStates))
	maps.Copy(states, c.observedFolderStates)
	return states
}

func (c *Client) observeFolderStates(
	ctx context.Context, mailboxes []string,
) (map[string]FolderState, int) {
	states := make(map[string]FolderState, len(mailboxes))
	unchanged := 0
	for _, mailbox := range mailboxes {
		status, err := c.statusFolder(ctx, mailbox)
		if err != nil {
			c.logger.Warn("STATUS failed, enumerating mailbox fully",
				"mailbox", mailbox, "error", err)
			continue
		}
		states[mailbox] = status
		if prior, ok := c.priorFolderStates[mailbox]; ok &&
			prior.UIDValidity == status.UIDValidity &&
			prior.UIDNext == status.UIDNext {
			unchanged++
		}
	}
	return states, unchanged
}

func (c *Client) trackFolderMessages(
	mailbox string, state FolderState, uids []imap.UID,
) {
	if c.folderStateSave == nil || len(uids) == 0 {
		return
	}
	if c.pendingFolderStates == nil {
		c.pendingFolderStates = make(map[string]FolderState)
		c.pendingFolderCounts = make(map[string]int)
		c.pendingMessageFolder = make(map[string]string)
		c.completedFolders = make(map[string]bool)
	}
	c.pendingFolderStates[mailbox] = state
	c.pendingFolderCounts[mailbox] += len(uids)
	for _, uid := range uids {
		c.pendingMessageFolder[compositeID(mailbox, uid)] = mailbox
	}
}

func (c *Client) clearFolderAcknowledgements() {
	c.pendingFolderStates = nil
	c.pendingFolderCounts = nil
	c.pendingMessageFolder = nil
	c.completedFolders = nil
}

// AcknowledgeMessages records message IDs that the syncer safely
// handled. When every listed message in a folder has been acknowledged,
// the folder state callback is invoked with that folder's high water mark.
func (c *Client) AcknowledgeMessages(_ context.Context, messageIDs []string) {
	var completed []struct {
		mailbox string
		state   FolderState
	}

	c.mu.Lock()
	if c.folderStateSave != nil {
		for _, id := range messageIDs {
			mailbox, ok := c.pendingMessageFolder[id]
			if !ok {
				continue
			}
			delete(c.pendingMessageFolder, id)
			c.pendingFolderCounts[mailbox]--
			if c.pendingFolderCounts[mailbox] > 0 || c.completedFolders[mailbox] {
				continue
			}
			c.completedFolders[mailbox] = true
			completed = append(completed, struct {
				mailbox string
				state   FolderState
			}{mailbox: mailbox, state: c.pendingFolderStates[mailbox]})
		}
	}
	save := c.folderStateSave
	c.mu.Unlock()

	if save == nil {
		return
	}
	for _, item := range completed {
		save(item.mailbox, item.state)
	}
}

// statusFolder fetches UIDVALIDITY and UIDNEXT for a mailbox via
// STATUS, retrying once through a reconnect on network errors.
// Caller must hold mu.
func (c *Client) statusFolder(ctx context.Context, mailbox string) (FolderState, error) {
	opts := &imap.StatusOptions{UIDNext: true, UIDValidity: true}
	data, err := c.conn.Status(mailbox, opts).Wait()
	if err != nil && isNetworkError(err) {
		c.logger.Warn("network error during STATUS, reconnecting",
			"mailbox", mailbox, "error", err)
		if reconErr := c.reconnect(ctx); reconErr != nil {
			return FolderState{}, fmt.Errorf(
				"reconnect failed during STATUS of %q: %w", mailbox, reconErr)
		}
		data, err = c.conn.Status(mailbox, opts).Wait()
	}
	if err != nil {
		return FolderState{}, fmt.Errorf("STATUS %q: %w", mailbox, err)
	}
	if data.UIDNext == 0 {
		return FolderState{}, fmt.Errorf("STATUS %q returned no UIDNEXT", mailbox)
	}
	return FolderState{
		UIDValidity: data.UIDValidity,
		UIDNext:     uint32(data.UIDNext),
	}, nil
}
