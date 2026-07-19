package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// fakeMsg is one message held by the fake Slack server. Channel messages
// live oldest→newest; ts values are assigned by the test.
type fakeMsg struct {
	TS        string
	ThreadTS  string
	User      string
	BotID     string
	Username  string
	Subtype   string
	Text      string
	Edited    bool
	Reactions []map[string]any
	Files     []map[string]any
	// Replies holds the thread's replies (oldest→newest) when this message
	// is a root. Reply fakeMsgs must carry ThreadTS = root TS.
	Replies []fakeMsg
}

func (m *fakeMsg) toJSON() map[string]any {
	out := map[string]any{"type": "message", "ts": m.TS, "text": m.Text}
	if m.User != "" {
		out["user"] = m.User
	}
	if m.BotID != "" {
		out["bot_id"] = m.BotID
		out["username"] = m.Username
	}
	if m.Subtype != "" {
		out["subtype"] = m.Subtype
	}
	if m.ThreadTS != "" {
		out["thread_ts"] = m.ThreadTS
	}
	if len(m.Replies) > 0 {
		out["thread_ts"] = m.TS
		out["reply_count"] = len(m.Replies)
		out["latest_reply"] = m.Replies[len(m.Replies)-1].TS
	}
	if m.Edited {
		out["edited"] = map[string]any{"user": m.User, "ts": m.TS}
	}
	if len(m.Reactions) > 0 {
		out["reactions"] = m.Reactions
	}
	if len(m.Files) > 0 {
		out["files"] = m.Files
	}
	return out
}

type fakeConv struct {
	ID      string
	Name    string
	Kind    string // "public" | "private" | "mpim" | "im"
	IMUser  string // peer for im
	Members []string
	Msgs    []fakeMsg // top-level messages, oldest → newest
}

func (c *fakeConv) toJSON() map[string]any {
	out := map[string]any{"id": c.ID, "name": c.Name, "is_member": true}
	switch c.Kind {
	case "im":
		out["is_im"] = true
		out["user"] = c.IMUser
	case "mpim":
		out["is_mpim"] = true
		out["is_private"] = true
	case "private":
		out["is_channel"] = true
		out["is_private"] = true
	default:
		out["is_channel"] = true
	}
	return out
}

// findRoot returns the root fakeMsg for ts, or nil.
func (c *fakeConv) findRoot(ts string) *fakeMsg {
	for i := range c.Msgs {
		if c.Msgs[i].TS == ts {
			return &c.Msgs[i]
		}
	}
	return nil
}

// fakeSlack simulates the Slack Web API surface the importer uses, with
// cursor pagination semantics matching the real API (newest-first history
// pages, oldest-exclusive bounds, next_cursor continuation).
type fakeSlack struct {
	t        *testing.T
	pageSize int

	mu    sync.Mutex
	users []map[string]any
	convs []*fakeConv
	// failHistory / failReplies make the named channel / root ts answer a
	// method error (a non-retryable fetch failure).
	failHistory map[string]bool
	failReplies map[string]bool
	// failHistoryContinuations fails only history requests carrying a page
	// cursor, emulating a walk that dies partway through a multi-page window.
	failHistoryContinuations bool
	// ghosts lists conversation IDs that stay enumerable but 404 on read
	// (see handleGhost).
	ghosts []string
	// rateLimit429s serves that many 429s (with Retry-After: 0) before
	// succeeding, per method call sequence.
	rateLimit429s int
	// historyCalls counts conversations.history requests.
	historyCalls int
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	return &fakeSlack{
		t: t, pageSize: 3,
		failHistory: map[string]bool{}, failReplies: map[string]bool{},
	}
}

// handleGhost keeps c in the users.conversations listing while making its
// history/members lookups answer channel_not_found, like a conversation the
// token can enumerate but not read.
func (f *fakeSlack) handleGhost(c *fakeConv) {
	f.ghosts = append(f.ghosts, c.ID)
}

func (f *fakeSlack) isGhost(id string) bool {
	return slices.Contains(f.ghosts, id)
}

func (f *fakeSlack) conv(id string) *fakeConv {
	if f.isGhost(id) {
		return nil
	}
	for _, c := range f.convs {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (f *fakeSlack) serve() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		f.reply(w, map[string]any{
			"url": "https://testers.slack.com/", "team": "Testers",
			"user": "me", "team_id": "T01", "user_id": "UME",
		})
	})
	mux.HandleFunc("/users.list", f.handleUsersList)
	mux.HandleFunc("/users.conversations", f.handleUsersConversations)
	mux.HandleFunc("/conversations.members", f.handleMembers)
	mux.HandleFunc("/conversations.history", f.handleHistory)
	mux.HandleFunc("/conversations.replies", f.handleReplies)
	// The 429 gate runs before any handler takes f.mu (reply is called with
	// the mutex held, so it must not lock).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		limited := f.rateLimit429s > 0
		if limited {
			f.rateLimit429s--
		}
		f.mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

// reply writes a successful envelope. Callers may hold f.mu.
func (f *fakeSlack) reply(w http.ResponseWriter, body map[string]any) {
	body["ok"] = true
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		f.t.Errorf("encode fake response: %v", err)
	}
}

func (f *fakeSlack) replyErr(w http.ResponseWriter, apiErr string) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": apiErr}); err != nil {
		f.t.Errorf("encode fake error response: %v", err)
	}
}

// page slices items[from:from+pageSize] driven by the "cursor" form value
// (a stringified index), returning the slice and the next cursor.
func (f *fakeSlack) page(r *http.Request, n int) (from, to int, next string) {
	from = 0
	if cur := r.FormValue("cursor"); cur != "" {
		idx, err := strconv.Atoi(cur)
		if err != nil {
			f.t.Errorf("bad cursor %q", cur)
			return 0, 0, ""
		}
		from = idx
	}
	to = min(from+f.pageSize, n)
	if to < n {
		next = strconv.Itoa(to)
	}
	return from, to, next
}

func (f *fakeSlack) handleUsersList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	from, to, next := f.page(r, len(f.users))
	f.reply(w, map[string]any{
		"members":           f.users[from:to],
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

func (f *fakeSlack) handleUsersConversations(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	channels := make([]map[string]any, 0, len(f.convs))
	for _, c := range f.convs {
		channels = append(channels, c.toJSON())
	}
	from, to, next := f.page(r, len(channels))
	f.reply(w, map[string]any{
		"channels":          channels[from:to],
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

func (f *fakeSlack) handleMembers(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.conv(r.FormValue("channel"))
	if c == nil {
		f.replyErr(w, "channel_not_found")
		return
	}
	from, to, next := f.page(r, len(c.Members))
	f.reply(w, map[string]any{
		"members":           c.Members[from:to],
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

// visibleHistory returns c's top-level messages newest→oldest within the
// (oldest, latest) exclusive ts bounds.
func visibleHistory(c *fakeConv, oldest, latest string) []fakeMsg {
	var out []fakeMsg
	for _, v := range slices.Backward(c.Msgs) {
		m := v
		if oldest != "" && !tsLess(oldest, m.TS) {
			continue
		}
		if latest != "" && !tsLess(m.TS, latest) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (f *fakeSlack) handleHistory(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyCalls++
	c := f.conv(r.FormValue("channel"))
	if c == nil {
		f.replyErr(w, "channel_not_found")
		return
	}
	if f.failHistory[c.ID] {
		f.replyErr(w, "internal_error")
		return
	}
	if f.failHistoryContinuations && r.FormValue("cursor") != "" {
		f.replyErr(w, "internal_error")
		return
	}
	visible := visibleHistory(c, r.FormValue("oldest"), r.FormValue("latest"))
	from, to, next := f.page(r, len(visible))
	msgs := make([]map[string]any, 0, to-from)
	for _, m := range visible[from:to] {
		msgs = append(msgs, m.toJSON())
	}
	f.reply(w, map[string]any{
		"messages":          msgs,
		"has_more":          next != "",
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

func (f *fakeSlack) handleReplies(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.conv(r.FormValue("channel"))
	if c == nil {
		f.replyErr(w, "channel_not_found")
		return
	}
	rootTS := r.FormValue("ts")
	if f.failReplies[rootTS] {
		f.replyErr(w, "internal_error")
		return
	}
	root := c.findRoot(rootTS)
	if root == nil {
		f.replyErr(w, "thread_not_found")
		return
	}
	oldest := r.FormValue("oldest")
	// The real API serves the root first, then replies oldest→newest.
	all := []fakeMsg{*root}
	for _, reply := range root.Replies {
		if oldest != "" && !tsLess(oldest, reply.TS) {
			continue
		}
		all = append(all, reply)
	}
	from, to, next := f.page(r, len(all))
	msgs := make([]map[string]any, 0, to-from)
	for _, m := range all[from:to] {
		msgs = append(msgs, m.toJSON())
	}
	f.reply(w, map[string]any{
		"messages":          msgs,
		"has_more":          next != "",
		"response_metadata": map[string]any{"next_cursor": next},
	})
}
