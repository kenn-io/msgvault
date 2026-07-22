package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
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
	// LegacyAttachments/Blocks emit as "attachments"/"blocks" (bot payloads).
	LegacyAttachments []map[string]any
	Blocks            []map[string]any
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
	if len(m.LegacyAttachments) > 0 {
		out["attachments"] = m.LegacyAttachments
	}
	if len(m.Blocks) > 0 {
		out["blocks"] = m.Blocks
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

// tombstone replaces the message at ts with its deleted-root form as probed
// live: subtype "tombstone", USLACKBOT sender, canonical text, no reactions
// or files — while reply_count survives (emitted from the kept Replies), so
// the thread stays discoverable and its orphaned replies stay served.
func (c *fakeConv) tombstone(ts string) {
	m := c.findRoot(ts)
	m.User = "USLACKBOT"
	m.BotID = ""
	m.Username = ""
	m.Subtype = "tombstone"
	m.Text = "This message was deleted."
	m.Edited = false
	m.Reactions = nil
	m.Files = nil
}

// findRoot resolves ts — a root's ts OR any reply's ts — to the thread's
// root fakeMsg, mimicking conversations.replies' anchor resolution.
func (c *fakeConv) findRoot(ts string) *fakeMsg {
	for i := range c.Msgs {
		if c.Msgs[i].TS == ts {
			return &c.Msgs[i]
		}
		for j := range c.Msgs[i].Replies {
			if c.Msgs[i].Replies[j].TS == ts {
				return &c.Msgs[i]
			}
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
	// failHistory / failReplies / failMembers make the named channel / root
	// ts answer a method error (a non-retryable fetch failure).
	failHistory map[string]bool
	failReplies map[string]bool
	failMembers map[string]bool
	// failSearch makes search.messages answer a method error.
	failSearch bool
	// searchMissingScope makes search.messages answer missing_scope (a
	// token without search:read).
	searchMissingScope bool
	// searchPageSize overrides the honored count for search pagination
	// (0 = honor the requested count).
	searchPageSize int
	// searchIndexedThrough hides hits newer than this ts, simulating search
	// index lag ("" = everything indexed instantly).
	searchIndexedThrough string
	// searchHidden hides individual hits by ts, simulating OUT-OF-ORDER
	// index lag: a message indexed later than ones created after it.
	searchHidden map[string]bool
	// searchTruncateDays makes the named on: days report a total beyond
	// the reachable-result ceiling, emulating a persistently >10k day
	// while other days stay pageable.
	searchTruncateDays map[string]bool
	// searchOmitThreadTS strips thread_ts from result permalinks, so hits
	// arrive with unparseable roots (the solo-entry degradation path).
	searchOmitThreadTS bool
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
	// onHistory, when set, runs synchronously at the top of each
	// conversations.history request (test-side fault injection at a
	// deterministic mid-run point).
	onHistory func(channelID string)
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	return &fakeSlack{
		t: t, pageSize: 3,
		failHistory: map[string]bool{}, failReplies: map[string]bool{},
		failMembers: map[string]bool{}, searchHidden: map[string]bool{},
		searchTruncateDays: map[string]bool{},
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
	mux.HandleFunc("/search.messages", f.handleSearch)
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

// page slices items[from:from+size] driven by the "cursor" form value (a
// stringified index), returning the slice and the next cursor. Like the real
// API, a "limit" smaller than the server's page size is honored.
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
	size := f.pageSize
	if raw := r.FormValue("limit"); raw != "" {
		if limit, err := strconv.Atoi(raw); err == nil && limit > 0 && limit < size {
			size = limit
		}
	}
	to = min(from+size, n)
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
	if f.failMembers[c.ID] {
		f.replyErr(w, "internal_error")
		return
	}
	from, to, next := f.page(r, len(c.Members))
	f.reply(w, map[string]any{
		"members":           c.Members[from:to],
		"response_metadata": map[string]any{"next_cursor": next},
	})
}

// visibleHistory returns c's top-level messages newest→oldest within the
// (oldest, latest) ts bounds — exclusive by default, inclusive like the real
// API's inclusive=true when requested.
func visibleHistory(c *fakeConv, oldest, latest string, inclusive bool) []fakeMsg {
	var out []fakeMsg
	for _, m := range slices.Backward(c.Msgs) {
		switch {
		case oldest == "":
		case inclusive && tsLess(m.TS, oldest):
			continue
		case !inclusive && !tsLess(oldest, m.TS):
			continue
		}
		switch {
		case latest == "":
		case inclusive && tsLess(latest, m.TS):
			continue
		case !inclusive && !tsLess(m.TS, latest):
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
	if f.onHistory != nil {
		f.onHistory(r.FormValue("channel"))
	}
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
	visible := visibleHistory(c, r.FormValue("oldest"), r.FormValue("latest"), r.FormValue("inclusive") == "true")
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
	if root.TS != rootTS {
		// Probed live: a REPLY ts anchor serves ONLY that reply — no
		// oldest/limit/cursor combination expands it to the thread.
		for i := range root.Replies {
			if root.Replies[i].TS == rootTS {
				f.reply(w, map[string]any{
					"messages":          []map[string]any{root.Replies[i].toJSON()},
					"has_more":          false,
					"response_metadata": map[string]any{"next_cursor": ""},
				})
				return
			}
		}
	}
	oldest := r.FormValue("oldest")
	// The real API serves the root first (even below the oldest bound —
	// probed live), then replies oldest→newest.
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

// searchHit pairs a reply with its location for the fake search index.
type searchHit struct {
	channelID string
	rootTS    string
	ts        string
}

// handleSearch mimics search.messages closely enough for the sweep:
// threads:replies filtering, on:/after: day bounds (UTC days — fake users
// carry tz_offset 0), in:<#ID> scoping, negated terms ignored, ascending
// timestamp sort, count/page pagination WITH the probed clamp behavior
// (page numbers beyond 100 are silently served as page 1).
func (f *fakeSlack) handleSearch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSearch {
		f.replyErr(w, "fatal_error")
		return
	}
	if f.searchMissingScope {
		f.replyErr(w, "missing_scope")
		return
	}
	query := r.FormValue("query")
	repliesOnly := false
	var onDay, afterDay string
	scopes := map[string]bool{}
	for tok := range strings.FieldsSeq(query) {
		switch {
		case tok == "threads:replies":
			repliesOnly = true
		case strings.HasPrefix(tok, "on:"):
			onDay = strings.TrimPrefix(tok, "on:")
		case strings.HasPrefix(tok, "after:"):
			afterDay = strings.TrimPrefix(tok, "after:")
		case strings.HasPrefix(tok, "in:<#") && strings.HasSuffix(tok, ">"):
			scopes[tok[len("in:<#"):len(tok)-1]] = true
		}
	}
	day := func(ts string) string { return tsTime(ts).UTC().Format("2006-01-02") }
	keep := func(ts string) bool {
		if f.searchHidden[ts] {
			return false // this message not yet indexed (out-of-order lag)
		}
		if f.searchIndexedThrough != "" && tsLess(f.searchIndexedThrough, ts) {
			return false // not yet indexed
		}
		d := day(ts)
		if onDay != "" && d != onDay {
			return false
		}
		if afterDay != "" && d <= afterDay {
			return false
		}
		return true
	}

	var hits []searchHit
	for _, c := range f.convs {
		if len(scopes) > 0 && !scopes[c.ID] {
			continue
		}
		for i := range c.Msgs {
			root := &c.Msgs[i]
			if !repliesOnly && keep(root.TS) {
				hits = append(hits, searchHit{channelID: c.ID, ts: root.TS})
			}
			for j := range root.Replies {
				if keep(root.Replies[j].TS) {
					hits = append(hits, searchHit{channelID: c.ID, rootTS: root.TS, ts: root.Replies[j].TS})
				}
			}
		}
	}
	slices.SortFunc(hits, func(a, b searchHit) int {
		if tsLess(a.ts, b.ts) {
			return -1
		}
		if tsLess(b.ts, a.ts) {
			return 1
		}
		return 0
	})

	count, _ := strconv.Atoi(r.FormValue("count"))
	if count <= 0 {
		count = 20
	}
	if f.searchPageSize > 0 && f.searchPageSize < count {
		count = f.searchPageSize
	}
	page, _ := strconv.Atoi(r.FormValue("page"))
	if page <= 0 {
		page = 1
	}
	if page > 100 {
		page = 1 // probed live: past-the-ceiling pages are CLAMPED to page 1
	}
	total := len(hits)
	if onDay != "" && f.searchTruncateDays[onDay] {
		total = sweepTruncationCeiling + 1
	}
	pages := (total + count - 1) / count
	from := min((page-1)*count, len(hits))
	to := min(from+count, len(hits))

	matches := make([]map[string]any, 0, to-from)
	for _, h := range hits[from:to] {
		permalink := "https://testers.slack.com/archives/" + h.channelID + "/p" + strings.ReplaceAll(h.ts, ".", "")
		if h.rootTS != "" && !f.searchOmitThreadTS {
			permalink += "?thread_ts=" + h.rootTS + "&cid=" + h.channelID
		}
		matches = append(matches, map[string]any{
			"ts":        h.ts,
			"channel":   map[string]any{"id": h.channelID},
			"permalink": permalink,
		})
	}
	f.reply(w, map[string]any{
		"messages": map[string]any{
			"total":   total,
			"paging":  map[string]any{"count": count, "total": total, "page": page, "pages": pages},
			"matches": matches,
		},
	})
}
