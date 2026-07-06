package beeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMsg is one message held by the fake Beeper server. Messages live in a
// per-chat slice ordered oldest→newest; SortKey doubles as the cursor value.
type fakeMsg struct {
	ID              string
	SortKey         int
	Timestamp       time.Time
	Type            string // "" defaults to TEXT
	Text            string
	SenderID        string
	SenderName      string
	IsSender        bool
	IsDeleted       bool
	IsHidden        bool
	EditedTimestamp *time.Time
	LinkedMessageID string
	Mentions        []string
	Reactions       []map[string]any
	Attachments     []map[string]any
}

type fakeChat struct {
	ID           string
	AccountID    string
	Network      string
	Title        string
	Type         string // "single" | "group"
	LastActivity time.Time
	Participants []map[string]any
	// ParticipantsTruncated makes the search listing report hasMore=true so
	// the importer fetches the chat detail for the full list.
	ParticipantsTruncated bool
	Msgs                  []fakeMsg // oldest → newest
}

// fakeBeeper simulates the Beeper Desktop API surface the importer uses,
// with sortKey-cursor pagination semantics matching the real API.
type fakeBeeper struct {
	t        *testing.T
	pageSize int

	mu    sync.Mutex
	chats []*fakeChat
	reqs  []string // "PATH?QUERY" per request, in order
}

func newFakeBeeper(t *testing.T) *fakeBeeper {
	t.Helper()
	return &fakeBeeper{t: t, pageSize: 20}
}

func (f *fakeBeeper) addChat(ch *fakeChat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chats = append(f.chats, ch)
}

func (f *fakeBeeper) chat(id string) *fakeChat {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.chats {
		if ch.ID == id {
			return ch
		}
	}
	return nil
}

// appendMsg adds a message to a chat and advances its LastActivity.
func (f *fakeBeeper) appendMsg(chatID string, m fakeMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.chats {
		if ch.ID == chatID {
			ch.Msgs = append(ch.Msgs, m)
			if m.Timestamp.After(ch.LastActivity) {
				ch.LastActivity = m.Timestamp
			}
			return
		}
	}
	f.t.Fatalf("appendMsg: unknown chat %s", chatID)
}

// requests returns the request log ("PATH?QUERY" entries).
func (f *fakeBeeper) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeBeeper) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

func (f *fakeBeeper) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		entry := r.URL.Path
		if r.URL.RawQuery != "" {
			entry += "?" + r.URL.RawQuery
		}
		f.reqs = append(f.reqs, entry)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case path == "/v1/accounts":
			f.writeAccounts(w)
		case path == "/v1/chats/search":
			f.writeChatSearch(w, r)
		case strings.HasPrefix(path, "/v1/chats/"):
			rest := strings.TrimPrefix(path, "/v1/chats/")
			switch parts := strings.SplitN(rest, "/", 3); {
			case len(parts) == 1:
				f.writeChat(w, parts[0])
			case len(parts) == 2 && parts[1] == "messages":
				f.writeMessages(w, r, parts[0])
			case len(parts) == 3 && parts[1] == "messages":
				f.writeMessage(w, parts[0], parts[2])
			default:
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			}
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
}

func (f *fakeBeeper) writeAccounts(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, ch := range f.chats {
		if seen[ch.AccountID] {
			continue
		}
		seen[ch.AccountID] = true
		out = append(out, map[string]any{
			"accountID": ch.AccountID,
			"network":   ch.Network,
			"user":      map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		})
	}
	writeJSON(f.t, w, out)
}

func (f *fakeBeeper) writeChatSearch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	accountID := q.Get("accountIDs")
	var after time.Time
	if v := q.Get("lastActivityAfter"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, `{"error":"bad lastActivityAfter"}`, http.StatusBadRequest)
			return
		}
		after = t
	}
	items := []map[string]any{}
	for _, ch := range f.chats {
		if accountID != "" && ch.AccountID != accountID {
			continue
		}
		if !after.IsZero() && !ch.LastActivity.After(after) {
			continue
		}
		items = append(items, f.chatJSON(ch, true))
	}
	writeJSON(f.t, w, map[string]any{
		"items": items, "hasMore": false, "oldestCursor": nil, "newestCursor": nil,
	})
}

func (f *fakeBeeper) writeChat(w http.ResponseWriter, chatID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.chats {
		if ch.ID == chatID {
			writeJSON(f.t, w, f.chatJSON(ch, false))
			return
		}
	}
	http.Error(w, `{"error":"chat not found"}`, http.StatusNotFound)
}

// chatJSON renders a chat. Listing entries truncate the participant list when
// ParticipantsTruncated is set (mirroring the API's 20-participant cap on
// search results); the detail endpoint always returns everyone.
func (f *fakeBeeper) chatJSON(ch *fakeChat, listing bool) map[string]any {
	parts := ch.Participants
	hasMore := false
	if listing && ch.ParticipantsTruncated {
		parts = parts[:1]
		hasMore = true
	}
	if parts == nil {
		parts = []map[string]any{}
	}
	return map[string]any{
		"id":        ch.ID,
		"accountID": ch.AccountID,
		"network":   ch.Network,
		"title":     ch.Title,
		"type":      ch.Type,
		"participants": map[string]any{
			"items": parts, "hasMore": hasMore, "total": len(ch.Participants),
		},
		"lastActivity": ch.LastActivity.UTC().Format(time.RFC3339Nano),
		"unreadCount":  0,
	}
}

func (f *fakeBeeper) writeMessages(w http.ResponseWriter, r *http.Request, chatID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ch *fakeChat
	for _, c := range f.chats {
		if c.ID == chatID {
			ch = c
			break
		}
	}
	if ch == nil {
		http.Error(w, `{"error":"chat not found"}`, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	cursor := q.Get("cursor")
	direction := q.Get("direction")
	if cursor != "" && direction == "" {
		direction = "before"
	}

	msgs := ch.Msgs // oldest → newest
	var window []fakeMsg
	var hasMore bool
	switch {
	case cursor == "":
		start := max(0, len(msgs)-f.pageSize)
		window = msgs[start:]
		hasMore = start > 0
	case direction == "before":
		cur, _ := strconv.Atoi(cursor)
		end := sort.Search(len(msgs), func(i int) bool { return msgs[i].SortKey >= cur })
		start := max(0, end-f.pageSize)
		window = msgs[start:end]
		hasMore = start > 0
	default: // "after"
		cur, _ := strconv.Atoi(cursor)
		start := sort.Search(len(msgs), func(i int) bool { return msgs[i].SortKey > cur })
		end := min(len(msgs), start+f.pageSize)
		window = msgs[start:end]
		hasMore = end < len(msgs)
	}

	items := make([]map[string]any, 0, len(window))
	// The real API returns pages newest-first.
	for _, m := range slices.Backward(window) {
		items = append(items, f.messageJSON(ch, &m))
	}
	out := map[string]any{"items": items, "hasMore": hasMore}
	if len(window) > 0 {
		out["oldestCursor"] = strconv.Itoa(window[0].SortKey)
		out["newestCursor"] = strconv.Itoa(window[len(window)-1].SortKey)
	} else {
		out["oldestCursor"] = nil
		out["newestCursor"] = nil
	}
	writeJSON(f.t, w, out)
}

func (f *fakeBeeper) writeMessage(w http.ResponseWriter, chatID, messageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.chats {
		if ch.ID != chatID {
			continue
		}
		for i := range ch.Msgs {
			if ch.Msgs[i].ID == messageID {
				writeJSON(f.t, w, f.messageJSON(ch, &ch.Msgs[i]))
				return
			}
		}
	}
	http.Error(w, `{"error":"message not found"}`, http.StatusNotFound)
}

func (f *fakeBeeper) messageJSON(ch *fakeChat, m *fakeMsg) map[string]any {
	typ := m.Type
	if typ == "" {
		typ = "TEXT"
	}
	out := map[string]any{
		"id":         m.ID,
		"chatID":     ch.ID,
		"accountID":  ch.AccountID,
		"senderID":   m.SenderID,
		"senderName": m.SenderName,
		"timestamp":  m.Timestamp.UTC().Format(time.RFC3339Nano),
		"sortKey":    strconv.Itoa(m.SortKey),
		"type":       typ,
		"isSender":   m.IsSender,
		"isDeleted":  m.IsDeleted,
	}
	if m.Text != "" {
		out["text"] = m.Text
	}
	if m.IsHidden {
		out["isHidden"] = true
	}
	if m.EditedTimestamp != nil {
		out["editedTimestamp"] = m.EditedTimestamp.UTC().Format(time.RFC3339Nano)
	}
	if m.LinkedMessageID != "" {
		out["linkedMessageID"] = m.LinkedMessageID
	}
	if m.Mentions != nil {
		out["mentions"] = m.Mentions
	}
	if m.Reactions != nil {
		out["reactions"] = m.Reactions
	}
	if m.Attachments != nil {
		out["attachments"] = m.Attachments
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode fake response: %v", err)
	}
}

func testToken(context.Context) (string, error) { return "test-token", nil }
