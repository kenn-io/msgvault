package daemonclient_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestMeetingLinksClient(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	var decisions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			assertions.Equal("/api/v1/meetings/links/decisions", r.URL.Path)
			var body struct {
				MessageID int64  `json:"message_id"`
				Event     string `json:"event"`
				Decision  string `json:"decision"`
			}
			if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
				return
			}
			assertions.Equal(int64(42), body.MessageID)
			assertions.Equal("evt:81@20270314T100000Z", body.Event)
			decisions = append(decisions, body.Decision)
			assertions.NoError(json.MarshalWrite(w, body))
			return
		}
		if r.URL.Path == "/api/v1/meetings/links" {
			assertions.Equal(http.MethodGet, r.Method)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("message_id") == "43" {
				_, err := w.Write([]byte(`{"links":[]}`))
				assertions.NoError(err)
				return
			}
			assertions.Equal("42", r.URL.Query().Get("message_id"))
			_, err := w.Write([]byte(`{"links":[{"ref":"evt:81","subject":"Planning","start":"2027-03-14T10:00:00Z","end":"2027-03-14T11:00:00Z"}]}`))
			assertions.NoError(err)
			return
		}
		assertions.Equal("/api/v1/meetings/suggestions", r.URL.Path)
		if r.URL.Query().Get("ref") == "msg:404" {
			w.WriteHeader(http.StatusNotFound)
			_, err := w.Write([]byte(`{"error":"meeting_not_found","message":"Missing"}`))
			assertions.NoError(err)
			return
		}
		assertions.Equal("evt:81@20270314T100000Z", r.URL.Query().Get("ref"))
		_, err := w.Write([]byte(`{"suggestions":[{"ref":"evt:81@20270314T100000Z","member_message_id":42,"overlap_minutes":30,"shared_attendees":1,"title_tokens":2,"reasons":["time_overlap"]}]}`))
		assertions.NoError(err)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })
	for _, decision := range []string{"link", "unlink"} {
		requirements.NoError(client.DecideMeetingLink(t.Context(), 42, "evt:81@20270314T100000Z", decision))
	}
	assertions.Equal([]string{"link", "unlink"}, decisions)
	suggestions, err := client.SuggestMeetingLinks(t.Context(), "evt:81@20270314T100000Z")
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(int64(42), suggestions[0].MemberMessageID)
	assertions.Equal(30, suggestions[0].OverlapMinutes)
	assertions.Equal(1, suggestions[0].SharedAttendees)
	assertions.Equal(2, suggestions[0].TitleTokens)
	assertions.Equal([]string{"time_overlap"}, suggestions[0].Reasons)
	_, err = client.SuggestMeetingLinks(t.Context(), "msg:404")
	var apiError *daemonclient.APIError
	requirements.ErrorAs(err, &apiError)
	assertions.Equal(404, apiError.Status)
	links, err := client.MeetingLinks(t.Context(), 42)
	requirements.NoError(err)
	requirements.Len(links, 1)
	assertions.Equal("evt:81", links[0].Ref)
	assertions.Equal("Planning", links[0].Subject)
	assertions.Equal("2027-03-14T10:00:00Z", links[0].Start.Format(time.RFC3339))
	links, err = client.MeetingLinks(t.Context(), 43)
	requirements.NoError(err)
	assertions.Empty(links)
}
