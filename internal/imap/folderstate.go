package imap

import (
	"context"
	"fmt"
	"maps"
	"slices"

	imap "github.com/emersion/go-imap/v2"
)

// MembershipObservation records one fetched mailbox UID and its provider
// identity without coupling the IMAP protocol package to the durable store.
type MembershipObservation struct {
	Mailbox                  string
	UIDValidity              uint32
	UID                      uint32
	SourceMessageID          string
	CanonicalSourceMessageID string
	RFC822MessageID          string
	RawSHA256                [32]byte
	RawSize                  int64
	Flags                    []string
}

// ObservedMemberships returns a defensive snapshot of memberships fetched
// during the current listing and message-download session.
func (c *Client) ObservedMemberships() []MembershipObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observedMemberships == nil {
		return nil
	}
	observations := make([]MembershipObservation, len(c.observedMemberships))
	for i, observation := range c.observedMemberships {
		if observation.Flags != nil {
			observation.Flags = append([]string{}, observation.Flags...)
		}
		observations[i] = observation
	}
	return observations
}

func (c *Client) recordMembershipLocked(
	mailbox string,
	uid imap.UID,
	canonicalSourceMessageID string,
	rfc822MessageID string,
	rawSHA256 [32]byte,
	rawSize int64,
	flags []imap.Flag,
) {
	if uid == 0 {
		return
	}
	canonicalFlags := make([]string, len(flags))
	for i, flag := range flags {
		canonicalFlags[i] = string(flag)
	}
	slices.Sort(canonicalFlags)
	sourceMessageID := compositeID(mailbox, uid)
	if canonicalSourceMessageID == "" {
		canonicalSourceMessageID = c.activeSourceAliases[sourceMessageID]
	}
	c.observedMemberships = append(c.observedMemberships, MembershipObservation{
		Mailbox:                  mailbox,
		UIDValidity:              c.selectedUIDValidity,
		UID:                      uint32(uid),
		SourceMessageID:          sourceMessageID,
		CanonicalSourceMessageID: canonicalSourceMessageID,
		RFC822MessageID:          rfc822MessageID,
		RawSHA256:                rawSHA256,
		RawSize:                  rawSize,
		Flags:                    canonicalFlags,
	})
}

// forgetMembershipLocked drops any observation of one mailbox UID recorded
// earlier in the run. A UID the server leaves out of a FETCH response is gone
// from the mailbox, and the run treats that as handled rather than failed. Its
// observation must not reach the durable commit: nothing ingested the message,
// so membership resolution finds no row to map it to and rolls back every
// mailbox cursor in the source. Deletion detection retires the stored
// membership on the next run.
func (c *Client) forgetMembershipLocked(mailbox string, uid imap.UID) {
	c.observedMemberships = slices.DeleteFunc(
		c.observedMemberships,
		func(observation MembershipObservation) bool {
			return observation.Mailbox == mailbox && observation.UID == uint32(uid)
		},
	)
}

// FolderState is the change-detection state of one mailbox.
type FolderState struct {
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64
	KnownUIDs     []uint32

	// NumMessages is the STATUS MESSAGES count observed this session, or nil
	// when the server did not report one. It is the deletion detector a server
	// without CONDSTORE cannot otherwise provide. Transient: never loaded from
	// or written to the store, so a state restored by WithFolderStates always
	// carries nil here.
	NumMessages *uint32
}

func cloneKnownUIDs(uids []uint32) []uint32 {
	if uids == nil {
		return nil
	}
	return append([]uint32{}, uids...)
}

// WithFolderStates provides per-mailbox states saved after the last
// completed sync. During message listing, mailboxes whose current
// STATUS matches the saved state are skipped without enumeration, and
// changed mailboxes are searched only for UIDs at or above the saved
// UIDNEXT high water mark. Ignored when a date filter is active. When the server
// exposes an \All mailbox, saved states short-circuit fully unchanged
// resyncs; changed runs still enumerate fully for label mapping.
func WithFolderStates(states map[string]FolderState) Option {
	return func(c *Client) {
		c.priorFolderStates = make(map[string]FolderState, len(states))
		for mailbox, state := range states {
			state.KnownUIDs = cloneKnownUIDs(state.KnownUIDs)
			c.priorFolderStates[mailbox] = state
		}
	}
}

// WithSourceMessageAliasLoader supplies durable mailbox-UID aliases from the
// last completed sync. They prevent a changed secondary copy without a
// Message-ID from being re-imported under its secondary mailbox identity. The
// client asks for the UIDs one listing touches, which on an incremental run is
// a handful of the source's stored memberships.
func WithSourceMessageAliasLoader(
	load func(mailbox string, uids []uint32) (map[string]string, error),
) Option {
	return func(c *Client) { c.aliasLoader = load }
}

// loadSourceMessageAliases merges the durable aliases of these mailbox UIDs
// into the session map. A load failure costs the run its aliases and nothing
// else: an unresolved copy is re-imported under its own identity.
// Caller must hold mu.
func (c *Client) loadSourceMessageAliases(mailbox string, uids []imap.UID) {
	if c.aliasLoader == nil || len(uids) == 0 {
		return
	}
	requested := make([]uint32, len(uids))
	for i, uid := range uids {
		requested[i] = uint32(uid)
	}
	aliases, err := c.aliasLoader(mailbox, requested)
	if err != nil {
		// One unreadable database would otherwise warn once per mailbox and
		// once per fetch chunk. Later requests still run: an alias this run
		// misses costs a re-imported copy.
		if !c.aliasLoadWarned {
			c.aliasLoadWarned = true
			c.logger.Warn("failed to load IMAP source message aliases",
				"mailbox", mailbox, "error", err)
		}
		return
	}
	if c.sourceMessageAliases == nil {
		c.sourceMessageAliases = make(map[string]string, len(aliases))
	}
	maps.Copy(c.sourceMessageAliases, aliases)
}

// CanonicalSourceMessageID returns a durable alias only after this session
// validated the mailbox epoch through the QRESYNC path.
func (c *Client) CanonicalSourceMessageID(sourceMessageID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	canonicalSourceMessageID, ok := c.activeSourceAliases[sourceMessageID]
	return canonicalSourceMessageID, ok
}

// WithForceFullEnumeration keeps saved folder identity metadata but disables
// UIDNEXT and unchanged-mailbox shortcuts for this client session.
func WithForceFullEnumeration() Option {
	return func(c *Client) { c.forceFullEnumeration = true }
}

// ForceFullEnumerationForLimitedSync disables sparse incremental listing for
// a limited run. The syncer reconciles processed labels immediately because a
// limited run cannot publish mailbox deltas, so those labels must come from a
// complete mailbox-membership view.
func (c *Client) ForceFullEnumerationForLimitedSync() {
	c.mu.Lock()
	c.forceFullEnumeration = true
	c.messageListCache = nil
	c.mu.Unlock()
}

// WithFolderStateSave sets a callback that is invoked after all listed
// messages for a mailbox have been safely handled by the syncer.
func WithFolderStateSave(fn func(string, FolderState)) Option {
	return func(c *Client) { c.folderStateSave = fn }
}

// ObservedFolderStates returns the per-mailbox states captured during
// the last message-list enumeration, for persistence after a completed
// sync. Mailboxes whose STATUS or enumeration failed are absent, and
// mailboxes with unacknowledged messages are omitted, so saved state for
// them is left untouched. Returns nil when folder tracking was disabled
// (date filter or no listing yet).
func (c *Client) ObservedFolderStates() map[string]FolderState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observedFolderStates == nil {
		return nil
	}
	states := make(map[string]FolderState, len(c.observedFolderStates))
	for mailbox, state := range c.observedFolderStates {
		if c.pendingFolderCounts[mailbox] > 0 {
			continue
		}
		state.KnownUIDs = cloneKnownUIDs(state.KnownUIDs)
		states[mailbox] = state
	}
	return states
}

func (c *Client) observeFolderStates(
	ctx context.Context, mailboxes []string,
) map[string]FolderState {
	states := make(map[string]FolderState, len(mailboxes))
	for _, mailbox := range mailboxes {
		status, err := c.statusFolder(ctx, mailbox)
		if err != nil {
			c.logger.Warn("STATUS failed, enumerating mailbox fully",
				"mailbox", mailbox, "error", err)
			continue
		}
		if prior, ok := c.priorFolderStates[mailbox]; ok &&
			folderStateUnchanged(prior, status) {
			status.KnownUIDs = cloneKnownUIDs(prior.KnownUIDs)
		}
		states[mailbox] = status
	}
	return states
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
	opts := &imap.StatusOptions{
		NumMessages:   true,
		UIDNext:       true,
		UIDValidity:   true,
		HighestModSeq: c.conn.Caps().Has(imap.CapCondStore),
	}
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
	if data.UIDValidity == 0 {
		return FolderState{}, fmt.Errorf("STATUS %q returned no UIDVALIDITY", mailbox)
	}
	if data.UIDNext == 0 {
		return FolderState{}, fmt.Errorf("STATUS %q returned no UIDNEXT", mailbox)
	}
	// A missing MESSAGES is not an error: it costs the caller the folder
	// shortcuts and nothing else, so degrade to full enumeration rather than
	// failing a STATUS the rest of which is usable.
	return FolderState{
		UIDValidity:   data.UIDValidity,
		UIDNext:       uint32(data.UIDNext),
		HighestModSeq: data.HighestModSeq,
		NumMessages:   data.NumMessages,
	}, nil
}
