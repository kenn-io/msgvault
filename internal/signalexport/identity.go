package signalexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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
	if c := r.Contact; c != nil && len(c.ACI) == 16 {
		name := strings.TrimSpace(c.Given + " " + c.Family)
		if name == "" {
			name = strings.TrimSpace(c.SystemGiven + " " + c.SystemFamily)
		}
		p := peer{key: "aci:" + hex.EncodeToString(c.ACI), name: name, kind: "direct_chat"}
		if p.name == "" && c.E164 != "" && c.E164 != "0" {
			p.name = "+" + string(c.E164)
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
	b, _ := json.Marshal([]string{"v" + strconv.Itoa(formatVersion), conversation, author, string(date), string(received), strconv.Itoa(ordinal)})
	return string(b)
}

// slotCounter holds small map entries per distinct conversation, author, and time tuple.
type slotCounter struct {
	next      map[[sha256.Size]byte]int
	ambiguous map[[sha256.Size]byte]bool
	counts    map[[sha256.Size]byte]int
}

func newSlotCounter(counts map[[sha256.Size]byte]int) *slotCounter {
	return &slotCounter{next: map[[sha256.Size]byte]int{}, ambiguous: map[[sha256.Size]byte]bool{}, counts: counts}
}

func slotTuple(conversation, author string, date, received decimal) [sha256.Size]byte {
	return sha256.Sum256([]byte(messageKey(conversation, author, date, received, 0)))
}

func (c *slotCounter) slot(ctx context.Context, st *store.Store, sourceID int64, conversation, author string, date, received decimal, current *standardMessage) (string, int64, bool, error) {
	tuple := slotTuple(conversation, author, date, received)
	n := c.next[tuple]
	c.next[tuple]++
	key := messageKey(conversation, author, date, received, n)
	next := messageKey(conversation, author, date, received, n+1)
	ids, err := st.MessageExistsBatchContext(ctx, sourceID, []string{key, next})
	if err != nil {
		return "", 0, false, err
	}
	if c.ambiguous[tuple] {
		return key, ids[key], true, nil
	}
	if c.counts[tuple] < 2 && n == 0 && ids[next] == 0 {
		return key, ids[key], false, nil
	}
	ambiguous, err := ambiguousText(ctx, st, ids[key], current)
	if err != nil {
		return "", 0, false, err
	}
	if ambiguous {
		c.ambiguous[tuple] = true
	}
	return key, ids[key], ambiguous, nil
}
func ambiguousText(ctx context.Context, st *store.Store, id int64, current *standardMessage) (bool, error) {
	if id == 0 {
		return false, nil
	}
	saved, err := st.GetMessageRawContext(ctx, id)
	if err != nil {
		return false, err
	}
	var old chatItem
	if err := json.Unmarshal(saved, &old); err != nil {
		return false, err
	}
	text := func(m *standardMessage) string {
		if m != nil && m.Text != nil {
			return m.Text.Body
		}
		return ""
	}
	return text(old.Standard) != text(current), nil
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
