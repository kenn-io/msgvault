package notionmeetings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
)

// The oracle is a set of independently selected IDs. The server serves a cyclic,
// paginated graph; it does not implement or imitate the import scheduler.
func TestHTTPAdmissionConservationAcrossStoreRestarts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		worldSize := hegel.Draw(ht, hegel.Integers(uint(0), ^uint(0)))
		// Materialize at most eight HTTP meetings per generated world.
		count := 1 + int(worldSize%8)
		mask := hegel.Draw(ht, hegel.Integers(0, 255))
		cut := hegel.Draw(ht, hegel.Integers(1, 8))
		st := testutil.NewTestStore(t)
		_, err := st.GetOrCreateSource(SourceType, "world")
		require.NoError(ht, err)
		expected := map[string]bool{}
		meetings := map[string]MeetingNote{}
		reads := map[string]int{}
		for i := range count {
			id := fmt.Sprintf("meeting-%d", i)
			m := hydrationMeeting()
			m.ID = id
			m.CreatedBy.ID = "other"
			m.LastEditedBy.ID = "human"
			m.MeetingNotes.CalendarEvent.Attendees = nil
			m.MeetingNotes.Children = MeetingChildren{TranscriptBlockID: "text-" + id}
			if mask&(1<<i) != 0 {
				if i%2 == 0 {
					m.CreatedBy.ID = "human"
				} else {
					m.MeetingNotes.CalendarEvent.Attendees = []string{"human"}
				}
				expected[id] = true
			}
			meetings[id] = m
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(ht, "Bearer primary", r.Header.Get("Authorization"))
			send := func(value any) { assert.NoError(ht, json.NewEncoder(w).Encode(value)) }
			switch {
			case r.URL.Path == "/v1/users/me":
				send(map[string]any{"object": "user", "id": "bot", "type": "bot", "bot": map[string]any{"owner": map[string]any{"type": "user", "user": map[string]any{"id": "human"}}}})
			case r.URL.Path == "/v1/blocks/meeting_notes/query":
				send(map[string]any{"results": []any{}, "has_more": false})
			case r.URL.Path == "/v1/blocks/root/children":
				start, _ := strconv.Atoi(r.URL.Query().Get("start_cursor"))
				size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
				if !assert.Positive(ht, size) {
					http.Error(w, "invalid page size", http.StatusBadRequest)
					return
				}
				ids := []Block{{Object: "block", ID: "root", Type: "child_page"}}
				for i := range count {
					ids = append(ids, Block{Object: "block", ID: fmt.Sprintf("meeting-%d", i), Type: "meeting_notes"})
				}
				end := min(len(ids), start+size)
				results := []map[string]any{}
				for _, b := range ids[start:end] {
					results = append(results, map[string]any{"object": "block", "id": b.ID, "type": b.Type, b.Type: map[string]any{}, "has_children": false})
				}
				next := ""
				if end < len(ids) {
					next = strconv.Itoa(end)
				}
				send(map[string]any{"results": results, "has_more": next != "", "next_cursor": next})
			case strings.HasPrefix(r.URL.Path, "/v1/blocks/text-"):
				id := strings.TrimPrefix(r.URL.Path, "/v1/blocks/text-")
				reads[id]++
				assert.True(ht, expected[id], "unselected refreshed header must never authorize a section")
				send(map[string]any{"object": "block", "id": "text-" + id, "type": "paragraph", "has_children": false, "paragraph": map[string]any{"rich_text": []map[string]string{{"plain_text": "SCOPED_" + id}}}})
			case strings.HasPrefix(r.URL.Path, "/v1/blocks/meeting-"):
				id := strings.TrimPrefix(r.URL.Path, "/v1/blocks/")
				send(meetings[id])
			case r.URL.Path == "/v1/users" || strings.HasPrefix(r.URL.Path, "/v1/users/"):
				w.WriteHeader(http.StatusForbidden)
				send(map[string]string{"code": "restricted_resource"})
			default:
				assert.Fail(ht, "unexpected provider route", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		noSearch := false
		opts := ImportOptions{Identifier: "world", TokenType: "pat", RootPageIDs: []string{"root"}, SearchAccessible: &noSearch, Limit: 1}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		checkpoints := 0
		opts.Progress = func(line string) {
			if line == "Notion discovery checkpoint saved" {
				checkpoints++
				if checkpoints == cut {
					cancel()
				}
			}
		}
		imp := NewImporter(st, NewClient(server.URL, "primary"))
		imp.limits.Pending = 1
		_, err = imp.Import(ctx, opts)
		if err != nil {
			require.ErrorIs(ht, err, context.Canceled)
		}
		opts.Progress = nil
		opts.Limit = 0
		// Restart from persisted Store state with a new real Client and importer.
		next := NewImporter(st, NewClient(server.URL, "primary"))
		next.limits.Pending = 1
		_, err = next.Import(t.Context(), opts)
		require.NoError(ht, err)
		source, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "world")
		require.NoError(ht, err)
		archived, err := st.SourceMessageMetadata(t.Context(), source.ID)
		require.NoError(ht, err)
		got := map[string]bool{}
		for id := range archived {
			got[id] = true
		}
		assert.Equal(ht, expected, got)
		for id := range expected {
			var messageID int64
			require.NoError(ht, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT id FROM messages WHERE source_id=? AND source_message_id=?"), source.ID, id).Scan(&messageID))
			body, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(ht, err)
			assert.Contains(ht, body, "SCOPED_"+id)
			assert.Positive(ht, reads[id])
		}
		_, err = next.Import(t.Context(), opts)
		require.NoError(ht, err)
		var n int
		require.NoError(ht, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&n))
		assert.Equal(ht, len(expected), n)
	}, hegel.WithTestCases(16), hegel.WithDatabase(""))
}

func TestIdentityProofVariesIndependentlyFromBotAndDirectory(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Text())
		owner := "human-" + suffix + "-id"
		configured := "configured-" + suffix + "-id"
		self := &User{Object: "user", ID: "bot-" + suffix, Type: "bot", Bot: BotInfo{Owner: BotOwner{Type: "user", User: UserRef{ID: owner}}}}
		pat, err := ResolveIdentity(self, " PAT ", "")
		require.NoError(ht, err)
		assert.Equal(ht, owner, pat.UserID)
		assert.Equal(ht, "pat_owner", pat.Provenance)
		explicit, err := ResolveIdentity(self, "", configured)
		require.NoError(ht, err)
		assert.Equal(ht, configured, explicit.UserID)
		_, err = ResolveIdentity(self, "pat", configured)
		require.ErrorIs(ht, err, ErrUnknownIdentity)
		_, err = ResolveIdentity(self, "", "")
		require.ErrorIs(ht, err, ErrUnknownIdentity)
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}

func TestQueryPredicatesIndependentlySelectInterval(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minutes := hegel.Draw(ht, hegel.Integers(1, 100000))
		offset := hegel.Draw(ht, hegel.Integers(-1, minutes+1))
		seconds := hegel.Draw(ht, hegel.Integers(0, 59))
		from := time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)
		to := from.Add(time.Duration(minutes) * time.Minute)
		at := from.Add(time.Duration(offset)*time.Minute + time.Duration(seconds)*time.Second)
		filter := queryWindow{From: from, To: to}.filter()
		actual := true
		for _, term := range filter.Filters {
			v := term.Filter.Value.Value
			bound, err := time.Parse("2006-01-02 15:04", v.StartDate+" "+v.StartTime)
			require.NoError(ht, err)
			assert.Equal(ht, "UTC", v.TimeZone)
			switch term.Filter.Operator {
			case "date_is_on_or_after":
				actual = actual && !at.Before(bound)
			case "date_is_before":
				actual = actual && at.Before(bound)
			default:
				assert.Fail(ht, "unsupported query operator")
			}
		}
		assert.Equal(ht, !at.Before(from) && at.Before(to), actual)
	}, hegel.WithTestCases(128), hegel.WithDatabase(""))
}

func TestInterruptedKnownMeetingDoesNotStarveLaterRefresh(t *testing.T) {
	for _, interruption := range []string{"budget", "cancel"} {
		t.Run(interruption, func(t *testing.T) {
			checkInterruptedMeetingQueue(t, true, interruption)
		})
	}
}

func TestInterruptedAdmissionDoesNotStarveLaterAdmission(t *testing.T) {
	for _, interruption := range []string{"budget", "cancel"} {
		t.Run(interruption, func(t *testing.T) {
			checkInterruptedMeetingQueue(t, false, interruption)
		})
	}
}

// Each run recreates the real Client/Importer around native Store checkpoints.
// The first meeting's section exceeds the whole allowance or cancels the caller;
// the second meeting has an inexpensive, independently observable archive update.
func checkInterruptedMeetingQueue(t *testing.T, known bool, interruption string) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "queue")
	require.NoError(t, err)
	meetings := make([]MeetingNote, 2)
	for i, id := range []string{"a", "b"} {
		meeting := hydrationMeeting()
		meeting.ID = id
		meeting.CreatedBy.ID = "human"
		meeting.MeetingNotes.CalendarEvent.Attendees = nil
		meeting.MeetingNotes.Children = MeetingChildren{TranscriptBlockID: "text-" + id}
		meetings[i] = meeting
	}
	var mu sync.Mutex
	interrupted := false
	var cancelRun context.CancelFunc
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active, cancel := interrupted, cancelRun
		if r.URL.Path == "/v1/blocks/a" || r.URL.Path == "/v1/blocks/b" {
			headers = append(headers, strings.TrimPrefix(r.URL.Path, "/v1/blocks/"))
		}
		mu.Unlock()
		send := func(value any) { assert.NoError(t, json.NewEncoder(w).Encode(value)) }
		switch {
		case r.URL.Path == "/v1/users/me":
			send(map[string]any{"object": "user", "id": "bot", "type": "bot", "bot": map[string]any{"owner": map[string]any{"type": "user", "user": map[string]any{"id": "human"}}}})
		case r.URL.Path == "/v1/blocks/meeting_notes/query":
			results := meetings
			if known && active {
				results = []MeetingNote{}
			}
			send(map[string]any{"results": results, "has_more": false})
		case r.URL.Path == "/v1/users":
			send(map[string]any{"results": []any{}, "has_more": false})
		case r.URL.Path == "/v1/blocks/a":
			send(meetings[0])
		case r.URL.Path == "/v1/blocks/b":
			send(meetings[1])
		case r.URL.Path == "/v1/blocks/text-a":
			if active && interruption == "cancel" {
				cancel()
			}
			send(map[string]any{"object": "block", "id": "text-a", "type": "paragraph", "has_children": active, "paragraph": map[string]any{"rich_text": []map[string]string{{"plain_text": "A_ORIGINAL"}}}})
		case strings.HasSuffix(r.URL.Path, "/children"):
			// Every page leads to another real HTTP child-list request. With the
			// reduced shared allowance this cannot finish, regardless of ordering.
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children") + "-child"
			send(map[string]any{"results": []map[string]any{{"object": "block", "id": id, "type": "paragraph", "has_children": true, "paragraph": map[string]any{"rich_text": []map[string]string{{"plain_text": "A_UNPUBLISHED"}}}}}, "has_more": false})
		case r.URL.Path == "/v1/blocks/text-b":
			body := "B_ORIGINAL"
			if active {
				body = "B_UPDATED"
			}
			send(map[string]any{"object": "block", "id": "text-b", "type": "paragraph", "has_children": false, "paragraph": map[string]any{"rich_text": []map[string]string{{"plain_text": body}}}})
		default:
			assert.Fail(t, "unexpected provider request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	noSearch := false
	opts := ImportOptions{Identifier: "queue", TokenType: "pat", SearchAccessible: &noSearch}
	var oldA knownMeeting
	var oldRaw []byte
	messageID := func(id string) int64 {
		t.Helper()
		var result int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT id FROM messages WHERE source_id=? AND source_message_id=?"), source.ID, id).Scan(&result))
		return result
	}
	if known {
		imp := NewImporter(st, NewClient(server.URL, "synthetic"))
		_, err := imp.Import(t.Context(), opts)
		require.NoError(t, err)
		state, err := imp.loadState(t.Context(), source.ID)
		require.NoError(t, err)
		oldA = state.Known["a"]
		oldRaw, err = st.GetMessageRaw(messageID("a"))
		require.NoError(t, err)
	}
	mu.Lock()
	interrupted, headers = true, nil
	mu.Unlock()
	for run := 0; run < 3; run++ {
		ctx, cancel := context.WithCancel(t.Context())
		mu.Lock()
		cancelRun = cancel
		mu.Unlock()
		imp := NewImporter(st, NewClient(server.URL, "synthetic"))
		imp.now = func() time.Time { return time.Date(2026, 11, 9, run, 0, 0, 0, time.UTC) }
		if interruption == "budget" {
			imp.limits.Attempts = 6
		}
		_, err := imp.Import(ctx, opts)
		cancel()
		if interruption == "budget" {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, context.Canceled)
		}
	}
	mu.Lock()
	requested := append([]string(nil), headers...)
	mu.Unlock()
	assert.Contains(t, requested, "a")
	assert.Contains(t, requested, "b", "interrupted head must not prevent a later header request")
	var publishedB int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?"), source.ID, "b").Scan(&publishedB))
	require.Equal(t, 1, publishedB, "later work must publish before the expensive meeting can finish")
	body, err := st.GetMessageBodyText(t.Context(), messageID("b"))
	require.NoError(t, err)
	assert.Contains(t, body, "B_UPDATED")
	state, err := NewImporter(st, NewClient(server.URL, "synthetic")).loadState(t.Context(), source.ID)
	require.NoError(t, err)
	var published int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=?"), source.ID).Scan(&published))
	if known {
		assert.Equal(t, 2, published)
		assert.Equal(t, oldA.LastSuccessAt, state.Known["a"].LastSuccessAt)
		assert.Equal(t, oldA.SectionFreshness, state.Known["a"].SectionFreshness)
		assert.Equal(t, oldA.SnapshotSHA256, state.Known["a"].SnapshotSHA256)
		retained, err := st.GetMessageRaw(messageID("a"))
		require.NoError(t, err)
		assert.Equal(t, oldRaw, retained)
	} else {
		assert.Equal(t, 1, published)
		assert.Contains(t, state.Admissions, "a")
		assert.NotContains(t, state.Known, "a")
		var unpublishedA int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?"), source.ID, "a").Scan(&unpublishedA))
		assert.Zero(t, unpublishedA)
	}
}
