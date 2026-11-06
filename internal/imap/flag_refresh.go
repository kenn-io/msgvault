package imap

import (
	"context"
	"fmt"
	"slices"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// flagRefreshWindow bounds how many already-archived UIDs of one mailbox a
// listing re-reads FLAGS for. UIDs grow with arrival, so the highest ones are
// the newest messages, which is where replies, reads and stars land. One
// window is one metadata FETCH of about 50 bytes per message, so even a full
// window costs one round trip and a few hundred kilobytes per mailbox per run.
// A flag change on an older message waits for the next full enumeration
// (sync-full --noresume), whose republish diffs every row.
const flagRefreshWindow = metadataChunkSize

// StoredMembership is what the archive holds for one mailbox UID: the source
// message ID of the archived message the membership points at, and the flags
// saved on that membership. Nil Flags means the saved value could not be read,
// so it never matches what the server reports.
type StoredMembership struct {
	SourceMessageID string
	Flags           []string
}

// WithStoredMembershipLoader lets a listing compare the server's current
// flags with the archived ones on servers that report no mod-sequence. Without
// CONDSTORE a flag change moves neither UIDNEXT nor the message count, so the
// UIDNEXT shortcut cannot see it. The loader returns the archived memberships
// of the requested UIDs in one mailbox epoch; UIDs it leaves out are not
// refreshed.
func WithStoredMembershipLoader(
	load func(mailbox string, uidValidity uint32, uids []uint32) (map[uint32]StoredMembership, error),
) Option {
	return func(c *Client) { c.storedMembershipLoader = load }
}

type fetchedFlags struct {
	uid   imap.UID
	flags []imap.Flag
}

// fetchFlagsCommand reads UID and FLAGS for a UID set. It tracks item
// presence rather than collecting buffers: FLAGS () is an answer, but a FETCH
// response without a FLAGS item says nothing about the message's flags, and
// treating it as empty would clear flags the server still holds.
func (c *Client) fetchFlagsCommand(ctx context.Context, uids imap.UIDSet) ([]fetchedFlags, error) {
	return runCommand(ctx, c, func() ([]fetchedFlags, error) {
		cmd := c.conn.Fetch(uids, &imap.FetchOptions{UID: true, Flags: true})
		var results []fetchedFlags
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			var result fetchedFlags
			hasFlags := false
			for item := msg.Next(); item != nil; item = msg.Next() {
				switch data := item.(type) {
				case imapclient.FetchItemDataUID:
					result.uid = data.UID
				case imapclient.FetchItemDataFlags:
					result.flags = data.Flags
					hasFlags = true
				}
			}
			if result.uid != 0 && hasFlags {
				results = append(results, result)
			}
		}
		if err := cmd.Close(); err != nil {
			return nil, fmt.Errorf("close flags fetch: %w", err)
		}
		return results, nil
	})
}

// flagRefreshCandidates returns the newest flagRefreshWindow UIDs of a
// mailbox's baseline, leaving out the UIDs this listing reads in full anyway.
func flagRefreshCandidates(known []uint32, listed []imap.UID) []uint32 {
	skip := make(map[uint32]struct{}, len(listed))
	for _, uid := range listed {
		skip[uint32(uid)] = struct{}{}
	}
	candidates := make([]uint32, 0, len(known))
	for _, uid := range known {
		if _, ok := skip[uid]; !ok && uid != 0 {
			candidates = append(candidates, uid)
		}
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	if len(candidates) > flagRefreshWindow {
		candidates = candidates[len(candidates)-flagRefreshWindow:]
	}
	return candidates
}

// refreshKnownFlagsLocked re-reads FLAGS for the newest archived UIDs of a
// mailbox on a server that reports no mod-sequence, and records a membership
// observation only for the UIDs whose flags differ from the archived ones.
// Those observations ride the mailbox's incremental delta, so the store
// rewrites exactly those rows and rebuilds labels for their messages alone.
//
// It is best effort. Any failure logs and leaves the archived flags in place:
// a missed flag refresh costs stale flags until the next run, while failing
// the mailbox would withhold the membership topology the listing just proved.
//
// Caller must hold mu.
func (c *Client) refreshKnownFlagsLocked(
	ctx context.Context, mailbox string, status FolderState, scan mailboxScan,
) {
	if c.storedMembershipLoader == nil || c.labelsSnapshotFilteredLocked() ||
		scan.err != nil || scan.reset {
		return
	}
	// A server that reports mod-sequences is covered by them: an unchanged
	// HIGHESTMODSEQ proves no flag moved, and a changed one forces a full
	// enumeration that re-reads every flag.
	prior, ok := c.priorFolderStates[mailbox]
	if !ok || prior.HighestModSeq != 0 || status.HighestModSeq != 0 ||
		prior.UIDValidity != status.UIDValidity {
		return
	}
	candidates := flagRefreshCandidates(scan.known, scan.uids)
	if len(candidates) == 0 {
		return
	}

	warn := func(msg string, err error) {
		c.logger.Warn(msg, "mailbox", mailbox, "error", err)
	}
	if c.conn == nil {
		// An earlier reconnect in this listing may have failed.
		if err := c.connect(ctx); err != nil {
			warn("flag refresh skipped: connect failed", err)
			return
		}
	}
	err := c.selectMailbox(ctx, mailbox)
	if err != nil && isNetworkError(err) {
		c.logger.Warn("network error selecting mailbox for flag refresh, reconnecting",
			"mailbox", mailbox, "error", err)
		if err = c.reconnect(ctx); err == nil {
			err = c.selectMailbox(ctx, mailbox)
		}
	}
	if err != nil {
		warn("flag refresh skipped: SELECT failed", err)
		return
	}
	// Observations carry the selected UIDVALIDITY, and the store rejects a
	// delta whose observations disagree with its epoch.
	if c.selectedUIDValidity != status.UIDValidity {
		c.logger.Warn("flag refresh skipped: UIDVALIDITY changed since STATUS",
			"mailbox", mailbox, "status", status.UIDValidity, "selected", c.selectedUIDValidity)
		return
	}
	if c.selectedNumMessages == 0 {
		return
	}

	stored, err := c.storedMembershipLoader(mailbox, status.UIDValidity, candidates)
	if err != nil {
		warn("flag refresh skipped: loading archived flags failed", err)
		return
	}

	changed := 0
	for _, chunk := range metadataFetchChunks(candidates) {
		if ctx.Err() != nil {
			return
		}
		var set imap.UIDSet
		expected := make(map[imap.UID]struct{}, len(chunk))
		for _, value := range chunk {
			set.AddNum(imap.UID(value))
			expected[imap.UID(value)] = struct{}{}
		}
		results, err := c.fetchFlagsCommand(ctx, set)
		if err != nil && isNetworkError(err) {
			c.logger.Warn("network error during flag refresh, reconnecting",
				"mailbox", mailbox, "error", err)
			if err = c.reconnect(ctx); err == nil {
				err = c.selectMailbox(ctx, mailbox)
			}
			if err == nil && c.selectedUIDValidity != status.UIDValidity {
				c.logger.Warn("flag refresh stopped: UIDVALIDITY changed during reconnect",
					"mailbox", mailbox)
				return
			}
			if err == nil {
				results, err = c.fetchFlagsCommand(ctx, set)
			}
		}
		if err != nil {
			warn("flag refresh stopped: UID FETCH FLAGS failed", err)
			return
		}
		for _, result := range results {
			if _, ok := expected[result.uid]; !ok {
				continue
			}
			// Answer each UID once, from the first response for it.
			delete(expected, result.uid)
			saved, ok := stored[uint32(result.uid)]
			if !ok || saved.SourceMessageID == "" {
				continue
			}
			if saved.Flags != nil && sameFlags(saved.Flags, result.flags) {
				continue
			}
			// The archived source message ID names the message the stored row
			// already points at, so the store resolves the observation back to
			// that row and changes nothing but its flags.
			c.recordMembershipLocked(
				mailbox, result.uid, saved.SourceMessageID, "", [32]byte{}, 0, result.flags)
			changed++
		}
	}
	c.logger.Debug("refreshed archived flags",
		"mailbox", mailbox, "checked", len(candidates), "changed", changed)
}

// sameFlags compares an archived flag list with a fetched one as sets, the
// way the store's republish diff does: servers may reorder flags.
func sameFlags(saved []string, fetched []imap.Flag) bool {
	current := make([]string, len(fetched))
	for i, flag := range fetched {
		current[i] = string(flag)
	}
	return slices.Equal(
		slices.Compact(slices.Sorted(slices.Values(saved))),
		slices.Compact(slices.Sorted(slices.Values(current))),
	)
}
