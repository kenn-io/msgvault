package granola

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListNotes_ParamsAndPagination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("Bearer grn_testkey", r.Header.Get("Authorization"))
		assert.Equal("/v1/notes", r.URL.Path)
		calls = append(calls, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = fmt.Fprint(w, `{"notes":[{"id":"not_Ab12Cd34Ef56Gh","object":"note","title":"First","owner":{"name":"Alice Smith","email":"alice@example.com"},"created_at":"2026-06-01T15:02:11Z","updated_at":"2026-06-01T16:45:00Z"}],"hasMore":true,"cursor":"c2"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"notes":[{"id":"not_Zz98Yy87Xx76Wv","object":"note","title":null,"owner":{"name":null,"email":"alice@example.com"},"created_at":"2026-06-02T09:15:00Z","updated_at":"2026-06-02T09:50:00Z"}],"hasMore":false,"cursor":null}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "grn_testkey")
	updatedAfter := time.Date(2026, 5, 1, 0, 0, 0, 123456789, time.UTC)

	page1, err := c.ListNotes(context.Background(), ListNotesParams{UpdatedAfter: updatedAfter, PageSize: 30})
	require.NoError(err)
	require.Len(page1.Notes, 1)
	assert.Equal("not_Ab12Cd34Ef56Gh", page1.Notes[0].ID)
	assert.Equal("Alice Smith", page1.Notes[0].Owner.Name)
	assert.True(page1.HasMore)
	require.Equal("c2", page1.Cursor)

	page2, err := c.ListNotes(context.Background(), ListNotesParams{UpdatedAfter: updatedAfter, PageSize: 30, Cursor: page1.Cursor})
	require.NoError(err)
	require.Len(page2.Notes, 1)
	assert.Empty(page2.Notes[0].Title, "null title decodes to empty string")
	assert.False(page2.HasMore)
	assert.Empty(page2.Cursor, "null cursor decodes to empty string")

	require.Len(calls, 2)
	assert.Equal("page_size=30&updated_after=2026-05-01T00%3A00%3A00.123456789Z", calls[0])
	assert.Equal("cursor=c2&page_size=30&updated_after=2026-05-01T00%3A00%3A00.123456789Z", calls[1])
}

func TestGetNote_DecodesAndKeepsRaw(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture, err := os.ReadFile("testdata/note_full.json")
	require.NoError(err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v1/notes/not_Ab12Cd34Ef56Gh", r.URL.Path)
		assert.Equal("transcript", r.URL.Query().Get("include"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	n, err := NewClient(srv.URL, "grn_testkey").GetNote(context.Background(), "not_Ab12Cd34Ef56Gh")
	require.NoError(err)

	assert.Equal("Quarterly Planning Review", n.Title)
	assert.Equal("alice@example.com", n.Owner.Email)
	require.NotNil(n.CalendarEvent)
	assert.Equal("bob@example.com", n.CalendarEvent.Organiser)
	assert.Equal(time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC), n.CalendarEvent.ScheduledStartTime)
	require.Len(n.Attendees, 3)
	assert.Empty(n.Attendees[2].Name, "null attendee name decodes to empty string")
	require.Len(n.Transcript, 3)
	assert.Equal("Alice Smith", n.Transcript[0].Speaker.Name)
	assert.Equal("Speaker C", n.Transcript[2].Speaker.DiarizationLabel)
	assert.JSONEq(string(fixture), string(n.Raw), "raw preserves the verbatim response")
}

func TestGetNote_NullCalendarEvent(t *testing.T) {
	require := require.New(t)

	fixture, err := os.ReadFile("testdata/note_no_calendar.json")
	require.NoError(err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	n, err := NewClient(srv.URL, "grn_testkey").GetNote(context.Background(), "not_Zz98Yy87Xx76Wv")
	require.NoError(err)
	require.Nil(n.CalendarEvent)
	require.Empty(n.Title)
	require.Empty(n.SummaryMarkdown, "null summary_markdown decodes to empty string")
	require.Len(n.Transcript, 2)
}

func TestClient_RetriesOn429(t *testing.T) {
	require := require.New(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"notes":[],"hasMore":false,"cursor":null}`)
	}))
	defer srv.Close()

	out, err := NewClient(srv.URL, "grn_testkey").ListNotes(context.Background(), ListNotesParams{})
	require.NoError(err)
	require.Empty(out.Notes)
	require.Equal(int32(2), hits.Load(), "expected one retry after the 429")
}

func TestClient_UnauthorizedIsActionable(t *testing.T) {
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "grn_bad").ListNotes(context.Background(), ListNotesParams{})
	require.Error(err)
	require.Contains(err.Error(), "api_key")
}

func TestListNotesOutput_FixtureRoundTrip(t *testing.T) {
	// Guards the struct tags against drift from the OpenAPI field names.
	require := require.New(t)
	fixture, err := os.ReadFile("testdata/note_full.json")
	require.NoError(err)
	var n Note
	require.NoError(json.Unmarshal(fixture, &n))
	require.Equal("not_Ab12Cd34Ef56Gh", n.ID)
	require.Equal("## Quarterly Planning Review\n\n- Agreed on **three priorities**\n- Budget approved", n.SummaryMarkdown)
	require.Len(n.FolderMembership, 1)
	require.Equal("Planning", n.FolderMembership[0].Name)
	require.Nil(n.FolderMembership[0].ParentFolderID)
}
