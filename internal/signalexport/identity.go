package signalexport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strconv"
	"strings"
	"time"
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
func messageKey(conversation, author string, date decimal) string {
	// A JSON array avoids delimiter ambiguity and versions the identity format.
	b, _ := json.Marshal([]string{"v1", conversation, author, string(date)})
	return string(b)
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
	if _, ok := sentTime(date); !ok {
		return "", false
	}
	var previous time.Time
	for i := range m.Revisions {
		rev := &m.Revisions[i]
		at, ok := sentTime(decimal(rev.DateSent))
		if !ok || rev.ChatID != m.ChatID || rev.AuthorID != m.AuthorID || len(rev.Revisions) > 0 || (!previous.IsZero() && !at.After(previous)) {
			return "", false
		}
		top, _ := sentTime(decimal(m.DateSent))
		if at.After(top) {
			return "", false
		}
		if i == 0 {
			date = decimal(rev.DateSent)
		}
		previous = at
	}
	return date, true
}
