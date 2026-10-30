package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsCursorOnlyPagination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var hostile atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hostile.Add(1) }))
	defer attacker.Close()
	var calls atomic.Int32
	c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Query().Get("after") {
		case "":
			respond(w, 200, `{"data":[`+mediaJSON+`],"paging":{"cursors":{"after":"A"},"next":"`+attacker.URL+`?access_token=secret"}}`)
		case "A":
			respond(w, 200, `{"data":[{"id":"2","timestamp":"2026-10-28T13:00:00Z"}],"paging":{"cursors":{"after":"B"}}}`)
		case "B":
			respond(w, 200, `{"data":[],"paging":{"cursors":{"after":"B"}}}`)
		default:
			assert.Fail("unexpected cursor")
			respond(w, 400, `{}`)
		}
	}, nil)
	var tracker CursorTracker
	var ids []string
	for after := ""; ; {
		p, err := c.Conversation(context.Background(), "123", after)
		require.NoError(err)
		for _, m := range p.Items {
			ids = append(ids, m.ID)
		}
		after, err = tracker.Next(p)
		require.NoError(err)
		if after == "" {
			break
		}
	}
	assert.Equal([]string{testID, "2"}, ids)
	assert.Equal(int32(3), calls.Load())
	assert.Equal(3, b.Attempts())
	assert.Zero(hostile.Load())
	for _, tc := range []struct{ name, after, body string }{
		{"repeated", "A", `{"data":[` + mediaJSON + `],"paging":{"cursors":{"after":"A"}}}`},
		{"missing cursor", "", `{"data":[` + mediaJSON + `]}`},
		{"missing data", "", `{"paging":{"cursors":{"after":"A"}}}`},
		{"wrong root", "", `{"data":[{"id":"2","timestamp":"2026-10-28T12:00:00Z","root_post":{"id":"999"}}],"paging":{"cursors":{"after":"A"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { respond(w, 200, tc.body) }, nil)
			_, err := client.Conversation(context.Background(), "123", tc.after)
			require.Error(err)
		})
	}
	t.Run("cycle and same page under fresh cursor", func(t *testing.T) {
		for _, pages := range [][]Page{
			{{Items: []Media{{ID: "1"}}, After: "A"}, {Items: []Media{{ID: "2"}}, After: "B"}, {Items: []Media{{ID: "3"}}, After: "A"}},
			{{Items: []Media{{ID: "1"}}, After: "A"}, {Items: []Media{{ID: "1"}}, After: "B"}},
		} {
			var walk CursorTracker
			for i, p := range pages {
				_, err := walk.Next(p)
				if i == len(pages)-1 {
					assert.ErrorIs(err, ErrPagination)
				} else {
					require.NoError(err)
				}
			}
		}
	})
}
