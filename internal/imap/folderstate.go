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
// UIDNEXT. Ignored when a date filter is active or when the server
// exposes an \All mailbox (Gmail-style virtual folders need full
// enumeration for label mapping).
func WithFolderStates(states map[string]FolderState) Option {
	return func(c *Client) { c.priorFolderStates = states }
}

// ObservedFolderStates returns the per-mailbox states captured during
// the last message-list enumeration, for persistence after a completed
// sync. Mailboxes whose STATUS or enumeration failed are absent, so
// saved state for them is left untouched. Returns nil when folder
// tracking was disabled (date filter, \All mailbox, or no listing yet).
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
