package imap

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// NativeMailboxScope is an authenticated, selectable mailbox's exact name
// and server-assigned epoch. Display names alone are never mailbox authority.
type NativeMailboxScope struct {
	Mailbox     string
	UIDValidity uint32
}

// NativeMailboxInventory enumerates the complete selected native domain and
// verifies read access plus UIDVALIDITY with EXAMINE. Failed listings or
// selections return no complete inventory and cannot assert mailbox absence.
func (c *Client) NativeMailboxInventory(ctx context.Context) (string, []NativeMailboxScope, []string, error) {
	var scopes []NativeMailboxScope
	var retired []string
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		items, err := conn.List("", "*", c.listOptionsLocked()).Collect()
		if err != nil {
			return fmt.Errorf("LIST native mailbox scopes: %w", err)
		}
		for _, item := range items {
			if hasAttr(item.Attrs, imap.MailboxAttrNoSelect) {
				retired = append(retired, item.Mailbox)
				continue
			}
			if !c.mailboxIncludedLocked(item.Mailbox) {
				// An explicit deny-list is authority, unlike omission from a bounded
				// --folder scan. Preserve the exact native mailbox identifier.
				if len(filterMailboxes([]string{item.Mailbox}, nil, c.folderFilterExclude)) == 0 {
					retired = append(retired, item.Mailbox)
				}
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			// A failed SELECT leaves no reusable selected-mailbox assumption.
			c.selectedMailbox, c.selectedUIDValidity = "", 0
			selected, err := conn.Select(item.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
			if err != nil {
				if status, ok := errors.AsType[*imap.Error](err); ok && status.Type == imap.StatusResponseTypeNo && (status.Code == imap.ResponseCodeNoPerm || status.Code == imap.ResponseCodeNonExistent) {
					retired = append(retired, item.Mailbox)
					continue
				}
				return fmt.Errorf("EXAMINE native mailbox scope %q: %w", item.Mailbox, err)
			}
			if selected.UIDValidity == 0 {
				return errors.New("native mailbox has no verified UIDVALIDITY")
			}
			c.selectedMailbox, c.selectedUIDValidity = item.Mailbox, selected.UIDValidity
			c.selectedNumMessages = selected.NumMessages
			c.recordNativeMailboxEpochLocked(item.Mailbox, selected.UIDValidity)
			scopes = append(scopes, NativeMailboxScope{Mailbox: item.Mailbox, UIDValidity: selected.UIDValidity})
		}
		return nil
	})
	if err != nil {
		return c.config.Identifier(), nil, retired, err
	}
	return c.config.Identifier(), scopes, retired, nil
}

// NativeMailboxMemberships binds a fetched canonical MIME to the exact native
// UID observations used by the archive's Message-ID deduplication. A matching
// header observation can establish a secondary copy only after a full fetch
// proves the canonical bytes. Headerless copies require the fetched digest.
func (c *Client) NativeMailboxMemberships(sourceMessageID string, raw []byte) ([]NativeMailboxScope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	index := c.nativeMailboxMembershipIndexLocked()
	digest := sha256.Sum256(raw)
	canonicalFetched := false
	canonical := index.bySource[sourceMessageID]
	for _, position := range canonical {
		observation := c.observedMemberships[position]
		if observation.UIDValidity != 0 && observation.UIDValidity == c.nativeMailboxEpochLocked(observation.Mailbox, index) && observation.RawSHA256 == digest && observation.RawSize == int64(len(raw)) {
			canonicalFetched = true
			break
		}
	}
	if !canonicalFetched {
		return nil, errors.New("native canonical mailbox fetch unavailable")
	}
	rfc822ID := rawMIMEMessageID(raw)
	candidates := append([]int(nil), canonical...)
	if rfc822ID != "" {
		candidates = append(candidates, index.byRFC822[rfc822ID]...)
	}
	seen := map[string]bool{}
	visited := map[int]bool{}
	var scopes []NativeMailboxScope
	for _, position := range candidates {
		if visited[position] {
			continue
		}
		visited[position] = true
		observation := c.observedMemberships[position]
		if observation.UID == 0 || observation.UIDValidity == 0 || observation.UIDValidity != c.nativeMailboxEpochLocked(observation.Mailbox, index) || !c.mailboxIncludedLocked(observation.Mailbox) {
			continue
		}
		sameBytes := observation.RawSHA256 == digest && observation.RawSize == int64(len(raw))
		sameCanonical := observation.SourceMessageID == sourceMessageID || observation.CanonicalSourceMessageID == sourceMessageID
		headerOnly := observation.RawSHA256 == [32]byte{} && observation.RawSize == 0
		sameRFC822ID := rfc822ID != "" && observation.RFC822MessageID == rfc822ID
		if (!sameCanonical || !sameBytes) && (!sameRFC822ID || !headerOnly && !sameBytes) {
			continue
		}
		if !seen[observation.Mailbox] {
			seen[observation.Mailbox] = true
			scopes = append(scopes, NativeMailboxScope{Mailbox: observation.Mailbox, UIDValidity: observation.UIDValidity})
		}
	}
	if len(scopes) == 0 {
		return nil, errors.New("native mailbox membership unavailable")
	}
	slices.SortFunc(scopes, func(a, b NativeMailboxScope) int {
		if a.Mailbox < b.Mailbox {
			return -1
		}
		if a.Mailbox > b.Mailbox {
			return 1
		}
		return 0
	})
	return scopes, nil
}

// NativeMailboxSourceIdentity identifies the configured account route. The
// route is useful for pinning failures; successful mailbox observations still
// require an authenticated native session.
func (c *Client) NativeMailboxSourceIdentity() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config.Identifier()
}

// NativeMailboxInventoryComplete distinguishes a bounded CLI --folder scan
// from the full native container domain. Configured folder selection and
// explicit exclusions define eligibility; date filters only bound messages,
// while this inventory still lists and verifies every selected container.
func (c *Client) NativeMailboxInventoryComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.folderFilterInclude) == 0
}

// Index session observations incrementally so each message touches its exact
// native identities rather than rescanning the archive-sized listing. The
// caller holds mu. Listing reset and forgotten UIDs invalidate the index.
type nativeMailboxMembershipIndex struct {
	observed int
	bySource map[string][]int
	byRFC822 map[string][]int
	epochs   map[string]uint32
}

func (c *Client) nativeMailboxMembershipIndexLocked() *nativeMailboxMembershipIndex {
	index := c.nativeMailboxIndex
	if index == nil {
		index = &nativeMailboxMembershipIndex{bySource: map[string][]int{}, byRFC822: map[string][]int{}, epochs: map[string]uint32{}}
		c.nativeMailboxIndex = index
	}
	for position := index.observed; position < len(c.observedMemberships); position++ {
		observation := c.observedMemberships[position]
		index.epochs[observation.Mailbox] = observation.UIDValidity
		if observation.SourceMessageID != "" {
			index.bySource[observation.SourceMessageID] = append(index.bySource[observation.SourceMessageID], position)
		}
		if observation.CanonicalSourceMessageID != "" && observation.CanonicalSourceMessageID != observation.SourceMessageID {
			index.bySource[observation.CanonicalSourceMessageID] = append(index.bySource[observation.CanonicalSourceMessageID], position)
		}
		if observation.RFC822MessageID != "" {
			index.byRFC822[observation.RFC822MessageID] = append(index.byRFC822[observation.RFC822MessageID], position)
		}
	}
	index.observed = len(c.observedMemberships)
	return index
}

// NativeMailboxSelectedEpochs returns authenticated SELECT evidence, including
// empty mailboxes. It performs no provider IO and never infers missing scopes.
func (c *Client) NativeMailboxSelectedEpochs() map[string]uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	epochs := make(map[string]uint32, len(c.nativeSelectedMailboxEpochs))
	maps.Copy(epochs, c.nativeSelectedMailboxEpochs)
	return epochs
}

func (c *Client) recordNativeMailboxEpochLocked(mailbox string, epoch uint32) {
	if epoch == 0 {
		return
	}
	if c.nativeSelectedMailboxEpochs == nil {
		c.nativeSelectedMailboxEpochs = map[string]uint32{}
	}
	c.nativeSelectedMailboxEpochs[mailbox] = epoch
}

func (c *Client) nativeMailboxEpochLocked(mailbox string, index *nativeMailboxMembershipIndex) uint32 {
	if epoch := c.nativeSelectedMailboxEpochs[mailbox]; epoch != 0 {
		return epoch
	}
	return index.epochs[mailbox]
}
