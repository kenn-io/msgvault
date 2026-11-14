package signalexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func recipientPeer(r recipient) peer {
	n := 0
	if r.Self != nil {
		n++
	}
	if r.Contact != nil {
		n++
	}
	if r.Group != nil {
		n++
	}
	if n != 1 {
		return peer{}
	}
	if r.Self != nil {
		return peer{key: "self", kind: "direct_chat"}
	}
	if c := r.Contact; c != nil && (len(c.ACI) == 16 || (c.E164 != "" && c.E164 != "0")) {
		name := strings.TrimSpace(c.Given + " " + c.Family)
		if name == "" {
			name = strings.TrimSpace(c.SystemGiven + " " + c.SystemFamily)
		}
		p := peer{name: name, kind: "direct_chat"}
		if len(c.ACI) == 16 {
			p.aci = "aci:" + hex.EncodeToString(c.ACI)
		}
		if c.E164 != "" && c.E164 != "0" {
			p.phone = "e164:+" + string(c.E164)
		}
		p.key = p.aci
		if p.key == "" {
			p.key = p.phone
		}
		return p
	}
	if g := r.Group; g != nil && len(g.MasterKey) == 32 {
		h := sha256.Sum256(g.MasterKey)
		return peer{key: "group:" + hex.EncodeToString(h[:]), name: g.Snapshot.Title.Title, kind: "group_chat"}
	}
	return peer{}
}
func accountScope(account string) string {
	h := sha256.Sum256([]byte(account))
	return hex.EncodeToString(h[:])
}
func scopedKey(scope, key string) string { return "v1:" + scope + ":" + key }
func messageKey(conversation, author string, date, received decimal, ordinal int) string {
	// A JSON array avoids delimiter ambiguity and versions the identity format.
	b, _ := json.Marshal([]string{"v3", conversation, author, string(date), string(received), strconv.Itoa(ordinal)})
	return string(b)
}

// slotCounter numbers frames sharing conversation, author and both times, since
// the export has no message ID. A frame with attachment identities takes the
// stored slot whose first record uniquely matches one; an unmatched frame skips
// a row that a later frame (from hints.go) uniquely claims. Otherwise export
// order decides, so changed text or attachments keep a message's slot.
type slotCounter struct {
	next    map[[sha256.Size]byte]int
	claimed map[string]bool
	hints   *identityHints
}

func newSlotCounter(hints *identityHints) *slotCounter {
	return &slotCounter{next: map[[sha256.Size]byte]int{}, claimed: map[string]bool{}, hints: hints}
}

// slot returns the key and stored ID (0 when new) for a frame.
func (c *slotCounter) slot(ctx context.Context, st *store.Store, sourceID int64, conversation, author string, date, received decimal, item *chatItem, line int) (string, int64, error) {
	m := item.Standard
	later := c.hints.after(hintTuple(item, date, received), line)
	tuple := sha256.Sum256([]byte(messageKey(conversation, author, date, received, 0)))
	n := c.next[tuple]
	c.next[tuple]++
	slotKey := func(k int) string { return messageKey(conversation, author, date, received, k) }
	key := slotKey(n)
	ids, err := st.MessageExistsBatchContext(ctx, sourceID, []string{key, slotKey(1)})
	if err != nil {
		return "", 0, err
	}
	own := ids[key]
	if n == 0 && ids[slotKey(1)] == 0 && (own == 0 || len(later) == 0) {
		return key, own, nil
	}
	want := attachmentIdentities(m)
	fits := func(have map[string]bool) bool {
		if len(want) == 0 {
			return len(have) == 0
		}
		for id := range want {
			if have[id] {
				return true
			}
		}
		return false
	}
	var have map[string]bool // own row's identities, read when the row is unclaimed
	if own != 0 && !c.claimed[key] {
		if have, err = storedIdentities(ctx, st, own); err != nil {
			return "", 0, err
		}
		if fits(have) {
			c.claimed[key] = true
			return key, own, nil
		}
	}
	if own == 0 && len(want) == 0 {
		c.claimed[key] = true
		return key, 0, nil
	}
	match, matchID, ambiguous := "", int64(0), false
	for k := 0; !ambiguous; k++ {
		other := slotKey(k)
		found, err := st.MessageExistsBatchContext(ctx, sourceID, []string{other})
		if err != nil {
			return "", 0, err
		}
		if found[other] == 0 {
			break
		}
		if c.claimed[other] {
			continue
		}
		have, err := storedIdentities(ctx, st, found[other])
		if err != nil {
			return "", 0, err
		}
		if !fits(have) {
			continue
		}
		ambiguous = match != ""
		if match == "" {
			match, matchID = other, found[other]
		}
		if len(want) == 0 {
			break
		}
	}
	if match == "" || ambiguous {
		match, matchID = key, own
		// Order is the fallback, but not for a row another frame already took or
		// one a later frame uniquely claims by identity.
		if c.claimed[key] || (own != 0 && carriedByOne(have, later)) {
			if match, err = c.freeKey(ctx, st, sourceID, slotKey); err != nil {
				return "", 0, err
			}
			matchID = 0
		}
	}
	c.claimed[match] = true
	return match, matchID, nil
}

// freeKey returns the first slot that holds no stored message and no claim.
func (c *slotCounter) freeKey(ctx context.Context, st *store.Store, sourceID int64, slotKey func(int) string) (string, error) {
	for k := 0; ; k++ {
		key := slotKey(k)
		found, err := st.MessageExistsBatchContext(ctx, sourceID, []string{key})
		if err != nil {
			return "", err
		}
		if found[key] == 0 && !c.claimed[key] {
			return key, nil
		}
	}
}

// attachmentIdentities names each attachment by its client UUID, else its
// locator key. A repeated identity makes the message ambiguous, so it has none.
func attachmentIdentities(m *standardMessage) map[string]bool {
	out := map[string]bool{}
	for _, a := range m.Attachments {
		id := ""
		if len(a.ClientUUID) > 0 {
			id = "uuid:" + string(a.ClientUUID)
		} else if a.Pointer != nil && len(a.Pointer.Locator.Key) > 0 {
			id = "key:" + string(a.Pointer.Locator.Key)
		}
		if id == "" {
			continue
		}
		if out[id] {
			return nil
		}
		out[id] = true
	}
	return out
}

func storedIdentities(ctx context.Context, st *store.Store, id int64) (map[string]bool, error) {
	raw, err := st.GetMessageRawContext(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read saved message identity: %w", err)
	}
	var old chatItem
	valid := json.Unmarshal(raw, &old) == nil
	if !valid || old.Standard == nil {
		return map[string]bool{}, nil // An unreadable first record has no identity, so export order decides.
	}
	return attachmentIdentities(old.Standard), nil
}
func originalReceived(m *chatItem) decimal {
	// An edit keeps the first revision's receive time, like its send time.
	if len(m.Revisions) > 0 {
		m = &m.Revisions[0]
	}
	info := m.Incoming
	if info == nil {
		info = m.Outgoing
	}
	if info == nil || info.DateReceived == nil {
		return ""
	}
	return *info.DateReceived
}
func sentTime(d decimal) (time.Time, bool) {
	n, err := strconv.ParseInt(string(d), 10, 64)
	if err != nil || n <= 0 || n > 253402300799999 {
		return time.Time{}, false
	}
	return time.UnixMilli(n).UTC(), true
}
func originalDate(m *chatItem) (decimal, bool) {
	// Revisions must describe this same message, oldest first; nested chains
	// are not native history and cannot define an unambiguous identity.
	date := decimal(m.DateSent)
	top, ok := sentTime(date)
	if !ok {
		return "", false
	}
	var previous time.Time
	for i := range m.Revisions {
		rev := &m.Revisions[i]
		at, ok := sentTime(decimal(rev.DateSent))
		if !ok || rev.ChatID != m.ChatID || rev.AuthorID != m.AuthorID || len(rev.Revisions) > 0 || (!previous.IsZero() && !at.After(previous)) || at.After(top) {
			return "", false
		}
		if i == 0 {
			date = decimal(rev.DateSent)
		}
		previous = at
	}
	return date, true
}
