package beeper

import (
	"context"
	"log/slog"
)

const (
	// discoverChatPages bounds the unfiltered chat sweep DiscoverAccounts uses
	// to find accounts the accounts endpoint omits. Chats come back ordered by
	// last activity, so a few pages of 200 cover every account that has been
	// used recently. This is discovery, not enumeration: the sweep runs in
	// add-beeper, never on the sync path.
	discoverChatPages = 5
	// discoverIdentityProbes bounds the chat-detail fetches used to work out
	// who "me" is on a discovered account.
	discoverIdentityProbes = 3
)

// candidateAccount is an account seen in the chat sweep but missing from the
// accounts endpoint, along with the chats that can answer who "me" is on it.
type candidateAccount struct {
	accountID string
	network   string
	chatIDs   []string
}

// DiscoverAccounts returns the accounts Beeper Desktop serves, including any
// that its accounts endpoint leaves out.
//
// Beeper carries some networks as native platform-sdk accounts rather than as
// Matrix bridges. Those accounts are absent from GET /v1/accounts — and from
// GET /v1/accounts/{id}, which answers 404 for them — while the chat and
// message endpoints serve them like any other account. Enumerating accounts
// from that endpoint alone therefore skips whole networks silently.
//
// Discovery unions the accounts endpoint with the distinct account IDs seen in
// a bounded, unfiltered chat sweep, synthesising an Account for each ID the
// endpoint did not report and marking it Discovered. Callers get one list and
// need not care which half an account came from.
//
// The sweep is best-effort: if it stops early, discovery keeps whatever it had
// already found and still returns everything the accounts endpoint reported.
func (c *Client) DiscoverAccounts(ctx context.Context) ([]Account, error) {
	listed, err := c.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(listed))
	for _, acct := range listed {
		known[acct.AccountID] = true
	}

	// A cancelled context means the caller gave up, not that Beeper failed a
	// probe. Both steps below tolerate fetch failures by design, so each has to
	// rule cancellation out explicitly; otherwise add-beeper would report
	// success and register sources whose identity was never actually looked up.
	candidates := c.sweepChatAccounts(ctx, known)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, cand := range candidates {
		acct, err := c.resolveCandidate(ctx, cand)
		if err != nil {
			return nil, err
		}
		listed = append(listed, acct)
	}
	return listed, nil
}

// sweepChatAccounts pages the unfiltered chat search and returns the accounts
// that own chats but are absent from known, in the order they were first seen.
//
// Discovery is additive, so a page that fails ends the sweep with whatever it
// has rather than failing registration: a Beeper Desktop that cannot serve the
// chat search still reports the accounts its accounts endpoint listed. Both
// early exits are logged, because from the outside they are indistinguishable
// from a network that genuinely has no chats.
func (c *Client) sweepChatAccounts(ctx context.Context, known map[string]bool) []candidateAccount {
	var (
		out   []candidateAccount
		index = make(map[string]int)
		p     SearchChatsParams
	)
	for range discoverChatPages {
		page, err := c.SearchChats(ctx, p)
		if err != nil {
			slog.Warn("beeper account discovery: chat sweep failed; an account missing from the accounts endpoint may go unregistered",
				"error", err)
			return out
		}
		for _, chat := range page.Items {
			if chat.ID == "" || chat.AccountID == "" || known[chat.AccountID] {
				continue
			}
			i, seen := index[chat.AccountID]
			if !seen {
				i = len(out)
				index[chat.AccountID] = i
				out = append(out, candidateAccount{accountID: chat.AccountID})
			}
			if out[i].network == "" {
				out[i].network = chat.Network
			}
			if len(out[i].chatIDs) < discoverIdentityProbes {
				out[i].chatIDs = append(out[i].chatIDs, chat.ID)
			}
		}
		if !page.HasMore || page.OldestCursor == "" {
			return out
		}
		p.Cursor = page.OldestCursor
	}
	slog.Warn("beeper account discovery: chat sweep reached its page bound; an account whose chats are all older than the scanned window may go unregistered",
		"pages", discoverChatPages)
	return out
}

// resolveCandidate turns a swept account ID into an Account, reading the
// logged-in user's own identity from the isSelf member of up to
// discoverIdentityProbes of its chats.
//
// Several chats are probed and their identity fields merged because Beeper
// exposes different fields on different chats: one chat may carry the self
// member's email and another the phone number. Getting this right decides
// which archived messages are later attributed to the account owner.
//
// A chat that cannot be fetched, or that omits the self member, is skipped: an
// account whose identity stays unknown is still worth registering. The one
// failure that is not tolerated is a cancelled context, which would otherwise
// look like a run of unreadable chats and yield an account with no identity at
// all.
func (c *Client) resolveCandidate(ctx context.Context, cand candidateAccount) (Account, error) {
	acct := Account{AccountID: cand.accountID, Network: cand.network, Discovered: true}
	for _, chatID := range cand.chatIDs {
		chat, err := c.GetChat(ctx, chatID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Account{}, ctxErr
			}
			continue
		}
		if acct.Network == "" {
			acct.Network = chat.Network
		}
		self, ok := selfParticipant(chat)
		if !ok {
			continue
		}
		mergeIdentity(&acct.User, self)
		if acct.User.PhoneNumber != "" && acct.User.Email != "" {
			break // nothing further to learn
		}
	}
	return acct, nil
}

// selfParticipant returns the chat member Beeper marks as the logged-in user.
func selfParticipant(chat *Chat) (User, bool) {
	for _, p := range chat.Participants.Items {
		if p.IsSelf {
			return p.User, true
		}
	}
	return User{}, false
}

// mergeIdentity fills empty identity fields on dst from src. Both describe the
// same account owner, so a field only ever moves from unknown to known.
func mergeIdentity(dst *User, src User) {
	if dst.ID == "" {
		dst.ID = src.ID
	}
	if dst.Username == "" {
		dst.Username = src.Username
	}
	if dst.PhoneNumber == "" {
		dst.PhoneNumber = src.PhoneNumber
	}
	if dst.Email == "" {
		dst.Email = src.Email
	}
	if dst.FullName == "" {
		dst.FullName = src.FullName
	}
	dst.IsSelf = true
}
