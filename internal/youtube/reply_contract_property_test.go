package youtube

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Every authorization field must match before any provider operation is possible.
func TestReplyConfirmationBindsEveryOperationFieldProperty(t *testing.T) {
	service, provider, target, _ := replyFixture(t)
	sequence := 0
	hegel.Test(t, func(ht *hegel.T) {
		sequence++
		body := fmt.Sprintf("Reply-%d \n", sequence) + hegel.Draw(ht, hegel.Text()) + "\r "
		beforeChecks, beforeSends := provider.checks.Load(), provider.sends.Load()
		preview, err := service.Preview(t.Context(), target, body)
		require.NoError(ht, err)
		changed := target
		token := preview.Token
		switch hegel.Draw(ht, hegel.Integers(0, 8)) {
		case 0:
			changed.SourceID++
		case 1:
			changed.ChannelID += "-other"
		case 2:
			changed.VideoID += "-other"
		case 3:
			changed.ParentID += "-other"
		case 4:
			changed.MessageID++
		case 5:
			changed.CommentID += "-other"
		case 6:
			changed.ThreadID += "-other"
		case 7:
			body += "\x00"
		case 8:
			token += "0"
		}
		_, err = service.Confirm(t.Context(), changed, body, token)
		require.ErrorContains(ht, err, "confirmation does not match")
		assert.Equal(ht, beforeChecks, provider.checks.Load())
		assert.Equal(ht, beforeSends, provider.sends.Load())
	}, hegel.WithTestCases(300))
}

// The HTTP request and provider response must preserve the exact Unicode bytes.
func TestReplyHTTPBodyRoundTripProperty(t *testing.T) {
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
			return
		}
		if r.URL.Path != "/comments" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var posted Comment
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		posted.ID = "sent-1"
		if err := json.NewEncoder(w).Encode(posted); err != nil {
			http.Error(w, "fixture response failed", http.StatusInternalServerError)
		}
	})
	hegel.Test(t, func(ht *hegel.T) {
		body := " \r\n\x00\"\\" + hegel.Draw(ht, hegel.Text()) + "\n "
		comment, err := client.InsertReply(t.Context(), "parent-1", body)
		require.NoError(ht, err)
		assert.Equal(ht, body, comment.Snippet.TextOriginal)
	}, hegel.WithTestCases(300))
}

func TestReplyCurrentParentEligibilityAcrossPagesProperty(t *testing.T) {
	type scenario struct {
		pages      int
		status     string
		permission int
	}
	var current atomic.Pointer[scenario]
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
		case "/channels":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"owner-1"}]}`)
		case "/commentThreads":
			fixture := current.Load()
			index := 0
			if token := r.URL.Query().Get("pageToken"); token != "" {
				parsed, err := strconv.Atoi(strings.TrimPrefix(token, "page-"))
				if err != nil {
					http.Error(w, "invalid fixture page", http.StatusBadRequest)
					return
				}
				index = parsed
			}
			response := map[string]any{}
			if index < fixture.pages {
				response["items"] = []any{map[string]any{"id": "other-thread"}}
				response["nextPageToken"] = fmt.Sprintf("page-%d", index+1)
			} else {
				snippet := map[string]any{
					"videoId":         "video-1",
					"topLevelComment": map[string]any{"id": "parent-1", "snippet": map[string]any{"moderationStatus": fixture.status}},
				}
				if fixture.permission != 0 {
					snippet["canReply"] = fixture.permission == 2
				}
				response["items"] = []any{map[string]any{"id": "thread-1", "snippet": snippet}}
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				http.Error(w, "fixture response failed", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	})
	target := ReplyTarget{ChannelID: "owner-1", VideoID: "video-1", ParentID: "parent-1", ThreadID: "thread-1"}
	statuses := []string{"published", "heldForReview", "likelySpam", "", "future-status"}
	hegel.Test(t, func(ht *hegel.T) {
		// Bound the materialized HTTP work, while drawing the full count domain.
		pages := min(hegel.Draw(ht, hegel.Integers(0, int(^uint(0)>>1))), 8)
		fixture := &scenario{pages: pages, status: statuses[hegel.Draw(ht, hegel.Integers(0, len(statuses)-1))], permission: hegel.Draw(ht, hegel.Integers(0, 2))}
		current.Store(fixture)
		err := client.CheckReply(t.Context(), target)
		if fixture.status == "published" && fixture.permission == 2 {
			require.NoError(ht, err)
		} else {
			require.ErrorIs(ht, err, ErrReplyRejected)
		}
	}, hegel.WithTestCases(100))
}
